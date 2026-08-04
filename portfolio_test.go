package quarry

import (
	"context"
	"testing"
	"time"
)

// These tests ARE the specification for the portfolio strategy (§2, "Alternatives
// considered"). A failing test means the design changed — amend docs/design.md in the
// same commit or revert.
//
// Portfolio and partition make CONTRADICTORY assumptions about what an identical
// child statement means. Under partition, two children with the same statement are
// redundant work and collapsing them is the DAG win §2 wants. Under portfolio, N
// children with the same statement are the entire point: independent attempts to be
// selected among. No inspection of the items can tell the two apart — only declared
// intent can — which is why Plan.Strategy exists and why every test below is about
// machinery that was silently correct for partitions and silently wrong for arms.

// portfolioOf builds an N-arm portfolio: the same problem, N times.
func portfolioOf(stmt string, n int) Plan {
	items := make([]PlanItem, n)
	for i := range items {
		items[i] = PlanItem{Problem: problem(stmt), Weight: 1, ExpectLeaf: true}
	}
	return Plan{Items: items, Strategy: StrategyPortfolio}
}

// --------------------------------------------------------------- no collapsing

func TestPortfolioArmsAreNotDeduped(t *testing.T) {
	// dedupePlan collapses same-key items, which is right for a partition and fatal
	// here: it would turn three attempts into one call and report the run as a
	// portfolio that had happened.
	p := portfolioOf("q", 3)
	if got := dedupePlan(p); len(got.Items) != 3 {
		t.Fatalf("want 3 arms preserved, got %d", len(got.Items))
	}
	// The contrast is the whole point: the same items under partition DO collapse.
	asPartition := Plan{Items: p.Items}
	if got := dedupePlan(asPartition); len(got.Items) != 1 {
		t.Fatalf("identical items under partition must still collapse to 1, got %d", len(got.Items))
	}
}

func TestDedupePreservesStrategy(t *testing.T) {
	// dedupePlan rebuilds the Plan, and a rebuild that dropped Strategy would convert
	// a portfolio into a partition at the exact moment the executor consults it.
	p := Plan{Items: []PlanItem{
		{Problem: problem("a"), Weight: 1},
		{Problem: problem("b"), Weight: 1},
	}, Strategy: StrategyPortfolio}
	if got := dedupePlan(p); got.Strategy != StrategyPortfolio {
		t.Errorf("strategy must survive dedupe, got %q", got.Strategy)
	}
}

// ------------------------------------------------------ positional apportionment

func TestEveryArmIsFundedSeparately(t *testing.T) {
	// Apportion returns allocations INDEXED BY POSITION. Keying them by Problem.Key()
	// collapsed a portfolio to a single allocation — every arm but one silently
	// underfunded, and nothing in the record would say why the run degraded.
	l := ledger(t, FromFloat(100))
	l.ReserveBP = 0
	got, err := l.Apportion(portfolioOf("q", 4), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want one allocation per arm, got %d", len(got))
	}
	for i, a := range got {
		if a.Spend != FromFloat(25) {
			t.Errorf("arm %d: want an equal share, got %s", i, a.Spend)
		}
	}
	if s := sum(got); s != FromFloat(100) {
		t.Errorf("shares must still sum to the pool exactly, got %s", s)
	}
}

func TestPortfolioRunsEveryArm(t *testing.T) {
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: portfolioOf("q", 3)}, prov)
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 3 {
		t.Fatalf("want one call per arm, got %d", prov.calls)
	}
	if len(res.Outcomes) != 4 {
		t.Fatalf("want root + 3 arms in the record, got %d", len(res.Outcomes))
	}
}

// --------------------------------------------------------- P7: arms stay samples

func TestArmsDoNotReadTheCache(t *testing.T) {
	// This is the precise way §6 warns a cache "saves money by destroying
	// replication". Arm 1 writes; if arms 2 and 3 could read, they would be served a
	// COPY of arm 1 and the run would report three independent attempts where one
	// happened. P7: a cache hit is not an independent sample.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	e := exec(t, StaticPlanner{P: portfolioOf("q", 3)}, prov)
	e.Cache = cache

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100))); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 3 {
		t.Fatalf("every arm must draw its own sample: want 3 calls, got %d", prov.calls)
	}
}

func TestArmsStillWriteToTheCache(t *testing.T) {
	// Suppressing the READ is not suppressing the write. Each arm is a genuine draw
	// on the same (problem, scope) key, so the entry should accumulate N real samples
	// — which is exactly the distribution P7 wants and a single-answer cache denies.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	e := exec(t, StaticPlanner{P: portfolioOf("q", 3)}, prov)
	e.Cache = cache

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100))); err != nil {
		t.Fatal(err)
	}
	if n := cache.N(problem("q"), now); n != 3 {
		t.Errorf("want 3 accumulated samples from 3 arms, got %d", n)
	}
}

