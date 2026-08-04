package provider

import (
	"testing"

	quarry "github.com/scttfrdmn/quarry"
)

// LIVE tests for the claim comparator. Skipped unless QUARRY_LIVE is set; a few
// tenths of a cent. Run with:
//
//	QUARRY_LIVE=1 AWS_PROFILE=aws go test ./provider/ -run LiveComparator -v
//
// WHY THESE EXIST, same argument as live_agents_test.go: comparator_test.go verifies
// the parsing against recordingConverser, a double written by the same author as the
// prompt, which can confirm the code reads what it expects and says NOTHING about
// whether a model produces it. The one claim that matters here — that a real model
// distinguishes a PARAPHRASE from an OPPOSITE conclusion — is precisely the claim a
// compliant fake cannot make, because the fake returns whatever the fixture says.
//
// The paraphrase case is also the whole justification for the feature. If a live model
// cannot see that "prices rose in Q3" and "there was a third-quarter price increase"
// are the same conclusion, then the paid rung buys nothing over the free one and the
// undercount stands.

// TestLiveComparatorSeesAParaphrase is the load-bearing one: the mechanical
// comparator CANNOT match these (different normalized forms) and the model should.
// This is the undercount §13 names, closed or not closed, measured on a real call.
func TestLiveComparatorSeesAParaphrase(t *testing.T) {
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bc, err := NewBedrockComparator(prov, model)
	if err != nil {
		t.Fatal(err)
	}
	a := quarry.Claim{Text: "Prices rose in Q3.", Norm: quarry.NormalizeText("Prices rose in Q3.")}
	b := quarry.Claim{Text: "There was a third-quarter price increase.",
		Norm: quarry.NormalizeText("There was a third-quarter price increase.")}

	// The premise: the free rung really cannot judge this pair, so the paid rung is
	// doing work rather than duplicating it.
	if _, ok, _ := (quarry.MechanicalComparator{}).Compare(ctx, a, b); ok {
		t.Fatal("the mechanical comparator can already judge this pair; the test is vacuous")
	}

	eq, ok, cost := bc.Compare(ctx, a, b)
	t.Logf("paraphrase: eq=%v ok=%v cost=%s", eq, ok, cost)
	if !ok {
		// Soft: one sample of a stochastic call is not evidence (P7), and an UNSURE is a
		// legitimate answer. But it means the feature bought nothing on this pair.
		t.Errorf("the model declined a clear paraphrase — the paid rung is not closing the "+
			"undercount on this pair (eq=%v)", eq)
		return
	}
	if !eq {
		t.Errorf("a real model should read these as the same conclusion; got DIFFERENT")
	}
	if cost <= 0 {
		t.Error("a live call must report a non-zero cost")
	}
}

// TestLiveComparatorRejectsAnOppositeConclusion is the error that INFLATES agreement,
// which is the one direction §7 must not be wrong in. "Prices rose" and "prices fell"
// are lexically and semantically close — a naive embedding similarity scores them
// high — and they are opposite conclusions.
func TestLiveComparatorRejectsAnOppositeConclusion(t *testing.T) {
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bc, err := NewBedrockComparator(prov, model)
	if err != nil {
		t.Fatal(err)
	}
	a := quarry.Claim{Text: "Prices rose in Q3.", Norm: quarry.NormalizeText("Prices rose in Q3.")}
	b := quarry.Claim{Text: "Prices fell in Q3.", Norm: quarry.NormalizeText("Prices fell in Q3.")}

	eq, ok, cost := bc.Compare(ctx, a, b)
	t.Logf("opposite: eq=%v ok=%v cost=%s", eq, ok, cost)
	if ok && eq {
		t.Error("OPPOSITE conclusions reported as the same claim — this inflates agreement " +
			"and would report disagreeing replicates as unanimous (§7)")
	}
}

// TestLiveComparatorIsOrderSymmetricInPractice checks the property the canonicalizing
// code exists to guarantee, end to end. Because the prompt is canonicalized the two
// calls are byte-identical, so this is really a test that the guarantee holds against
// a real endpoint rather than only in the prompt builder.
func TestLiveComparatorIsOrderSymmetric(t *testing.T) {
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bc, err := NewBedrockComparator(prov, model)
	if err != nil {
		t.Fatal(err)
	}
	a := quarry.Claim{Text: "The reaction is exothermic.", Norm: quarry.NormalizeText("The reaction is exothermic.")}
	b := quarry.Claim{Text: "The reaction releases heat.", Norm: quarry.NormalizeText("The reaction releases heat.")}

	eqAB, okAB, _ := bc.Compare(ctx, a, b)
	eqBA, okBA, _ := bc.Compare(ctx, b, a)
	t.Logf("A,B -> eq=%v ok=%v | B,A -> eq=%v ok=%v", eqAB, okAB, eqBA, okBA)
	if eqAB != eqBA || okAB != okBA {
		t.Errorf("comparison is not order-symmetric against a live model: "+
			"(A,B)=(%v,%v) but (B,A)=(%v,%v)", eqAB, okAB, eqBA, okBA)
	}
}

