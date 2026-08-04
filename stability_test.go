package quarry

import (
	"context"
	"testing"
	"time"
)

// These tests ARE the specification for build step 7 (§7 replicates, stability,
// plan pinning). A failing test means the design changed — amend docs/design.md
// in the same commit or revert.

// recWithClaims fakes a record whose single node carries the given claim texts.
// Stability compares claims, so a record is just a claim carrier here.
func recWithClaims(texts ...string) RunRecord {
	ex := MechanicalExtractor{}
	var claims []Claim
	for _, t := range texts {
		cs, _ := ex.Extract(context.Background(), Sample{Content: t}, "n0")
		claims = append(claims, cs...)
	}
	return RunRecord{Outcomes: []NodeOutcome{{NodeID: "n0", Claims: claims}}}
}

// ------------------------------------------------------- clustering & support

func TestStabilityClustersEquivalentClaims(t *testing.T) {
	// Two replicates asserting the same conclusion (modulo wording) form one
	// cluster with support 2 — agreement is measured on conclusions, not text (§7).
	r1 := recWithClaims("The sky is blue.")
	r2 := recWithClaims("the   SKY is BLUE")
	rep := Stability([]RunRecord{r1, r2}, nil, 0)
	if len(rep.Claims) != 1 {
		t.Fatalf("equivalent claims must cluster to one, got %d", len(rep.Claims))
	}
	if rep.Claims[0].Support != 2 {
		t.Errorf("want support 2 across 2 replicates, got %d", rep.Claims[0].Support)
	}
}

func TestUnanimousClaimIsStableDisagreedIsNot(t *testing.T) {
	// Default minSupport is unanimity. A claim in every replicate is stable; one in
	// a single replicate is unstable and is the valuable output (§7).
	r1 := recWithClaims("Prices rose.", "Rates held.")
	r2 := recWithClaims("prices rose", "Rates fell.")
	rep := Stability([]RunRecord{r1, r2}, nil, 0)

	unstable := rep.Unstable()
	// "rates held" and "rates fell" each appear once → both unstable; "prices rose"
	// unanimous → stable.
	if len(rep.Stable()) != 1 {
		t.Errorf("want 1 unanimous stable claim, got %d", len(rep.Stable()))
	}
	if len(unstable) != 2 {
		t.Errorf("want 2 unstable claims, got %d: %v", len(unstable), unstable)
	}
}

func TestWithinReplicateRepetitionDoesNotInflateSupport(t *testing.T) {
	// A single run repeating a claim across nodes must not count as its own
	// agreement — support counts DISTINCT replicates (P7: n=1 is one sample).
	dup := RunRecord{Outcomes: []NodeOutcome{
		{NodeID: "a", Claims: mustClaims("The sky is blue.")},
		{NodeID: "b", Claims: mustClaims("the sky is blue")},
	}}
	rep := Stability([]RunRecord{dup}, nil, 1)
	if len(rep.Claims) != 1 {
		t.Fatalf("one distinct claim expected, got %d", len(rep.Claims))
	}
	if rep.Claims[0].Support != 1 {
		t.Errorf("repetition within one replicate must not inflate support, got %d", rep.Claims[0].Support)
	}
}

func TestStabilityThresholdBelowUnanimity(t *testing.T) {
	// A minSupport below the replicate count lets a majority claim count as stable.
	r1 := recWithClaims("Alpha.")
	r2 := recWithClaims("Alpha.")
	r3 := recWithClaims("Beta.")
	rep := Stability([]RunRecord{r1, r2, r3}, nil, 2)
	if len(rep.Stable()) != 1 || rep.Stable()[0].Support != 2 {
		t.Errorf("2-of-3 claim must be stable at minSupport 2, got %+v", rep.Stable())
	}
}

func TestStabilityRateReportedAlongsideList(t *testing.T) {
	r1 := recWithClaims("Alpha.", "Beta.")
	r2 := recWithClaims("Alpha.", "Gamma.")
	rep := Stability([]RunRecord{r1, r2}, nil, 0)
	rate, ok := rep.StabilityRate()
	if !ok {
		t.Fatal("a report with claims must have a rate")
	}
	// 3 distinct claims (alpha, beta, gamma); only alpha is unanimous → 1/3.
	if rate < 0.33 || rate > 0.34 {
		t.Errorf("want ~0.333 stability rate, got %f", rate)
	}
}

