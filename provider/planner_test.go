package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	quarry "github.com/scttfrdmn/quarry"
)

// Tests for the model-backed Planner. No network: a fake Converser returns the
// reply, so what is under test is the CONTRACT quarry imposes on a planner's output
// — not the model's judgement, which no test can pin.
//
// The bias of this file is deliberate. Almost every test asserts that a malformed or
// self-contradictory reply is REFUSED. §2 calls the planner the error concentrator,
// and the failure mode that matters is not a bad plan — it is a salvaged plan: the
// run proceeds, spends the whole budget on a shape nobody chose, and the record says
// the planner meant it.

// recordingConverser hands back scripted replies in order and keeps every prompt it
// was given. Recording prompts is what lets a test assert P9 — that the budget
// reached the planner — which is otherwise invisible from the outside.
type recordingConverser struct {
	replies []string
	prompts []string
	err     error
	inTok   int32
	outTok  int32
}

func (r *recordingConverser) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	for _, m := range in.Messages {
		for _, blk := range m.Content {
			if t, ok := blk.(*brtypes.ContentBlockMemberText); ok {
				r.prompts = append(r.prompts, t.Value)
			}
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	reply := ""
	if n := len(r.prompts) - 1; n >= 0 && n < len(r.replies) {
		reply = r.replies[n]
	} else if len(r.replies) > 0 {
		reply = r.replies[len(r.replies)-1]
	}
	return &bedrockruntime.ConverseOutput{
		Output: &brtypes.ConverseOutputMemberMessage{Value: brtypes.Message{
			Role:    brtypes.ConversationRoleAssistant,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: reply}},
		}},
		Usage: &brtypes.TokenUsage{
			InputTokens:  aws.Int32(r.inTok),
			OutputTokens: aws.Int32(r.outTok),
			TotalTokens:  aws.Int32(r.inTok + r.outTok),
		},
	}, nil
}

func plannerWith(replies ...string) (*BedrockPlanner, *recordingConverser) {
	rc := &recordingConverser{replies: replies, inTok: 100, outTok: 50}
	p := &BedrockProvider{
		Client: rc,
		Prices: map[string]Pricing{testModel: {InputPerMTok: 1, OutputPerMTok: 5}},
		Now:    func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) },
	}
	return NewBedrockPlanner(p, testModel), rc
}

var someAlloc = quarry.Allocation{Spend: quarry.FromFloat(10)}

// ------------------------------------------------------------------ P1: decline

func TestPlannerDeclineIsAFirstClassAnswer(t *testing.T) {
	// P1 forbids splitting by default, which is only enforceable if the planner can
	// actually say no. A planner whose refusal came back as an empty item list would
	// be indistinguishable from a broken one.
	bp, _ := plannerWith(`{"decline":true,"reasoning":"atomic question"}`)
	plan, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Declined {
		t.Error("a declining reply must produce a declined plan (P1)")
	}
	if plan.Reasoning != "atomic question" {
		t.Errorf("the reason for declining must survive into the record: %q", plan.Reasoning)
	}
}

func TestPlannerDeclineWinsOverItems(t *testing.T) {
	// A reply that both declines and lists children has contradicted itself. Honouring
	// the decline degrades to one solve; honouring the items runs a split nobody
	// endorsed. The safe reading is the cheap one.
	bp, _ := plannerWith(`{"decline":true,"items":[{"statement":"a","weight":1}]}`)
	plan, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Declined || len(plan.Items) != 0 {
		t.Errorf("a self-contradicting reply must resolve to a decline, got declined=%v items=%d",
			plan.Declined, len(plan.Items))
	}
}

func TestPlannerEmptyPlanIsAnErrorNotADecline(t *testing.T) {
	// The distinction is about the RECORD, not control flow: silently converting
	// "said nothing" into "declined to split" would attribute a deliberate judgement
	// to a planner that made none.
	bp, _ := plannerWith(`{"decline":false,"items":[]}`)
	_, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if !errors.Is(err, ErrUnparseablePlan) {
		t.Fatalf("want ErrUnparseablePlan for an empty non-declining plan, got %v", err)
	}
}

// ------------------------------------------------------- refusing to guess a plan

