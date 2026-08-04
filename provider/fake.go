package provider

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// The fake Provider and Planner — a whole run with no network, no credentials and
// no money, so quarry can be demonstrated and exercised by anyone who can build it.
//
// WHY THIS IS NOT A TEST DOUBLE. Every fake in this repo lived in a _test.go file,
// which means it was unreachable from a binary: `go build` cannot see it. So the
// only way to run quarry end to end was to have AWS credentials and spend real
// money, and the one thing a new reader most needs — watch it work once — was also
// the most expensive thing available. That is a gap in the deliverable, not a
// missing test fixture.
//
// DETERMINISTIC ON PURPOSE, and this is the property that makes it more than a demo
// prop. Content, cost and token counts are all pure functions of the prompt, so a
// fake run is byte-for-byte replayable through the same RecordedProvider path a live
// run uses (P8). A random fake would have looked more lifelike and quietly made
// `quarry replay --fake` untestable.
//
// WHAT IT DOES NOT DO. It does not reason. FakePlanner splits on a mechanical
// heuristic and FakeProvider answers by restating the question, so a fake run
// demonstrates SHAPE, COST and PROVENANCE — the things quarry is actually about —
// and demonstrates nothing whatsoever about answer quality. Anything read out of
// fake content is read out of a hash.

// FakeProvider implements quarry.Provider without a network.
//
// Cost scales with prompt length so a budget genuinely burns down and admission
// control (§3) makes real decisions on a fake run. That matters more than it
// sounds: a zero-cost fake exercises the executor's happy path only, and the
// interesting behaviour — apportionment, the floor, planned degradation under P9 —
// is invisible until money moves.
type FakeProvider struct {
	// PerKTokenIn and PerKTokenOut price the synthetic token counts, in whatever
	// currency Units denominate. Zero values fall back to the defaults, because a
	// fake that costs nothing hides exactly the machinery worth showing.
	PerKTokenIn  float64
	PerKTokenOut float64

	// Latency, when set, sleeps before answering — the difference between a live
	// tree and a still frame. A demo where every node completes in a microsecond
	// renders the finished tree and nothing else, so the progression §9 describes
	// ("planning / spending / verified") is never actually seen.
	//
	// Honours ctx: a deadline must cut a fake run short exactly as it cuts a real
	// one, or the partial-tolerance path (§3.1) cannot be demonstrated either.
	//
	// TREATED AS A MEAN, not a constant, and this turned out to matter. A fixed
	// latency makes every sibling finish at the same instant, so a deadline lands
	// either before all of them or after all of them — and a PARTIAL run, which is
	// the entire subject of §3.1 and the thing the reducer's partial path exists to
	// handle, could not be produced on a fake run at all. The spread is
	// deterministic per prompt (LatencySpread), so replay is unaffected.
	Latency time.Duration

	// LatencySpread is the fraction of Latency the per-call time varies by, in
	// [0, 1). Zero means DefaultFakeLatencySpread; set it negative for a genuinely
	// constant latency, which is what a test comparing wall-clock wants.
	LatencySpread float64

	// Now stamps CreatedAt. A provider at the edge may read the clock (the CORE may
	// not — Go rule 4); nil leaves CreatedAt zero, which reads as unstamped rather
	// than as the epoch.
	Now func() time.Time
}

// Default fake prices. Roughly the order of magnitude of a small hosted model, so
// the numbers on screen are not absurd — but they are invented, and a fake receipt
// is not a quote.
const (
	DefaultFakeInPerKTok  = 0.0002
	DefaultFakeOutPerKTok = 0.0008

	// DefaultFakeLatencySpread is ±40% of the mean. Wide enough that a deadline set
	// near the mean cuts some siblings and not others — the partial run §3.1 is about.
	DefaultFakeLatencySpread = 0.4
)

// Complete answers deterministically from the prompt alone.
func (f *FakeProvider) Complete(ctx context.Context, prompt, model string, scope quarry.Scope) (quarry.Sample, error) {
	return f.CompleteBounded(ctx, prompt, model, scope, 0)
}

