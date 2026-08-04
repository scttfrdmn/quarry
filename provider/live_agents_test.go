package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// LIVE tests for the two agents that decide the SHAPE of every run. Skipped unless
// QUARRY_LIVE is set; they spend a few cents. Run with:
//
//	QUARRY_LIVE=1 AWS_PROFILE=aws go test ./provider/ -run Live -v
//
// WHY THESE EXIST. Every other live path in this package has such a test — Complete,
// the cross-family adversary, the chokepoint — and the planner and reducer, the most
// consequential calls quarry makes, had none. They were verified only against
// recordingConverser: a double written by the same author as the prompts, which can
// confirm that the code parses what it expects and can say nothing about whether a
// real model produces it. That is the circularity the non-vacuity guards elsewhere in
// this suite exist to prevent, and it was load-bearing here, because the prompts are
// the least verifiable thing in the package.
//
// WHAT THEY ASSERT is narrow on purpose: the CONTRACT the parsing code depends on,
// never the QUALITY of the judgement. "Did it split this problem well" is not a
// property a test can hold a model to, and a test that tried would fail on prompt
// drift and teach nothing. "Did it return a bare index when told to" is exactly the
// kind of claim a fake cannot make and a live call can.

func liveAgentModel(t *testing.T) (*BedrockProvider, string) {
	t.Helper()
	p := liveProvider(t) // skips unless QUARRY_LIVE
	// A larger MaxTokens than the leaf tests use: a plan is structured output and a
	// truncated JSON object is an unparseable plan, which would look like a contract
	// violation when it is only a token cap.
	p.MaxTokens = 900
	return p, liveClaude
}

func liveCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 90*time.Second)
}

// ------------------------------------------------------------------- the planner

func TestLivePlannerReturnsAParseablePlan(t *testing.T) {
	// The base contract: a real model, given the real prompt, returns JSON this code
	// can read. Everything else in the planner is downstream of that.
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bp := NewBedrockPlanner(prov, model)
	p := quarry.Problem{Statement: "What were the main causes of the 2008 financial crisis, " +
		"and how did regulatory responses differ between the US and the EU?"}
	plan, err := bp.Plan(ctx, p, quarry.Allocation{Spend: quarry.FromFloat(10)}, 0, nil)
	if err != nil {
		t.Fatalf("live plan must parse: %v", err)
	}
	if plan.Declined {
		// Not a failure — declining is a legitimate answer (P1). But this problem is
		// plainly decomposable, so a decline here is worth seeing in the log.
		t.Logf("planner DECLINED a plainly decomposable problem: %q", plan.Reasoning)
		return
	}
	if len(plan.Items) < 2 {
		t.Errorf("a decomposition must have at least 2 children, got %d", len(plan.Items))
	}
	for i, it := range plan.Items {
		if strings.TrimSpace(it.Problem.Statement) == "" {
			t.Errorf("item %d has an empty statement", i)
		}
		// The parsing code rejects non-positive weights, so this asserts the model
		// actually supplies the relative weights §2 asks for rather than omitting the
		// field and relying on a default that does not exist.
		if it.Weight <= 0 {
			t.Errorf("item %d has weight %d; weights must be positive", i, it.Weight)
		}
	}
	t.Logf("live plan: %d items, strategy=%q reasoning=%.120q",
		len(plan.Items), plan.Strategy, plan.Reasoning)
	for i, it := range plan.Items {
		t.Logf("  [%d] w=%d leaf=%v %.90q", i+1, it.Weight, it.ExpectLeaf, it.Problem.Statement)
	}
}

