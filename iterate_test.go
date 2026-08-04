package quarry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// §8.1 invariants. The two operations are distinguished by what they carry (a
// planner versus evidence) and by what they refuse, so most of these assert on the
// Iteration rather than on a run — which is also why they need no provider.

var iterCaps = Caps{Spend: FromFloat(100), Latency: time.Hour}

// truncRun produces a genuinely spend-truncated record: TWO children, unequally
// weighted, priced so the heavy one is funded and the light one is not. Both halves
// matter — a completed child gives the extend something to serve from cache, and an
// unfunded one gives it something to refill. Weighted 9:1 rather than evenly
// because equal shares of this pool clear the estimate for neither child, and a run
// where nothing at all was solved cannot demonstrate delta pricing.
func truncRun(t *testing.T, spend Units) (RunRecord, *fakeProvider, *MemCache) {
	t.Helper()
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	e := exec(t, StaticPlanner{P: weightedPlan("alpha", 9, "beta", 1)}, prov)
	e.MaxDepth = 1
	e.Cache = cache
	e.Estimate = func(Problem) Units { return FromFloat(20) }

	caps := Caps{Spend: spend, Latency: time.Hour}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, problem("root"), caps, ModeFresh), prov, cache
}

// wholeRun produces a completed record with no truncation of any kind.
func wholeRun(t *testing.T) RunRecord {
	t.Helper()
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("alpha", "beta")}, prov)
	e.MaxDepth = 1
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, problem("root"), caps, ModeFresh)
}

// childrenOutOfTimeRun produces the case the CLI hit: every CHILD ran out of time
// while the ROOT context is still live.
//
// That gap between the two windows is structural, not incidental. ChildContext withholds
// DefaultTimeReserveBP so the reducer has time to merge (§3.1), so a child's window is
// strictly shorter than its parent's — and the root's is the whole cap. A deadline that
// truncates the entire fanout therefore leaves ctx.Err() nil at the root, which is
// precisely why reading BoundBy from the root context reported nothing.
//
// Distinct from partialRun, which cancels the root and so cannot see this: there,
// BoundBy(ctx, l) already answers "latency" and the fallback is never reached. The two
// fixtures are the two halves of one question, and only this one was missing.
func childrenOutOfTimeRun(t *testing.T) RunRecord {
	t.Helper()
	// A real clock, for the reason partialRun documents: ChildContext derives the child
	// window as now.Add(remaining−reserve), so the injected instant and the context
	// deadline must be read at the same moment or the arithmetic is nonsense.
	start := time.Now()
	// Generous by test standards and deliberately so. Every leaf hangs until its own
	// window closes, so this is the fixture's entire wall-clock cost — and making it tight
	// is what turned an earlier fixture into a -race coin flip.
	const window = 300 * time.Millisecond
	caps := Caps{Spend: FromFloat(1000), Latency: window}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		Planner: StaticPlanner{P: fanoutPlan("alpha", "beta")},
		// Every prompt hangs: no leaf returns, so the whole fanout gaps. Nothing to
		// coordinate and no ordering to get right — which is why this fixture needs no
		// cancellation channel.
		Solver: ProviderSolver{Provider: &gappyProvider{
			cost: FromFloat(3), hang: map[string]bool{"alpha": true, "beta": true},
		}, Model: "fake"},
		Reducer:  ConcatReducer{Sep: "|"},
		Now:      start,
		MaxDepth: 1,
	}
	ctx, cancel := RootContext(context.Background(), caps, start)
	defer cancel()
	res, err := e.Run(ctx, problem("root"), l)
	if err != nil {
		t.Fatalf("a time miss must not fail the run (§3.1): %v", err)
	}
	// The ROOT must still be live, or the fixture is partialRun by another name and the
	// fallback under test is dead code.
	if ctx.Err() != nil {
		t.Fatalf("fixture invariant: the root window (%s) must outlast its children's, "+
			"or this tests the same path partialRun already covers", window)
	}
	return NewRunRecord(res, problem("root"), caps, ModeFresh)
}

