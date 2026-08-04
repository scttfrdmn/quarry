// Package otel emits a quarry run as an OpenTelemetry trace, where the SPAN TREE
// IS THE DECOMPOSITION TREE (quarry docs/design.md §9): one span per node, a child
// node's span parented to its parent node's span. The same trace then serves a
// developer in Jaeger and a researcher-facing view, without a bespoke viewer.
//
// It lives in a SUBPACKAGE, not the core, for the reason core purity exists at all
// (Go rule 4): package quarry imports no SDK, dials no network, and calls no
// time.Now(). An exporter must do the last two, so it sits behind the
// TelemetrySink seam here — the core stays testable with no collector, and a run
// with no tracer wired behaves identically.
//
// # Why GenAI semconv and not agenkit's namespace
//
// agenkit owns OTel in this ecosystem, so the natural move was to copy its
// convention. Per agenkit#711 the maintainer's answer was the opposite: agenkit's
// tracing is an ad-hoc namespace (`agent.{name}.process`, tracer
// `agenkit.observability`) with ZERO of the seven attributes quarry needs — no
// model version, cost, token split, cache flag, verifier verdict or retry count
// exists as a span attribute anywhere in its five language implementations. The
// explicit guidance was "don't wait on us — emit raw OTel now," following OTel
// GenAI semconv, because that is where agenkit intends to converge (agenkit#715)
// and it makes AgentCore/CloudWatch tooling work with no translation layer.
//
// So: standard `gen_ai.*` keys wherever one exists, and a documented `quarry.*`
// namespace ONLY for the concepts semconv has no key for. Every such key is listed
// in this file's constants with a note on why semconv could not express it — an
// unlisted custom key is a bug, because a private key is invisible to generic
// tooling and silently loses the thing it was meant to record.
//
// # Determinism is NOT claimed here
//
// A trace carries wall-clock timestamps and random span IDs, so it is not
// byte-reproducible and MUST NOT be treated as the citable artifact. The
// RunRecord is that (P8); a trace is a second, lossy view of the same run, exactly
// like the agate RunEvent projection. Nothing in quarry may read a decision back
// out of a span.
package otel

import (
	"context"
	"fmt"
	"os"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	quarry "github.com/scttfrdmn/quarry"
)

// TracerName identifies the instrumentation scope. Deliberately NOT
// "agenkit.observability": these spans carry a different attribute set under a
// different convention, and borrowing agenkit's scope name would make two
// incompatible schemas indistinguishable to a consumer (agenkit#711).
const TracerName = "github.com/scttfrdmn/quarry"

