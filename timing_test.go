package quarry

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the two fields added to NodeOutcome for §9 and §8.2, and they
// exist mainly to hold ONE line: timing must never enter the hash. Token counts
// and durations were added together but have OPPOSITE reproducibility properties —
// a token count is a deterministic property of a call and belongs in the record; a
// duration differs on every run and would make every replay look like a divergence.
// If a future change hashes Timing, TestTimingIsExcludedFromTheHash fails, and that
// failure is the design speaking.

// stepClock returns a clock that advances a fixed step per call, so durations are
// deterministic and a test never reads the real time (Go rule 4).
func stepClock(start time.Time, step time.Duration) func() time.Time {
	var n int64
	return func() time.Time {
		i := atomic.AddInt64(&n, 1)
		return start.Add(time.Duration(i) * step)
	}
}

// timedRun builds the same two-leaf tree as record_test's runFor, but with a clock
// wired so per-node timing is recorded.
func timedRun(t *testing.T, prov Provider, clock func() time.Time) (RunRecord, Result) {
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
		Clock:    clock,
	}
	root := problem("root")
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, root, caps, ModeFresh), res
}

// ------------------------------------------------------------- timing and P8

func TestTimingIsExcludedFromTheHash(t *testing.T) {
	// THE invariant that made timing safe to add (P8, §12). Two runs of the same
	// tree under DIFFERENT clocks must still hash identically: a duration is the one
	// field replay cannot reproduce, so if it reached the canonical bytes, every
	// replay would report a divergence that did not happen.
	fast, _ := timedRun(t, &fakeProvider{cost: FromFloat(3)}, stepClock(now, time.Millisecond))
	slow, _ := timedRun(t, &fakeProvider{cost: FromFloat(3)}, stepClock(now.Add(time.Hour), time.Minute))

	fb, err := fast.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := slow.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fb, sb) {
		t.Fatalf("timing must not reach the canonical bytes\n fast: %s\n slow: %s", fb, sb)
	}
	if fast.RunID != slow.RunID {
		t.Errorf("differently-timed runs of the same tree must hash alike: %s vs %s", fast.RunID, slow.RunID)
	}
	// And the timings really were different — otherwise this test passes vacuously.
	fd, ok := fast.Outcomes[0].Timing.Duration()
	if !ok {
		t.Fatal("the fast run must have recorded a duration")
	}
	sd, _ := slow.Outcomes[0].Timing.Duration()
	if fd == sd {
		t.Fatal("test is vacuous: both clocks produced the same duration")
	}
}

func TestReplayIsByteIdenticalWithTimingRecorded(t *testing.T) {
	// The step-5 determinism guarantee must survive timing being present: run under
	// a clock, replay under a DIFFERENT clock, bytes still identical.
	orig, _ := timedRun(t, &fakeProvider{cost: FromFloat(3)}, stepClock(now, time.Second))
	replayed, _ := timedRun(t, NewRecordedProvider(orig), stepClock(now.Add(9*time.Hour), 7*time.Second))

	ob, err := orig.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := replayed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, rb) {
		t.Fatalf("replay must stay byte-identical with timing live\n orig: %s\n rep:  %s", ob, rb)
	}
}

func TestNoClockMeansNoTimingNotZeroTiming(t *testing.T) {
	// An unmeasured node must report "not measured", never a zero duration: 0ms and
	// "nobody looked" are different claims, and the second must not read as the
	// first (the same distinction as the three-state verdict).
	rec, _ := runFor(t, &fakeProvider{cost: FromFloat(1)}) // runFor wires no Clock
	for _, o := range rec.Outcomes {
		if _, ok := o.Timing.Duration(); ok {
			t.Errorf("%s: no clock was injected, so no duration may be reported", o.NodeID)
		}
	}
}

func TestDurationRejectsIncoherentBrackets(t *testing.T) {
	// A half-stamped or reversed bracket is not a measurement. Returning ok=false
	// beats returning a negative or wildly wrong duration that a consumer would
	// chart.
	cases := []struct {
		name string
		tm   NodeTiming
	}{
		{"unset", NodeTiming{}},
		{"start only", NodeTiming{StartedAt: now}},
		{"end only", NodeTiming{EndedAt: now}},
		{"reversed", NodeTiming{StartedAt: now.Add(time.Second), EndedAt: now}},
	}
	for _, c := range cases {
		if _, ok := c.tm.Duration(); ok {
			t.Errorf("%s: must not report a duration", c.name)
		}
	}
	good := NodeTiming{StartedAt: now, EndedAt: now.Add(250 * time.Millisecond)}
	d, ok := good.Duration()
	if !ok || d != 250*time.Millisecond {
		t.Errorf("a coherent bracket must measure: got %v ok=%v", d, ok)
	}
}

func TestParentTimingContainsItsSubtree(t *testing.T) {
	// Span nesting implies containment, so a parent's bracket must enclose its
	// children's. This is what makes the OTel tree readable as a flame graph rather
	// than a set of unrelated intervals.
	rec, _ := timedRun(t, &fakeProvider{cost: FromFloat(1)}, stepClock(now, time.Millisecond))

	byID := map[string]NodeOutcome{}
	for _, o := range rec.Outcomes {
		byID[o.NodeID] = o
	}
	root, ok := byID["n0"]
	if !ok {
		t.Fatal("no root outcome")
	}
	if _, ok := root.Timing.Duration(); !ok {
		t.Fatal("the root must have been timed")
	}
	for _, kid := range root.Children {
		k := byID[kid]
		if k.Timing.StartedAt.Before(root.Timing.StartedAt) {
			t.Errorf("%s started before its parent", kid)
		}
		if k.Timing.EndedAt.After(root.Timing.EndedAt) {
			t.Errorf("%s ended after its parent", kid)
		}
	}
}

