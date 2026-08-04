package quarry

import (
	"context"
	"testing"
)

// These tests ARE the specification for metered, three-state claim comparison and
// order-independent clustering (§7, §12). A failing test means the design changed —
// amend docs/design.md in the same commit or revert.

// ---------------------------------------------------------- fixtures & doubles

func claimOf(text string) Claim {
	return Claim{Text: text, Norm: NormalizeText(text)}
}

func recOfClaims(texts ...string) RunRecord {
	var cs []Claim
	for _, t := range texts {
		cs = append(cs, claimOf(t))
	}
	return RunRecord{Outcomes: []NodeOutcome{{NodeID: "n0", Claims: cs}}}
}

// nearComparator is a NON-TRANSITIVE paid comparator. Every model-backed comparator
// is non-transitive, so the double exhibits the property that matters rather than a
// convenient one.
//
// TWO SHAPES, and the difference between them is not academic — it is the difference
// between a test that proves something and one that does not.
//
//	CHAIN (newNear):    a~b, b~c, a!~c   — "rose sharply"~"rose", "rose"~"did not
//	                                        fall", but not the outer pair.
//	HUB   (newHub):     a~b, a~c, b!~c   — "rose" is near both "rose sharply" and
//	                                        "rose slightly"; those two are not near
//	                                        each other.
//
// Only the HUB distinguishes single-link from complete-link clustering. Under the
// chain, the claim that rejects is also the canonical representative, so comparing
// against the representative alone happens to reach the same answer — a probe
// confirmed single-link passes the chain test. Under the hub, single-link merges b
// and c through a and reports THREE REPLICATES UNANIMOUSLY AGREEING on a claim the
// comparator explicitly said b and c do not share.
type nearComparator struct {
	near     map[string]map[string]bool
	unit     Units
	calls    int
	assessed map[string]bool // pairs it refuses to judge, keyed "a|b"
}

func newNear(unit Units) *nearComparator {
	return &nearComparator{
		near: map[string]map[string]bool{
			"a": {"b": true},
			"b": {"a": true, "c": true},
			"c": {"b": true},
		},
		unit:     unit,
		assessed: map[string]bool{},
	}
}

// newHub is the shape that actually discriminates the clustering algorithm.
func newHub(unit Units) *nearComparator {
	return &nearComparator{
		near: map[string]map[string]bool{
			"a": {"b": true, "c": true},
			"b": {"a": true},
			"c": {"a": true},
		},
		unit:     unit,
		assessed: map[string]bool{},
	}
}

func (n *nearComparator) Name() string       { return "near" }
func (n *nearComparator) CostRatio() float64 { return 0.25 }

// Estimate returns the EXACT unit price, so admission is neither generous nor
// stingy and the cap tests measure the clustering's behaviour rather than a
// mis-estimate. A real comparator over-estimates; a double that did would make
// "stopped at the cap" ambiguous between the two.
func (n *nearComparator) Estimate(_, _ Claim) Units { return n.unit }

func (n *nearComparator) Compare(_ context.Context, a, b Claim) (bool, bool, Units) {
	n.calls++
	if n.assessed[a.Text+"|"+b.Text] || n.assessed[b.Text+"|"+a.Text] {
		return false, false, n.unit // billed, and could not tell — both must be reported
	}
	if a.Text == b.Text {
		return true, true, n.unit
	}
	return n.near[a.Text][b.Text], true, n.unit
}

// ------------------------------------------------- the order-independence defect

