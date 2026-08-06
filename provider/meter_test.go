package provider

import (
	"context"
	"errors"
	"sync"
	"testing"

	quarry "github.com/scttfrdmn/quarry"
)

// Tests for Meter — the "says how much" half of #15 D4. What is under test is not
// arithmetic but a CLAIM the plan artifact carries: that planning ran under its own
// stated ceiling and cost the stated amount. A meter that under-reports, or one whose
// ceiling silently fails to hold, makes the artifact assert something false about real
// money.

// scriptedProvider charges a fixed amount per call and estimates whatever it is told to,
// so the pre-call and post-call halves of the cap can be driven apart. They coincide in
// FakeProvider (its estimate is exact), which is precisely why it cannot exercise the
// case where an under-predicting estimate is the only thing standing between the caller
// and an overspend.
type scriptedProvider struct {
	cost quarry.Units // charged per Complete
	est  quarry.Units // returned by Estimate

	mu    sync.Mutex
	calls int
}

func (s *scriptedProvider) Complete(_ context.Context, _, model string, _ quarry.Scope) (quarry.Sample, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return quarry.Sample{Content: "x", Cost: s.cost, Model: model, ModelVersion: model + "@scripted"}, nil
}

func (s *scriptedProvider) Estimate(string, string) quarry.Units { return s.est }

func (s *scriptedProvider) seen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

var _ quarry.Provider = (*scriptedProvider)(nil)

// THE PRE-CALL HALF: the call that would exceed the cap does not happen. This is the
// only half that can actually prevent a spend, and the assertion that matters is that
// the inner provider was never reached — a meter that refuses AFTER calling has already
// spent the money it claims to have withheld.
func TestMeterRefusesTheCallThatWouldExceedTheCapWithoutMakingIt(t *testing.T) {
	inner := &scriptedProvider{cost: 400, est: 400}
	m := NewMeter(inner, 1000)

	for i := range 2 {
		if _, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{}); err != nil {
			t.Fatalf("call %d must be admitted (800 of 1000): %v", i, err)
		}
	}
	// 800 spent, 400 estimated → 1200 > 1000.
	_, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{})
	if !errors.Is(err, quarry.ErrCapExceeded) {
		t.Fatalf("the third call must be refused, wrapping ErrCapExceeded, got %v", err)
	}
	if got := inner.seen(); got != 2 {
		t.Errorf("a refused call must NOT reach the provider: %d inner calls, want 2 — "+
			"refusing after spending is not a ceiling", got)
	}
	spent, calls := m.Spent()
	if spent != 800 || calls != 2 {
		t.Errorf("the refusal must not be counted as spend: got %s across %d calls, want 800 across 2",
			spent, calls)
	}
}

// THE POST-CALL HALF: an estimate that under-predicts must not let the cap be exceeded
// silently. The money is already gone, so this reports rather than prevents — but the
// caller must LEARN it, because the artifact would otherwise state a ceiling that did
// not hold.
func TestMeterReportsABreachAnUnderPredictingEstimateLetThrough(t *testing.T) {
	// Estimate says 1; the call actually costs 5000, well over the cap. Only the
	// post-check can catch this, which is why both halves exist.
	inner := &scriptedProvider{cost: 5000, est: 1}
	m := NewMeter(inner, 1000)

	s, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{})
	if !errors.Is(err, quarry.ErrCapExceeded) {
		t.Fatalf("a breach must be REPORTED even though it cannot be prevented, got %v", err)
	}
	// The sample is returned alongside the error: the call completed, and discarding its
	// content would mean paying for an answer and throwing it away.
	if s.Cost != 5000 {
		t.Errorf("the completed sample must still be returned with the breach, got cost %s", s.Cost)
	}
	// AND THE SPEND MUST BE RECORDED. A meter that refused to count the call that broke
	// its cap would report a cost below the cap while the bill was above it — the exact
	// false claim D4 is about.
	spent, calls := m.Spent()
	if spent != 5000 || calls != 1 {
		t.Errorf("a breaching call must still be counted: got %s across %d calls, want 5000 across 1",
			spent, calls)
	}
}

// Unlimited is not a cap of zero. Absence is not zero, restated at a new site: a planning
// phase deliberately run without a ceiling must not be refused on its first call.
func TestMeterWithAnUnlimitedCapRefusesNothing(t *testing.T) {
	inner := &scriptedProvider{cost: 1_000_000, est: 1_000_000}
	m := NewMeter(inner, quarry.Unlimited)
	for range 3 {
		if _, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{}); err != nil {
			t.Fatalf("Unlimited means no ceiling, not a ceiling of zero: %v", err)
		}
	}
	if spent, calls := m.Spent(); spent != 3_000_000 || calls != 3 {
		t.Errorf("an uncapped meter must still MEASURE: got %s across %d calls", spent, calls)
	}
}