func TestPlannerUnparseableReplyIsRefusedNotSalvaged(t *testing.T) {
	bp, _ := plannerWith("I think we should look at three things. First,")
	_, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if !errors.Is(err, ErrUnparseablePlan) {
		t.Fatalf("prose must not be read as a plan: %v", err)
	}
}

func TestPlannerAcceptsFencedJSON(t *testing.T) {
	// A code fence is a formatting artifact, not a wrong answer. Failing runs over
	// backticks would be strictness pointed at the wrong thing — unwrapping is not
	// guessing, because json.Unmarshal remains the judge of what is inside.
	bp, _ := plannerWith("Here is the plan:\n```json\n{\"items\":[{\"statement\":\"a\",\"weight\":1}]}\n```\n")
	plan, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 {
		t.Errorf("want 1 item out of the fenced reply, got %d", len(plan.Items))
	}
}

func TestPlannerRejectsNonPositiveWeight(t *testing.T) {
	// Zero is ambiguous — "free" or "omit"? — and guessing which is not this layer's
	// call. Catching it here also names the planner rather than letting Apportion
	// report it as a weights problem.
	bp, _ := plannerWith(`{"items":[{"statement":"a","weight":1},{"statement":"b","weight":0}]}`)
	_, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if !errors.Is(err, ErrUnparseablePlan) {
		t.Fatalf("want ErrUnparseablePlan for weight 0, got %v", err)
	}
	if !strings.Contains(err.Error(), "item 1") {
		t.Errorf("the error must name the offending item, got %q", err)
	}
}

func TestPlannerRejectsEmptyStatement(t *testing.T) {
	bp, _ := plannerWith(`{"items":[{"statement":"   ","weight":1}]}`)
	_, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if !errors.Is(err, ErrUnparseablePlan) {
		t.Fatalf("want ErrUnparseablePlan for a blank statement, got %v", err)
	}
}

