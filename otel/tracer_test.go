package otel

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	quarry "github.com/scttfrdmn/quarry"
)

// These tests use an in-memory exporter: no collector, no network, no env vars, so
// `go test ./...` stays offline (the same discipline the core keeps). They encode
// two things as invariants:
//
//   - THE SPAN TREE IS THE DECOMPOSITION TREE (§9). That is the whole claim of this
//     package, so parentage is asserted structurally, not by eyeballing a trace.
//   - The three-state verification verdict survives onto the span. Collapsing
//     unchecked into failed is the specific loss §8 exists to prevent, and a bool
//     attribute would have done exactly that.

func harness() (*Tracer, *tracetest.InMemoryExporter, *sdktrace.TracerProvider) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return NewTracer(tp), exp, tp
}

func attrOf(s tracetest.SpanStub, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func mustAttr(t *testing.T, s tracetest.SpanStub, key attribute.Key) attribute.Value {
	t.Helper()
	v, ok := attrOf(s, key)
	if !ok {
		t.Fatalf("span %q missing attribute %q", s.Name, key)
	}
	return v
}

// tree is a two-level run: a root that reduced two leaves, one verified, one not.
func tree() []quarry.NodeOutcome {
	yes := true
	return []quarry.NodeOutcome{
		{NodeID: "n0", Depth: 0, Content: "merged", Cost: quarry.FromFloat(1),
			Children: []string{"n0.0", "n0.1"}},
		{NodeID: "n0.0", Depth: 1, Content: "a", Cost: quarry.FromFloat(2),
			Model: "claude", ModelVersion: "claude-v1", Verified: &yes},
		{NodeID: "n0.1", Depth: 1, Content: "b", Cost: quarry.FromFloat(3),
			Model: "claude", ModelVersion: "claude-v1", BaseCase: quarry.BaseMaxDepth},
	}
}

func TestSpanTreeIsTheDecompositionTree(t *testing.T) {
	// The core claim of §9: a child node's span is parented to its parent node's
	// span, and all spans share one trace. Asserted on span IDs, not names.
	tr, exp, _ := harness()
	for _, o := range tree() {
		tr.Node(o)
	}
	tr.Run("hash123", nil)

	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("want one span per node, got %d", len(spans))
	}
	byNode := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		v, ok := attrOf(s, AttrNodeID)
		if !ok {
			t.Fatalf("span %q has no node id to join back to the record", s.Name)
		}
		byNode[v.AsString()] = s
	}
	root, ok := byNode["n0"]
	if !ok {
		t.Fatal("no span for the root node")
	}
	if root.Parent.IsValid() {
		t.Error("the root node's span must have no parent")
	}
	for _, kid := range []string{"n0.0", "n0.1"} {
		s, ok := byNode[kid]
		if !ok {
			t.Fatalf("no span for %s", kid)
		}
		if s.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Errorf("%s must be parented to the root's span, got %v", kid, s.Parent.SpanID())
		}
		if s.SpanContext.TraceID() != root.SpanContext.TraceID() {
			t.Errorf("%s must share the root's trace, got %v", kid, s.SpanContext.TraceID())
		}
	}
}

func TestDeepNestingFollowsDottedNodeIDs(t *testing.T) {
	// Three levels: parentage is derived from the executor's dotted childID
	// convention, so a grandchild must nest under its own parent, not the root.
	tr, exp, _ := harness()
	for _, o := range []quarry.NodeOutcome{
		{NodeID: "n0", Depth: 0, Children: []string{"n0.0"}},
		{NodeID: "n0.0", Depth: 1, Children: []string{"n0.0.0"}},
		{NodeID: "n0.0.0", Depth: 2},
	} {
		tr.Node(o)
	}
	tr.Run("h", nil)

	byNode := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byNode[mustAttr(t, s, AttrNodeID).AsString()] = s
	}
	if got := byNode["n0.0.0"].Parent.SpanID(); got != byNode["n0.0"].SpanContext.SpanID() {
		t.Errorf("grandchild must nest under n0.0, got parent %v", got)
	}
	if v := mustAttr(t, byNode["n0.0.0"], AttrParentID); v.AsString() != "n0.0" {
		t.Errorf("parent id attribute must be n0.0, got %q", v.AsString())
	}
}

