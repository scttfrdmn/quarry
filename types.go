// Package quarry implements bounded recursive decomposition with verified
// provenance.
//
// See docs/design.md for the full design. The nine principles P1-P9 are
// load-bearing; section references appear in doc comments as (§n).
//
// Nothing in this package imports an AWS SDK, touches the network, or reads the
// clock. Time enters through context.Context and through explicit now
// parameters. That is what keeps build steps 1-7 runnable with no AWS and no
// LLM.
package quarry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Units is normalized budget, in micro-units.
//
// Integer rather than float deliberately. Apportionment divides a balance by
// relative weights, and float division makes the result sensitive to summation
// order — two replays of the same tree could allocate differently in the last
// bits and diverge. Under P8 the record must replay exactly, so ledger
// arithmetic is exact and deterministic, with remainders distributed by an
// explicit rule (see Apportion).
//
// The unit of account is deliberately not dollars; see the open question in
// docs/design.md §12.
type Units int64

// Unlimited marks a cap that is not set. A run must carry at least one real cap
// (P9): planning is budget-conditioned, so an uncapped run has nothing to plan
// against.
const Unlimited Units = -1

// Limited reports whether this is a real cap rather than Unlimited. Callers must ask
// before comparing: Unlimited is -1, so `spend < cap` silently treats an uncapped run
// as a zero budget.
func (u Units) Limited() bool { return u >= 0 }

// String renders micro-units as a 4-dp quantity, or "unlimited". Fixed precision
// rather than %g: a receipt column that changes width between runs is unreadable.
func (u Units) String() string {
	if !u.Limited() {
		return "unlimited"
	}
	return fmt.Sprintf("%.4f", float64(u)/1e6)
}

// FromFloat converts a human-facing quantity into micro-units.
func FromFloat(f float64) Units { return Units(f * 1e6) }

// Denomination identifies which kind of cap bound a run (§3.1, §8.2).
type Denomination string

// The three denominations a cap can be expressed in. DenomDue is a DEADLINE as an
// instant, distinct from DenomLatency's duration: extending a run needs to know which
// one bound it, because raising the wrong one buys nothing (§8.1).
const (
	DenomSpend   Denomination = "spend"
	DenomLatency Denomination = "latency"
	DenomDue     Denomination = "due"
)

// BaseCase records why a node stopped recursing (§2), for telemetry (§8.2).
type BaseCase string

// The four reasons a node stops recursing. BaseNoVerifier is the PRIMARY terminator
// (P2: recurse only as deep as you have verifiers); BaseMaxDepth is the backstop, and a
// run bounded by it is under-verified rather than complete.
const (
	BaseNoVerifier      BaseCase = "no_verifier"
	BasePlannerDeclined BaseCase = "planner_declined"
	BaseBelowFloor      BaseCase = "below_floor"
	BaseMaxDepth        BaseCase = "max_depth"
)

// Mode records how a run relates to its predecessor (§8.1).
type Mode string

// The three modes. ModeExtend raises a cap and continues; ModeRefine re-plans from a
// prior decomposition — and the two are recorded separately because refine shows the
// planner a previous answer, which §8.1 names as anchoring: it biases the planner
// toward the same shape, and §7 calls an independent decomposition the strongest
// replication signal there is. Which mode produced a record changes how much its
// agreement with its predecessor is worth.
const (
	ModeFresh  Mode = "fresh"
	ModeExtend Mode = "extend"
	ModeRefine Mode = "refine"
)

// Scope carries identity and entitlement tags from agate (P6).
//
// Travels with the Ledger so authority and budget cannot drift apart, and forms
// part of every cache key. A child's scope is its parent's scope or narrower,
// never wider.
type Scope struct {
	Tags map[string]string
}

// Key renders the scope canonically for hashing.
func (s Scope) Key() string {
	keys := make([]string, 0, len(s.Tags))
	for k := range s.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s;", k, s.Tags[k])
	}
	return b.String()
}

// NarrowsTo reports whether other is this scope or narrower — the P6 check.
func (s Scope) NarrowsTo(other Scope) bool {
	for k, v := range s.Tags {
		if other.Tags[k] != v {
			return false
		}
	}
	return true
}

// Caps are the constraints a run must satisfy (§3.1, P9).
//
// Any subset may be present, but not none. Zero-valued Deadline and Due mean
// unset; use Unlimited for Spend.
type Caps struct {
	Spend   Units
	Latency time.Duration
	Due     time.Time
}

