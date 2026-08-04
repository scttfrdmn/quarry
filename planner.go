package quarry

import "context"

// Reference Planner implementations for build step 2 (fixed depth 1).
//
// The production Planner is an LLM agent (§2): the error concentrator, doing the
// hardest reasoning with the least information, and therefore the node most
// worth verifying (P3). It is built and checked separately. These deterministic
// doubles exist so the executor, apportionment, and fanout can be exercised
// end to end with no model — which is exactly what keeps steps 1-7 LLM-free.

// StaticPlanner returns a fixed plan regardless of input.
//
// The canonical fanout double: give it a plan with N items and the executor
// drives N parallel children. Deterministic, so it also underwrites the replay
// test in step 5.
type StaticPlanner struct {
	P Plan
}

// Plan returns the fixed plan, IGNORING the allocation and depth. That makes it a
// test double and not a planner: a real planner is budget-conditioned (P9), and this
// one will happily return a plan the ledger cannot admit — which is the point, since
// ErrPlanDoesNotFit needs a way to be provoked.
func (sp StaticPlanner) Plan(ctx context.Context, p Problem, alloc Allocation, depth int, prior []NodeOutcome) (Plan, error) {
	return sp.P, nil
}

// DeclinePlanner always declines to split (P1).
//
// Declining is a legitimate outcome when surface-to-volume does not favour
// decomposition. The executor must solve the whole problem as a single leaf
// rather than treating the empty plan as an error.
type DeclinePlanner struct {
	Reason string
}

// Plan always declines, carrying Reason so the record says WHY it did not split —
// a declined plan with no reasoning is indistinguishable from a planner that failed.
func (d DeclinePlanner) Plan(ctx context.Context, p Problem, alloc Allocation, depth int, prior []NodeOutcome) (Plan, error) {
	return Plan{Declined: true, Reasoning: d.Reason}, nil
}

var (
	_ Planner = StaticPlanner{}
	_ Planner = DeclinePlanner{}
)
