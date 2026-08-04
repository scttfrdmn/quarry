package otel

import (
	"context"
	"sort"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// newOTLPExporter builds the OTLP/HTTP span exporter. Isolated in its own file so
// the network dependency is in one place: everything in tracer.go is a pure
// mapping and is tested with an in-memory exporter and no collector.
//
// The exporter reads OTEL_EXPORTER_OTLP_ENDPOINT itself (the OTel spec name), so
// no endpoint is threaded through — Setup checks it only to fail loudly rather
// than silently exporting to localhost.
func newOTLPExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	return otlptracehttp.New(ctx)
}

// sortedKeys gives a stable attribute order. A trace is not a reproducible
// artifact (see tracer.go) so this is not a P8 requirement — it is here so span
// attributes read the same way across runs when a human is comparing two traces
// side by side, and so tests are not flaky on map iteration order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
