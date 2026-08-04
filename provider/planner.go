package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	quarry "github.com/scttfrdmn/quarry"
)

// The model-backed Planner — the first time quarry's central act is performed by a
// model rather than a fixture (§2). Everything built before this ran on
// StaticPlanner, which returns a plan someone typed; the machinery was real and the
// decomposition was not.
//
// §2 calls the planner THE ERROR CONCENTRATOR: it does the hardest reasoning with
// the least information, deciding the split before knowing what the children will
// find, and every node below inherits its mistakes. That is why P3 says verify it
// hardest, and why this file is more defensive than the solver — a bad leaf answer
// is one bad answer, a bad plan is a bad run.
//
// Three properties are load-bearing, and each is enforced rather than requested:
//
//   - IT MUST BE ABLE TO DECLINE (P1). A planner that always splits is the failure
//     P1 exists to forbid, so the prompt offers declining as a first-class answer
//     with its own field, not as an empty item list.
//   - IT EMITS RELATIVE WEIGHTS, never currency (§2). Relative estimation is far
//     more reliable than absolute, and the Ledger converts weights to money. A model
//     asked for dollars would be answering a question it cannot answer.
//   - A REPLY IT CANNOT PARSE IS AN ERROR, never a guess. A silently-salvaged plan
//     is the worst outcome available here: the run proceeds, spends real money on a
//     decomposition nobody chose, and records it as though the planner meant it.

// ErrUnparseablePlan is returned when the model's reply is not a plan. It wraps
// nothing and is deliberately terminal: a plan is structured output, and the
// alternative to failing is spending a whole budget on a shape read out of noise.
var ErrUnparseablePlan = fmt.Errorf("planner reply is not a parseable plan")

// BedrockPlanner implements quarry.Planner with one Converse call.
//
// It is budget-conditioned per P9: the allocation it is given goes INTO the prompt,
// because §2's whole framing is "decompose this given this balance" — a planner told
// the budget can decline or narrow, and a plan that fits is worth more than a good
// plan that does not. What the planner may NOT do is see the budget and quote a
// price; it returns weights and the Ledger does the arithmetic.
type BedrockPlanner struct {
	Provider *BedrockProvider
	Model    string

	// MaxItems bounds the fanout the planner may propose. Zero means DefaultMaxItems.
	//
	// This is a guard on the model, not on the design: a plan with forty children
	// passes every mechanical check (the weights sum, each share may clear the floor)
	// while being a decomposition nobody would accept, and the floor alone will not
	// catch it under a large cap. Rejecting is right — silently truncating would
	// discard sub-problems the planner deemed necessary and record the remainder as
	// if it were the whole plan.
	MaxItems int
}

// DefaultMaxItems bounds proposed fanout when MaxItems is unset.
const DefaultMaxItems = 12

// NewBedrockPlanner wires a planner to a provider and an explicit model version.
// The version must be explicit, never an alias (P8): the plan shapes the whole run,
// so a record that cannot name what planned it is not replayable.
func NewBedrockPlanner(p *BedrockProvider, model string) *BedrockPlanner {
	return &BedrockPlanner{Provider: p, Model: model, MaxItems: DefaultMaxItems}
}

// planReply is the wire shape asked of the model. Kept flat and small: every field
// the model must fill is a field it can get wrong, and this is the node whose
// mistakes propagate furthest.
type planReply struct {
	Decline   bool   `json:"decline"`
	Reasoning string `json:"reasoning"`
	Strategy  string `json:"strategy"`
	Items     []struct {
		Statement  string `json:"statement"`
		Weight     int64  `json:"weight"`
		ExpectLeaf bool   `json:"expect_leaf"`
		Rationale  string `json:"rationale"`
	} `json:"items"`
	Excluded []string `json:"excluded"`
}

