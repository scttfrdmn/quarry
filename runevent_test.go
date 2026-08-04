package quarry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// These tests pin the WIRE CONTRACT with agate (agate#265, docs/integration-
// requirements.md §3). agate is a separate repo in another language with no shared
// IDL, so a rename or a stray field here is a silent break there. Two invariants
// carry the weight:
//
//   - the emitted JSON keys match agate's TS/pydantic twins EXACTLY (its pydantic
//     models declare extra="forbid", so an added field is a validation error, not
//     an ignored one);
//   - the projection is deterministic, so a replayed record yields identical bytes.

func recTwoLeaves() RunRecord {
	yes := true
	return RunRecord{
		RunID: "hash123",
		Outcomes: []NodeOutcome{
			{NodeID: "n0", Problem: Problem{Statement: "root question"},
				Content: "merged answer", Cost: FromFloat(1), Children: []string{"n0.0", "n0.1"}},
			{NodeID: "n0.0", Problem: Problem{Statement: "part one"}, Content: "a",
				Cost: FromFloat(2), Model: "claude", ModelVersion: "claude-v1", Verified: &yes},
			{NodeID: "n0.1", Problem: Problem{Statement: "part two"}, Content: "b",
				Cost: FromFloat(3), Model: "claude", ModelVersion: "claude-v1"},
		},
		Unverified: []string{"n0", "n0.1"},
	}
}