// The quarry-specific attribute keys. Each exists because GenAI semconv has no
// equivalent — the justification is on the line, so a reviewer can challenge any
// one of them and a future semconv release can retire it.
const (
	// AttrNodeID / AttrParentID / AttrDepth: semconv has no notion of a node in a
	// decomposition tree. Span parentage already encodes the shape, but the IDs are
	// recorded too so a span can be joined back to the RunRecord — the trace is a
	// view, and it must be traceable to the artifact (P8).
	AttrNodeID   = attribute.Key("quarry.node.id")
	AttrParentID = attribute.Key("quarry.node.parent_id")
	AttrDepth    = attribute.Key("quarry.node.depth")

	// AttrCostMicro: cost in int64 MICRO-UNITS, not float dollars. semconv has no
	// cost attribute at all. Micro-units keep the span consistent with the ledger
	// (Go rule 3) — emitting a float here would put a differently-rounded number
	// next to the receipt's and invite the two to disagree.
	AttrCostMicro = attribute.Key("quarry.cost.micro_units")

	// AttrCacheHit: semconv has no cache concept for GenAI. Recorded because a hit
	// is BOTH a saving and a replication hazard (P7): a served answer is not an
	// independent sample, so a trace that hides hits overstates the evidence.
	AttrCacheHit = attribute.Key("quarry.cache.hit")

	// AttrVerified: the three-state verdict, and the reason a bare bool would not
	// do — "not_assessed" is a real state distinct from "failed" (§8). agenkit has
	// no cross-language verdict enum to align to (agenkit#711), so quarry defines
	// one and reconciliation is deferred.
	AttrVerified = attribute.Key("quarry.verify.verdict")

	// AttrRetries: re-solves after a failed verification. Distinct from a transport
	// retry, which is why a generic http.retry_count would be the wrong key: this
	// counts QUALITY retries and each one consumed budget (§3).
	AttrRetries = attribute.Key("quarry.verify.retries")

	// AttrBaseCase: why recursion stopped (§2). The single most diagnostic
	// attribute quarry has — it tells you whether P2 (no verifier), the budget
	// floor, or the depth backstop is the real terminator, which is the difference
	// between a tuned system and one hitting a wall it cannot see.
	AttrBaseCase = attribute.Key("quarry.base_case")

	// AttrGap: truncated or unreturnable (§3.1). Named on the span for the same
	// reason it is named in the record: degradation is disclosed, never silent.
	// Only time produces a gap — the standing ruling.
	AttrGap = attribute.Key("quarry.gap")

	// AttrSurfaceToVolume: halo tokens over generated (§8.2), P1 made observable. A
	// derived ratio, so semconv would not carry it; recorded because a high value is
	// evidence the split was not worth making.
	AttrSurfaceToVolume = attribute.Key("quarry.surface_to_volume")

	// AttrClaims: claims extracted from the node's content (§7).
	AttrClaims = attribute.Key("quarry.claims.count")

	// AttrTimingMeasured: whether this span's start/end are REAL measurements or
	// just when the trace was built. semconv has no such key because a normal
	// instrumentation always measures — quarry's does not, since timing needs an
	// injected clock (Go rule 4) and is absent without one. Emitted ALWAYS, and
	// false is the load-bearing value: a duration a consumer cannot distinguish
	// from an artifact of trace assembly is worse than no duration at all.
	AttrTimingMeasured = attribute.Key("quarry.timing.measured")

	// Run-level keys, set on the root span when the record is finalized.
	AttrRunID      = attribute.Key("quarry.run.id")
	AttrBoundBy    = attribute.Key("quarry.run.bound_by")
	AttrUnverified = attribute.Key("quarry.run.unverified")
	AttrStability  = attribute.Key("quarry.run.stability")
	AttrAdversary  = attribute.Key("quarry.run.adversarial_findings")
)

// The verdict vocabulary for AttrVerified. Three values, because unchecked and
// checked-and-failed are different facts and collapsing them is the specific
// failure §8 exists to prevent.
const (
	VerdictPassed      = "passed"
	VerdictFailed      = "failed"
	VerdictNotAssessed = "not_assessed"
)

// Tracer emits spans for a quarry run. It satisfies quarry.TelemetrySink, so the
// core learns nothing about OTel.
//
// # Wiring it takes TWO steps, and one field alone exports NOTHING
//
// Setting Executor.Sink is necessary but NOT sufficient. The executor calls
// Sink.Node per node, but it never calls Sink.Run — that is the caller's job,
// after NewRunRecord (see quarry.RunMetrics). For quarry's AggregateSink that
// omission is benign, because node data still accumulates and Snapshot still
// works. For a Tracer it is TOTAL SILENT FAILURE: Node only buffers, so a run
// wired with Sink but no Run() call produces zero spans and no error.
//
//	tracer := otel.NewTracer(tp)
//	e := quarry.Executor{Sink: tracer /* ... */}
//	res, err := e.Run(ctx, root, ledger)
//	rec := quarry.NewRunRecord(...)
//	tracer.Run(rec.RunID, quarry.RunMetrics(rec)) // <- WITHOUT THIS, NO SPANS
//
// This is pinned by TestNodeWithoutRunExportsNothing so the footgun is a tested,
// documented property rather than a surprise. It is not defended against in code:
// a Tracer cannot know a run has ended, and flushing on a timer would need a
// clock, which is exactly what this package's host forbids.
//
// # The problem this type actually solves
//
// A TelemetrySink is called when a node COMPLETES, and children complete before
// their parent. So by the time a parent's span could be created, its children have
// already finished — the natural span nesting is unavailable at emission time.
//
// Rather than fake the linkage, Tracer BUFFERS outcomes and builds the whole span
// tree at Run(), when the shape is known. Spans are created parent-before-child in
// depth order with the parent's context, so the exported tree really is the
// decomposition tree. The cost is that nothing exports until the run ends: this is
// a post-hoc trace, not a live one. That is the honest trade — a live stream would
// require the executor to hand spans down through node(), which means the core
// importing OTel, which Go rule 4 forbids.
//
// Being post-hoc does NOT mean the timestamps are fake: a node's recorded Timing is
// replayed onto its span, so durations are real whenever a clock was injected. See
// Run.
type Tracer struct {
	tp     trace.TracerProvider
	mu     sync.Mutex
	nodes  []quarry.NodeOutcome
	closed bool
}

