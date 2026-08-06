package provider

import (
	"context"
	"fmt"
	"sync"

	quarry "github.com/scttfrdmn/quarry"
)

// Meter is a Provider that counts what passes through it and refuses to exceed a
// ceiling — the "says how much" half of #15 D4.
//
// WHY THIS EXISTS RATHER THAN A LEDGER DEBIT. The planning phase of `quarry plan`
// happens BEFORE any run ledger is meaningful: there is no tree, no apportionment, and
// the cap the plan is being made against is the RUN's cap, which planning must not
// consume. Debiting planning from the run's ledger would make the approved plan's own
// budget smaller than the budget it was planned under — quietly violating D1 with the
// mechanism meant to satisfy D4.
//
// So planning gets its OWN small cap, and this is the thing that enforces it. §4 sizes
// the probe at "one call, ~1/N of the run", which is a real number and not a rounding
// error: at k=5 SamplePlans spends five of them.
//
// A SEPARATE, KNOWN GAP IT DOES NOT CLOSE — issue #26. Inside a run, Executor.node
// calls the planner and NOTHING DEBITS THAT CALL — the only debits are the leaf and the
// reduce. Every recorded run therefore under-reports its true cost by one planner call
// per internal node. That is a defect in the cost receipt (§8) and it is not this type's
// to fix: closing it means the executor debiting a cost the Planner seam does not
// return, which is a change to the seam. Filed rather than patched here, because a
// meter wrapped around the run's planner would produce a number the RECORD still does
// not carry, and a cost known only to the CLI is not a receipt.
//
// Safe for concurrent use, which the Planner seam requires of anything beneath it.
type Meter struct {
	// Inner is the metered provider.
	Inner quarry.Provider

	// Cap is the ceiling on total metered spend. Refuses the call that would exceed
	// it, wrapping quarry.ErrCapExceeded. quarry.Unlimited (-1) means no ceiling.
	//
	// CHECKED BEFORE THE CALL against the ESTIMATE and again after against the actual,
	// which is the same two-sided discipline Ledger.Admit/Debit uses: an estimate that
	// under-predicts must not be able to spend past the cap unnoticed. The post-check
	// cannot un-spend the call — the money is gone — so it reports rather than
	// prevents, and the caller learns the cap was breached instead of believing it held.
	Cap quarry.Units

	mu    sync.Mutex
	spent quarry.Units
	calls int
}

// NewMeter wraps a provider with a spend ceiling.
func NewMeter(inner quarry.Provider, ceiling quarry.Units) *Meter {
	return &Meter{Inner: inner, Cap: ceiling}
}

// Complete meters one call.
func (m *Meter) Complete(ctx context.Context, prompt, model string, scope quarry.Scope) (quarry.Sample, error) {
	// Pre-call admission on the ESTIMATE (advisory, P4) — it can only refuse, never
	// authorise, so a provider with no estimate degrades to the post-check alone.
	est := m.Inner.Estimate(prompt, model)
	m.mu.Lock()
	spent := m.spent
	m.mu.Unlock()
	if m.Cap.Limited() && est > 0 && spent+est > m.Cap {
		return quarry.Sample{}, fmt.Errorf("%w: planning has spent %s of %s and the next call is "+
			"estimated at %s\n  planning has its own cap so it cannot eat the budget the plan is "+
			"being made against (#15 D1/D4)", quarry.ErrCapExceeded, spent, m.Cap, est)
	}

	s, err := m.Inner.Complete(ctx, prompt, model, scope)
	if err != nil {
		return s, err
	}

	m.mu.Lock()
	m.spent += s.Cost
	m.calls++
	total, calls := m.spent, m.calls
	m.mu.Unlock()

	if m.Cap.Limited() && total > m.Cap {
		// The call already happened; this reports the breach rather than hiding it. A
		// metered number that silently exceeded its own stated ceiling is worse than a
		// missing one, because the artifact would carry it as if the ceiling had held.
		return s, fmt.Errorf("%w: planning cost %s across %d call(s), over its cap of %s\n"+
			"  the call completed and the money is spent; this reports it rather than "+
			"recording a ceiling that did not hold", quarry.ErrCapExceeded, total, calls, m.Cap)
	}
	return s, nil
}

// Estimate passes through — the meter prices nothing of its own.
func (m *Meter) Estimate(prompt, model string) quarry.Units { return m.Inner.Estimate(prompt, model) }

// Spent is what actually passed through, and Calls how many did.
//
// MEASURED, NOT ESTIMATED, which is the whole point: D4 says "'near-zero spend' must be
// a stated number with its own cap, not a hope". Calls is returned alongside because
// zero spend across one call and zero spend across zero calls are different facts — a
// fake provider legitimately reports the first, and an artifact that showed only the
// total could not tell a caller which they got.
func (m *Meter) Spent() (quarry.Units, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent, m.calls
}

var _ quarry.Provider = (*Meter)(nil)