// --------------------------------------------------- token split (§8.2, P1)

func TestOutcomeCarriesTheTokenSplit(t *testing.T) {
	// The point of moving tokens onto the outcome: surface-to-volume is computable
	// from a NodeOutcome alone, so a TelemetrySink can report the one number that
	// makes P1 observable. Previously it required holding the Sample.
	rec, _ := runFor(t, &fakeProvider{cost: FromFloat(1)})

	var leaves int
	for _, o := range rec.Outcomes {
		if len(o.Children) > 0 || o.Gap {
			continue
		}
		leaves++
		if o.HaloTokens != 40 || o.GeneratedTokens != 10 {
			t.Errorf("%s: want 40/10 tokens, got %d/%d", o.NodeID, o.HaloTokens, o.GeneratedTokens)
		}
		ratio, ok := o.SurfaceToVolume()
		if !ok || ratio != 4.0 {
			t.Errorf("%s: want ratio 4.0, got %v ok=%v", o.NodeID, ratio, ok)
		}
	}
	if leaves == 0 {
		t.Fatal("test is vacuous: no leaves in the tree")
	}
}

func TestSurfaceToVolumeIsAbsentNotZeroWhenNothingGenerated(t *testing.T) {
	// An internal node or a gap generated nothing. Reporting 0.0 would claim a
	// measured ratio of zero — "this node produced nothing from its context" —
	// which is a real and alarming finding, not the absence of data.
	if _, ok := (NodeOutcome{HaloTokens: 100}).SurfaceToVolume(); ok {
		t.Error("no generated tokens means no ratio, not a zero ratio")
	}
	r, ok := NodeOutcome{HaloTokens: 30, GeneratedTokens: 15}.SurfaceToVolume()
	if !ok || r != 2.0 {
		t.Errorf("want 2.0, got %v ok=%v", r, ok)
	}
}

func TestRetriesAccumulateTokensLikeCost(t *testing.T) {
	// A retried node really re-sent its context, so its halo must reflect every
	// attempt. Overwriting with the last attempt's counts would under-report the
	// exact quantity P1 judges a decomposition on — and it would do so silently,
	// making a wasteful node look efficient.
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	// A verifier that never passes forces MaxRetries attempts.
	never := FuncVerifier{Label: "never", Check: func(Problem, Sample) bool { return false }}
	e := &Executor{
		Planner:    DeclinePlanner{},
		Solver:     ProviderSolver{Provider: &fakeProvider{cost: FromFloat(1)}, Model: "fake"},
		Reducer:    ConcatReducer{},
		Now:        now,
		MaxDepth:   1,
		Verifier:   never,
		MaxRetries: 2,
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	o := res.Outcomes[0]
	if o.Retries != 2 {
		t.Fatalf("expected 2 retries, got %d", o.Retries)
	}
	// Three attempts total (initial + 2 retries) at 40/10 each.
	if o.HaloTokens != 120 || o.GeneratedTokens != 30 {
		t.Errorf("tokens must sum over attempts: want 120/30, got %d/%d", o.HaloTokens, o.GeneratedTokens)
	}
	// The ratio is unchanged by retrying here (every attempt has the same split),
	// which is correct: retrying does not make a node more or less surface-heavy.
	if r, _ := o.SurfaceToVolume(); r != 4.0 {
		t.Errorf("ratio should stay 4.0 across identical attempts, got %v", r)
	}
}

func TestCacheHitCarriesTokensButNoCost(t *testing.T) {
	// A hit is "real tokens, no charge": the tokens were genuinely spent once, and
	// the entry records what they were, but THIS run paid nothing. Reporting zero
	// tokens would make a cache-heavy run look like it did no work; reporting a cost
	// would double-charge for one call.
	caps := Caps{Spend: FromFloat(1000), Latency: time.Hour}
	cache := NewMemCache(0)
	cache.Append(problem("root"), Sample{
		Content: "warm", Cost: FromFloat(5), CreatedAt: now,
		HaloTokens: 80, GeneratedTokens: 20,
	}, nil, now)

	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{
		Planner: DeclinePlanner{},
		Solver:  ProviderSolver{Provider: &fakeProvider{cost: FromFloat(1)}, Model: "fake"},
		Reducer: ConcatReducer{},
		Now:     now,
		Cache:   cache,
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	o := res.Outcomes[0]
	if !o.CacheHit {
		t.Fatal("expected a cache hit")
	}
	if o.HaloTokens != 80 || o.GeneratedTokens != 20 {
		t.Errorf("a hit must carry the stored split: got %d/%d", o.HaloTokens, o.GeneratedTokens)
	}
	if o.Cost != 0 {
		t.Errorf("a hit costs this run nothing: got %s", o.Cost)
	}
}

func TestInternalNodeReportsItsOwnTokensNotTheSubtreeSum(t *testing.T) {
	// An internal node's tokens are the REDUCE call's own. Rolling up children would
	// make every ancestor double-count, so a tree-wide sum would be wrong by a
	// factor that grows with depth — and its surface-to-volume would stop measuring
	// what the merge actually paid for.
	rec, _ := runFor(t, &fakeProvider{cost: FromFloat(1)})
	root := rec.Outcomes[0]
	if len(root.Children) == 0 {
		t.Fatal("expected the root to be an internal node")
	}
	// ConcatReducer is model-free and reports no tokens, so the root must report
	// none — NOT the 80/20 its two leaves consumed.
	if root.HaloTokens != 0 || root.GeneratedTokens != 0 {
		t.Errorf("internal node must report only the reduce's own tokens, got %d/%d",
			root.HaloTokens, root.GeneratedTokens)
	}
}
