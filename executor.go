package quarry

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
)

// The tree driver (build steps 2-3). A node is Sequential(Planner → Parallel(
// children) → Reducer) from §2, and recursion is structural: a child re-enters
// node() rather than always solving, so a decomposing node is just a node whose
// children decompose too. Termination is the base case of §2, not a fixed depth.
//
// Two invariants from §3.1 are structural here, not optional:
//
//   - The propagation duality. Children run under a SHORTENED context (time
//     divides along depth) but hold their FULL allocation (money is inherited
//     whole per child, reserved from the parent up front and refunded unspent).
//     The reducer runs under the PARENT context, which still holds the time
//     reserve — so it can merge even after the children's window has closed.
//
//   - Partial tolerance. A child that misses does not abort the run. The reducer
//     folds whatever returned. Only genuinely unexpected faults (a provider
//     error that is not a context error) propagate and fail the run.
//
// Miss classification follows the standing ruling: ONLY TIME IS A GAP. A node
// stopped by context cancellation or deadline is truncated at runtime and its
// outcome is flagged Gap — the record must name it (§3.1). A node that cannot be
// afforded is planned degradation, already disclosed as Plan.Excluded at the
// gate under P9, so it is recorded with empty content but NOT flagged Gap.
//
// The cache (§6) is consulted on entry and appended on completion. A hit that
// serves returns a stored sample and spends nothing, collapsing an identical
// sub-problem to zero further calls — divide-and-conquer becomes dynamic
// programming. Within a single plan, identical sub-problems are deduped before
// fanout so "one call" holds regardless of sibling timing (§2, DAG not tree).

// DefaultMaxDepth backstops recursion when no explicit cap is set (P2). It is a
// backstop, not the design — the real terminators are planner-declines, the
// below-floor gate, and (step 4) verifier availability.
const DefaultMaxDepth = 8

// Executor drives one run. All collaborators are seams; none reads the clock —
// now is a field (Go rule 4).
type Executor struct {
	Planner Planner
	Solver  Solver
	Reducer Reducer
	Floor   Units
	Now     time.Time

	// Clock, when set, measures per-node wall-clock into NodeOutcome.Timing (§9).
	// A FUNCTION, unlike Now: Now is one fixed instant used for deadline and TTL
	// arithmetic, which must not move mid-run or apportionment would drift. Timing
	// needs a value that DOES advance, so it is a separate seam rather than a
	// reinterpretation of Now.
	//
	// Optional, and nil is the honest default: no clock means no timing recorded,
	// which reads as "not measured" rather than "took no time". Keeping it a field
	// preserves Go rule 4 — the core still never calls time.Now() itself, so a test
	// can inject a deterministic sequence and the whole package stays clock-free.
	//
	// Timings are NOT hashed into the record (see NodeTiming), so wiring a real
	// clock here cannot break byte-identical replay.
	Clock func() time.Time

	// MaxDepth is the recursion backstop (P2). Zero means DefaultMaxDepth.
	MaxDepth int

	// Verifier checks leaf results and gates recursion (P2, §5). Optional; nil
	// means no verification and recursion is bounded by MaxDepth and budget
	// alone. When present, verifier AVAILABILITY is the primary terminator: a
	// node whose children have no available verifier must not recurse, because
	// error compounds along depth and the merge becomes an act of faith (P2).
	Verifier Verifier

	// MaxRetries caps re-solves of a leaf whose result fails verification (§5,
	// §3 decorator order Budget(Retry(agent))). Retries consume budget from the
	// reserve and stop when it runs out. Zero means one attempt, no retry.
	MaxRetries int

	// Extractor pulls comparable claims from produced content (§7, step 6), so
	// CostPerVerifiedClaim has real input and stability (step 7) has something to
	// compare across replicates. Optional; nil leaves Claims empty. Must be a pure
	// function of content — extraction runs during replay too and may not perturb
	// the byte-for-byte record (P8).
	Extractor ClaimExtractor

	// Cache memoizes sub-problems (§6). Optional; nil disables memoization.
	Cache Cache

	// ReadPolicy chooses serve vs extend on a warm cache entry (§6). Optional;
	// nil defaults to serve-when-warm. n is the number of samples already held.
	// Step 7 flips unstable nodes to extend; until then serving demonstrates DAG
	// reuse without destroying replication, because a cold entry always draws.
	ReadPolicy func(p Problem, n int) string

	// Estimate gives the pre-call admission cost for a leaf. Optional; when nil
	// the executor admits against a zero estimate and relies on the post-call
	// debit to record actuals. A real deployment wires the Provider's Estimate.
	Estimate func(p Problem) Units

	// Sink receives node-level telemetry. On from build step 1 — nearly free,
	// and the corpus only accumulates (§8.2). Optional.
	Sink TelemetrySink

	// Observer watches the run AS IT HAPPENS (§9) — the live tree. Distinct from
	// Sink, which reads finished nodes as artifacts; see the Observer doc for why
	// widening TelemetrySink would have been wrong. Optional; nil means no live view.
	//
	// Called on the executor's own goroutines, so an Observer that blocks slows the
	// run it is watching and under a deadline can cause the gaps it displays.
	Observer Observer

	// Adversary, when set, turns on the surplus-budget policy (§3 Surplus): a run
	// that completes UNDER CAP spends its remaining balance on adversarial passes
	// over the highest-exposure claims (P3), so budget converts to quality rather
	// than evaporating. Active work inside the already-authorized ceiling, so it
	// is consistent with P5. Optional; nil means no surplus pass.
	//
	// Independence is the adversary's own contract (§5): the executor does not know
	// model families, so wiring a same-family adversary type-checks and violates
	// the principle. See provider.NewBedrockAdversary, which enforces it.
	Adversary Adversary
}