// ------------------------------------------------------------------- extend

func TestExtendRefusesACompletedRun(t *testing.T) {
	// The central extend/refine distinction, enforced rather than documented. A
	// completed run's decomposition, pinned and handed more money, serves every node
	// from cache and refills nothing — the caller pays for a copy of the predecessor.
	// That failure is silent unless this refuses.
	whole := wholeRun(t)
	if whole.Truncated() {
		t.Fatalf("the fixture is not a completed run (bound by %q); the test probes nothing",
			whole.BoundBy)
	}
	_, err := Extend(whole, Caps{Spend: FromFloat(5000), Latency: time.Hour})
	if !errors.Is(err, ErrNothingToExtend) {
		t.Errorf("extending a completed run must fail with ErrNothingToExtend, got %v", err)
	}
}

func TestExtendAcceptsASpendTruncatedRunThatHasNoGaps(t *testing.T) {
	// The case Gaps() alone gets wrong. Only TIME is a gap (§3.1), so a run that hit
	// its spend cap and dropped children has NO gaps while being the clearest possible
	// extend candidate — and spend is the cap researchers actually set. Deciding on
	// Gaps alone would route the common case to refine and re-plan a decomposition
	// that was never given the money to prove itself.
	rec, _, _ := truncRun(t, FromFloat(60))
	if len(rec.Gaps()) != 0 {
		t.Fatalf("fixture has %d gaps; it was supposed to be spend-truncated, so this test "+
			"no longer distinguishes Truncated from Gaps", len(rec.Gaps()))
	}
	if !rec.Truncated() {
		t.Fatal("a run whose children were priced out is truncated")
	}
	it, err := Extend(rec, iterCaps)
	if err != nil {
		t.Fatalf("a spend-truncated run must be extendable: %v", err)
	}
	if it.Mode != ModeExtend {
		t.Errorf("mode = %q, want %q", it.Mode, ModeExtend)
	}
}

func TestExtendRefusesACapItCannotUse(t *testing.T) {
	// An extend at the same ceiling re-derives the same truncation, so it is refused
	// rather than run — the caller would pay for a second identical failure.
	rec, _, _ := truncRun(t, FromFloat(60))
	_, err := Extend(rec, rec.Caps)
	if !errors.Is(err, ErrCapNotRaised) {
		t.Errorf("an unchanged cap must fail with ErrCapNotRaised, got %v", err)
	}
}

func TestATimeTruncatedRunRecordsThatTimeBoundIt(t *testing.T) {
	// FOUND BY RUNNING THE BINARY, and the reason it survived is one line below in
	// TestExtendComparesTheCapThatActuallyBound: that test ASSIGNS BoundBy = DenomLatency
	// by hand, so it proves Extend reads the field correctly while proving nothing about
	// whether a real run ever sets it. It did not. A 4-node fake run at --deadline 60ms
	// gapped every node and recorded BoundBy "".
	//
	// The cause: the exported BoundBy inspects the ROOT context, whose window is the whole
	// cap, while each child gets a slice of it — so the root finishes inside a deadline
	// that truncated everything beneath it (§3.1).
	//
	// The consequence is not cosmetic. With BoundBy empty, capsAllowExtend takes its
	// unknown-binding branch and accepts ANY raise, so a purely time-truncated run
	// accepted a spend raise that could not refill a single gapped node — the exact
	// mistake the test below exists to prevent, arriving through the front door.
	rec := childrenOutOfTimeRun(t)
	if len(rec.Gaps()) == 0 {
		t.Fatal("fixture must produce a gap for this to test anything")
	}
	if rec.BoundBy != DenomLatency {
		t.Fatalf("a run with %d time-truncated nodes must record latency as the binding cap, got %q",
			len(rec.Gaps()), rec.BoundBy)
	}
	// And the consequence, end to end: the raise that cannot help must now be refused.
	if _, err := Extend(rec, Caps{Spend: rec.Caps.Spend * 10, Latency: rec.Caps.Latency}); !errors.Is(err, ErrCapNotRaised) {
		t.Errorf("more money cannot refill a node that ran out of time; want ErrCapNotRaised, got %v", err)
	}
}