// A provider with no useful estimate degrades to the post-check alone rather than having
// every call refused. `est > 0` in the pre-check is what makes that true.
//
// THE STATE THIS NEEDS IS NOT THE OBVIOUS ONE. At a fresh meter the guard is unreachable
// — `0 + 0 > cap` is false with or without it — so a test that made one estimate-less call
// from zero would pass against a meter that had no guard at all. The guard only bites once
// the balance is ALREADY over the cap, and the only thing that puts it there is a breach
// the post-check reported. So this drives the meter through a breach first, and what it
// then asserts is that an estimate-less provider is not permanently bricked by it.
func TestMeterAdmitsWhenTheProviderOffersNoEstimate(t *testing.T) {
	inner := &scriptedProvider{cost: 1500, est: 0}
	m := NewMeter(inner, 1000)

	// The breach: reported, and it leaves spent (1500) above the cap (1000).
	if _, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{}); !errors.Is(err, quarry.ErrCapExceeded) {
		t.Fatalf("setup: the first call must breach, or the guard under test is unreachable: %v", err)
	}
	if spent, _ := m.Spent(); spent <= m.Cap {
		t.Fatalf("setup: spent (%s) must exceed the cap (%s) for this to test anything", spent, m.Cap)
	}

	// Now the guard: est == 0, spent > cap. Without `est > 0` this is refused.
	if _, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{}); !errors.Is(err, quarry.ErrCapExceeded) {
		// It breaches again on the POST-check, which is correct and expected — the
		// distinction being asserted is that the call HAPPENED.
		t.Logf("post-check breach, as expected: %v", err)
	}
	if got := inner.seen(); got != 2 {
		t.Errorf("an absent estimate must not be read as an infinite one: %d inner calls, "+
			"want 2 — a zero estimate that pre-refuses makes an estimate-less provider "+
			"unusable rather than merely unmeasured (absence is not zero)", got)
	}
}

// ZERO SPEND ACROSS ONE CALL AND ZERO SPEND ACROSS ZERO CALLS ARE DIFFERENT FACTS, which
// is why Spent returns both. The fake reports the first legitimately; an artifact showing
// only the total could not tell a reader which they got, and "0.0000" would read as "no
// model was consulted" when a model had been.
func TestMeterDistinguishesZeroSpendFromNoCalls(t *testing.T) {
	free := &scriptedProvider{cost: 0, est: 0}
	m := NewMeter(free, 1000)
	if spent, calls := m.Spent(); spent != 0 || calls != 0 {
		t.Fatalf("a fresh meter has made no calls: got %s across %d", spent, calls)
	}
	if _, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{}); err != nil {
		t.Fatal(err)
	}
	spent, calls := m.Spent()
	if spent != 0 {
		t.Errorf("a free call costs nothing: got %s", spent)
	}
	if calls != 1 {
		t.Errorf("but it IS a call: got %d, and reporting 0 calls would say no model was "+
			"consulted when one was", calls)
	}
}

// Estimate passes straight through: the meter prices nothing of its own, so admission
// upstream sees the provider's own number rather than a second, divergent model of cost.
func TestMeterPassesTheEstimateThrough(t *testing.T) {
	inner := &scriptedProvider{cost: 1, est: 4242}
	m := NewMeter(inner, quarry.Unlimited)
	if got := m.Estimate("p", "model-1"); got != 4242 {
		t.Errorf("the meter must not invent an estimate: got %s, want 4242", got)
	}
}

// The Planner seam is used concurrently (SamplePlans runs k plans), so the counters must
// be race-free. Under -race this fails on the guarantee rather than on a symptom; without
// it the totals themselves come out wrong.
func TestMeterCountsConcurrentCallsExactly(t *testing.T) {
	const n = 32
	inner := &scriptedProvider{cost: 3, est: 3}
	m := NewMeter(inner, quarry.Unlimited)

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Complete(context.Background(), "p", "model-1", quarry.Scope{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	spent, calls := m.Spent()
	if calls != n || spent != quarry.Units(3*n) {
		t.Errorf("concurrent metering lost counts: got %s across %d calls, want %s across %d",
			spent, calls, quarry.Units(3*n), n)
	}
}

// AND THE REAL WIRING: a Meter around FakeProvider is what `quarry plan --fake` actually
// builds, and a meter the planner cannot be given is a meter the CLI cannot use. This is
// the "a wired seam the CLI does not wire" check for this type — BedrockPlanner.Provider
// was widened from *BedrockProvider to quarry.Provider precisely so this compiles.
func TestAMeterCanStandWhereABedrockPlannerExpectsAProvider(t *testing.T) {
	m := NewMeter(&FakeProvider{}, 10_000)
	p := &BedrockPlanner{Provider: m, Model: "model-1"}
	if p.Provider != quarry.Provider(m) {
		t.Fatal("the planner must hold the meter itself, or the plan's cost is measured " +
			"somewhere the artifact never reads")
	}
}
