package quarry

import (
	"context"
	"testing"
)

// These tests ARE the specification for build step 8 (§4 cost estimation). A
// failing test means the design changed — amend docs/design.md in the same commit
// or revert. Everything under test is ADVISORY (P4): these pin the estimator's
// math and its honesty flags, not a contract anything depends on.

// --------------------------------------------------------- structural ceiling

func TestStructuralCeilingIsTheGeometricSum(t *testing.T) {
	// b=2, D=2: 1 + 2 + 4 = 7 nodes, times per-node cost. Always true, wildly
	// pessimistic — the CAP, not the estimate (§4).
	got := StructuralCeiling(2, 2, FromFloat(1))
	if got != FromFloat(7) {
		t.Errorf("want 7 nodes worth, got %s", got)
	}
}

func TestStructuralCeilingChainWhenNoBranching(t *testing.T) {
	// b<=1 degenerates to a chain of D+1 nodes, not a divide-by-zero.
	if got := StructuralCeiling(1, 3, FromFloat(2)); got != FromFloat(8) {
		t.Errorf("chain of 4 nodes at cost 2 = 8, got %s", got)
	}
	if got := StructuralCeiling(0, 0, FromFloat(5)); got != FromFloat(5) {
		t.Errorf("single node, got %s", got)
	}
}

func TestStructuralCeilingUnlimitedPerNodeStaysUnlimited(t *testing.T) {
	if got := StructuralCeiling(2, 2, Unlimited); got.Limited() {
		t.Errorf("an unlimited per-node cost cannot yield a finite ceiling, got %s", got)
	}
}

// ------------------------------------------------------- Galton-Watson project

func TestProjectSubcriticalConverges(t *testing.T) {
	// m < 1: the process is subcritical. Node count is finite and the estimate is
	// neither divergent nor near-unity — the regime where a number is meaningful.
	est := Project(0.5, 0.25, 10, FromFloat(1))
	if est.Diverges {
		t.Error("m=0.5 is subcritical, must not be flagged divergent")
	}
	if est.NearUnity {
		t.Error("m=0.5 is well away from unity")
	}
	// Truncated sum Σ 0.5^k over k=0..10 ≈ 1.999, below the 1/(1-m)=2 limit.
	if est.Nodes < 1.9 || est.Nodes > 2.0 {
		t.Errorf("subcritical node count should approach 1/(1-m)=2, got %f", est.Nodes)
	}
	if !est.P50.Limited() || est.P50 <= 0 {
		t.Errorf("a subcritical projection must quote a positive finite P50, got %s", est.P50)
	}
}

func TestProjectFlagsSupercritical(t *testing.T) {
	// m >= 1: the branching process is critical/supercritical. Only the ceiling is
	// trustworthy; the flag makes the UI say so rather than quoting theatre (§4).
	est := Project(1.5, 0.5, 8, FromFloat(1))
	if !est.Diverges {
		t.Error("m=1.5 must be flagged divergent")
	}
}

func TestProjectFlagsNearUnity(t *testing.T) {
	// Near m=1 the variance dominates the mean and any single number is theatre —
	// the estimate must announce it, not hide it (§4).
	est := Project(1.0, 1.0, 8, FromFloat(1))
	if !est.NearUnity {
		t.Error("m=1.0 must be flagged near-unity")
	}
}

func TestProjectP90ExceedsP50UnderSkew(t *testing.T) {
	// The report is three numbers, never one (§4). With real variance the fitted
	// lognormal is right-skewed, so P90 carries the tail above the median.
	est := Project(0.7, 1.0, 6, FromFloat(1))
	if est.P90 < est.P50 {
		t.Errorf("P90 must be >= P50, got P50=%s P90=%s", est.P50, est.P90)
	}
	if est.Ceiling < est.P90 {
		t.Errorf("the structural ceiling must dominate the projection, ceiling=%s P90=%s",
			est.Ceiling, est.P90)
	}
}

func TestProjectDeterministic(t *testing.T) {
	// Same inputs, same estimate — advisory but still a pure function, so the UI
	// does not flicker between refreshes.
	a := Project(0.6, 0.3, 7, FromFloat(2))
	b := Project(0.6, 0.3, 7, FromFloat(2))
	if a != b {
		t.Errorf("projection must be deterministic: %+v vs %+v", a, b)
	}
}

// --------------------------------------------------------------- the probe

func TestProbeReadsBranchingOffThePlan(t *testing.T) {
	// One planner call yields the depth-1 branching factor: three recursing
	// children → m=3, no fanout beyond the planner (§4).
	plan := Plan{Items: []PlanItem{
		{Problem: problem("a")}, {Problem: problem("b")}, {Problem: problem("c")},
	}} // none tagged ExpectLeaf → all recurse
	m, _, _, err := Probe(context.Background(), StaticPlanner{P: plan}, problem("root"), Allocation{})
	if err != nil {
		t.Fatal(err)
	}
	if m != 3 {
		t.Errorf("three recursing children → m=3, got %f", m)
	}
}

func TestProbeLeafChildrenDoNotCountAsOffspring(t *testing.T) {
	// A child expected to be a leaf will not recurse, so it is zero offspring.
	m, _, _, err := Probe(context.Background(), StaticPlanner{P: fanoutPlan("a", "b")},
		problem("root"), Allocation{})
	if err != nil {
		t.Fatal(err)
	}
	if m != 0 {
		t.Errorf("all-leaf plan → m=0, got %f", m)
	}
}

