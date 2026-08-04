package quarry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// THE PROPAGATION DUALITY (§3.1). Getting this backwards is an easy and
// expensive bug, so the API is shaped to make it hard:
//
//	            | across breadth (siblings) | along depth (parent -> child)
//	------------|---------------------------|------------------------------
//	money       | DIVIDES (each gets share) | inherited whole
//	time        | inherited whole (shared)  | DIVIDES (parent reserves reduce)
//
// Money is an explicit *Ledger parameter and is divided by Apportion.
// Time is carried by context.Context and is narrowed by ChildContext.
// The Go idiom expresses the duality directly: a context deadline is inherited
// by every goroutine that shares it and shortened as you descend, which is
// exactly what time does here. Cancellation comes along for free — when a
// reducer prunes a branch, cancelling the branch context kills the subtree
// (§10).

// Reserve fractions, in basis points of 10,000. Both are guesses flagged for
// measurement in docs/design.md §12 — do not treat them as tuned.
const (
	// DefaultReserveBP is withheld from children to cover the reducer's own
	// cost, retry headroom, and adversarial surplus (§3).
	DefaultReserveBP int64 = 3500

	// DefaultTimeReserveBP is withheld from the window so the reducer has time
	// to run. A node that hands children its full remaining window cannot merge
	// their results — the same failure as spending the whole balance on
	// children, arriving by another route.
	DefaultTimeReserveBP int64 = 2500
)

var (
	// ErrCapExceeded is returned when admission control refuses a call (§3).
	ErrCapExceeded = errors.New("cap exceeded")

	// ErrPlanDoesNotFit is returned when a plan fails mechanical validation
	// against its balance (§2, P9).
	ErrPlanDoesNotFit = errors.New("plan does not fit")

	// ErrScopeWidens is returned when a child would gain authority its parent
	// does not hold (P6).
	ErrScopeWidens = errors.New("child scope must narrow or match parent (P6)")
)

// Ledger is a node's balance. It rides with the request like trace context —
// baggage, never a global (§3).
//
// Carries Scope (P6) so authority and budget travel together and cannot drift
// apart. Not safe for concurrent use; each node holds its own.
type Ledger struct {
	Spend         Units // allocated; Unlimited if uncapped
	Scope         Scope
	Depth         int
	Lineage       []string
	ReserveBP     int64
	TimeReserveBP int64

	spent    Units
	refunded Units
}

// NewLedger builds a root ledger from a run's caps.
func NewLedger(caps Caps, scope Scope) (*Ledger, error) {
	if err := caps.Validate(); err != nil {
		return nil, err
	}
	return &Ledger{
		Spend:         caps.Spend,
		Scope:         scope,
		ReserveBP:     DefaultReserveBP,
		TimeReserveBP: DefaultTimeReserveBP,
	}, nil
}

// RootContext derives the run context from its caps. The earlier of latency and
// due wins.
func RootContext(parent context.Context, caps Caps, now time.Time) (context.Context, context.CancelFunc) {
	var deadline time.Time
	if caps.Latency > 0 {
		deadline = now.Add(caps.Latency)
	}
	if !caps.Due.IsZero() && (deadline.IsZero() || caps.Due.Before(deadline)) {
		deadline = caps.Due
	}
	if deadline.IsZero() {
		return context.WithCancel(parent)
	}
	return context.WithDeadline(parent, deadline)
}

// Balance is what remains: allocation less spend plus refunds from children.
func (l *Ledger) Balance() Units {
	if !l.Spend.Limited() {
		return Unlimited
	}
	return l.Spend - l.spent + l.refunded
}

// Apportionable is what may be given to children — balance less the reserve (§3).
func (l *Ledger) Apportionable() Units {
	b := l.Balance()
	if !b.Limited() {
		return Unlimited
	}
	return b * Units(10000-l.ReserveBP) / 10000
}

// Admit is the pre-call check.
//
// In the integrated deployment this is the agate chokepoint, which already does
// exact pre-call caps and server-enforced metering; the standalone build
// implements the same contract in-process so the two are swappable (§3).
func (l *Ledger) Admit(ctx context.Context, cost Units) error {
	if cost < 0 {
		return fmt.Errorf("cost must be non-negative")
	}
	if b := l.Balance(); b.Limited() && cost > b {
		return fmt.Errorf("%w: call costs %s, balance is %s at depth %d",
			ErrCapExceeded, cost, b, l.Depth)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v at depth %d", ErrCapExceeded, err, l.Depth)
	}
	return nil
}

// Debit admits then records the spend.
func (l *Ledger) Debit(ctx context.Context, cost Units) error {
	if err := l.Admit(ctx, cost); err != nil {
		return err
	}
	l.spent += cost
	return nil
}

// Reserve moves money into a child's allocation (§3).
//
// This is an accounting move, not a model call, so — unlike Admit/Debit — it
// does NOT consult the deadline: reserving budget for a child that will itself
// check the clock when it solves must not fail merely because time is short.
// The clock gates calls, not apportionment (§3.1). Balance is still enforced,
// though Apportion has already guaranteed the shares fit.
func (l *Ledger) Reserve(amount Units) error {
	if amount < 0 {
		return fmt.Errorf("reserve must be non-negative")
	}
	if b := l.Balance(); b.Limited() && amount > b {
		return fmt.Errorf("%w: reserve %s exceeds balance %s at depth %d",
			ErrCapExceeded, amount, b, l.Depth)
	}
	l.spent += amount
	return nil
}

// Refund returns a child's unspent balance to its parent (§3).
func (l *Ledger) Refund(amount Units) error {
	if amount < 0 {
		return fmt.Errorf("refund must be non-negative")
	}
	l.refunded += amount
	return nil
}

