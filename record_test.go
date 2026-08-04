package quarry

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// These tests ARE the specification for build step 5 (§7 reproduce, §8 record).
// A failing test means the design changed — amend docs/design.md in the same
// commit or revert.

// runFor builds a small tree over the fake provider and returns its record.
func runFor(t *testing.T, prov Provider) (RunRecord, Result) {
	t.Helper()
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		Planner:  StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:   ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 2,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, root, caps, ModeFresh), res
}

// --------------------------------------------------------- canonical bytes

func TestCanonicalEncodingIsStable(t *testing.T) {
	// Encoding the same record twice yields identical bytes — the artifact is a
	// pure function of content (P8).
	rec, _ := runFor(t, &fakeProvider{cost: FromFloat(1)})
	a, err := rec.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	b, err := rec.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("canonical encoding must be byte-stable across calls")
	}
}

func TestRunIDIsTheContentHash(t *testing.T) {
	// Identity is a function of content: recompute the hash with RunID zeroed and
	// it must match the stored ID (P8).
	rec, _ := runFor(t, &fakeProvider{cost: FromFloat(1)})
	if rec.RunID == "" {
		t.Fatal("record must carry a content-hash ID")
	}
	if got := contentHash(rec); got != rec.RunID {
		t.Errorf("RunID must equal the content hash: got %s want %s", got, rec.RunID)
	}
}

// ------------------------------------------------------------- replay (§7)

func TestReplayIsByteForByteIdentical(t *testing.T) {
	// THE determinism test (§7, build step 5). Run once, replay against recorded
	// responses with no model calls, and the two records must be byte-identical.
	// This is what Units being integral buys: largest-remainder apportionment
	// replays without drift.
	orig, _ := runFor(t, &fakeProvider{cost: FromFloat(3)})

	replayed, _ := runFor(t, NewRecordedProvider(orig))

	ob, err := orig.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := replayed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, rb) {
		t.Fatalf("replay must be byte-for-byte identical\n orig: %s\n rep:  %s", ob, rb)
	}
	if orig.RunID != replayed.RunID {
		t.Errorf("identical content must hash identically: %s vs %s", orig.RunID, replayed.RunID)
	}
}

func TestReplayMakesNoModelCalls(t *testing.T) {
	// Reproduce re-executes the tree against recorded responses — the recorded
	// provider is the only source, and it counts as zero live calls (§7).
	orig, _ := runFor(t, &fakeProvider{cost: FromFloat(1)})
	rp := NewRecordedProvider(orig)

	// A recorded provider serving a leaf that was in the run succeeds; anything
	// absent is a divergence, not a silent miss.
	if _, err := rp.Complete(context.Background(), "alpha", "fake", Scope{}); err != nil {
		t.Errorf("recorded leaf must replay: %v", err)
	}
	if _, err := rp.Complete(context.Background(), "never-asked", "fake", Scope{}); err == nil {
		t.Error("a call the record does not contain must surface as divergence")
	}
}

// gappyProvider answers some prompts and hangs on others until the context expires,
// producing a run where part of the tree is time-truncated and part is not.
//
// Needed because every other double here either answers everything or nothing, and a
// PARTIAL run is the case §3.1 is entirely about — the one the fixtures could not
// produce, which is how the defect below survived.
type gappyProvider struct {
	cost Units
	hang map[string]bool // prompts that never return

	// answered closes once every non-hanging prompt has been served, so the caller can
	// cancel the run immediately instead of waiting out a deadline. Without it the
	// fixture's cost is the whole timeout — and the timeout has to be generous, because
	// making it tight is what made this -race-dependent in the first place.
	answered chan struct{}
	mu       sync.Mutex
	served   int
	wantHits int
}

