package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// These tests pin the CONTRACT the comparison parsing depends on — the three
// verdicts, order symmetry, and that cost is reported even with no verdict. They say
// nothing about whether the model's judgements are GOOD, which is not a property a
// test can hold a model to; live_test.go probes the real thing.

func comparatorWith(replies ...string) (*BedrockComparator, *recordingConverser) {
	rc := &recordingConverser{replies: replies, inTok: 80, outTok: 3}
	p := &BedrockProvider{
		Client: rc,
		Prices: map[string]Pricing{testModel: {InputPerMTok: 1, OutputPerMTok: 5}},
		Now:    func() time.Time { return time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC) },
	}
	return &BedrockComparator{Provider: p, Model: testModel, Ratio: 0.15}, rc
}

func cl(text string) quarry.Claim {
	return quarry.Claim{Text: text, Norm: quarry.NormalizeText(text)}
}

// ------------------------------------------------------------- the three verdicts

func TestComparatorReadsAllThreeVerdicts(t *testing.T) {
	// A bool cannot carry "I could not tell", which is the entire reason this seam
	// exists. UNSURE must reach the caller as ok=false, NOT as DIFFERENT: a declined
	// comparison counted as disagreement would inflate instability, and counted as
	// agreement would inflate stability (§7).
	cases := []struct {
		reply  string
		wantEq bool
		wantOK bool
	}{
		{"SAME", true, true},
		{"SAME - both say prices went up", true, true},
		{"DIFFERENT", false, true},
		{"different\nopposite conclusions", false, true},
		{"UNSURE", false, false},
		{"UNSURE - the second is ambiguous", false, false},
	}
	for _, c := range cases {
		bc, _ := comparatorWith(c.reply)
		eq, ok, cost := bc.Compare(context.Background(), cl("a"), cl("b"))
		if eq != c.wantEq || ok != c.wantOK {
			t.Errorf("reply %q: want eq=%v ok=%v, got eq=%v ok=%v", c.reply, c.wantEq, c.wantOK, eq, ok)
		}
		if cost <= 0 {
			t.Errorf("reply %q: a completed call must report its cost, got %s", c.reply, cost)
		}
	}
}

func TestUnsureIsAVerdictNotAParseFailure(t *testing.T) {
	// Both reach the caller as ok=false, but they are different events and the
	// distinction matters for debugging: UNSURE means the comparator worked and
	// declined; unparseable means the prompt or the model drifted.
	bc, _ := comparatorWith("UNSURE")
	if _, ok, _ := bc.Compare(context.Background(), cl("a"), cl("b")); ok {
		t.Error("UNSURE must be unassessable")
	}
	bc2, _ := comparatorWith("well, it depends on how you look at it")
	if _, ok, _ := bc2.Compare(context.Background(), cl("a"), cl("b")); ok {
		t.Error("an unparseable reply must be unassessable, never a guessed verdict")
	}
}

func TestComparatorDoesNotInferFromProse(t *testing.T) {
	// "these are the same" is NOT a verdict — inferring one from words is the guessing
	// the parser refuses to do, the same rule parseIndex follows for selection.
	for _, reply := range []string{
		"these two are the same conclusion",
		"I think they match",
		"yes",
	} {
		bc, _ := comparatorWith(reply)
		if eq, ok, _ := bc.Compare(context.Background(), cl("a"), cl("b")); eq || ok {
			t.Errorf("reply %q must not be read as a verdict, got eq=%v ok=%v", reply, eq, ok)
		}
	}
}

// -------------------------------------------------------------- order symmetry

func TestComparisonIsOrderSymmetric(t *testing.T) {
	// Equivalence is symmetric; a model asked "does A say the same as B" is not, since
	// the first claim frames the question. The pair is canonicalized so both argument
	// orders build an IDENTICAL prompt — which is what carries cluster.go's
	// order-independence guarantee down to the model itself.
	a, b := cl("zebra conclusion"), cl("aardvark conclusion")

	fwd, rcF := comparatorWith("SAME")
	fwd.Compare(context.Background(), a, b)
	rev, rcR := comparatorWith("SAME")
	rev.Compare(context.Background(), b, a)

	if len(rcF.prompts) != 1 || len(rcR.prompts) != 1 {
		t.Fatalf("want one prompt each, got %d and %d", len(rcF.prompts), len(rcR.prompts))
	}
	if rcF.prompts[0] != rcR.prompts[0] {
		t.Errorf("argument order changed the prompt:\n--- A,B ---\n%s\n--- B,A ---\n%s",
			rcF.prompts[0], rcR.prompts[0])
	}
	// Non-vacuity: the two claims must really differ, or an identical prompt is
	// guaranteed for a trivial reason.
	if a.Norm == b.Norm {
		t.Fatal("fixture claims are identical; the symmetry check is vacuous")
	}
	// And the canonical order must be the one it claims: smaller normalized form first.
	iA := strings.Index(rcF.prompts[0], a.Text)
	iB := strings.Index(rcF.prompts[0], b.Text)
	if iA < 0 || iB < 0 {
		t.Fatal("both claims must appear in the prompt")
	}
	if b.Norm < a.Norm && iB > iA {
		t.Error("canonicalization must put the smaller normalized form first")
	}
}