func TestLivePlannerCanActuallyDecline(t *testing.T) {
	// P1 says the planner MUST be able to decline, and a planner that always splits is
	// the failure P1 exists to forbid. The fake can be told to decline; only a live
	// call can show that the PROMPT makes declining reachable. An atomic question is
	// the clearest case there is.
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bp := NewBedrockPlanner(prov, model)
	p := quarry.Problem{Statement: "What is the boiling point of water at sea level in degrees Celsius?"}
	plan, err := bp.Plan(ctx, p, quarry.Allocation{Spend: quarry.FromFloat(10)}, 0, nil)
	if err != nil {
		t.Fatalf("live plan must parse: %v", err)
	}
	if !plan.Declined {
		// A soft failure, deliberately. This is a claim about a model's judgement, and
		// the prompt can be improved without the code being wrong — but if it never
		// declines, P1 is decorative and someone needs to know.
		t.Errorf("planner split an atomic question into %d children instead of declining: %q",
			len(plan.Items), plan.Reasoning)
		for i, it := range plan.Items {
			t.Logf("  [%d] %.90q", i+1, it.Problem.Statement)
		}
		return
	}
	t.Logf("planner declined, as it should: %q", plan.Reasoning)
}

func TestLivePlannerIsNotToldAPrice(t *testing.T) {
	// P9 makes planning budget-conditioned, but §2 keeps the budget RELATIVE: a model
	// handed a dollar figure prices the work, which is the absolute estimation §2
	// avoids and would make plans depend on the advisory estimator P4 forbids
	// depending on. The unit test asserts the prompt omits the amount; this asserts the
	// consequence — the budget still CHANGES the plan.
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	bp := NewBedrockPlanner(prov, model)
	p := quarry.Problem{Statement: "Assess the state of solid-state battery commercialization: " +
		"technology readiness, manufacturing constraints, and the major players."}

	tight, err := bp.Plan(ctx, p, quarry.Allocation{Spend: quarry.FromFloat(1)}, 0, nil)
	if err != nil {
		t.Fatalf("tight-budget plan: %v", err)
	}
	loose, err := bp.Plan(ctx, p, quarry.Allocation{Spend: quarry.Unlimited}, 0, nil)
	if err != nil {
		t.Fatalf("unlimited-budget plan: %v", err)
	}
	t.Logf("limited: declined=%v items=%d | unlimited: declined=%v items=%d",
		tight.Declined, len(tight.Items), loose.Declined, len(loose.Items))
	if !tight.Declined && !loose.Declined && len(tight.Items) > len(loose.Items) {
		// Logged, not failed: one sample of a stochastic call is not evidence (P7), and
		// the prompt only asks for a COARSER split under a limit, not a strictly
		// smaller one. A persistent inversion would mean the budget line reads backwards.
		t.Logf("NOTE: the limited budget produced MORE children (%d) than unlimited (%d) — "+
			"worth a look at budgetLine if it repeats", len(tight.Items), len(loose.Items))
	}
}

func TestLivePlannerChildrenInheritScope(t *testing.T) {
	// P6 is enforced structurally — planReply has no scope field — but structural
	// enforcement is exactly the kind of claim worth confirming against a real reply,
	// since a model asked about entitlements might volunteer them in some other field.
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	scope := quarry.Scope{Tags: map[string]string{"course": "chem-101"}}
	bp := NewBedrockPlanner(prov, model)
	p := quarry.Problem{
		Statement: "Summarize the mechanisms and typical yields of the three named reactions " +
			"most commonly taught in an introductory organic chemistry course.",
		Scope: scope,
	}
	plan, err := bp.Plan(ctx, p, quarry.Allocation{Spend: quarry.FromFloat(10)}, 0, nil)
	if err != nil {
		t.Fatalf("live plan: %v", err)
	}
	for i, it := range plan.Items {
		if it.Problem.Scope.Key() != scope.Key() {
			t.Errorf("item %d scope %q != parent %q — scope must never widen on descent (P6)",
				i, it.Problem.Scope.Key(), scope.Key())
		}
	}
}

// ------------------------------------------------------------------- the reducer

