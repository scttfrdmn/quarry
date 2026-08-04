package quarry

import (
	"context"
	"encoding/json"
	"testing"
)

// These tests pin the provenance summary quarry emits into agate's ArtifactEvent
// (agate#265 C3). The field names/JSON tags MUST match agate's twin exactly — a
// rename here is a wire break there — so a test asserts the marshalled keys.

func TestProvenanceCountsVerifiedAndUnverified(t *testing.T) {
	yes, no := true, false
	rec := RunRecord{
		RunID:      "abc123",
		Unverified: []string{"n2", "n3"},
		Outcomes: []NodeOutcome{
			{NodeID: "n0", Verified: &yes},
			{NodeID: "n1", Verified: &no}, // checked-and-failed: not verified, not in Unverified
			{NodeID: "n2"},                // unverified
			{NodeID: "n3"},                // unverified
		},
	}
	p := ProvenanceOf(rec, nil)
	if p.RecordHash != "abc123" {
		t.Errorf("record hash must be the RunID, got %q", p.RecordHash)
	}
	if p.Verified != 1 {
		t.Errorf("one node passed, got %d", p.Verified)
	}
	if p.Unverified != 2 {
		t.Errorf("Unverified is the record's list, got %d", p.Unverified)
	}
}

func TestProvenanceStabilityUnknownForSingleRun(t *testing.T) {
	// A single run has no stability estimate — that needs replicates (§7, P7). The
	// summary must not fabricate a 0.0 that reads as "measured, and unstable".
	p := ProvenanceOf(RunRecord{RunID: "x"}, nil)
	if p.StabilityKnown {
		t.Error("a single run must not claim a known stability")
	}
	if p.Stability != 0 {
		t.Errorf("unknown stability must be zero-valued, got %f", p.Stability)
	}
}

func TestProvenanceStabilityFromReplicates(t *testing.T) {
	// With replicates, the fraction is real. Two records: one unanimous claim
	// (stable), one that disagrees (unstable) → rate 0.5.
	r1 := recWithClaims("Alpha.", "Beta.")
	r2 := recWithClaims("Alpha.", "Gamma.")
	report := Stability([]RunRecord{r1, r2}, nil, 0)
	p := ProvenanceOf(RunRecord{RunID: "x"}, &report)
	if !p.StabilityKnown {
		t.Fatal("replicates must yield a known stability")
	}
	if p.Stability < 0.33 || p.Stability > 0.34 {
		t.Errorf("1 of 3 claims stable → ~0.333, got %f", p.Stability)
	}
}

func TestProvenanceCountsAdversarialBreaks(t *testing.T) {
	rec := RunRecord{
		RunID: "x",
		Adversarial: []AdversarialFinding{
			{Broke: true}, {Broke: false}, {Broke: true},
		},
	}
	if got := ProvenanceOf(rec, nil).AdversarialFindings; got != 2 {
		t.Errorf("two refuted claims, got %d", got)
	}
}