func TestClusteringIsIndependentOfReplicateOrder(t *testing.T) {
	// THE KEYSTONE TEST. Replicates are exchangeable draws (P7), so the order they
	// are passed in is an artifact of iteration and must carry NO information.
	//
	// Before complete-link clustering this failed loudly and in the DANGEROUS
	// direction: with a~b, b~c, a!~c, the order [b a c] chained all three into one
	// cluster and reported UNANIMOUS AGREEMENT that no comparator ever asserted,
	// while [a b c] and [c b a] reported nothing stable.
	orders := [][]string{
		{"a", "b", "c"},
		{"b", "a", "c"},
		{"c", "b", "a"},
		{"c", "a", "b"},
		{"b", "c", "a"},
		{"a", "c", "b"},
	}
	// Both non-transitive shapes: order-independence has to hold for either, and they
	// fail differently under a naive algorithm (see nearComparator).
	shapes := map[string]func(Units) *nearComparator{"chain": newNear, "hub": newHub}
	for name, mk := range shapes {
		var first, firstOrder string
		for _, order := range orders {
			recs := make([]RunRecord, 0, len(order))
			for _, s := range order {
				recs = append(recs, recOfClaims(s))
			}
			// A ledger is REQUIRED here: nearComparator has a non-zero CostRatio, and a paid
			// comparator with no ledger is refused outright (P4). Passing nil made an earlier
			// draft of this test vacuous — nothing was compared, so order-independence held
			// trivially. Verified by probe: with nil, single-link clustering passes too.
			rep := StabilityWith(context.Background(), recs, mk(0), 0, ledger(t, FromFloat(100)))

			// The whole verdict, rendered: how many clusters, which claim represents each,
			// and what support it drew. Any of the three shifting is a changed conclusion.
			got := itoa(len(rep.Claims)) + " clusters, " + itoa(len(rep.Stable())) + " stable: "
			for _, c := range rep.Claims {
				got += c.Claim.Text + "=" + itoa(c.Support) + "/" + itoa(c.Total) + " "
			}
			if first == "" {
				first, firstOrder = got, orderStr(order)
				continue
			}
			if got != first {
				t.Errorf("%s: replicate order changed the verdict\n  order %s -> %s\n  order %v -> %s",
					name, firstOrder, first, order, got)
			}
		}
		// Non-vacuity: each fixture must really be non-transitive, or the test only
		// re-proves that equality is transitive.
		n := mk(0)
		ab, _, _ := n.Compare(context.Background(), claimOf("a"), claimOf("b"))
		bc, _, _ := n.Compare(context.Background(), claimOf("b"), claimOf("c"))
		ac, _, _ := n.Compare(context.Background(), claimOf("a"), claimOf("c"))
		if ab && bc && ac {
			t.Fatalf("%s fixture is transitive (a~b=%v b~c=%v a~c=%v); the test is vacuous",
				name, ab, bc, ac)
		}
	}
}

