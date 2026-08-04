package quarry

import (
	"context"
	"testing"
)

// These tests ARE the specification for build step 9 (§3 Surplus, §5
// adversarial). A failing test means the design changed — amend docs/design.md in
// the same commit or revert.

// fakeAdversary is the no-network Adversary double. It breaks claims whose text
// contains a target substring and charges a flat cost — deterministic, so it
// underwrites the surplus tests without a model. Real adversaries route through a
// different provider family (§5); that lives in provider/, not here.
type fakeAdversary struct {
	cost      Units
	breakWord string // a claim whose text contains this is "broken"
	calls     int
	assessErr bool // if set, Attack reports ok=false (not assessable)
}

func (a *fakeAdversary) Name() string         { return "fake-adversary" }
func (a *fakeAdversary) CostRatio() float64   { return 0.5 }
func (a *fakeAdversary) Estimate(Claim) Units { return a.cost }

func (a *fakeAdversary) Attack(_ context.Context, c Claim, _ Sample) (bool, string, Units, bool) {
	a.calls++
	if a.assessErr {
		return false, "", a.cost, false
	}
	if a.breakWord != "" && contains(c.Text, a.breakWord) {
		return true, "refuted: " + c.Text, a.cost, true
	}
	return false, "survived", a.cost, true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- exposure (P3)

func TestExposureFavoursDependedOnAndUnverifiedNodes(t *testing.T) {
	// P3: verification spend ∝ downstream exposure. A claim on a node others depend
	// on, or on an unverified node, scores higher — attack it first.
	verified := true
	outcomes := []NodeOutcome{
		{NodeID: "root", Children: []string{"a", "b"}},
		{NodeID: "a", Verified: &verified}, // checked, no dependents
		{NodeID: "b"},                      // unverified, no dependents
	}
	ca := Claim{Text: "from a", NodeID: "a"}
	cb := Claim{Text: "from b", NodeID: "b"}
	if ExposureOf(cb, outcomes) <= ExposureOf(ca, outcomes) {
		t.Errorf("unverified node b must outscore verified node a: b=%d a=%d",
			ExposureOf(cb, outcomes), ExposureOf(ca, outcomes))
	}
}

// -------------------------------------------------------- surplus selection

func TestSurplusPlanSelectsMostExposedFirstWithinBudget(t *testing.T) {
	// Surplus spends the remainder on the HIGHEST-exposure claims (§3). With a
	// budget for two attacks at cost 10, the two most-exposed claims are chosen.
	outcomes := []NodeOutcome{
		{NodeID: "root", Children: []string{"hi", "mid", "lo"}},
		{NodeID: "hi"}, // unverified → +2 exposure
		{NodeID: "mid", Verified: boolp(true), Children: nil},
		{NodeID: "lo", Verified: boolp(true)},
	}
	// Give "mid" a dependent so it outranks "lo" but not the unverified "hi".
	outcomes[2].Children = []string{"lo"}
	claims := []Claim{
		{Text: "lo claim", NodeID: "lo"},
		{Text: "hi claim", NodeID: "hi"},
		{Text: "mid claim", NodeID: "mid"},
	}
	adv := &fakeAdversary{cost: FromFloat(10)}
	sel := SurplusPlan(claims, outcomes, adv, FromFloat(20)) // affords 2
	if len(sel) != 2 {
		t.Fatalf("budget for 2 attacks must select 2, got %d", len(sel))
	}
	if sel[0].NodeID != "hi" {
		t.Errorf("most-exposed (unverified hi) must be first, got %s", sel[0].NodeID)
	}
}

func TestSurplusPlanStopsAtFirstUnaffordableClaim(t *testing.T) {
	// Exposure order is the point: selection stops at the first claim that will not
	// fit rather than skipping to a cheaper lower-exposure one (P3).
	outcomes := []NodeOutcome{{NodeID: "root", Children: []string{"a"}}, {NodeID: "a"}}
	claims := []Claim{{Text: "one", NodeID: "a"}, {Text: "two", NodeID: "a"}}
	adv := &fakeAdversary{cost: FromFloat(10)}
	sel := SurplusPlan(claims, outcomes, adv, FromFloat(10)) // affords exactly 1
	if len(sel) != 1 {
		t.Fatalf("a budget for one attack selects one, got %d", len(sel))
	}
}

// ------------------------------------------------------------- surplus run

func TestRunSurplusAttacksWithinBudgetAndRecordsFindings(t *testing.T) {
	// Surplus is active spend inside the authorized ceiling (§3, P5): each attack
	// passes admission, and the findings land on the receipt (§8).
	l := ledger(t, FromFloat(100))
	adv := &fakeAdversary{cost: FromFloat(10), breakWord: "wrong"}
	claims := []Claim{
		{Text: "this is wrong", NodeID: "a"},
		{Text: "this is fine", NodeID: "b"},
	}
	findings := RunSurplus(context.Background(), l, adv, claims, nil)
	if len(findings) != 2 {
		t.Fatalf("want a finding per attacked claim, got %d", len(findings))
	}
	if !findings[0].Broke || findings[1].Broke {
		t.Errorf("adversary must break the 'wrong' claim only: %+v", findings)
	}
	// Two attacks at 10 each were debited from the surplus balance.
	if l.Balance() != FromFloat(80) {
		t.Errorf("surplus spend must be metered: want balance 80, got %s", l.Balance())
	}
}

func TestRunSurplusStopsWhenBudgetExhausted(t *testing.T) {
	// A claim that cannot be afforded ends the pass — unattacked claims are left
	// unattacked (planned degradation, not a gap: only time is a gap).
	l := ledger(t, FromFloat(15)) // affords one attack at 10, not two
	adv := &fakeAdversary{cost: FromFloat(10)}
	claims := []Claim{{Text: "a", NodeID: "a"}, {Text: "b", NodeID: "b"}}
	findings := RunSurplus(context.Background(), l, adv, claims, nil)
	if len(findings) != 1 {
		t.Errorf("only the affordable attack runs, got %d findings", len(findings))
	}
	if adv.calls != 1 {
		t.Errorf("the unaffordable claim must not be attacked, got %d calls", adv.calls)
	}
}

func TestRunSurplusRecordsUnassessableClaims(t *testing.T) {
	// A claim the adversary cannot assess (ok=false) is recorded as a non-breaking
	// finding so the receipt can still say it was reached — the checked/unchecked
	// distinction the record must preserve (§8).
	l := ledger(t, FromFloat(100))
	adv := &fakeAdversary{cost: FromFloat(10), assessErr: true}
	findings := RunSurplus(context.Background(), l, adv, []Claim{{Text: "x", NodeID: "a"}}, nil)
	if len(findings) != 1 || findings[0].Broke {
		t.Fatalf("unassessable claim must record a non-breaking finding, got %+v", findings)
	}
	if findings[0].Detail != "not assessable" {
		t.Errorf("want 'not assessable' detail, got %q", findings[0].Detail)
	}
}

func boolp(b bool) *bool { return &b }

// ---------------------------------------------- surplus wired into the executor

func TestExecutorSpendsSurplusWhenUnderCap(t *testing.T) {
	// §3 Surplus: a run that completes UNDER cap spends the remainder on
	// adversarial passes over its claims, automatically — no external RunSurplus
	// call. Budget converts to quality rather than evaporating (P5).
	prov := &fakeProvider{cost: FromFloat(1)}
	adv := &fakeAdversary{cost: FromFloat(5), breakWord: "beta"}
	e := &Executor{
		Planner:   StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:    ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:   ConcatReducer{Sep: "|"},
		Extractor: MechanicalExtractor{},
		Adversary: adv,
		Now:       now,
		MaxDepth:  1,
	}
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adversarial) == 0 {
		t.Fatal("a run finishing under cap must spend surplus on adversarial passes")
	}
	if adv.calls == 0 {
		t.Error("the wired adversary must be invoked during the surplus pass")
	}
	// The finding must reach the record for the receipt (§8).
	rec := NewRunRecord(res, problem("root"), Caps{Spend: FromFloat(100)}, ModeFresh)
	if len(rec.Adversarial) != len(res.Adversarial) {
		t.Errorf("surplus findings must be carried into the record, got %d", len(rec.Adversarial))
	}
}

