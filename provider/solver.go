package provider

import (
	"context"
	"fmt"
	"strings"

	quarry "github.com/scttfrdmn/quarry"
)

// The budget-conditioned Solver — P9 at the one place in the system that spends
// money (§2, "How a leaf is told about its budget").
//
// quarry.ProviderSolver receives an Allocation and discards it, sending the bare
// statement. That made P9 hold for the planner, which is cheap, and fail at the leaf,
// which is not: on the first live run each leaf answered a bare sub-question with a
// 1000-1800 character markdown essay, 249-1008 generated tokens against a 30-token
// halo, while five of thirty nodes went unfunded and 68% of the cap went unspent.
//
// TWO HALVES, AND ONLY ONE OF THEM BINDS. The prompt asks for a length; MaxTokens
// enforces one. A model asked for brevity may decline — models routinely do — so the
// prompt alone would be a request dressed as a constraint. The ceiling alone would
// truncate mid-sentence with no explanation. Both, derived from ONE number so they
// cannot drift apart, is the design.
//
// WHY THIS LIVES IN provider/ AND NOT IN THE CORE. Sizing a ceiling means converting
// Units to tokens, which needs a price sheet; the core imports no price sheet and may
// not (Go rule 4). BedrockProvider.price is already the system's only Units<->tokens
// converter, so Ceiling inverts the sheet that is already here.
//
// WHY THE PROMPT IS BUILT IN THE SOLVER AND NOT IN THE PROVIDER. This is the P8
// constraint and it is the whole reason the change is not one line. RecordedProvider
// INDEXES on the recorded Problem and LOOKS UP on the prompt it is handed
// (record.go): those coincide only while the solver passes the bare statement.
// Wrapping inside a Provider would make every leaf replay miss with "replay
// diverged" — a faithful record reported as a divergence. The executor records the
// PROBLEM, so building the prompt above the Provider keeps the recorded key the bare
// statement and leaves quarry.ProviderSolver as exactly what replay needs. See
// replay_budgeted_test.go, which asserts both halves.

// Budgeter is a Provider that can be given a per-call generation ceiling and can
// price one from an allocation.
//
// Two methods rather than a MaxTokens field because the ceiling is a property of a
// CALL, not of an endpoint: sibling leaves funded differently must get different
// ceilings, and BedrockProvider.MaxTokens — one value for the whole run — cannot
// express that. The field stays, and still applies to the planner and reducer calls
// that have no allocation of their own.
type Budgeter interface {
	quarry.Provider

	// CompleteBounded runs a prompt with an explicit output ceiling. maxOut of 0
	// means "the model's own default", matching BedrockProvider.MaxTokens' existing
	// convention — an absent ceiling, not a zero-token one.
	CompleteBounded(ctx context.Context, prompt, model string, scope quarry.Scope, maxOut int32) (quarry.Sample, error)

	// Ceiling prices an output-token ceiling from a spend allowance. Returns 0 when
	// it cannot price one — an unlimited allowance or a model absent from the sheet —
	// because a fabricated ceiling is worse than none: it would cap generation on a
	// number nothing supports.
	Ceiling(model string, spend quarry.Units) int32
}

// BudgetedSolver solves a leaf with its allocation reaching the model, in both halves:
// a word budget in the prompt and a token ceiling on the call.
type BudgetedSolver struct {
	Provider Budgeter
	Model    string

	// Headroom divides the allocation before it is priced into a ceiling. Zero means
	// DefaultHeadroom.
	//
	// It exists because the Solver seam cannot see Executor.MaxRetries (§5): a leaf
	// that spent its entire balance on the first attempt leaves nothing for a
	// re-solve, so the first answer would also be the last one regardless of the
	// verdict. Spending a fraction is not timidity — it is what keeps the retry
	// budget the executor believes it has from being fictional.
	Headroom int
}

// DefaultHeadroom leaves the leaf spending about a third of its balance per attempt,
// so one solve plus a re-solve plus the verifier's own call all fit. Coarse and
// deliberately so: it sizes an advisory ceiling, not a cap (P4). The CAP is the
// ledger's, and it is unaffected by this number.
const DefaultHeadroom = 3

// Leaf ceiling clamps, in output tokens.
//
// The LOWER bound is the one that matters, and it is not a rounding convenience. A
// node allocated almost nothing prices out at a handful of tokens, and a handful of
// tokens buys a fragment that costs real money and asserts nothing — the worst
// available outcome, because it enters the record as an ANSWER. Deciding a node is
// too poor to be worth solving is the FLOOR's job (quarry.Floor, §3), which refuses
// the node outright and records BaseBelowFloor. A ceiling that silently degraded the
// same nodes to fragments would be a second, invisible floor with none of the first's
// bookkeeping.
//
// The UPPER bound stops a large cap from being read as a licence to write at length:
// the first live run's problem was not that leaves were poor, it was that nothing
// bounded them.
const (
	MinLeafOutputTokens int32 = 128
	MaxLeafOutputTokens int32 = 700
)