// Complete answers immediately, or waits for cancellation on a hung prompt.
//
// The answering path does NOT sleep and the hanging path has no timer of its own, so
// which leaves gap is decided by g.hang and not by a race against a deadline. An
// earlier version leaned on a 30ms window and passed normally while failing under
// -race, where instrumentation ate the margin: a fixture whose outcome depends on
// scheduling luck tests the machine, not the code.
func (g *gappyProvider) Complete(ctx context.Context, prompt, model string, scope Scope) (Sample, error) {
	if g.hang[prompt] {
		<-ctx.Done()
		return Sample{}, ctx.Err()
	}
	// The signal is sent UNDER the lock. It is one channel close, not I/O, and doing it
	// outside would race two concurrent siblings into a double close — which panics.
	g.mu.Lock()
	g.served++
	if g.served >= g.wantHits && g.answered != nil {
		close(g.answered)
		g.answered = nil
	}
	g.mu.Unlock()
	return Sample{
		Content: "ans:" + prompt, Cost: g.cost, Model: model, ModelVersion: model + "-v1",
		HaloTokens: 40, GeneratedTokens: 10,
	}, nil
}

func (g *gappyProvider) Estimate(string, string) Units { return g.cost }

// partialRun executes a tree in which "beta" never returns and "alpha" always does,
// yielding a record with some gaps and some answers.
//
// Now is read HERE rather than at package scope: ChildContext derives the children's
// window as now.Add(remaining−reserve), so a `now` captured before the test started
// silently shortens that window — which is how this became -race-dependent. The
// injected clock and the context deadline must be read at the same moment.
//
// The rest of this package keeps the fixed `now`; Go rule 4 exists so the core is
// clock-free. A partial run is the one case that genuinely cannot be faked, because it
// asks whether one call finished before another did.
func partialRun(t *testing.T, cost Units) (RunRecord, Caps) {
	t.Helper()
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan struct{})
	prov := &gappyProvider{
		cost: cost, hang: map[string]bool{"beta": true},
		answered: answered, wantHits: 1, // "alpha" is the only prompt that returns
	}
	start := time.Now()
	e := &Executor{
		Planner: StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:  ProviderSolver{Provider: prov, Model: "fake"},
		// MaxDepth 1: the root plans once and its two children are leaves. StaticPlanner
		// returns the SAME plan at every depth, so MaxDepth 2 gives a 7-node tree with
		// several "alpha" leaves — and cancelling on the first answered alpha then gaps
		// alphas at depth 2 nondeterministically. A flat tree makes exactly one prompt
		// answer and exactly one gap, which is all this needs.
		Reducer: ConcatReducer{Sep: "|"}, Now: start, MaxDepth: 1,
	}
	// Cancelled by the ANSWERING leaf finishing, not by a clock: the timeout is only a
	// backstop against a hang, so it can be generous without costing wall-clock. A tight
	// timeout is what made an earlier version pass normally and fail under -race, and a
	// generous one alone made the fixture cost a full second per call.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		<-answered
		cancel() // "alpha" is in; "beta" is now a gap, deterministically
	}()
	res, err := e.Run(ctx, problem("root"), l)
	if err != nil {
		t.Fatalf("a time miss must not fail the run (§3.1): %v", err)
	}
	return NewRunRecord(res, problem("root"), caps, ModeFresh), caps
}

