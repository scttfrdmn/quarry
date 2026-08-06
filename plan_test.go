package quarry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// These tests ARE the specification for the plan artifact (#15, §9's approval gate).
// A failing one means the design changed: amend docs/design.md in the same commit or
// revert.
//
// The property under test throughout is AUTHORIZATION, not serialization. An artifact
// that round-trips but authorises the wrong run is the defect the gate exists to
// prevent — money spent on a split nobody approved, recorded as though somebody had.

// artifactFor builds an artifact the way planCmd does, so the tests exercise the same
// assembly the CLI performs rather than a hand-built value.
//
// HAND-ASSIGNING PlanID WOULD MAKE EVERY HASH TEST VACUOUS: a test that constructs the
// state it means to detect cannot discover that nothing produces it. NewPlanArtifact
// seals the hash itself, which is exactly the code path under test.
func artifactFor(t *testing.T, p Problem, caps Caps, plan Plan, depth int) PlanArtifact {
	t.Helper()
	l, err := NewLedger(caps, p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	var allocs []Allocation
	if !plan.Declined && len(plan.Items) > 0 {
		allocs, err = l.Apportion(DedupePlan(plan), 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	mean, varc := PlanMoments(plan)
	return NewPlanArtifact(p, caps, 0, depth, plan, allocs,
		Project(mean, varc, depth, FromFloat(0.01)), 0, FromFloat(0.01), FakePlannerModel)
}

func planCaps(spend Units) Caps { return Caps{Spend: spend, Latency: time.Hour} }

// ------------------------------------------------------------------ D1: the cap

// D1 is the integrity property the whole gate rests on. Planning is budget-conditioned
// (P9): the planner receives the balance and may decline, so the same split under half
// the money is a plan it might have refused.
func TestPlanIsOnlyValidForTheCapItWasPlannedUnder(t *testing.T) {
	p := problem("root")
	art := artifactFor(t, p, planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)

	if err := art.Authorizes(p, planCaps(FromFloat(1)), 0, 2, FakePlannerModel); err != nil {
		t.Fatalf("the cap it was planned under must be authorized: %v", err)
	}
	err := art.Authorizes(p, planCaps(FromFloat(0.5)), 0, 2, FakePlannerModel)
	if !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("half the cap must be refused, got %v", err)
	}
	// MORE money is refused too, and that direction is the one worth asserting: it is
	// tempting to allow it as "surely safe", but a planner given twice the balance might
	// have proposed a wider split, so executing the narrow one silently under-uses an
	// approval the operator granted for a different plan. The refusal says re-plan.
	if err := art.Authorizes(p, planCaps(FromFloat(2)), 0, 2, FakePlannerModel); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a LARGER cap must also be refused (the plan is conditioned on its own), got %v", err)
	}
}

// The latency cap is part of the budget too (§3.1: time and money are the two
// denominations), so a plan made with an hour must not run with a second.
func TestPlanRefusesADifferentDeadline(t *testing.T) {
	p := problem("root")
	art := artifactFor(t, p, Caps{Spend: FromFloat(1), Latency: time.Hour}, fanoutPlan("a", "b"), 2)
	err := art.Authorizes(p, Caps{Spend: FromFloat(1), Latency: time.Second}, 0, 2, FakePlannerModel)
	if !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a different latency cap must be refused, got %v", err)
	}
}

// The floor decides how the money DIVIDES, so it changes the approved apportionment
// even when every weight and the total cap are identical.
func TestPlanRefusesADifferentFloor(t *testing.T) {
	p := problem("root")
	art := artifactFor(t, p, planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	if err := art.Authorizes(p, planCaps(FromFloat(1)), FromFloat(0.01), 2, FakePlannerModel); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatal("a different floor must be refused: it changes where the money goes")
	}
}

func TestPlanRefusesADifferentDepthBound(t *testing.T) {
	p := problem("root")
	art := artifactFor(t, p, planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	if err := art.Authorizes(p, planCaps(FromFloat(1)), 0, 5, FakePlannerModel); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatal("a different depth bound must be refused: the tree approved was bounded by it (P2)")
	}
}