// Floor is the minimum viable allocation: one solve plus one verification.
//
// A child below this cannot act on its own verifier, which makes P2
// unenforceable at that node.
func Floor(solve, verify Units) Units { return solve + verify }

// Apportion converts the planner's RELATIVE weights into absolute allocations.
//
// Money only — time is handled by ChildContext, per the duality above.
//
// Uses largest-remainder distribution so the shares sum exactly to the pool with
// no drift, and so the same plan apportions identically on every replay (P8).
//
// Returns ErrPlanDoesNotFit if any child would fall below floor. That is the
// mechanical, model-free verifier for the planner (§2): per P3 the planner is
// the node most worth checking, and budget-conditioning makes part of it
// checkable for free.
//
// Allocations are returned INDEXED BY PLAN POSITION, not keyed by Problem.Key().
// The map form was a latent defect: a key is not unique across a plan's items. A
// partition plan whose planner proposed the same sub-problem twice collapsed to one
// entry, and a PORTFOLIO plan (§2) — where every arm is deliberately the SAME
// problem — collapsed to a single allocation, silently under-funding every arm but
// one. Position is unique by construction; a problem key never was.
func (l *Ledger) Apportion(plan Plan, floor Units) ([]Allocation, error) {
	if plan.Declined || len(plan.Items) == 0 {
		return nil, nil
	}
	totalW := plan.TotalWeight()
	if totalW <= 0 {
		return nil, fmt.Errorf("%w: weights must be positive", ErrPlanDoesNotFit)
	}

	out := make([]Allocation, len(plan.Items))
	pool := l.Apportionable()

	if !pool.Limited() {
		for i := range plan.Items {
			out[i] = Allocation{Spend: Unlimited}
		}
		return out, nil
	}

	type share struct {
		base Units
		rem  int64
		idx  int
	}
	shares := make([]share, len(plan.Items))
	var assigned Units
	for i, it := range plan.Items {
		num := int64(pool) * it.Weight
		base := Units(num / totalW)
		shares[i] = share{base: base, rem: num % totalW, idx: i}
		assigned += base
	}

	// Distribute the remainder to the largest fractional parts, ties broken by
	// position so the result is deterministic.
	leftover := int64(pool - assigned)
	order := make([]int, len(shares))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := shares[order[a]], shares[order[b]]
		if x.rem != y.rem {
			return x.rem > y.rem
		}
		return x.idx < y.idx
	})
	for i := int64(0); i < leftover; i++ {
		shares[order[i%int64(len(order))]].base++
	}

	for _, s := range shares {
		if s.base < floor {
			return nil, fmt.Errorf(
				"%w: child %d (%s) allocated %s, below floor %s — split coarser or raise the cap",
				ErrPlanDoesNotFit, s.idx, truncKey(plan.Items[s.idx].Problem.Key()), s.base, floor)
		}
		out[s.idx] = Allocation{Spend: s.base}
	}
	return out, nil
}

// truncKey shortens a problem key for error messages. Guards the length rather
// than slicing blindly: a Problem with an empty statement still has a full-length
// hash today, but a future key format must not turn a budget error into a panic.
func truncKey(k string) string {
	if len(k) > 12 {
		return k[:12]
	}
	return k
}

// ChildContext derives the window children share.
//
// Every sibling receives the SAME deadline — they run in parallel, so time is
// inherited whole across breadth. The window is shortened by the time reserve,
// which is the division along depth (§3.1).
//
// The returned CancelFunc kills the whole subtree; call it when the reducer
// prunes a branch (§10).
func (l *Ledger) ChildContext(parent context.Context, now time.Time) (context.Context, context.CancelFunc, error) {
	deadline, ok := parent.Deadline()
	if !ok {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, nil
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return nil, nil, fmt.Errorf("%w: no time remains to apportion", ErrPlanDoesNotFit)
	}
	shortened := time.Duration(int64(remaining) * (10000 - l.TimeReserveBP) / 10000)
	ctx, cancel := context.WithDeadline(parent, now.Add(shortened))
	return ctx, cancel, nil
}

// Child derives a child ledger. Scope narrows or is inherited, never widens (P6).
//
// Pass a zero Scope to inherit. Money is inherited whole: the child's balance is
// its allocation, undiminished by depth.
func (l *Ledger) Child(alloc Allocation, nodeID string, scope Scope) (*Ledger, error) {
	if scope.Tags == nil {
		scope = l.Scope
	}
	if !l.Scope.NarrowsTo(scope) {
		return nil, ErrScopeWidens
	}
	lineage := make([]string, len(l.Lineage), len(l.Lineage)+1)
	copy(lineage, l.Lineage)
	return &Ledger{
		Spend:         alloc.Spend,
		Scope:         scope,
		Depth:         l.Depth + 1,
		Lineage:       append(lineage, nodeID),
		ReserveBP:     l.ReserveBP,
		TimeReserveBP: l.TimeReserveBP,
	}, nil
}

// ValidatePlan performs mechanical plan validation — no model call (§2, P9).
func ValidatePlan(l *Ledger, plan Plan, floor Units) error {
	if plan.Declined {
		return nil
	}
	if len(plan.Items) == 0 {
		return fmt.Errorf("%w: a non-declined plan must have children", ErrPlanDoesNotFit)
	}
	_, err := l.Apportion(plan, floor)
	return err
}

// BoundBy reports which cap is actually binding.
//
// Recorded per run so the population tells you whether money or time is the real
// constraint — which nothing else in the system can reveal (§8.2).
func BoundBy(ctx context.Context, l *Ledger) Denomination {
	if b := l.Balance(); b.Limited() && b <= 0 {
		return DenomSpend
	}
	if ctx.Err() != nil {
		return DenomLatency
	}
	return ""
}