// NewTracer wraps a TracerProvider. Pass the one from Setup, or any provider
// (including a test provider with an in-memory exporter — that is how this package
// is tested with no collector).
func NewTracer(tp trace.TracerProvider) *Tracer { return &Tracer{tp: tp} }

// Node buffers a completed node. Concurrency-safe: siblings complete on separate
// goroutines and emit as they finish, which the TelemetrySink contract requires.
func (t *Tracer) Node(o quarry.NodeOutcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.nodes = append(t.nodes, o)
}

// Run builds and exports the span tree for a finished run.
//
// metrics is the run-level map from quarry.RunMetrics; its keys are attached to
// the root span with a quarry.run. prefix so the run's §8.2 numbers travel with
// the trace. recordID is the content-hash RunID (P8) — the join key from any span
// back to the citable artifact.
//
// TIMESTAMPS: spans are built after the fact, but they carry the node's REAL
// recorded start/end when NodeOutcome.Timing holds a measurement — so a post-hoc
// trace still shows true per-node latency, and a parent's span really does span its
// subtree's. Timing is recorded only when a clock is injected (Executor.Clock),
// because the core never calls time.Now() itself (Go rule 4).
//
// When timing is ABSENT the span falls back to build-time timestamps, whose
// duration is an artifact of trace assembly rather than latency. That case is
// labelled with quarry.timing.measured=false rather than left to be guessed: a
// consumer who cannot distinguish a measured duration from an assembly artifact is
// worse off than one given no duration at all.
func (t *Tracer) Run(recordID string, metrics map[string]any) {
	t.mu.Lock()
	nodes := t.nodes
	t.nodes = nil
	t.closed = true
	t.mu.Unlock()

	if len(nodes) == 0 {
		return
	}

	tr := t.tp.Tracer(TracerName)
	byID := make(map[string]quarry.NodeOutcome, len(nodes))
	children := make(map[string][]string, len(nodes))
	var roots []string
	for _, o := range nodes {
		byID[o.NodeID] = o
	}
	for _, o := range nodes {
		if p, ok := parentOf(o.NodeID); ok {
			if _, exists := byID[p]; exists {
				children[p] = append(children[p], o.NodeID)
				continue
			}
		}
		// No parent in this run's outcome set: a root, or an orphan whose parent was
		// never recorded. Either way it anchors a tree rather than being dropped — a
		// span silently omitted is worse than one with a surprising parent.
		roots = append(roots, o.NodeID)
	}

	for _, id := range roots {
		t.emit(context.Background(), tr, byID, children, id, recordID, metrics)
	}
}