// A synthetic plan's Units are not the same quantity as a real one's, so approving a
// --fake plan must not authorise spending money. This is D1 arriving from an
// unexpected direction rather than a separate rule.
func TestPlanRefusesASyntheticPlanExecutedWithRealMoney(t *testing.T) {
	p := problem("root")
	art := artifactFor(t, p, planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	err := art.Authorizes(p, planCaps(FromFloat(1)), 0, 2, "us.anthropic.claude-haiku-4-5-20251001-v1:0")
	if !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a fake plan must not authorize a live run, got %v", err)
	}
	// And the reverse: a plan bought with real money must not be executed by the fake
	// planner either, or a --fake run would inherit an approval priced in real Units.
	live := NewPlanArtifact(p, planCaps(FromFloat(1)), 0, 2, fanoutPlan("a", "b"), nil,
		CostEstimate{}, 0, 0, "us.anthropic.claude-haiku-4-5-20251001-v1:0")
	if err := live.Authorizes(p, planCaps(FromFloat(1)), 0, 2, FakePlannerModel); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatal("a live plan must not authorize a fake run")
	}
}

// Caps.SameAs exists because == would refuse an artifact whose due date is exactly the
// one it was planned with: a time.Time that went out as RFC3339 and came back parsed
// has lost its monotonic reading. The gate must not manufacture refusals.
func TestCapsSameAsAcceptsTwoSpellingsOfOneInstant(t *testing.T) {
	// THE CASE THAT ACTUALLY REACHES THE DEFECT: two RFC3339 spellings of a single moment,
	// both of which resolveDue accepts and its own error message offers a host. A UTC
	// instant round-tripped through JSON compares equal under ==, so a test built that way
	// passes with the guard removed — vacuous. This one does not.
	planned, err := time.Parse(time.RFC3339, "2026-08-06T17:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	asked, err := time.Parse(time.RFC3339, "2026-08-06T13:00:00-04:00")
	if err != nil {
		t.Fatal(err)
	}
	plannedCaps := Caps{Spend: FromFloat(1), Due: planned}
	askedCaps := Caps{Spend: FromFloat(1), Due: asked}

	// Reintroduce the defect behind the test's back: == is what SameAs replaces, and if ==
	// agrees here then this fixture no longer exercises the guarantee.
	if plannedCaps == askedCaps {
		t.Fatal("this fixture no longer reproduces the defect SameAs guards: == accepts these " +
			"two spellings, so the test would pass with the guard removed")
	}
	if !plannedCaps.SameAs(askedCaps) {
		t.Fatal("SameAs must accept two spellings of one instant: refusing the deadline it was " +
			"planned with turns the gate into a source of spurious refusals")
	}

	p := Problem{Statement: "root"}
	art := NewPlanArtifact(p, plannedCaps, 0, 2, fanoutPlan("a", "b"), nil, CostEstimate{}, 0, 0, FakePlannerModel)
	if err := art.Authorizes(p, askedCaps, 0, 2, FakePlannerModel); err != nil {
		t.Fatalf("the same deadline spelled differently must still be authorized: %v", err)
	}
	// A DIFFERENT instant is still refused — the tolerance is about spelling, not about
	// deadlines being negotiable.
	later := Caps{Spend: FromFloat(1), Due: planned.Add(time.Hour)}
	if err := art.Authorizes(p, later, 0, 2, FakePlannerModel); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a genuinely different due date must be refused, got %v", err)
	}
}

// The artifact must survive the round trip a host actually performs: written, read by
// something else, handed back.
func TestARoundTrippedArtifactStillAuthorizesItsOwnConditions(t *testing.T) {
	p := Problem{Statement: "root"}
	caps := Caps{Spend: FromFloat(1), Due: time.Date(2026, 8, 6, 17, 0, 0, 0, time.UTC)}
	art := NewPlanArtifact(p, caps, 0, 2, fanoutPlan("a", "b"), nil, CostEstimate{}, 0, 0, FakePlannerModel)

	b, err := art.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodePlanArtifact(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := back.Authorizes(p, caps, 0, 2, FakePlannerModel); err != nil {
		t.Fatalf("a round-tripped artifact must still authorize its own conditions: %v", err)
	}
}

// ---------------------------------------------------------------- D2: the scope

// P6: scope never widens on descent, and the two-phase gate is a new place that can
// break — approved under one scope, executed under a broader one.
func TestPlanRefusesAWidenedScope(t *testing.T) {
	planned := Problem{Statement: "root", Scope: Scope{Tags: map[string]string{"team": "a", "project": "x"}}}
	art := artifactFor(t, planned, planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)

	// Dropping a tag WIDENS: the run would reach material the approved plan could not.
	wider := Problem{Statement: "root", Scope: Scope{Tags: map[string]string{"team": "a"}}}
	err := art.Authorizes(wider, planCaps(FromFloat(1)), 0, 2, FakePlannerModel)
	if !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("dropping a scope tag must be refused, got %v", err)
	}
	// BOTH sentinels, so a caller checking either finds it. This is a plan refusal AND a
	// P6 violation, and reporting only one sends the reader to the wrong file.
	if !errors.Is(err, ErrScopeWidens) {
		t.Fatalf("a widened scope must also wrap ErrScopeWidens (P6), got %v", err)
	}

	// NARROWING is allowed, in the direction Ledger.Child already enforces.
	narrower := Problem{Statement: "root", Scope: Scope{Tags: map[string]string{
		"team": "a", "project": "x", "dataset": "d"}}}
	if err := art.Authorizes(narrower, planCaps(FromFloat(1)), 0, 2, FakePlannerModel); err != nil {
		t.Fatalf("narrowing the scope must be allowed (P6 forbids widening only): %v", err)
	}
}

