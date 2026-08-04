package main

import (
	"testing"

	quarry "github.com/scttfrdmn/quarry"
)

// THE FIRST TEST IN cmd/quarry, and its absence is part of the story. Two of the three
// defects the first live Bedrock run exposed were in this package — maxDepthOf and
// diffRecords — and neither was reachable from the library's tests, because the CLI is
// where a record is turned back into an Executor. `--fake` exercised both every time and
// never disagreed, for the reason recorded on maxDepthOf: the fake planner declines on
// clause length long before depth, so no --fake record has a max_depth leaf at all.
//
// These tests are unit tests over the two record-reading helpers, deliberately. Driving
// the whole command needs a provider; the defects were in the derivations.

// recordWithMaxDepthLeaves builds the shape a real run produces under --depth 2: leaves
// that stopped because they hit the bound, not because a planner declined.
func recordWithMaxDepthLeaves() quarry.RunRecord {
	return quarry.RunRecord{
		Outcomes: []quarry.NodeOutcome{
			{NodeID: "n0", Depth: 0, Children: []string{"n0.0"}, Content: "merged"},
			{NodeID: "n0.0", Depth: 1, Children: []string{"n0.0.0"}, Content: "merged"},
			{NodeID: "n0.0.0", Depth: 2, BaseCase: quarry.BaseMaxDepth, Content: "leaf", Model: "m"},
		},
	}
}

func TestBoundsAreReadFromTheRecordWhenPresent(t *testing.T) {
	// THE THIRD INSTANCE of one defect, and the reason RunBounds exists as a field rather
	// than a third derivation here. Found by a --fake regression sweep AFTER the live run:
	// `--cap 0.0001` recorded BaseBelowFloor at the root, and replay — which set no Floor —
	// planned instead and recorded BasePlannerDeclined. With no floor, zero is never below
	// it, so the replay ran a different algorithm.
	//
	// It had been failing silently the whole time: the old diffRecords compared only
	// Content and Cost, so it reported "no field-level difference was found" and blamed the
	// encoder. Fixing the reporter is what surfaced this.
	rec := recordWithMaxDepthLeaves()
	rec.Bounds = quarry.RunBounds{MaxDepth: 7, Floor: quarry.FromFloat(0.5), MaxRetries: 3}
	if got := maxDepthOf(rec); got != 7 {
		t.Errorf("a record that states its bound must be believed over any inference: "+
			"want 7, got %d", got)
	}
}

func TestEveryExecutorKnobTheRecordCarriesIsWiredIntoTheReplay(t *testing.T) {
	// THE FOURTH DEFECT, asserted where it lived. `--fake --cap 0.0001` recorded
	// BaseBelowFloor at the root; replay set no Floor, so zero was never below it, the root
	// planned instead and recorded BasePlannerDeclined — a divergence against a faithful
	// record, from a replay running a different algorithm.
	//
	// It had been silent the whole time: the old diffRecords compared only Content and Cost,
	// so it fell through to blaming the encoder. Fixing the reporter is what surfaced this.
	//
	// The assertion is on the WIRING, not on a derivation, because the wiring is what was
	// wrong — replayExecutor was an inline literal and a missing field was unreachable from
	// any test. Whenever RunBounds gains a knob, this test fails until it is carried across,
	// which is the point.
	rec := recordWithMaxDepthLeaves()
	rec.Bounds = quarry.RunBounds{MaxDepth: 7, Floor: quarry.FromFloat(0.5), MaxRetries: 3}

	e := replayExecutor(rec, "m")
	if e.MaxDepth != 7 {
		t.Errorf("MaxDepth must come from the record: want 7, got %d", e.MaxDepth)
	}
	if e.Floor != quarry.FromFloat(0.5) {
		t.Errorf("Floor must come from the record — a replay with no floor takes a different "+
			"base case: want %s, got %s", quarry.FromFloat(0.5), e.Floor)
	}
	if e.MaxRetries != 3 {
		t.Errorf("MaxRetries must come from the record: want 3, got %d", e.MaxRetries)
	}
	// The three substituted seams (§7). A replay wired with a live planner or reducer would
	// issue model calls during "replay" — spending money and producing a different tree.
	if e.Planner == nil || e.Reducer == nil || e.Solver == nil {
		t.Error("all three recorded seams must be wired, or part of the replay runs live")
	}
	// NO CACHE, deliberately: a hit would substitute a served answer for a recorded one.
	if e.Cache != nil {
		t.Error("a replay must not consult a cache — the comparison would be against the " +
			"wrong thing")
	}
}