func TestExtendComparesTheCapThatActuallyBound(t *testing.T) {
	// Raising the wrong denomination changes nothing: more money will not refill a
	// node that ran out of TIME. So the comparison is against BoundBy (§8.2), not
	// against whichever cap the caller happened to change.
	//
	// BoundBy is assigned here rather than produced by a run, deliberately: this isolates
	// Extend's arithmetic from the question of who populates the field. The test above
	// covers the other half, and exists because for a while nothing did.
	rec, _, _ := truncRun(t, FromFloat(60))
	rec.BoundBy = DenomLatency
	rec.Caps = Caps{Spend: FromFloat(60), Latency: time.Hour}

	// Ten times the money, same deadline: refused, because latency is what bit.
	if _, err := Extend(rec, Caps{Spend: FromFloat(600), Latency: time.Hour}); !errors.Is(err, ErrCapNotRaised) {
		t.Errorf("raising spend on a latency-bound run must be refused, got %v", err)
	}
	// More time: accepted.
	if _, err := Extend(rec, Caps{Spend: FromFloat(60), Latency: 2 * time.Hour}); err != nil {
		t.Errorf("raising latency on a latency-bound run must be accepted, got %v", err)
	}
}

func TestExtendCarriesAPlannerAndRefineDoesNot(t *testing.T) {
	// The asymmetry IS the distinction, made structural: an extend carries a planner
	// and no evidence (it re-plans nothing), a refine carries evidence and no planner
	// (it re-plans everything). A refine that shipped a PinnedPlanner would be an
	// extend wearing the wrong label, spending new budget on a decomposition already
	// judged to be split in the wrong places.
	rec, _, _ := truncRun(t, FromFloat(60))

	ext, err := Extend(rec, iterCaps)
	if err != nil {
		t.Fatal(err)
	}
	if ext.Planner == nil {
		t.Error("extend must carry the pinned prior decomposition")
	}
	if len(ext.Prior) != 0 {
		t.Errorf("extend re-plans nothing, so it must carry no distilled prior; got %d nodes",
			len(ext.Prior))
	}

	ref := Refine(rec, iterCaps, nil)
	if ref.Planner != nil {
		t.Error("refine must NOT carry a pinned planner — it re-plans from scratch (§8.1)")
	}
	if ref.Mode != ModeRefine {
		t.Errorf("mode = %q, want %q", ref.Mode, ModeRefine)
	}
}