// Result is what a run produced, plus enough structure to assemble a record
// (step 5). Outcomes are pre-order (self before children) so replay is
// deterministic (P8).
type Result struct {
	Answer   Sample
	Outcomes []NodeOutcome
	Plan     Plan
	BoundBy  Denomination

	// Adversarial holds the surplus-budget passes (§3 Surplus), empty unless an
	// Adversary is wired and budget remained after the tree completed. Carried into
	// the record so the receipt names what was attacked and what broke (§8).
	Adversarial []AdversarialFinding

	// Bounds reports the executor settings this tree was grown under, so the record can
	// carry them and a replay can re-execute under the same rules (§7, P8). Beside
	// BoundBy for the same reason: both are facts of the execution that the tree's shape
	// does not fully determine.
	Bounds RunBounds
}

// subtree is what node() returns: this node's own outcome and answer, plus the
// flat pre-order list of the whole subtree for the record.
type subtree struct {
	outcome  NodeOutcome
	sample   Sample
	outcomes []NodeOutcome
}

// Run executes the root problem against its ledger and context.
func (e *Executor) Run(ctx context.Context, root Problem, l *Ledger) (Result, error) {
	// The root inherits nothing: it is never a portfolio arm and no plan funded it,
	// so its PlanWeight stays zero.
	st, plan, err := e.node(ctx, root, l, "n0", 0, parentPlan{})
	if err != nil {
		return Result{}, err
	}
	res := Result{
		Answer:   st.sample,
		Outcomes: st.outcomes,
		Plan:     plan,
		BoundBy:  boundBy(ctx, l, st.outcomes),
		// The resolved values, not the raw fields: maxDepth() applies DefaultMaxDepth when
		// MaxDepth is zero, and a replay needs the bound that actually applied.
		Bounds: RunBounds{MaxDepth: e.maxDepth(), Floor: e.Floor, MaxRetries: e.MaxRetries},
	}
	res.Adversarial = e.surplus(ctx, l, st.outcomes)
	return res, nil
}

// surplus spends a completed run's remaining balance on adversarial passes over
// the highest-exposure claims (§3 Surplus, P3). It runs only when an Adversary is
// wired and the run finished with budget to spare — the "completes UNDER CAP"
// precondition. Time expiry means the run is already truncated: there is no slack
// to convert, and starting fresh attacks against a dead deadline would only
// produce gaps, so surplus is skipped and whatever exists is returned now (§3.1).
//
// The claim set is the whole tree's claims (populated by the Extractor in step
// 6); with no extractor there are no claims and this is a no-op. SurplusPlan
// orders and budgets the selection, RunSurplus attacks under the same admission
// control every other call obeys — surplus is metered spend, never a bypass.
func (e *Executor) surplus(ctx context.Context, l *Ledger, outcomes []NodeOutcome) []AdversarialFinding {
	if e.Adversary == nil || ctx.Err() != nil {
		return nil
	}
	budget := l.Balance()
	if budget.Limited() && budget <= 0 {
		return nil // nothing left over; the run spent its cap
	}

	var claims []Claim
	samples := make(map[string]Sample)
	for _, o := range outcomes {
		claims = append(claims, o.Claims...)
		if o.Content != "" {
			samples[o.NodeID] = Sample{Content: o.Content, Model: o.Model, ModelVersion: o.ModelVersion}
		}
	}
	if len(claims) == 0 {
		return nil
	}

	selected := SurplusPlan(claims, outcomes, e.Adversary, budget)
	return RunSurplus(ctx, l, e.Adversary, selected, samples)
}