// Validate enforces that at least one cap is real (P9).
func (c Caps) Validate() error {
	if c.Spend.Limited() && c.Spend <= 0 {
		return fmt.Errorf("spend cap must be positive")
	}
	if c.Latency < 0 {
		return fmt.Errorf("latency cap must be positive")
	}
	if !c.Spend.Limited() && c.Latency == 0 && c.Due.IsZero() {
		return fmt.Errorf("at least one cap is required (P9)")
	}
	return nil
}

// Deferrable reports whether slack is convertible into money (§3.1).
//
// A due date without a latency cap means the run is not needed soon, so batch
// inference, off-peak, and deferred execution are available. Giving up fast
// mechanically buys cheap — the deadline field is a price control, not merely a
// scheduling field.
func (c Caps) Deferrable() bool { return !c.Due.IsZero() && c.Latency == 0 }

// Problem is a statement to be solved, under a scope.
type Problem struct {
	Statement string
	Scope     Scope
}

// Key is the content address, scope-qualified (§6).
//
// NOT the statement hash alone. Two users can pose a hash-identical sub-problem
// while holding different entitlements, and one's cached answer may derive from
// documents the other cannot see. Serving across that line walks straight
// through the ABAC boundary (P6).
//
// TODO(§12): normalization is unresolved. Exact match is the conservative
// placeholder — it under-hits rather than over-hits, which is the correct
// direction to be wrong in.
func (p Problem) Key() string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(p.Statement) + "\x00" + p.Scope.Key()))
	return hex.EncodeToString(sum[:])
}

// PlanItem is one proposed child (§2).
//
// Weight is RELATIVE, not an absolute cost. Relative estimation is far more
// reliable than absolute — the same reason story points beat hour estimates —
// and it sidesteps the calibration gap that makes cost estimation weak (§4).
type PlanItem struct {
	Problem    Problem
	Weight     int64
	ExpectLeaf bool
	Rationale  string
}

// Strategy names the SHAPE a plan asks for (§2 "Alternatives considered").
//
// Partition and portfolio differ in what identical child statements MEAN, which is
// why the shape has to be declared rather than inferred. Under partition, two
// identical sub-problems are redundant work and collapse to one call (the DAG win,
// §2). Under portfolio they are the entire point — N independent attempts at the
// same problem, selected among rather than merged — so collapsing them would
// destroy the strategy. No amount of looking at the items distinguishes the two
// cases; only the planner's intent does.
type Strategy string

const (
	// StrategyPartition splits a problem into DIFFERENT sub-problems and merges
	// their answers. The default, and the zero value: an unset Strategy is a
	// partition, so every planner written before strategies existed still means
	// what it meant.
	StrategyPartition Strategy = ""

	// StrategyPortfolio makes N attempts at the SAME problem and SELECTS one
	// (§2: "strictly better when selection is easier than generation… the natural
	// fallback when P1 says don't decompose, and should be a first-class strategy
	// the planner can choose").
	//
	// Three consequences the executor must honour, all following from the arms
	// being the same problem:
	//   - Arms are NOT deduped. dedupePlan skips portfolios entirely.
	//   - Arms do NOT read the cache. A served arm is a copy of another arm, not an
	//     independent attempt — the precise way a cache "saves money by destroying
	//     replication" (§6, P7). Arms still WRITE, so the entry accumulates N real
	//     samples, which is what P7 wants.
	//   - The Reducer SELECTS rather than merges. Concatenating five attempts at one
	//     question produces five answers, not an answer.
	StrategyPortfolio Strategy = "portfolio"
)

// Plan is a planner's output: a split, an apportionment, and a disclosure.
//
// Excluded is what the cap could not cover. Under P9 degradation is planned and
// disclosed before spend rather than discovered at minute fifteen; the plan gate
// (§9) shows it so the researcher can raise the cap or accept reduced scope.
type Plan struct {
	Items    []PlanItem
	Excluded []string
	Declined bool

	// Strategy is the shape (§2). Zero value = StrategyPartition, so this field is
	// backward-compatible by construction.
	Strategy Strategy

	Reasoning string
}

// IsPortfolio reports whether the plan's arms are competing attempts at one
// problem rather than a partition of it. Read it wherever identical child
// statements would otherwise be treated as redundant.
func (p Plan) IsPortfolio() bool { return p.Strategy == StrategyPortfolio }

// TotalWeight sums the relative weights.
func (p Plan) TotalWeight() int64 {
	var t int64
	for _, i := range p.Items {
		t += i.Weight
	}
	return t
}

// Allocation is what a node is given to work with, in both denominations (§3.1).
type Allocation struct {
	Spend    Units
	Deadline time.Time
}