func TestAPartialRunReplaysAsPartial(t *testing.T) {
	// FOUND BY RUNNING THE BINARY. `quarry replay` on a run with three time-truncated
	// nodes failed with "replay diverged: no recorded sample" — reporting a divergence
	// when the replay was faithful. NewRecordedProvider skipped gaps entirely, so the
	// replay asked for a call the record correctly did not contain, and the two cases
	// were indistinguishable: "this node was cut short" and "this node is not in the
	// record at all" are opposite claims about the SAME lookup failure.
	//
	// §3.1 makes a partial run the normal outcome under a deadline, so this made replay
	// unavailable for exactly the records most worth interrogating.
	orig, caps := partialRun(t, FromFloat(3))
	root := problem("root")

	// Non-vacuity: the fixture must actually be partial, or this test proves nothing
	// about gaps. Both halves matter — all-gaps would be a different case.
	gaps := orig.Gaps()
	if len(gaps) == 0 {
		t.Fatal("fixture must produce at least one gap")
	}
	if len(gaps) == len(orig.Outcomes) {
		t.Fatal("fixture must leave at least one node answered; an all-gap run is not partial")
	}

	// The replay: same executor, recorded seams, NO deadline — which is the detail that
	// made the executor's ctx.Err() gap test unable to see a recorded gap.
	l2, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	// The fixed `now` is correct on THIS side: the replay has no deadline at all, so
	// ChildContext takes its no-deadline branch and the injected instant is never used
	// for window arithmetic. Byte-identity across a real-clock run and a fixed-clock
	// replay is itself the point — timings are deliberately unhashed (P8), so the record
	// must not depend on which clock produced it.
	e2 := &Executor{
		Planner: StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:  ProviderSolver{Provider: NewRecordedProvider(orig), Model: "fake"},
		// MaxDepth must MATCH the run's. A replay at a different depth produces a
		// different tree, which replay correctly reports as a divergence — so a mismatch
		// here tests the harness rather than the code.
		Reducer: ConcatReducer{Sep: "|"}, Now: now, MaxDepth: 1,
	}
	res2, err := e2.Run(context.Background(), root, l2)
	if err != nil {
		t.Fatalf("a partial record must replay, not report divergence: %v", err)
	}
	replayed := ReplayRecord(res2, orig)

	// The fixture cancels the ROOT, so this run is bound by latency — and that is what
	// exposed the second half of this defect. BoundBy is read from the live context, which
	// a replay deliberately does not reproduce, so assembling the replayed record with
	// NewRunRecord recomputed it as "" and the comparison failed on a field the replay had
	// no way to get right. `quarry replay` could not see it because a deadline normally
	// cuts CHILDREN while the root finishes inside its window, leaving BoundBy empty on
	// both sides. Non-vacuity guard, so this test keeps covering that:
	if orig.BoundBy != DenomLatency {
		t.Fatalf("fixture must be latency-bound for this to test ReplayRecord's inheritance, got %q",
			orig.BoundBy)
	}

	ob, _ := orig.Canonical()
	rb, _ := replayed.Canonical()
	if !bytes.Equal(ob, rb) {
		t.Errorf("a partial run must replay byte-identically\n orig: %s\n rep:  %s", ob, rb)
	}
	// And the gap must come back AS A GAP. Replaying it as an empty answer would be the
	// subtler failure: it converts a node that was never asked into one that was asked
	// and said nothing, which is the distinction §8 exists to preserve.
	if len(replayed.Gaps()) != len(gaps) {
		t.Errorf("replay must reproduce %d gaps, got %d", len(gaps), len(replayed.Gaps()))
	}
}

func TestAnAllGapRunIsStillReplayable(t *testing.T) {
	// The limit case, and the third time this shape has been a defect: an all-gap record
	// is a FAITHFUL record of the most time-bound run the system can produce, not a broken
	// one. `quarry run --fake --deadline 60ms` makes one, and the CLI refused it with
	// "there is nothing to replay" because no node named a model.
	//
	// Nothing has to be relaxed for this to work: gaps are indexed without a model, so a
	// record with no model calls at all still has a complete gap index. What it pins is
	// that replay does not need a single answered node to reproduce a tree.
	orig := childrenOutOfTimeRun(t)
	if len(orig.Gaps()) != len(orig.Outcomes) {
		t.Fatalf("fixture must gap every node for this to be the limit case: %d of %d",
			len(orig.Gaps()), len(orig.Outcomes))
	}

	l, err := NewLedger(orig.Caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		Planner: StaticPlanner{P: fanoutPlan("alpha", "beta")},
		// The model is deliberately a string no record would name. A gap lookup ignores it,
		// so if replay ever DOES consult the provider, the miss is a divergence — which is
		// the honest answer, because the record contains no answer to serve.
		Solver:   ProviderSolver{Provider: NewRecordedProvider(orig), Model: "no-model"},
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 1,
	}
	res, err := e.Run(context.Background(), orig.Problem, l)
	if err != nil {
		t.Fatalf("an all-gap record must replay, not fail: %v", err)
	}
	replayed := ReplayRecord(res, orig)

	ob, _ := orig.Canonical()
	rb, _ := replayed.Canonical()
	if !bytes.Equal(ob, rb) {
		t.Errorf("an all-gap run must replay byte-identically\n orig: %s\n rep:  %s", ob, rb)
	}
	// And every gap must come back as a gap, not as an empty answer — the distinction §8
	// exists to preserve, at the one size where "all of them" makes it easiest to lose.
	if len(replayed.Gaps()) != len(orig.Gaps()) {
		t.Errorf("replay must reproduce all %d gaps, got %d", len(orig.Gaps()), len(replayed.Gaps()))
	}
}