// parentPlan is what a node inherits from the plan that created it. Both fields are
// facts about the PARENT's decision, which the child cannot derive for itself and
// which the record would otherwise lose.
//
// Grouped into one struct rather than added as two more positional parameters
// because they travel together and mean nothing apart: they are the plan item,
// minus the problem the node already has.
type parentPlan struct {
	// arm marks this node as one competing attempt among N at the same problem (§2).
	// Such a node must NOT read the cache: a served arm is a copy of a sibling, not
	// an independent draw, which is exactly how a cache "saves money by destroying
	// replication" (§6, P7). It still WRITES, so the entry accumulates N genuine
	// samples.
	arm bool

	// weight is the relative weight this node was funded by (§3). Zero for the root,
	// which no plan funded. See NodeOutcome.PlanWeight.
	weight int64

	// id is the parent's node ID, carried for the live Observer seam (§9): a viewer
	// must place a node in the tree at ENTRY, before it has an answer, and parentage
	// is the one fact it cannot derive from the node itself. Empty for the root, the
	// only parentless node.
	//
	// The positional NodeID encodes parentage too ("n0.2.1" is a child of "n0.2"), so
	// this is redundant with the ID — deliberately. That encoding is quarry's
	// business, and a renderer should not have to parse it to draw a line.
	id string

	// index is this node's position in the parent's plan, carried for the same reason
	// and to the same field on NodeEnter. Children are entered concurrently, so a
	// viewer ordering siblings by arrival draws a different tree on every run.
	index int
}