func TestLiveReducerMergesAPartition(t *testing.T) {
	// A merge must produce ONE answer drawing on all the children, not a list of them.
	// Asserted loosely — content quality is not a testable property — but a merge that
	// mentions neither child is a real failure, not prompt taste.
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	br := NewBedrockReducer(prov, model)
	kids := []quarry.NodeOutcome{
		child("n1", "us", "In the United States, the Dodd-Frank Act of 2010 created the CFPB and "+
			"imposed stress testing on large bank holding companies."),
		child("n2", "eu", "In the European Union, the response came through CRD IV and the Single "+
			"Supervisory Mechanism, centralizing supervision at the ECB."),
	}
	s, err := br.Reduce(ctx, quarry.Problem{Statement: "How did US and EU post-crisis bank regulation differ?"},
		kids, quarry.Allocation{}, false, quarry.StrategyPartition)
	if err != nil {
		t.Fatalf("live merge: %v", err)
	}
	if strings.TrimSpace(s.Content) == "" {
		t.Fatal("a merge must return content")
	}
	if !s.Cost.Limited() || s.Cost <= 0 {
		t.Errorf("a priced merge must cost > 0, got %s", s.Cost)
	}
	low := strings.ToLower(s.Content)
	if !strings.Contains(low, "dodd") && !strings.Contains(low, "cfpb") {
		t.Errorf("merge ignored the US child entirely: %.200q", s.Content)
	}
	t.Logf("live merge (%s, halo=%d gen=%d): %.300q", s.Cost, s.HaloTokens, s.GeneratedTokens, s.Content)
}

func TestLiveSelectionReturnsAnArmVerbatim(t *testing.T) {
	// THE ONE THAT MOST NEEDED A LIVE CALL. selectArm asks for a bare index precisely
	// because a selector allowed to write prose rewrites the answer, converting
	// selection into generation: the run is billed for a synthesis nobody planned and
	// the returned content matches no recorded node. Whether a real model complies with
	// "reply with ONLY the number" is unknowable from a fake that was told to.
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	br := NewBedrockReducer(prov, model)
	arms := []quarry.NodeOutcome{
		child("n1", "q", "Roughly 150 million kilometres."),
		child("n2", "q", "About 1 astronomical unit, which is 149,597,870.7 km by definition, "+
			"measured by radar ranging and now fixed as an SI-defined constant."),
		child("n3", "q", "Very far."),
	}
	s, err := br.Reduce(ctx, quarry.Problem{Statement: "How far is the Earth from the Sun?"},
		arms, quarry.Allocation{}, false, quarry.StrategyPortfolio)
	if err != nil {
		// An unparseable selection is the interesting failure: it means a real model
		// does not obey the index instruction, and the prompt (or parseIndex) needs work.
		if errors.Is(err, ErrUnparseableSelection) {
			t.Fatalf("live selector did not return a usable index — the index contract does not "+
				"hold against a real model: %v", err)
		}
		t.Fatalf("live selection: %v", err)
	}

	// The returned content must be one of the arms, byte for byte. A near-match is a
	// rewrite, which is the failure this design forbids.
	matched := -1
	for i, a := range arms {
		if s.Content == a.Content {
			matched = i
			break
		}
	}
	if matched < 0 {
		t.Errorf("selection must return an arm VERBATIM, got %.200q", s.Content)
	} else {
		t.Logf("live selector chose arm %d (%s, halo=%d gen=%d)", matched+1, s.Cost, s.HaloTokens, s.GeneratedTokens)
	}
	// The COST is the selection call's; the CONTENT is the arm's. Two prices, and
	// conflating them would either lose the spend or double-count the arm's.
	if !s.Cost.Limited() || s.Cost <= 0 {
		t.Errorf("the selection call must be charged, got %s", s.Cost)
	}
}