func TestExecutorWithoutAdversaryRunsNoSurplus(t *testing.T) {
	// Opt-in: no Adversary means no surplus pass and an empty findings list, so
	// existing runs and the replay determinism test are unaffected.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := &Executor{
		Planner:   StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:    ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:   ConcatReducer{Sep: "|"},
		Extractor: MechanicalExtractor{},
		Now:       now,
		MaxDepth:  1,
	}
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adversarial) != 0 {
		t.Errorf("no adversary wired must mean no surplus findings, got %d", len(res.Adversarial))
	}
}

func TestExecutorSurplusSkippedWhenBudgetExhausted(t *testing.T) {
	// Surplus is the UNDER-cap case. A run that spends its whole cap has no
	// remainder to convert, so no attack fires.
	prov := &fakeProvider{cost: FromFloat(50)}
	adv := &fakeAdversary{cost: FromFloat(5), breakWord: "beta"}
	e := &Executor{
		Planner:   StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:    ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:   ConcatReducer{Sep: "|"},
		Extractor: MechanicalExtractor{},
		Adversary: adv,
		Estimate:  func(Problem) Units { return FromFloat(50) },
		Now:       now,
		MaxDepth:  1,
	}
	// 100 cap, two children at 50 each = fully spent.
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}
	if adv.calls != 0 {
		t.Errorf("no budget remained, so no surplus attack should fire, got %d calls", adv.calls)
	}
	_ = res
}