// ------------------------------------------------------------------- metering

func TestCostIsReportedWhenTheCallFails(t *testing.T) {
	// A transport fault yields no verdict. Whatever the provider reported is passed
	// through rather than assumed zero — the ledger must not be flattered.
	rc := &recordingConverser{err: errors.New("throttled")}
	p := &BedrockProvider{
		Client: rc,
		Prices: map[string]Pricing{testModel: {InputPerMTok: 1, OutputPerMTok: 5}},
		Now:    func() time.Time { return time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC) },
	}
	bc := &BedrockComparator{Provider: p, Model: testModel}
	eq, ok, _ := bc.Compare(context.Background(), cl("a"), cl("b"))
	if eq || ok {
		t.Error("a failed call is no verdict")
	}
}

func TestComparatorRefusesAnUnpinnedModel(t *testing.T) {
	// A stability number is attributed to the comparator that produced it, so "auto"
	// would make the attribution meaningless (P8) — the same refusal the provider makes.
	p := &BedrockProvider{}
	for _, model := range []string{"", "auto"} {
		if _, err := NewBedrockComparator(p, model); err == nil {
			t.Errorf("model %q must be refused (P8)", model)
		}
	}
	if _, err := NewBedrockComparator(nil, testModel); err == nil {
		t.Error("a comparator with no provider must be refused")
	}
	bc, err := NewBedrockComparator(p, testModel)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bc.Name(), testModel) {
		t.Errorf("Name must carry the pinned model version, got %q", bc.Name())
	}
}

// ------------------------------------------------------------------- the prompt

func TestPromptAsksAboutConclusionsNotQuality(t *testing.T) {
	// A comparator drawn into judging correctness would be doing the adversary's job
	// without the adversary's independence guarantee (§5), and its answers would then
	// depend on facts about the world rather than on the two strings — making
	// stability unreproducible for a reason no record could explain.
	bc, rc := comparatorWith("SAME")
	bc.Compare(context.Background(), cl("prices rose"), cl("prices increased"))
	prompt := rc.prompts[0]
	for _, want := range []string{"SAME", "DIFFERENT", "UNSURE"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt must offer the verdict %q", want)
		}
	}
	if !strings.Contains(prompt, "opposite") {
		t.Error("the prompt must name the opposite-conclusion case explicitly: it is the " +
			"error a similarity measure makes, and it inflates agreement")
	}
	lower := strings.ToLower(prompt)
	if !strings.Contains(lower, "do not judge whether either") {
		t.Error("the prompt must forbid judging truth or quality")
	}
}

// ------------------------------------------------- it satisfies the core's ladder

func TestBedrockComparatorSlotsIntoTheLadder(t *testing.T) {
	// The point of the whole exercise: the paid rung drops into quarry's ladder and a
	// free match still short-circuits it, so identical wordings cost nothing.
	bc, rc := comparatorWith("SAME")
	ladder := quarry.LadderComparator{Paid: bc}

	eq, ok, cost := ladder.Compare(context.Background(), cl("same claim"), cl("SAME   claim"))
	if !eq || !ok {
		t.Errorf("normalized-equal claims must match for free, got eq=%v ok=%v", eq, ok)
	}
	if len(rc.prompts) != 0 {
		t.Errorf("a free match must not reach the model, got %d calls", len(rc.prompts))
	}
	if cost != 0 {
		t.Errorf("a free match must cost nothing, got %s", cost)
	}

	// A paraphrase escalates and the paid rung resolves it — the undercount closed.
	eq, ok, cost = ladder.Compare(context.Background(),
		cl("prices rose in Q3"), cl("there was a third-quarter price increase"))
	if !eq || !ok {
		t.Errorf("the paid rung said SAME; the ladder must report it, got eq=%v ok=%v", eq, ok)
	}
	if len(rc.prompts) != 1 {
		t.Errorf("want exactly 1 paid call for a paraphrase, got %d", len(rc.prompts))
	}
	if cost <= 0 {
		t.Error("an escalated comparison must report what it cost")
	}
}