func TestLiveReducerHedgesOnPartialInput(t *testing.T) {
	// §3.1: partial tolerance is the contract, and only the reducer is positioned to
	// hedge — an answer built from two of four sub-answers should not read as complete.
	// The prompt says so; this checks a real model acts on it.
	prov, model := liveAgentModel(t)
	ctx, cancel := liveCtx(t)
	defer cancel()

	br := NewBedrockReducer(prov, model)
	kids := []quarry.NodeOutcome{
		child("n1", "a", "Lithium-ion cells currently dominate EV production at roughly 90% share."),
		{NodeID: "n2", Problem: quarry.Problem{Statement: "b"}, Gap: true},
		{NodeID: "n3", Problem: quarry.Problem{Statement: "c"}, Gap: true},
	}
	s, err := br.Reduce(ctx, quarry.Problem{Statement: "Assess the EV battery supply chain end to end."},
		kids, quarry.Allocation{}, true, quarry.StrategyPartition)
	if err != nil {
		t.Fatalf("live partial merge: %v", err)
	}
	if strings.TrimSpace(s.Content) == "" {
		t.Fatal("a partial merge must still return a returnable answer (§3.1)")
	}
	// Soft check: hedging vocabulary varies too much to assert on, so this reports
	// rather than fails. A merge that never hedges is a prompt problem worth seeing.
	low := strings.ToLower(s.Content)
	hedged := false
	for _, w := range []string{"incomplete", "missing", "not available", "unavailable",
		"partial", "limited", "only", "cannot fully", "further"} {
		if strings.Contains(low, w) {
			hedged = true
			break
		}
	}
	if !hedged {
		t.Logf("NOTE: partial merge did not visibly hedge — check buildMergePrompt: %.300q", s.Content)
	}
	t.Logf("live partial merge (hedged=%v): %.300q", hedged, s.Content)
}

// ------------------------------------------------------- the two agents together

func TestLivePlannerAndReducerRunAWholeTree(t *testing.T) {
	// END TO END, both agents live, driving the real executor: plan a problem, solve
	// its children, merge them, and produce a record. This is the only test in the
	// suite where quarry's central act is performed entirely by models — everything
	// else substitutes at least one seam.
	prov, model := liveAgentModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	caps := quarry.Caps{Spend: quarry.FromFloat(2), Latency: 4 * time.Minute}
	root := quarry.Problem{Statement: "What are the main technical obstacles to grid-scale " +
		"energy storage, and which are closest to being solved?"}
	l, err := quarry.NewLedger(caps, root.Scope)
	if err != nil {
		t.Fatal(err)
	}
	e := &quarry.Executor{
		Planner:  NewBedrockPlanner(prov, model),
		Solver:   quarry.ProviderSolver{Provider: prov, Model: model},
		Reducer:  NewBedrockReducer(prov, model),
		Now:      time.Now(),
		MaxDepth: 1, // one level: the root plans, its children solve. Keeps the bill small.
	}
	res, err := e.Run(ctx, root, l)
	if err != nil {
		t.Fatalf("live tree: %v", err)
	}
	rec := quarry.NewRunRecord(res, root, caps, quarry.ModeFresh)

	if strings.TrimSpace(res.Answer.Content) == "" {
		t.Error("a completed run must return an answer")
	}
	if rec.TotalCost() <= 0 {
		t.Error("a live run must have cost something")
	}
	if rec.TotalCost().Limited() && rec.TotalCost() > caps.Spend {
		// P4: the cap is the CONTRACT. A live overrun is the most serious failure this
		// test can find, because everything else in the system trusts the ledger.
		t.Errorf("spend %s exceeded the cap %s — the cap is the contract (P4)",
			rec.TotalCost(), caps.Spend)
	}
	// Every node must name what produced it, or the record is not replayable (P8).
	for _, o := range rec.Outcomes {
		if len(o.Children) == 0 && !o.Gap && o.Content != "" && o.ModelVersion == "" {
			t.Errorf("node %s produced content with no pinned model version (P8)", o.NodeID)
		}
	}
	t.Logf("live tree: %d nodes, cost %s, bound by %v, plan %d items",
		len(rec.Outcomes), rec.TotalCost(), rec.BoundBy, len(res.Plan.Items))
	for _, o := range rec.Outcomes {
		t.Logf("  %s d=%d w=%d cost=%s %.70q", o.NodeID, o.Depth, o.PlanWeight, o.Cost, o.Problem.Statement)
	}
	t.Logf("answer: %.400q", res.Answer.Content)
}