func TestARecordThatCalledNoModelIsStillReplayable(t *testing.T) {
	// THE FIFTH DEFECT, and the second time this guard was too strict. It excused gaps only,
	// so `--cap 0.000001` — one below-floor root, no model, no gaps — was refused as having
	// "nothing to replay" when it replays byte-identically. Under §3.1 a cap-truncated run
	// is the normal outcome, and refusing to replay one makes the records most worth
	// interrogating the ones the tool declines to look at.
	//
	// "No model was called" and "nothing happened" are different claims. Only the second
	// makes a record unreplayable.
	unfundedRoot := quarry.RunRecord{Outcomes: []quarry.NodeOutcome{
		{NodeID: "n0", Depth: 0, BaseCase: quarry.BaseBelowFloor},
	}}
	if err := replayableRecord(unfundedRoot, ""); err != nil {
		t.Errorf("a record whose root was priced out must be replayable, got: %v", err)
	}

	allGapped := quarry.RunRecord{Outcomes: []quarry.NodeOutcome{
		{NodeID: "n0", Depth: 0, Gap: true},
	}}
	if err := replayableRecord(allGapped, ""); err != nil {
		t.Errorf("an all-gap record must be replayable, got: %v", err)
	}

	// The one case that really is unreplayable, and the reason the guard stays: nothing was
	// computed, so there is nothing to reproduce.
	allCached := quarry.RunRecord{Outcomes: []quarry.NodeOutcome{
		{NodeID: "n0", Depth: 0, CacheHit: true, Content: "served"},
	}}
	if err := replayableRecord(allCached, ""); err == nil {
		t.Error("an all-cache-hit record has nothing to replay and must say so")
	}
}

func TestTheDepthBoundIsReadFromTheRecordNotInferred(t *testing.T) {
	// FOUND BY THE FIRST LIVE BEDROCK RUN. `--depth 2` produced 22 leaves all recording
	// BaseMaxDepth; inferring the bound as deepest+1 gave replay a limit of 3, so those
	// nodes were no longer AT the bound, called the pinned planner, and came back
	// BasePlannerDeclined. Twenty-two fields differed and replay reported a divergence
	// against a faithful record.
	//
	// Same shape as ReplayRecord's BoundBy: a fact of the original EXECUTION cannot be
	// re-derived from the tree's geometry. deepest+1 is a lower bound on the cap, not the
	// cap, and the two coincide only when nothing hit it.
	rec := recordWithMaxDepthLeaves()
	if got := maxDepthOf(rec); got != 2 {
		t.Errorf("a node that stopped at max_depth names the bound exactly: want 2, got %d "+
			"(deepest+1 would give 3, which lets the leaf plan again)", got)
	}
}

func TestTheDepthBoundFallsBackWhenNothingHitIt(t *testing.T) {
	// The complement, and the reason the fallback stays. With no max_depth node the bound
	// is unobservable — the run never reached it — so any limit at least as deep as the
	// tree reproduces the same shape.
	rec := quarry.RunRecord{
		Outcomes: []quarry.NodeOutcome{
			{NodeID: "n0", Depth: 0, Children: []string{"n0.0"}, Content: "merged"},
			{NodeID: "n0.0", Depth: 1, BaseCase: quarry.BasePlannerDeclined, Content: "leaf", Model: "m"},
		},
	}
	if got := maxDepthOf(rec); got != 2 {
		t.Errorf("with no max_depth node the bound is deepest+1: want 2, got %d", got)
	}
}

// ------------------------------------------------------------------ diffRecords

