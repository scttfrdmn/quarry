package quarry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These tests ARE the specification for §3/§3.1. If a change makes one fail,
// either the change is wrong or docs/design.md needs amending — say which in the
// PR. Do not adjust a test to make a change pass without that.

var now = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func planOf(weights ...int64) Plan {
	items := make([]PlanItem, len(weights))
	for i, w := range weights {
		items[i] = PlanItem{
			Problem:    Problem{Statement: string(rune('a' + i))},
			Weight:     w,
			ExpectLeaf: true,
		}
	}
	return Plan{Items: items}
}

func ledger(t *testing.T, spend Units) *Ledger {
	t.Helper()
	l, err := NewLedger(Caps{Spend: spend, Latency: time.Hour}, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func sum(allocs []Allocation) Units {
	var t Units
	for _, a := range allocs {
		t += a.Spend
	}
	return t
}

// ---------------------------------------------------------------- the duality

func TestMoneyDividesAcrossSiblings(t *testing.T) {
	// Spend tracks total nodes, so it constrains breadth (§3.1).
	l := ledger(t, FromFloat(100))
	l.ReserveBP = 0
	got, err := l.Apportion(planOf(1, 1, 2), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 children, got %d", len(got))
	}
	if s := sum(got); s != FromFloat(100) {
		t.Errorf("shares must sum to the pool exactly: got %s", s)
	}
}

func TestApportionmentIsExactAndDeterministic(t *testing.T) {
	// Largest-remainder distribution: no drift, and identical on every replay,
	// which float division cannot promise (P8).
	l := ledger(t, 100)
	l.ReserveBP = 0
	first, err := l.Apportion(planOf(1, 1, 1), 0) // 100/3 does not divide
	if err != nil {
		t.Fatal(err)
	}
	if s := sum(first); s != 100 {
		t.Fatalf("lost units to rounding: want 100, got %d", s)
	}
	for i := 0; i < 50; i++ {
		again, _ := l.Apportion(planOf(1, 1, 1), 0)
		for k, v := range first {
			if again[k] != v {
				t.Fatalf("apportionment not deterministic across replays")
			}
		}
	}
}

func TestTimeIsInheritedWholeByEverySibling(t *testing.T) {
	// Siblings run in parallel, so they SHARE the window rather than splitting
	// it. Getting this backwards starves a wide tree of time it actually has.
	l := ledger(t, FromFloat(100))
	l.TimeReserveBP = 0
	parent, cancel := context.WithDeadline(context.Background(), now.Add(time.Hour))
	defer cancel()

	a, cancelA, err := l.ChildContext(parent, now)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelA()
	b, cancelB, _ := l.ChildContext(parent, now)
	defer cancelB()

	da, _ := a.Deadline()
	db, _ := b.Deadline()
	if !da.Equal(db) {
		t.Errorf("siblings must share a window: %v vs %v", da, db)
	}
	if !da.Equal(now.Add(time.Hour)) {
		t.Errorf("window must be inherited whole, got %v", da)
	}
}

func TestTimeDividesAlongDepth(t *testing.T) {
	// A node that hands children its full window cannot merge their results —
	// the same failure as spending the whole balance on children (§3.1).
	l := ledger(t, FromFloat(100))
	l.TimeReserveBP = 2500
	parent, cancel := context.WithDeadline(context.Background(), now.Add(time.Hour))
	defer cancel()

	child, cancelChild, err := l.ChildContext(parent, now)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelChild()

	dc, _ := child.Deadline()
	dp, _ := parent.Deadline()
	if !dc.Equal(now.Add(45 * time.Minute)) {
		t.Errorf("want 45m window, got %v", dc.Sub(now))
	}
	if !dc.Before(dp) {
		t.Error("child window must be strictly shorter than parent's")
	}
}

func TestMoneyIsInheritedWholeAlongDepth(t *testing.T) {
	l := ledger(t, FromFloat(100))
	l.ReserveBP = 0
	allocs, _ := l.Apportion(planOf(1), 0)
	var alloc Allocation
	for _, a := range allocs {
		alloc = a
	}
	child, err := l.Child(alloc, "n0", Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if child.Balance() != FromFloat(100) {
		t.Errorf("child balance is its allocation, undiminished: got %s", child.Balance())
	}
	if child.Depth != 1 {
		t.Errorf("want depth 1, got %d", child.Depth)
	}
}

func TestCancellingAParentKillsTheSubtree(t *testing.T) {
	// When the reducer prunes a branch, the subtree dies (§10). In Go this is
	// the context tree rather than a mechanism to build.
	l := ledger(t, FromFloat(100))
	root, cancelRoot := context.WithCancel(context.Background())
	child, cancelChild, _ := l.ChildContext(root, now)
	defer cancelChild()
	grandchild, cancelGC, _ := l.ChildContext(child, now)
	defer cancelGC()

	cancelRoot()
	if grandchild.Err() == nil {
		t.Error("cancelling the root must kill descendants")
	}
}

// ------------------------------------------------------------------- reserve

func TestReserveIsWithheldFromChildren(t *testing.T) {
	// The reducer must be funded before the split. Allocating 100% to children
	// means paying for every sub-answer with nothing left to combine them (§3).
	l := ledger(t, FromFloat(100))
	l.ReserveBP = 3500
	got, err := l.Apportion(planOf(1, 1), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s := sum(got); s != FromFloat(65) {
		t.Errorf("want 65 apportioned, got %s", s)
	}
	if l.Balance() != FromFloat(100) {
		t.Error("reserve stays with the parent")
	}
}

func TestRefundReturnsUnspentBalance(t *testing.T) {
	ctx := context.Background()
	l := ledger(t, FromFloat(100))
	if err := l.Debit(ctx, FromFloat(30)); err != nil {
		t.Fatal(err)
	}
	if l.Balance() != FromFloat(70) {
		t.Fatalf("want 70, got %s", l.Balance())
	}
	_ = l.Refund(FromFloat(10))
	if l.Balance() != FromFloat(80) {
		t.Errorf("want 80, got %s", l.Balance())
	}
}

// --------------------------------------------------------------------- floor

func TestChildBelowFloorRejectsThePlan(t *testing.T) {
	// One solve plus one verification is the minimum viable allocation: below
	// it a child cannot act on its own verifier, making P2 unenforceable.
	l := ledger(t, FromFloat(10))
	l.ReserveBP = 0
	_, err := l.Apportion(planOf(1, 1, 1, 1, 1), Floor(FromFloat(2), FromFloat(1)))
	if !errors.Is(err, ErrPlanDoesNotFit) {
		t.Fatalf("want ErrPlanDoesNotFit, got %v", err)
	}
}

func TestMechanicalPlanValidationNeedsNoModel(t *testing.T) {
	// Per P3 the planner is the node most worth verifying, and
	// budget-conditioning makes part of it checkable for free (§2).
	l := ledger(t, FromFloat(100))
	if err := ValidatePlan(l, planOf(1, 1), Floor(FromFloat(1), FromFloat(1))); err != nil {
		t.Fatal(err)
	}
}

func TestDeclinedPlanIsValid(t *testing.T) {
	// Declining to split is a legitimate outcome under P1.
	if err := ValidatePlan(ledger(t, FromFloat(10)), Plan{Declined: true}, 0); err != nil {
		t.Fatal(err)
	}
}

func TestNonDeclinedPlanMustHaveChildren(t *testing.T) {
	if err := ValidatePlan(ledger(t, FromFloat(10)), Plan{}, 0); !errors.Is(err, ErrPlanDoesNotFit) {
		t.Fatalf("want ErrPlanDoesNotFit, got %v", err)
	}
}

// ------------------------------------------------------------ admission control

func TestAdmissionRefusesOverBalance(t *testing.T) {
	l := ledger(t, FromFloat(10))
	if err := l.Admit(context.Background(), FromFloat(11)); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("want ErrCapExceeded, got %v", err)
	}
}

func TestAdmissionRefusesAfterDeadline(t *testing.T) {
	l := ledger(t, FromFloat(10))
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Minute))
	defer cancel()
	if err := l.Admit(ctx, FromFloat(1)); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("want ErrCapExceeded on expired context, got %v", err)
	}
}