// TestLiveStabilityWithARealComparator runs the whole §7 path against a real model:
// three synthetic replicates, one of which words its conclusion differently and one
// of which disagrees. The right answer is one stable cluster and one unstable claim —
// which the FREE comparator cannot produce, because it cannot see the paraphrase.
func TestLiveStabilityWithARealComparator(t *testing.T) {
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bc, err := NewBedrockComparator(prov, model)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(text string) quarry.RunRecord {
		return quarry.RunRecord{Outcomes: []quarry.NodeOutcome{{NodeID: "n0",
			Claims: []quarry.Claim{{Text: text, Norm: quarry.NormalizeText(text)}}}}}
	}
	recs := []quarry.RunRecord{
		mk("The treatment reduced mortality."),
		mk("Mortality was lowered by the treatment."), // same conclusion, different words
		mk("The treatment had no effect on mortality."),
	}

	l, err := quarry.NewLedger(quarry.Caps{Spend: quarry.FromFloat(1)}, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	ladder := quarry.LadderComparator{Paid: bc}
	rep := quarry.StabilityWith(ctx, recs, ladder, 2, l)

	t.Logf("comparedBy=%s clusters=%d stable=%d unstable=%d unassessed=%d cost=%s truncated=%v",
		rep.ComparedBy, len(rep.Claims), len(rep.Stable()), len(rep.Unstable()),
		rep.Unassessed, rep.ComparisonCost, rep.Truncated)
	for _, c := range rep.Claims {
		t.Logf("  %q support=%d/%d stable=%v", c.Claim.Text, c.Support, c.Total, c.Stable())
	}

	if rep.Truncated {
		t.Fatal("the comparison pass ran out of budget; raise the cap for this test")
	}
	if rep.ComparisonCost <= 0 {
		t.Error("a live comparison pass must report what it spent (P4)")
	}
	// The claim that matters: the paraphrase clustered, so a 2-of-3 majority exists.
	// Soft-failed, because it depends on one stochastic judgement per pair (P7).
	if len(rep.Stable()) == 0 {
		t.Errorf("expected the paraphrase pair to cluster into a 2-of-3 majority; " +
			"got nothing stable, so the paid rung did not close the undercount here")
	}
	if len(rep.Claims) == 3 {
		t.Errorf("three clusters means NOTHING merged — the same answer the free " +
			"comparator gives, so the paid rung bought nothing")
	}
}

// TestLiveComparatorRespectsTheCap pins P4 against a real endpoint: a cap too small
// for the comparisons must stop the pass and SAY it stopped, not spend past it.
//
// THIS TEST FOUND A REAL DEFECT, and its first version would not have. It asserted
// only that Truncated was reported, and the first implementation reported it
// perfectly honestly AFTER spending 200 micro-units against a 1-micro-unit cap —
// because it called Compare and then debited, which consults the cap once the money
// is gone. The assertion now covers the SPEND, since disclosing an overrun is not the
// same as not overrunning (P4: the cap is the contract, not a target).
func TestLiveComparatorRespectsTheCap(t *testing.T) {
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bc, err := NewBedrockComparator(prov, model)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(text string) quarry.RunRecord {
		return quarry.RunRecord{Outcomes: []quarry.NodeOutcome{{NodeID: "n0",
			Claims: []quarry.Claim{{Text: text, Norm: quarry.NormalizeText(text)}}}}}
	}
	recs := []quarry.RunRecord{
		mk("Alpha is the cause."), mk("Beta is the cause."), mk("Gamma is the cause."),
	}
	// One micro-unit: enough to admit nothing.
	const cap = quarry.Units(1)
	l, err := quarry.NewLedger(quarry.Caps{Spend: cap}, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	ladder := quarry.LadderComparator{Paid: bc}
	// Non-vacuity: a real comparison must genuinely cost more than the cap, or the
	// pass would finish and there would be nothing to truncate.
	est := ladder.Estimate(quarry.Claim{Text: "x"}, quarry.Claim{Text: "y"})
	if est <= cap {
		t.Fatalf("estimate %s is within the cap %s; the truncation check is vacuous", est, cap)
	}

	rep := quarry.StabilityWith(ctx, recs, ladder, 2, l)
	t.Logf("cost=%s truncated=%v clusters=%d balance=%s",
		rep.ComparisonCost, rep.Truncated, len(rep.Claims), l.Balance())
	if !rep.Truncated {
		t.Error("a cap that cannot fund the comparisons must truncate the pass and disclose it")
	}
	if rep.ComparisonCost > cap {
		t.Errorf("spent %s against a cap of %s — a live overrun, not a reporting problem (P4)",
			rep.ComparisonCost, cap)
	}
	if l.Balance() < 0 {
		t.Errorf("the ledger went negative against a real endpoint: %s", l.Balance())
	}
}