func TestPlanRefusesADifferentProblem(t *testing.T) {
	art := artifactFor(t, problem("what does storage cost"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	err := art.Authorizes(problem("what does compute cost"), planCaps(FromFloat(1)), 0, 2, FakePlannerModel)
	if !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a plan for another statement must be refused, got %v", err)
	}
	// The MESSAGE matters here and is asserted, because a plan for a different question
	// reported as a cap problem sends the operator to the wrong flag. Assertion order is
	// what decides which cause gets blamed.
	if !strings.Contains(err.Error(), "different problem") {
		t.Fatalf("the message must name the problem mismatch, not a cap: %v", err)
	}
}

// The refusal must show WHERE the statements differ, not the first 60 characters of each.
//
// THIS IS A DEFECT THE SUITE COULD NOT HAVE FOUND ABOVE, and the reason is instructive:
// TestPlanRefusesADifferentProblem's two statements diverge at byte 12, so any truncation
// window shows it. The message was `%.60q` on each side, which clipped two long statements
// sharing a long prefix down to the SAME visible text and printed them under the words "a
// different problem" — the operator is told two identical lines differ. Found by pasting
// the command `quarry plan` prints, which is also how the missing statement in that command
// was found.
func TestTheProblemMismatchShowsWhereTheStatementsDiverge(t *testing.T) {
	const planned = "What does Amazon's storage cost, how does it scale, and what dominates the bill?"
	asked := strings.TrimSuffix(planned, "?") // one byte, past any 60-char prefix window

	art := artifactFor(t, problem(planned), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	err := art.Authorizes(problem(asked), planCaps(FromFloat(1)), 0, 2, FakePlannerModel)
	if !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a trailing-byte difference is still a different problem, got %v", err)
	}
	msg := err.Error()

	// The two rendered statements must not be the SAME text — that is the defect, stated as
	// the guarantee rather than as a mechanism. Asserting "contains the byte offset" alone
	// would pass on a message that still printed two identical lines beside it.
	if got := strings.Count(msg, planned[:60]); got > 0 {
		t.Fatalf("the message shows a common prefix window, which cannot distinguish the two:\n%s", msg)
	}
	if !strings.Contains(msg, `dominates the bill?"`) || !strings.Contains(msg, `dominates the bill"`) {
		t.Fatalf("both statements must be shown clipped around the divergence:\n%s", msg)
	}
	if !strings.Contains(msg, "differ at byte 79") {
		t.Fatalf("the message must locate the divergence:\n%s", msg)
	}
}

// quoteAround must not cut a multi-byte rune in half.
//
// The window arithmetic is in BYTES because the divergence offset is, so a statement with
// non-ASCII text ahead of the difference would otherwise be clipped mid-character and the
// message about a text difference would itself be mojibake.
func TestTheMismatchMessageClipsOnRuneBoundaries(t *testing.T) {
	planned := strings.Repeat("café… ", 20) + "alpha"
	asked := strings.Repeat("café… ", 20) + "beta"

	art := artifactFor(t, problem(planned), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	err := art.Authorizes(problem(asked), planCaps(FromFloat(1)), 0, 2, FakePlannerModel)
	if err == nil {
		t.Fatal("statements differing in their tail must be refused")
	}
	msg := err.Error()

	// THE OBVIOUS ASSERTIONS HERE ARE VACUOUS, and finding that out is why this comment is
	// long. `utf8.ValidString(msg)` and a check for U+FFFD both PASS with the rune guard
	// removed, because strconv.Quote renders a stray continuation byte as the escape text
	// `\x80` — valid UTF-8 describing invalid bytes. The guarantee is that the window landed
	// on a character boundary, and the only visible evidence of that is the ABSENCE of a
	// hex-byte escape in the quoted output.
	if strings.Contains(msg, `\x`) {
		t.Fatalf("the window cut a rune in half — quoted as a raw byte escape:\n%s", msg)
	}
	// Non-vacuity of the check above: the fixture must actually place a multi-byte rune
	// across the window's start, or the assertion is testing nothing. The divergence sits at
	// byte 180 and the window opens 20 bytes earlier, mid-ellipsis.
	if d := commonPrefixLen(planned, asked); d != 180 {
		t.Fatalf("fixture no longer diverges where this test needs it (byte %d, want 180): "+
			"the rune-boundary case may not be exercised at all", d)
	}
	if !utf8.ValidString(msg) {
		t.Fatalf("the refusal message is not valid UTF-8: %q", msg)
	}
}

// ----------------------------------------------------------------- D3: identity

func TestPlanArtifactHashesToItsOwnID(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	if art.PlanID == "" {
		t.Fatal("NewPlanArtifact must seal the artifact with its hash")
	}
	if err := art.Verify(); err != nil {
		t.Fatalf("a freshly built artifact must verify: %v", err)
	}
	if got := PlanArtifactHash(art); got != art.PlanID {
		t.Fatalf("recomputed hash %s != PlanID %s", got, art.PlanID)
	}
}

// THE TAMPER CASE, and the one this whole type exists for. Reintroducing the defect
// behind the test's back means editing a field and leaving the ID: without the check,
// the run proceeds on an unapproved split while recording an approval.
func TestAnEditedPlanArtifactIsRefusedNotWarnedAbout(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)

	tests := []struct {
		name string
		edit func(*PlanArtifact)
	}{
		// Each of these is a real attack on the gate rather than a variation on one: the
		// weight moves the money, the cap breaks D1, the scope breaks D2, and the extra item
		// spends on a sub-problem no planner proposed.
		{"a weight", func(a *PlanArtifact) { a.Plan.Items[0].Weight = 99 }},
		{"the cap", func(a *PlanArtifact) { a.Caps.Spend = FromFloat(100) }},
		{"the scope", func(a *PlanArtifact) { a.Problem.Scope = Scope{Tags: map[string]string{"t": "x"}} }},
		{"the apportionment", func(a *PlanArtifact) { a.Allocations[0].Spend = FromFloat(50) }},
		{"an added child", func(a *PlanArtifact) {
			a.Plan.Items = append(a.Plan.Items, PlanItem{Problem: problem("smuggled"), Weight: 1})
		}},
		{"the statement", func(a *PlanArtifact) { a.Problem.Statement = "something else" }},
		{"the metered cost", func(a *PlanArtifact) { a.PlanCost = FromFloat(9) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edited := art
			// Deep-copy the slices, or an edit leaks into the shared fixture and the next
			// subtest asserts against a corrupted baseline.
			edited.Plan.Items = append([]PlanItem(nil), art.Plan.Items...)
			edited.Allocations = append([]Allocation(nil), art.Allocations...)
			tc.edit(&edited)
			if err := edited.Verify(); !errors.Is(err, ErrPlanTampered) {
				t.Fatalf("editing %s must be detected as tampering, got %v", tc.name, err)
			}
		})
	}
}

func TestPlanArtifactWithoutAnIDIsNotAnArtifact(t *testing.T) {
	var empty PlanArtifact
	empty.Version = PlanArtifactVersion
	if err := empty.Verify(); !errors.Is(err, ErrPlanTampered) {
		t.Fatalf("an artifact with no PlanID must be refused, got %v", err)
	}
}

// A host must be able to REFUSE an artifact before acting on one, which it can only do
// if the version is declared — the same reason the event stream's frame is written
// first.
func TestPlanArtifactRefusesAnUnknownVersion(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	art.Version = PlanArtifactVersion + 1
	if err := art.Verify(); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a future version must be refused, got %v", err)
	}
}

