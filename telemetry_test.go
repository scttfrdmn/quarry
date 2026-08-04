package quarry

import (
	"context"
	"sync"
	"testing"
	"time"
)

// These tests ARE the specification for the telemetry aggregation half of build
// step 10 (§8.2). A failing test means the design changed — amend docs/design.md
// in the same commit or revert. The load-bearing invariant is the GOODHART
// guardrail: no efficiency metric without a quality denominator.

// ---------------------------------------------------- the Goodhart guardrail

func TestNoBareCostPerRunMetric(t *testing.T) {
	// §8.2, structural: cost-per-run is trivially gamed by doing less, so it must
	// not exist as a metric. This test guards the guardrail — if someone adds a
	// cost_per_run key or a CostPerRun method, it must be caught here.
	rec := RunRecord{Outcomes: []NodeOutcome{{NodeID: "n0", Cost: FromFloat(10)}}}
	m := RunMetrics(rec)
	for k := range m {
		if k == "cost_per_run" || k == "cost_per_node" {
			t.Errorf("forbidden bare efficiency metric %q — needs a quality denominator (§8.2)", k)
		}
	}
	// total_cost (a raw count, not a ratio) is fine; the forbidden thing is a
	// cost-per-unit-of-work ratio with no quality in the denominator.
	if _, ok := m["total_cost"]; !ok {
		t.Error("raw total_cost is a legitimate count and should be present")
	}
}

func TestCostPerVerifiedClaimOnlyWhenClaimsVerified(t *testing.T) {
	// The one allowed cost ratio has quality in the denominator (§8.2). Absent any
	// verified claim, it is omitted rather than reported as zero or infinity.
	bare := RunMetrics(RunRecord{Outcomes: []NodeOutcome{{NodeID: "n0", Cost: FromFloat(10)}}})
	if _, ok := bare["cost_per_verified_claim"]; ok {
		t.Error("no verified claims → the ratio must be omitted, not fabricated")
	}

	yes := true
	withClaim := RunMetrics(RunRecord{Outcomes: []NodeOutcome{{
		NodeID:   "n0",
		Cost:     FromFloat(10),
		Verified: &yes,
		Claims:   []Claim{{Text: "a claim", NodeID: "n0"}},
	}}})
	if _, ok := withClaim["cost_per_verified_claim"]; !ok {
		t.Error("a verified claim must yield the cost-per-verified-claim ratio")
	}
}

// -------------------------------------------------------- node aggregation

func TestAggregateSinkClassifiesNodes(t *testing.T) {
	yes, no := true, false
	sink := NewAggregateSink()
	sink.Node(NodeOutcome{NodeID: "a", Cost: FromFloat(5), Verified: &yes})   // verified
	sink.Node(NodeOutcome{NodeID: "b", Cost: FromFloat(5), Verified: &no})    // failed
	sink.Node(NodeOutcome{NodeID: "c", Cost: FromFloat(5)})                   // unverified
	sink.Node(NodeOutcome{NodeID: "d", Gap: true})                            // gap
	sink.Node(NodeOutcome{NodeID: "e", CacheHit: true})                       // cache hit
	sink.Node(NodeOutcome{NodeID: "f", BaseCase: BaseNoVerifier, Retries: 2}) // leaf w/ retries

	m := sink.Snapshot()
	if m.Nodes != 6 {
		t.Errorf("want 6 nodes, got %d", m.Nodes)
	}
	// c (plain), e (cache hit, nil verdict) and f (leaf, nil verdict) are all
	// unverified: a nil verdict on a non-gap node is genuinely unchecked, and a
	// cache hit with no stored verdict is unchecked too (§8).
	if m.Verified != 1 || m.VerifyFailed != 1 || m.Unverified != 3 || m.Gaps != 1 {
		t.Errorf("misclassified: verified=%d failed=%d unverified=%d gaps=%d",
			m.Verified, m.VerifyFailed, m.Unverified, m.Gaps)
	}
	if m.CacheHits != 1 {
		t.Errorf("want 1 cache hit, got %d", m.CacheHits)
	}
	if m.Retries != 2 {
		t.Errorf("want 2 retries summed, got %d", m.Retries)
	}
	if m.BaseCases[BaseNoVerifier] != 1 {
		t.Errorf("base-case distribution must be tracked, got %v", m.BaseCases)
	}
	// Only a, b, c carry cost (5 each); d/e/f have none.
	if m.TotalCost != FromFloat(15) {
		t.Errorf("want summed cost 15, got %s", m.TotalCost)
	}
}

func TestGapIsNotCountedUnverified(t *testing.T) {
	// A gap produced no answer, so it is a degradation event, NOT an unverified
	// answer — the same distinction the record's Unverified list draws (§3.1, §8).
	sink := NewAggregateSink()
	sink.Node(NodeOutcome{NodeID: "g", Gap: true}) // Verified is nil, but it is a gap
	m := sink.Snapshot()
	if m.Unverified != 0 {
		t.Errorf("a gap must not be counted unverified, got %d", m.Unverified)
	}
	if m.Gaps != 1 {
		t.Errorf("a gap must be counted as a degradation event, got %d", m.Gaps)
	}
}

func TestVerifiedRateHasQualityInNumerator(t *testing.T) {
	yes, no := true, false
	sink := NewAggregateSink()
	sink.Node(NodeOutcome{NodeID: "a", Verified: &yes})
	sink.Node(NodeOutcome{NodeID: "b", Verified: &yes})
	sink.Node(NodeOutcome{NodeID: "c", Verified: &no})
	sink.Node(NodeOutcome{NodeID: "d"}) // unverified — must not count in the denominator

	rate, ok := sink.Snapshot().VerifiedRate()
	if !ok {
		t.Fatal("assessed nodes must yield a rate")
	}
	if rate < 0.66 || rate > 0.67 {
		t.Errorf("2 of 3 ASSESSED nodes passed → ~0.667, got %f (unverified must not dilute)", rate)
	}
}

