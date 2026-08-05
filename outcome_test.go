package quarry

import "testing"

// Classify is a CONTRACT TWO OTHER REPOS BRANCH ON (#9 D4), so these tests are about the
// distinctions it must preserve rather than about its return values. Every case below is a
// pair that a simpler predicate would collapse, and each collapse has a named consequence
// for a host.

func TestClassifyKeepsTimeAndMoneyApart(t *testing.T) {
	// THE DISTINCTION THE WHOLE VOCABULARY EXISTS FOR. Both runs stopped short with a
	// partial answer; they differ only in the denomination that bound them, and the
	// remedies do not substitute — offering a deadline raise to the second refills nothing.
	// This is §3.1's standing ruling at the CLI boundary, and ErrRecordedUnfunded is the
	// same ruling one layer down.
	timeBound := RunRecord{Outcomes: []NodeOutcome{
		{NodeID: "n0", Content: "a partial answer", Cost: 100, Children: []string{"n0.0"}},
		{NodeID: "n0.0", Gap: true, Depth: 1},
	}}
	if got := timeBound.Classify(); got != OutcomeTimeTruncated {
		t.Errorf("a gapped child means TIME truncated the run, got %q", got)
	}

	moneyBound := RunRecord{Outcomes: []NodeOutcome{
		{NodeID: "n0", Content: "a partial answer", Cost: 100, Children: []string{"n0.0"}},
		// Unfunded: no model, no content, no verdict, no children (types.go Unfunded).
		{NodeID: "n0.0", Depth: 1},
	}}
	if got := moneyBound.Classify(); got != OutcomeDegraded {
		t.Errorf("a priced-out child is planned degradation inside authority, not a gap, got %q", got)
	}
}

func TestClassifyReportsTimeAheadOfMoneyWhenBothBit(t *testing.T) {
	// A run CAN be both — a deadline expires while the cap is nearly gone — and the
	// precedence is a decision, not an accident. Gaps mean work that is missing and
	// unrecorded, which is the more consequential fact: spend is visible on the receipt,
	// while a gap is the absence of a line. Reporting this as merely degraded would
	// understate it and send a host after money it does not need.
	both := RunRecord{
		BoundBy: DenomSpend, // the money signal, deliberately set alongside a gap
		Outcomes: []NodeOutcome{
			{NodeID: "n0", Content: "some of it", Cost: 100, Children: []string{"n0.0", "n0.1"}},
			{NodeID: "n0.0", Gap: true, Depth: 1},
			{NodeID: "n0.1", Depth: 1},
		},
	}
	if got := both.Classify(); got != OutcomeTimeTruncated {
		t.Errorf("with both signals present, TIME is reported: it names the missing work, got %q", got)
	}
	// And the counts stay separate, because the outcome name collapses what the numbers do
	// not. A host that only had the classification could not size either shortfall.
	if len(both.Gaps()) != 1 || len(both.Unfunded()) != 1 {
		t.Errorf("both denominations must remain countable: %d gaps, %d unfunded",
			len(both.Gaps()), len(both.Unfunded()))
	}
}

func TestClassifyReportsNoAnswerAheadOfEveryTruncation(t *testing.T) {
	// A run that returned NOTHING is almost always also truncated, and the ordering
	// matters for a concrete reason: reporting the truncation tells a host to EXTEND, and
	// there is no partial answer here to extend. This is the case both degenerate CI runs
	// produce (`--cap 0.000001` and a deadline shorter than one call), so it is the common
	// shape rather than a contrived one.
	for name, rec := range map[string]RunRecord{
		"every node gapped": {BoundBy: DenomLatency, Outcomes: []NodeOutcome{
			{NodeID: "n0", Gap: true},
		}},
		"root priced out": {Outcomes: []NodeOutcome{
			{NodeID: "n0"},
		}},
		"no outcomes at all": {},
	} {
		if got := rec.Classify(); got != OutcomeNoAnswer {
			t.Errorf("%s: want %q, got %q", name, OutcomeNoAnswer, got)
		}
	}
}