// wordsPerToken converts a token ceiling to the word budget the prompt states.
// English averages ~0.75 words per token; asking for slightly fewer words than the
// ceiling can hold means a model that obeys the request is NOT truncated by the cap,
// which is the ordering that makes the truncation signal meaningful — a stop at the
// ceiling then indicates the model ignored the request, not that the two disagreed.
const wordsPerToken = 0.6

// Solve issues one bounded Complete call with the allocation in the prompt.
//
// The ceiling and the word budget come from the SAME number, and that is the point of
// deriving one from the other rather than configuring both: two independently
// configured limits drift, and the failure when they do is silent — a prompt asking
// for 400 words under a 200-token cap produces confident answers that stop
// mid-sentence.
func (bs BudgetedSolver) Solve(ctx context.Context, p quarry.Problem, alloc quarry.Allocation) (quarry.Sample, error) {
	if bs.Provider == nil {
		return quarry.Sample{}, fmt.Errorf("solver has no provider")
	}
	maxOut := bs.Provider.Ceiling(bs.Model, bs.headroom(alloc.Spend))
	prompt := leafPrompt(p, maxOut, !alloc.Deadline.IsZero())
	return bs.Provider.CompleteBounded(ctx, prompt, bs.Model, p.Scope, maxOut)
}

// headroom divides a limited allowance; an unlimited one stays unlimited rather than
// becoming a large number. Absence is not a big value any more than it is zero.
func (bs BudgetedSolver) headroom(spend quarry.Units) quarry.Units {
	if !spend.Limited() {
		return quarry.Unlimited
	}
	h := bs.Headroom
	if h <= 0 {
		h = DefaultHeadroom
	}
	return spend / quarry.Units(h)
}

// leafPrompt states the length budget, the shape rules, and then the problem.
//
// NO CURRENCY APPEARS HERE, and that is a design constraint rather than a style
// preference (§2). Relative estimation beats absolute for the same reason story points
// beat hour estimates, and a model told "you have $0.002" is being asked to price
// tokens it cannot see — it would answer that question badly and its brevity would
// track its guess about pricing rather than the budget. A word count is a constraint
// the SYSTEM chose from a number it actually has.
//
// maxOut of 0 means no ceiling could be priced (unlimited allowance, or an unpriced
// model). The budget line then says so instead of naming a number, exactly as
// budgetLine does for an unlimited planner allocation: a model that cannot tell "no
// stated limit" from "a limit I was not told" will hedge against a cap that is not
// there.
//
// THE STATEMENT GOES LAST, and this is load-bearing beyond readability. FakeProvider's
// answer echoes the tail after the final newline for any prompt over 120 characters
// (fake.go), so a preamble placed after the statement would make every --fake answer
// echo a fragment of quarry's own instructions instead of the question — and --fake is
// how the system is demonstrated.
//
// NO DEPTH SENTENCE, unlike buildPlanPrompt's. quarry.Solver.Solve takes no depth
// parameter, and widening a core seam to carry one sentence of prose is not a trade
// worth making. Named as a deliberate omission so it is not re-added as an oversight.
func leafPrompt(p quarry.Problem, maxOut int32, deadline bool) string {
	var b strings.Builder
	b.WriteString("Answer the PROBLEM below directly. It is one sub-problem of a larger ")
	b.WriteString("question, so answer only what is asked and do not address the wider question.\n\n")

	b.WriteString("BUDGET: ")
	if maxOut > 0 {
		fmt.Fprintf(&b, "answer in at most %d words. This is a hard ceiling, not a target — ",
			wordBudget(maxOut))
		b.WriteString("a longer answer is cut off mid-sentence, not rewarded.")
	} else {
		// Symmetric with budgetLine's unlimited case: stated, never omitted.
		b.WriteString("no stated length limit — answer as briefly as the problem allows.")
	}
	if deadline {
		b.WriteString(" A deadline applies: a shorter answer now is worth more than a fuller one late.")
	}
	b.WriteString("\n\n")

	// The shape rules do double duty. They are what makes the word budget achievable —
	// a preamble and a restatement can consume a third of a short answer — and they
	// attack the extractor's structural-claim problem at its source: markdown headings
	// and table rows are what MechanicalExtractor counts as claims (see the tracking
	// issue on claim splitting), and the cheapest fix for junk claims is not producing
	// the junk.
	b.WriteString("Rules:\n")
	b.WriteString("- No preamble, and do not restate the question.\n")
	b.WriteString("- State the conclusion first, then only what is needed to support it.\n")
	b.WriteString("- No headings, bullets or tables unless the answer is genuinely tabular.\n")
	b.WriteString("- Say plainly what you do not know rather than hedging at length.\n")

	// PROBLEM last — see the doc comment; the fake's tail echo depends on it.
	b.WriteString("\nPROBLEM:\n")
	b.WriteString(p.Statement)
	return b.String()
}

// wordBudget converts an output-token ceiling into the word count the prompt states.
// Floors at 1 so the sentence never reads "at most 0 words", which a model would
// answer either by refusing or by ignoring the line entirely.
func wordBudget(maxOut int32) int {
	w := int(float64(maxOut) * wordsPerToken)
	if w < 1 {
		w = 1
	}
	return w
}

var _ quarry.Solver = BudgetedSolver{}