func TestVerificationVerdictHasThreeStates(t *testing.T) {
	// Unchecked and checked-and-failed are DIFFERENT facts (§8). A bool attribute
	// cannot express that, which is why the verdict is a string enum.
	yes, no := true, false
	cases := []struct {
		verified *bool
		want     string
	}{
		{&yes, VerdictPassed},
		{&no, VerdictFailed},
		{nil, VerdictNotAssessed},
	}
	for _, c := range cases {
		tr, exp, _ := harness()
		tr.Node(quarry.NodeOutcome{NodeID: "n0", Verified: c.verified})
		tr.Run("h", nil)
		got := mustAttr(t, exp.GetSpans()[0], AttrVerified).AsString()
		if got != c.want {
			t.Errorf("verdict: want %q, got %q", c.want, got)
		}
	}
}

func TestGapIsAnErrorStatusButFailedVerificationIsNot(t *testing.T) {
	// A gap did not complete — that is what OTel's error status means, and generic
	// tooling reads it. A failed verification COMPLETED and returned a verdict: the
	// check worked, the answer was bad. Marking it an error would conflate a working
	// verifier with a broken run.
	no := false
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n0", Gap: true})
	tr.Node(quarry.NodeOutcome{NodeID: "n1", Verified: &no})
	tr.Run("h", nil)

	byNode := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byNode[mustAttr(t, s, AttrNodeID).AsString()] = s
	}
	if byNode["n0"].Status.Code != codes.Error {
		t.Errorf("a truncated node must be an error span, got %v", byNode["n0"].Status.Code)
	}
	if byNode["n1"].Status.Code == codes.Error {
		t.Error("a failed verification is a successful assessment, not a span error")
	}
}

func TestCostIsIntegralMicroUnitsNotFloat(t *testing.T) {
	// Go rule 3: the ledger is int64 micro-units. A float on the span would sit
	// beside the receipt's differently-rounded number and invite disagreement.
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n0", Cost: quarry.FromFloat(1.5)})
	tr.Run("h", nil)
	v := mustAttr(t, exp.GetSpans()[0], AttrCostMicro)
	if v.Type() != attribute.INT64 {
		t.Fatalf("cost must be an int64 attribute, got %v", v.Type())
	}
	if v.AsInt64() != 1_500_000 {
		t.Errorf("want 1500000 micro-units, got %d", v.AsInt64())
	}
}

func TestUnlimitedCostEmitsNoCostAttribute(t *testing.T) {
	// An unpriced node has no cost, which is different from costing zero. Emitting
	// -1 (the Unlimited sentinel) would be read as a real negative cost.
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n0", Cost: quarry.Unlimited})
	tr.Run("h", nil)
	if _, ok := attrOf(exp.GetSpans()[0], AttrCostMicro); ok {
		t.Error("an unpriced node must omit the cost attribute, not emit the sentinel")
	}
}

func TestGenAISemconvKeysAreUsedForModelAndTokens(t *testing.T) {
	// agenkit#711: follow OTel GenAI semconv rather than a private namespace, so
	// AgentCore/CloudWatch tooling works with no translation layer. These exact
	// spec key names are the contract.
	attrs := NodeAttributes(quarry.NodeOutcome{NodeID: "n0", Model: "claude", ModelVersion: "claude-v1"})
	want := map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.request.model":  "claude-v1",
		"gen_ai.response.model": "claude-v1",
	}
	got := map[string]string{}
	for _, kv := range attrs {
		// String(), not the deprecated Emit(). They agree for every type this exporter
		// emits — checked against attribute/value.go, where the two switches differ only
		// in how they format SLICE kinds, and these attributes are strings and ints.
		got[string(kv.Key)] = kv.Value.String()
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("semconv key %s: want %q, got %q", k, v, got[k])
		}
	}

	// The token split uses the spec's usage keys, read straight off the outcome.
	tokens := NodeAttributes(quarry.NodeOutcome{
		NodeID: "n0", ModelVersion: "m1", HaloTokens: 100, GeneratedTokens: 25,
	})
	tk := map[string]attribute.Value{}
	for _, kv := range tokens {
		tk[string(kv.Key)] = kv.Value
	}
	if v, ok := tk["gen_ai.usage.input_tokens"]; !ok || v.AsInt64() != 100 {
		t.Errorf("gen_ai.usage.input_tokens must carry halo tokens, got %v", v)
	}
	if v, ok := tk["gen_ai.usage.output_tokens"]; !ok || v.AsInt64() != 25 {
		t.Errorf("gen_ai.usage.output_tokens must carry generated tokens, got %v", v)
	}
	// Surface-to-volume makes P1 observable (§8.2): 100 halo / 25 generated = 4.0.
	if v, ok := tk[string(AttrSurfaceToVolume)]; !ok || v.AsFloat64() != 4.0 {
		t.Errorf("surface/volume ratio must be 4.0, got %v", v)
	}
}