// node solves one node, recursing into children unless a base case fires.
//
// Returns the subtree, the plan this node used (for the root's Result; empty for
// leaves), and a non-nil error ONLY for a genuine fault or a plan that violates
// its P9 contract. Budget and time misses are recorded in the outcome, never
// returned as errors — partial tolerance (§3.1).
//
// from carries what the PARENT's plan decided about this node; see parentPlan.
func (e *Executor) node(ctx context.Context, p Problem, l *Ledger, id string, depth int, from parentPlan) (subtree, Plan, error) {
	started := e.tick()
	// Live entry announcement (§9), BEFORE the cache read and before planning, so a
	// viewer can place this node in the tree while it is still empty. This moment
	// always existed — the start timestamp above is taken here — but nothing was
	// published from it, which is what made the live tree of §9 look blocked on a
	// redesign rather than on one call.
	//
	// The allocation is captured here because it is only knowable here: by the time
	// the node finishes, what it was ALLOWED is gone and only what it SPENT remains.
	e.enter(NodeEnter{
		NodeID: id, ParentID: from.id, Depth: depth, Index: from.index, Problem: p,
		Alloc: Allocation{Spend: l.Balance(), Deadline: deadlineOf(ctx)},
		Arm:   from.arm, At: started,
	})

	// Cache read (§6). A warm entry may be served — spend nothing, collapse the
	// sub-problem — or extended, which falls through to recompute and append.
	if e.Cache != nil && !from.arm {
		if samples := e.Cache.Get(p, e.Now); len(samples) > 0 && e.readServe(p, len(samples)) {
			s := samples[0] // deterministic pick for replay (P8); step 7 uses the full distribution
			o := NodeOutcome{NodeID: id, Problem: p, Content: s.Content, Depth: depth, CacheHit: true, Verified: s.Verified, PlanWeight: from.weight}
			// A hit carries the stored token split: the tokens were really spent once,
			// and the entry records what they were. The COST stays zero — this run paid
			// nothing — so a hit is visibly "real tokens, no charge" rather than either
			// a free call or an uncounted one.
			o.HaloTokens, o.GeneratedTokens = s.HaloTokens, s.GeneratedTokens
			e.extractClaims(ctx, &o, s)
			e.stamp(&o, started)
			e.emit(o)
			return subtree{outcome: o, sample: s, outcomes: []NodeOutcome{o}}, Plan{}, nil
		}
	}

	// Base cases that need no planner call (§2) — the depth bound and the floor.
	//
	// SHARED WITH THE PLAN GATE via PrePlanBase (plan.go, #15), so `quarry plan`
	// cannot emit an artifact promising a split that this executor would never
	// perform. Two copies of this ordering would be two things to keep in agreement.
	if base, done := PrePlanBase(l, e.Floor, depth, e.maxDepth()); done {
		st, ferr := e.leaf(ctx, p, l, id, depth, base, from)
		return st, Plan{}, ferr
	}
	// P2, the PRIMARY terminator: recurse only as deep as you have verifiers.
	// A node whose own results cannot be checked must not sit atop a subtree —
	// error compounds along depth, and the merge it would perform is an act of
	// faith no local check can catch. Solve directly instead (base case 1).
	// Checked before planning so an unverifiable node never pays for a plan.
	if e.Verifier != nil && !e.Verifier.AvailableFor(p) {
		st, ferr := e.leaf(ctx, p, l, id, depth, BaseNoVerifier, from)
		return st, Plan{}, ferr
	}

	alloc := Allocation{Spend: l.Balance(), Deadline: deadlineOf(ctx)}
	plan, err := e.Planner.Plan(ctx, p, alloc, depth, nil)
	if err != nil {
		return subtree{}, Plan{}, fmt.Errorf("plan: %w", err)
	}

	// P1: declining to split is legitimate. Solve the whole problem as one leaf.
	if plan.Declined || len(plan.Items) == 0 {
		st, ferr := e.leaf(ctx, p, l, id, depth, BasePlannerDeclined, from)
		return st, plan, ferr
	}

	// DAG (§2): collapse identical sub-problems before fanout so "one call" holds
	// regardless of sibling timing, merging their relative weights.
	plan = dedupePlan(plan)

	// §2/P9: the mechanical, model-free verifier for the planner. A plan whose
	// children fall below floor violates the contract the planner was given (it
	// received the balance and must fit it) — surface it rather than silently
	// degrading to a leaf.
	allocs, err := l.Apportion(plan, e.Floor)
	if err != nil {
		return subtree{}, plan, err // ErrPlanDoesNotFit
	}

	// Time divides along depth: children share a window shortened by the reserve,
	// leaving the parent time to reduce (§3.1).
	childCtx, cancel, err := l.ChildContext(ctx, e.Now)
	if err != nil {
		return subtree{}, plan, err // no time remains to apportion
	}
	defer cancel()

	kids := make([]subtree, len(plan.Items))
	leftover := make([]Units, len(plan.Items)) // unspent child balance, refunded after Wait
	g, gctx := errgroup.WithContext(childCtx)
	for i, item := range plan.Items {
		i, item := i, item
		a := allocs[i] // by POSITION: a problem key is not unique across items
		childLedger, cerr := l.Child(a, childID(id, i), item.Problem.Scope)
		if cerr != nil {
			cancel()
			return subtree{}, plan, cerr // ErrScopeWidens
		}
		// Money is inherited whole per child but reserved from the parent up
		// front, so the parent's balance tracks committed spend (§3). The child
		// returns whatever it does not use.
		if a.Spend.Limited() {
			if derr := l.Reserve(a.Spend); derr != nil {
				cancel()
				return subtree{}, plan, derr
			}
		}
		g.Go(func() error {
			// The POST-dedupe weight: item.Weight already carries any merged siblings'
			// weights, and it is what Apportion funded this child by.
			inherited := parentPlan{arm: plan.IsPortfolio(), weight: item.Weight, id: id, index: i}
			kid, _, ferr := e.node(gctx, item.Problem, childLedger, childID(id, i), depth+1, inherited)
			kids[i] = kid
			leftover[i] = childLedger.Balance() // read own ledger only — no shared writes
			return ferr
		})
	}
	werr := g.Wait()
	// Refund in the main goroutine — the parent ledger is not concurrent-safe.
	for _, b := range leftover {
		if b.Limited() && b > 0 {
			_ = l.Refund(b)
		}
	}
	if werr != nil {
		// A genuine fault, not a budget/time miss. §3.1 mandates partial
		// tolerance only for those two; a hard fault fails the run.
		return subtree{}, plan, fmt.Errorf("child fault: %w", werr)
	}

	// Assemble the reduce inputs and the pre-order outcome list.
	//
	// TWO DISTINCT FLAGS, and conflating them mislabels the record. They were one
	// variable, which made every priced-out subtree report a gap:
	//
	//   partial — the REDUCER's input is incomplete, from any cause. It must hedge.
	//   truncated — this node was cut short by TIME, which is the only thing the
	//     standing ruling (§3.1) lets Gap mean. A child that could not be AFFORDED is
	//     planned degradation, already disclosed as Plan.Excluded at the gate under
	//     P9, and flagging it a gap would report a runtime shortfall that never
	//     happened — while making an unaffordable run indistinguishable from a
	//     deadline miss, which is exactly the distinction the receipt exists to draw.
	//
	// A child's gap DOES propagate: a parent whose child was killed by the deadline is
	// itself time-truncated, and §3.1 requires the record to name that.
	childOutcomes := make([]NodeOutcome, len(kids))
	childIDs := make([]string, len(kids))
	truncated := ctx.Err() != nil
	partial := truncated
	descendants := make([]NodeOutcome, 0, len(kids)+1)
	for i, kid := range kids {
		childOutcomes[i] = kid.outcome
		childIDs[i] = kid.outcome.NodeID
		if kid.outcome.Gap {
			truncated = true
		}
		if kid.outcome.Gap || kid.outcome.Content == "" {
			partial = true
		}
		descendants = append(descendants, kid.outcomes...)
	}

	// The reducer runs under the PARENT context, which still holds the time
	// reserve — it can merge even after the children's window closed.
	reduced, err := e.Reducer.Reduce(ctx, p, childOutcomes, Allocation{Spend: l.Balance()}, partial, plan.Strategy)
	if err != nil {
		return subtree{}, plan, fmt.Errorf("reduce: %w", err)
	}
	if reduced.Cost.Limited() && reduced.Cost > 0 {
		_ = l.Debit(ctx, reduced.Cost)
	}

	o := NodeOutcome{
		NodeID:   id,
		Problem:  p,
		Content:  reduced.Content,
		Cost:     reduced.Cost,
		Depth:    depth,
		Children: childIDs,
		// Gap means TIME, never money — see the two flags above.
		Gap: truncated,
		// The shape this node used, so the record can tell a portfolio from a
		// partition whose children happened to coincide (§2). Plan pinning needs it;
		// see the NodeOutcome field note.
		Strategy: plan.Strategy,
		// The weight this node was itself funded by — a fact about its PARENT's plan,
		// not about the plan it just made.
		PlanWeight: from.weight,
	}
	// An internal node's token counts are the REDUCE call's own, not the subtree's
	// sum: the children already report theirs, and rolling them up would make every
	// ancestor double-count and a tree-wide total meaningless. Its surface-to-volume
	// then measures what it should — how much context the merge paid for.
	o.HaloTokens, o.GeneratedTokens = reduced.HaloTokens, reduced.GeneratedTokens
	e.extractClaims(ctx, &o, reduced)
	// Guarded on completeness, like the leaf path: a merge over a truncated or
	// unfunded child set is not an answer to this problem and must not be served as
	// one. See appendCache.
	e.appendCache(p, reduced, o, partial)
	// Bracketed from before the cache check to after the reduce, so a parent's
	// duration contains its whole subtree's — what span nesting already implies.
	e.stamp(&o, started)
	e.emit(o)

	outcomes := make([]NodeOutcome, 0, len(descendants)+1)
	outcomes = append(outcomes, o)
	outcomes = append(outcomes, descendants...)
	return subtree{outcome: o, sample: reduced, outcomes: outcomes}, plan, nil
}