// The canonical bytes ARE the artifact: a host that returns a pretty re-encoding hands
// back a file that hashes differently, so the round trip must be byte-stable.
func TestPlanArtifactRoundTripsByteIdentically(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	b1, err := art.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodePlan(b1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := back.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("canonical bytes changed across a round trip:\n%s\n%s", b1, b2)
	}
	if err := back.Verify(); err != nil {
		t.Fatalf("a round-tripped artifact must verify: %v", err)
	}
}

// ----------------------------------------------------- D3: the record names it

// A record that cannot name the plan it was authorised to run leaves the approval
// unverifiable afterwards, which is the whole value of gating.
func TestRecordNamesThePlanItExecuted(t *testing.T) {
	res := Result{Outcomes: []NodeOutcome{{NodeID: "n0", Content: "x"}}}
	base := NewRunRecord(res, problem("root"), planCaps(FromFloat(1)), ModeFresh)
	gated := base.WithPlan("abc123")

	if gated.PlanID != "abc123" {
		t.Fatal("WithPlan must set the field")
	}
	// INSIDE THE IDENTITY, like Iteration.Record's ParentRun: two runs of the same tree
	// under the same caps, one gated and one not, are different records.
	if gated.RunID == base.RunID {
		t.Fatal("naming the plan must change the RunID: the approval is part of what the " +
			"record asserts, so a gated and an ungated run of the same tree cannot share an ID")
	}
	if got := RecordHash(gated); got != gated.RunID {
		t.Fatalf("a gated record must hash to its own RunID (P8): %s != %s", got, gated.RunID)
	}
}

