package quarry

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// These tests ARE the specification for build step 6 (§7 claim-level
// equivalence). A failing test means the design changed — amend docs/design.md
// in the same commit or revert. They pin the invariants of the MECHANICAL spike;
// semantic equivalence is explicitly out of scope and marked TODO(§7) in claim.go.

var extractor = MechanicalExtractor{}

// ------------------------------------------------------------- extraction

func TestExtractOneClaimPerSentence(t *testing.T) {
	got, err := extractor.Extract(context.Background(),
		Sample{Content: "The sky is blue. Water is wet. Fire is hot."}, "n0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 claims from 3 sentences, got %d: %v", len(got), got)
	}
	for _, c := range got {
		if c.NodeID != "n0" {
			t.Errorf("every claim traces to its node, got %q", c.NodeID)
		}
		if c.Norm == "" {
			t.Errorf("every claim pins its normalized form (P8), got empty for %q", c.Text)
		}
	}
}

func TestExtractSkipsEmptyAndPunctuationOnlySegments(t *testing.T) {
	// Trailing terminators, blank lines and a pure-punctuation segment assert
	// nothing and must not become claims.
	got, err := extractor.Extract(context.Background(),
		Sample{Content: "Real claim.\n\n...\n   \nAnother one!"}, "n0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 real claims, got %d: %v", len(got), got)
	}
}

func TestExtractDedupesWithinOneResult(t *testing.T) {
	// A claim repeated (modulo case/punctuation) inside one result is one claim,
	// not two — the DAG spirit at the claim level (§2).
	got, err := extractor.Extract(context.Background(),
		Sample{Content: "Prices rose. PRICES ROSE! Prices fell."}, "n0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 distinct claims after dedup, got %d: %v", len(got), got)
	}
}

func TestExtractEmptyContentYieldsNoClaims(t *testing.T) {
	got, err := extractor.Extract(context.Background(), Sample{Content: ""}, "n0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty content asserts nothing, got %d claims", len(got))
	}
}

// ------------------------------------------------------------- equivalence

func TestEquivalentIgnoresCasePunctuationWhitespace(t *testing.T) {
	a := Claim{Text: "The sky is blue."}
	b := Claim{Text: "the   SKY is BLUE"}
	if !extractor.Equivalent(a, b) {
		t.Error("normalization must fold case, punctuation and whitespace")
	}
}

func TestEquivalentIsWordOrderSensitive(t *testing.T) {
	// Meaning depends on order: "dog bites man" is not "man bites dog". The spike
	// deliberately does not sort tokens away.
	a := Claim{Text: "dog bites man"}
	b := Claim{Text: "man bites dog"}
	if extractor.Equivalent(a, b) {
		t.Error("word order carries meaning; these must not be equivalent")
	}
}

func TestEquivalentEmptyClaimsNeverMatch(t *testing.T) {
	// Two claims that normalize to nothing have no assertion to agree on.
	a := Claim{Text: "..."}
	b := Claim{Text: "!!!"}
	if extractor.Equivalent(a, b) {
		t.Error("empty-normalized claims must never be equivalent")
	}
}

func TestEquivalentUsesPinnedNormOverText(t *testing.T) {
	// A recorded claim carries the normalized form that produced it. Equivalence
	// compares that pinned form, so a normalizer change later cannot retroactively
	// alter what two archived claims meant (P8). Here Norm and Text disagree on
	// purpose: the pinned Norm wins.
	a := Claim{Text: "totally different words", Norm: "shared"}
	b := Claim{Text: "and these too", Norm: "shared"}
	if !extractor.Equivalent(a, b) {
		t.Error("equivalence must compare pinned Norm when present, not re-derive from Text")
	}
}

// ---------------------------------------------------- determinism / purity

func TestExtractIsDeterministic(t *testing.T) {
	// Extraction runs during replay too; identical input must give byte-identical
	// claims or the record diverges (P8).
	s := Sample{Content: "One. Two. Three."}
	a, _ := extractor.Extract(context.Background(), s, "n0")
	b, _ := extractor.Extract(context.Background(), s, "n0")
	if len(a) != len(b) {
		t.Fatalf("nondeterministic claim count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Text != b[i].Text || a[i].Norm != b[i].Norm || a[i].NodeID != b[i].NodeID {
			t.Errorf("claim %d differs across extractions: %v vs %v", i, a[i], b[i])
		}
	}
}

// ------------------------------------------------- wired into the executor

// claimRunFor builds the same small tree as record_test's runFor but with the
// mechanical extractor wired, and returns the record.
func claimRunFor(t *testing.T, prov Provider) (RunRecord, Result) {
	t.Helper()
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		Planner:   StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:    ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:   ConcatReducer{Sep: "|"},
		Extractor: MechanicalExtractor{},
		Now:       now,
		MaxDepth:  2,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, root, caps, ModeFresh), res
}

func TestExecutorPopulatesClaims(t *testing.T) {
	// Wiring check: with an extractor set, content-bearing nodes carry claims, so
	// CostPerVerifiedClaim has real input.
	_, res := claimRunFor(t, &fakeProvider{cost: FromFloat(1)})
	var withClaims int
	for _, o := range res.Outcomes {
		if o.Content != "" && len(o.Claims) == 0 {
			t.Errorf("node %s has content but no claims", o.NodeID)
		}
		if len(o.Claims) > 0 {
			withClaims++
		}
	}
	if withClaims == 0 {
		t.Fatal("expected at least one node to carry extracted claims")
	}
}

func TestClaimsDoNotBreakReplayDeterminism(t *testing.T) {
	// Step 5's byte-for-byte guarantee must survive claims in the record: extract
	// is pure, so run and replay serialize identically even with Claims populated
	// (P8). This is the step-5 invariant re-asserted with the step-6 field live.
	orig, _ := claimRunFor(t, &fakeProvider{cost: FromFloat(3)})
	replayed, _ := claimRunFor(t, NewRecordedProvider(orig))

	ob, err := orig.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := replayed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, rb) {
		t.Fatalf("claims must not break byte-for-byte replay\n orig: %s\n rep:  %s", ob, rb)
	}
	if orig.RunID != replayed.RunID {
		t.Errorf("identical content must hash identically: %s vs %s", orig.RunID, replayed.RunID)
	}
}

func TestCostPerVerifiedClaimUsesExtractedClaims(t *testing.T) {
	// The whole point of wiring extraction now: the quality denominator becomes
	// real. A verified leaf with extracted claims makes CostPerVerifiedClaim
	// computable (§8.2).
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, _ := NewLedger(caps, Scope{})
	e := &Executor{
		Planner:   DeclinePlanner{},
		Solver:    ProviderSolver{Provider: &fakeProvider{cost: FromFloat(1)}, Model: "fake"},
		Reducer:   ConcatReducer{},
		Extractor: MechanicalExtractor{},
		Verifier:  NonEmptyOracle(),
		Now:       now,
		MaxDepth:  1,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRunRecord(res, root, caps, ModeFresh)
	if _, ok := rec.CostPerVerifiedClaim(); !ok {
		t.Fatal("a verified leaf with extracted claims must yield CostPerVerifiedClaim")
	}
}