// leaf solves a node directly, verifies the result, and retries in place on a
// verification failure until it passes, retries run out, or the budget can no
// longer fund another attempt (§5, §3). A non-nil error is a genuine provider
// fault (not a budget/time miss) and fails the run.
//
// This is the Budget(Retry(agent)) decorator order made concrete: each re-solve
// goes through solveLeaf's admission gate, so retries consume budget and stop
// when the reserve is exhausted — retries are never free (§3).
//
// o.Verified records the check's verdict: true/false when a verifier ran, nil
// when none was available. That distinction — unchecked versus checked-and-
// failed — is exactly what the receipt must preserve (§8).
func (e *Executor) leaf(ctx context.Context, p Problem, l *Ledger, id string, depth int, base BaseCase, from parentPlan) (subtree, error) {
	started := e.tick()
	o, s, solved, ferr := e.solveLeaf(ctx, p, l, id, depth, base, from)
	if ferr != nil {
		return subtree{}, ferr
	}

	// Verify only a solve that actually ran. A gap (time/budget miss) produced
	// no sample and has nothing to check; a solve that returned empty content is
	// a bad answer the non-empty oracle exists to catch.
	if solved && e.Verifier != nil && e.Verifier.AvailableFor(p) {
		for {
			passed, ok := e.Verifier.Verify(ctx, p, s)
			if ok {
				o.Verified = &passed
				s.Verified = &passed
			}
			if !ok || passed || o.Retries >= e.MaxRetries {
				break
			}
			// Retry in place. A fresh attempt; solveLeaf admits it against the
			// remaining balance, so an exhausted reserve ends the loop here.
			ro, rs, rSolved, rerr := e.solveLeaf(ctx, p, l, id, depth, base, from)
			if rerr != nil {
				return subtree{}, rerr
			}
			o.Retries++
			if !rSolved {
				// Could not afford or complete another attempt: keep the failed
				// verdict and stop. A budget miss is not a gap; a time miss is.
				o.Gap = ro.Gap
				break
			}
			o.Content, o.Cost, s = ro.Content, o.Cost+ro.Cost, rs
			// Tokens ACCUMULATE across retries exactly as cost does: every attempt
			// really consumed them. Overwriting with the last attempt's counts would
			// under-report the halo a retried node paid for, which is the specific
			// number P1 is judged on — a node that took three attempts really did
			// re-send its context three times, and surface-to-volume must show it.
			o.HaloTokens += ro.HaloTokens
			o.GeneratedTokens += ro.GeneratedTokens
		}
	}

	e.extractClaims(ctx, &o, s)
	e.appendCache(p, s, o, false)
	e.stamp(&o, started)
	e.emit(o)
	return subtree{outcome: o, sample: s, outcomes: []NodeOutcome{o}}, nil
}

