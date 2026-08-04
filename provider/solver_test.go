package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// These tests are the specification for the budget-conditioned leaf (§2, "How a leaf
// is told about its budget", P9). A failing test means the design changed — amend
// docs/design.md in the same commit or revert.

// --------------------------------------------------------- no currency in the prompt

func TestLeafPromptNamesNoCurrency(t *testing.T) {
	// THE constraint §2 fixes: the budget reaches the leaf in terms the model can act
	// on, never as money. A model told "you have $0.002" is being asked to price tokens
	// it cannot see, and its brevity would then track its guess about pricing rather
	// than the budget.
	//
	// Reintroduce the defect by interpolating alloc.Spend into leafPrompt and this
	// fails on the guarantee — the number appears — not on a mechanism.
	spend := quarry.FromFloat(0.002137) // distinctive digits, so a partial leak is caught too
	solver := BudgetedSolver{Provider: &FakeProvider{}, Model: "fake"}
	rec := &promptRecorder{Budgeter: &FakeProvider{}}
	solver.Provider = rec

	_, err := solver.Solve(context.Background(), quarry.Problem{Statement: "how much does storage cost?"},
		quarry.Allocation{Spend: spend})
	if err != nil {
		t.Fatal(err)
	}

	// Every rendering of the amount the solver was handed, and of the share it
	// actually priced against. None may appear.
	forbidden := []string{
		spend.String(),                  // "0.0021"
		"0.002137",                      // the float form
		"2137",                          // micro-units, in case someone prints them raw
		solver.headroom(spend).String(), // the divided share
		"$",                             // any currency sigil at all
	}
	for _, f := range forbidden {
		if strings.Contains(rec.prompt, f) {
			t.Errorf("the leaf prompt must name no currency; found %q in:\n%s", f, rec.prompt)
		}
	}
}

func TestLeafPromptEndsWithTheStatement(t *testing.T) {
	// Load-bearing beyond readability. FakeProvider's answer echoes the tail after the
	// final newline for any prompt over 120 characters, so a preamble placed after the
	// statement makes every --fake answer echo quarry's own instructions instead of the
	// question — and --fake is how the system is demonstrated.
	stmt := "what dominates the bill?"
	got := leafPrompt(quarry.Problem{Statement: stmt}, 300, false)
	if !strings.HasSuffix(got, stmt) {
		t.Errorf("the statement must be last in the prompt, got tail %q", got[max(0, len(got)-80):])
	}

	// And the property that depends on it, asserted through the fake rather than
	// assumed: the answer echoes the QUESTION.
	f := &FakeProvider{}
	s, err := f.CompleteBounded(context.Background(), got, "fake", quarry.Scope{}, 300)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Content, "what dominates the bill") {
		t.Errorf("the fake must echo the question, not the preamble: %q", s.Content)
	}
}

// ------------------------------------------------------------------ the ceiling

func TestUnlimitedSpendYieldsNoCeiling(t *testing.T) {
	// Absence is not zero, and it is not the minimum either. An unlimited allowance
	// means "no stated limit", which maps to maxOut 0 — the model's own default,
	// MaxTokens' existing convention — NOT to MinLeafOutputTokens, which would make an
	// uncapped run the most tightly constrained one in the system.
	for _, b := range []Budgeter{&FakeProvider{}, testProvider(&fakeConverser{})} {
		if got := b.Ceiling(testModel, quarry.Unlimited); got != 0 {
			t.Errorf("%T: unlimited spend must yield no ceiling, got %d", b, got)
		}
	}
}

func TestUnpricedModelYieldsNoCeiling(t *testing.T) {
	// Matches how price already treats a missing sheet: the absence surfaces rather
	// than being filled in. A fabricated ceiling would cap generation on a number
	// nothing supports.
	p := testProvider(&fakeConverser{})
	if got := p.Ceiling("some.model.nobody.priced", quarry.FromFloat(1)); got != 0 {
		t.Errorf("an unpriced model must yield no ceiling, got %d", got)
	}
}

func TestCeilingIsMonotonicAndClamped(t *testing.T) {
	p := testProvider(&fakeConverser{})

	// Monotonic: more money never buys a smaller answer.
	prev := int32(-1)
	for _, spend := range []float64{0.0001, 0.001, 0.002, 0.005, 0.01, 1.0} {
		got := p.Ceiling(testModel, quarry.FromFloat(spend))
		if got < prev {
			t.Errorf("ceiling must not shrink as spend grows: %v gave %d after %d", spend, got, prev)
		}
		prev = got
	}

	// Clamped below. A node allocated almost nothing must not be handed a ceiling that
	// buys a fragment: deciding a node is too poor to solve is the FLOOR's job (§3),
	// and a ceiling that degraded it instead would be a second floor with none of the
	// first's bookkeeping.
	if got := p.Ceiling(testModel, 1); got != MinLeafOutputTokens {
		t.Errorf("a near-zero allowance must clamp to the minimum, got %d", got)
	}
	// Clamped above, so a large cap is not read as a licence to write at length.
	if got := p.Ceiling(testModel, quarry.FromFloat(100)); got != MaxLeafOutputTokens {
		t.Errorf("a large allowance must clamp to the maximum, got %d", got)
	}
}