// A replay does not re-approve anything and holds no artifact, so the approval must be
// INHERITED. Re-deriving it would either drop it — making a gated run replay as an
// ungated one — or assert an approval the replay never saw.
func TestReplayInheritsThePlanID(t *testing.T) {
	res := Result{Outcomes: []NodeOutcome{{NodeID: "n0", Content: "x"}}}
	orig := NewRunRecord(res, problem("root"), planCaps(FromFloat(1)), ModeFresh).WithPlan("abc123")

	rep := ReplayRecord(res, orig)
	if rep.PlanID != orig.PlanID {
		t.Fatalf("replay must inherit the PlanID, got %q want %q", rep.PlanID, orig.PlanID)
	}
	if rep.RunID != orig.RunID {
		t.Fatalf("a replay of a gated run must reproduce its RunID byte-identically (P8): %s != %s",
			rep.RunID, orig.RunID)
	}
}

// BACKWARD COMPATIBILITY, against a record captured BEFORE the field existed
// (testdata/record-pre-planid.json). omitempty is what makes this hold: an
// unconditional field would add `"PlanID":""` to every record's canonical bytes and
// every pre-existing record would stop hashing to its own RunID.
//
// The fixture is a REAL pre-change record — `quarry run --fake` at the commit before
// PlanID existed — rather than a hand-built one, because a test that constructs the state
// it means to detect cannot discover that nothing produces it. A hand-built RunRecord is
// assembled by TODAY's code and would carry today's field set by construction; only bytes
// written by the old code can show that the old bytes still verify.
func TestAnUngatedRecordHashesAsItDidBeforeThePlanFieldExisted(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "record-pre-planid.json"))
	if err != nil {
		t.Fatal(err)
	}
	// It does not mention the field, because the code that wrote it had never heard of it.
	if strings.Contains(string(b), "PlanID") {
		t.Fatal("fixture is not a pre-change record — recapture it from a commit before the " +
			"field existed, or this test is comparing today's code with itself")
	}

	var old RunRecord
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatal(err)
	}
	if old.PlanID != "" {
		t.Fatal("a record written before the gate existed cannot name an approval")
	}
	// THE GUARANTEE: it still hashes to the RunID it was written with. Remove omitempty and
	// `"PlanID":""` joins the canonical bytes, so this — and every record any user already
	// has on disk — stops hashing to its own identity.
	if got := RecordHash(old); got != old.RunID {
		t.Fatalf("a record written before PlanID existed must still hash to its own RunID: "+
			"stored %s, recomputed %s — omitempty is what makes that true, and without it "+
			"every pre-existing record is reported as edited (P8)", old.RunID, got)
	}

	// And a freshly-written ungated record is byte-compatible with that convention, so the
	// property holds going forward and not only for the captured file.
	fresh := NewRunRecord(Result{Outcomes: []NodeOutcome{{NodeID: "n0", Content: "x"}}},
		problem("root"), planCaps(FromFloat(1)), ModeFresh)
	if fresh.PlanID != "" {
		t.Fatal("an ungated run must not claim an approval")
	}
	fb, err := fresh.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fb), "PlanID") {
		t.Fatal("an ungated record's canonical bytes must not mention PlanID")
	}
}

// ------------------------------------------------------------ D5: the estimate