func TestProbeDeclinedPlanHasZeroOffspring(t *testing.T) {
	m, v, _, err := Probe(context.Background(), DeclinePlanner{}, problem("root"), Allocation{})
	if err != nil {
		t.Fatal(err)
	}
	if m != 0 || v != 0 {
		t.Errorf("a declined plan does not decompose: want m=0 v=0, got m=%f v=%f", m, v)
	}
}

// ------------------------------------------------------- plan-variance diag

func TestSamplePlansZeroDivergenceForDeterministicPlanner(t *testing.T) {
	// A deterministic planner has no divergence — the diagnostic reports zero, and
	// that is correct, not a failure (§4).
	pv, err := SamplePlans(context.Background(), StaticPlanner{P: fanoutPlan("a", "b")},
		problem("root"), Allocation{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if pv.StdevM != 0 {
		t.Errorf("deterministic planner must show zero plan divergence, got %f", pv.StdevM)
	}
	if pv.Underspecified {
		t.Error("zero divergence must not be flagged underspecified")
	}
	if pv.K != 5 || len(pv.ItemCounts) != 5 {
		t.Errorf("want k=5 samples recorded, got K=%d counts=%d", pv.K, len(pv.ItemCounts))
	}
}

// varyingPlanner alternates between a wide and a narrow plan, so successive
// samples diverge — a stand-in for a stochastic planner.
type varyingPlanner struct{ n int }

func (v *varyingPlanner) Plan(_ context.Context, _ Problem, _ Allocation, _ int, _ []NodeOutcome) (Plan, error) {
	v.n++
	if v.n%2 == 0 {
		return Plan{Items: []PlanItem{{Problem: problem("a")}, {Problem: problem("b")},
			{Problem: problem("c")}, {Problem: problem("d")}}}, nil
	}
	return Plan{Items: []PlanItem{{Problem: problem("a")}}}, nil
}

func TestSamplePlansFlagsUnderspecification(t *testing.T) {
	// Plans that swing between wide and narrow across samples diverge enough to
	// warn the researcher the problem is underspecified before any spend (§4).
	pv, err := SamplePlans(context.Background(), &varyingPlanner{}, problem("root"), Allocation{}, 6)
	if err != nil {
		t.Fatal(err)
	}
	if pv.StdevM == 0 {
		t.Fatal("diverging plans must show nonzero divergence")
	}
	if !pv.Underspecified {
		t.Errorf("high plan divergence (stdev %f) must flag underspecified", pv.StdevM)
	}
}

// --------------------------------------------------------- calibration corpus

func TestCalibratorPrefersHistoricalActuals(t *testing.T) {
	// Every run deposits (shape → ACTUAL spend); nearest-neighbour over the class
	// returns the median actual, the historical data that beats an a-priori model
	// (§4, the SLURM-walltime situation).
	c := NewCalibrator()
	root := problem("recurring-question")
	caps := Caps{Spend: FromFloat(1000)}
	for _, cost := range []Units{FromFloat(10), FromFloat(20), FromFloat(30)} {
		rec := RunRecord{
			Problem:  root,
			Outcomes: []NodeOutcome{{NodeID: "n0", Cost: cost, Children: []string{"n0.0"}}},
			Caps:     caps,
		}
		c.Deposit(rec)
	}
	med, n, ok := c.EstimateFor(root)
	if !ok {
		t.Fatal("a class with deposits must return an estimate")
	}
	if n != 3 {
		t.Errorf("want 3 samples in the class, got %d", n)
	}
	if med != FromFloat(20) {
		t.Errorf("median of 10,20,30 is 20, got %s", med)
	}
}

func TestCalibratorColdProblemFallsThrough(t *testing.T) {
	// No matching class → ok=false, the signal to fall back to the a-priori
	// projection for a cold problem (§4).
	c := NewCalibrator()
	if _, _, ok := c.EstimateFor(problem("never-seen")); ok {
		t.Error("a cold problem must not return a historical estimate")
	}
}

func TestCalibratorKeysAreScopeQualified(t *testing.T) {
	// The class key carries scope exactly as the cache does (P6): the same
	// statement under a different scope is a different class, so learning cannot
	// leak across the entitlement boundary (§8.2 guardrail).
	c := NewCalibrator()
	stmt := "same statement"
	a := Problem{Statement: stmt, Scope: Scope{Tags: map[string]string{"tenant": "x"}}}
	b := Problem{Statement: stmt, Scope: Scope{Tags: map[string]string{"tenant": "y"}}}
	c.Deposit(RunRecord{Problem: a, Outcomes: []NodeOutcome{{NodeID: "n0", Cost: FromFloat(5)}}})
	if _, _, ok := c.EstimateFor(b); ok {
		t.Error("a different scope is a different class — must not serve across P6")
	}
	if _, _, ok := c.EstimateFor(a); !ok {
		t.Error("the depositing scope must find its own class")
	}
}

func TestCalibratorEmptyRunDepositsNothing(t *testing.T) {
	c := NewCalibrator()
	c.Deposit(RunRecord{Problem: problem("x")}) // no outcomes
	if c.N(problem("x")) != 0 {
		t.Error("a run with no outcomes has no shape to deposit")
	}
}
