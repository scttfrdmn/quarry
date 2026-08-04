package quarry

import "sync"

// Telemetry aggregation (build step 10, §8.2). The instrumentation is already
// required by P8 — run records exist for provenance — so this is a SECOND READER
// of artifacts kept anyway, not new instrumentation. On from run one.
//
// This file is the pure, in-process aggregator: it implements TelemetrySink,
// reads node outcomes as they complete, and answers with a Metrics snapshot.
// Nothing here reads the clock or dials a network (Go rule 4). The OTel span
// exporter — which does both — now exists as otel.Tracer, a SEPARATE
// implementation of the same seam rather than a wrapper: it follows OTel GenAI
// semconv (see docs/integration-requirements.md §4) and projects the tree, while
// this type projects scalars. Two readers of one seam, neither privileged.
//
// THE GOODHART GUARDRAIL IS STRUCTURAL (§8.2). "No efficiency metric without a
// quality denominator." Cost-per-run is trivially gamed by doing less — shallower
// trees, fewer verifications, more cache hits all improve it while degrading what
// the system exists to protect. So this type does NOT expose cost-per-run. The
// only cost ratio it will compute is cost per VERIFIED claim (RunRecord already
// has it), and even the raw counters below are quality-labelled (verified rate,
// degradation count) rather than efficiency-labelled.

// Metrics is a point-in-time snapshot of aggregated node telemetry (§8.2). Raw
// counts and quality signals only — no efficiency ratio without a denominator.
type Metrics struct {
	Nodes        int   // node outcomes seen
	Leaves       int   // nodes that solved directly (no children)
	CacheHits    int   // served from cache, spent nothing (§6)
	Verified     int   // a verifier ran AND passed
	VerifyFailed int   // a verifier ran AND failed — checked-and-failed, not unchecked
	Unverified   int   // no verifier assessed the node (Verified == nil), non-gap
	Gaps         int   // truncated / unreturnable — degradation events (§3.1)
	Retries      int   // total re-solves across all nodes
	TotalCost    Units // summed node cost

	// HaloTokens and GeneratedTokens sum the token split across nodes that called a
	// model, which makes the aggregate surface-to-volume computable (P1, §8.2).
	// Before the split lived on NodeOutcome, a sink saw cost but could not see this
	// — the observer could not reach the one ratio P1 is judged on.
	HaloTokens      int
	GeneratedTokens int

	// TimedNodes counts nodes that carried a real wall-clock measurement, so a
	// consumer can tell "fast" from "never measured" (§9). Not a duration sum:
	// parent brackets contain their children's, so summing would multiply-count.
	TimedNodes int

	// BaseCases counts why leaves stopped recursing (§2) — the distribution of
	// terminators tells you whether P2 (no verifier) or budget is the real floor.
	BaseCases map[BaseCase]int
}

// SurfaceToVolume is the aggregate halo-to-generated ratio (P1, §8.2) — high means
// the tree spent its money re-sending context rather than producing anything, which
// is the quantitative form of "this decomposition was not worth making".
//
// Safe under the Goodhart guardrail even though it is a ratio, because BOTH terms
// are work the system actually did: you cannot improve it by verifying less or
// recursing shallower. It is diagnostic about SHAPE, not a cost-efficiency figure,
// and it is never divided into spend. ok=false when nothing was generated.
func (m Metrics) SurfaceToVolume() (float64, bool) {
	if m.GeneratedTokens == 0 {
		return 0, false
	}
	return float64(m.HaloTokens) / float64(m.GeneratedTokens), true
}

// VerifiedRate is the share of assessed nodes that passed. Quality, not
// efficiency: it has verification in the numerator, so it cannot be gamed by
// doing less — skipping verification moves nodes to Unverified, not to Verified.
// ok=false when nothing was assessed. [§8.2 guardrail]
func (m Metrics) VerifiedRate() (float64, bool) {
	assessed := m.Verified + m.VerifyFailed
	if assessed == 0 {
		return 0, false
	}
	return float64(m.Verified) / float64(assessed), true
}

// CacheHitRate is the share of nodes served from cache. Deliberately NOT labelled
// an efficiency metric and never divided into cost: a high hit rate is a
// replication HAZARD as much as a saving (P7 — a served answer is not an
// independent sample), so it is reported as a raw rate for diagnosis, never as a
// thing to maximize. ok=false when no nodes were seen.
func (m Metrics) CacheHitRate() (float64, bool) {
	if m.Nodes == 0 {
		return 0, false
	}
	return float64(m.CacheHits) / float64(m.Nodes), true
}

