package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// Tests for the model-backed Reducer. The load-bearing claim under test is the one
// seams.go states outright: the strategy argument "cannot be inferred", so a reducer
// that ignores it is silently wrong on every portfolio. Most of this file exists to
// make that failure impossible to reintroduce.

func reducerWith(replies ...string) (*BedrockReducer, *recordingConverser) {
	rc := &recordingConverser{replies: replies, inTok: 100, outTok: 20}
	p := &BedrockProvider{
		Client: rc,
		Prices: map[string]Pricing{testModel: {InputPerMTok: 1, OutputPerMTok: 5}},
		Now:    func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) },
	}
	return NewBedrockReducer(p, testModel), rc
}

func child(id, stmt, content string) quarry.NodeOutcome {
	return quarry.NodeOutcome{NodeID: id, Problem: quarry.Problem{Statement: stmt}, Content: content}
}

func verifiedChild(id, stmt, content string) quarry.NodeOutcome {
	ok := true
	c := child(id, stmt, content)
	c.Verified = &ok
	return c
}

// -------------------------------------------------------- §2: merge vs select

func TestPartitionMergesAndPortfolioSelects(t *testing.T) {
	// The two strategies get DIFFERENT prompts because they are different operations,
	// not two flavours of one. Concatenating five attempts at a question yields five
	// answers; merging is only meaningful when the children differ.
	kids := []quarry.NodeOutcome{child("n1", "a", "A"), child("n2", "b", "B")}

	mr, mrc := reducerWith("merged answer")
	if _, err := mr.Reduce(context.Background(), quarry.Problem{Statement: "q"}, kids,
		quarry.Allocation{}, false, quarry.StrategyPartition); err != nil {
		t.Fatal(err)
	}
	sr, src := reducerWith("1")
	if _, err := sr.Reduce(context.Background(), quarry.Problem{Statement: "q"}, kids,
		quarry.Allocation{}, false, quarry.StrategyPortfolio); err != nil {
		t.Fatal(err)
	}
	if mrc.prompts[0] == src.prompts[0] {
		t.Fatal("merge and select must not share a prompt — the strategy is load-bearing (§2)")
	}
	if !strings.Contains(src.prompts[0], "ATTEMPTS") {
		t.Error("a portfolio must be presented as competing attempts")
	}
	if !strings.Contains(mrc.prompts[0], "SUB-ANSWERS") {
		t.Error("a partition must be presented as sub-answers to combine")
	}
}

func TestSelectionReturnsAnArmVerbatimNeverASynthesis(t *testing.T) {
	// The model is asked for an INDEX, and only the named arm's text is returned. If a
	// selector were allowed to write prose, the run would be billed for a synthesis
	// nobody planned and the answer would match no recorded node — severing the link
	// between the answer and its producer (§8, P8).
	arms := []quarry.NodeOutcome{child("n1", "q", "first attempt"), child("n2", "q", "second attempt")}
	br, _ := reducerWith("2")
	got, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, arms,
		quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "second attempt" {
		t.Errorf("want arm 2 verbatim, got %q", got.Content)
	}
}

func TestSelectionChargesTheSelectionNotTheArm(t *testing.T) {
	// The arms recorded their own spend as their own nodes. Re-reporting the winner's
	// cost here would double-count it in the tree total; what this node legitimately
	// costs is the one selection call.
	arms := []quarry.NodeOutcome{
		{NodeID: "n1", Problem: quarry.Problem{Statement: "q"}, Content: "a", Cost: quarry.FromFloat(9)},
	}
	br, _ := reducerWith("1")
	got, _ := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, arms,
		quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if got.Cost == quarry.FromFloat(9) {
		t.Error("the selection must not inherit the arm's cost — that double-counts the arm")
	}
	if got.Cost == 0 {
		t.Error("the selection call itself cost money and must be reported")
	}
}

func TestVerifiedArmsAreFlaggedToTheSelector(t *testing.T) {
	// A verdict is real evidence and the selector cannot see it otherwise. Unverified
	// is left UNSAID rather than labelled: "not checked" and "checked and failed" are
	// different facts, and conflating them is the §8 error this codebase refuses.
	arms := []quarry.NodeOutcome{child("n1", "q", "plain"), verifiedChild("n2", "q", "checked")}
	br, rc := reducerWith("2")
	_, _ = br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, arms,
		quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if !strings.Contains(rc.prompts[0], "passed an independent check") {
		t.Error("a verified arm must be flagged to the selector")
	}
	if strings.Count(rc.prompts[0], "passed an independent check") != 1 {
		t.Error("only the verified arm may be flagged")
	}
}