func TestADivergenceReporterNamesTheFieldThatDiverged(t *testing.T) {
	// THE THIRD DEFECT, and the one that sent me to the wrong file. diffRecords compared
	// only Content and Cost, so a replay differing in 22 BaseCase fields fell through to
	// "no field-level difference was found — likely a field ordering or encoding change,
	// which is itself a P8 break". That message blames the ENCODER for a difference this
	// function simply did not look for.
	//
	// A divergence reporter that cannot name the divergence is worse than no reporter: it
	// asserts something specific and wrong about where the defect is.
	a := recordWithMaxDepthLeaves()
	b := recordWithMaxDepthLeaves()
	b.Outcomes[2].BaseCase = quarry.BasePlannerDeclined

	got := diffRecords(a, b)
	if got == "" {
		t.Fatal("a differing record must produce a diff")
	}
	if containsPhrase(got, "no field-level difference") {
		t.Errorf("diffRecords fell through to the encoder-blaming fallback on a real "+
			"field difference:\n%s", got)
	}
	if !containsPhrase(got, "base case") {
		t.Errorf("the diff must name the field that differs, got:\n%s", got)
	}
}

func TestTheDiffCatchesAnEditedRecord(t *testing.T) {
	// THE SAME DEFECT A THIRD TIME, found by scripting a tamper demo — the one demo where
	// editing a record IS the point. `diffRecords` skipped Claims on the stated grounds that
	// they were "covered by content", and they are not: content is REPLAYED from the record
	// while claims are RE-EXTRACTED from it. So an edited record diverges in claims *only* —
	// the replay faithfully returns the tampered content, then extracts what it actually
	// says. The reporter fell through to blaming the encoder on the single case a citable
	// artifact exists to detect.
	a := recordWithMaxDepthLeaves()
	b := recordWithMaxDepthLeaves()
	b.Outcomes[2].Claims = []quarry.Claim{{Text: "Altered after the fact.", Norm: "altered after the fact"}}

	got := diffRecords(a, b)
	if containsPhrase(got, "no field-level difference") {
		t.Errorf("an edited record must be named, not blamed on the encoder:\n%s", got)
	}
	if !containsPhrase(got, "claim") {
		t.Errorf("the diff must name claims as the field that differs, got:\n%s", got)
	}
}

func TestTheFallbackIsReservedForAGenuinelyUnexplainedDiff(t *testing.T) {
	// The fallback must still exist: it is the honest answer when every hashed field
	// agrees and the bytes do not, which really would be an encoding break. Identical
	// records exercise the same path — nothing found — and that must not crash or invent
	// a difference.
	a := recordWithMaxDepthLeaves()
	if got := diffRecords(a, a); !containsPhrase(got, "no field-level difference") {
		t.Errorf("identical records must reach the unexplained-diff fallback, got:\n%s", got)
	}
}

func TestTheDiffCatchesAGapCategoryFlip(t *testing.T) {
	// The specific confusion the unfunded sentinel exists to prevent, asserted at the
	// reporting layer too: if spend degradation ever came back flagged as a time gap, the
	// diff must say so in those words rather than fall through (§3.1).
	a := recordWithMaxDepthLeaves()
	b := recordWithMaxDepthLeaves()
	b.Outcomes[2].Gap = true

	got := diffRecords(a, b)
	if !containsPhrase(got, "gap") {
		t.Errorf("a gap flip must be named as one, got:\n%s", got)
	}
}

func TestTheDiffDistinguishesUncheckedFromFailed(t *testing.T) {
	// Verified is a *bool because nil means NOT CHECKED, which is not checked-and-failed
	// (§8). Comparing the pointers would miss a nil-vs-false flip entirely — the one
	// difference that turns "we did not look" into "we looked and it failed".
	no := false
	a := recordWithMaxDepthLeaves()
	b := recordWithMaxDepthLeaves()
	b.Outcomes[2].Verified = &no

	got := diffRecords(a, b)
	if !containsPhrase(got, "unchecked") {
		t.Errorf("an unchecked-to-failed flip must be named, got:\n%s", got)
	}
}

// containsPhrase is a substring test spelled out, so the assertions above read as
// English rather than as strings.Contains calls.
func containsPhrase(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