// Claim is an assertion extracted from a result, traceable to its origin (§8).
//
// TODO(§7, §12): claim extraction is the highest-risk unbuilt piece. This shape
// is provisional and will change once extraction is prototyped.
type Claim struct {
	Text string
	// Norm is the canonical form the extractor reduced Text to for comparison.
	// Pinned into the record so equivalence replays under the normalization that
	// produced it, not whatever the normalizer does later (P8). See claim.go.
	Norm    string
	NodeID  string
	Sources []string
	Stable  *bool // nil = not yet assessed across replicates
}

// Sample is one result for one (problem, scope) key.
//
// Cache entries accumulate these rather than storing a single answer (§6): a hit
// returns the distribution, a fresh run appends. Repeated runs increase n and
// tighten error bars instead of echoing the first result back — which is what P7
// requires and a naive cache destroys.
type Sample struct {
	Content         string
	Cost            Units
	Model           string
	ModelVersion    string // explicit, never an alias (P8)
	CreatedAt       time.Time
	HaloTokens      int
	GeneratedTokens int
	Verified        *bool
}

// SurfaceToVolume makes P1 observable rather than a judgement call (§8.2).
//
// Halo is context replicated into the node; generated is what it produced. A
// high ratio means the node paid for its parent's context and did little with
// it — evidence the split was not worth making. Returns ok=false when nothing
// was generated.
func (s Sample) SurfaceToVolume() (ratio float64, ok bool) {
	if s.GeneratedTokens == 0 {
		return 0, false
	}
	return float64(s.HaloTokens) / float64(s.GeneratedTokens), true
}

// NodeOutcome is what a node returned, plus what it cost and how it terminated.
type NodeOutcome struct {
	NodeID   string
	Problem  Problem
	Content  string
	Cost     Units
	Depth    int
	BaseCase BaseCase
	CacheHit bool
	Verified *bool

	// Model and ModelVersion name what produced a leaf's content. Empty on
	// internal (reduced) nodes and cache hits. Explicit versions, never aliases:
	// a record that cannot name its producer is not replayable (§8, P8).
	Model        string
	ModelVersion string

	Retries  int
	Children []string
	Claims   []Claim
	Gap      bool // truncated or unreturnable — named, never silent (§3.1)

	// Strategy is the shape of the plan this node used (§2). Empty on leaves, which
	// used no plan, and on partitions, whose strategy IS the empty zero value.
	//
	// Recorded because without it the record cannot distinguish a portfolio from a
	// partition that happened to propose identical children — and those are opposite
	// claims about the run. Plan pinning (§7) proved the need concretely: a pinned
	// re-run that lost the strategy re-ran three arms as one deduped child, cutting
	// the work to a third while reporting a faithful replay of the shape. An
	// experimental control that silently degrades the thing it controls for is worse
	// than no control.
	Strategy Strategy

	// PlanWeight is the RELATIVE weight the parent's plan assigned to this node —
	// the number the Ledger turned into this node's share of the budget (§3).
	//
	// Zero on the root, which no plan funded, and on any node from a record written
	// before weights were recorded. Zero therefore means UNRECORDED, not "weightless":
	// a plan item's weight must be positive, so zero cannot be a real weight.
	//
	// Recorded because plan pinning (§7) is an experimental control, and a control
	// must hold fixed everything it claims to. Without the weight a pinned re-run
	// reproduced the recorded SHAPE while apportioning the budget uniformly across it
	// — so a spread between run and re-run could come from the different split of
	// money rather than from the solving, which is the one thing pinning exists to
	// isolate. Same class of defect as the missing Strategy, found the same way.
	//
	// This is the POST-dedupe weight (§2): when identical sub-problems collapse into
	// one child their weights sum, and the sum is what actually funded the node. A
	// replay wants the number that was spent against, not the one first proposed.
	PlanWeight int64

	// HaloTokens and GeneratedTokens carry the token split onto the outcome, so
	// surface-to-volume (P1, §8.2) is computable from a NodeOutcome alone. They
	// previously lived only on Sample, which meant a TelemetrySink saw cost but
	// could not see the ONE ratio that makes P1 observable — the observer could not
	// reach the metric it exists to report.
	//
	// These are hashed like every other field: token counts are a deterministic
	// property of a call, so a replay that produces different ones has genuinely
	// diverged and the record SHOULD say so.
	//
	// Zero on internal (reduced) nodes and gaps — no model call was made. A cache
	// hit carries the stored split, because the tokens were really spent once and
	// the entry records what they were.
	HaloTokens      int
	GeneratedTokens int

	// Timing is wall-clock, and is DELIBERATELY EXCLUDED FROM THE HASH (P8) — see
	// NodeTiming. It is the one thing a replay cannot reproduce.
	Timing NodeTiming `json:"-"`
}

