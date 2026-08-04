package quarry

// Provenance is the trust summary quarry emits into agate's SPA (build step 10;
// agate#265 C3). agate accepted this exact shape as an OPTIONAL field on its
// existing ArtifactEvent — additive, omitted when absent, so existing consumers
// are unaffected. The SPA badges trust beside cost with it; quarry's full
// RunRecord remains the citable artifact (P8), and the event carries this summary
// plus a url pointer to the record.
//
// This is the answer to quarry's whole reason to exist made visible at the UI: a
// run is an estimate, not an answer (P7), and this is the "how much to trust it"
// that a bare cost receipt cannot express (§8).
//
// Field names and JSON tags match agate's ArtifactEvent.provenance twin
// (web/src/events/protocol.ts + agate/artifact.py RunProvenance) EXACTLY. Keep
// them in lockstep by hand — there is no shared IDL (docs/integration-
// requirements.md). A rename here is a wire break there.
type Provenance struct {
	RecordHash          string  `json:"record_hash"`          // RunRecord content-hash (RunID) — the pointer's identity (P8)
	Verified            int     `json:"verified"`             // nodes a verifier checked AND passed
	Unverified          int     `json:"unverified"`           // nodes no verifier assessed — what was NOT checked (§8)
	Stability           float64 `json:"stability"`            // stable-claim fraction 0..1; meaningful only across replicates (§7)
	AdversarialFindings int     `json:"adversarial_findings"` // claims an adversary refuted (§5)

	// StabilityKnown is quarry-side only (json:"-"): a single run has no stability
	// estimate — that needs replicates (§7) — so Stability would be a fabricated 0.
	// This flag lets a caller decide whether to emit the field at all rather than
	// send a false certainty. agate's wire shape has no such flag; when false, the
	// caller should omit provenance or agate reads 0.0 as "not yet measured".
	//
	// It is ALSO false when the rate exists but would mislead — see the three
	// qualifiers below and ProvenanceOf's rule.
	StabilityKnown bool `json:"-"`

	// The three comparison qualifiers, all quarry-side only (json:"-") because
	// agate's RunProvenance is pydantic extra="forbid" — an unrecognized key is a
	// HARD validation error on their side, so adding one is a coordinated two-repo
	// change (docs/integration-requirements.md). They are recorded here anyway
	// because the omission rule needs them and because a caller writing its own
	// artifact should not have to re-derive them from the report.

	// StabilityIsFloor reports that unassessed comparisons make the rate a LOWER
	// BOUND rather than a point estimate (§7). Claims that should have merged did
	// not, so the cluster count is inflated and the stable fraction depressed. Under
	// the free comparator this is true of almost every multi-replicate report, which
	// is exactly why it cannot by itself suppress the number.
	StabilityIsFloor bool `json:"-"`

	// Unassessed is how many comparisons could not be made — neither agreement nor
	// disagreement (§7). Zero on the free path only when every wording matched.
	Unassessed int `json:"-"`

	// ComparedBy names the comparator that decided equivalence. A floor from
	// normalized-string equality and a measurement from a model reach the wire as
	// the same bare float, so without this the number cannot be attributed (P8).
	ComparedBy string `json:"-"`
}

// ProvenanceOf builds the trust summary from a completed record and, optionally,
// a stability report over its replicates.
//
// Pass a nil report for a single run: Stability is left 0 and StabilityKnown
// false, because a stable-claim fraction is not defined for n=1 (P7 — one run is
// one sample, not a distribution). Pass the StabilityReport from Stability(...)
// once replicates exist, and the fraction is real.
//
// Verified counts nodes a verifier passed; Unverified is the record's own
// unverified list (§8), so the two never double-count a gap. AdversarialFindings
// counts refuted claims — the high-value refine signal (§5, §8.1).
func ProvenanceOf(r RunRecord, report *StabilityReport) Provenance {
	p := Provenance{
		RecordHash:          r.RunID,
		Unverified:          len(r.Unverified),
		AdversarialFindings: brokenCount(r.Adversarial),
	}
	for _, o := range r.Outcomes {
		if o.Verified != nil && *o.Verified {
			p.Verified++
		}
	}
	if report != nil {
		p.Unassessed = report.Unassessed
		p.ComparedBy = report.ComparedBy
		p.StabilityIsFloor = report.Unassessed > 0 || report.Truncated
		if rate, ok := report.StabilityRate(); ok && !fabricatedZero(*report, rate) {
			p.Stability = rate
			p.StabilityKnown = true
		}
	}
	return p
}

// fabricatedZero reports whether a computable rate would still MISLEAD, in which
// case ProvenanceOf leaves StabilityKnown false and the caller omits provenance.
//
// THE LEAK THIS CLOSES, found by probing ProvenanceOf against the new report
// fields. Two replicates asserting the same conclusion in different words, free
// comparator: 2 clusters, 0 stable, unassessed=1 — and StabilityRate() returns
// (0.0, ok=true). The SPA then badges "stability 0.0", which reads as MEASURED AND
// NOTHING REPLICATED, when the truth is that nobody could tell. That is silence
// converted into a finding (§8) at the last hop before the UI, having been kept
// distinct all the way through the comparator seam and the report.
//
// THE RULE IS NARROW ON PURPOSE, and the narrowness is the whole difficulty. Under
// the free comparator EVERY differing wording is unassessed, so "unassessed > 0 →
// omit" would suppress almost every multi-replicate report and throw away the
// perfectly well-defined verified/unverified counts with it — the same over-broad
// omission that agate#265 already flags as the cost of a non-nullable stability
// field. So a floor is still published as long as it is a floor ABOVE ZERO: some
// claim really did replicate, and a lower bound with real support is information.
//
// What is suppressed is the case where the number carries NO information but reads
// as a strong finding:
//
//   - rate == 0 AND something was unassessed — indistinguishable, from the float
//     alone, from "measured, and nothing replicated". The dangerous direction.
//   - the pass was TRUNCATED — the clustering is admittedly incomplete and
//     under-merged, so every number derived from it is provisional.
//
// A rate of 0 with NOTHING unassessed and no truncation is a real finding and is
// published: the comparator was asked about every pair and no claim replicated.
func fabricatedZero(r StabilityReport, rate float64) bool {
	if r.Truncated {
		return true
	}
	return rate == 0 && r.Unassessed > 0
}

// brokenCount is how many adversarial passes located a defect (§5). A broken
// claim is the strongest single refine signal quarry produces.
func brokenCount(findings []AdversarialFinding) int {
	n := 0
	for _, f := range findings {
		if f.Broke {
			n++
		}
	}
	return n
}
