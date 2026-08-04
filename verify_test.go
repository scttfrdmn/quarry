package quarry

import (
	"context"
	"regexp"
	"sync/atomic"
	"testing"
)

// These tests ARE the specification for build step 4 (§5, P2, P3). A failing
// test means the design changed — amend docs/design.md in the same commit or
// revert.

// flakyProvider fails verification for its first `bad` calls, then succeeds.
// Models a solver that gets it right on retry — the case retry-in-place exists
// for (§5).
type flakyProvider struct {
	bad   int64 // number of leading calls that return unusable content
	calls int64
	cost  Units
}

func (f *flakyProvider) Complete(ctx context.Context, prompt, model string, scope Scope) (Sample, error) {
	n := atomic.AddInt64(&f.calls, 1)
	content := "good:" + prompt
	if n <= f.bad {
		content = "" // an empty result fails the non-empty oracle
	}
	return Sample{Content: content, Cost: f.cost, Model: model, ModelVersion: model + "-v1"}, nil
}

func (f *flakyProvider) Estimate(prompt, model string) Units { return f.cost }

// ------------------------------------------------------- mechanical oracles

func TestNonEmptyOraclePassesAndFails(t *testing.T) {
	v := NonEmptyOracle()
	if v.CostRatio() != 0 {
		t.Errorf("a mechanical oracle costs ~0, got %v", v.CostRatio())
	}
	if passed, ok := v.Verify(context.Background(), problem("p"), Sample{Content: "x"}); !ok || !passed {
		t.Error("non-empty content must pass")
	}
	if passed, ok := v.Verify(context.Background(), problem("p"), Sample{Content: ""}); !ok || passed {
		t.Error("empty content must fail, and the check must have run")
	}
}

func TestMultiVerifierIsUnavailableWhenNoMemberApplies(t *testing.T) {
	// A verifier that applies to nothing reports ok=false — the node is
	// unchecked, not checked-and-passed. The receipt must tell these apart (§8).
	only := FuncVerifier{
		Label:   "python-only",
		Check:   func(Problem, Sample) bool { return true },
		Applies: func(p Problem) bool { return p.Statement == "python" },
	}
	m := MultiVerifier{Verifiers: []Verifier{only}}
	if m.AvailableFor(problem("prose")) {
		t.Error("must be unavailable when no member applies")
	}
	if _, ok := m.Verify(context.Background(), problem("prose"), Sample{Content: "x"}); ok {
		t.Error("an unavailable verifier reports ok=false, not a pass")
	}
}

func TestMultiVerifierRunsCheapestFirstAndShortCircuits(t *testing.T) {
	// The §5 ladder: a ~0 oracle that fails saves the cost of everything above
	// it. Record call order; the expensive check must not run once the cheap one
	// fails.
	var order []string
	cheap := FuncVerifier{Label: "cheap", Check: func(Problem, Sample) bool {
		order = append(order, "cheap")
		return false // fails
	}}
	expensive := trackedVerifier{label: "pricey", ratio: 0.5, ran: &order, result: true}
	m := MultiVerifier{Verifiers: []Verifier{expensive, cheap}}
	passed, ok := m.Verify(context.Background(), problem("p"), Sample{Content: "x"})
	if !ok || passed {
		t.Error("a failed cheap oracle must fail the set")
	}
	if len(order) != 1 || order[0] != "cheap" {
		t.Errorf("cheapest must run first and short-circuit, got %v", order)
	}
}

type trackedVerifier struct {
	label  string
	ratio  float64
	ran    *[]string
	result bool
}

func (v trackedVerifier) Name() string              { return v.label }
func (v trackedVerifier) CostRatio() float64        { return v.ratio }
func (v trackedVerifier) AvailableFor(Problem) bool { return true }
func (v trackedVerifier) Verify(context.Context, Problem, Sample) (bool, bool) {
	*v.ran = append(*v.ran, v.label)
	return v.result, true
}

// --------------------------------------------------- P2: verifier gates depth

