package quarry

import (
	"encoding/json"
	"io"
	"math"
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
// Σ ModelEvent.Cost IS LESS THAN ReceiptEvent.Total ON A REAL RUN, and a host must not
// treat the difference as a defect. This comment used to claim the opposite — "internal
// reduce nodes and cache hits carry no model, so they contribute nothing here —
// correctly: nothing they did was a fresh model call attributable to a version" — and
// the second half of that was wrong. A cache hit is cost 0 and drops out of both sides,
// but a REDUCE node under BedrockReducer is a real model call with an explicit pinned
// version, and executor.go's reduce path assigns Cost while never assigning Model or
// ModelVersion. So it lands in the receipt and in TotalCost and in no ModelEvent.
//
// Measured on quarry-run-72c970d2ef42.json (the 25-row live record): 7 of its 25 spending
// nodes carry no version, and the model events account for 38395 of 80437 micro-units —
// a 42042 residual, over half the run. Not a rounding artefact. Left as
// a residual rather than fixed here, because the fix is in the executor and changes what
// a NodeOutcome hashes to — see issue #20; the superseded claim stays visible above per
// the standing convention. The wrong statement was in the doc for a year and only a host
// author reading it from outside found it.
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

// StreamVersion is the framed-stream contract version a host branches on (#9 D2).
//
// A host that spawns quarry as a subprocess must be able to REFUSE a stream it does
// not understand, and it cannot do that by inspecting events it has never seen. So
// the version is stated first, before anything a consumer would try to fold.
//
// The compatibility rule, which is the actual deliverable here:
//
//   - ADDING an event kind is a MINOR change and does not bump this. agate's SPA
//     reducer tolerates unknown types and its build_artifact dispatches on `type`
//     and skips what it does not know (verified against the real Python), so a new
//     kind cannot break a conforming consumer.
//   - CHANGING or REMOVING a field, or changing what an existing kind means, is a
//     MAJOR change and bumps this. Those are the changes a consumer cannot absorb.
//
// The version rides its OWN EVENT rather than a field on ArtifactEvent, and that is
// forced rather than chosen: agate's RunProvenance twin declares extra="forbid", so
// an added field is a hard validation error on the Python side, while an unknown
// event type is skipped. Verified both directions against agate's real build_artifact
// before committing to this shape.
const StreamVersion = 1

// StreamEvent opens a framed stream, declaring the contract version (#9 D2).
//
// Its type is namespaced (`quarry_stream`, not `version`) because this event is
// quarry's own, not part of agate's union: agate ignores it, and a name like
// "version" is exactly the kind a second producer might also pick with a different
// meaning.
type StreamEvent struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	// Producer names what wrote the stream. A host reading a vendored fixture months
	// later needs to know which implementation produced it — there is a parallel Python
	// quarry, and the two agree on behaviour but are not the same code.
	Producer string `json:"producer"`
}

func (StreamEvent) eventType() string { return "quarry_stream" }

// OutcomeEvent closes a framed stream, stating how the run ended (#9 D4).
//
// THE TERMINAL EVENT, and its presence is as load-bearing as its content. A host that
// reads a stream to EOF cannot otherwise tell a finished run from a killed one: NDJSON
// gives it complete lines either way, so a run cut off after the artifact event looks
// exactly like a run that finished. #9's corpus requires a crashed run to be
// distinguishable from a time-truncated one, and only a terminal marker does that. The
// exit code says the same thing, but a host reading a captured stream from a file — which
// is what vendoring a fixture means — has no exit code at all.
//
// WHY IT IS NOT IN agate's UNION. agate has no gap representation (docs/integration-
// requirements.md §4, a standing §9 divergence), so the ONE fact a supervising host most
// needs — did this answer cover the question, or part of it — cannot ride on any event
// agate accepts. Rather than widen agate's contract, this is quarry's own event on
// quarry's own frame, skipped by agate's build_artifact along with the frame itself.
//
// The three counts are here because they are the distinction the classification collapses.
// Outcome names the headline; Gaps and Unfunded name WHICH denomination produced the
// shortfall, and under the standing ruling those are not interchangeable — a host offered
// a deadline raise when it needed money is the §3.1 mislabelling ErrRecordedUnfunded
// exists to prevent.
type OutcomeEvent struct {
	Type    string  `json:"type"`
	Outcome Outcome `json:"outcome"`
	// BoundBy is the denomination that actually bit, or "" for neither (§8.2). Empty is
	// meaningful — an unbound run — not missing, so it is NOT omitempty: a host that saw
	// no key could not tell "no cap bound this" from "this producer does not report it".
	BoundBy Denomination `json:"bound_by"`
	// Gaps counts nodes TIME cut short; Unfunded counts nodes the cap priced out. Both
	// always present for the same reason BoundBy is: zero is a measurement here.
	Gaps     int `json:"gaps"`
	Unfunded int `json:"unfunded"`
	// TotalMicros is the run's spend in MICRO-UNITS, integral — the one figure on this
	// stream that is not a float. Everything agate's union carries prices in USD because
	// agate does; this event is quarry's own, so it carries the ledger's own integers and
	// a host has nothing to reconcile (#18, integration-requirements §3 and §6).
	TotalMicros int64 `json:"total_micros"`
	// CapMicros is the spend cap, or -1 for Unlimited — the contract the total sits under
	// (P4). Unlimited is already -1 in Units, so this is the value not a sentinel swap.
	CapMicros int64 `json:"cap_micros"`
}