// emit creates one node's span and recurses into its children under that span's
// context, so parentage in the exported trace mirrors the decomposition.
func (t *Tracer) emit(ctx context.Context, tr trace.Tracer, byID map[string]quarry.NodeOutcome,
	children map[string][]string, id, recordID string, metrics map[string]any) {
	o := byID[id]
	// Real recorded timestamps when a clock was injected, so a post-hoc trace still
	// shows true per-node latency and nesting. When it was NOT, the span gets the
	// build-time default and its duration is meaningless — which is why
	// quarry.timing.measured is emitted below: a consumer must be able to tell a
	// measured duration from an artifact of when the trace happened to be built.
	start := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindInternal)}
	var endOpts []trace.SpanEndOption
	measured := false
	if d, ok := o.Timing.Duration(); ok && d >= 0 {
		start = append(start, trace.WithTimestamp(o.Timing.StartedAt))
		endOpts = append(endOpts, trace.WithTimestamp(o.Timing.EndedAt))
		measured = true
	}
	ctx, span := tr.Start(ctx, SpanName(o), start...)
	span.SetAttributes(AttrTimingMeasured.Bool(measured))
	span.SetAttributes(NodeAttributes(o)...)
	if recordID != "" {
		span.SetAttributes(AttrRunID.String(recordID))
	}
	if o.Depth == 0 {
		span.SetAttributes(runAttributes(metrics)...)
	}
	// A gap is an error on the span, not merely an attribute: OTel's error status is
	// how generic tooling surfaces "this did not complete", and a truncated node is
	// exactly that. A failed VERIFICATION is NOT an error — the check worked and
	// returned a verdict, which is a successful assessment of a bad answer (§8).
	if o.Gap {
		span.SetStatus(codes.Error, "truncated: deadline or cancellation (§3.1)")
	}
	for _, kid := range children[id] {
		t.emit(ctx, tr, byID, children, kid, recordID, metrics)
	}
	span.End(endOpts...)
}

// SpanName names a node's span. GenAI semconv's convention is
// "{operation} {model}", so a leaf that called a model reads as an operation on
// that model; an internal node reduced children rather than calling a model, and
// says so. The node ID is an attribute, not part of the name — a name with an ID in
// it fragments aggregation across runs.
func SpanName(o quarry.NodeOutcome) string {
	if o.CacheHit {
		return "cache " + o.NodeID
	}
	if len(o.Children) > 0 {
		return "reduce " + o.NodeID
	}
	if o.ModelVersion != "" {
		return "chat " + o.ModelVersion
	}
	return "solve " + o.NodeID
}

// NodeAttributes maps a NodeOutcome to span attributes: GenAI semconv keys where
// one exists, documented quarry.* keys where none does.
//
// Exported so the mapping is directly testable and so a consumer can reuse it
// against a different exporter without depending on Tracer's buffering.
func NodeAttributes(o quarry.NodeOutcome) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		AttrNodeID.String(o.NodeID),
		AttrDepth.Int(o.Depth),
		AttrCacheHit.Bool(o.CacheHit),
		AttrGap.Bool(o.Gap),
		AttrRetries.Int(o.Retries),
		AttrVerified.String(verdict(o.Verified)),
		AttrClaims.Int(len(o.Claims)),
	}
	if p, ok := parentOf(o.NodeID); ok {
		attrs = append(attrs, AttrParentID.String(p))
	}
	if o.BaseCase != "" {
		attrs = append(attrs, AttrBaseCase.String(string(o.BaseCase)))
	}
	if o.Cost.Limited() {
		attrs = append(attrs, AttrCostMicro.Int64(int64(o.Cost)))
	}

	// The token split now lives ON the outcome, so surface-to-volume (§8.2, P1) is
	// reportable from a sink alone — it previously required the caller to hold the
	// Sample, which meant the one metric that makes P1 observable was the one the
	// observer could not reach. Emitted only for nodes that actually called a model:
	// a zero split on an internal node or a gap is an absence, and semconv's usage
	// keys would read it as a measured zero.
	if o.HaloTokens > 0 || o.GeneratedTokens > 0 {
		attrs = append(attrs,
			semconv.GenAIUsageInputTokens(o.HaloTokens),
			semconv.GenAIUsageOutputTokens(o.GeneratedTokens),
		)
		if ratio, ok := o.SurfaceToVolume(); ok {
			attrs = append(attrs, AttrSurfaceToVolume.Float64(ratio))
		}
	}

	// GenAI semconv where it applies. gen_ai.request.model is the model asked for;
	// gen_ai.response.model is what actually answered — for quarry these differ only
	// when a router resolves an alias, and quarry forbids aliases (P8), so both carry
	// the explicit pinned version. Recording both is what lets a consumer detect a
	// future violation of that rule rather than assume it holds.
	if o.ModelVersion != "" {
		attrs = append(attrs,
			semconv.GenAIOperationNameChat, // the spec's enum member, not a free string
			semconv.GenAIRequestModel(o.ModelVersion),
			semconv.GenAIResponseModel(o.ModelVersion),
		)
	} else if o.Model != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(o.Model))
	}
	return attrs
}

