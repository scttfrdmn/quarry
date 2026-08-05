package quarry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests ARE the specification for build step 2 (§2, §3.1). A failing test
// means the design changed — amend docs/design.md in the same commit or revert.

// fakeProvider is the model double: no network, deterministic, counts calls. The
// real Provider lands with provider/ in a later step; nothing here needs it.
type fakeProvider struct {
	cost  Units
	calls int64
	block chan struct{} // if non-nil, Complete waits on it (to force a deadline)
	fail  error
	// emptyContent answers with NO content while still reporting a model and a cost — the
	// model was asked and had nothing to say. That is a RESULT (§8), not an unfunded node,
	// and it is the only state that distinguishes the unfunded predicate from a naive
	// content-emptiness check. Its absence made one such test vacuous (see
	// TestWireUnfundedAgreesWithTheRecordOnEveryNode).
	emptyContent bool
}

func (f *fakeProvider) Complete(ctx context.Context, prompt, model string, scope Scope) (Sample, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.fail != nil {
		return Sample{}, f.fail
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Sample{}, ctx.Err()
		}
	}
	content := "ans:" + prompt
	if f.emptyContent {
		content = ""
	}
	return Sample{
		Content:      content,
		Cost:         f.cost,
		Model:        model,
		ModelVersion: model + "-v1",
		// Non-zero and unequal so the token split is exercised by the whole suite,
		// and so a ratio of exactly 1.0 can never hide a halo/generated swap.
		HaloTokens:      40,
		GeneratedTokens: 10,
	}, nil
}

func (f *fakeProvider) Estimate(prompt, model string) Units { return f.cost }

func problem(stmt string) Problem { return Problem{Statement: stmt} }

func fanoutPlan(stmts ...string) Plan {
	items := make([]PlanItem, len(stmts))
	for i, s := range stmts {
		items[i] = PlanItem{Problem: problem(s), Weight: 1, ExpectLeaf: true}
	}
	return Plan{Items: items}
}

// exec builds a depth-1 executor: MaxDepth 1 means the root plans once and its
// children solve as leaves — the fixed-depth-1 shape the step-2 invariants
// specify, now that recursion is on by default.
func exec(t *testing.T, p Planner, prov Provider) *Executor {
	t.Helper()
	return &Executor{
		Planner:  p,
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Floor:    0,
		Now:      now,
		MaxDepth: 1,
	}
}

// ------------------------------------------------------- the depth-1 fanout

func TestDepth1FansOutToEveryChild(t *testing.T) {
	// Sequential(Planner → Parallel(children) → Reducer) at fixed depth 1 (§2).
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b", "c")}, prov)
	l := ledger(t, FromFloat(100))

	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 3 {
		t.Errorf("want one call per child, got %d", prov.calls)
	}
	if got := res.Answer.Content; got != "ans:a|ans:b|ans:c" {
		t.Errorf("reducer must fold children in plan order, got %q", got)
	}
	// Root outcome plus one per child, root first.
	if len(res.Outcomes) != 4 || res.Outcomes[0].NodeID != "n0" {
		t.Fatalf("want root-first + 3 children, got %d outcomes", len(res.Outcomes))
	}
	for _, o := range res.Outcomes[1:] {
		if o.Depth != 1 {
			t.Errorf("children live at depth 1, got %d", o.Depth)
		}
	}
}

// ------------------------------------------------------- P1: planner declines

func TestDeclinedPlanSolvesRootAsOneLeaf(t *testing.T) {
	// Declining is legitimate (P1): solve the whole problem, do not error.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, DeclinePlanner{Reason: "not worth splitting"}, prov)
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Errorf("a declined plan is a single leaf solve, got %d calls", prov.calls)
	}
	if res.Answer.Content != "ans:root" {
		t.Errorf("want the leaf answer, got %q", res.Answer.Content)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].BaseCase != BasePlannerDeclined {
		t.Errorf("want one outcome with BasePlannerDeclined")
	}
}

// -------------------------------------------------- P9: mechanical plan gate

func TestPlanThatDoesNotFitFailsBeforeAnySpend(t *testing.T) {
	// The mechanical planner verifier (§2/P9): a plan whose children fall below
	// floor is rejected with no model call at all.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b", "c", "d", "e")}, prov)
	e.Floor = Floor(FromFloat(50), 0) // 100/5 = 20 < 50
	_, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if !errors.Is(err, ErrPlanDoesNotFit) {
		t.Fatalf("want ErrPlanDoesNotFit, got %v", err)
	}
	if prov.calls != 0 {
		t.Errorf("no spend before the gate, got %d calls", prov.calls)
	}
}

// --------------------------------------------- §3.1: only time is a gap

