package quarry

import (
	"context"
	"time"
)

// The seams. Every one is a small interface so the core stays pure and testable.
//
// Implementations may reach the network; nothing in this package does.

// Provider is a model endpoint — the only thing in the system that costs money.
type Provider interface {
	// Complete runs a prompt. ModelVersion on the returned Sample must be
	// explicit, never an alias (P8): a record that cannot name what produced it
	// is not replayable.
	Complete(ctx context.Context, prompt string, model string, scope Scope) (Sample, error)

	// Estimate gives a pre-call cost for admission control.
	Estimate(prompt string, model string) Units
}

// Planner decomposes a problem, given a balance.
//
// Receives the allocation and MUST return a plan that fits it (P9). The planner
// is the error concentrator: it does the hardest reasoning with the least
// information, and every node below inherits its mistakes. Verify it hardest
// (P3).
//
// Emits RELATIVE weights; the Ledger converts them to currency (§2, §3).
// Declining to split is a legitimate outcome when surface-to-volume does not
// favour decomposition, or when the balance will not fund children plus their
// verification (P1).
//
// MUST be safe for concurrent use, for the same reason TelemetrySink must:
// recursion means sibling subtrees plan on separate goroutines at the same time.
// A planner holding mutable per-call state (a counter, a cached prompt, a reused
// buffer) races the moment the tree is deeper than one level — and a racy planner
// corrupts the SHAPE of the run, which every node below inherits.
type Planner interface {
	// Prior carries the distilled predecessor on refine (§8.1).
	//
	// TODO(§12): anchoring is unresolved. Passing the prior tree shape biases
	// the new plan toward the old one, and §7 names an independent
	// decomposition as the strongest replication signal — so iteration is the
	// mechanism most likely to destroy the best evidence available. Prefer
	// passing findings and difficulty actuals; withhold shape until settled.
	Plan(ctx context.Context, p Problem, alloc Allocation, depth int, prior []NodeOutcome) (Plan, error)
}

// Solver answers a leaf problem directly.
type Solver interface {
	Solve(ctx context.Context, p Problem, alloc Allocation) (Sample, error)
}

// Reducer merges children into an answer — or SELECTS among them, depending on the
// strategy it is handed.
//
// MUST be a different agent from the Planner — it needs to see what returned
// without inheriting the priors that produced the split (§2).
//
// MUST tolerate partial input. You can stop spending, but you cannot stop time:
// on deadline expiry whatever exists has to be returnable now. The tree holds a
// returnable answer at all times, degrading in quality rather than sitting in an
// unreturnable intermediate state (§3.1).
//
// MUST be safe for concurrent use, like Planner: sibling subtrees reduce on
// separate goroutines whenever the tree is deeper than one level.
type Reducer interface {
	// Reduce folds children under the plan's strategy.
	//
	// The strategy parameter is load-bearing and cannot be inferred: a partition's
	// children are different sub-problems whose answers COMBINE, while a portfolio's
	// are competing attempts at one problem among which exactly one must be CHOSEN
	// (§2). The children look the same either way. Concatenating five attempts at one
	// question yields five answers, not an answer — so a reducer that ignores this
	// argument is silently wrong on every portfolio.
	Reduce(ctx context.Context, p Problem, children []NodeOutcome, alloc Allocation, partial bool, strategy Strategy) (Sample, error)
}

// Verifier checks a result. Verifier availability, not max depth, is the primary
// termination condition (P2).
//
// CostRatio is what makes "good" a specifiable axis at all: quality expressed as
// verification spend relative to generation (§5). Mechanical oracles report ~0
// and should always run.
type Verifier interface {
	Name() string
	CostRatio() float64
	AvailableFor(p Problem) bool

	// Verify returns ok=false when the result could not be assessed, which is
	// distinct from a failed check. The receipt must be able to say what was NOT
	// checked (§8).
	Verify(ctx context.Context, p Problem, s Sample) (passed bool, ok bool)
}

// Cache is the content-addressed sub-problem store (§6).
//
// Entries ACCUMULATE samples rather than holding one answer. A hit returns the
// distribution; a fresh run appends. Otherwise the cache saves money by
// destroying replication — the second run stops being an independent sample,
// which is exactly what P7 needs it to be.
//
// Keys are scope-qualified. Never key on the statement hash alone (P6).
// EVERY method that can observe expiry takes now, and for the usual reason: no
// implementation may read the clock itself (Go rule 4), so the caller supplies it.
// That includes Append, which starts the TTL clock, and N, which must agree with
// Get about what the store holds — an N that counts samples Get will not return
// makes a broken cache look like a working one.
type Cache interface {
	Get(p Problem, now time.Time) []Sample

	// Append records a sample, as of now. sources are the document versions it
	// depended on, so the entry can be invalidated when they change (§6).
	//
	// now is the STORE's clock, not the sample's provenance: Sample.CreatedAt is
	// stamped by whoever made the model call and is legitimately zero for any solver
	// that does not stamp it. Deriving expiry from it made every unstamped sample
	// expire on write.
	Append(p Problem, s Sample, sources []string, now time.Time)

	Invalidate(source string) int
	N(p Problem, now time.Time) int
}