func TestProvenanceWireTagsMatchAgateContract(t *testing.T) {
	// agate#265 C3: the JSON keys are the contract with agate's ArtifactEvent twin.
	// StabilityKnown is quarry-internal and must NOT appear on the wire.
	b, err := json.Marshal(Provenance{RecordHash: "h", Verified: 1, Unverified: 2,
		Stability: 0.5, AdversarialFindings: 3, StabilityKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, key := range []string{
		`"record_hash"`, `"verified"`, `"unverified"`, `"stability"`, `"adversarial_findings"`,
	} {
		if !contains(got, key) {
			t.Errorf("wire form missing agate key %s: %s", key, got)
		}
	}
	if contains(got, "StabilityKnown") || contains(got, "stability_known") {
		t.Errorf("internal StabilityKnown must not be on the wire: %s", got)
	}
}

// ------------------------------------- the unassessable-becomes-zero leak (§7, §8)

func TestUnassessableDoesNotBecomeAStabilityOfZero(t *testing.T) {
	// THE LEAK, found by probing ProvenanceOf against the new report fields. Two
	// replicates asserting the SAME conclusion in different words, free comparator:
	// 2 clusters, 0 stable, unassessed=1 — and StabilityRate() returns (0.0, true).
	// Emitting that badges "stability 0.0" in the SPA, which reads as MEASURED AND
	// NOTHING REPLICATED when the truth is that nobody could tell.
	//
	// The three-state distinction is kept all the way through the comparator seam and
	// the report; this asserts it survives the last hop to the artifact, which is
	// where silence would finally become a finding (§8).
	recs := []RunRecord{
		recOfClaims("prices rose in Q3"),
		recOfClaims("there was a third-quarter price increase"),
	}
	rep := StabilityWith(context.Background(), recs, MechanicalComparator{}, 0, nil)

	// Non-vacuity: the report must really be the ambiguous case, or this proves nothing.
	if rate, ok := rep.StabilityRate(); !ok || rate != 0 {
		t.Fatalf("fixture must produce a computable rate of 0; got %f ok=%v", rate, ok)
	}
	if rep.Unassessed == 0 {
		t.Fatal("fixture must have an unassessed comparison; the leak check is vacuous")
	}

	p := ProvenanceOf(RunRecord{RunID: "x"}, &rep)
	if p.StabilityKnown {
		t.Error("a rate of 0 that is really 'could not tell' must NOT be published as a " +
			"known stability — the SPA would read it as 'nothing replicated'")
	}
	if p.Unassessed != rep.Unassessed {
		t.Errorf("the summary must carry the unassessed count, got %d want %d",
			p.Unassessed, rep.Unassessed)
	}
	if !p.StabilityIsFloor {
		t.Error("an unassessed comparison makes any rate a floor, and the summary must say so")
	}
}

func TestATruncatedComparisonPassIsNotPublishedAsAStability(t *testing.T) {
	// A truncated pass is admittedly under-merged, so every number derived from it is
	// provisional — including a NON-zero rate, which is why truncation suppresses
	// independently of the rate's value.
	l := ledger(t, FromFloat(1))
	recs := []RunRecord{recOfClaims("a"), recOfClaims("b"), recOfClaims("c")}
	rep := StabilityWith(context.Background(), recs, newNear(FromFloat(10)), 0, l)
	if !rep.Truncated {
		t.Fatal("fixture must be truncated; the check is vacuous")
	}
	p := ProvenanceOf(RunRecord{RunID: "x"}, &rep)
	if p.StabilityKnown {
		t.Error("an incomplete clustering must not be published as a measured stability")
	}
	if !p.StabilityIsFloor {
		t.Error("a truncated pass under-merges, so its rate is a floor")
	}
}

func TestARealZeroStabilityIsStillPublished(t *testing.T) {
	// THE OTHER DIRECTION, and the reason the rule is narrow. "Nothing replicated" is
	// a genuine and important finding when the comparator was asked about every pair
	// and answered. Suppressing it too would hide the strongest possible instability
	// signal (§7) behind the same flag that exists to prevent overclaiming.
	r1 := recWithClaims("Alpha.")
	r2 := recWithClaims("Beta.")
	rep := Stability([]RunRecord{r1, r2}, nil, 0) // bool relation: every pair gets a verdict
	if rep.Unassessed != 0 {
		t.Fatalf("the free bool path asserts a verdict on every pair, got %d unassessed",
			rep.Unassessed)
	}
	rate, ok := rep.StabilityRate()
	if !ok || rate != 0 {
		t.Fatalf("fixture must be a real zero; got %f ok=%v", rate, ok)
	}
	p := ProvenanceOf(RunRecord{RunID: "x"}, &rep)
	if !p.StabilityKnown {
		t.Error("a zero reached by ASKING about every pair is a real finding and must be " +
			"published; suppressing it would hide the strongest instability signal")
	}
}

func TestProvenanceStillPublishesAFloorAboveZero(t *testing.T) {
	// The narrowness that matters most economically: under the free comparator almost
	// every multi-replicate report has unassessed pairs, so suppressing on that alone
	// would omit nearly every provenance object — and with it the perfectly
	// well-defined verified/unverified counts, which is the over-broad omission
	// agate#265 already flags as the cost of a non-nullable stability field.
	recs := []RunRecord{
		recOfClaims("shared conclusion", "only in run one"),
		recOfClaims("shared conclusion", "only in run two"),
	}
	rep := StabilityWith(context.Background(), recs, MechanicalComparator{}, 0, nil)
	rate, ok := rep.StabilityRate()
	if !ok || rate == 0 {
		t.Fatalf("fixture must yield a non-zero floor; got %f ok=%v", rate, ok)
	}
	if rep.Unassessed == 0 {
		t.Fatal("fixture must also have unassessed pairs, or it is not testing a FLOOR")
	}
	p := ProvenanceOf(RunRecord{RunID: "x"}, &rep)
	if !p.StabilityKnown {
		t.Error("a floor above zero has real support and must still be published")
	}
	if !p.StabilityIsFloor {
		t.Error("...but it must be labelled a floor, not a point estimate")
	}
}

func TestProvenanceNamesTheComparator(t *testing.T) {
	// A floor from string equality and a measurement from a model reach the wire as
	// the same bare float, so the summary has to carry the attribution even though
	// agate has no field for it yet (P8).
	r1 := recWithClaims("Alpha.")
	r2 := recWithClaims("Alpha.")
	rep := Stability([]RunRecord{r1, r2}, nil, 0)
	p := ProvenanceOf(RunRecord{RunID: "x"}, &rep)
	if p.ComparedBy == "" {
		t.Error("the summary must name what decided equivalence")
	}
	if p.ComparedBy != rep.ComparedBy {
		t.Errorf("attribution must match the report, got %q want %q", p.ComparedBy, rep.ComparedBy)
	}
}