// Plan asks the model for a decomposition and converts it to a quarry.Plan.
//
// The prior argument is accepted and DELIBERATELY UNUSED. §8.1's anchoring problem
// is unresolved (see the TODO on quarry.Planner): showing the planner a previous
// decomposition biases it toward that decomposition, and §7 names an INDEPENDENT
// decomposition as the strongest replication signal available — so passing the prior
// shape would trade the best evidence the system has for a marginally warmer start.
// Withholding it is the conservative choice while §12 is open, and it is a choice,
// not an omission.
func (bp *BedrockPlanner) Plan(ctx context.Context, p quarry.Problem, alloc quarry.Allocation, depth int, prior []quarry.NodeOutcome) (quarry.Plan, error) {
	if bp.Provider == nil {
		return quarry.Plan{}, fmt.Errorf("planner has no provider")
	}
	prompt := buildPlanPrompt(p, alloc, depth, bp.maxItems())
	// Scope is passed through so the call is attributed correctly; the planner sees
	// only the statement, and entitlement is enforced before the call reaches here.
	sample, err := bp.Provider.Complete(ctx, prompt, bp.Model, p.Scope)
	if err != nil {
		return quarry.Plan{}, fmt.Errorf("plan call: %w", err)
	}

	var reply planReply
	if err := json.Unmarshal([]byte(extractJSON(sample.Content)), &reply); err != nil {
		return quarry.Plan{}, fmt.Errorf("%w: %v (reply: %.200q)", ErrUnparseablePlan, err, sample.Content)
	}

	// Declining is a real answer (P1), and it is checked FIRST: a model that both
	// declines and lists items has contradicted itself, and honouring the decline is
	// the safe reading — it degrades to one solve rather than to a split nobody
	// endorsed.
	if reply.Decline {
		return quarry.Plan{Declined: true, Reasoning: reply.Reasoning}, nil
	}
	if len(reply.Items) == 0 {
		// An empty non-declining plan is a contradiction, not a decline. Treating it
		// as one would hide a broken planner behind a legitimate-looking outcome —
		// the record would say "declined to split" when the planner said nothing.
		return quarry.Plan{}, fmt.Errorf("%w: no items and no decline", ErrUnparseablePlan)
	}
	if len(reply.Items) > bp.maxItems() {
		return quarry.Plan{}, fmt.Errorf("%w: %d items exceeds MaxItems %d",
			quarry.ErrPlanDoesNotFit, len(reply.Items), bp.maxItems())
	}

	strategy, err := parseStrategy(reply.Strategy)
	if err != nil {
		return quarry.Plan{}, err
	}

	items := make([]quarry.PlanItem, 0, len(reply.Items))
	for i, it := range reply.Items {
		stmt := strings.TrimSpace(it.Statement)
		if stmt == "" {
			return quarry.Plan{}, fmt.Errorf("%w: item %d has an empty statement", ErrUnparseablePlan, i)
		}
		// A non-positive weight would make Apportion reject the whole plan with a
		// message about weights rather than about the planner. Catching it here names
		// the actual culprit, and a zero weight is ambiguous besides — it could mean
		// "free" or "omit", and guessing which is not this layer's call.
		if it.Weight <= 0 {
			return quarry.Plan{}, fmt.Errorf("%w: item %d has weight %d; weights are relative and must be positive",
				ErrUnparseablePlan, i, it.Weight)
		}
		items = append(items, quarry.PlanItem{
			// Scope is INHERITED from the parent, never taken from the model's reply.
			// P6: scope never widens on descent, and a planner that could name its
			// children's scope could widen it — so the field is simply not exposed.
			Problem:    quarry.Problem{Statement: stmt, Scope: p.Scope},
			Weight:     it.Weight,
			ExpectLeaf: it.ExpectLeaf,
			Rationale:  it.Rationale,
		})
	}

	return quarry.Plan{
		Items:     items,
		Excluded:  reply.Excluded,
		Strategy:  strategy,
		Reasoning: reply.Reasoning,
	}, nil
}

func (bp *BedrockPlanner) maxItems() int {
	if bp.MaxItems > 0 {
		return bp.MaxItems
	}
	return DefaultMaxItems
}

// parseStrategy maps the reply's strategy string onto the enum. An unrecognized
// value is an error rather than a default: silently treating an unknown strategy as
// a partition would run a shape the planner did not ask for, and the two shapes
// disagree about what identical child statements mean (§2).
func parseStrategy(s string) (quarry.Strategy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "partition":
		return quarry.StrategyPartition, nil
	case "portfolio":
		return quarry.StrategyPortfolio, nil
	default:
		return "", fmt.Errorf("%w: unknown strategy %q", ErrUnparseablePlan, s)
	}
}