func TestHasAnswerTreatsWhitespaceAsNothing(t *testing.T) {
	// What a model returns when it declines. A host would render this as a blank pane and
	// call it a result, so it must not count — and the CLI's own no-answer check uses
	// TrimSpace for the same reason, which is what these two must agree about.
	blank := RunRecord{Outcomes: []NodeOutcome{{NodeID: "n0", Content: " \n\t\r "}}}
	if blank.HasAnswer() {
		t.Error("whitespace-only content is not an answer")
	}
	if got := blank.Classify(); got != OutcomeNoAnswer {
		t.Errorf("a whitespace answer classifies as no-answer, got %q", got)
	}
	if !(RunRecord{Outcomes: []NodeOutcome{{NodeID: "n0", Content: "x"}}}).HasAnswer() {
		t.Error("a single non-space rune is an answer")
	}
}

func TestClassifyCallsAnUnboundRunComplete(t *testing.T) {
	// The negative case, and it is worth asserting because Truncated() is a BROAD
	// predicate — three signals, any one sufficient — so a bug there reads as "nothing ever
	// completes", which is the failure mode that would make the whole vocabulary useless
	// by reporting every run as degraded.
	rec := RunRecord{Outcomes: []NodeOutcome{
		{NodeID: "n0", Content: "merged", Cost: 100, Children: []string{"n0.0"}},
		{NodeID: "n0.0", Content: "a", Cost: 50, Model: "m", ModelVersion: "m@1", Depth: 1},
	}}
	if got := rec.Classify(); got != OutcomeComplete {
		t.Errorf("a run inside its caps with an answer is complete, got %q", got)
	}
}

func TestOutcomeEventCarriesTheLedgersIntegersNotFloats(t *testing.T) {
	// The one event on this stream with no float on it (#18). A host reading total_micros
	// has nothing to reconcile — no rounding rule, no epsilon — which is the point of
	// putting quarry's own numbers on quarry's own event rather than reusing agate's USD.
	rec := RunRecord{
		Caps: Caps{Spend: 250_000},
		Outcomes: []NodeOutcome{
			{NodeID: "n0", Content: "merged", Cost: 13911, Children: []string{"n0.0"}},
			{NodeID: "n0.0", Content: "a", Cost: 3509, Model: "m", ModelVersion: "m@1", Depth: 1},
		},
	}
	var ev OutcomeEvent
	var found bool
	for _, e := range HostRunEvents(rec, "", nil) {
		if o, ok := e.(OutcomeEvent); ok {
			ev, found = o, true
		}
	}
	if !found {
		t.Fatal("a framed stream must carry a terminal outcome event")
	}
	if ev.TotalMicros != 17420 {
		t.Errorf("total_micros is the ledger's own sum, want 17420, got %d", ev.TotalMicros)
	}
	if ev.CapMicros != 250_000 {
		t.Errorf("cap_micros is the contract the total sat under, got %d", ev.CapMicros)
	}
}

func TestUnlimitedCapReachesTheWireAsMinusOneNotZero(t *testing.T) {
	// ABSENCE IS NOT ZERO, at a new site. A deadline-only run has no spend cap, and a host
	// told cap_micros=0 would read it as "a cap of nothing" — the opposite of unlimited,
	// and it would render every such run as having overspent by its entire total.
	rec := RunRecord{Caps: Caps{Spend: Unlimited, Latency: 1}, Outcomes: []NodeOutcome{
		{NodeID: "n0", Content: "answer", Cost: 100},
	}}
	for _, e := range HostRunEvents(rec, "", nil) {
		if o, ok := e.(OutcomeEvent); ok {
			if o.CapMicros != -1 {
				t.Errorf("an unlimited cap must reach the wire as -1, got %d", o.CapMicros)
			}
		}
	}
}