// P4: nothing may gate on the estimate, and the caveat must travel WITH the number so
// a host cannot render one without the other — the failure this prevents is an approval
// screen showing "$0.31" beside an Approve button.
func TestEveryArtifactCarriesItsEstimateCaveat(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	if art.EstimateCaveat == "" {
		t.Fatal("the caveat must be a FIELD, not documentation: it is what stops a host " +
			"rendering an advisory projection as a commitment (P4)")
	}
	if !strings.Contains(art.EstimateCaveat, "P4") {
		t.Fatalf("the caveat must name the principle it comes from: %q", art.EstimateCaveat)
	}
}

// The caveat SHARPENS where the projection is worst. A single fixed sentence would
// under-warn on exactly the runs whose numbers are theatre — at m >= 1 only the
// ceiling means anything.
func TestTheCaveatSharpensWhenTheProjectionIsUntrustworthy(t *testing.T) {
	sub := EstimateCaveat(CostEstimate{Mean: 0.4})
	near := EstimateCaveat(CostEstimate{Mean: 0.98, NearUnity: true})
	div := EstimateCaveat(CostEstimate{Mean: 2, Diverges: true})

	if sub == near || near == div || sub == div {
		t.Fatal("the three regimes must produce three different caveats: advisory is not " +
			"equally true in each")
	}
	if !strings.Contains(div, "ceiling") || !strings.Contains(near, "ceiling") {
		t.Fatal("at or near m=1 the caveat must point at the ceiling, the only trustworthy number")
	}
}

// ----------------------------------------------------- D6: declining is an outcome

// §13 recorded that the gate needed "a decision about what a declined plan does to a
// run record". This is that decision: a decline is a valid approvable artifact and it
// executes as a single node.
func TestADeclinedPlanRoundTripsAndRunsAsASingleNode(t *testing.T) {
	p := problem("what does storage cost")
	declined := Plan{Declined: true, Reasoning: "surface-to-volume does not favour a split (P1)"}
	art := artifactFor(t, p, planCaps(FromFloat(1)), declined, 2)

	if err := art.Verify(); err != nil {
		t.Fatalf("a declined plan must be a VALID artifact, not an error: %v", err)
	}
	if len(art.Allocations) != 0 {
		t.Fatal("a declined plan divides no money: there are no children to fund")
	}

	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, art.Planner(nil), prov)
	e.MaxDepth = 2
	l := ledger(t, FromFloat(100))
	res, err := e.Run(context.Background(), p, l)
	if err != nil {
		t.Fatalf("an approved decline must EXECUTE, not fail: %v", err)
	}
	if len(res.Outcomes) != 1 {
		t.Fatalf("an approved decline runs as ONE node, got %d", len(res.Outcomes))
	}
	if res.Outcomes[0].BaseCase != BasePlannerDeclined {
		t.Fatalf("the single node must record WHY it did not split, got %q", res.Outcomes[0].BaseCase)
	}
}

// ------------------------------------------------- executing the approved plan

// The artifact governs the root; the delegate plans below it. That is the "no approving
// below the root" non-goal made structural: each child works inside an allocation the
// gate approved, so a child re-planning within its share spends no authority nobody
// granted.
func TestApprovedPlannerReplaysTheRootAndDelegatesBelow(t *testing.T) {
	p := problem("root")
	approved := fanoutPlan("alpha", "beta")
	art := artifactFor(t, p, planCaps(FromFloat(1)), approved, 2)

	below := &countingPlanner{inner: StaticPlanner{P: Plan{Declined: true}}}
	ap := art.Planner(below)

	got, err := ap.Plan(context.Background(), p, Allocation{Spend: FromFloat(1)}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].Problem.Statement != "alpha" {
		t.Fatalf("the root must return the APPROVED split, got %+v", got.Items)
	}
	if below.calls != 0 {
		t.Fatal("the delegate must not be consulted at the root: the approved plan governs it")
	}
	if _, err := ap.Plan(context.Background(), problem("alpha"), Allocation{}, 1, nil); err != nil {
		t.Fatal(err)
	}
	if below.calls != 1 {
		t.Fatalf("below the root the delegate plans, got %d calls", below.calls)
	}
}

// A mismatch at the root must be LOUD, never a fall-through to the delegate: falling
// through would spend on an unapproved split while the record still named the artifact,
// and a gate that quietly does the ungated thing is worse than no gate.
func TestApprovedPlannerRefusesAMismatchedRootRatherThanDelegating(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	below := &countingPlanner{inner: StaticPlanner{P: fanoutPlan("x")}}

	_, err := art.Planner(below).Plan(context.Background(), problem("a different root"), Allocation{}, 0, nil)
	if !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a mismatched root must be refused, got %v", err)
	}
	if below.calls != 0 {
		t.Fatal("a mismatched root must NOT fall through to the delegate")
	}
}