// CompleteBounded is Complete with an output ceiling, so the budget-conditioned leaf
// path (BudgetedSolver) is reachable under --fake rather than live-only.
//
// That matters here more than it would elsewhere. The rule is that anything reachable
// with --fake stays reachable with --fake, and a Budgeter the fake did not implement
// would have made the CLI's fake branch fall back to the unbudgeted solver — so the
// path would be exercised only by a run that costs money, which is how three replay
// defects previously survived.
//
// The ceiling clamps the SYNTHETIC token count, so a bounded fake call is genuinely
// cheaper than an unbounded one and admission control sees the difference. The fake's
// content length is unaffected: it is a hash-derived sentence, not generated text, and
// truncating it to match would corrupt the one thing the fake is careful about — that
// its answer is one extractable claim (see fakeAnswer).
func (f *FakeProvider) CompleteBounded(ctx context.Context, prompt, model string, _ quarry.Scope, maxOut int32) (quarry.Sample, error) {
	if err := f.wait(ctx, prompt); err != nil {
		return quarry.Sample{}, err
	}
	in, out := fakeTokens(prompt)
	if maxOut > 0 && out > int(maxOut) {
		out = int(maxOut)
	}
	now := time.Time{}
	if f.Now != nil {
		now = f.Now()
	}
	return quarry.Sample{
		Content: fakeAnswer(prompt),
		Cost:    f.price(in, out),
		Model:   model,
		// The fake IS its own version, and it is explicit (P8). Naming it "fake" rather
		// than borrowing a real model ID keeps a fake record from ever being mistaken
		// for evidence about a real model.
		ModelVersion:    model + "@fake",
		CreatedAt:       now,
		HaloTokens:      in,
		GeneratedTokens: out,
	}, nil
}

// Estimate prices the prompt it is actually given, which the Bedrock provider
// cannot do (it has no tokenizer, so it prices a prior). A fake with a perfect
// estimate is the useful case to have available: it makes an admission-control
// question answerable without the estimator's error confounding it, and estimation
// is advisory regardless (P4).
func (f *FakeProvider) Estimate(prompt, _ string) quarry.Units {
	in, out := fakeTokens(prompt)
	return f.price(in, out)
}

// Ceiling prices a ceiling off the fake's own output rate, so --fake exercises the
// same code path a live run takes rather than a stub of it.
//
// AND IT IS NOT A SMALL LIVE CEILING, in the specific way that keeps biting. The
// fake's synthetic output is 30-130 tokens (fakeTokens) while MinLeafOutputTokens is
// 128, so on a fake run the ceiling is essentially never the binding constraint: the
// clamp path runs, the truncation path almost never does. A change to how truncation
// behaves needs a live run, or a test that constructs the ceiling directly — reading a
// green --fake run as evidence that bounding works is the error this comment exists to
// prevent.
func (f *FakeProvider) Ceiling(_ string, spend quarry.Units) int32 {
	if !spend.Limited() {
		return 0
	}
	pout := f.PerKTokenOut
	if pout == 0 {
		pout = DefaultFakeOutPerKTok
	}
	if pout <= 0 {
		return 0
	}
	// price() charges pout per 1000 tokens against micro-units, so inverting it is
	// (spend/1e6)/pout*1000 — the same arithmetic run backwards, not a second model.
	tokens := float64(spend) / 1e6 / pout * 1000
	return clampCeiling(tokens)
}

func (f *FakeProvider) wait(ctx context.Context, prompt string) error {
	d := f.latencyFor(prompt)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		// A cancelled fake call reports the cancellation rather than answering anyway.
		// The executor turns this into a gap (§3.1), which is the behaviour a demo of
		// deadline truncation needs.
		return ctx.Err()
	}
}

