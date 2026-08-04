package quarry

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// These tests ARE the specification for replay once the PLANNER AND REDUCER are model
// calls (§7, P8). A failing test means the design changed — amend docs/design.md in the
// same commit or revert.
//
// Build step 5's determinism test passes with deterministic doubles for both, which
// meant it silently stopped covering the two most important nodes the moment
// provider.BedrockPlanner and provider.BedrockReducer existed. The doubles below are
// STOCHASTIC on purpose: each one answers differently every call, so any test that
// passes here is passing because the record was replayed, not because the double
// happened to agree with itself.

// drifting is a planner and reducer that never repeats itself. It stands in for a
// model: same input, different output, which is precisely why their outputs have to
// be in the record rather than re-derived (P8).
//
// Mutex-guarded, because Planner and Reducer must be safe for concurrent use —
// sibling subtrees plan and reduce on separate goroutines. The race detector caught
// this double before it caught a real implementation, which is the argument for the
// contract now being stated on both seams rather than only on TelemetrySink.
type drifting struct {
	mu      sync.Mutex
	plans   int
	reduces int
}

func (d *drifting) Plan(_ context.Context, _ Problem, _ Allocation, depth int, _ []NodeOutcome) (Plan, error) {
	d.mu.Lock()
	d.plans++
	n := d.plans
	d.mu.Unlock()

	if depth > 0 {
		return Plan{Declined: true, Reasoning: "leaf"}, nil
	}
	// The SHAPE drifts too: a different split on every call. A replay that re-planned
	// live would get a different tree, not merely different prose.
	if n == 1 {
		return fanoutPlan("alpha", "beta"), nil
	}
	return fanoutPlan("gamma", "delta", "epsilon"), nil
}

func (d *drifting) Reduce(_ context.Context, _ Problem, children []NodeOutcome, _ Allocation, _ bool, _ Strategy) (Sample, error) {
	d.mu.Lock()
	d.reduces++
	n := d.reduces
	d.mu.Unlock()

	var b bytes.Buffer
	for _, c := range children {
		b.WriteString(c.Content)
	}
	// Prose drifts on every call, and the reduce COSTS money — an internal node that
	// spends is the case docs/integration-requirements.md records as previously missed.
	return Sample{
		Content:         b.String() + "|synthesis-" + itoa(n),
		Cost:            FromFloat(2),
		HaloTokens:      100,
		GeneratedTokens: 25,
	}, nil
}

// counts reads the call tallies under the lock.
func (d *drifting) counts() (plans, reduces int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.plans, d.reduces
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

var replayCaps = Caps{Spend: FromFloat(1000), Latency: time.Hour}

// stochasticRun does one live run with drifting planner and reducer.
func stochasticRun(t *testing.T) (RunRecord, *drifting, *fakeProvider) {
	t.Helper()
	d := &drifting{}
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:  d,
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  d,
		Now:      now,
		MaxDepth: 2,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, root, replayCaps, ModeFresh), d, prov
}

// replayRun re-runs the same tree against all three recorded seams.
func replayRun(t *testing.T, rec RunRecord) RunRecord {
	t.Helper()
	seams := Replayable(rec)
	e := &Executor{
		Planner:  seams.Planner,
		Solver:   ProviderSolver{Provider: seams.Provider, Model: "fake"},
		Reducer:  seams.Reducer,
		Now:      now,
		MaxDepth: 2,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, root, replayCaps, ModeFresh)
}

// ----------------------------------------------------- the determinism guarantee

func TestReplayIsIdenticalWithAStochasticPlannerAndReducer(t *testing.T) {
	orig, _, _ := stochasticRun(t)
	replayed := replayRun(t, orig)

	ob, err := orig.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := replayed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, rb) {
		t.Fatalf("replay must be byte-identical even when planning and reducing are stochastic\n orig: %s\n rep:  %s", ob, rb)
	}
	if orig.RunID != replayed.RunID {
		t.Errorf("identical content must hash identically: %s vs %s", orig.RunID, replayed.RunID)
	}
}