// A split plan with nothing wired below must not silently make every child a leaf: the
// approved plan says these children exist, and turning them into leaves would execute
// a different tree while reporting it as faithful.
func TestApprovedPlannerWithNoDelegateFailsBelowTheRootRatherThanDeclining(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	_, err := art.Planner(nil).Plan(context.Background(), problem("a"), Allocation{}, 1, nil)
	if err == nil {
		t.Fatal("no planner below an approved root must be an ERROR: declining would silently " +
			"execute a shallower tree than the one approved")
	}
}

// The artifact must not be mutable by the run executing it, or the PlanID becomes a
// claim about something that no longer exists.
func TestExecutingAnApprovedPlanDoesNotMutateTheArtifact(t *testing.T) {
	art := artifactFor(t, problem("root"), planCaps(FromFloat(1)), fanoutPlan("a", "b"), 2)
	id := art.PlanID

	got, err := art.Planner(nil).Plan(context.Background(), problem("root"), Allocation{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	got.Items[0].Weight = 999
	got.Items[0].Problem.Statement = "smuggled"

	if err := art.Verify(); err != nil {
		t.Fatalf("mutating the returned plan must not affect the artifact: %v", err)
	}
	if art.PlanID != id || art.Plan.Items[0].Weight == 999 {
		t.Fatal("the artifact handed out its own slice: a run could rewrite what it was approved to do")
	}
}

// The mechanical check on top of the hash. It catches a class the hash cannot: an
// artifact that is internally consistent but no longer apportions the way it did —
// changed arithmetic, a different reserve, a partly-spent balance.
func TestApportionRefusesWhenTheApprovedSharesNoLongerHold(t *testing.T) {
	p := problem("root")
	caps := planCaps(FromFloat(1))
	art := artifactFor(t, p, caps, fanoutPlan("a", "b"), 2)

	fresh, err := NewLedger(caps, p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	allocs, err := art.Apportion(fresh)
	if err != nil {
		t.Fatalf("the ledger the plan was made against must apportion identically: %v", err)
	}
	if len(allocs) != 2 {
		t.Fatalf("want 2 allocations, got %d", len(allocs))
	}

	// Reintroduce the divergence behind the test's back: spend some of the balance, so
	// the SAME weights against the SAME cap now divide differently. The hash still
	// verifies — nothing was edited — which is exactly why this check has to exist.
	spent, err := NewLedger(caps, p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := spent.Debit(context.Background(), FromFloat(0.5)); err != nil {
		t.Fatal(err)
	}
	if err := art.Verify(); err != nil {
		t.Fatalf("the artifact itself is untouched, so it must still verify: %v", err)
	}
	if _, err := art.Apportion(spent); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("a balance that no longer produces the approved shares must be refused, got %v", err)
	}
}

// THE GATE MUST APPORTION THE SHAPE THE EXECUTOR WILL RUN, not the one the artifact
// literally lists. Executor.node collapses identical children before apportioning (the
// DAG rule, §2), so an artifact carrying duplicates describes N children and runs fewer.
//
// FOUND BY REINTRODUCING A DEFECT AND WATCHING NOTHING FAIL. Removing the CLI's own
// DedupePlan call left every test green: the artifact then stored four allocations, the
// check re-derived four from the same un-collapsed items, they matched, and the run
// executed three children on a division of the money nobody approved. The gate approved
// one tree while the executor ran another — the single failure it exists to prevent, and
// it was invisible because both sides of the comparison shared the same blind spot.
//
// This test hands Apportion an artifact whose stored allocations were computed WITHOUT
// deduping, which is precisely what an artifact written by a producer that forgot looks
// like. Nothing this CLI writes is like that any more, which is why the input is built by
// hand here rather than through artifactFor — the state has to be constructible to be
// detectable, and the run above is what proved something produces it.
func TestApportionCollapsesTheSameDuplicatesTheExecutorWill(t *testing.T) {
	p := problem("root")
	caps := planCaps(FromFloat(1))
	dup := fanoutPlan("a", "a", "b") // two identical children: the executor merges them

	// The un-deduped apportionment — three shares — as a forgetful producer would store it.
	naive, err := NewLedger(caps, p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := naive.Apportion(dup, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("fixture: want 3 naive shares, got %d — this test needs the two shapes to "+
			"DIFFER or it proves nothing", len(stored))
	}
	mean, varc := PlanMoments(dup)
	art := NewPlanArtifact(p, caps, 0, 2, dup, stored,
		Project(mean, varc, 2, FromFloat(0.01)), 0, FromFloat(0.01), FakePlannerModel)

	fresh, err := NewLedger(caps, p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := art.Apportion(fresh); !errors.Is(err, ErrPlanNotAuthorized) {
		t.Fatalf("an artifact whose shares were computed WITHOUT the collapse must be "+
			"refused: the run would fund 2 children where 3 were approved, got %v", err)
	}

	// And the collapsed artifact — what the gate actually writes — is authorised, with the
	// duplicate's weight merged. Without this half the test would pass against an Apportion
	// that refused every plan containing a repeated statement.
	ok := artifactFor(t, p, caps, dup, 2)
	if len(ok.Allocations) != 2 {
		t.Fatalf("the approved shape is the COLLAPSED one: want 2 allocations, got %d",
			len(ok.Allocations))
	}
	fresh2, err := NewLedger(caps, p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	allocs, err := ok.Apportion(fresh2)
	if err != nil {
		t.Fatalf("a correctly-collapsed artifact must be authorized: %v", err)
	}
	// 2:1 weights after the merge, so the doubled child gets twice the money. That the
	// merged WEIGHT survives is the other half of the DAG rule — a collapse that dropped
	// it would underfund the child two siblings asked for.
	if allocs[0].Spend <= allocs[1].Spend {
		t.Errorf("the merged child carries both weights, so it must be funded more: got %s and %s",
			allocs[0].Spend, allocs[1].Spend)
	}
}

// ------------------------------------------------- the shared pre-plan base cases

// `quarry plan` must not promise a split the executor would never perform. The
// executor and the gate call the SAME function, so the two cannot drift.
func TestPrePlanBaseAgreesWithWhatTheExecutorDoesBeforePlanning(t *testing.T) {
	l := ledger(t, FromFloat(1))

	if _, done := PrePlanBase(l, 0, 0, 3); done {
		t.Fatal("a funded root under its depth bound must reach the planner")
	}
	if base, done := PrePlanBase(l, 0, 3, 3); !done || base != BaseMaxDepth {
		t.Fatalf("at the depth bound the node must terminate as %q, got %q/%v", BaseMaxDepth, base, done)
	}
	if base, done := PrePlanBase(l, FromFloat(10), 0, 3); !done || base != BaseBelowFloor {
		t.Fatalf("below the floor the node must terminate as %q, got %q/%v", BaseBelowFloor, base, done)
	}

	// An UNLIMITED balance is never below the floor. Absence is not zero: asking the
	// comparison without Limited() would read Unlimited (-1) as the poorest possible node
	// and terminate every uncapped run at its root.
	unlimited, err := NewLedger(Caps{Spend: Unlimited, Latency: time.Hour}, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, done := PrePlanBase(unlimited, FromFloat(10), 0, 3); done {
		t.Fatal("an unlimited balance must not be judged below the floor")
	}
}

// The executor must actually route through PrePlanBase, not merely have a copy that
// agrees today. Reintroducing the defect means the two diverging, which a test that
// only calls PrePlanBase directly could never see.
func TestTheExecutorTerminatesExactlyWherePrePlanBaseSaysItWill(t *testing.T) {
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.Floor = FromFloat(10)

	l := ledger(t, FromFloat(1)) // below the floor
	if base, done := PrePlanBase(l, e.Floor, 0, e.MaxDepth); !done || base != BaseBelowFloor {
		t.Fatalf("fixture must be below floor, got %q/%v", base, done)
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].BaseCase != BaseBelowFloor {
		t.Fatalf("the executor must terminate as PrePlanBase says: %d nodes, base %q",
			len(res.Outcomes), res.Outcomes[0].BaseCase)
	}
}

// countingPlanner records whether it was consulted, which is what the delegation tests
// actually assert — a returned plan cannot distinguish "the delegate answered" from
// "the artifact happened to match".
type countingPlanner struct {
	inner Planner
	calls int
}

func (c *countingPlanner) Plan(ctx context.Context, p Problem, alloc Allocation, depth int, prior []NodeOutcome) (Plan, error) {
	c.calls++
	return c.inner.Plan(ctx, p, alloc, depth, prior)
}