func (OutcomeEvent) eventType() string { return "quarry_outcome" }

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
	// total — see the ReceiptEvent doc on why rows are not leaves-only. Read it with
	// ReceiptReconciles, not by summing the floats; #18 says why.
	//
	// THE SKIP AND THE TOTAL DISAGREE ABOUT Unlimited, and it is recorded rather than
	// fixed. A row is skipped when !Limited, but TotalCost sums unconditionally, so an
	// Unlimited cost would land in the total with no line explaining it — the very
	// thing the ReceiptEvent doc forbids. It is UNREACHABLE today: every provider
	// prices a Limited cost (bedrock.go price returns 0 for an unsheeted model,
	// chokepoint takes the metered actual, fake prices from its own sheet), so only a
	// test constructs one. Guarding here would be a branch no input reaches, and
	// CLAUDE.md prefers a marked gap to a plausible guess.
	// TODO(§8): if a provider ever returns an Unlimited cost, this is where the total
	// stops being explicable, and TotalCost is the side that has to change.
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

// HostRunEvents is RunEvents FRAMED — a version line in front, an outcome line behind —
// the stream a supervising host consumes (#9).
//
// A SEPARATE FOLD, not a flag on RunEvents, because #9's non-goals forbid changing
// what agate receives. agate reaches quarry through its own transport and has never
// needed a version (its SPA predates this integration); a host spawning quarry as a
// subprocess does need one, and cannot get it from a stream whose first line is
// already payload. The two consumers differ in exactly two events, so they differ in
// exactly one function.
//
// Everything BETWEEN the frames is byte-identical to RunEvents, which is the property
// that keeps ONE union rather than three: forking the events per host would give
// quarry three protocols to hold in lockstep by hand. The frame is additive at both
// ends and agate's build_artifact skips both — verified against the real Python, which
// dispatches on `type` and ignores what it does not know.
//
// The two frames are not symmetric in what they prove. The version lets a host REFUSE a
// stream; the outcome lets it TRUST one it read to EOF. Only the second distinguishes a
// finished run from a killed one, since NDJSON yields whole lines either way.
func HostRunEvents(r RunRecord, recordURL string, prov *Provenance) []RunEvent {
	events := []RunEvent{StreamEvent{
		Type: "quarry_stream", Version: StreamVersion, Producer: "quarry-go",
	}}
	events = append(events, RunEvents(r, recordURL, prov)...)
	return append(events, OutcomeEvent{
		Type:     "quarry_outcome",
		Outcome:  r.Classify(),
		BoundBy:  r.BoundBy,
		Gaps:     len(r.Gaps()),
		Unfunded: len(r.Unfunded()),
		// int64(Units) directly, NOT unitsToUSD: this event is quarry's own and carries
		// the ledger's integers, so there is no float to reconcile. Unlimited passes
		// through as its own -1 (see the field docs).
		TotalMicros: int64(r.TotalCost()),
		CapMicros:   int64(r.Caps.Spend),
	})
}

// WriteRunEvents writes events as newline-delimited JSON — the format agate's SPA
// transport decodes (parseEventBlob splits on newlines and parses each line).
//
// HTML escaping is off so answer content round-trips unaltered, matching the
// record's canonical encoding. Byte-for-byte deterministic for a given record,
// which is what lets a replay's event stream be compared as an artifact (P8).
//
// Every line is \n-terminated INCLUDING THE LAST — json.Encoder.Encode appends one.
// A host reading line-by-line otherwise cannot tell a complete final event from a
// truncated one, which is precisely the crashed-vs-finished distinction #9's exit
// codes exist to make.
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

// USDToUnits converts a wire float back to micro-units — THE RULE FOR READING A
// RECEIPT, and the reason it is exported (#18).
//
// Every value unitsToUSD emits is an exact 6-dp decimal, because Go writes the
// shortest float64 that round-trips; so each row and the total agree with the ledger
// to the micro-unit individually. What does NOT hold is that the rows SUM to the
// total in float64: unitsToUSD divides each row by 1e6 separately and the errors
// accumulate, while TotalCost sums integers and converts once. On a real 25-node run
// the two differ by 1.4e-17, and quarry's own test missed it for a year because its
// fixture used 1+2+3 — three values that happen to sum exactly in binary floating
// point (§8, "a receipt that does not add up is worse than no receipt").
//
// So a consumer must come back to integers BEFORE summing. Two independent hosts in
// two languages would otherwise each pick their own epsilon, and any epsilon is a
// guess about tree size: the error grows with the number of rows.
//
// math.Round, NOT FromFloat, and this is not interchangeable. FromFloat truncates
// (Units(f * 1e6)), which fails to round-trip 2884 of the first 200000 micro-unit
// values — 0.000249 truncates to 248. It is the same defect provider.usdToUnits
// already documents on the agate seam, where int() would desync the local debit from
// the remote meter by one micro-unit.
func USDToUnits(usd float64) Units {
	return Units(math.Round(usd * 1e6))
}

// ReceiptReconciles reports whether a receipt's rows sum to its total, compared the
// only way that is stable: in micro-units (#18).
//
// The twins' conformance corpus (#9) checks exactly this, in Go and in Rust, so the
// rule lives here as code rather than as a sentence in a doc that each host reimplements.
func ReceiptReconciles(r ReceiptEvent) bool {
	var sum Units
	for _, row := range r.Rows {
		sum += USDToUnits(row.Cost)
	}
	return sum == USDToUnits(r.Total)
}