// solveLeaf runs one leaf: pre-call admission, solve, post-call debit.
//
// The decorator order is Budget(Retry(agent)) (§3): the budget wraps the call so
// the spend is metered; the call does not police itself. Retry headroom is
// funded by the reserve and lands in step 4 with the verifier.
//
// Returns the outcome, the sample, whether a solve actually ran (solved=false
// when admission or time refused the call, distinct from a solve that returned
// empty content), and a non-nil error ONLY for a genuine provider fault. A
// budget or time miss is not an error — it is recorded in the outcome (Gap iff
// time) and the run continues (§3.1).
func (e *Executor) solveLeaf(ctx context.Context, p Problem, l *Ledger, id string, depth int, base BaseCase, from parentPlan) (o NodeOutcome, s Sample, solved bool, err error) {
	o = NodeOutcome{NodeID: id, Problem: p, Depth: depth, BaseCase: base, PlanWeight: from.weight}

	// Pre-call gate. A miss here is either time (a gap) or spend (planned
	// degradation, not a gap) — the standing ruling.
	est := Units(0)
	if e.Estimate != nil {
		est = e.Estimate(p)
	}
	if aerr := l.Admit(ctx, est); aerr != nil {
		o.Gap = ctx.Err() != nil
		return o, Sample{}, false, nil
	}

	s, err = e.Solver.Solve(ctx, p, Allocation{Spend: l.Balance(), Deadline: deadlineOf(ctx)})
	if err != nil {
		// ErrRecordedGap is a REPLAY of a time miss, and it needs its own test because
		// the live one cannot see it: a replay runs with no deadline, so ctx.Err() is nil
		// and a recorded gap would propagate as a provider fault and fail the whole run.
		// A partial record must replay as partial — that is the same guarantee, restated
		// for the case where the truncation is history rather than happening now (§3.1).
		if ctx.Err() != nil || errors.Is(err, ErrRecordedGap) {
			o.Gap = true // time-induced failure — returnable-now, not fatal
			return o, Sample{}, false, nil
		}
		// A recorded UNFUNDED node replays with NO Gap flag, which is the whole point of
		// its being a separate sentinel. The live equivalent is the Admit miss above: it
		// returns an outcome with empty content, no model and no verdict, and that is
		// exactly the shape this must reproduce. Setting Gap here would relabel spend
		// degradation as time truncation and change what Extend offers the run (§8.1).
		if errors.Is(err, ErrRecordedUnfunded) {
			return o, Sample{}, false, nil
		}
		return o, Sample{}, false, err // genuine provider fault — propagates
	}

	// Record actuals. Debit re-admits against the balance; in the normal path
	// (actual ≈ estimate) it succeeds. An overrun discovered post-call is only a
	// gap if time expired mid-call.
	if derr := l.Debit(ctx, s.Cost); derr != nil && ctx.Err() != nil {
		o.Gap = true
	}
	o.Content = s.Content
	o.Cost = s.Cost
	o.Verified = s.Verified
	o.Model = s.Model
	o.ModelVersion = s.ModelVersion
	o.HaloTokens, o.GeneratedTokens = s.HaloTokens, s.GeneratedTokens
	return o, s, true, nil
}