// latencyFor spreads Latency deterministically around its mean, so siblings do not
// all finish at the same instant. Derived from the prompt's hash, NOT from a random
// source: a fake run must replay byte-for-byte (P8), and while timing is deliberately
// unhashed, a random sleep would also make any test that asserts on WHICH nodes
// completed flaky rather than merely slow.
//
// Bounded below at a tenth of the mean. A spread that could reach zero would let a
// node complete instantly and hide the very interleaving this exists to produce.
func (f *FakeProvider) latencyFor(prompt string) time.Duration {
	if f.Latency <= 0 {
		return 0
	}
	spread := f.LatencySpread
	if spread == 0 {
		spread = DefaultFakeLatencySpread
	}
	if spread < 0 {
		return f.Latency // explicitly constant
	}
	if spread > 0.9 {
		spread = 0.9
	}
	// digest → [-1, 1), scaled by spread. A separate salt from fakeTokens', so a
	// prompt's answer length and its latency are not correlated.
	frac := float64(digest("latency\x00"+prompt)%2001)/1000.0 - 1.0
	d := time.Duration(float64(f.Latency) * (1 + spread*frac))
	if min := f.Latency / 10; d < min {
		d = min
	}
	return d
}

func (f *FakeProvider) price(in, out int) quarry.Units {
	pin, pout := f.PerKTokenIn, f.PerKTokenOut
	if pin == 0 {
		pin = DefaultFakeInPerKTok
	}
	if pout == 0 {
		pout = DefaultFakeOutPerKTok
	}
	return quarry.FromFloat(float64(in)/1000*pin + float64(out)/1000*pout)
}

// fakeTokens is a crude token count: ~4 characters per token, the usual rule of
// thumb. The output count is derived from the prompt's hash rather than fixed, so
// surface-to-volume (P1, §8.2) varies across nodes and the ratio on screen is not
// the same number repeated.
func fakeTokens(prompt string) (in, out int) {
	in = len(prompt)/4 + 1
	// 30-130 output tokens, deterministic per prompt.
	out = 30 + int(digest(prompt)%101)
	return in, out
}

// fakeAnswer restates the question and appends a deterministic filler sentence.
//
// Deliberately obvious as a fake. An answer that READ like a real answer would be
// the worse failure mode here: a screenshot or a pasted record would circulate as
// though a model had said it.
func fakeAnswer(prompt string) string {
	stmt := prompt
	// Planner and reducer prompts are long and structured; a leaf prompt is the bare
	// statement. Show only the tail of anything long, so the line stays readable.
	if i := strings.LastIndex(stmt, "\n"); i >= 0 && len(stmt) > 120 {
		stmt = strings.TrimSpace(stmt[i+1:])
	}
	if len(stmt) > 80 {
		stmt = stmt[:80] + "…"
	}
	// The echoed statement is the OTHER source of sentence boundaries, and the one that
	// actually bit: cleanSplit re-appends "?" to each sub-question, so a leaf's statement
	// ends in one, and echoing it mid-line made MechanicalExtractor split the answer —
	// "…dominant term in the total" plus a dangling "— two sources agree…" as a second
	// CLAIM. Removing the punctuation from the fillers fixed only half of it, because the
	// half that showed up on screen came from the user's own question.
	stmt = deSentence(stmt)
	h := digest(prompt)
	// NO SENTENCE-BOUNDARY PUNCTUATION inside the line. MechanicalExtractor splits on
	// it, and an earlier version's "…was called; h=1f3c" produced "h=1f3c)" as a second
	// extracted CLAIM — so the demo's first visible output was quarry asserting a hash
	// fragment. The fake exists to show the machinery working; content that makes a
	// downstream stage look broken defeats that.
	return fmt.Sprintf("[fake answer] %s — %s (synthetic, no model was called, h=%08x)",
		stmt, fakeFillers[h%uint64(len(fakeFillers))], h&0xffffffff)
}

// deSentence removes the characters MechanicalExtractor treats as claim boundaries,
// so an echoed statement stays one claim.
//
// It replaces rather than strips, because dropping them would run two clauses
// together into a different sentence. A middle dot is visibly a substitution.
func deSentence(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '!', '?', ';':
			return '·'
		case '\n':
			return ' '
		}
		return r
	}, s)
}