// buildPlanPrompt states the task, the budget (P9) and the reply format.
//
// The allocation appears in the prompt because P9 makes planning budget-conditioned:
// "decompose this given this balance". It is stated in RELATIVE terms — how many
// sub-answers the balance can fund — rather than as a currency amount, because a
// model handed a dollar figure will try to price the work, which is precisely the
// absolute estimation §2 avoids.
func buildPlanPrompt(p quarry.Problem, alloc quarry.Allocation, depth, maxItems int) string {
	var b strings.Builder
	b.WriteString("You are a research planner. Decompose the PROBLEM below into independent ")
	b.WriteString("sub-problems that can each be answered on their own, then STOP — you are not ")
	b.WriteString("answering the problem itself.\n\n")

	b.WriteString("Rules:\n")
	b.WriteString("- Decompose ONLY if the sub-problems are genuinely easier to answer separately ")
	b.WriteString("and their answers combine into an answer to the whole. If splitting would not help, ")
	b.WriteString("set \"decline\": true. Declining is a correct and expected answer, not a failure.\n")
	b.WriteString("- Weights are RELATIVE effort (a child worth 3 is roughly three times the work of ")
	b.WriteString("a child worth 1). Do NOT estimate money or time.\n")
	b.WriteString("- Sub-problems must be INDEPENDENT: none may need another's answer as input.\n")
	fmt.Fprintf(&b, "- Propose at most %d sub-problems.\n", maxItems)
	b.WriteString("- Set \"expect_leaf\": true for a sub-problem you expect to be answered directly ")
	b.WriteString("rather than split further.\n")
	b.WriteString("- List anything the budget cannot cover in \"excluded\", so it is disclosed up front ")
	b.WriteString("rather than discovered later.\n")
	b.WriteString("- \"strategy\": use \"partition\" to split into DIFFERENT sub-problems (the normal case). ")
	b.WriteString("Use \"portfolio\" ONLY when the problem should not be split but is worth attempting ")
	b.WriteString("several independent ways, so the best attempt can be selected; then every item ")
	b.WriteString("must restate the SAME problem with a different approach in its rationale.\n\n")

	b.WriteString(budgetLine(alloc, depth))
	b.WriteString("\n\nPROBLEM:\n")
	b.WriteString(p.Statement)

	b.WriteString("\n\nReply with ONLY a JSON object, no prose and no code fence:\n")
	b.WriteString(`{"decline":false,"strategy":"partition","reasoning":"...",`)
	b.WriteString(`"items":[{"statement":"...","weight":1,"expect_leaf":true,"rationale":"..."}],`)
	b.WriteString(`"excluded":[]}`)
	return b.String()
}

// budgetLine renders the allocation as guidance without naming a currency amount.
// Unlimited is stated as such rather than omitted: a planner that cannot tell "no
// stated limit" from "a limit I was not told" will hedge against a cap that is not
// there.
func budgetLine(alloc quarry.Allocation, depth int) string {
	var b strings.Builder
	b.WriteString("BUDGET: ")
	if alloc.Spend.Limited() {
		b.WriteString("limited — prefer a coarser split with fewer, larger sub-problems, ")
		b.WriteString("and decline rather than propose a split the budget cannot fund")
	} else {
		b.WriteString("no stated limit — split as the problem warrants")
	}
	if !alloc.Deadline.IsZero() {
		b.WriteString(". A deadline applies, so prefer breadth over depth: ")
		b.WriteString("sub-problems that split further take longer than ones answered directly")
	}
	fmt.Fprintf(&b, ". This problem sits at depth %d", depth)
	if depth > 0 {
		b.WriteString(" (it is already a sub-problem of a larger one, so keep the scope narrow)")
	}
	b.WriteString(".")
	return b.String()
}

// extractJSON pulls the outermost JSON object out of a reply.
//
// Models wrap JSON in code fences and preambles despite instructions, and that is a
// formatting artifact, not a wrong answer — refusing it would fail runs over
// backticks. The rule is narrow on purpose: take the first '{' through the last '}'
// and let json.Unmarshal be the judge. It does NOT repair malformed JSON, because
// the boundary between "unwrapping" and "guessing what the planner meant" is exactly
// where a salvaged plan would start costing money.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return s // let the unmarshal error report the real content
	}
	return s[start : end+1]
}

var _ quarry.Planner = (*BedrockPlanner)(nil)
