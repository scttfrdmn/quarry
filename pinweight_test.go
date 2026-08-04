package quarry

import (
	"context"
	"sync"
	"testing"
	"time"
)

// These tests ARE the specification for what plan pinning CONTROLS FOR (§7). A
// failing test means the design changed — amend docs/design.md in the same commit or
// revert.
//
// Pinning is an experimental control: it freezes the plan so that any spread between
// a run and its re-run is attributable to SOLVING rather than to planning. A control
// that varies something it claims to hold fixed is worse than no control, because the
// variance it reports is partly its own. Two fields have now been caught missing this
// way — Strategy, then Weight — and both failed in the direction that still looked
// like a faithful replay, so these tests assert the CONSEQUENCE (how much money each
// child received) rather than the field's presence.

// allocSpy is a Solver that records what each problem was allocated. Apportionment is
// otherwise invisible: the shape of a re-run can be read off the outcomes, but the
// division of money can only be observed at the moment a child is asked to spend it.
type allocSpy struct {
	mu    sync.Mutex
	spend map[string]Units
	cost  Units
}

func newAllocSpy(cost Units) *allocSpy {
	return &allocSpy{spend: map[string]Units{}, cost: cost}
}

func (a *allocSpy) Solve(_ context.Context, p Problem, alloc Allocation) (Sample, error) {
	a.mu.Lock()
	a.spend[p.Statement] = alloc.Spend
	a.mu.Unlock()
	return Sample{
		Content:         "ans:" + p.Statement,
		Cost:            a.cost,
		Model:           "spy",
		ModelVersion:    "spy-v1",
		HaloTokens:      40,
		GeneratedTokens: 10,
	}, nil
}

func (a *allocSpy) got(stmt string) Units {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.spend[stmt]
}

// weightedPlan builds a partition whose children carry deliberately UNEQUAL weights,
// so a uniform re-apportionment is detectable rather than coincidentally identical.
func weightedPlan(pairs ...any) Plan {
	items := make([]PlanItem, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		items = append(items, PlanItem{
			Problem:    problem(pairs[i].(string)),
			Weight:     int64(pairs[i+1].(int)),
			ExpectLeaf: true,
		})
	}
	return Plan{Items: items}
}

var pinCaps = Caps{Spend: FromFloat(1000), Latency: time.Hour}

// pinRun does one run with the given planner and returns its record plus the spy.
func pinRun(t *testing.T, p Planner) (RunRecord, *allocSpy) {
	t.Helper()
	spy := newAllocSpy(FromFloat(1))
	e := &Executor{
		Planner:  p,
		Solver:   spy,
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 1,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, root, pinCaps, ModeFresh), spy
}

// ------------------------------------------------- the control holds apportionment

func TestPinningReproducesTheApportionmentNotJustTheShape(t *testing.T) {
	// THE POINT. A 6/3/1 split re-planned uniformly gives each child a third — the
	// shape is faithful and the money is not, so a spread between the two runs could
	// come from the re-division rather than from the solving pinning exists to isolate.
	orig, spyA := pinRun(t, StaticPlanner{P: weightedPlan("alpha", 6, "beta", 3, "gamma", 1)})

	spyB := newAllocSpy(FromFloat(1))
	pinned := &Executor{
		Planner:  PinPlan(orig),
		Solver:   spyB,
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 1,
	}
	if _, err := pinned.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000))); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{"alpha", "beta", "gamma"} {
		if a, b := spyA.got(stmt), spyB.got(stmt); a != b {
			t.Errorf("%s: pinned re-run allocated %s, original allocated %s", stmt, b, a)
		}
	}
	// Non-vacuity: the weights must really have produced an uneven split, or the test
	// would pass against a planner that ignored them entirely.
	if spyA.got("alpha") <= spyA.got("gamma") {
		t.Fatalf("the weighted plan must fund alpha above gamma, got %s vs %s",
			spyA.got("alpha"), spyA.got("gamma"))
	}
}

func TestWeightIsRecordedOnTheChildItFunded(t *testing.T) {
	// The weight is a fact about the PARENT's plan, but it is observable only through
	// the node it paid for — so it is recorded on the child. Reading it off the parent
	// would need a per-child list, which is the same data in a shape that can fall out
	// of step with Children.
	rec, _ := pinRun(t, StaticPlanner{P: weightedPlan("alpha", 6, "beta", 3, "gamma", 1)})
	want := map[string]int64{"alpha": 6, "beta": 3, "gamma": 1}
	for _, o := range rec.Outcomes {
		if w, ok := want[o.Problem.Statement]; ok && o.PlanWeight != w {
			t.Errorf("%s: recorded weight %d, want %d", o.Problem.Statement, o.PlanWeight, w)
		}
	}
}

func TestTheRootHasNoPlanWeight(t *testing.T) {
	// No plan funded the root, so it has no weight — and zero must read as "unrecorded"
	// rather than as a real weight of zero, which Apportion would reject outright.
	rec, _ := pinRun(t, StaticPlanner{P: weightedPlan("alpha", 6, "beta", 3, "gamma", 1)})
	if rec.Outcomes[0].NodeID != "n0" {
		t.Fatalf("outcomes are pre-order; want the root first, got %q", rec.Outcomes[0].NodeID)
	}
	if rec.Outcomes[0].PlanWeight != 0 {
		t.Errorf("the root must carry no plan weight, got %d", rec.Outcomes[0].PlanWeight)
	}
}