// stamp brackets a node's wall-clock into its outcome (§9). A no-op when no Clock
// is injected, so timing is absent rather than zero.
//
// Called around the node's OWN work. On an internal node that includes its
// children's time, which is what a span tree wants: a parent's duration should
// contain its subtree's, exactly as nesting implies.
func (e *Executor) stamp(o *NodeOutcome, started time.Time) {
	if e.Clock == nil || started.IsZero() {
		return
	}
	o.Timing = NodeTiming{StartedAt: started, EndedAt: e.Clock()}
}

// tick reads the injected clock, or the zero time when none is wired. Separate
// from stamp so a caller can take the start instant before the work begins.
func (e *Executor) tick() time.Time {
	if e.Clock == nil {
		return time.Time{}
	}
	return e.Clock()
}

func (e *Executor) readServe(p Problem, n int) bool {
	if e.ReadPolicy == nil {
		return true // serve-when-warm; a cold entry never reaches here
	}
	return e.ReadPolicy(p, n) == ReadServe
}

// appendCache stores a completed result, and ONLY a completed one.
//
// An incomplete result must never be cached, because a cache entry cannot express
// incompleteness: a served hit copies Content and sets CacheHit, and nothing
// restores Gap or the empty-because-unfunded condition. Storing a partial answer
// therefore launders it into a complete one for every later reader — and it is
// exactly the node an extend run (§8.1) needs to re-solve, so the most incomplete
// node in the tree would be the one most confidently served, and extend would
// reliably refill nothing.
//
// partial is the caller's judgement that the result rests on incomplete input. It
// is a SEPARATE argument from the outcome's own emptiness because a merge over one
// of two children is the dangerous case: it has real content and, since only time
// is a gap (§3.1), no Gap flag either — so it looks complete from the outcome alone
// while being precisely the half-answer that must not be served as whole. Leaves
// pass false: a leaf answers its problem or it does not, and has no partial state.
//
// The gate lives here rather than at each call site because the leaf path had it
// and the internal-node path did not, which is the kind of asymmetry that survives
// precisely because both call sites look correct in isolation.
func (e *Executor) appendCache(p Problem, s Sample, o NodeOutcome, partial bool) {
	if e.Cache == nil || partial || o.Gap || o.Content == "" {
		return
	}
	e.Cache.Append(p, s, nil, e.Now) // fake provider retrieves nothing; sources land with real retrieval
}

func (e *Executor) maxDepth() int {
	if e.MaxDepth <= 0 {
		return DefaultMaxDepth
	}
	return e.MaxDepth
}

// emit publishes a COMPLETED node to both readers: the aggregating sink (§8.2) and
// the live observer (§9).
//
// Both fire from this one funnel deliberately. Every completion path in node() and
// leaf() already routes through here, so pairing Exit with Enter is structural
// rather than a matter of having found every return statement — and a missing Exit
// would leave a viewer showing a node as permanently in-flight.
func (e *Executor) emit(o NodeOutcome) {
	if e.Sink != nil {
		e.Sink.Node(o)
	}
	if e.Observer != nil {
		e.Observer.Exit(o)
	}
}