func TestPartitionChildrenStillReadTheCache(t *testing.T) {
	// The suppression must be scoped to arms alone. If it leaked to every child, the
	// DAG win of §2 would be gone — and that regression would look like a slower,
	// more expensive system with no failing test to name it.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	cache.Append(problem("a"), Sample{Content: "cached-a"}, nil, now)

	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.Cache = cache
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Errorf("the warm child must be served: want 1 call, got %d", prov.calls)
	}
	var hits int
	for _, o := range res.Outcomes {
		if o.CacheHit {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("want exactly 1 cache hit among partition children, got %d", hits)
	}
}

func TestRootIsNeverAnArm(t *testing.T) {
	// Only a CHILD of a portfolio plan is an arm. The root's own cache read must stay
	// live — a whole run repeating an identical root problem is the memoization §6 is
	// for, and the root has no siblings to be a copy of.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	cache.Append(problem("root"), Sample{Content: "cached-root"}, nil, now)

	e := exec(t, StaticPlanner{P: portfolioOf("q", 3)}, prov)
	e.Cache = cache
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 0 {
		t.Errorf("a warm root must be served even under a portfolio plan, got %d calls", prov.calls)
	}
	if res.Answer.Content != "cached-root" {
		t.Errorf("want the cached root answer, got %q", res.Answer.Content)
	}
}

// ------------------------------------------------------------- select, not merge

func TestPortfolioSelectsOneAnswerRatherThanConcatenatingThem(t *testing.T) {
	// ConcatReducer is the DEFAULT reducer, so this is the likeliest way a portfolio
	// would appear to "work" while being useless: joining three attempts at one
	// question returns three answers, and the caller asked for one.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: portfolioOf("q", 3)}, prov)
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer.Content != "ans:q" {
		t.Errorf("want a single selected arm, got %q", res.Answer.Content)
	}
}

func TestSelectReducerPrefersAVerifiedArm(t *testing.T) {
	// The one piece of real judgement available for free, and the right one: a
	// portfolio whose arms are individually verified turns selection into "take one
	// that passed", which §2 names as the case portfolio is strictly better for.
	yes, no := true, false
	arms := []NodeOutcome{
		{NodeID: "n0.0", Content: "unchecked"},
		{NodeID: "n0.1", Content: "failed", Verified: &no},
		{NodeID: "n0.2", Content: "passed", Verified: &yes},
	}
	got, err := SelectReducer{}.Reduce(context.Background(), problem("q"), arms,
		Allocation{}, false, StrategyPortfolio)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "passed" {
		t.Errorf("want the verified arm, got %q", got.Content)
	}
}

func TestSelectReducerFallsBackToAnUnverifiedArm(t *testing.T) {
	// UNVERIFIED is not the same as FAILED (§8). A portfolio with no verifier must
	// still return an answer — it simply cannot claim the answer was checked.
	arms := []NodeOutcome{{NodeID: "n0.0", Content: "first"}, {NodeID: "n0.1", Content: "second"}}
	got, _ := SelectReducer{}.Reduce(context.Background(), problem("q"), arms,
		Allocation{}, false, StrategyPortfolio)
	if got.Content != "first" {
		t.Errorf("want the first arm with content, got %q", got.Content)
	}
	if got.Verified != nil {
		t.Error("selecting an unverified arm must not manufacture a verdict")
	}
}

func TestSelectionDoesNotDoubleCountTheArmsCost(t *testing.T) {
	// Each arm recorded its own spend as its own node. Carrying the winner's cost onto
	// the selection would bill the run twice for one call and inflate the tree total —
	// the same error as rolling a subtree's tokens into its parent.
	arms := []NodeOutcome{{NodeID: "n0.0", Content: "a", Cost: FromFloat(7), HaloTokens: 40, GeneratedTokens: 10}}
	got, _ := SelectReducer{}.Reduce(context.Background(), problem("q"), arms,
		Allocation{}, false, StrategyPortfolio)
	if got.Cost != 0 || got.HaloTokens != 0 || got.GeneratedTokens != 0 {
		t.Errorf("the mechanical selection is free and carries no arm accounting: cost=%s halo=%d gen=%d",
			got.Cost, got.HaloTokens, got.GeneratedTokens)
	}
}

func TestAllArmsGappedReturnsEmptyNotAnError(t *testing.T) {
	// Partial tolerance (§3.1) on the portfolio path: every arm missing is a gap, not
	// a fault. A run that produced nothing must still be returnable and citable.
	arms := []NodeOutcome{{NodeID: "n0.0", Gap: true}, {NodeID: "n0.1", Content: ""}}
	got, err := SelectReducer{}.Reduce(context.Background(), problem("q"), arms,
		Allocation{}, true, StrategyPortfolio)
	if err != nil {
		t.Fatalf("an all-gapped portfolio must not error: %v", err)
	}
	if got.Content != "" {
		t.Errorf("want an empty sample, got %q", got.Content)
	}
}