func TestNoVerifierAvailableStopsRecursion(t *testing.T) {
	// P2, primary terminator: a node whose problem cannot be verified must solve
	// directly rather than sit atop an unverifiable subtree. The StaticPlanner
	// would otherwise recurse to MaxDepth.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.MaxDepth = 10
	// A verifier available for nothing: recursion must stop at the root.
	e.Verifier = FuncVerifier{
		Label:   "never",
		Check:   func(Problem, Sample) bool { return true },
		Applies: func(Problem) bool { return false },
	}
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].BaseCase != BaseNoVerifier {
		t.Fatalf("want a single no-verifier leaf, got %d outcomes (base %q)",
			len(res.Outcomes), res.Outcomes[0].BaseCase)
	}
	if prov.calls != 1 {
		t.Errorf("unverifiable node solves once, no fanout: got %d calls", prov.calls)
	}
}

func TestVerifierAvailableAllowsRecursion(t *testing.T) {
	// The complement: with a verifier available, P2 does not block descent, and
	// the tree recurses to the backstop.
	prov := &fakeProvider{cost: 0}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.MaxDepth = 2
	e.Verifier = NonEmptyOracle() // available everywhere
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outcomes) != 7 { // depth-2 binary tree: 1+2+4
		t.Errorf("want a full depth-2 tree, got %d nodes", len(res.Outcomes))
	}
}

// --------------------------------------------------- retry in place (§5, §3)

func TestFailedVerificationRetriesInPlace(t *testing.T) {
	// A leaf whose first result fails the oracle re-solves until it passes.
	prov := &flakyProvider{bad: 1, cost: FromFloat(1)}
	e := exec(t, DeclinePlanner{}, prov)
	e.Verifier = NonEmptyOracle()
	e.MaxRetries = 3
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	o := res.Outcomes[0]
	if o.Retries != 1 {
		t.Errorf("one bad attempt then a good one: want 1 retry, got %d", o.Retries)
	}
	if o.Verified == nil || !*o.Verified {
		t.Error("the retried result must end verified")
	}
	if prov.calls != 2 {
		t.Errorf("want 2 solves (1 bad + 1 good), got %d", prov.calls)
	}
	if o.Cost != FromFloat(2) {
		t.Errorf("retries are not free: want cost 2, got %s", o.Cost)
	}
}

func TestRetriesStopAtMaxAndRecordTheFailure(t *testing.T) {
	// Retries are bounded. When they run out the node is recorded verified=false,
	// not silently dropped — the receipt keeps the failure (§8).
	prov := &flakyProvider{bad: 100, cost: FromFloat(1)} // never succeeds
	e := exec(t, DeclinePlanner{}, prov)
	e.Verifier = NonEmptyOracle()
	e.MaxRetries = 2
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	o := res.Outcomes[0]
	if o.Retries != 2 {
		t.Errorf("want exactly MaxRetries=2, got %d", o.Retries)
	}
	if o.Verified == nil || *o.Verified {
		t.Error("an exhausted retry budget must record verified=false")
	}
	if prov.calls != 3 { // 1 initial + 2 retries
		t.Errorf("want 3 solves, got %d", prov.calls)
	}
}

func TestRetryStopsWhenBudgetExhausted(t *testing.T) {
	// Budget(Retry(agent)) (§3): retries consume budget and stop when the reserve
	// cannot fund another attempt, even below MaxRetries.
	prov := &flakyProvider{bad: 100, cost: FromFloat(10)}
	e := exec(t, DeclinePlanner{}, prov)
	e.Verifier = NonEmptyOracle()
	e.MaxRetries = 100
	e.Estimate = func(Problem) Units { return FromFloat(10) }
	// 25 units funds 2 attempts (10 each); the third fails admission.
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(25)))
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls > 2 {
		t.Errorf("budget funds 2 attempts, not %d", prov.calls)
	}
	if res.Outcomes[0].Verified == nil || *res.Outcomes[0].Verified {
		t.Error("stopped-on-budget must still record the failed verdict")
	}
}

func TestUnverifiedNodeHasNilVerified(t *testing.T) {
	// No verifier available means unchecked, distinct from checked-and-failed.
	// Verified stays nil (§8).
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, DeclinePlanner{}, prov) // no Verifier set
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[0].Verified != nil {
		t.Error("an unchecked node must have Verified == nil")
	}
}

var _ = regexp.MustCompile