// TelemetrySink aggregates over run records (§8.2).
//
// The instrumentation is already required by P8 — this is not new
// instrumentation, it is a second reader of artifacts kept anyway. On from run
// one.
//
// GOODHART: never emit an efficiency ratio without a quality denominator. Cost
// per run is trivially gamed by doing less — shallower trees, fewer
// verifications and more cache hits all improve it while degrading precisely
// what this system exists to protect.
//
// MUST be safe for concurrent use: sibling nodes complete on separate goroutines
// and emit as they finish, so Node is called from many goroutines at once.
type TelemetrySink interface {
	Node(o NodeOutcome)
	Run(recordID string, metrics map[string]any)
}

// PriorStore holds learned priors, versioned.
//
// Priors are advisory and visible at the plan gate, never silently binding. If
// the planner's behaviour depends on a prior and the prior drifts, replay breaks
// silently — so every prior consulted is pinned into the record (§8.2, P8).
type PriorStore interface {
	Fetch(name string) (PriorRef, map[string]any, bool)
}

// Admitter is the admission-control seam (§3). Every model call passes it: does
// this node's balance cover this call? In the standalone build the node's own
// *Ledger IS the Admitter — its balance is the cap, enforced in-process. In the
// integrated deployment this is the agate chokepoint, which already does exact
// pre-call caps and server-enforced metering. The two are swappable precisely
// because both satisfy this interface — "no new mechanism" (§3).
//
// The decorator order is load-bearing (§3): Budget(Retry(agent)), never
// Retry(Budget(agent)). Retries pass admission and so consume budget; an
// admitter that let retries through free would make the cap not a cap.
//
// TODO(§10): the executor still calls *Ledger.Admit/Debit directly rather than
// through this interface. Indirecting it waits on one unresolved point in the
// agate contract (docs/integration-requirements.md §1): whether the chokepoint is
// synchronous-per-call or hands out a signed budget lease. Those compose with the
// local apportionment ledger differently, and wiring the indirection now would
// bake in a guess. The interface is named so the contract is fixed; the wiring
// lands with agate's answer.
type Admitter interface {
	// Admit is the pre-call check: refuse with an error wrapping ErrCapExceeded if
	// the cost does not fit, so callers can errors.Is it and distinguish a cap
	// refusal (planned degradation) from a transport fault (fails the run).
	Admit(ctx context.Context, cost Units) error

	// Debit records the actual post-call spend against the same balance.
	Debit(ctx context.Context, cost Units) error
}

// Adversary is the high rung of the §5 verification ladder: it tries to REFUTE a
// claim rather than confirm it. Two properties make it different from a Verifier:
//
//   - It is ASYMMETRIC. One hit is enough to matter — an adversarial pass that
//     finds a single defect has earned its cost, so it is scored on defects
//     found, not on a pass rate.
//   - Its independence is LOAD-BEARING. A Verifier can share the solver's model;
//     an Adversary must not. Same-family judging correlates errors, so the
//     Adversary must be routed through a DIFFERENT provider family than produced
//     the claim (§5). The core cannot enforce that — it is the wiring's job (see
//     provider/) — but the seam names it so the requirement is not lost.
//
// Cost-metered like a Provider: Estimate sizes admission, Attack reports actuals.
// This is what lets the surplus policy (§3 Surplus) spend a completed run's
// remainder on adversarial passes over the highest-exposure claims without
// breaching the cap.
type Adversary interface {
	Name() string
	CostRatio() float64
	Estimate(c Claim) Units

	// Attack tries to break a claim, given the sample it came from for context.
	// found=true is the valuable asymmetric signal: a defect was located. detail
	// is a human-facing note for the receipt. cost is the actual spend. ok=false
	// means the claim could not be assessed — distinct from attacked-and-survived,
	// the same unchecked/checked distinction the Verifier draws (§8).
	Attack(ctx context.Context, c Claim, s Sample) (found bool, detail string, cost Units, ok bool)
}

// ClaimExtractor extracts comparable claims from a result.
//
// THE HIGHEST-RISK UNBUILT PIECE (§7, §12). Without claim-level equivalence,
// "these runs agree" is vibes with a percentage attached, and everything in §7
// assumes it works. Spike it in parallel from day one; expect the Claim shape to
// change.
//
// This is the one seam likely to be implemented in Python, behind a service
// boundary — the ML tooling lives there. Keep the interface narrow enough that
// the hop is cheap.
type ClaimExtractor interface {
	Extract(ctx context.Context, s Sample, nodeID string) ([]Claim, error)
	Equivalent(a, b Claim) bool
}