func TestRunEventsOrderMirrorsALiveRun(t *testing.T) {
	// agate's SPA folds an ORDERED stream: models, answer, receipt, artifact. The
	// artifact must close the run — agate's build_artifact reads provenance off it.
	events := RunEvents(recTwoLeaves(), "s3://records/hash123", nil)
	var got []string
	for _, ev := range events {
		got = append(got, ev.eventType())
	}
	want := []string{"model", "answer", "receipt", "artifact"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestRunEventsOneModelEventPerDistinctVersion(t *testing.T) {
	// Two leaves share a pinned version, so ONE model event carrying their summed
	// spend — not one per node, which would flood a wide tree's stream.
	events := RunEvents(recTwoLeaves(), "", nil)
	var models []ModelEvent
	for _, ev := range events {
		if m, ok := ev.(ModelEvent); ok {
			models = append(models, m)
		}
	}
	if len(models) != 1 {
		t.Fatalf("two leaves on one version → one model event, got %d", len(models))
	}
	if models[0].Label != "claude-v1" {
		t.Errorf("label must be the explicit pinned version (P8), got %q", models[0].Label)
	}
	if models[0].Cost != 5.0 { // 2 + 3; the root's 1 is a reduce with no model version
		t.Errorf("model cost must sum only nodes on that version, want 5.0, got %v", models[0].Cost)
	}
}

func TestRunEventsReceiptTotalEqualsSumOfRows(t *testing.T) {
	// A receipt that does not add up is worse than none (§8). Rows cover every
	// spending node, reduce nodes included — not leaves only.
	var receipt ReceiptEvent
	for _, ev := range RunEvents(recTwoLeaves(), "", nil) {
		if r, ok := ev.(ReceiptEvent); ok {
			receipt = r
		}
	}
	if len(receipt.Rows) != 3 {
		t.Fatalf("all three spending nodes get a row, got %d", len(receipt.Rows))
	}
	var sum float64
	for _, row := range receipt.Rows {
		sum += row.Cost
		if row.Kind != KindLLM {
			t.Errorf("agate#265 C2: only kind %q may be emitted, got %q", KindLLM, row.Kind)
		}
	}
	if sum != receipt.Total {
		t.Errorf("rows sum %v must equal total %v", sum, receipt.Total)
	}
	if receipt.Total != 6.0 {
		t.Errorf("total must be the record's TotalCost, want 6.0, got %v", receipt.Total)
	}
}

func TestRunEventsSkipsUnpricedNodes(t *testing.T) {
	// A cache hit spends nothing and a gap produced nothing; neither belongs on a
	// cost receipt. Nothing may invent a zero-cost line implying work was metered.
	rec := RunRecord{
		RunID: "x",
		Outcomes: []NodeOutcome{
			{NodeID: "n0", Content: "answer", Cost: FromFloat(1)},
			{NodeID: "n0.0", CacheHit: true, Content: "served"},    // free (§6)
			{NodeID: "n0.1", Gap: true},                            // truncated (§3.1)
			{NodeID: "n0.2", Cost: Unlimited, Content: "unpriced"}, // no price sheet
		},
	}
	for _, ev := range RunEvents(rec, "", nil) {
		if r, ok := ev.(ReceiptEvent); ok {
			if len(r.Rows) != 1 {
				t.Fatalf("only the one spending node is itemised, got %d rows", len(r.Rows))
			}
			if !strings.HasPrefix(r.Rows[0].Label, "n0") {
				t.Errorf("row must be labelled by node, got %q", r.Rows[0].Label)
			}
		}
	}
}

func TestRunEventsArtifactCarriesProvenanceWhenGiven(t *testing.T) {
	rec := recTwoLeaves()
	prov := ProvenanceOf(rec, nil)
	var art ArtifactEvent
	for _, ev := range RunEvents(rec, "s3://records/hash123", &prov) {
		if a, ok := ev.(ArtifactEvent); ok {
			art = a
		}
	}
	if art.RunID != "hash123" {
		t.Errorf("artifact run_id is the content hash (P8), got %q", art.RunID)
	}
	if art.URL != "s3://records/hash123" {
		t.Errorf("artifact must point back at the citable record, got %q", art.URL)
	}
	if art.Provenance == nil {
		t.Fatal("provenance must ride on the artifact event (agate#265 C3)")
	}
	if art.Provenance.Verified != 1 || art.Provenance.Unverified != 2 {
		t.Errorf("provenance counts must come from the record, got %+v", art.Provenance)
	}
}

func TestRunEventsOmitsProvenanceWhenNil(t *testing.T) {
	// A producer that did not verify must emit NO provenance key rather than a
	// zeroed one — agate reads absent as "does not verify" and 0.0 as measured zero.
	var buf bytes.Buffer
	events := RunEvents(recTwoLeaves(), "", nil)
	if err := WriteRunEvents(&buf, events); err != nil {
		t.Fatal(err)
	}
	if contains(buf.String(), "provenance") {
		t.Errorf("nil provenance must be omitted from the wire form: %s", buf.String())
	}
}

func TestRunEventsArtifactEmittedForAnEmptyRun(t *testing.T) {
	// A run that produced nothing must still be citable by hash (§8): no models, no
	// answer, an empty receipt, and the artifact that identifies it.
	events := RunEvents(RunRecord{RunID: "empty"}, "", nil)
	if len(events) != 2 {
		t.Fatalf("an empty run emits receipt + artifact, got %d events", len(events))
	}
	if events[len(events)-1].eventType() != "artifact" {
		t.Errorf("the artifact must close the run, got %q", events[len(events)-1].eventType())
	}
}

func TestProvenanceWireKeysAreExactlyAgatesSet(t *testing.T) {
	// agate's pydantic RunProvenance declares extra="forbid" (agate/artifact.py), so
	// an EXTRA key is a hard validation failure on their side, not an ignored field.
	// Assert the key set exactly — presence alone is not enough.
	b, err := json.Marshal(Provenance{RecordHash: "h", StabilityKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"record_hash": true, "verified": true, "unverified": true,
		"stability": true, "adversarial_findings": true,
	}
	for k := range m {
		if !want[k] {
			t.Errorf("extra key %q would be rejected by agate's extra=\"forbid\" model", k)
		}
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing agate key %q", k)
		}
	}
}

func TestWriteRunEventsIsNewlineDelimitedAndDeterministic(t *testing.T) {
	// agate's transport decodes NDJSON, one event per line (parseEventBlob). And the
	// projection is a pure fold, so two writes of one record are byte-identical —
	// which is what lets a replay's event stream be compared as an artifact (P8).
	rec := recTwoLeaves()
	var a, b bytes.Buffer
	if err := WriteRunEvents(&a, RunEvents(rec, "u", nil)); err != nil {
		t.Fatal(err)
	}
	if err := WriteRunEvents(&b, RunEvents(rec, "u", nil)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("the projection must be byte-stable across calls (P8)")
	}
	lines := strings.Split(strings.TrimRight(a.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want one line per event, got %d: %q", len(lines), a.String())
	}
	for i, ln := range lines {
		var probe map[string]any
		if err := json.Unmarshal([]byte(ln), &probe); err != nil {
			t.Fatalf("line %d is not standalone JSON: %v", i, err)
		}
		if _, ok := probe["type"]; !ok {
			t.Errorf("line %d has no discriminant type field: %s", i, ln)
		}
	}
}

func TestWriteRunEventsDoesNotEscapeContent(t *testing.T) {
	// Answer text round-trips unaltered, matching the record's canonical encoding —
	// an escaped ampersand would make the projected answer differ from the artifact's.
	rec := RunRecord{RunID: "x", Outcomes: []NodeOutcome{
		{NodeID: "n0", Content: `Smith & Jones showed a < b`},
	}}
	var buf bytes.Buffer
	if err := WriteRunEvents(&buf, RunEvents(rec, "", nil)); err != nil {
		t.Fatal(err)
	}
	if !contains(buf.String(), `Smith & Jones showed a < b`) {
		t.Errorf("content must round-trip unescaped: %s", buf.String())
	}
}

func TestRowLabelTruncatesDeterministically(t *testing.T) {
	long := strings.Repeat("x", labelStatementLimit+20)
	got := rowLabel(NodeOutcome{NodeID: "n0", Problem: Problem{Statement: long}})
	if !strings.HasSuffix(got, "…") {
		t.Errorf("an over-long statement must be truncated, got %q", got)
	}
	if got != rowLabel(NodeOutcome{NodeID: "n0", Problem: Problem{Statement: long}}) {
		t.Error("the label must be a pure function of the outcome (P8)")
	}
	bare := rowLabel(NodeOutcome{NodeID: "n7"})
	if bare != "n7" {
		t.Errorf("a statement-less node falls back to its ID, got %q", bare)
	}
}

func TestUnitsToUSDIsTheInverseOfMicroUnits(t *testing.T) {
	// agate prices in USD at 6 dp, 1:1 with quarry's micro-units (agate#265 §4). The
	// round trip must be exact at micro-unit granularity or the SPA's total and the
	// ledger's disagree.
	for _, u := range []Units{0, 1, 999999, 1_000_000, 6_500_000} {
		if got := FromFloat(unitsToUSD(u)); got != u {
			t.Errorf("round trip %d → %v → %d", u, unitsToUSD(u), got)
		}
	}
	if unitsToUSD(Unlimited) != 0 {
		t.Error("an unpriced node has no cost line, not a negative one")
	}
}

func TestAnUnpublishableStabilityIsOmittedFromTheWire(t *testing.T) {
	// END TO END, because the omission is the CALLER's move and a rule nobody applies
	// is not a rule. ProvenanceOf can clear StabilityKnown correctly and the misleading
	// 0.0 still reaches agate if RunEvents is handed the summary anyway.
	//
	// agate's `stability` is a non-nullable number, so omitting the whole provenance
	// object is the only in-band way to say "not measured" (agate#265).
	recs := []RunRecord{
		recOfClaims("prices rose in Q3"),
		recOfClaims("there was a third-quarter price increase"),
	}
	rep := StabilityWith(context.Background(), recs, MechanicalComparator{}, 0, nil)
	prov := ProvenanceOf(RunRecord{RunID: "x"}, &rep)
	if prov.StabilityKnown {
		t.Fatal("fixture must be unpublishable; the check is vacuous")
	}

	// What a caller following the documented rule does.
	var pass *Provenance
	if prov.StabilityKnown {
		pass = &prov
	}
	events := RunEvents(RunRecord{RunID: "x"}, "http://example/x", pass)

	var buf bytes.Buffer
	if err := WriteRunEvents(&buf, events); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"stability"`)) {
		t.Errorf("an unpublishable stability must not reach the wire at all; agate would "+
			"read the 0.0 as measured:\n%s", buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"artifact"`)) {
		t.Error("the run must still be citable by hash even with no trust summary (§8)")
	}
}