// SampleAttributes is REMOVED. It existed only because NodeOutcome did not carry
// token counts, so a caller holding a Sample had to attach them by hand while a
// plain sink could not report surface-to-volume at all. NodeOutcome now carries the
// split, so NodeAttributes emits the usage keys and the ratio directly.
//
// Deleting it rather than keeping it as a thin alias is deliberate: two functions
// producing the same attributes from different sources is how the two drift, and a
// span whose token counts disagree with the record's is worse than one with none.

// ProvenanceAttributes puts the trust summary on a span (§8) — the trace's answer
// to "how much should I believe this", alongside what it cost.
func ProvenanceAttributes(p quarry.Provenance) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		AttrUnverified.Int(p.Unverified),
		AttrAdversary.Int(p.AdversarialFindings),
	}
	// Stability only when it was actually measured: it needs replicates (P7), and a
	// 0.0 on a span reads as "measured, and unstable" — a stronger and different
	// claim than "not measured". Omission is the honest encoding.
	if p.StabilityKnown {
		attrs = append(attrs, AttrStability.Float64(p.Stability))
	}
	return attrs
}

// runAttributes lifts quarry.RunMetrics' map onto the root span. Keys are
// prefixed so they cannot collide with semconv, and only scalar types are mapped —
// an unexpected type is skipped rather than stringified, because a silently
// reformatted metric is worse than an absent one.
func runAttributes(metrics map[string]any) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(metrics))
	for _, k := range sortedKeys(metrics) {
		key := attribute.Key("quarry.run." + k)
		switch v := metrics[k].(type) {
		case int64:
			attrs = append(attrs, key.Int64(v))
		case int:
			attrs = append(attrs, key.Int(v))
		case float64:
			attrs = append(attrs, key.Float64(v))
		case bool:
			attrs = append(attrs, key.Bool(v))
		case string:
			attrs = append(attrs, key.String(v))
		}
	}
	return attrs
}

// verdict renders the three-state verification outcome. The nil case is the whole
// point: "no verifier assessed this" is a fact the receipt must be able to state
// (§8), and a bool cannot hold it.
func verdict(v *bool) string {
	switch {
	case v == nil:
		return VerdictNotAssessed
	case *v:
		return VerdictPassed
	default:
		return VerdictFailed
	}
}

// parentOf derives a parent node ID from quarry's dotted child IDs ("n0.1.2" →
// "n0.1"). This reads the executor's childID convention, so it is coupled to it;
// the alternative is a Parent field on NodeOutcome, which is a core change. A node
// with no dot is a root.
func parentOf(id string) (string, bool) {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '.' {
			return id[:i], true
		}
	}
	return "", false
}

// Setup builds a TracerProvider exporting over OTLP/HTTP, with quarry's own
// service.name resource.
//
// Per agenkit#711, agenkit's Go tracing sets only service.name and its Rust
// implementation HARDCODES service.name=agenkit ignoring the caller — so quarry
// must set its own resource attributes rather than rely on agenkit's. The endpoint
// is the OTel-spec env var OTEL_EXPORTER_OTLP_ENDPOINT: agenkit's docs contradict
// themselves between that and OTLP_ENDPOINT, and neither is actually read by its
// InitTracing, so the standard name is the one to settle on (it is also what
// AgentCore expects).
//
// Returns a shutdown func that FLUSHES. Call it — this is a post-hoc trace
// exported at Run(), so dropping it loses the whole run's spans, not just a tail.
func Setup(ctx context.Context, serviceName, version string) (trace.TracerProvider, func(context.Context) error, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil, fmt.Errorf("otel: OTEL_EXPORTER_OTLP_ENDPOINT is unset; pass a TracerProvider explicitly for offline use")
	}
	exp, err := newOTLPExporter(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("otel: build exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("otel: build resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return tp, tp.Shutdown, nil
}

var _ quarry.TelemetrySink = (*Tracer)(nil)