func TestPinnedWeightSurvivesDedupe(t *testing.T) {
	// dedupePlan collapses identical partition children and SUMS their weights (§2), so
	// the weight that funded the surviving child is the sum. Recording the pre-dedupe
	// number would pin a weight no node was ever apportioned by.
	p := Plan{Items: []PlanItem{
		{Problem: problem("dup"), Weight: 3, ExpectLeaf: true},
		{Problem: problem("dup"), Weight: 5, ExpectLeaf: true},
		{Problem: problem("other"), Weight: 2, ExpectLeaf: true},
	}}
	rec, _ := pinRun(t, StaticPlanner{P: p})
	for _, o := range rec.Outcomes {
		if o.Problem.Statement == "dup" && o.PlanWeight != 8 {
			t.Errorf("a deduped child must record the SUMMED weight 8, got %d", o.PlanWeight)
		}
	}
}

// --------------------------------------------------------- portfolio and fallback

func TestPinnedPortfolioArmsKeepTheirWeights(t *testing.T) {
	// A portfolio's arms share a problem key, so a weights list keyed by problem would
	// collapse them — the same defect that lost Strategy. Positional storage keeps all
	// three, and equal weights here mean the test proves survival, not distinctness.
	arms := portfolioOf("attempt", 3)
	for i := range arms.Items {
		arms.Items[i].Weight = int64(i + 1) // 1, 2, 3 — deliberately unequal
	}
	rec, _ := pinRun(t, StaticPlanner{P: arms})

	pp := PinPlan(rec)
	plan, err := pp.Plan(context.Background(), problem("root"), Allocation{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 3 {
		t.Fatalf("all three arms must be pinned, got %d", len(plan.Items))
	}
	var sum int64
	for _, it := range plan.Items {
		sum += it.Weight
	}
	if sum != 6 {
		t.Errorf("arm weights must survive pinning; want 1+2+3=6, got %d", sum)
	}
}

func TestAnUnweightedRecordStillPinsItsShape(t *testing.T) {
	// A record written before weights were recorded has PlanWeight zero everywhere.
	// Zero is not a legal weight (Apportion rejects a non-positive total), so pinning
	// must fall back to uniform rather than fail — an old record should still pin the
	// shape it does have.
	rec, _ := pinRun(t, StaticPlanner{P: weightedPlan("alpha", 6, "beta", 3)})
	for i := range rec.Outcomes {
		rec.Outcomes[i].PlanWeight = 0 // simulate a pre-weight record
	}
	plan, err := PinPlan(rec).Plan(context.Background(), problem("root"), Allocation{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("shape must still pin, got %d items", len(plan.Items))
	}
	for i, it := range plan.Items {
		if it.Weight != 1 {
			t.Errorf("item %d: unrecorded weight must fall back to uniform, got %d", i, it.Weight)
		}
	}
}

func TestUniformFallbackIsDisclosedInTheRecord(t *testing.T) {
	// The difference between a full control and a shape-only one has to be legible to
	// whoever compares the two runs — otherwise a uniform re-apportionment is reported
	// in exactly the same words as a faithful one.
	rec, _ := pinRun(t, StaticPlanner{P: weightedPlan("alpha", 6, "beta", 3)})
	full, _ := PinPlan(rec).Plan(context.Background(), problem("root"), Allocation{}, 0, nil)

	for i := range rec.Outcomes {
		rec.Outcomes[i].PlanWeight = 0
	}
	degraded, _ := PinPlan(rec).Plan(context.Background(), problem("root"), Allocation{}, 0, nil)

	if full.Reasoning == degraded.Reasoning {
		t.Errorf("a uniformly-apportioned pin must say so; both read %q", full.Reasoning)
	}
}

func TestPartialWeightsFallBackWholesale(t *testing.T) {
	// ALL-OR-NOTHING per node. Substituting 1 for only the missing entries would change
	// the RATIO between the recorded ones — a child weighted 6 against a substituted 1
	// gets a share the original plan never assigned — and a wrong apportionment
	// presented as a pinned one is worse than an honest uniform split.
	rec, _ := pinRun(t, StaticPlanner{P: weightedPlan("alpha", 6, "beta", 3)})
	for i := range rec.Outcomes {
		if rec.Outcomes[i].Problem.Statement == "beta" {
			rec.Outcomes[i].PlanWeight = 0 // one weight lost, the other intact
		}
	}
	plan, _ := PinPlan(rec).Plan(context.Background(), problem("root"), Allocation{}, 0, nil)
	for i, it := range plan.Items {
		if it.Weight != 1 {
			t.Errorf("item %d: a partial weight set must fall back wholesale, got %d", i, it.Weight)
		}
	}
}

// ------------------------------------------------------------------- determinism

func TestWeightsDoNotBreakReplayDeterminism(t *testing.T) {
	// PlanWeight is hashed like every other recorded field: a plan's weights are a
	// deterministic property of the plan, so a replay producing different ones has
	// genuinely diverged and the record SHOULD say so (P8).
	// Run through the recorded-provider seam rather than the allocSpy, so the replay
	// can actually find its samples: RecordedProvider keys on the model that produced
	// them, and the spy's model is not the one replayRun asks for.
	plan := weightedPlan("alpha", 6, "beta", 3, "gamma", 1)
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:  StaticPlanner{P: plan},
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 1,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	orig := NewRunRecord(res, root, pinCaps, ModeFresh)

	seams := Replayable(orig)
	replayer := &Executor{
		Planner:  seams.Planner,
		Solver:   ProviderSolver{Provider: seams.Provider, Model: "fake"},
		Reducer:  seams.Reducer,
		Now:      now,
		MaxDepth: 1,
	}
	res2, err := replayer.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	replayed := NewRunRecord(res2, root, pinCaps, ModeFresh)
	if orig.RunID == replayed.RunID {
		return // identical, as expected
	}
	ob, _ := orig.Canonical()
	rb, _ := replayed.Canonical()
	t.Errorf("replay must be byte-identical with weights recorded\n orig: %s\n rep:  %s", ob, rb)
}