func TestPlannerRejectsOverwideFanoutRatherThanTruncating(t *testing.T) {
	// Truncating would discard sub-problems the planner deemed necessary and then
	// record the remainder as if it were the whole plan — a decomposition nobody chose,
	// reported as one somebody did.
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; i < 5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"statement":"s","weight":1}`)
	}
	b.WriteString(`]}`)

	bp, _ := plannerWith(b.String())
	bp.MaxItems = 3
	_, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if !errors.Is(err, quarry.ErrPlanDoesNotFit) {
		t.Fatalf("want ErrPlanDoesNotFit for 5 items under MaxItems 3, got %v", err)
	}
}

// ------------------------------------------------------------------- P6: scope

func TestPlannerChildrenInheritScopeAndCannotWidenIt(t *testing.T) {
	// P6: scope never widens on descent. The enforcement here is STRUCTURAL — the
	// wire shape has no scope field at all, so a planner cannot name its children's
	// entitlements even to narrow them. This test pins that the reply's shape stays
	// unable to influence scope: extra JSON keys are ignored, and the parent's scope
	// lands on every child.
	scope := quarry.Scope{Tags: map[string]string{"agate:dept": "bio", "agate:proj": "x"}}
	bp, _ := plannerWith(`{"items":[{"statement":"a","weight":1,"scope":{"tags":{"agate:dept":"bio"}}}]}`)
	plan, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q", Scope: scope}, someAlloc, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].Problem.Scope.Key(); got != scope.Key() {
		t.Errorf("child scope must be inherited from the parent, not read from the reply: got %q want %q",
			got, scope.Key())
	}
}

// ---------------------------------------------------------------- P9: budget in

func TestPlanningIsBudgetConditioned(t *testing.T) {
	// P9: "decompose this given this balance". A planner that never sees the budget is
	// planning first and discovering the constraint later, which is the ordering P9
	// exists to forbid.
	bp, rc := plannerWith(`{"decline":true}`)
	_, _ = bp.Plan(context.Background(), quarry.Problem{Statement: "q"},
		quarry.Allocation{Spend: quarry.FromFloat(1)}, 0, nil)
	limited := rc.prompts[0]

	bp2, rc2 := plannerWith(`{"decline":true}`)
	_, _ = bp2.Plan(context.Background(), quarry.Problem{Statement: "q"},
		quarry.Allocation{Spend: quarry.Unlimited}, 0, nil)
	unlimited := rc2.prompts[0]

	if limited == unlimited {
		t.Error("the prompt must differ with the budget, or planning is not budget-conditioned (P9)")
	}
	if !strings.Contains(limited, "BUDGET") {
		t.Error("the budget must be stated to the planner")
	}
}

func TestPlannerIsNeverToldACurrencyAmount(t *testing.T) {
	// §2: the planner emits RELATIVE weights and the Ledger does the arithmetic. A
	// model handed a dollar figure will try to price the work, which is exactly the
	// absolute estimation §2 avoids — and it would make the plan depend on the
	// advisory estimator that P4 says nothing may depend on.
	bp, rc := plannerWith(`{"decline":true}`)
	_, _ = bp.Plan(context.Background(), quarry.Problem{Statement: "q"},
		quarry.Allocation{Spend: quarry.FromFloat(37.5)}, 0, nil)
	prompt := rc.prompts[0]
	for _, leak := range []string{"37.5", "37500000", "$"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("prompt leaks an absolute amount (%q): %s", leak, prompt)
		}
	}
}

// ------------------------------------------------------------------ §2: strategy

func TestPlannerReadsPortfolioStrategy(t *testing.T) {
	bp, _ := plannerWith(`{"strategy":"portfolio","items":[{"statement":"q","weight":1},{"statement":"q","weight":1}]}`)
	plan, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IsPortfolio() {
		t.Error("a portfolio reply must produce a portfolio plan")
	}
	if len(plan.Items) != 2 {
		t.Errorf("portfolio arms must not collapse: got %d items", len(plan.Items))
	}
}

func TestMissingStrategyMeansPartition(t *testing.T) {
	// The zero value is the partition, so a planner written before strategies existed
	// still means what it meant.
	bp, _ := plannerWith(`{"items":[{"statement":"a","weight":1}]}`)
	plan, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IsPortfolio() || plan.Strategy != quarry.StrategyPartition {
		t.Errorf("an unset strategy must be a partition, got %q", plan.Strategy)
	}
}

func TestUnknownStrategyIsAnErrorNotADefault(t *testing.T) {
	// Defaulting to partition would run a shape the planner did not ask for, and the
	// two shapes disagree about what identical child statements MEAN (§2): under
	// partition duplicates are redundant work to collapse, under portfolio they are
	// the entire point. Guessing picks one of two contradictory readings.
	bp, _ := plannerWith(`{"strategy":"debate","items":[{"statement":"a","weight":1}]}`)
	_, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil)
	if !errors.Is(err, ErrUnparseablePlan) {
		t.Fatalf("want ErrUnparseablePlan for an unknown strategy, got %v", err)
	}
}

// -------------------------------------------------------------- §8.1: anchoring

func TestPriorIsNotShownToThePlanner(t *testing.T) {
	// §8.1 is unresolved and this is the conservative side of it: §7 names an
	// INDEPENDENT decomposition as the strongest replication signal available, so
	// showing the planner its previous split would trade the best evidence the system
	// has for a warmer start. If §12 later resolves the other way, this test is the
	// thing to change — deliberately, and with the doc.
	prior := []quarry.NodeOutcome{{
		Problem: quarry.Problem{Statement: "SENTINEL-PRIOR-SUBPROBLEM"},
		Content: "SENTINEL-PRIOR-CONTENT",
	}}
	bp, rc := plannerWith(`{"decline":true}`)
	_, _ = bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 1, prior)
	if strings.Contains(rc.prompts[0], "SENTINEL-PRIOR") {
		t.Error("the prior decomposition must not reach the planner while §8.1 is open")
	}
}

// ------------------------------------------------------------------ plumbing

func TestPlannerPropagatesProviderError(t *testing.T) {
	rc := &recordingConverser{err: errors.New("throttled")}
	bp := NewBedrockPlanner(&BedrockProvider{Client: rc, Now: func() time.Time { return time.Time{} }}, testModel)
	if _, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil); err == nil {
		t.Error("a provider fault must propagate rather than yield an empty plan")
	}
}

func TestPlannerWithoutProviderFailsLoudly(t *testing.T) {
	bp := &BedrockPlanner{Model: testModel}
	if _, err := bp.Plan(context.Background(), quarry.Problem{Statement: "q"}, someAlloc, 0, nil); err == nil {
		t.Error("an unwired planner must error, not silently decline")
	}
}