func TestUnfundedNamesNodesThatReachedNoModelButNotEmptyAnswers(t *testing.T) {
	// The discriminator, asserted directly, because three call sites derived it separately
	// and one had it wrong. An unfunded node reached no model; a node that was SOLVED and
	// answered emptily is a result, and §8 exists to keep those apart.
	no := false
	r := RunRecord{Outcomes: []NodeOutcome{
		{NodeID: "unfunded", Problem: problem("a")},
		{NodeID: "gapped", Problem: problem("b"), Gap: true},
		{NodeID: "cached", Problem: problem("c"), CacheHit: true},
		{NodeID: "internal", Problem: problem("d"), Children: []string{"x"}},
		{NodeID: "solved-empty", Problem: problem("e"), Model: "m", Verified: &no},
		{NodeID: "answered", Problem: problem("f"), Model: "m", Content: "text"},
	}}
	got := r.Unfunded()
	if len(got) != 1 || got[0].NodeID != "unfunded" {
		var ids []string
		for _, o := range got {
			ids = append(ids, o.NodeID)
		}
		t.Fatalf("Unfunded must name exactly the node that reached no model, got %v", ids)
	}
}

func TestARunThatCalledNoModelAtAllIsStillTruncated(t *testing.T) {
	// The spend counterpart of an all-gap run: a single root priced out below the floor.
	// It has NO gaps — only time gaps (§3.1) — so anything deciding extend-versus-refine
	// on Gaps alone would call this a complete run and offer it a refine, re-planning a
	// decomposition that was never given the money to prove itself (§8.1).
	r := RunRecord{Outcomes: []NodeOutcome{
		{NodeID: "n0", Problem: problem("root"), BaseCase: BaseBelowFloor},
	}}
	if len(r.Gaps()) != 0 {
		t.Fatal("a below-floor node must not be a gap — only time gaps (§3.1)")
	}
	if !r.Truncated() {
		t.Error("a run whose root was priced out is truncated: it stopped short of what it " +
			"set out to do, and needs money rather than a deadline raise")
	}
}

func TestTheRecordCarriesTheBoundsTheTreeGrewUnder(t *testing.T) {
	// P8: the record is self-sufficient — replayable without asking the environment
	// anything. THREE defects said otherwise before this field existed. BoundBy, the depth
	// bound and the floor were each re-derived by a replay from the tree's geometry, and
	// each is really a fact of the original EXECUTION: the depth cap is visible only if some
	// node hit it, and the floor is not visible at all.
	//
	// A knob that changes which base case a node takes is exactly what self-sufficiency has
	// to cover, so it is recorded rather than inferred.
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		Planner:    StaticPlanner{P: fanoutPlan("alpha", "beta")},
		Solver:     ProviderSolver{Provider: &fakeProvider{cost: FromFloat(1)}, Model: "fake"},
		Reducer:    ConcatReducer{Sep: "|"},
		Now:        now,
		MaxDepth:   1,
		Floor:      FromFloat(0.25),
		MaxRetries: 2,
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRunRecord(res, problem("root"), caps, ModeFresh)

	if rec.Bounds.MaxDepth != 1 {
		t.Errorf("MaxDepth must be recorded: want 1, got %d", rec.Bounds.MaxDepth)
	}
	if rec.Bounds.Floor != FromFloat(0.25) {
		t.Errorf("Floor must be recorded — it is invisible in the tree unless a node hit it: "+
			"want %s, got %s", FromFloat(0.25), rec.Bounds.Floor)
	}
	if rec.Bounds.MaxRetries != 2 {
		t.Errorf("MaxRetries must be recorded: want 2, got %d", rec.Bounds.MaxRetries)
	}
}