// fakeFillers vary the prose so a rendered tree does not look like one string
// copied N times. They assert nothing.
//
// None contains sentence-terminating punctuation, for the reason fakeAnswer states:
// the extractor splits on it and a filler with a semicolon turns one answer into two
// claims, one of them a fragment.
var fakeFillers = []string{
	"the evidence points one way but thinly",
	"two sources agree and a third does not",
	"the mechanism is plausible and unmeasured",
	"this reduces to a question already asked upstream",
	"the answer depends on a definition nobody fixed",
	"a bound exists where a point estimate does not",
}

// digest is a stable 64-bit hash. sha256-derived rather than maphash or FNV
// because it must be identical across processes and Go versions — a fake record
// that replays only within one binary is not replayable (P8).
func digest(s string) uint64 {
	sum := sha256.Sum256([]byte(s))
	return binary.BigEndian.Uint64(sum[:8])
}

// FakePlanner decomposes without a model, by splitting the statement on its
// conjunctions and clause boundaries.
//
// It exists because StaticPlanner (the in-core double) returns THE SAME plan for
// every node regardless of the problem, which is fine for testing apportionment and
// useless for a demo: every child restates its siblings and the tree carries no
// information. This one produces a different, plausible split per statement while
// staying deterministic.
//
// It declines (P1) rather than always splitting — on a short statement, past a
// depth, or when the balance will not fund the children. A demo planner that always
// split would show the one behaviour P1 exists to forbid.
type FakePlanner struct {
	// MaxItems bounds the fanout. Zero means DefaultFakeMaxItems.
	MaxItems int

	// MaxSplitDepth is the depth past which it always declines. Zero means
	// DefaultFakeSplitDepth. This is NOT the recursion bound — Executor.MaxDepth is
	// the backstop (P2) and Verifier availability is the real terminator — it is the
	// fake's own judgement, so that a fake run terminates because a planner chose to
	// rather than because it hit the wall.
	MaxSplitDepth int

	// MinFundedItems is the number of children the balance must be able to fund
	// before this planner will split at all (P9: planning is budget-conditioned).
	// Zero means 2 — splitting into one child is not a decomposition.
	MinFundedItems int

	// PerItemCost is what the planner assumes one child will cost, used only for the
	// funding check above. Advisory, like every estimate (P4); a wrong value yields a
	// coarser or finer split, never a broken one.
	PerItemCost quarry.Units
}

// Defaults for the fake planner's shape. Both are small on purpose: --fake exists to
// exercise the machinery, and a wide deep fake tree costs test time without testing
// anything the shallow one does not.
const (
	DefaultFakeMaxItems   = 5
	DefaultFakeSplitDepth = 2
)