// NodeTiming is a node's wall-clock measurement, held apart from the hashed
// record because a duration is the one field replay can never reproduce (P8).
//
// The tension is real and worth stating rather than hiding. §9 needs per-node
// latency for a live tree and for anything resembling performance work. But two
// replays of the same tree MUST produce identical bytes, and no two runs take the
// same time — so a duration inside the hashed record would make every replay
// "diverge" and destroy the one guarantee the record exists to give.
//
// The resolution: timing lives on the outcome (so telemetry and a tree view can
// read it) but is tagged `json:"-"` on the field above (so the canonical encoding
// never sees it). This is the "separate channel that is never hashed" named in
// §12, made concrete. The cost of that choice, stated plainly: **timing is
// therefore NOT part of the citable artifact** — a record proves what was spent
// and decided, never how long it took. Anyone wanting durable timing must persist
// them alongside the record, not inside it.
//
// Zero when no clock was injected, which is the normal case for the no-AWS
// no-LLM tests: an absent measurement reads as absent, not as instantaneous.
type NodeTiming struct {
	// StartedAt and EndedAt bracket the node's own work. Both come from the
	// injected Executor.Now-style clock, never time.Now() (Go rule 4).
	StartedAt time.Time
	EndedAt   time.Time
}

// Duration is the node's elapsed wall-clock, and ok=false when it was never
// measured. A bare zero would be indistinguishable from a genuinely instantaneous
// node, and reporting an unmeasured node as "0ms" is the same class of lie as
// fabricating a span timestamp.
func (t NodeTiming) Duration() (d time.Duration, ok bool) {
	if t.StartedAt.IsZero() || t.EndedAt.IsZero() || t.EndedAt.Before(t.StartedAt) {
		return 0, false
	}
	return t.EndedAt.Sub(t.StartedAt), true
}

// SurfaceToVolume makes P1 observable from an outcome alone (§8.2). Same
// semantics as Sample.SurfaceToVolume: ok=false when nothing was generated, so an
// internal node or a gap reports "no ratio" rather than a misleading zero.
func (o NodeOutcome) SurfaceToVolume() (ratio float64, ok bool) {
	if o.GeneratedTokens == 0 {
		return 0, false
	}
	return float64(o.HaloTokens) / float64(o.GeneratedTokens), true
}

// PriorRef pins a learned prior into the record.
//
// If the planner's behaviour depends on a prior and the prior drifts, replay
// breaks silently even though the transcript looks intact. Learned state is part
// of the reproducibility envelope (§8.2, P8).
type PriorRef struct {
	Name    string
	Version string
}

// RunRecord is the citable artifact (§8, P8).
//
// Self-sufficient: replayable without model access, indefinitely.
type RunRecord struct {
	RunID               string
	Problem             Problem
	Caps                Caps
	Mode                Mode
	Outcomes            []NodeOutcome
	Priors              []PriorRef
	ParentRun           string       // lineage across refine (§8.1)
	LineOfInquiry       string       // cumulative accounting (§8.1)
	BoundBy             Denomination // which cap actually bit (§8.2)
	Unverified          []string     // what was NOT checked — required (§8)
	RegressTerminatedAt string

	// Adversarial records the surplus-budget passes (§3 Surplus, §5): what was
	// attacked, by whom, and what broke. A broken claim is a high-value refine
	// signal alongside the unstable-claim list (§7). Empty when no surplus ran.
	Adversarial []AdversarialFinding

	// Bounds records the EXECUTOR PARAMETERS that shaped this tree, so a replay can
	// re-execute under the same rules (§7, P8).
	//
	// THIRD INSTANCE OF ONE DEFECT, which is why it is a recorded field rather than a
	// third derivation in the CLI. BoundBy, the depth bound and the floor were each
	// re-derived by replay from the tree's geometry, and each was really a fact of the
	// original EXECUTION: the depth cap is only visible if some node hit it, and the floor
	// is not visible at all. A replay configured differently re-executes a different
	// algorithm and reports the difference as a divergence in the record.
	//
	// The record is supposed to be self-sufficient (P8) — replayable without asking the
	// environment anything — and a knob that changes which base case a node takes is
	// exactly what that promise covers.
	Bounds RunBounds
}