func TestTheResolvedDepthBoundIsRecordedNotTheZeroValue(t *testing.T) {
	// An executor left at MaxDepth 0 runs under DefaultMaxDepth, so recording the raw field
	// would write a bound that was never in force — and a replay reading it would apply
	// no bound at all, or fall back to inferring one. The RESOLVED value is the fact.
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, _ := NewLedger(caps, Scope{})
	e := &Executor{
		Planner: DeclinePlanner{},
		Solver:  ProviderSolver{Provider: &fakeProvider{cost: FromFloat(1)}, Model: "fake"},
		Reducer: ConcatReducer{},
		Now:     now,
		// MaxDepth deliberately unset.
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRunRecord(res, problem("root"), caps, ModeFresh)
	if rec.Bounds.MaxDepth != DefaultMaxDepth {
		t.Errorf("an unset MaxDepth must record the default that actually applied (%d), got %d",
			DefaultMaxDepth, rec.Bounds.MaxDepth)
	}
}

func TestAReplayInheritsBoundsRatherThanRederivingThem(t *testing.T) {
	// Bounds are what the replay was CONFIGURED FROM, so re-deriving them from the replay's
	// own executor would be circular — agreeing by construction and proving nothing. Same
	// reason BoundBy is inherited (see ReplayRecord).
	orig, _ := runFor(t, &fakeProvider{cost: FromFloat(3)})
	if orig.Bounds.MaxDepth == 0 {
		t.Fatal("fixture must record a depth bound for this to test inheritance")
	}
	// A replay whose executor was configured differently must still report the ORIGINAL's
	// bounds, because the record describes that run.
	res := Result{
		Outcomes: orig.Outcomes,
		Bounds:   RunBounds{MaxDepth: 99, Floor: FromFloat(9), MaxRetries: 9},
	}
	replayed := ReplayRecord(res, orig)
	if replayed.Bounds != orig.Bounds {
		t.Errorf("a replayed record must carry the ORIGINAL bounds, got %+v want %+v",
			replayed.Bounds, orig.Bounds)
	}
}

func TestASpendTruncatedRunReplaysAsSpendTruncated(t *testing.T) {
	// FOUND BY THE FIRST LIVE BEDROCK RUN, and it is the gap defect wearing the other cap.
	// A 28-node run under a $0.25 cap left 4 nodes unfunded — empty content, no model, and
	// NO Gap flag, because only TIME is a gap (§3.1) — and `quarry replay` failed with
	// "no recorded sample", reporting a divergence against a faithful record. Same symptom
	// as the gap case, different category, and spend is the cap researchers actually set,
	// so this was the more common half.
	//
	// WHY --fake COULD NOT FIND IT: the fake's per-call cost is uniform, so the planner's
	// affordability check either funds every child or declines the split. A tree with SOME
	// children priced out needs costs that differ per sub-question, which needs a real
	// price sheet. This fixture gets there in-core with a weighted plan instead.
	orig, _, _ := truncRun(t, FromFloat(60))

	// Non-vacuity, both halves: unfunded nodes present, and NOT as gaps. If the executor
	// ever started flagging them Gap this test would silently become the gap test.
	if len(orig.Gaps()) != 0 {
		t.Fatalf("fixture must be SPEND-truncated: only time is a gap (§3.1), got %d gaps",
			len(orig.Gaps()))
	}
	var unfunded int
	for _, o := range orig.Outcomes {
		if o.Model == "" && o.Content == "" && o.Verified == nil && len(o.Children) == 0 {
			unfunded++
		}
	}
	if unfunded == 0 {
		t.Fatal("fixture must leave at least one node unfunded, or this tests nothing")
	}

	l, err := NewLedger(orig.Caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		Planner:  StaticPlanner{P: weightedPlan("alpha", 9, "beta", 1)},
		Solver:   ProviderSolver{Provider: NewRecordedProvider(orig), Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      now,
		MaxDepth: 1,
		// NO Estimate, matching `quarry replay` exactly — and this is the whole reason the
		// test works. My first version wired the run's estimate here, which made ADMISSION
		// refuse the unfunded node before it ever reached the provider: the record replayed
		// byte-identically with the fix reverted, because the lookup under test never
		// happened. A fixture that pre-empts the seam it is checking proves nothing.
		//
		// With no estimate, admission passes and the provider is asked — which is what the
		// CLI does, and where the divergence actually lived.
	}
	res, err := e.Run(context.Background(), orig.Problem, l)
	if err != nil {
		t.Fatalf("a spend-truncated record must replay, not report divergence: %v", err)
	}
	replayed := ReplayRecord(res, orig)

	ob, _ := orig.Canonical()
	rb, _ := replayed.Canonical()
	if !bytes.Equal(ob, rb) {
		t.Errorf("a spend-truncated run must replay byte-identically\n orig: %s\n rep:  %s", ob, rb)
	}
	// And the unfunded node must NOT come back as a gap. That is the distinction the
	// separate sentinel exists for: relabelling spend degradation as time truncation would
	// make Extend offer a deadline raise where money was needed (§8.1).
	if len(replayed.Gaps()) != 0 {
		t.Errorf("an unfunded node must not replay as a time gap (§3.1), got %d gaps",
			len(replayed.Gaps()))
	}
}

func TestAnUnfundedNodeIsNotReplayedAsAGap(t *testing.T) {
	// The sentinel-level assertion, isolated from the tree. ErrRecordedGap and
	// ErrRecordedUnfunded must stay distinct: the executor's ErrRecordedGap path sets
	// Gap, so serving an unfunded node through it would relabel the category. Three
	// separate lookups, because "not a sample" is three different answers.
	orig, _, _ := truncRun(t, FromFloat(60))
	rp := NewRecordedProvider(orig)

	// Find the node the cap could not fund, from the record rather than by name.
	var unfunded Problem
	for _, o := range orig.Outcomes {
		if o.Model == "" && o.Content == "" && o.Verified == nil && len(o.Children) == 0 {
			unfunded = o.Problem
			break
		}
	}
	if unfunded.Statement == "" {
		t.Fatal("fixture must contain an unfunded node")
	}

	_, err := rp.Complete(context.Background(), unfunded.Statement, "fake", unfunded.Scope)
	if !errors.Is(err, ErrRecordedUnfunded) {
		t.Errorf("an unfunded node must replay as ErrRecordedUnfunded, got %v", err)
	}
	if errors.Is(err, ErrRecordedGap) {
		t.Error("spend degradation must not be served as a time gap — that is the one " +
			"distinction §3.1's standing ruling turns on")
	}
	// And an absent call is still neither.
	_, err = rp.Complete(context.Background(), "never-asked", "fake", Scope{})
	if errors.Is(err, ErrRecordedUnfunded) || errors.Is(err, ErrRecordedGap) {
		t.Error("an absent call must remain a plain divergence, or a changed tree shape " +
			"becomes undetectable")
	}
	// P6: an unfunded node is scope-qualified like everything else.
	if _, err := rp.Complete(context.Background(), unfunded.Statement, "fake",
		Scope{Tags: map[string]string{"lab": "other"}}); errors.Is(err, ErrRecordedUnfunded) {
		t.Error("an unfunded node must be scope-qualified (P6)")
	}
}

func TestAnUnrecordedCallIsStillADivergence(t *testing.T) {
	// The other side of the fix above: making gaps replayable must NOT make every
	// unknown prompt look like a gap. If it did, replay would stop being able to detect
	// that the tree changed shape — which is the only thing it is for.
	orig, _ := partialRun(t, FromFloat(1))
	if len(orig.Gaps()) == 0 {
		t.Fatal("fixture must produce a gap for the first assertion to mean anything")
	}
	rp := NewRecordedProvider(orig)

	// A recorded gap: reported as a gap, so the executor can reproduce it.
	if _, err := rp.Complete(context.Background(), "beta", "fake", Scope{}); !errors.Is(err, ErrRecordedGap) {
		t.Errorf("a recorded gap must replay as ErrRecordedGap, got %v", err)
	}
	// Never asked: still a divergence, and NOT a gap.
	_, err := rp.Complete(context.Background(), "never-asked", "fake", Scope{})
	if err == nil {
		t.Fatal("a call the record does not contain must surface as divergence")
	}
	if errors.Is(err, ErrRecordedGap) {
		t.Error("an absent call must not be mistaken for a recorded gap — that would make " +
			"a changed tree shape undetectable")
	}
	// P6: a gap must not be served across a scope boundary either.
	if _, err := rp.Complete(context.Background(), "beta", "fake",
		Scope{Tags: map[string]string{"lab": "other"}}); errors.Is(err, ErrRecordedGap) {
		t.Error("a recorded gap must be scope-qualified like a sample (P6)")
	}
}

func TestReplayTwiceIsStable(t *testing.T) {
	// Replaying the replay is still identical — determinism is not a one-shot
	// coincidence of the first re-run.
	orig, _ := runFor(t, &fakeProvider{cost: FromFloat(2)})
	r1, _ := runFor(t, NewRecordedProvider(orig))
	r2, _ := runFor(t, NewRecordedProvider(r1))
	b1, _ := r1.Canonical()
	b2, _ := r2.Canonical()
	if !bytes.Equal(b1, b2) {
		t.Error("replay of a replay must remain byte-identical")
	}
}

// ---------------------------------------------------- the unverified list (§8)

func TestRecordListsWhatWasNotVerified(t *testing.T) {
	// §8: the receipt must say what was NOT checked, distinct from checked-and-
	// passed. With no verifier, every node is unverified.
	rec, res := runFor(t, &fakeProvider{cost: FromFloat(1)})
	if len(rec.Unverified) != len(res.Outcomes) {
		t.Errorf("with no verifier every node is unverified: want %d, got %d",
			len(res.Outcomes), len(rec.Unverified))
	}
}

func TestVerifiedNodesAreNotListedUnverified(t *testing.T) {
	// The complement: a node a verifier passed is not in the unverified list.
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, _ := NewLedger(caps, Scope{})
	e := &Executor{
		Planner:  DeclinePlanner{},
		Solver:   ProviderSolver{Provider: &fakeProvider{cost: FromFloat(1)}, Model: "fake"},
		Reducer:  ConcatReducer{},
		Now:      now,
		MaxDepth: 1,
		Verifier: NonEmptyOracle(),
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRunRecord(res, root, caps, ModeFresh)
	if len(rec.Unverified) != 0 {
		t.Errorf("a verified leaf must not be listed unverified, got %v", rec.Unverified)
	}
}

// --------------------------------------------------------- record contents

func TestRecordCarriesModeAndBoundBy(t *testing.T) {
	rec, _ := runFor(t, &fakeProvider{cost: FromFloat(1)})
	if rec.Mode != ModeFresh {
		t.Errorf("want ModeFresh, got %q", rec.Mode)
	}
	// A slack budget means neither cap bit — BoundBy is empty, which is itself
	// information (§8.2).
	if rec.BoundBy != "" {
		t.Errorf("an unbound run reports no binding cap, got %q", rec.BoundBy)
	}
}
