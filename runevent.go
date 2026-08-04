package quarry

import (
	"encoding/json"
	"io"
	"strings"
)

// Projection of a completed run into agate's RunEvent stream (build step 10, §8;
// contract in docs/integration-requirements.md §3, verified in agate#265).
//
// agate's SPA consumes a FLAT, ordered event stream — not a span tree — decoded
// from newline-delimited JSON (web/src/transport/agentcore.ts parseEventBlob). So
// making a quarry run visible in the agate UI is a pure FOLD of the RunRecord into
// that stream. This file is that fold and nothing more:
//
//   - It is a PROJECTION, not a replacement. quarry's RunRecord stays the citable
//     artifact (P8); these events are a lossy view for a UI, and the artifact event
//     carries a url pointer back to the real record. We do not flatten quarry's
//     provenance into agate's wire form and call it the record.
//   - It is PURE. No clock, no network, no AWS (Go rule 4) — a fold over a value.
//     Emitting the bytes over a transport is the host's job.
//   - It is DETERMINISTIC. Pre-order outcomes in, fixed field order out, so the
//     same record produces byte-identical NDJSON on every machine (P8).
//
// Two constraints from agate#265 are load-bearing here:
//
//   - C2: receipt rows may only use kinds the TS union accepts. quarry emits
//     kind:"llm" exclusively. Do NOT widen to embedding/retrieval.
//   - C3: provenance rides on the EXISTING ArtifactEvent as an optional field —
//     there is no separate provenance event. agate's pydantic twin declares
//     extra="forbid", so quarry may not add a field to that object either; see the
//     exact-key-set test in provenance_test.go.
//
// Floats appear here and ONLY here at the wire edge: agate prices in USD rounded
// to 6 dp, which is 1:1 with quarry's int64 micro-units. The ledger stays integral
// (Go rule 3); conversion happens at the boundary, in one function.

// RunEvent is one event in agate's stream. The union is open — agate's SPA reducer
// tolerates unknown types — so this is a marker interface rather than a closed sum
// type, and adding an event kind here cannot break an existing consumer.
type RunEvent interface {
	eventType() string
}

// ModelEvent names a model that produced output in this run (agate ModelEvent).
//
// One event per DISTINCT pinned model version, not per node: a wide tree would
// otherwise flood the stream, and agate's fold dedupes by label anyway
// (artifact.py build_artifact collects distinct labels into RunArtifact.models).
//
// Tier duplicates Label deliberately. agate's `tier` is its own roster vocabulary
// ("frontier", "open-weight-70b"); quarry has no tiers — it pins explicit versioned
// model IDs (P8). Inventing a tier would be a false claim about agate's roster, so
// the version stands in for both fields and the record remains honest.
type ModelEvent struct {
	Type  string  `json:"type"`
	Tier  string  `json:"tier"`
	Label string  `json:"label"`
	State string  `json:"state"` // always "done": this is a post-hoc projection, not a live run
	Cost  float64 `json:"cost"`
}

func (ModelEvent) eventType() string { return "model" }

// AnswerEvent carries the root node's reduced answer (agate AnswerEvent).
type AnswerEvent struct {
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
}

func (AnswerEvent) eventType() string { return "answer" }

// ReceiptRow is one itemised cost line. Kind is always KindLLM (agate#265 C2).
type ReceiptRow struct {
	Label string  `json:"label"`
	Kind  string  `json:"kind"`
	Cost  float64 `json:"cost"`
}

// KindLLM is the only receipt kind quarry emits. agate's Python CostKind also has
// embedding/retrieval/compute, and the TS union now carries them too (agate#265
// C2), but quarry only makes model calls — emitting a kind it does not incur would
// be fiction.
const KindLLM = "llm"

// ReceiptEvent is the itemised receipt closing the run (agate ReceiptEvent).
//
// Total is the record's TOTAL COST and Rows account for all of it: every node that
// SPENT gets a row, internal reduce nodes included. Rows-per-leaf would leave the
// reducer's spend in the total with no line explaining it — a receipt that does not
// add up is worse than no receipt (§8).
type ReceiptEvent struct {
	Type  string       `json:"type"`
	Rows  []ReceiptRow `json:"rows"`
	Total float64      `json:"total"`
}

func (ReceiptEvent) eventType() string { return "receipt" }

// ArtifactEvent points at the citable record and, optionally, summarizes how much
// to trust it (agate ArtifactEvent + RunProvenance, agate#265 C3).
//
// This event is why the integration is worth building: agate's SPA can show what a
// run COST without it, but only Provenance lets it show how much to TRUST the
// answer — which is quarry's entire reason to exist (P7).
type ArtifactEvent struct {
	Type       string      `json:"type"`
	RunID      string      `json:"run_id"`
	URL        string      `json:"url"`
	Provenance *Provenance `json:"provenance,omitempty"`
}

func (ArtifactEvent) eventType() string { return "artifact" }

// labelStatementLimit truncates a problem statement in a receipt label. Fixed so
// the projection stays a deterministic function of the record (P8).
const labelStatementLimit = 60