// RunBounds are the executor settings a replay must reproduce to re-execute the same
// tree. Recorded rather than inferred: see RunRecord.Bounds.
//
// Deliberately NOT the caps, which have their own field and a different meaning: caps are
// the contract with the user (P4), while these are the algorithm's configuration. A
// reader comparing two runs wants to know the difference between "same rules, less money"
// and "different rules".
type RunBounds struct {
	// MaxDepth is the recursion backstop actually in force (P2). Zero means the record
	// predates this field; a replay then falls back to inferring it, which is correct for
	// every run that never reached the bound.
	MaxDepth int

	// Floor is the smallest allocation worth giving a child (§3). Zero legitimately means
	// "no floor", which is indistinguishable from an old record — acceptable because a
	// zero floor is also the value that makes BaseBelowFloor unreachable, so inferring
	// wrongly costs nothing that was observable.
	Floor Units

	// MaxRetries is the per-leaf re-solve budget (§5). Recorded for the same reason as the
	// other two: a replay with a different retry count can re-solve a node the original
	// gave up on.
	MaxRetries int
}

// TotalCost sums node costs.
func (r RunRecord) TotalCost() Units {
	var t Units
	for _, o := range r.Outcomes {
		t += o.Cost
	}
	return t
}

// Gaps returns nodes that could not be completed.
func (r RunRecord) Gaps() []NodeOutcome {
	var out []NodeOutcome
	for _, o := range r.Outcomes {
		if o.Gap {
			out = append(out, o)
		}
	}
	return out
}

// Unfunded returns the nodes the cap could not afford: they reached no model and
// produced nothing, but they are NOT gaps, because only time gaps (§3.1).
//
// The discriminator is the absence of a MODEL, and it is the only thing separating an
// unfunded node from one that was solved and answered emptily. An empty answer is a
// result; conflating the two erases the distinction §8 exists to preserve, which is why
// the verdict is checked too — a solved-then-verified-empty node stays out.
//
// Named as an accessor because three callers need exactly this set and each derived it
// separately: RecordedProvider's index, Truncated, and `quarry replay`'s
// is-there-anything-to-replay guard. The third had it WRONG — it excused gaps only, so a
// record whose every node was priced out was refused as unreplayable when it replays
// perfectly well and makes no model call doing it.
func (r RunRecord) Unfunded() []NodeOutcome {
	var out []NodeOutcome
	for _, o := range r.Outcomes {
		if o.Gap || o.CacheHit || len(o.Children) > 0 {
			continue
		}
		if o.Model == "" && o.Content == "" && o.Verified == nil {
			out = append(out, o)
		}
	}
	return out
}

// Truncated reports whether the run stopped short of what it set out to do — the
// precondition for an extend rather than a refine (§8.1).
//
// BROADER THAN Gaps, and it has to be. Under the standing ruling only TIME is a gap
// (§3.1): a node that could not be AFFORDED is planned degradation, recorded with
// empty content and no Gap flag. So a run that hit its spend cap and dropped half
// its children has no gaps at all, while being the clearest possible case for
// extend. Deciding extend-versus-refine on Gaps alone would send exactly the
// spend-truncated runs — the common case, since spend is the cap researchers
// actually set — down the refine path, re-planning a decomposition that was never
// given the money to prove itself.
//
// Three signals, any one sufficient:
//
//   - a gap: time truncation, named directly (§3.1);
//   - BoundBy set: a cap actually bit (§8.2);
//   - a node with neither content nor children and no verdict: it produced nothing
//     and reduced nothing, which is what an unfunded node looks like.
//
// The third is what catches spend truncation in a record whose BoundBy was never
// populated. It deliberately does not fire on a node that merely returned an empty
// answer while carrying a verdict — that node was solved and checked, and its
// emptiness is a result, not a shortfall.
func (r RunRecord) Truncated() bool {
	if r.BoundBy != "" {
		return true
	}
	for _, o := range r.Outcomes {
		if o.Gap {
			return true
		}
	}
	// The third signal, via the accessor rather than a second copy of the predicate: an
	// unfunded node produced nothing and reduced nothing, which is spend truncation in a
	// record whose BoundBy was never populated.
	return len(r.Unfunded()) > 0
}

// CostPerVerifiedClaim is the only cost ratio safe to optimize against (§8.2).
//
// Cost per run is trivially gamed by doing less: shallower trees, fewer
// verifications and more cache hits all improve it while degrading exactly what
// this system exists to protect. Quality must be in the denominator.
func (r RunRecord) CostPerVerifiedClaim() (Units, bool) {
	var n int64
	for _, o := range r.Outcomes {
		if o.Verified != nil && *o.Verified {
			n += int64(len(o.Claims))
		}
	}
	if n == 0 {
		return 0, false
	}
	return r.TotalCost() / Units(n), true
}