// AggregateSink is the reference TelemetrySink: a concurrency-safe accumulator.
//
// Node is called from many goroutines at once — sibling nodes complete on
// separate goroutines and emit as they finish (the TelemetrySink contract) — so
// every mutation holds the lock. The corpus only accumulates.
type AggregateSink struct {
	mu sync.Mutex
	m  Metrics

	// runs records per-run metric maps as they are finalized, for Run().
	runMetrics map[string]map[string]any
}

// NewAggregateSink builds an empty sink.
func NewAggregateSink() *AggregateSink {
	return &AggregateSink{
		m:          Metrics{BaseCases: map[BaseCase]int{}},
		runMetrics: map[string]map[string]any{},
	}
}

// Node folds one completed node into the running totals (§8.2). Classification
// mirrors the executor's own semantics so the counts agree with the record:
//   - a cache hit is counted as a hit and nothing else spend-wise (it cost 0);
//   - Verified==nil AND not a gap is genuinely unverified (unchecked), distinct
//     from checked-and-failed — the same distinction the receipt draws (§8);
//   - a gap is a degradation event (only time is a gap, per the standing ruling).
func (s *AggregateSink) Node(o NodeOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.Nodes++
	s.m.TotalCost += o.Cost
	s.m.Retries += o.Retries
	s.m.HaloTokens += o.HaloTokens
	s.m.GeneratedTokens += o.GeneratedTokens
	if _, ok := o.Timing.Duration(); ok {
		s.m.TimedNodes++
	}
	if len(o.Children) == 0 {
		s.m.Leaves++
	}
	if o.CacheHit {
		s.m.CacheHits++
	}
	if o.BaseCase != "" {
		s.m.BaseCases[o.BaseCase]++
	}
	switch {
	case o.Gap:
		s.m.Gaps++
	case o.Verified == nil:
		s.m.Unverified++
	case *o.Verified:
		s.m.Verified++
	default:
		s.m.VerifyFailed++
	}
}

// Run records a finalized run's metric map (§8.2). The executor/record layer
// computes the run-level numbers — cost per verified claim, stability rate,
// which cap bound — because those need the whole RunRecord, not a single node;
// this sink just retains them keyed by run for later readout.
func (s *AggregateSink) Run(recordID string, metrics map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runMetrics[recordID] = metrics
}

// Snapshot returns a copy of the accumulated metrics, safe to read after. The
// BaseCases map is copied so the caller cannot mutate the sink's state.
func (s *AggregateSink) Snapshot() Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.m
	out.BaseCases = make(map[BaseCase]int, len(s.m.BaseCases))
	for k, v := range s.m.BaseCases {
		out.BaseCases[k] = v
	}
	return out
}

// RunMetrics returns the metric map recorded for a run, if any.
func (s *AggregateSink) RunMetrics(recordID string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.runMetrics[recordID]
	return m, ok
}

var _ TelemetrySink = (*AggregateSink)(nil)

// RunMetrics assembles the run-level §8.2 numbers from a completed record. It is
// the bridge from AggregateSink.Node (per-node) to AggregateSink.Run (per-run):
// call it after NewRunRecord and hand the result to the sink's Run method.
//
// Every efficiency figure here carries a quality denominator or is omitted (§8.2
// guardrail). Cost per verified claim is present only when there ARE verified
// claims; a bare cost-per-run is deliberately never emitted.
func RunMetrics(r RunRecord) map[string]any {
	m := map[string]any{
		"total_cost":  int64(r.TotalCost()),
		"nodes":       len(r.Outcomes),
		"gaps":        len(r.Gaps()),
		"unverified":  len(r.Unverified),
		"adversarial": len(r.Adversarial),
		"bound_by":    string(r.BoundBy), // which cap bit — money vs time (§8.2)
	}
	if cpvc, ok := r.CostPerVerifiedClaim(); ok {
		m["cost_per_verified_claim"] = int64(cpvc)
	}
	broke := 0
	for _, f := range r.Adversarial {
		if f.Broke {
			broke++
		}
	}
	m["adversarial_broke"] = broke // claims an adversary refuted — refine signal (§5, §8.1)
	return m
}