func TestStabilityRateEmptyWhenNoClaims(t *testing.T) {
	rep := Stability(nil, nil, 0)
	if _, ok := rep.StabilityRate(); ok {
		t.Error("no claims means no rate")
	}
}

func mustClaims(texts ...string) []Claim {
	ex := MechanicalExtractor{}
	var out []Claim
	for _, t := range texts {
		cs, _ := ex.Extract(context.Background(), Sample{Content: t}, "n0")
		out = append(out, cs...)
	}
	return out
}

// --------------------------------------------------------------- replicate

func TestReplicateDrawsNIndependentRuns(t *testing.T) {
	// n independent draws, one record each (§7). Cache left nil so each run draws
	// fresh — the independence the whole method depends on.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:   StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:    ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:   ConcatReducer{Sep: "|"},
		Extractor: MechanicalExtractor{},
		Now:       now,
		MaxDepth:  2,
	}
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	recs, err := Replicate(context.Background(), e, problem("root"), caps, Scope{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("want 3 replicate records, got %d", len(recs))
	}
	// Deterministic fake → identical conclusions → every claim unanimous.
	rep := Stability(recs, nil, 0)
	if len(rep.Unstable()) != 0 {
		t.Errorf("a deterministic provider must yield fully stable claims, got %d unstable",
			len(rep.Unstable()))
	}
}

func TestReplicateGivesEachRunItsOwnLedger(t *testing.T) {
	// Independence at the budget level: three runs each pay in full, so the fake's
	// call count is per-run, not shared. (No cache, so no cross-run serving.)
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:  StaticPlanner{P: fanoutPlan("a", "b")},
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{},
		Now:      now,
		MaxDepth: 1, // two-level tree: root plans, children solve as leaves
	}
	caps := Caps{Spend: FromFloat(100), Latency: time.Hour}
	if _, err := Replicate(context.Background(), e, problem("root"), caps, Scope{}, 3); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 6 { // 2 children × 3 runs
		t.Errorf("want 6 calls across 3 independent runs, got %d", prov.calls)
	}
}

// --------------------------------------------------------------- plan pinning

func TestPinnedPlannerReplaysRecordedShape(t *testing.T) {
	// Pinning re-runs the recorded decomposition: same children, so the same fanout
	// happens on a fresh run (§7). This attributes spread to solving, not planning.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:  StaticPlanner{P: fanoutPlan("alpha", "beta", "gamma")},
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 1, // two-level tree; distinct leaves, no self-similar key collision
	}
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRunRecord(res, root, caps, ModeFresh)

	// Re-run with the plan pinned; the StaticPlanner is replaced by the recorded
	// shape and must reproduce the same 3-child fanout.
	prov2 := &fakeProvider{cost: FromFloat(1)}
	pinned := &Executor{
		Planner:  PinPlan(rec),
		Solver:   ProviderSolver{Provider: prov2, Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 1,
	}
	res2, err := pinned.Run(context.Background(), root, ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if prov2.calls != 3 {
		t.Errorf("pinned re-run must reproduce the 3-child fanout, got %d calls", prov2.calls)
	}
	if res2.Answer.Content != res.Answer.Content {
		t.Errorf("pinned shape must fold to the same answer: %q vs %q",
			res2.Answer.Content, res.Answer.Content)
	}
}

func TestPinnedLeafDeclinesToSplit(t *testing.T) {
	// A problem the record solved as a leaf must not sprout a split on pin: pinning
	// reproduces the shape, it never invents one (§7).
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:  DeclinePlanner{},
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{},
		Now:      now,
		MaxDepth: 2,
	}
	caps := Caps{Spend: FromFloat(100), Latency: time.Hour}
	root := problem("root")
	res, _ := e.Run(context.Background(), root, ledger(t, FromFloat(100)))
	rec := NewRunRecord(res, root, caps, ModeFresh)

	pp := PinPlan(rec)
	plan, _ := pp.Plan(context.Background(), root, Allocation{}, 0, nil)
	if !plan.Declined {
		t.Error("a recorded leaf must decline under the pinned planner")
	}
}