func TestTheDoublesReallyDrift(t *testing.T) {
	// NON-VACUITY GUARD. If the drifting planner and reducer ever became stable, the
	// test above would keep passing while covering nothing — the exact failure mode the
	// timing determinism test needed its own guard for.
	d := &drifting{}
	p1, _ := d.Plan(context.Background(), problem("root"), Allocation{}, 0, nil)
	p2, _ := d.Plan(context.Background(), problem("root"), Allocation{}, 0, nil)
	if len(p1.Items) == len(p2.Items) {
		t.Error("the planner double must produce a different shape on a second call")
	}
	kids := []NodeOutcome{{Content: "x", Depth: 1}}
	r1, _ := d.Reduce(context.Background(), problem("root"), kids, Allocation{}, false, StrategyPartition)
	r2, _ := d.Reduce(context.Background(), problem("root"), kids, Allocation{}, false, StrategyPartition)
	if r1.Content == r2.Content {
		t.Error("the reducer double must produce different prose on a second call")
	}
}

func TestReplayCallsNeitherPlannerNorReducer(t *testing.T) {
	// Replay must not touch a model. Wiring only the provider — the state before this
	// change — left plan and reduce calls LIVE during "replay": real money, and a tree
	// that would not match.
	orig, d, _ := stochasticRun(t)
	livePlans, liveReduces := d.counts()
	if livePlans == 0 || liveReduces == 0 {
		t.Fatal("the live run must have planned and reduced")
	}
	_ = replayRun(t, orig)
	if plans, reduces := d.counts(); plans != livePlans || reduces != liveReduces {
		t.Errorf("replay used the live seams: plans %d→%d, reduces %d→%d",
			livePlans, plans, liveReduces, reduces)
	}
}

// ------------------------------------------------------------ divergence signals

func TestUnrecordedReductionIsADivergenceNotASilentMerge(t *testing.T) {
	// Being asked to reduce a node the record does not contain means the replay
	// produced a different tree. Folding the children live instead would hide a real
	// signal about the pinned plan behind an answer that looks fine.
	orig, _, _ := stochasticRun(t)
	rr := NewRecordedReducer(orig)
	_, err := rr.Reduce(context.Background(), problem("never-planned"),
		[]NodeOutcome{{Depth: 1, Content: "x"}}, Allocation{}, false, StrategyPartition)
	if err == nil {
		t.Error("an unrecorded reduction must surface as divergence")
	}
}

func TestRecordedReducerIndexesInternalNodesOnly(t *testing.T) {
	// Leaves are the provider's job; a cache hit performed no reduce. Indexing either
	// would let a leaf's content be served as a merge — an answer attributed to a node
	// that never combined anything.
	orig, _, _ := stochasticRun(t)
	rr := NewRecordedReducer(orig)
	if len(rr.byPosition) != 1 {
		t.Errorf("want only the root indexed as an internal node, got %d entries", len(rr.byPosition))
	}
	if _, err := rr.Reduce(context.Background(), problem("alpha"),
		[]NodeOutcome{{Depth: 2}}, Allocation{}, false, StrategyPartition); err == nil {
		t.Error("a recorded LEAF must not be replayable as a reduction")
	}
}

func TestReplayPreservesTheInternalNodesSpend(t *testing.T) {
	// An internal reduce node spends real money (§8.2), so a replay that dropped its
	// cost would produce a cheaper record — a receipt that understates the run, which
	// is the one direction a cost receipt must never be wrong in.
	orig, _, _ := stochasticRun(t)
	replayed := replayRun(t, orig)
	if orig.TotalCost() != replayed.TotalCost() {
		t.Errorf("replayed total must match: %s vs %s", orig.TotalCost(), replayed.TotalCost())
	}
	if orig.Outcomes[0].Cost == 0 {
		t.Fatal("the reduce node must have cost something, or this test proves nothing")
	}
}

// ---------------------------------------------------------- all three or none

func TestReplayableWiresEveryStochasticSeam(t *testing.T) {
	// The trap this guards: wiring a partial replay. A replay is only a replay if all
	// three stochastic seams are substituted, so they are built together from one
	// record rather than assembled by a caller who might forget one.
	orig, _, _ := stochasticRun(t)
	seams := Replayable(orig)
	if seams.Provider == nil || seams.Reducer == nil {
		t.Fatal("Replayable must supply both recorded seams")
	}
	plan, err := seams.Planner.Plan(context.Background(), problem("root"), Allocation{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Declined || len(plan.Items) != 2 {
		t.Errorf("the pinned planner must reproduce the recorded 2-child shape, got %d items",
			len(plan.Items))
	}
}