func TestConcatReducerDelegatesPortfoliosRatherThanJoiningThem(t *testing.T) {
	// Pinned at the unit level too, because the delegation inside ConcatReducer is the
	// safety net for every reducer that predates strategies.
	arms := []NodeOutcome{{NodeID: "n0.0", Content: "A"}, {NodeID: "n0.1", Content: "B"}}
	got, _ := ConcatReducer{Sep: "|"}.Reduce(context.Background(), problem("q"), arms,
		Allocation{}, false, StrategyPortfolio)
	if got.Content != "A" {
		t.Errorf("want a selection, got the concatenation %q", got.Content)
	}
}

// ------------------------------------------------------------ §7: plan pinning

func TestPinnedPortfolioReplaysEveryArm(t *testing.T) {
	// Plan pinning is §7's experimental control: freeze the shape, re-run the leaves,
	// attribute the spread to solving rather than planning. A control that silently
	// changes the shape is worse than no control — and this one did. Before the fix a
	// pinned portfolio came back as a partition, dedupePlan collapsed three identical
	// arms into one child, and the re-run did a THIRD of the work while reporting a
	// faithful shape replay.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: portfolioOf("q", 3)}, prov)
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRunRecord(res, root, caps, ModeFresh)

	prov2 := &fakeProvider{cost: FromFloat(1)}
	pinned := exec(t, PinPlan(rec), prov2)
	if _, err := pinned.Run(context.Background(), root, ledger(t, FromFloat(1000))); err != nil {
		t.Fatal(err)
	}
	if prov2.calls != prov.calls {
		t.Errorf("a pinned portfolio must re-run every arm: original %d calls, pinned %d",
			prov.calls, prov2.calls)
	}
}

func TestStrategyIsRecordedOnTheNodeThatUsedIt(t *testing.T) {
	// Without this field the record cannot distinguish a portfolio from a partition
	// whose children happened to coincide — opposite claims about what the run did.
	// It is hashed like every other field: the strategy is a deterministic property of
	// the plan, so a replay that produced a different one has genuinely diverged.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: portfolioOf("q", 2)}, prov)
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[0].Strategy != StrategyPortfolio {
		t.Errorf("the internal node must record its strategy, got %q", res.Outcomes[0].Strategy)
	}
	for _, o := range res.Outcomes[1:] {
		if o.Strategy != StrategyPartition {
			t.Errorf("arm %s used no plan and must carry the zero strategy, got %q",
				o.NodeID, o.Strategy)
		}
	}
}

func TestPinningSeparatesAnArmFromItsParent(t *testing.T) {
	// A portfolio's arms share their PARENT'S problem key by definition (§2: N attempts
	// at the same problem). Indexing pinned nodes by key alone let the arms — leaves,
	// so childless — overwrite the parent's entry, and the whole portfolio came back as
	// a decline. Depth separates them: parent at d, arms at d+1.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:  StaticPlanner{P: portfolioOf("root", 3)}, // arms restate the ROOT problem
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{},
		Now:      now,
		MaxDepth: 1,
	}
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	pp := PinPlan(NewRunRecord(res, root, caps, ModeFresh))

	plan, _ := pp.Plan(context.Background(), root, Allocation{}, 0, nil)
	if plan.Declined || len(plan.Items) != 3 {
		t.Fatalf("the root's portfolio must survive pinning: declined=%v items=%d",
			plan.Declined, len(plan.Items))
	}
	// And the arm at depth 1 — same problem key — must still pin as a leaf.
	armPlan, _ := pp.Plan(context.Background(), root, Allocation{}, 1, nil)
	if !armPlan.Declined {
		t.Error("an arm recorded as a leaf must decline at its own depth")
	}
}

// -------------------------------------------------------------- P6 still holds

func TestArmsInheritScopeLikeAnyChild(t *testing.T) {
	// Portfolio changes the SHAPE, not the entitlement rules. P6 is unconditional:
	// scope never widens on descent, whatever the strategy.
	l, err := NewLedger(Caps{Spend: FromFloat(100)},
		Scope{Tags: map[string]string{"agate:dept": "bio", "agate:proj": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	_, cerr := l.Child(Allocation{Spend: FromFloat(10)}, "n0.0",
		Scope{Tags: map[string]string{"agate:dept": "bio"}})
	if cerr == nil {
		t.Error("an arm with widened scope must still be refused (P6)")
	}
}