func TestExtendServesCompletedSubtreesFromTheSharedCache(t *testing.T) {
	// The economic claim §8.1 makes for the cache's complexity: an extend pays the
	// DELTA, not the total. Completed work must come back for free, which requires the
	// caller to pass the prior run's cache — and requires the cache never to have
	// stored the incomplete nodes (see appendCache).
	rec, prov, cache := truncRun(t, FromFloat(60))
	firstCalls := prov.calls
	if firstCalls == 0 {
		t.Fatal("the first run solved nothing; there is no completed work to serve")
	}

	it, err := Extend(rec, iterCaps)
	if err != nil {
		t.Fatal(err)
	}
	e := exec(t, nil, prov)
	e.MaxDepth = 1
	e.Cache = cache // THE PRIOR RUN'S CACHE — without it the delta becomes the total
	l, err := NewLedger(it.Caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := runIteration(context.Background(), e, it, l)
	if err != nil {
		t.Fatal(err)
	}

	// The child the first run DID fund is served; only the unfunded one is solved.
	if got := prov.calls - firstCalls; got != 1 {
		t.Errorf("extend must re-solve only the unfilled child: want 1 new call, got %d", got)
	}
	var hits int
	for _, o := range rec2.Outcomes {
		if o.CacheHit {
			hits++
		}
	}
	if hits == 0 {
		t.Error("extend served nothing from cache — the delta pricing of §8.1 is not happening")
	}
	if rec2.Mode != ModeExtend {
		t.Errorf("the extended record must say what produced it: mode = %q", rec2.Mode)
	}
}

func TestAnExtendFillsWhatTheTruncatedRunLeftEmpty(t *testing.T) {
	// The outcome that matters to a researcher: the second run answers what the first
	// could not. Asserted on content rather than on call counts, because a run that
	// makes the right number of calls and still returns nothing has not extended
	// anything.
	rec, prov, cache := truncRun(t, FromFloat(60))
	// The truncated root answered from ONE of its two children — a half-answer, which
	// is the interesting case rather than an empty one: it has content, and since only
	// time is a gap (§3.1) it carries no Gap flag either, so nothing about the outcome
	// alone announces that it is incomplete.
	before := rec.Outcomes[0].Content
	if !strings.Contains(before, "alpha") || strings.Contains(before, "beta") {
		t.Fatalf("fixture root = %q; expected the funded child only, so this test no longer "+
			"probes a half-answer being completed", before)
	}
	it, err := Extend(rec, iterCaps)
	if err != nil {
		t.Fatal(err)
	}
	e := exec(t, nil, prov)
	e.MaxDepth = 1
	e.Cache = cache
	l, err := NewLedger(it.Caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := runIteration(context.Background(), e, it, l)
	if err != nil {
		t.Fatal(err)
	}
	after := rec2.Outcomes[0].Content
	if !strings.Contains(after, "beta") {
		t.Errorf("the extended answer must include the child the first run could not fund: %q", after)
	}
	if !strings.Contains(after, "alpha") {
		t.Errorf("the extended answer must retain what the first run did fund: %q", after)
	}
	if rec2.Truncated() {
		t.Errorf("the extended run is still truncated (bound by %q)", rec2.BoundBy)
	}
}

// ------------------------------------------------------------------- lineage

func TestAnIterationRecordNamesItsPredecessor(t *testing.T) {
	// P8: the citable artifact may be a LINEAGE rather than a single record, which
	// requires each record to name the one it came from. NewRunRecord cannot set these
	// fields, so a record assembled with it is indistinguishable from a fresh run —
	// the chain breaks silently at the point it was meant to prove continuity.
	rec, _, _ := truncRun(t, FromFloat(60))
	it, err := Extend(rec, iterCaps)
	if err != nil {
		t.Fatal(err)
	}
	child := it.Record(Result{Outcomes: []NodeOutcome{{NodeID: "n0", Content: "x"}}})

	if child.ParentRun != rec.RunID {
		t.Errorf("ParentRun = %q, want the predecessor's hash %q", child.ParentRun, rec.RunID)
	}
	if child.RunID == rec.RunID {
		t.Error("a follow-up run must have its own identity")
	}
	if child.RunID == "" {
		t.Error("an iteration record must be content-hashed like any other (P8)")
	}
}

func TestALineOfInquiryIsNamedForItsRootRun(t *testing.T) {
	// A first run has no line; the first iteration creates one, named for the root run
	// so the handle stays stable however long the chain grows. Cumulative spend is then
	// a sum over records sharing it — which is what a PI means by "what did this
	// question cost".
	first, _, _ := truncRun(t, FromFloat(60))
	if first.LineOfInquiry != "" {
		t.Fatal("a fresh run should not already belong to a line")
	}
	it, err := Extend(first, iterCaps)
	if err != nil {
		t.Fatal(err)
	}
	second := it.Record(Result{Outcomes: []NodeOutcome{{NodeID: "n0", Content: "x", Cost: FromFloat(7)}}})
	if second.LineOfInquiry != first.RunID {
		t.Errorf("LineOfInquiry = %q, want the root run's ID %q", second.LineOfInquiry, first.RunID)
	}

	// A third run inherits the SAME handle rather than starting a new line at its own
	// parent — otherwise every iteration would begin a fresh line and cumulative
	// accounting would only ever see two records.
	third := Refine(second, iterCaps, nil).
		Record(Result{Outcomes: []NodeOutcome{{NodeID: "n0", Content: "y", Cost: FromFloat(11)}}})
	if third.LineOfInquiry != first.RunID {
		t.Errorf("the line must survive a second hop: got %q, want %q",
			third.LineOfInquiry, first.RunID)
	}
	if third.ParentRun != second.RunID {
		t.Errorf("ParentRun must be the immediate predecessor, got %q", third.ParentRun)
	}
}

func TestLineCostSumsTheQuestionNotTheInvocation(t *testing.T) {
	// §8.1's cross-run accounting. Records outside the line are ignored rather than
	// rejected so a caller can pass a whole store.
	line := "line-a"
	records := []RunRecord{
		{RunID: "r1", LineOfInquiry: line, Outcomes: []NodeOutcome{{Cost: FromFloat(10)}}},
		{RunID: "r2", LineOfInquiry: line, Outcomes: []NodeOutcome{{Cost: FromFloat(5)}}},
		{RunID: "other", LineOfInquiry: "line-b", Outcomes: []NodeOutcome{{Cost: FromFloat(999)}}},
	}
	total, n := LineCost(line, records)
	if n != 2 {
		t.Errorf("want 2 records in the line, got %d", n)
	}
	if total != FromFloat(15) {
		t.Errorf("cumulative cost = %s, want 15", total)
	}
}

func TestACacheHitCostsTheLineNothing(t *testing.T) {
	// The delta pricing of §8.1 seen from the accounting side: a served node
	// contributes zero, because the tokens were paid for once in the run that produced
	// them. If a hit were charged again, a line's cumulative cost would grow on
	// re-reads and the cache would look expensive rather than free.
	line := "l"
	records := []RunRecord{
		{RunID: "r1", LineOfInquiry: line, Outcomes: []NodeOutcome{{Cost: FromFloat(10)}}},
		{RunID: "r2", LineOfInquiry: line, Outcomes: []NodeOutcome{
			{CacheHit: true, HaloTokens: 800, GeneratedTokens: 200}, // real tokens, no charge
		}},
	}
	if total, _ := LineCost(line, records); total != FromFloat(10) {
		t.Errorf("a served node must add nothing to the line: got %s, want 10", total)
	}
}

// ------------------------------------------------------------------- distill

// distillFixture is a record with one expensive node, one cheap one, one that
// failed verification, and one that behaved exactly as planned.
func distillFixture() (RunRecord, []ClaimStability) {
	no, yes := false, true
	rec := RunRecord{
		RunID:   "prior",
		Problem: problem("root"),
		Outcomes: []NodeOutcome{
			{NodeID: "n0", Problem: problem("root"), Content: "merged", Cost: FromFloat(2),
				Children: []string{"n0.0", "n0.1", "n0.2"}, Depth: 0, Strategy: StrategyPartition},
			{NodeID: "n0.0", Problem: problem("cheap"), Content: "a", Cost: FromFloat(1),
				PlanWeight: 5, Depth: 1, Verified: &yes},
			{NodeID: "n0.1", Problem: problem("expensive"), Content: "b", Cost: FromFloat(50),
				PlanWeight: 1, Depth: 1, Verified: &yes, HaloTokens: 9000, GeneratedTokens: 50},
			{NodeID: "n0.2", Problem: problem("failed"), Content: "c", Cost: FromFloat(3),
				PlanWeight: 2, Depth: 1, Verified: &no, Retries: 2},
			{NodeID: "n0.3", Problem: problem("free"), Content: "d", Cost: 0,
				PlanWeight: 1, Depth: 1, Verified: &yes},
		},
	}
	unstable := []ClaimStability{
		{Claim: Claim{Text: "shaky claim", Norm: "shaky claim", NodeID: "n0.1"}, Support: 1, Total: 3},
	}
	return rec, unstable
}

func TestDistillWithholdsTheTreeShape(t *testing.T) {
	// THE UNRESOLVED TENSION, handled by declining to guess (§8.1, §12). Showing the
	// planner the prior decomposition biases it toward that decomposition, and §7 names
	// an INDEPENDENT decomposition as the strongest replication signal available — so a
	// refine that anchored on the prior plan would destroy the best evidence the system
	// has, in the name of improving it. Shape is Children + NodeID + Depth + Strategy;
	// all four must be absent.
	rec, unstable := distillFixture()
	for _, d := range Distill(rec, unstable) {
		if len(d.Children) != 0 {
			t.Errorf("%q leaked its children — that is the tree shape", d.Problem.Statement)
		}
		if d.NodeID != "" {
			t.Errorf("%q leaked a node ID, which reconstructs the tree by prefix", d.Problem.Statement)
		}
		if d.Depth != 0 {
			t.Errorf("%q leaked its depth", d.Problem.Statement)
		}
		if d.Strategy != "" {
			t.Errorf("%q leaked the parent's strategy", d.Problem.Statement)
		}
	}
}

func TestDistillCarriesDifficultyAgainstItsOwnPriorWeight(t *testing.T) {
	// §8.1's first signal, and the one PlanWeight was added for: "expensive" alone is
	// not a correction on the planner's weighting — "expensive RELATIVE TO WHAT IT WAS
	// EXPECTED TO COST" is. A node weighted 1 that cost 50 is the planner's mistake
	// made legible; without the weight it is just a big number.
	rec, unstable := distillFixture()
	var found bool
	for _, d := range Distill(rec, unstable) {
		if d.Problem.Statement == "expensive" {
			found = true
			if d.Cost != FromFloat(50) {
				t.Errorf("cost = %s, want 50", d.Cost)
			}
			if d.PlanWeight != 1 {
				t.Errorf("PlanWeight = %d, want 1 — difficulty is meaningless without it", d.PlanWeight)
			}
		}
	}
	if !found {
		t.Error("the most expensive node must reach the planner")
	}
}

func TestDistillDropsNodesThatTaughtNothing(t *testing.T) {
	// A node that cost nothing, passed its check and was stable is not evidence; it is
	// halo (P1). The planner pays in context for everything it is shown, so the
	// distillate must be a filter, not a reformat.
	rec, unstable := distillFixture()
	for _, d := range Distill(rec, unstable) {
		if d.Problem.Statement == "free" {
			t.Error("a node that behaved exactly as planned must not reach the planner")
		}
	}
}

func TestDistillCarriesOnlyTheUnstableClaims(t *testing.T) {
	// §7's targeting signal: the first run measures where the instrument is at its
	// limit and the second spends precisely there. The STABLE claims are the part a
	// refine need not revisit, so including them would spend halo re-describing
	// settled ground.
	rec, unstable := distillFixture()
	var claims int
	for _, d := range Distill(rec, unstable) {
		claims += len(d.Claims)
		for _, c := range d.Claims {
			if c.Text != "shaky claim" {
				t.Errorf("a stable claim reached the planner: %q", c.Text)
			}
			if d.Problem.Statement != "expensive" {
				t.Errorf("claim %q attached to the wrong node %q", c.Text, d.Problem.Statement)
			}
		}
	}
	if claims != 1 {
		t.Errorf("want the 1 unstable claim carried, got %d", claims)
	}
}

func TestDistillKeepsFailedVerificationAndRetries(t *testing.T) {
	// §8.1's third signal. A node that failed its check, or needed three attempts, was
	// harder than its cost alone shows — and "which nodes failed verification" is
	// named explicitly as something the re-planning planner should see.
	rec, unstable := distillFixture()
	var found bool
	for _, d := range Distill(rec, unstable) {
		if d.Problem.Statement == "failed" {
			found = true
			if d.Verified == nil || *d.Verified {
				t.Error("the failed verdict must survive distillation")
			}
			if d.Retries != 2 {
				t.Errorf("Retries = %d, want 2 — retries are difficulty evidence", d.Retries)
			}
		}
	}
	if !found {
		t.Error("a node that failed verification must reach the planner")
	}
}

func TestDistillIsDeterministic(t *testing.T) {
	// P8: replay must be byte-stable, so the distillate cannot depend on map iteration
	// order or on an unstable sort. Expensive-first is also the order a
	// budget-conditioned planner (P9) should read them in.
	rec, unstable := distillFixture()
	first := Distill(rec, unstable)
	for i := 0; i < 20; i++ {
		got := Distill(rec, unstable)
		if len(got) != len(first) {
			t.Fatalf("length varies across calls: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j].Problem.Key() != first[j].Problem.Key() {
				t.Fatalf("order varies at %d: %q vs %q", j, got[j].Problem.Statement,
					first[j].Problem.Statement)
			}
		}
	}
	if len(first) > 1 && first[0].Cost < first[1].Cost {
		t.Error("the distillate must be expensive-first")
	}
}

func TestARefineWithNoReplicatesSaysSoRatherThanClaimingStability(t *testing.T) {
	// Instability needs REPLICATES, which a single run does not have. Nil unstable is
	// therefore the honest input after one run, not a degraded one — and it must not
	// silently read as "everything was stable", which is the reading that would send a
	// refine looking for nothing to fix.
	rec, _ := distillFixture()
	for _, d := range Distill(rec, nil) {
		if len(d.Claims) != 0 {
			t.Errorf("with no replicates there is no stability verdict to carry, got %d claims on %q",
				len(d.Claims), d.Problem.Statement)
		}
		// The difficulty signal still arrives: a refine after one run is not evidence-free.
		if d.Problem.Statement == "expensive" && d.Cost == 0 {
			t.Error("difficulty actuals must survive even with no stability data")
		}
	}
}

// ------------------------------------------------------------------- guards

func TestRunIterationRefusesToRunARefine(t *testing.T) {
	// The helper handles extend only, because a refine's planner is the CALLER's and
	// how the prior reaches it is the anchoring question this file declines to answer.
	// Refusing beats silently running a refine with no planner — which would inherit
	// whatever planner the executor already had and produce a run whose mode field
	// lies about what it did.
	rec, _, _ := truncRun(t, FromFloat(60))
	ref := Refine(rec, iterCaps, nil)
	e := exec(t, DeclinePlanner{}, &fakeProvider{cost: FromFloat(1)})
	l, err := NewLedger(iterCaps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runIteration(context.Background(), e, ref, l); err == nil {
		t.Error("running a refine through the extend helper must fail loudly")
	} else if !strings.Contains(err.Error(), "planner") {
		t.Errorf("the error should name the missing planner, got %v", err)
	}
}

func TestAnIterationKeepsThePredecessorsProblem(t *testing.T) {
	// An iteration is a further attempt at the SAME question. A different question is
	// a fresh run, and letting the root problem drift would make the lineage assert a
	// continuity that does not hold.
	rec, _, _ := truncRun(t, FromFloat(60))
	ext, err := Extend(rec, iterCaps)
	if err != nil {
		t.Fatal(err)
	}
	if ext.Problem.Key() != rec.Problem.Key() {
		t.Errorf("extend changed the problem: %q vs %q", ext.Problem.Statement, rec.Problem.Statement)
	}
	if got := Refine(rec, iterCaps, nil); got.Problem.Key() != rec.Problem.Key() {
		t.Errorf("refine changed the problem: %q", got.Problem.Statement)
	}
}