func TestSelectionDoesNotClaimAVerdictItDidNotPerform(t *testing.T) {
	// Copying the winning arm's Verified onto the selection would let the parent claim
	// a check it never made. The verdict belongs to the arm's own node.
	arms := []quarry.NodeOutcome{verifiedChild("n1", "q", "checked")}
	br, _ := reducerWith("1")
	got, _ := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, arms,
		quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if got.Verified != nil {
		t.Error("the selection must not carry the arm's verdict forward")
	}
}

// ------------------------------------------------------ unparseable selection

func TestUnparseableSelectionIsRefusedByDefault(t *testing.T) {
	arms := []quarry.NodeOutcome{child("n1", "q", "a"), child("n2", "q", "b")}
	br, _ := reducerWith("they are both good in different ways")
	_, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, arms,
		quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if !errors.Is(err, ErrUnparseableSelection) {
		t.Fatalf("want ErrUnparseableSelection, got %v", err)
	}
}

func TestOutOfRangeSelectionIsRefused(t *testing.T) {
	// A selector naming arm 7 of 2 answered about arms that do not exist; its choice
	// cannot be honoured, and clamping to the last arm would honour a different choice
	// while recording it as the model's.
	arms := []quarry.NodeOutcome{child("n1", "q", "a"), child("n2", "q", "b")}
	br, _ := reducerWith("7")
	_, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, arms,
		quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if !errors.Is(err, ErrUnparseableSelection) {
		t.Fatalf("want ErrUnparseableSelection for an out-of-range arm, got %v", err)
	}
}

func TestSelectFallbackIsOptInAndStillCharges(t *testing.T) {
	// The money was spent whether or not the reply parsed. Hiding that would make the
	// ledger wrong in the direction that flatters the system — the one direction a cost
	// receipt must never be wrong in (§8.2).
	arms := []quarry.NodeOutcome{child("n1", "q", "a"), verifiedChild("n2", "q", "checked")}
	br, _ := reducerWith("no idea")
	br.SelectFallback = true
	got, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, arms,
		quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "checked" {
		t.Errorf("the mechanical rule prefers a verified arm, got %q", got.Content)
	}
	if got.Cost == 0 {
		t.Error("a wasted selection call must still be charged")
	}
}

func TestParseIndexReadsCommonReplyShapes(t *testing.T) {
	// Models answer "2", "2.", "Attempt 2", "[2]" — all naming the same arm
	// unambiguously. Prose ("the second one") is NOT interpreted: inferring an index
	// from words is guessing, and the fallback exists for exactly that case.
	for _, reply := range []string{"2", "2.", "Attempt 2", "[2]", " 2 \n"} {
		if got, ok := parseIndex(reply, 3); !ok || got != 1 {
			t.Errorf("parseIndex(%q) = %d,%v; want 1,true", reply, got, ok)
		}
	}
	for _, reply := range []string{"the second one", "", "none", "0", "4"} {
		if _, ok := parseIndex(reply, 3); ok {
			t.Errorf("parseIndex(%q) must not resolve to an arm", reply)
		}
	}
}

// ----------------------------------------------------- §3.1: partial tolerance

func TestGappedChildrenDoNotAbortTheMerge(t *testing.T) {
	// Budget exhaustion lets you stop spending; a deadline does not let you return
	// later. Whatever came back must fold into a returnable answer now (§3.1).
	kids := []quarry.NodeOutcome{
		child("n1", "a", "A"),
		{NodeID: "n2", Problem: quarry.Problem{Statement: "b"}, Gap: true},
	}
	br, rc := reducerWith("partial answer")
	got, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, kids,
		quarry.Allocation{}, false, quarry.StrategyPartition)
	if err != nil {
		t.Fatalf("a gapped child must not abort the merge: %v", err)
	}
	if got.Content != "partial answer" {
		t.Errorf("want the merged answer, got %q", got.Content)
	}
	if strings.Contains(rc.prompts[0], `SUB-QUESTION: b`) {
		t.Error("a gapped child contributes nothing and must not appear as a sub-answer")
	}
}