func TestEveryCustomKeyIsNamespaced(t *testing.T) {
	// A custom key is only acceptable when semconv has none, and it must be
	// namespaced so it cannot collide with a future spec key. Anything outside
	// gen_ai.* must be under quarry.*.
	yes := true
	attrs := NodeAttributes(quarry.NodeOutcome{
		NodeID: "n0.1", Depth: 1, Cost: quarry.FromFloat(1), Verified: &yes,
		BaseCase: quarry.BaseNoVerifier, ModelVersion: "m1", Retries: 2,
	})
	for _, kv := range attrs {
		k := string(kv.Key)
		if !hasPrefix(k, "gen_ai.") && !hasPrefix(k, "quarry.") {
			t.Errorf("attribute %q is neither semconv nor namespaced under quarry.", k)
		}
	}
}

func TestBaseCaseIsRecordedForDiagnosis(t *testing.T) {
	// Why recursion stopped is the most diagnostic thing quarry knows (§2): it
	// distinguishes P2 (no verifier) from a budget floor from the depth backstop.
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n0", BaseCase: quarry.BaseNoVerifier})
	tr.Run("h", nil)
	if v := mustAttr(t, exp.GetSpans()[0], AttrBaseCase); v.AsString() != string(quarry.BaseNoVerifier) {
		t.Errorf("want base case %q, got %q", quarry.BaseNoVerifier, v.AsString())
	}
}

func TestRunMetricsLandOnTheRootSpanOnly(t *testing.T) {
	// Run-level §8.2 numbers belong on the root: repeating them on every span would
	// multiply-count them in any aggregation.
	tr, exp, _ := harness()
	for _, o := range tree() {
		tr.Node(o)
	}
	rec := quarry.RunRecord{Outcomes: tree(), BoundBy: quarry.DenomSpend, Unverified: []string{"n0", "n0.1"}}
	tr.Run("hash123", quarry.RunMetrics(rec))

	for _, s := range exp.GetSpans() {
		node := mustAttr(t, s, AttrNodeID).AsString()
		_, hasMetric := attrOf(s, attribute.Key("quarry.run.total_cost"))
		if node == "n0" && !hasMetric {
			t.Error("the root span must carry the run metrics")
		}
		if node != "n0" && hasMetric {
			t.Errorf("%s must not repeat run-level metrics", node)
		}
		// Every span joins back to the citable record (P8).
		if v := mustAttr(t, s, AttrRunID); v.AsString() != "hash123" {
			t.Errorf("%s must carry the record hash, got %q", node, v.AsString())
		}
	}
}

func TestProvenanceOmitsUnmeasuredStability(t *testing.T) {
	// Stability needs replicates (P7). A 0.0 reads as "measured and unstable" — a
	// different and stronger claim than "not measured", so it is omitted.
	unknown := ProvenanceAttributes(quarry.Provenance{Unverified: 2, StabilityKnown: false})
	for _, kv := range unknown {
		if kv.Key == AttrStability {
			t.Error("unmeasured stability must be omitted, not emitted as 0.0")
		}
	}
	known := ProvenanceAttributes(quarry.Provenance{Stability: 0.5, StabilityKnown: true})
	var found bool
	for _, kv := range known {
		if kv.Key == AttrStability && kv.Value.AsFloat64() == 0.5 {
			found = true
		}
	}
	if !found {
		t.Error("a measured stability must appear on the span")
	}
}

