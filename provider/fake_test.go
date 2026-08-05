package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	quarry "github.com/scttfrdmn/quarry"
)

// The fake's determinism is LOAD-BEARING, not a convenience: `quarry run --fake`
// followed by `quarry replay` must produce identical bytes (P8), and that only holds
// if content, cost and token counts are pure functions of the prompt. These tests
// pin that, plus the P1/P6/P9 properties the fake planner must honour to be an
// honest demonstration rather than a prop.

func TestFakeProviderIsDeterministic(t *testing.T) {
	// Two separate provider instances, so the determinism cannot come from cached
	// state inside one of them.
	a, b := &FakeProvider{}, &FakeProvider{}
	ctx := context.Background()
	for _, prompt := range []string{"what is the cost of X", "", strings.Repeat("long ", 200)} {
		s1, err := a.Complete(ctx, prompt, "fake", quarry.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		s2, err := b.Complete(ctx, prompt, "fake", quarry.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		if s1.Content != s2.Content {
			t.Errorf("content differs for %q:\n a=%q\n b=%q", prompt, s1.Content, s2.Content)
		}
		if s1.Cost != s2.Cost {
			t.Errorf("cost differs for %q: %s vs %s", prompt, s1.Cost, s2.Cost)
		}
		if s1.HaloTokens != s2.HaloTokens || s1.GeneratedTokens != s2.GeneratedTokens {
			t.Errorf("token counts differ for %q", prompt)
		}
	}
}

func TestFakeProviderCostsSomething(t *testing.T) {
	// A zero-cost fake would exercise only the executor's happy path: apportionment,
	// the floor and planned degradation are all invisible until money moves.
	f := &FakeProvider{}
	s, err := f.Complete(context.Background(), "a question of some length to price", "fake", quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Cost.Limited() || s.Cost <= 0 {
		t.Fatalf("a fake call must still cost something, got %s", s.Cost)
	}
	if s.HaloTokens == 0 || s.GeneratedTokens == 0 {
		t.Errorf("both token halves must be reported: halo=%d gen=%d", s.HaloTokens, s.GeneratedTokens)
	}
	if _, ok := s.SurfaceToVolume(); !ok {
		t.Error("surface-to-volume must be computable from a fake sample (P1)")
	}
	// Cost must scale with the prompt, or a budget cannot burn down differentially and
	// every node looks equally expensive.
	long, err := f.Complete(context.Background(), strings.Repeat("x", 4000), "fake", quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if long.Cost <= s.Cost {
		t.Errorf("a longer prompt must cost more: %s vs %s", long.Cost, s.Cost)
	}
}

func TestFakeModelVersionIsNotARealModelID(t *testing.T) {
	// P8 wants an explicit version; this additionally wants a fake record to be
	// unmistakable, so a pasted receipt cannot be read as evidence about a real model.
	f := &FakeProvider{}
	s, err := f.Complete(context.Background(), "q", "us.anthropic.claude-haiku-4-5", quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(s.ModelVersion, "@fake") {
		t.Errorf("a fake sample's version must be self-identifying, got %q", s.ModelVersion)
	}
	if !strings.Contains(s.Content, "fake") {
		t.Errorf("fake content must announce itself, got %q", s.Content)
	}
}

func TestFakeContentIsOneExtractableClaim(t *testing.T) {
	// Found by running the demo and reading its output. MechanicalExtractor splits on
	// sentence terminators, so an earlier fake answer ending "…was called; h=1f3c"
	// produced "h=1f3c)" as a second CLAIM — the demo's first visible output was quarry
	// asserting a hash fragment. The fake exists to show the machinery working, so
	// content that makes a downstream stage look broken defeats the point.
	// The prompts MUST include terminating punctuation. An earlier version of this test
	// used only bare phrases, so it passed while the demo was visibly broken: the
	// boundary that split the answer came from the ECHOED STATEMENT, and cleanSplit
	// re-appends "?" to every sub-question it produces. A fixture cleaner than the real
	// input tests nothing.
	f := &FakeProvider{}
	ex := quarry.MechanicalExtractor{}
	for _, prompt := range []string{
		"a question about the cost of a thing",
		"and what is the dominant term in the total?", // exactly what cleanSplit emits
		"one thing. then another thing entirely",
		"a clause; and a second clause",
		"why not!",
		"",
	} {
		s, err := f.Complete(context.Background(), prompt, "fake", quarry.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		claims, err := ex.Extract(context.Background(), s, "n0")
		if err != nil {
			t.Fatal(err)
		}
		if len(claims) != 1 {
			t.Errorf("a fake answer must extract as exactly one claim, got %d from %q:",
				len(claims), s.Content)
			for _, c := range claims {
				t.Errorf("    · %q", c.Text)
			}
		}
	}
	// And the same for every filler, a second source of the same defect.
	for i, filler := range fakeFillers {
		if strings.ContainsAny(filler, ".!?;\n") {
			t.Errorf("filler %d contains a sentence boundary and will split into two claims: %q",
				i, filler)
		}
	}

	// The composed path, because both unit fixtures above were once clean enough to pass
	// while the demo was broken. Split a real question, answer each part as the executor
	// would, and extract: one claim per part, no fragments.
	p, err := FakePlanner{}.Plan(context.Background(), quarry.Problem{
		Statement: "What does the deployment cost per month, how does that scale, " +
			"and what is the dominant term in the total?",
	}, quarry.Allocation{Spend: quarry.FromFloat(100)}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) < 2 {
		t.Fatal("fixture must split")
	}
	for _, it := range p.Items {
		s, err := f.Complete(context.Background(), it.Problem.Statement, "fake", quarry.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		claims, err := ex.Extract(context.Background(), s, "n0")
		if err != nil {
			t.Fatal(err)
		}
		if len(claims) != 1 {
			t.Errorf("answering the planner's own sub-question %q yielded %d claims:",
				it.Problem.Statement, len(claims))
			for _, c := range claims {
				t.Errorf("    · %q", c.Text)
			}
		}
	}
}

func TestFakeEstimateMatchesTheActualCost(t *testing.T) {
	// The fake is the one provider that CAN estimate exactly (it has the prompt and
	// its own pricing), which makes admission-control behaviour observable without
	// estimator error confounding it.
	f := &FakeProvider{}
	const prompt = "estimate this"
	s, err := f.Complete(context.Background(), prompt, "fake", quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Estimate(prompt, "fake"); got != s.Cost {
		t.Errorf("fake estimate %s must equal the actual cost %s", got, s.Cost)
	}
}

func TestFakeLatencyHonoursCancellation(t *testing.T) {
	// A deadline must cut a fake run short exactly as it cuts a real one, or the
	// partial-tolerance path (§3.1) cannot be demonstrated on a fake run either.
	f := &FakeProvider{Latency: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Complete(ctx, "q", "fake", quarry.Scope{}); err == nil {
		t.Fatal("a cancelled fake call must report the cancellation, not answer anyway")
	}
}

func TestFakeLatencyVariesSoADeadlineCanLandMidTree(t *testing.T) {
	// A CONSTANT latency makes every sibling finish at the same instant, so a deadline
	// lands before all of them or after all of them. Found by trying to demo a partial
	// run and being unable to produce one at any deadline: the tree went from 4 answers
	// to 4 gaps with nothing in between, and a PARTIAL result is the entire subject of
	// §3.1 and the reason the reducer has a partial path at all.
	f := &FakeProvider{Latency: 100 * time.Millisecond}
	seen := map[time.Duration]bool{}
	for i := 0; i < 8; i++ {
		seen[f.latencyFor(fmt.Sprintf("sub-question number %d", i))] = true
	}
	if len(seen) < 4 {
		t.Errorf("8 distinct prompts must not collapse to %d distinct latencies", len(seen))
	}
	for d := range seen {
		// Bounded: a node that completes instantly hides the interleaving, and one that
		// takes many times the mean makes --deadline unpredictable to set.
		if d < f.Latency/10 || d > 2*f.Latency {
			t.Errorf("latency %s is outside a usable band around the %s mean", d, f.Latency)
		}
	}

	// Deterministic, like everything else here: the same prompt must always take the
	// same time, or a test that asserts WHICH nodes survived a deadline is flaky.
	a := f.latencyFor("the same prompt")
	if b := f.latencyFor("the same prompt"); a != b {
		t.Errorf("latency must be a pure function of the prompt: %s then %s", a, b)
	}
	// And a caller who wants a constant can have one.
	g := &FakeProvider{Latency: 50 * time.Millisecond, LatencySpread: -1}
	if g.latencyFor("x") != g.latencyFor("y") {
		t.Error("a negative spread must give a genuinely constant latency")
	}
}

func TestFakePlannerDeclines(t *testing.T) {
	// P1 forbids a planner that always splits, so the fake must decline in the three
	// cases it claims to: nothing to split on, too deep, and too little money.
	fp := FakePlanner{}
	big := quarry.Allocation{Spend: quarry.FromFloat(100)}

	short, err := fp.Plan(context.Background(), quarry.Problem{Statement: "why?"}, big, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !short.Declined {
		t.Errorf("a statement with no separable parts must be declined, got %d items", len(short.Items))
	}

	deep, err := fp.Plan(context.Background(), splittable(), big, 9, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !deep.Declined {
		t.Error("past MaxSplitDepth the planner must decline")
	}

	// P9: budget-conditioned. A balance that cannot fund a split must produce a
	// decline, not a split that will not fit.
	poor, err := fp.Plan(context.Background(), splittable(),
		quarry.Allocation{Spend: quarry.FromFloat(0.00005)}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !poor.Declined {
		t.Errorf("an unfundable split must be declined (P9), got %d items", len(poor.Items))
	}
	if poor.Reasoning == "" {
		t.Error("a decline must say why — it is disclosed at the plan gate (§9)")
	}
}

func TestFakePlannerSplitsAndDiscloses(t *testing.T) {
	fp := FakePlanner{}
	p, err := fp.Plan(context.Background(), splittable(),
		quarry.Allocation{Spend: quarry.FromFloat(100)}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Declined || len(p.Items) < 2 {
		t.Fatalf("a multi-clause question must split, got declined=%v items=%d", p.Declined, len(p.Items))
	}
	for i, it := range p.Items {
		if it.Weight <= 0 {
			t.Errorf("item %d has weight %d; weights are relative and must be positive", i, it.Weight)
		}
		if strings.TrimSpace(it.Problem.Statement) == "" {
			t.Errorf("item %d has an empty statement", i)
		}
	}
	// Not every child may restate the parent — that is a portfolio, and this planner
	// emits partitions.
	for _, it := range p.Items {
		if it.Problem.Statement == splittable().Statement {
			t.Error("a partition's children must be different sub-problems, not the parent restated")
		}
	}
}

func TestFakePlannerNeverWidensScope(t *testing.T) {
	// P6, enforced structurally: the planner has no way to name a child's scope, so
	// this test is a guard against someone giving it one later.
	parent := quarry.Problem{
		Statement: splittable().Statement,
		Scope:     quarry.Scope{Tags: map[string]string{"lab": "kempner", "tier": "restricted"}},
	}
	p, err := FakePlanner{}.Plan(context.Background(), parent,
		quarry.Allocation{Spend: quarry.FromFloat(100)}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) == 0 {
		t.Fatal("fixture must split")
	}
	for _, it := range p.Items {
		if !parent.Scope.NarrowsTo(it.Problem.Scope) {
			t.Errorf("child scope %v widens its parent's %v (P6)", it.Problem.Scope.Tags, parent.Scope.Tags)
		}
	}
}

func TestFakePlannerExcludesRatherThanDropsSilently(t *testing.T) {
	// P9 again: degradation is DISCLOSED before spend. A plan trimmed to fit must say
	// what it dropped, or the run answers something narrower than it was asked while
	// reporting a complete plan.
	fp := FakePlanner{PerItemCost: quarry.FromFloat(1)}
	p, err := fp.Plan(context.Background(), splittable(),
		quarry.Allocation{Spend: quarry.FromFloat(2)}, 0, nil) // funds exactly 2
	if err != nil {
		t.Fatal(err)
	}
	if p.Declined {
		t.Fatal("a balance funding two children must split, not decline")
	}
	if len(p.Items) != 2 {
		t.Errorf("a balance funding two children must propose two, got %d", len(p.Items))
	}
	if len(p.Excluded) == 0 {
		t.Error("trimmed sub-problems must be disclosed in Excluded (P9), not dropped silently")
	}
}

func TestFakePlannerIsDeterministic(t *testing.T) {
	// Same reason as the provider: replay compares bytes, and a drifting plan changes
	// the SHAPE, which is worse than drifting prose.
	a, err := FakePlanner{}.Plan(context.Background(), splittable(),
		quarry.Allocation{Spend: quarry.FromFloat(100)}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FakePlanner{}.Plan(context.Background(), splittable(),
		quarry.Allocation{Spend: quarry.FromFloat(100)}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Items) != len(b.Items) {
		t.Fatalf("fanout differs across calls: %d vs %d", len(a.Items), len(b.Items))
	}
	for i := range a.Items {
		if a.Items[i].Problem.Statement != b.Items[i].Problem.Statement ||
			a.Items[i].Weight != b.Items[i].Weight {
			t.Errorf("item %d differs across calls", i)
		}
	}
}

func TestCleanSplitFoldsTheRemainderRatherThanDroppingIt(t *testing.T) {
	// The cap on fanout must not silently discard sub-questions. Folding keeps the
	// whole question in the plan; dropping would answer less than was asked.
	stmt := "the first clause is long enough to count, the second clause is also long enough, " +
		"the third clause qualifies too, the fourth clause likewise qualifies, " +
		"the fifth clause also qualifies here, the sixth clause qualifies as well"
	parts := cleanSplit(stmt, ",", 3)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	if !strings.Contains(parts[2], "sixth") {
		t.Errorf("the remainder must be folded into the last part, got %q", parts[2])
	}
}

// splittable is a question the fake planner will split, used wherever a test needs
// a real split rather than a decline.
func splittable() quarry.Problem {
	return quarry.Problem{Statement: "What does the deployment cost per month, " +
		"how does that scale with the number of users, " +
		"and what is the dominant term in the total"}
}

func TestFakeAnswerTruncatesByRuneAndNeverEmitsABrokenOne(t *testing.T) {
	// FOUND BY RUNNING THE BINARY, not by the suite: `quarry run --fake` on a question in
	// French and Chinese printed "究竟是什��…" — an orphaned byte pair, because the echo
	// truncated at 80 BYTES and a CJK rune is three. Go's JSON encoder replaces the orphan
	// with U+FFFD, so the RECORD contained corrupted text where the user's own question had
	// a character, and the record is the deliverable (§8).
	//
	// Invisible to every existing test for the reason CLAUDE.md names: a fixture cleaner
	// than the real input cannot discover what the real input does. Every fake fixture here
	// was ASCII, where bytes and runes coincide.
	long := strings.Repeat("存储成本如何", 30) // 180 runes, 540 bytes — well past the limit
	got := fakeAnswer(long)
	if !utf8.ValidString(got) {
		t.Errorf("a truncated answer must remain valid UTF-8; a host writing this to JSON "+
			"emits U+FFFD where the question had a character: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("no replacement character may appear: it is indistinguishable from the "+
			"model having said one, got %q", got)
	}
	// And the truncation still HAPPENED, or this test would pass on a fake that echoed
	// everything and pin nothing.
	if !strings.Contains(got, "…") {
		t.Errorf("a 180-rune statement must still be truncated, got %q", got)
	}
	// Mixed-width input, which is the shape that actually bit: an ASCII prefix long enough
	// to put the boundary inside a multibyte rune.
	mixed := strings.Repeat("cost ", 15) + strings.Repeat("存储成本如何", 5)
	if m := fakeAnswer(mixed); strings.ContainsRune(m, utf8.RuneError) {
		t.Errorf("a boundary landing mid-rune must not corrupt the echo, got %q", m)
	}
}