func TestPartialityIsDetectedFromTheInputNotOnlyTheFlag(t *testing.T) {
	// The executor sets partial from tree state; the reducer also sees it directly in
	// its own input. An unhedged partial answer reads exactly like a complete one, and
	// the reducer is the only component positioned to hedge the prose (§8).
	kids := []quarry.NodeOutcome{child("n1", "a", "A"), {NodeID: "n2", Gap: true}}
	br, rc := reducerWith("x")
	_, _ = br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, kids,
		quarry.Allocation{}, false /* executor says complete */, quarry.StrategyPartition)
	if !strings.Contains(rc.prompts[0], "SUB-ANSWERS ARE MISSING") {
		t.Error("the reducer must hedge when its own input is incomplete, whatever the flag said")
	}
}

func TestCompleteInputIsNotHedged(t *testing.T) {
	kids := []quarry.NodeOutcome{child("n1", "a", "A"), child("n2", "b", "B")}
	br, rc := reducerWith("x")
	_, _ = br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, kids,
		quarry.Allocation{}, false, quarry.StrategyPartition)
	if strings.Contains(rc.prompts[0], "SUB-ANSWERS ARE MISSING") {
		t.Error("a complete merge must not be told to hedge")
	}
}

func TestNoUsableChildrenSpendsNothing(t *testing.T) {
	// Paying a model to summarize an empty set bills the run for producing nothing.
	// An empty sample is the honest result; the parent records the gap.
	kids := []quarry.NodeOutcome{{NodeID: "n1", Gap: true}, {NodeID: "n2", Content: "  "}}
	br, rc := reducerWith("should never be called")
	got, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, kids,
		quarry.Allocation{}, true, quarry.StrategyPartition)
	if err != nil {
		t.Fatalf("an all-gapped merge must not error (§3.1): %v", err)
	}
	if got.Content != "" || got.Cost != 0 {
		t.Errorf("want an empty free sample, got %q at %s", got.Content, got.Cost)
	}
	if len(rc.prompts) != 0 {
		t.Error("no model call may be made with nothing to reduce")
	}
}

func TestEmptyPortfolioSelectsNothingRatherThanErroring(t *testing.T) {
	// The same tolerance on the portfolio path: every arm gapping is a gap, not a
	// fault. A run that produced nothing must still be returnable and citable.
	br, _ := reducerWith("1")
	got, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"},
		[]quarry.NodeOutcome{{NodeID: "n1", Gap: true}}, quarry.Allocation{}, true, quarry.StrategyPortfolio)
	if err != nil || got.Content != "" {
		t.Errorf("want an empty sample and no error, got %q / %v", got.Content, err)
	}
}

// ------------------------------------------------------------------ plumbing

func TestReducerIsADistinctAgentFromThePlanner(t *testing.T) {
	// §2: the reducer "needs to see what returned without inheriting the priors that
	// produced the split". This is a structural check, not a behavioural one — the two
	// are separate types with separate model fields, so a shared conversation is not
	// expressible. What the test pins is that the reduce prompt carries no plan: a
	// reducer shown the decomposition's rationale would be re-deriving the planner's
	// priors, which is the thing separation exists to prevent.
	kids := []quarry.NodeOutcome{child("n1", "a", "A")}
	br, rc := reducerWith("merged")
	_, _ = br.Reduce(context.Background(), quarry.Problem{Statement: "q"}, kids,
		quarry.Allocation{}, false, quarry.StrategyPartition)
	for _, planWord := range []string{"decompose", "weight", "decline"} {
		if strings.Contains(strings.ToLower(rc.prompts[0]), planWord) {
			t.Errorf("the reduce prompt must not carry planning framing (%q)", planWord)
		}
	}
}

func TestReducerPropagatesProviderError(t *testing.T) {
	rc := &recordingConverser{err: errors.New("throttled")}
	br := NewBedrockReducer(&BedrockProvider{Client: rc, Now: func() time.Time { return time.Time{} }}, testModel)
	_, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"},
		[]quarry.NodeOutcome{child("n1", "a", "A")}, quarry.Allocation{}, false, quarry.StrategyPartition)
	if err == nil {
		t.Error("a provider fault must propagate rather than yield an empty answer")
	}
}

func TestReducerWithoutProviderFailsLoudly(t *testing.T) {
	br := &BedrockReducer{Model: testModel}
	_, err := br.Reduce(context.Background(), quarry.Problem{Statement: "q"},
		[]quarry.NodeOutcome{child("n1", "a", "A")}, quarry.Allocation{}, false, quarry.StrategyPartition)
	if err == nil {
		t.Error("an unwired reducer must error, not silently return nothing")
	}
}