func TestSpanNameDoesNotEmbedNodeIDForModelCalls(t *testing.T) {
	// A leaf that called a model is named "chat {model}" per GenAI semconv, so spans
	// aggregate across runs. Node IDs are high-cardinality and belong in attributes.
	if got := SpanName(quarry.NodeOutcome{NodeID: "n0.3", ModelVersion: "claude-v1"}); got != "chat claude-v1" {
		t.Errorf("model-call span name: got %q", got)
	}
	if got := SpanName(quarry.NodeOutcome{NodeID: "n0", Children: []string{"n0.0"}}); got != "reduce n0" {
		t.Errorf("internal node span name: got %q", got)
	}
	if got := SpanName(quarry.NodeOutcome{NodeID: "n0.1", CacheHit: true}); got != "cache n0.1" {
		t.Errorf("cache hit span name: got %q", got)
	}
}

func TestCacheHitIsAlwaysRecorded(t *testing.T) {
	// A hit is both a saving and a replication HAZARD (P7): a served answer is not an
	// independent sample, so it must always be visible — including as an explicit
	// false, so its absence can never be mistaken for a hit.
	for _, hit := range []bool{true, false} {
		attrs := NodeAttributes(quarry.NodeOutcome{NodeID: "n0", CacheHit: hit})
		var found bool
		for _, kv := range attrs {
			if kv.Key == AttrCacheHit {
				found = true
				if kv.Value.AsBool() != hit {
					t.Errorf("cache hit: want %v, got %v", hit, kv.Value.AsBool())
				}
			}
		}
		if !found {
			t.Error("the cache flag must always be present")
		}
	}
}

func TestOrphanNodeStillGetsASpan(t *testing.T) {
	// A node whose parent was never recorded must not vanish: a silently dropped
	// span loses a real node, which is worse than an unexpectedly-rooted one.
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n5.7", Depth: 2}) // no n5 recorded
	tr.Run("h", nil)
	if len(exp.GetSpans()) != 1 {
		t.Fatalf("the orphan must still be exported, got %d spans", len(exp.GetSpans()))
	}
	if exp.GetSpans()[0].Parent.IsValid() {
		t.Error("an orphan anchors its own tree rather than inventing a parent")
	}
}

func TestNodeIsConcurrencySafe(t *testing.T) {
	// The TelemetrySink contract: siblings complete on separate goroutines and emit
	// as they finish. Run with -race.
	tr, exp, _ := harness()
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func(i int) {
			tr.Node(quarry.NodeOutcome{NodeID: "n0." + string(rune('a'+i)), Depth: 1})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 16; i++ {
		<-done
	}
	tr.Run("h", nil)
	if len(exp.GetSpans()) != 16 {
		t.Errorf("want 16 spans, got %d", len(exp.GetSpans()))
	}
}

func TestRunAfterRunExportsNothingTwice(t *testing.T) {
	// Run drains the buffer and closes it, so a double Run cannot duplicate a run's
	// spans and a late Node after Run cannot leak into a fresh trace.
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n0"})
	tr.Run("h", nil)
	tr.Node(quarry.NodeOutcome{NodeID: "n1"})
	tr.Run("h", nil)
	if len(exp.GetSpans()) != 1 {
		t.Errorf("a second Run must export nothing, got %d spans", len(exp.GetSpans()))
	}
}

func TestTracerSatisfiesTheSinkSeamAndNeedsNoCollector(t *testing.T) {
	// The point of the seam: the core imports no SDK, and a test needs no collector
	// or env var.
	tr, _, tp := harness()
	var sink quarry.TelemetrySink = tr
	sink.Node(quarry.NodeOutcome{NodeID: "n0"})
	sink.Run("h", map[string]any{"nodes": 1})
	var _ trace.TracerProvider = tp
}

func TestNodeWithoutRunExportsNothing(t *testing.T) {
	// Pins the footgun documented on Tracer: Executor.Sink alone is NOT enough,
	// because the executor never calls Sink.Run. Node only buffers, so a caller who
	// sets the field and forgets the Run call gets zero spans AND NO ERROR — total
	// silent loss of the trace, unlike AggregateSink where node data still
	// accumulates. Asserted so the asymmetry between the two sinks stays known.
	tr, exp, _ := harness()
	for _, o := range tree() {
		tr.Node(o)
	}
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("Node must only buffer; got %d spans before Run", n)
	}
	tr.Run("h", nil)
	if n := len(exp.GetSpans()); n != 3 {
		t.Errorf("Run is what exports; want 3 spans, got %d", n)
	}
}