// enter publishes the entry event. Separate from emit because the two carry
// different types for a real reason: entry knows the ALLOCATION and not the result,
// completion knows the result and no longer knows the allocation.
func (e *Executor) enter(ev NodeEnter) {
	if e.Observer != nil {
		e.Observer.Enter(ev)
	}
}

// extractClaims populates o.Claims from a sample's content (§7, step 6). No-op
// when no extractor is wired or the content is empty — a gap or priced-out node
// has nothing to assert. Runs on replay too, so it must be pure; an extraction
// error is swallowed rather than failing the run, because a claim is provenance
// on top of an answer that already exists, not a precondition for returning it.
func (e *Executor) extractClaims(ctx context.Context, o *NodeOutcome, s Sample) {
	if e.Extractor == nil || s.Content == "" {
		return
	}
	if claims, err := e.Extractor.Extract(ctx, s, o.NodeID); err == nil {
		o.Claims = claims
	}
}

// dedupePlan collapses items sharing a (statement, scope) key, summing their
// relative weights and keeping first-occurrence order. This is the DAG rule made
// mechanical: identical sub-problems become one child (§2).
func dedupePlan(p Plan) Plan {
	if len(p.Items) < 2 {
		return p
	}
	// A PORTFOLIO's arms are the same problem on purpose (§2), so deduping one would
	// collapse N competing attempts into a single call and silently turn the strategy
	// into its opposite. The DAG win and the portfolio shape make contradictory
	// assumptions about what an identical statement means, so the strategy — not the
	// items — decides.
	if p.IsPortfolio() {
		return p
	}
	out := Plan{Excluded: p.Excluded, Declined: p.Declined, Strategy: p.Strategy, Reasoning: p.Reasoning}
	seen := make(map[string]int, len(p.Items))
	for _, it := range p.Items {
		k := it.Problem.Key()
		if idx, ok := seen[k]; ok {
			out.Items[idx].Weight += it.Weight
			continue
		}
		seen[k] = len(out.Items)
		out.Items = append(out.Items, it)
	}
	return out
}

// DedupePlan is dedupePlan, exported for the plan gate (#15).
//
// THE GATE MUST COLLAPSE WHAT THE RUN WILL COLLAPSE, or the artifact promises a fanout
// the executor does not perform: an approved plan showing five children that dedupes to
// three at run time would apportion differently, and D1's apportionment check would
// refuse the run over a difference the gate itself introduced. Same function, so the two
// cannot disagree.
func DedupePlan(p Plan) Plan { return dedupePlan(p) }

// boundBy names the cap that bit during THIS RUN, which is not the same question
// BoundBy(ctx, l) answers.
//
// FOUND BY RUNNING THE BINARY. A 4-node fake run at --deadline 60ms gapped every
// single node and recorded BoundBy "" — because the exported BoundBy inspects the ROOT
// context, and the root's window is the whole cap while a child's is a slice of it
// (ChildContext, §3.1). The root therefore finishes comfortably inside a deadline that
// truncated everything below it, so the field meant to say "time bound this run" was
// empty on the most time-bound run the system can produce.
//
// That is not only a cosmetic record defect. Extend compares caps ONLY in the binding
// denomination (iterate.go), precisely so more money cannot be offered to refill a node
// that ran out of time — and an empty BoundBy falls through to the unknown-binding
// branch, which accepts any raise. A purely time-truncated run was accepting a spend
// raise that could not possibly help it.
//
// The ctx/ledger reading stays FIRST: if the root itself expired or the balance is
// actually exhausted, that is the more direct evidence and it distinguishes spend from
// latency. The gap sweep is the fallback, and it can only ever conclude "latency" —
// under the standing ruling only TIME produces a gap (§3.1), so a gap anywhere in the
// tree is proof the clock bit somewhere even when it did not bite at the root.
//
// Spend has no equivalent fallback and deliberately gets none: an unfunded node is
// planned degradation with a positive balance remaining, so inferring DenomSpend from
// it would report a cap as binding when the run simply declined to spend more on that
// branch. Truncated() already catches that case with its own third signal.
func boundBy(ctx context.Context, l *Ledger, outs []NodeOutcome) Denomination {
	if d := BoundBy(ctx, l); d != "" {
		return d
	}
	for _, o := range outs {
		if o.Gap {
			return DenomLatency
		}
	}
	return ""
}

func childID(parent string, i int) string { return parent + "." + strconv.Itoa(i) }

func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}