func TestVerifiedRateEmptyWhenNothingAssessed(t *testing.T) {
	sink := NewAggregateSink()
	sink.Node(NodeOutcome{NodeID: "a"}) // unverified
	if _, ok := sink.Snapshot().VerifiedRate(); ok {
		t.Error("nothing assessed → no rate")
	}
}

// ------------------------------------------- P1 becomes observable (§8.2)

func TestAggregateSinkSumsTheTokenSplit(t *testing.T) {
	// The reason tokens moved onto NodeOutcome: an aggregator can now report the one
	// ratio P1 is judged on. Before, a sink saw cost but not the split, so the
	// observer could not reach the metric it exists to report.
	sink := NewAggregateSink()
	sink.Node(NodeOutcome{NodeID: "a", HaloTokens: 90, GeneratedTokens: 30})
	sink.Node(NodeOutcome{NodeID: "b", HaloTokens: 30, GeneratedTokens: 10})
	sink.Node(NodeOutcome{NodeID: "root", Children: []string{"a", "b"}}) // reduce, no model

	m := sink.Snapshot()
	if m.HaloTokens != 120 || m.GeneratedTokens != 40 {
		t.Fatalf("want 120/40 summed, got %d/%d", m.HaloTokens, m.GeneratedTokens)
	}
	// The aggregate ratio is over the whole tree, not a mean of per-node ratios: a
	// mean would weight a tiny node equally with an expensive one.
	r, ok := m.SurfaceToVolume()
	if !ok || r != 3.0 {
		t.Errorf("want aggregate ratio 3.0, got %v ok=%v", r, ok)
	}
}

func TestAggregateSurfaceToVolumeIsAbsentNotZero(t *testing.T) {
	// Same absence-not-zero rule as NodeOutcome.SurfaceToVolume and
	// NodeTiming.Duration: 0.0 claims a measured ratio of zero, which would read as
	// "this run consumed context and produced nothing" — a real and alarming finding.
	sink := NewAggregateSink()
	sink.Node(NodeOutcome{NodeID: "a", HaloTokens: 100}) // nothing generated
	if _, ok := sink.Snapshot().SurfaceToVolume(); ok {
		t.Error("no generated tokens means no ratio")
	}
}

func TestSurfaceToVolumeIsNotACostRatio(t *testing.T) {
	// §8.2 guardrail check. Surface-to-volume is admissible where cost-per-run is not
	// BECAUSE both its terms are work done: you cannot improve it by verifying less,
	// recursing shallower, or taking more cache hits. Pinned by construction —
	// changing the cost must not move the ratio.
	mk := func(cost Units) Metrics {
		s := NewAggregateSink()
		s.Node(NodeOutcome{NodeID: "a", Cost: cost, HaloTokens: 80, GeneratedTokens: 20})
		return s.Snapshot()
	}
	cheap, _ := mk(FromFloat(1)).SurfaceToVolume()
	dear, _ := mk(FromFloat(1000)).SurfaceToVolume()
	if cheap != dear {
		t.Errorf("the ratio must be independent of spend: %v vs %v", cheap, dear)
	}
}

func TestTimedNodesCountsMeasurementsNotDurations(t *testing.T) {
	// A count, deliberately, not a duration sum: parent brackets CONTAIN their
	// children's, so summing would multiply-count the same wall-clock. What a
	// consumer needs from the aggregate is whether timing is trustworthy at all —
	// "fast" and "never measured" must stay distinguishable (§9).
	sink := NewAggregateSink()
	sink.Node(NodeOutcome{NodeID: "a", Timing: NodeTiming{
		StartedAt: now, EndedAt: now.Add(time.Second),
	}})
	sink.Node(NodeOutcome{NodeID: "b"})                                     // no clock was wired
	sink.Node(NodeOutcome{NodeID: "c", Timing: NodeTiming{StartedAt: now}}) // half-stamped
	sink.Node(NodeOutcome{NodeID: "d", Timing: NodeTiming{                  // reversed
		StartedAt: now.Add(time.Second), EndedAt: now,
	}})

	if got := sink.Snapshot().TimedNodes; got != 1 {
		t.Errorf("only coherent brackets count as measured, got %d", got)
	}
}

// --------------------------------------------------- concurrency (contract)

func TestAggregateSinkIsConcurrencySafe(t *testing.T) {
	// The TelemetrySink contract requires Node to be safe under concurrent calls —
	// siblings emit from separate goroutines. Run under -race to make this real.
	sink := NewAggregateSink()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink.Node(NodeOutcome{NodeID: "x", Cost: FromFloat(1)})
		}()
	}
	wg.Wait()
	if m := sink.Snapshot(); m.Nodes != 100 {
		t.Errorf("want 100 nodes counted under concurrency, got %d", m.Nodes)
	}
}

// ------------------------------------------------- wired into a real run

func TestAggregateSinkWiredIntoExecutor(t *testing.T) {
	// End to end: the sink counts every node of an actual run, matching the record.
	prov := &fakeProvider{cost: FromFloat(1)}
	sink := NewAggregateSink()
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, _ := NewLedger(caps, Scope{})
	e := &Executor{
		Planner:  StaticPlanner{P: fanoutPlan("a", "b", "c")},
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Sink:     sink,
		Now:      now,
		MaxDepth: 1,
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Snapshot().Nodes; got != len(res.Outcomes) {
		t.Errorf("sink node count %d must match record outcomes %d", got, len(res.Outcomes))
	}
}