func TestRecordedTimingBecomesRealSpanTimestamps(t *testing.T) {
	// The span carries the node's OWN measured bracket, not when the trace happened
	// to be assembled — which is what makes a post-hoc trace still usable as a flame
	// graph. Before NodeOutcome recorded timing, this was impossible and the package
	// documented durations as meaningless.
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n0", Timing: quarry.NodeTiming{
		StartedAt: start, EndedAt: start.Add(250 * time.Millisecond),
	}})
	tr.Run("h", nil)

	s := exp.GetSpans()[0]
	if !s.StartTime.Equal(start) {
		t.Errorf("span must start at the recorded instant: got %v want %v", s.StartTime, start)
	}
	if d := s.EndTime.Sub(s.StartTime); d != 250*time.Millisecond {
		t.Errorf("span duration must be the measured one: got %v", d)
	}
	if v := mustAttr(t, s, AttrTimingMeasured); !v.AsBool() {
		t.Error("a timed node must be marked measured")
	}
}

func TestUnmeasuredTimingIsLabelledNotDisguised(t *testing.T) {
	// An untimed node still gets a span, but its duration is an artifact of trace
	// assembly. quarry.timing.measured=false is what lets a consumer tell the two
	// apart — without it, an assembly artifact is indistinguishable from a real
	// sub-millisecond latency, which is the kind of number people build dashboards
	// on.
	tr, exp, _ := harness()
	tr.Node(quarry.NodeOutcome{NodeID: "n0"}) // no Timing
	tr.Run("h", nil)

	if v := mustAttr(t, exp.GetSpans()[0], AttrTimingMeasured); v.AsBool() {
		t.Error("an untimed node must be marked unmeasured")
	}
}

func TestTokenSplitAndRatioComeFromTheOutcomeAlone(t *testing.T) {
	// The whole point of moving tokens onto NodeOutcome: a sink sees the split and
	// can emit surface-to-volume without holding the Sample. This was the one §8.2
	// number the observer could not previously reach.
	attrs := NodeAttributes(quarry.NodeOutcome{
		NodeID: "n0.0", ModelVersion: "m1", HaloTokens: 90, GeneratedTokens: 30,
	})
	got := map[string]attribute.Value{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value
	}
	if v, ok := got["gen_ai.usage.input_tokens"]; !ok || v.AsInt64() != 90 {
		t.Errorf("input tokens must come off the outcome: got %v", v)
	}
	if v, ok := got["gen_ai.usage.output_tokens"]; !ok || v.AsInt64() != 30 {
		t.Errorf("output tokens must come off the outcome: got %v", v)
	}
	if v, ok := got[string(AttrSurfaceToVolume)]; !ok || v.AsFloat64() != 3.0 {
		t.Errorf("ratio must be 3.0, got %v", v)
	}
}

func TestNoTokensMeansNoUsageAttributes(t *testing.T) {
	// An internal node or a gap made no model call. semconv's usage keys would read
	// a zero as a MEASURED zero — "this call used no tokens" — so absence must stay
	// absence, exactly as with the cost sentinel.
	attrs := NodeAttributes(quarry.NodeOutcome{NodeID: "n0", Children: []string{"n0.0"}})
	for _, kv := range attrs {
		switch string(kv.Key) {
		case "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", string(AttrSurfaceToVolume):
			t.Errorf("a node that called no model must omit %s", kv.Key)
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