// orderStr renders an order for a failure message without importing fmt.
func orderStr(order []string) string {
	out := " ["
	for i, s := range order {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out + "]"
}

func TestCompleteLinkDoesNotMergeThroughAnIntermediate(t *testing.T) {
	// THE TEST THAT DISCRIMINATES THE ALGORITHM, and it took a second fixture to get
	// right: the chain shape passes under single-link too (probed and confirmed),
	// because there the rejecting claim is also the representative. The HUB is the
	// shape that separates them.
	//
	// With a~b, a~c, b!~c and one claim per replicate, a cluster of support 3 is
	// precisely a cluster containing both b and c — a pair the comparator was ASKED
	// about and said no to. Single-link reports that as unanimous agreement.
	byReplicate := [][]Claim{{claimOf("a")}, {claimOf("b")}, {claimOf("c")}}
	hub := newHub(0)
	// Ledger required — see the note in TestClusteringIsIndependentOfReplicateOrder.
	clusters, _, _, _ := ClusterClaims(context.Background(), byReplicate, hub, ledger(t, FromFloat(100)))
	for _, c := range clusters {
		if c.Support() == 3 {
			t.Errorf("b and c are NOT equivalent yet share a cluster (support 3/3, members %v) — "+
				"single-link merging through the hub a, reported as unanimous agreement",
				claimTexts(c.Members))
		}
	}
	if len(clusters) < 2 {
		t.Errorf("want at least 2 clusters for a hub triple, got %d", len(clusters))
	}
	// Non-vacuity: the fixture must really be a hub, or this proves nothing.
	ab, _, _ := hub.Compare(context.Background(), claimOf("a"), claimOf("b"))
	ac, _, _ := hub.Compare(context.Background(), claimOf("a"), claimOf("c"))
	bc, _, _ := hub.Compare(context.Background(), claimOf("b"), claimOf("c"))
	if !ab || !ac || bc {
		t.Fatalf("fixture is not a hub (a~b=%v a~c=%v b~c=%v); the test is vacuous", ab, ac, bc)
	}
}

func TestClusterRepresentativeIsCanonicalNotFirstSeen(t *testing.T) {
	// The representative is what a refine reads and a receipt cites, so it must not
	// depend on which replicate happened to be walked first.
	r1 := recOfClaims("zebra conclusion")
	r2 := recOfClaims("zebra conclusion")
	fwd := StabilityWith(context.Background(), []RunRecord{r1, r2}, nil, 0, nil)
	rev := StabilityWith(context.Background(), []RunRecord{r2, r1}, nil, 0, nil)
	if len(fwd.Claims) != 1 || len(rev.Claims) != 1 {
		t.Fatalf("want one cluster each, got %d and %d", len(fwd.Claims), len(rev.Claims))
	}
	if fwd.Claims[0].Claim.Text != rev.Claims[0].Claim.Text {
		t.Errorf("representative depends on order: %q vs %q",
			fwd.Claims[0].Claim.Text, rev.Claims[0].Claim.Text)
	}
}

// ------------------------------------------------------------- the three states

func TestMechanicalComparatorReportsUnassessableNotDisagreement(t *testing.T) {
	// The free rung is SOUND but INCOMPLETE: equal norms mean equal claims, unequal
	// norms mean it cannot tell. Reporting a non-match as a disagreement is the
	// overstatement claim.go's TODO warns about.
	m := MechanicalComparator{}
	eq, ok, cost := m.Compare(context.Background(), claimOf("The sky is blue."), claimOf("the   SKY is BLUE"))
	if !eq || !ok {
		t.Errorf("equal normalized forms must be a confident match, got eq=%v ok=%v", eq, ok)
	}
	if cost != 0 {
		t.Errorf("the free rung must cost nothing, got %s", cost)
	}
	eq, ok, _ = m.Compare(context.Background(), claimOf("prices rose in Q3"), claimOf("there was a third-quarter price increase"))
	if eq {
		t.Error("mechanical comparison must not claim a paraphrase matches")
	}
	if ok {
		t.Error("a differing wording is UNASSESSABLE mechanically, not a disagreement — " +
			"ok must be false so the report counts it as unassessed rather than unstable")
	}
}

func TestUnassessedIsCountedSeparatelyFromInstability(t *testing.T) {
	// "We could not tell" must not be laundered into either agreement or
	// disagreement. Under the free comparator every differing wording is unassessed.
	recs := []RunRecord{
		recOfClaims("prices rose in Q3"),
		recOfClaims("there was a third-quarter price increase"),
	}
	rep := StabilityWith(context.Background(), recs, MechanicalComparator{}, 0, nil)
	if rep.Unassessed == 0 {
		t.Error("a paraphrase pair under the mechanical comparator must be recorded as " +
			"unassessed, not silently as two independent claims")
	}
	if rep.ComparedBy != "mechanical" {
		t.Errorf("the report must name its comparator, got %q", rep.ComparedBy)
	}
}

func TestFuncComparatorCanAbstain(t *testing.T) {
	// A domain oracle outside its competence must abstain, not guess — the same
	// distinction FuncVerifier.Applies draws for verification (§8).
	f := FuncComparator{
		Label:  "numeric",
		Eq:     func(a, b Claim) bool { return true },
		Assess: func(a, b Claim) bool { return a.Text == "n" },
	}
	if _, ok, _ := f.Compare(context.Background(), claimOf("x"), claimOf("y")); ok {
		t.Error("a comparator outside its competence must report ok=false")
	}
	if _, ok, _ := f.Compare(context.Background(), claimOf("n"), claimOf("y")); !ok {
		t.Error("an applicable comparison must be assessable")
	}
}

// --------------------------------------------------------------- the ladder

func TestLadderDoesNotPayToConfirmAFreeMatch(t *testing.T) {
	// Normalized equality is a SOUND sufficient condition, so a paid call after a
	// free match buys nothing and every micro-unit it spends is waste. This is the
	// whole cost argument for the design.
	paid := newNear(FromFloat(1))
	l := LadderComparator{Paid: paid}
	eq, ok, cost := l.Compare(context.Background(), claimOf("same claim"), claimOf("SAME   claim"))
	if !eq || !ok {
		t.Fatalf("free match must stand, got eq=%v ok=%v", eq, ok)
	}
	if paid.calls != 0 {
		t.Errorf("the paid rung must not run after a free match, got %d calls", paid.calls)
	}
	if cost != 0 {
		t.Errorf("a free match must cost nothing, got %s", cost)
	}
}

func TestLadderEscalatesAFreeNonMatch(t *testing.T) {
	// A free non-match is NOT a verdict, so it must escalate rather than be reported.
	paid := newNear(FromFloat(1))
	l := LadderComparator{Paid: paid}
	eq, ok, cost := l.Compare(context.Background(), claimOf("a"), claimOf("b"))
	if !eq || !ok {
		t.Errorf("the paid rung says a~b; the ladder must report it, got eq=%v ok=%v", eq, ok)
	}
	if paid.calls != 1 {
		t.Errorf("want exactly 1 paid call on a free non-match, got %d", paid.calls)
	}
	if cost != FromFloat(1) {
		t.Errorf("the ladder must report the paid rung's cost, got %s", cost)
	}
}

func TestLadderWithNoPaidRungStaysHonest(t *testing.T) {
	// Nil Paid must not silently become a total relation.
	l := LadderComparator{}
	if _, ok, _ := l.Compare(context.Background(), claimOf("a"), claimOf("b")); ok {
		t.Error("with no paid rung a differing wording must remain unassessed")
	}
	if l.CostRatio() != 0 {
		t.Errorf("a mechanical ladder costs 0, got %f", l.CostRatio())
	}
}

func TestComparisonCostIsReportedEvenWhenUnassessable(t *testing.T) {
	// A refused or unparseable model reply was still billed. Hiding it would make the
	// ledger wrong in the flattering direction — the rule RunSurplus already follows.
	paid := newNear(FromFloat(2))
	paid.assessed["a|b"] = true
	l := LadderComparator{Paid: paid}
	_, ok, cost := l.Compare(context.Background(), claimOf("a"), claimOf("b"))
	if ok {
		t.Fatal("fixture must refuse this pair")
	}
	if cost != FromFloat(2) {
		t.Errorf("an unassessable paid comparison must still report its cost, got %s", cost)
	}
}

// ------------------------------------------------------------------- metering

func TestPaidComparisonSpendsAgainstTheLedger(t *testing.T) {
	// Comparison is spend like any other (P4): it passes through the ledger so it is
	// inside the cap, not beside it.
	l := ledger(t, FromFloat(100))
	before := l.Balance()
	recs := []RunRecord{recOfClaims("a"), recOfClaims("b")}
	rep := StabilityWith(context.Background(), recs, newNear(FromFloat(3)), 0, l)
	if rep.ComparisonCost == 0 {
		t.Error("a paid comparison pass must report what it spent")
	}
	if l.Balance() >= before {
		t.Errorf("comparison must debit the ledger: balance %s -> %s", before, l.Balance())
	}
	if l.Balance() != before-rep.ComparisonCost {
		t.Errorf("the debit must equal the reported cost: %s spent, balance moved %s",
			rep.ComparisonCost, before-l.Balance())
	}
}

func TestComparisonStopsAtTheCapAndSaysItWasTruncated(t *testing.T) {
	// You can stop spending. What you must not do is present a partial clustering as
	// a complete one — an under-merged report that says it is under-merged is a real
	// result; one that stays silent is a lie about agreement.
	l := ledger(t, FromFloat(1))
	recs := []RunRecord{recOfClaims("a"), recOfClaims("b"), recOfClaims("c")}
	rep := StabilityWith(context.Background(), recs, newNear(FromFloat(10)), 0, l)
	if !rep.Truncated {
		t.Error("a comparison pass that ran out of budget must report Truncated")
	}
}

func TestComparisonNeverSpendsPastTheCap(t *testing.T) {
	// THE CAP IS THE CONTRACT, NOT A TARGET (P4). This test exists because the one
	// above did not catch a real overrun: it asserted only that Truncated was
	// reported, and an implementation that calls Compare and THEN debits reports
	// Truncated perfectly honestly after the money is gone. A LIVE run against a
	// 1-micro-unit cap spent 200 that way.
	//
	// Disclosing an overrun is not the same as not overrunning, so the assertion has
	// to be about the SPEND, not about the flag.
	cap := FromFloat(1)
	l := ledger(t, cap)
	recs := []RunRecord{recOfClaims("a"), recOfClaims("b"), recOfClaims("c")}
	// Each comparison costs 10× the entire cap, so a correct implementation makes
	// ZERO paid comparisons — admission refuses the first one.
	paid := newNear(FromFloat(10))
	rep := StabilityWith(context.Background(), recs, paid, 0, l)

	if rep.ComparisonCost > cap {
		t.Errorf("spent %s against a cap of %s — the cap is the contract (P4)",
			rep.ComparisonCost, cap)
	}
	if paid.calls != 0 {
		t.Errorf("no comparison is affordable at this cap, so none may be MADE; got %d calls "+
			"(admission must precede the call, not follow it)", paid.calls)
	}
	if l.Balance() < 0 {
		t.Errorf("the ledger went negative: %s", l.Balance())
	}
	// Non-vacuity: the fixture must really be unaffordable, or this passes for the
	// wrong reason.
	if paid.Estimate(claimOf("a"), claimOf("b")) <= cap {
		t.Fatal("fixture comparison is affordable; the overrun check is vacuous")
	}
}

func TestAnAffordableComparisonStillHappens(t *testing.T) {
	// The guard above must not become a blanket refusal. Admission that rejects
	// everything would trivially satisfy "never spends past the cap" while making the
	// paid rung dead code — and the free path would hide it, since the free rung
	// clusters identical wordings with no comparator call at all.
	l := ledger(t, FromFloat(100))
	recs := []RunRecord{recOfClaims("a"), recOfClaims("b")}
	paid := newNear(FromFloat(1))
	rep := StabilityWith(context.Background(), recs, paid, 0, l)
	if paid.calls == 0 {
		t.Error("an affordable comparison must actually run")
	}
	if rep.Truncated {
		t.Error("an affordable pass must not report Truncated")
	}
	if rep.ComparisonCost == 0 {
		t.Error("a paid comparison must report its spend")
	}
}

func TestAPaidComparatorRefusesToRunWithoutALedger(t *testing.T) {
	// A paid comparator with no ledger would bill outside every cap (P4). Refusing
	// and reporting Truncated is the only honest option: silently spending is a cap
	// violation, and silently not spending would look like a completed pass.
	paid := newNear(FromFloat(5))
	recs := []RunRecord{recOfClaims("a"), recOfClaims("b")}
	rep := StabilityWith(context.Background(), recs, paid, 0, nil)
	if paid.calls != 0 {
		t.Errorf("a paid comparator must not spend with no ledger, got %d calls", paid.calls)
	}
	if !rep.Truncated {
		t.Error("refusing to spend must be disclosed as Truncated, not look like a full pass")
	}
}

// ------------------------------------------------------ the free path is intact

func TestStabilityFreePathIsUnchangedAndNamesItself(t *testing.T) {
	// The bool relation keeps its old meaning: a total relation where a non-match IS
	// a disagreement. That is documented as undercounting, and the report now says
	// which comparator produced the number so it cannot be confused with a measured
	// one (P8 applied to a post-hoc analysis).
	r1 := recWithClaims("Alpha.", "Beta.")
	r2 := recWithClaims("Alpha.", "Gamma.")
	rep := Stability([]RunRecord{r1, r2}, nil, 0)
	if len(rep.Claims) != 3 {
		t.Errorf("want 3 distinct claims on the free path, got %d", len(rep.Claims))
	}
	if len(rep.Stable()) != 1 {
		t.Errorf("only alpha is unanimous, got %d stable", len(rep.Stable()))
	}
	if rep.ComparedBy == "" {
		t.Error("the report must name its comparator")
	}
	if rep.ComparisonCost != 0 {
		t.Errorf("the free path must cost nothing, got %s", rep.ComparisonCost)
	}
	if rep.Unassessed != 0 {
		t.Errorf("the bool relation asserts a verdict on every pair, so nothing is "+
			"unassessed on the free path, got %d", rep.Unassessed)
	}
}

func TestClusteringIsDeterministicAcrossRuns(t *testing.T) {
	// Replay must be byte-stable (P8), and clustering feeds a refine's targeting.
	recs := []RunRecord{
		recOfClaims("gamma", "alpha", "beta"),
		recOfClaims("beta", "gamma"),
		recOfClaims("alpha"),
	}
	var prev string
	for i := 0; i < 5; i++ {
		rep := StabilityWith(context.Background(), recs, MechanicalComparator{}, 2, nil)
		var s string
		for _, c := range rep.Claims {
			s += c.Claim.Norm + ":" + itoa(c.Support) + "|"
		}
		if i > 0 && s != prev {
			t.Fatalf("clustering is not deterministic:\n  %q\n  %q", prev, s)
		}
		prev = s
	}
	if prev == "" {
		t.Fatal("fixture produced no clusters; the determinism check is vacuous")
	}
}

func TestWithinReplicateRepetitionStillDoesNotInflateSupport(t *testing.T) {
	// P7 through the new path: support counts DISTINCT replicates, so one run
	// repeating itself across nodes contributes at most one.
	dup := RunRecord{Outcomes: []NodeOutcome{
		{NodeID: "a", Claims: []Claim{claimOf("The sky is blue.")}},
		{NodeID: "b", Claims: []Claim{claimOf("the sky is blue")}},
	}}
	rep := StabilityWith(context.Background(), []RunRecord{dup}, MechanicalComparator{}, 1, nil)
	if len(rep.Claims) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(rep.Claims))
	}
	if rep.Claims[0].Support != 1 {
		t.Errorf("within-replicate repetition must not inflate support, got %d", rep.Claims[0].Support)
	}
}

// claimTexts renders a cluster's members for failure messages.
func claimTexts(cs []Claim) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Text)
	}
	return out
}