func TestWordBudgetIsDerivedFromTheCeiling(t *testing.T) {
	// The request and the cap come from ONE number on purpose. Two independently
	// configured limits drift, and the failure when they do is silent: a prompt asking
	// for 400 words under a 200-token ceiling produces confident answers that stop
	// mid-sentence.
	//
	// The direction is also load-bearing: the stated word budget must fit INSIDE what
	// the ceiling can hold, so a model that obeys the request is not truncated — which
	// is what makes a stop-at-ceiling signal mean "the model ignored the request".
	for _, maxOut := range []int32{MinLeafOutputTokens, 300, MaxLeafOutputTokens} {
		words := wordBudget(maxOut)
		if words < 1 {
			t.Fatalf("maxOut %d gave a word budget of %d", maxOut, words)
		}
		if float64(words) > float64(maxOut)*0.75 {
			t.Errorf("word budget %d must fit inside %d tokens at ~0.75 words/token",
				words, maxOut)
		}
		if !strings.Contains(leafPrompt(quarry.Problem{Statement: "q"}, maxOut, false),
			"at most "+itoa(words)+" words") {
			t.Errorf("the prompt must state the derived word budget for maxOut %d", maxOut)
		}
	}
}

func TestNoCeilingIsStatedRatherThanOmitted(t *testing.T) {
	// Symmetric with budgetLine's unlimited case, for the same reason: a model that
	// cannot tell "no stated limit" from "a limit I was not told" hedges against a cap
	// that is not there.
	got := leafPrompt(quarry.Problem{Statement: "q"}, 0, false)
	if !strings.Contains(got, "no stated length limit") {
		t.Errorf("an unpriceable ceiling must be stated, not omitted:\n%s", got)
	}
	if strings.Contains(got, "at most") {
		t.Errorf("no word count may be named when none could be derived:\n%s", got)
	}
}

// ------------------------------------------------------------- the two halves agree

func TestSolveAppliesTheCeilingItStates(t *testing.T) {
	// The prompt is a REQUEST; the ceiling is the CAP. Both must be present, and both
	// must come from the same allocation — a solver that stated a word budget and
	// passed maxOut 0 would have shipped the half that does not bind.
	rec := &promptRecorder{Budgeter: testProvider(&fakeConverser{reply: "ok", out: 10})}
	solver := BudgetedSolver{Provider: rec, Model: testModel}

	if _, err := solver.Solve(context.Background(),
		quarry.Problem{Statement: "how does it scale?"},
		quarry.Allocation{Spend: quarry.FromFloat(0.01)}); err != nil {
		t.Fatal(err)
	}
	if rec.maxOut <= 0 {
		t.Fatalf("Solve must pass a ceiling, got %d", rec.maxOut)
	}
	if !strings.Contains(rec.prompt, "at most "+itoa(wordBudget(rec.maxOut))+" words") {
		t.Errorf("the stated word budget must match the ceiling %d passed:\n%s",
			rec.maxOut, rec.prompt)
	}
}

func TestSolveMentionsTheDeadlineOnlyWhenThereIsOne(t *testing.T) {
	// The deadline half of the Allocation was the other thing ProviderSolver discarded.
	// Absent means absent: a run with no deadline must not be told to hurry.
	withNone := leafPrompt(quarry.Problem{Statement: "q"}, 300, false)
	withOne := leafPrompt(quarry.Problem{Statement: "q"}, 300, true)
	if strings.Contains(withNone, "deadline") {
		t.Errorf("no deadline means no deadline sentence:\n%s", withNone)
	}
	if !strings.Contains(withOne, "deadline") {
		t.Errorf("a deadline must be stated:\n%s", withOne)
	}

	// And the plumbing that decides which: a zero Allocation.Deadline is "unset".
	rec := &promptRecorder{Budgeter: &FakeProvider{}}
	solver := BudgetedSolver{Provider: rec, Model: "fake"}
	if _, err := solver.Solve(context.Background(), quarry.Problem{Statement: "q"},
		quarry.Allocation{Spend: quarry.FromFloat(0.01), Deadline: time.Time{}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.prompt, "deadline") {
		t.Errorf("a zero deadline is UNSET, not imminent:\n%s", rec.prompt)
	}
}

func TestSolveWithoutAProviderIsAnError(t *testing.T) {
	// Same contract as ProviderSolver: a missing provider is a wiring fault, reported
	// rather than nil-panicked deep in a goroutine the executor is fanning out.
	if _, err := (BudgetedSolver{Model: "fake"}).Solve(context.Background(),
		quarry.Problem{Statement: "q"}, quarry.Allocation{}); err == nil {
		t.Error("a solver with no provider must report it")
	}
}

func TestHeadroomLeavesRoomForARetry(t *testing.T) {
	// The Solver seam cannot see Executor.MaxRetries (§5), so a leaf that spent its
	// whole balance on the first attempt would make the executor's retry budget
	// fictional — the first answer would also be the last regardless of the verdict.
	solver := BudgetedSolver{Provider: &FakeProvider{}, Model: "fake"}
	spend := quarry.FromFloat(0.03)
	if got := solver.headroom(spend); got >= spend {
		t.Errorf("headroom must leave room for a re-solve: %s of %s", got, spend)
	}
	// Unlimited stays unlimited rather than becoming a large number: absence is not a
	// big value any more than it is zero.
	if got := solver.headroom(quarry.Unlimited); got.Limited() {
		t.Errorf("unlimited spend must divide to unlimited, got %s", got)
	}
}

// -------------------------------------------------------------------- helpers

// promptRecorder wraps a Budgeter and captures what the solver actually sent. The
// assertions here are about the PROMPT and the CEILING, which no other seam exposes.
type promptRecorder struct {
	Budgeter
	prompt string
	maxOut int32
}

func (p *promptRecorder) CompleteBounded(ctx context.Context, prompt, model string, scope quarry.Scope, maxOut int32) (quarry.Sample, error) {
	p.prompt, p.maxOut = prompt, maxOut
	return p.Budgeter.CompleteBounded(ctx, prompt, model, scope, maxOut)
}

// itoa avoids pulling strconv in for one call in a test file that is otherwise about
// strings.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
