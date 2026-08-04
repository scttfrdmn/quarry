package quarry

import (
	"context"
	"fmt"
)

// Reference Solver for build step 2.
//
// A Solver answers a leaf problem directly by calling a Provider — the only
// thing in the system that costs money (§2). Admission and metering are the
// executor's job, not the solver's: the solver reports what a call cost, the
// ledger decides whether it could be afforded. Keeping the two apart is what
// makes the decorator ordering Budget(Retry(agent)) enforceable (§3) — the
// budget wraps the call, the call does not police itself.

// ProviderSolver solves a leaf by issuing one Provider.Complete call.
type ProviderSolver struct {
	Provider Provider
	Model    string
}

// Solve issues one Complete call.
//
// IT DISCARDS alloc, and that is a known defect rather than a design: the leaf is the
// only place in the system that spends money, and it is told nothing about its budget,
// so P9 holds for the planner and fails here. Fixing it means adding the first prompt
// in quarry that is not a bare question — see the tracking issue, and §2 on why the
// budget must reach the model in RELATIVE terms if it reaches it at all.
func (ps ProviderSolver) Solve(ctx context.Context, p Problem, alloc Allocation) (Sample, error) {
	if ps.Provider == nil {
		return Sample{}, fmt.Errorf("solver has no provider")
	}
	return ps.Provider.Complete(ctx, p.Statement, ps.Model, p.Scope)
}

var _ Solver = ProviderSolver{}