// Plan splits the statement mechanically. prior is accepted and unused, for the
// same reason BedrockPlanner ignores it: §8.1's anchoring question is open, and
// showing a planner the previous decomposition biases it toward that decomposition
// (see the TODO on quarry.Planner).
func (fp FakePlanner) Plan(_ context.Context, p quarry.Problem, alloc quarry.Allocation, depth int, _ []quarry.NodeOutcome) (quarry.Plan, error) {
	if depth >= fp.maxSplitDepth() {
		return quarry.Plan{Declined: true,
			Reasoning: fmt.Sprintf("at depth %d this is narrow enough to answer directly", depth)}, nil
	}

	parts := splitStatement(p.Statement, fp.maxItems())
	if len(parts) < 2 {
		return quarry.Plan{Declined: true,
			Reasoning: "the statement has no separable sub-questions; surface-to-volume does not favour a split (P1)"}, nil
	}

	// P9: the budget is an INPUT to the decision, not a constraint discovered later.
	// Drop items the balance cannot fund and disclose them as Excluded, so degradation
	// is visible at the plan gate rather than at the end.
	var excluded []string
	if alloc.Spend.Limited() && fp.perItem() > 0 {
		affordable := int(alloc.Spend / fp.perItem())
		if affordable < fp.minFunded() {
			return quarry.Plan{Declined: true,
				Reasoning: fmt.Sprintf("balance %s funds only %d sub-answers; a split needs %d",
					alloc.Spend, affordable, fp.minFunded())}, nil
		}
		if affordable < len(parts) {
			excluded = append(excluded, parts[affordable:]...)
			parts = parts[:affordable]
		}
	}

	items := make([]quarry.PlanItem, 0, len(parts))
	for i, part := range parts {
		items = append(items, quarry.PlanItem{
			// Scope is INHERITED, never synthesized. P6: scope never widens on descent, and
			// the cheapest way to guarantee that is to give the planner no way to set it.
			Problem: quarry.Problem{Statement: part, Scope: p.Scope},
			// Weights are RELATIVE (§2). Longer clauses are weighted heavier — a weak
			// proxy, stated as one: it makes apportionment non-uniform so the ledger's
			// largest-remainder arithmetic is actually exercised, rather than dividing
			// evenly every time and hiding the remainder case.
			Weight:     int64(len(part)/40 + 1),
			ExpectLeaf: depth+1 >= fp.maxSplitDepth(),
			Rationale:  fmt.Sprintf("clause %d of %d, answerable independently", i+1, len(parts)),
		})
	}
	return quarry.Plan{
		Items:     items,
		Excluded:  excluded,
		Reasoning: fmt.Sprintf("split into %d independent sub-questions", len(items)),
	}, nil
}

func (fp FakePlanner) maxItems() int {
	if fp.MaxItems > 0 {
		return fp.MaxItems
	}
	return DefaultFakeMaxItems
}

func (fp FakePlanner) maxSplitDepth() int {
	if fp.MaxSplitDepth > 0 {
		return fp.MaxSplitDepth
	}
	return DefaultFakeSplitDepth
}

func (fp FakePlanner) minFunded() int {
	if fp.MinFundedItems > 0 {
		return fp.MinFundedItems
	}
	return 2
}

func (fp FakePlanner) perItem() quarry.Units {
	if fp.PerItemCost > 0 {
		return fp.PerItemCost
	}
	return quarry.FromFloat(0.0001)
}

// splitStatement breaks a statement into sub-questions on the boundaries a
// research question usually offers: explicit list separators first, then
// conjunctions. Returns fewer than two parts when it finds nothing to split on,
// which is the signal to decline.
func splitStatement(stmt string, max int) []string {
	stmt = strings.TrimSpace(stmt)
	if len(stmt) < 40 {
		return nil // too short to have separable parts
	}
	// Strongest signal first: an author who wrote a list meant a list.
	for _, sep := range []string{";", "?", ",", " and ", " versus ", " vs ", " then "} {
		if parts := cleanSplit(stmt, sep, max); len(parts) >= 2 {
			return parts
		}
	}
	return nil
}

// cleanSplit splits on sep, drops fragments too short to be a question, and caps
// the count. The remainder past the cap is FOLDED INTO THE LAST ITEM rather than
// dropped — silently discarding part of the question would make the run answer
// something narrower than it was asked while reporting a complete plan.
func cleanSplit(stmt, sep string, max int) []string {
	raw := strings.Split(stmt, sep)
	if len(raw) < 2 {
		return nil
	}
	var parts []string
	for _, r := range raw {
		r = strings.TrimSpace(strings.Trim(r, " ,.;"))
		if len(r) < 12 {
			continue // a fragment, not a sub-question
		}
		if sep == "?" {
			r += "?"
		}
		if len(parts) >= max {
			parts[len(parts)-1] += "; " + r
			continue
		}
		parts = append(parts, r)
	}
	if len(parts) < 2 {
		return nil
	}
	return parts
}

var (
	_ quarry.Provider = (*FakeProvider)(nil)
	_ Budgeter        = (*FakeProvider)(nil)
	_ quarry.Planner  = FakePlanner{}
)