// RunEvents folds a completed record into agate's ordered event stream.
//
// recordURL is where the full RunRecord is retrievable; it becomes the artifact
// event's url, the pointer from the lossy UI view back to the citable artifact
// (P8). Pass an empty string if the record is not yet addressable — the event is
// still emitted, because a run must be citable by hash even before it is hosted.
//
// prov is the trust summary from ProvenanceOf, or nil to omit it. **Check
// StabilityKnown and pass nil when it is false** — agate's field is declared
// `stability: number`, not nullable, so an unpublishable estimate reaches the SPA as
// a measured 0.0 and badges "nothing replicated". quarry cannot signal the
// difference in-band: the pydantic twin forbids extra fields, so a flag would be
// rejected on the wire (docs/integration-requirements.md §3).
//
// THREE cases clear that flag, not just the n=1 one this comment used to name. A
// single run has no estimate (P7); a rate of 0 reached with UNASSESSED comparisons is
// "nobody could tell", not "nothing replicated" (§7, §8); and a TRUNCATED comparison
// pass is admittedly under-merged, so any rate off it is provisional. All three would
// render identically as 0.0, and all three would be read as a finding. See
// ProvenanceOf's fabricatedZero for why the rule stops there and still publishes a
// non-zero floor.
//
// Event order mirrors a live agate run: the models that produced output, the
// answer, the receipt, then the artifact that closes it. Deterministic throughout —
// outcomes arrive pre-order and first-seen model order is preserved.
func RunEvents(r RunRecord, recordURL string, prov *Provenance) []RunEvent {
	var events []RunEvent

	// Models: distinct pinned versions in first-seen (pre-order) order, with each
	// version's summed spend. Internal reduce nodes and cache hits carry no model
	// (types.go), so they contribute nothing here — correctly: nothing they did was
	// a fresh model call attributable to a version.
	order := make([]string, 0, len(r.Outcomes))
	spend := make(map[string]Units, len(r.Outcomes))
	for _, o := range r.Outcomes {
		if o.ModelVersion == "" {
			continue
		}
		if _, seen := spend[o.ModelVersion]; !seen {
			order = append(order, o.ModelVersion)
		}
		if o.Cost.Limited() {
			spend[o.ModelVersion] += o.Cost
		}
	}
	for _, v := range order {
		events = append(events, ModelEvent{
			Type: "model", Tier: v, Label: v, State: "done", Cost: unitsToUSD(spend[v]),
		})
	}

	// The answer is the root's content. Outcomes are pre-order, so the root is
	// first; an empty-outcome record (a run that failed before any node completed)
	// has no answer to emit and skips straight to the receipt and artifact.
	if len(r.Outcomes) > 0 && r.Outcomes[0].Content != "" {
		events = append(events, AnswerEvent{Type: "answer", Text: r.Outcomes[0].Content})
	}

	// The receipt itemises every node that spent, and its total is the record's
	// total — see the ReceiptEvent doc on why rows are not leaves-only.
	rows := make([]ReceiptRow, 0, len(r.Outcomes))
	for _, o := range r.Outcomes {
		if !o.Cost.Limited() || o.Cost == 0 {
			continue // a cache hit, a gap, or a priced-out node: nothing to itemise
		}
		rows = append(rows, ReceiptRow{Label: rowLabel(o), Kind: KindLLM, Cost: unitsToUSD(o.Cost)})
	}
	events = append(events, ReceiptEvent{Type: "receipt", Rows: rows, Total: unitsToUSD(r.TotalCost())})

	// The artifact closes the run and carries the trust summary. Emitted even for an
	// empty or failed run: the record is identified by its content hash regardless,
	// and a run that produced nothing is exactly the kind a researcher must be able
	// to cite (§8).
	events = append(events, ArtifactEvent{
		Type: "artifact", RunID: r.RunID, URL: recordURL, Provenance: prov,
	})
	return events
}

// WriteRunEvents writes events as newline-delimited JSON — the format agate's SPA
// transport decodes (parseEventBlob splits on newlines and parses each line).
//
// HTML escaping is off so answer content round-trips unaltered, matching the
// record's canonical encoding. Byte-for-byte deterministic for a given record,
// which is what lets a replay's event stream be compared as an artifact (P8).
func WriteRunEvents(w io.Writer, events []RunEvent) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

// rowLabel makes a receipt row legible: the node ID plus a truncated statement.
// The ID alone ("n0.1.2") tells a researcher nothing about what they paid for, and
// agate's row has no field for a statement, so it rides in the label. Truncation
// is at a fixed rune count, so the label stays a pure function of the record.
func rowLabel(o NodeOutcome) string {
	st := strings.TrimSpace(o.Problem.Statement)
	if st == "" {
		return o.NodeID
	}
	if runes := []rune(st); len(runes) > labelStatementLimit {
		st = string(runes[:labelStatementLimit]) + "…"
	}
	return o.NodeID + " " + st
}

// unitsToUSD converts micro-units to agate's 6-dp USD. The inverse of
// provider.usdToUnits, and the ONLY float conversion in the core: it exists at the
// wire edge because agate prices in dollars, and it never feeds back into ledger
// arithmetic (Go rule 3). Unlimited reads as zero — an unpriced node has no line on
// a cost receipt.
func unitsToUSD(u Units) float64 {
	if !u.Limited() {
		return 0
	}
	return float64(u) / 1e6
}
