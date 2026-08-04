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

// ProviderSolver solves a leaf by issuing one Provider.Complete call with the BARE
// STATEMENT — no preamble, no length guidance.
//
// That makes it the REPLAY solver rather than the run solver. A live run wants P9 to
// hold at the spend site, which needs the allocation in the prompt and a ceiling on
// the call; both need a price sheet, so both live in provider/ (BudgetedSolver). What
// stays here is the solver whose prompt is exactly what the record contains — see
// Solve.
type ProviderSolver struct {
	Provider Provider
	Model    string
}

// Solve issues one Complete call on the statement alone.
//
// IT DISCARDS alloc, and that is now a property rather than the defect it once was.
// RecordedProvider indexes recorded samples by the recorded PROBLEM and looks them up
// by the PROMPT it receives (record.go), so those two must agree for a replay to hit —
// and they agree precisely when the solver sends the bare statement. A solver that
// wrapped the statement would miss every leaf and report "replay diverged" against a
// faithful record.
//
// So the budget reaches the model one layer up, in the Solver that a RUN wires
// (provider.BudgetedSolver), and the prompt it builds never enters the record. See §2,
// "How a leaf is told about its budget", for why that layering is the design and not a
// convenience.
func (ps ProviderSolver) Solve(ctx context.Context, p Problem, _ Allocation) (Sample, error) {
	if ps.Provider == nil {
		return Sample{}, fmt.Errorf("solver has no provider")
	}
	return ps.Provider.Complete(ctx, p.Statement, ps.Model, p.Scope)
}

var _ Solver = ProviderSolver{}