func TestUnlimitedSpendAdmitsAnything(t *testing.T) {
	// A time-only run has no spend ceiling to check.
	l, err := NewLedger(Caps{Spend: Unlimited, Latency: time.Hour}, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Admit(context.Background(), FromFloat(1e6)); err != nil {
		t.Fatal(err)
	}
}

// --------------------------------------------------------------------- scope

func TestScopeNeverWidensOnDescent(t *testing.T) {
	// A decomposer minting sub-agents with broader scope is a confused deputy,
	// and defeats the boundary agate exists to hold (P6).
	l, _ := NewLedger(
		Caps{Spend: FromFloat(10)},
		Scope{Tags: map[string]string{"agate:dept": "bio", "agate:proj": "x"}},
	)
	_, err := l.Child(Allocation{Spend: FromFloat(5)}, "n0",
		Scope{Tags: map[string]string{"agate:dept": "bio"}})
	if !errors.Is(err, ErrScopeWidens) {
		t.Fatalf("want ErrScopeWidens, got %v", err)
	}
}

func TestScopeMayNarrow(t *testing.T) {
	l, _ := NewLedger(Caps{Spend: FromFloat(10)},
		Scope{Tags: map[string]string{"agate:dept": "bio"}})
	c, err := l.Child(Allocation{Spend: FromFloat(5)}, "n0",
		Scope{Tags: map[string]string{"agate:dept": "bio", "agate:proj": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Scope.Tags["agate:proj"] != "x" {
		t.Error("narrowed tag lost")
	}
}

func TestScopeIsInheritedByDefault(t *testing.T) {
	l, _ := NewLedger(Caps{Spend: FromFloat(10)},
		Scope{Tags: map[string]string{"agate:dept": "bio"}})
	c, _ := l.Child(Allocation{Spend: FromFloat(5)}, "n0", Scope{})
	if c.Scope.Key() != l.Scope.Key() {
		t.Error("scope must be inherited when unspecified")
	}
}

// ---------------------------------------------------------------- which cap bit

func TestBoundByReportsTheBindingConstraint(t *testing.T) {
	// Across the population this says whether money or time is the real
	// constraint — which nothing else in the system can tell you (§8.2).
	ctx := context.Background()
	l := ledger(t, FromFloat(10))
	if d := BoundBy(ctx, l); d != "" {
		t.Errorf("want unbound, got %q", d)
	}
	_ = l.Debit(ctx, FromFloat(10))
	if d := BoundBy(ctx, l); d != DenomSpend {
		t.Errorf("want spend, got %q", d)
	}

	expired, cancel := context.WithDeadline(context.Background(), now.Add(-time.Minute))
	defer cancel()
	l2, _ := NewLedger(Caps{Spend: Unlimited, Latency: time.Hour}, Scope{})
	if d := BoundBy(expired, l2); d != DenomLatency {
		t.Errorf("want latency, got %q", d)
	}
}

// ---------------------------------------------------------------------- caps

func TestCapsRequireAtLeastOneConstraint(t *testing.T) {
	// P9: planning is budget-conditioned, so an uncapped run has nothing to
	// plan against.
	if err := (Caps{Spend: Unlimited}).Validate(); err == nil {
		t.Error("want error for a wholly uncapped run")
	}
}

func TestRootContextTakesTheEarlierOfLatencyAndDue(t *testing.T) {
	ctx, cancel := RootContext(context.Background(),
		Caps{Spend: FromFloat(50), Latency: time.Hour, Due: now.AddDate(0, 0, 6)}, now)
	defer cancel()
	d, ok := ctx.Deadline()
	if !ok || !d.Equal(now.Add(time.Hour)) {
		t.Errorf("want the latency deadline, got %v", d)
	}
}

func TestDueWithoutLatencyIsDeferrable(t *testing.T) {
	// Slack is convertible into money: batch inference, off-peak, deferred
	// execution. Giving up fast mechanically buys cheap (§3.1).
	if !(Caps{Spend: FromFloat(50), Due: now.AddDate(0, 0, 6)}).Deferrable() {
		t.Error("a due date with no latency cap must be deferrable")
	}
	if (Caps{Spend: FromFloat(50), Latency: time.Hour, Due: now.AddDate(0, 0, 6)}).Deferrable() {
		t.Error("a latency cap makes a run non-deferrable")
	}
}