func TestSpendExhaustionIsNotAGap(t *testing.T) {
	// A child that cannot be afforded is PLANNED degradation, disclosed at the
	// gate under P9 — recorded with empty content but NOT flagged Gap. Only time
	// is a gap (standing ruling).
	prov := &fakeProvider{cost: FromFloat(40)}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b", "c")}, prov)
	e.Estimate = func(Problem) Units { return FromFloat(40) }
	// Reserve default 3500bp: 65 apportioned across 3 ≈ 21 each — below the 40
	// estimate, so every child misses admission on spend.
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 0 {
		t.Errorf("children priced out must not call the provider, got %d", prov.calls)
	}
	for _, o := range res.Outcomes[1:] {
		if o.Gap {
			t.Errorf("spend-priced-out child must NOT be a gap (only time is): %s", o.NodeID)
		}
		if o.Content != "" {
			t.Errorf("priced-out child has no content, got %q", o.Content)
		}
	}
}

func TestDeadlineExpiryFlagsAGapAndStillReturns(t *testing.T) {
	// A deadline gives no option to return later: whatever exists must be
	// returnable now, with the shortfall named as a gap (§3.1).
	prov := &fakeProvider{cost: FromFloat(1), block: make(chan struct{})} // never unblocks
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)

	ctx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Second))
	defer cancel()
	res, err := e.Run(ctx, problem("root"), ledger(t, FromFloat(100)))
	// An expired deadline before children even start: ChildContext refuses to
	// apportion a window that is already gone.
	if err == nil {
		// If it did proceed, the answer must still be returnable and gapped.
		if !res.Outcomes[0].Gap {
			t.Error("a time-truncated run must be flagged partial at the root")
		}
		return
	}
	if !errors.Is(err, ErrPlanDoesNotFit) {
		t.Fatalf("want no-time-to-apportion (ErrPlanDoesNotFit), got %v", err)
	}
}

func TestChildTruncatedByDeadlineIsAGapNotAFault(t *testing.T) {
	// A child stopped by the context deadline mid-call is truncated, not faulted:
	// the run returns a degraded answer rather than failing (§3.1).
	prov := &fakeProvider{cost: FromFloat(1), block: make(chan struct{})}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)

	// A live window that expires while the blocked provider waits.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := e.Run(ctx, problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatalf("time truncation must not fail the run, got %v", err)
	}
	for _, o := range res.Outcomes[1:] {
		if !o.Gap {
			t.Errorf("child %s truncated by deadline must be a gap", o.NodeID)
		}
	}
	if !res.Outcomes[0].Gap {
		t.Error("root must be flagged partial when a child gapped")
	}
}

// ---------------------------------------------------- genuine faults propagate

func TestProviderFaultFailsTheRun(t *testing.T) {
	// A non-context error is neither budget nor time: partial tolerance does not
	// apply, and the run fails (§3.1).
	prov := &fakeProvider{cost: FromFloat(1), fail: errors.New("model 500")}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	_, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err == nil || !strings.Contains(err.Error(), "child fault") {
		t.Fatalf("want a propagated child fault, got %v", err)
	}
}

// --------------------------------------------------------------- refunds

func TestUnspentChildBalanceRefundsToParent(t *testing.T) {
	// Money is inherited whole and returned on completion (§3): a cheap child
	// gives its surplus back, so the parent's balance reflects real spend.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	l := ledger(t, FromFloat(100))
	if _, err := e.Run(context.Background(), problem("root"), l); err != nil {
		t.Fatal(err)
	}
	// Two children cost 1 each = 2 spent; the rest returns. Balance is close to
	// the original, not drained by the apportionment.
	if l.Balance() != FromFloat(98) {
		t.Errorf("want 98 after 2 refunded children, got %s", l.Balance())
	}
}

// --------------------------------------------------------------- telemetry

func TestTelemetryEmitsEveryNode(t *testing.T) {
	// Node-level telemetry is on from step 1 — every node reported (§8.2).
	prov := &fakeProvider{cost: FromFloat(1)}
	sink := &countingSink{}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b", "c")}, prov)
	e.Sink = sink
	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100))); err != nil {
		t.Fatal(err)
	}
	if sink.nodes != 4 {
		t.Errorf("want 4 node events (root + 3 children), got %d", sink.nodes)
	}
}

// countingSink is concurrency-safe, as the TelemetrySink contract requires:
// siblings emit from separate goroutines.
type countingSink struct {
	mu    sync.Mutex
	nodes int
}

func (c *countingSink) Node(NodeOutcome) {
	c.mu.Lock()
	c.nodes++
	c.mu.Unlock()
}
func (c *countingSink) Run(string, map[string]any) {}
