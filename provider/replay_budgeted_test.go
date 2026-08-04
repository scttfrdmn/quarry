package provider

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// The P8 guarantee for the budget-conditioned leaf, and the test whose absence would
// have let the change ship broken.
//
// RecordedProvider INDEXES recorded samples by the recorded Problem and LOOKS THEM UP
// by the prompt it is handed (record.go). Those coincide only while the solver sends
// the bare statement — so wrapping the leaf prompt is exactly the change that could
// make every leaf replay miss with "replay diverged", reporting a divergence against a
// faithful record. That is why the wrapping lives in the Solver, above the Provider,
// and why both directions are asserted here rather than reasoned about in a comment.
//
// This test lives in provider/ rather than in the root package because it needs
// BudgetedSolver, which cannot live in the core (it needs a price sheet, Go rule 4).

// budgetedRun executes a small real tree with the budgeted solver over the fake
// provider and returns its record.
func budgetedRun(t *testing.T, solver quarry.Solver) quarry.RunRecord {
	t.Helper()
	caps := quarry.Caps{Spend: quarry.FromFloat(1), Latency: time.Hour}
	// Multi-clause: FakePlanner declines to split a short statement, and a single node
	// cannot demonstrate that LEAVES replay — the guarantee under test is about leaf
	// lookups, so the tree has to have leaves that are not the root.
	root := quarry.Problem{Statement: "How much does storage cost, how does it scale, " +
		"and what dominates the bill?"}
	l, err := quarry.NewLedger(caps, root.Scope)
	if err != nil {
		t.Fatal(err)
	}
	e := &quarry.Executor{
		Planner:  FakePlanner{},
		Solver:   solver,
		Reducer:  quarry.ConcatReducer{Sep: "\n"},
		Now:      time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC),
		MaxDepth: 2,
	}
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	rec := quarry.NewRunRecord(res, root, caps, quarry.ModeFresh)
	// The fixture must actually produce the shape it claims to, or the assertions below
	// are vacuous: a one-node tree has no leaf lookups to get wrong.
	leaves := 0
	for _, o := range rec.Outcomes {
		if len(o.Children) == 0 && o.Content != "" {
			leaves++
		}
	}
	if leaves < 2 {
		t.Fatalf("this fixture must produce at least two answered leaves, got %d of %d nodes",
			leaves, len(rec.Outcomes))
	}
	return rec
}

func TestBudgetedRunReplaysThroughTheBareStatementSolver(t *testing.T) {
	orig := budgetedRun(t, BudgetedSolver{Provider: &FakeProvider{}, Model: "fake"})

	// Replay wires ProviderSolver — the BARE STATEMENT solver — against the recorded
	// seams, which is exactly what cmd/quarry's replayExecutor does.
	seams := quarry.Replayable(orig)
	replayed := budgetedRun(t, quarry.ProviderSolver{Provider: seams.Provider, Model: "fake"})

	ob, err := orig.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := replayed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, rb) {
		t.Fatalf("a budgeted run must replay byte-for-byte (P8)\n orig: %s\n rep:  %s", ob, rb)
	}
	if orig.RunID != replayed.RunID {
		t.Errorf("identical content must hash identically: %s vs %s", orig.RunID, replayed.RunID)
	}
}

func TestTheBudgetPreambleNeverEntersTheRecord(t *testing.T) {
	// The structural reason the replay above works, asserted directly rather than
	// inferred from it: the executor records the PROBLEM, not the prompt, so the
	// recorded key is stable no matter how leafPrompt is later reworded.
	//
	// Without this, a future change that moved prompt construction down into a Provider
	// would break replay and this file's other test would report it as a hash mismatch
	// — true but uninformative. This one names the cause.
	orig := budgetedRun(t, BudgetedSolver{Provider: &FakeProvider{}, Model: "fake"})

	// A phrase from every budgeted prompt, and one from the shape rules.
	for _, marker := range []string{"BUDGET:", "PROBLEM:", "No preamble"} {
		for _, o := range orig.Outcomes {
			if strings.Contains(o.Problem.Statement, marker) {
				t.Errorf("node %s recorded the PROMPT rather than the problem (found %q): %.120q",
					o.NodeID, marker, o.Problem.Statement)
			}
		}
	}
}

func TestBudgetedSolverInAReplayIsALoudFailure(t *testing.T) {
	// THE TRAP, asserted. Wiring BudgetedSolver into a replay is the plausible-looking
	// "consistency" fix, and it must fail loudly rather than quietly answer off a wrong
	// key. A silent miss here would be the worst outcome available: replay is the load-
	// bearing claim of the whole system (P8), so a replay that appeared to work while
	// serving something other than the recorded sample would invalidate the guarantee
	// rather than merely fail to demonstrate it.
	orig := budgetedRun(t, BudgetedSolver{Provider: &FakeProvider{}, Model: "fake"})
	seams := quarry.Replayable(orig)

	// Reach the lookup directly: the wrapped prompt cannot key to a recorded statement.
	prompt := leafPrompt(orig.Outcomes[1].Problem, 300, false)
	_, err := seams.Provider.Complete(context.Background(), prompt, "fake",
		orig.Outcomes[1].Problem.Scope)
	if err == nil {
		t.Fatal("a wrapped prompt must not resolve to a recorded sample")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("the miss must name divergence, not read as a provider fault: %v", err)
	}

	// And the bare statement — what ProviderSolver sends — does resolve. Both halves,
	// because a test that only showed the miss could not distinguish "keyed on the
	// statement" from "nothing replays at all".
	if _, err := seams.Provider.Complete(context.Background(),
		orig.Outcomes[1].Problem.Statement, "fake", orig.Outcomes[1].Problem.Scope); err != nil {
		t.Errorf("the bare statement must resolve to the recorded sample: %v", err)
	}
}

func TestBoundedFakeCallsCostLessThanUnboundedOnes(t *testing.T) {
	// --fake must exercise the budgeted path rather than stub it, or the CLI's fake
	// branch would demonstrate a solver nobody runs. The ceiling has to actually move
	// money on a fake run for admission control (§3) to see any difference.
	f := &FakeProvider{}
	prompt := leafPrompt(quarry.Problem{Statement: "what dominates the bill?"}, 300, false)

	unbounded, err := f.CompleteBounded(context.Background(), prompt, "fake", quarry.Scope{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 1 token: far below anything fakeTokens produces (30-130), so the clamp is
	// guaranteed to bite. A ceiling inside that range would make this test pass or fail
	// on a hash.
	bounded, err := f.CompleteBounded(context.Background(), prompt, "fake", quarry.Scope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bounded.GeneratedTokens >= unbounded.GeneratedTokens {
		t.Errorf("a ceiling must cut generated tokens: %d bounded vs %d unbounded",
			bounded.GeneratedTokens, unbounded.GeneratedTokens)
	}
	if bounded.Cost >= unbounded.Cost {
		t.Errorf("fewer generated tokens must cost less: %s vs %s", bounded.Cost, unbounded.Cost)
	}
	// Content is UNAFFECTED on purpose: the fake's answer is a hash-derived sentence,
	// not generated text, and truncating it would break the property fakeAnswer is
	// careful about — that it extracts as exactly one claim.
	if bounded.Content != unbounded.Content {
		t.Errorf("the fake's content must not vary with the ceiling:\n %q\n %q",
			bounded.Content, unbounded.Content)
	}
}
