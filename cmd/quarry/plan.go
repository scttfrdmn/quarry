package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	quarry "github.com/scttfrdmn/quarry"
	"github.com/scttfrdmn/quarry/provider"
)

// `quarry plan` — the FOURTH verb, and the first half of §9's approval gate (#15).
//
// §9 asks for "human-in-loop at the plan, before fanout", and names what the gate must
// show: "under P9 the gate shows three things, not one: the split, WHERE THE MONEY
// GOES, and WHAT THE CAP EXCLUDES". §13 recorded why it was not built — it needed "a
// decision about what a declined plan does to a run record, which is a question about
// the artifact rather than about the interface". That is now decided: a decline is a
// valid approvable artifact and runs as a single node (D6).
//
// TWO PHASES RATHER THAN AN INTERACTIVE PROMPT, and the split is what makes the gate
// useful to anything other than a terminal. `plan` writes an artifact; a host — or a
// person — inspects it; `run --plan` executes exactly it. An interactive y/n could not
// be reviewed by a second party, could not be diffed, and could not be cited afterwards.
// Implementing the twins' approval UI is explicitly not quarry's job; emitting the
// object it approves is.
//
// APPROVAL IS NOT EDITING. A host may approve or refuse, never amend: a host-edited
// plan is one the planner never proposed and may have declined, so honouring it would
// spend money on a decomposition no planner endorsed while the record named an approval.
// The content hash enforces this mechanically rather than asking.

// defaultFloorS is the `--floor` default BOTH verbs carry, in one place so they cannot
// drift. If they did, a plan and the run executing it would derive different floors from
// the same absent flag, and Authorizes would refuse over a difference nobody typed.
const defaultFloorS = "0.0002"

// defaultFloor is defaultFloorS parsed, for the summary's "print it only if it differs"
// check. Panics on a malformed constant, which is a build-time fact, not a runtime one.
var defaultFloor = mustFloor()

func mustFloor() quarry.Units {
	u, err := capFlag(defaultFloorS)
	if err != nil {
		panic("malformed defaultFloorS: " + err.Error())
	}
	return u
}

// shellQuote renders a string safe to paste into a POSIX shell.
//
// NEEDED BECAUSE THE STATEMENT IS ARBITRARY TEXT — a research question routinely holds
// spaces, apostrophes ("what does Amazon's egress cost?") and question marks, and an
// unquoted one would reach `run` as several argv entries or, with a glob character, as
// whatever the shell matched. Single-quoted, with each embedded quote closed, backslash-
// escaped and reopened; inside single quotes no character is special, so nothing else
// needs handling. (Do not spell that escape out in this comment: gofmt rewrites a doubled
// ASCII quote into a typographic one, which then fails `gofmt -l`.)
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func planCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	var (
		capS      = fs.String("cap", "1.00", "the spend cap the plan is planned AGAINST (P9); the plan is only valid for it (D1)")
		capMicros = fs.Int64("cap-micros", 0, "the spend cap in integer micro-units — the host path (#11 D1)")
		floorS    = fs.String("floor", defaultFloorS, "smallest allocation worth giving a child (§3); part of what is approved")
		deadline  = fs.Duration("deadline", 0, "latency cap (§3.1)")
		due       = fs.String("due", "", "absolute RFC3339 deadline; the host owns the clock (#11 D2)")
		depth     = fs.Int("depth", 3, "max recursion depth — a BACKSTOP, not the design (P2)")
		fake      = fs.Bool("fake", false, "use the built-in fake planner: no credentials, no money")
		model     = fs.String("model", "us.anthropic.claude-haiku-4-5-20251001-v1:0", "explicit model version, never an alias (P8)")
		region    = fs.String("region", "us-east-1", "AWS region for Bedrock")
		scopeS    = fs.String("scope", "", "scope tags as k=v,k=v; the plan may not be executed under a WIDER scope (D2)")
		out       = fs.String("out", "", "write the plan artifact here (default: quarry-plan-<hash>.json)")
		planCapS  = fs.String("plan-cap", "0.01",
			"the cap on PLANNING ITSELF (D4). one planner call is not free; this is its own budget, "+
				"separate from --cap so planning cannot eat the run's")
		samples = fs.Int("samples", 1,
			"planner samples for the plan-variance diagnostic (§4). k>1 costs k planner calls and "+
				"reports whether the plans agree; the emitted plan is the FIRST sample")
		jsonOnly = fs.Bool("json", false, "print the artifact to stdout and nothing else; no file is written")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: quarry plan [flags] \"<problem statement>\"\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	statement := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if statement == "" {
		fs.Usage()
		return usageErrf("a problem statement is required")
	}
	if *samples < 1 {
		return usageErrf("--samples must be at least 1, got %d", *samples)
	}

	floor, err := capFlag(*floorS)
	if err != nil {
		return err
	}
	if !floor.Limited() {
		floor = 0
	}
	planCap, err := capFlag(*planCapS)
	if err != nil {
		return err
	}

	// The SAME resolution `run` uses, from the same function, so a plan and the run that
	// executes it derive their caps identically. Two resolvers would be two chances for
	// D1's equality check to fail on a difference nobody intended — a plan refused for a
	// cap the user set the same way both times is the gate becoming an obstacle.
	//
	// hostMode is false: the events stream is a `run` concern, and this verb's whole
	// output is a machine-readable artifact regardless.
	cfg, err := resolveRoot(rootInputs{
		capDecimal: *capS,
		capMicros:  *capMicros,
		deadline:   *deadline,
		due:        *due,
		depth:      *depth,
		scope:      *scopeS,
		set:        setFlags(fs),
	}, os.Getenv)
	if err != nil {
		return err
	}
	caps, scope := cfg.Caps, cfg.Scope
	if err := caps.Validate(); err != nil {
		return usageErrf("%w\n  planning is budget-conditioned (P9): a plan made against no budget is "+
			"not a plan, and there would be nothing for D1 to pin it to", err)
	}

	p := quarry.Problem{Statement: statement, Scope: scope}

	// The ledger the plan is apportioned against — the SAME construction `run` performs,
	// and unspent, because planning divides no money. It exists so Apportion sees the
	// balance and reserve the run will actually see.
	l, err := quarry.NewLedger(caps, scope)
	if err != nil {
		return err
	}

	// THE PRE-PLAN BASE CASES, ASKED BEFORE PAYING FOR A PLAN (§2, plan.go PrePlanBase).
	// A run whose root is below the floor, or bounded at depth 0, never calls its planner
	// — so emitting an artifact promising a split would be promising something the run
	// would not do, and the approval would be of a fiction. Shared with the executor
	// rather than reimplemented.
	//
	// Emitted as a DECLINED artifact rather than an error, which is D6 reaching a second
	// case: the host gets something approvable that runs as a single node, and the
	// reasoning says which bound produced it.
	planner, planModel, meter, err := planSeams(ctx, *fake, *model, *region, planCap)
	if err != nil {
		return err
	}

	var plan quarry.Plan
	var variance *quarry.PlanVariance
	if base, done := quarry.PrePlanBase(l, floor, 0, cfg.Depth); done {
		plan = quarry.Plan{Declined: true, Reasoning: fmt.Sprintf(
			"the executor terminates this root before it would call a planner (base case %q), "+
				"so there is no split to approve (§2)", base)}
	} else if *samples > 1 {
		// The §4 diagnostic. k planner calls, and the FIRST sample is the plan emitted —
		// picking the "best" would need a quality measure that does not exist, and picking
		// the modal shape would emit a plan no single planner call actually proposed.
		pv, perr := quarry.SamplePlans(ctx, planner, p, quarry.Allocation{Spend: l.Balance()}, *samples)
		if perr != nil {
			return explain(perr)
		}
		variance = &pv
		// SamplePlans returns moments, not plans, so the emitted plan is a further call.
		// Honest about the cost: --samples k spends k+1 calls, which is what the summary
		// reports and what PlanCost measures.
		plan, err = planOnce(ctx, planner, p, l)
		if err != nil {
			return explain(err)
		}
	} else {
		plan, err = planOnce(ctx, planner, p, l)
		if err != nil {
			return explain(err)
		}
	}

	// Apportionment (D1's other half). A DECLINED plan apportions to nothing, and that is
	// not an error: there are no children, so there is no money to divide, and the whole
	// balance stays at the root that will solve it as one node.
	var allocs []quarry.Allocation
	if !plan.Declined && len(plan.Items) > 0 {
		// The SAME collapse the executor performs before apportioning (§2's DAG rule), so
		// the artifact's fanout is the run's fanout. Skipping it would let the gate show
		// five children and the run apportion three, and D1's check would then refuse the
		// run over a difference the gate itself introduced.
		plan = quarry.DedupePlan(plan)
		allocs, err = l.Apportion(plan, floor)
		if err != nil {
			// The mechanical verifier of §2/P9 firing at the gate rather than mid-run, which
			// is strictly better: nothing has been spent, and the operator learns the split
			// does not fit BEFORE approving it.
			return explain(err)
		}
	}

	// The advisory estimate (D5, P4). Read off the plan that WAS ACTUALLY PRODUCED, via
	// PlanMoments rather than Probe: the plan is already paid for, and Probe would buy a
	// second one and then project a plan other than the one being approved.
	//
	// The per-node cost is the PROVIDER's own estimate for the statement — the same
	// advisory number admission uses (run.go's e.Estimate), keyed on the bare statement
	// and so understating the halo by whatever preamble the solver adds. Advisory in both
	// places and nothing gates on it (P4); the alternative is a second cost model here
	// that could disagree with the one the run will use.
	mean, varc := quarry.PlanMoments(plan)
	est := quarry.Project(mean, varc, cfg.Depth, meter.Estimate(statement, planModel))

	spent, calls := meter.Spent()
	art := quarry.NewPlanArtifact(p, caps, floor, cfg.Depth, plan, allocs, est, spent, planCap, planModel)

	b, err := art.Canonical()
	if err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}

	if *jsonOnly {
		// The CANONICAL bytes on stdout, not a pretty re-encoding: a host that pipes this
		// straight to a file must get a file that hashes to its own PlanID.
		if _, werr := os.Stdout.Write(append(b, '\n')); werr != nil {
			return fmt.Errorf("write plan: %w", werr)
		}
		return nil
	}

	path := *out
	if path == "" {
		path = fmt.Sprintf("quarry-plan-%s.json", art.PlanID[:12])
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}

	summarizePlan(newPrinter(os.Stdout), art, variance, path, calls, *fake)
	return nil
}

// planOnce makes one planner call at the root.
//
// depth 0 and a nil prior, matching Probe and matching what Executor.node passes at the
// root — the plan being approved must be the plan the run would have made, so every
// argument the planner sees has to be the run's.
func planOnce(ctx context.Context, planner quarry.Planner, p quarry.Problem, l *quarry.Ledger) (quarry.Plan, error) {
	// The allocation is the ROOT's balance, as Executor.node computes it. No deadline: a
	// wall-clock instant would make two plans of the same problem differ in a field
	// nothing can reproduce, and the artifact is content-hashed. The Due cap still
	// travels in Caps, which is where a host reads it.
	return planner.Plan(ctx, p, quarry.Allocation{Spend: l.Balance()}, 0, nil)
}

// planSeams wires the PLANNER ONLY, through a meter (D4).
//
// DELIBERATELY NOT wireSeams. That function wires three seams together on the argument
// that they must agree about what backs them — and it is right, but planning uses
// exactly one of them. Wiring a solver and reducer here would build two seams this verb
// cannot reach, and a live one would sit there holding credentials for calls that never
// happen.
//
// Returns the model name for the artifact and the meter for the cost, because D4's
// number must be MEASURED at this seam and there is nowhere else it exists.
func planSeams(ctx context.Context, fake bool, model, region string, planCap quarry.Units) (quarry.Planner, string, *provider.Meter, error) {
	if fake {
		fp := &provider.FakeProvider{Now: time.Now}
		m := provider.NewMeter(fp, planCap)
		// FakePlanner does not call a provider at all, so the meter reports 0 across 0
		// calls — which is the TRUTH and not a hole: --fake spends nothing, and an artifact
		// claiming otherwise would be worse. The meter is still constructed so the summary
		// has one number to print in both modes, and PlannerModel carries the fake marker
		// so Authorizes can refuse executing this plan with real money (plan.go).
		return provider.FakePlanner{}, quarry.FakePlannerModel, m, nil
	}
	prices := planPrices()
	if _, priced := prices[model]; !priced {
		// The same refusal wireSeams makes, and D4 sharpens it: an unpriced model reports
		// every call as free, so PlanCost would state a MEASURED ZERO for a call that cost
		// money — worse than an absent number, because the artifact carries it as a fact.
		return nil, "", nil, usageErrf("no price sheet for model %q\n  an unpriced model reports every "+
			"call as free, and D4 requires planning's cost to be a stated number (§8)", model)
	}
	bp, err := provider.NewBedrockProvider(ctx, region, prices)
	if err != nil {
		return nil, "", nil, fmt.Errorf("build bedrock provider (is AWS_PROFILE set?): %w", err)
	}
	m := provider.NewMeter(bp, planCap)
	return &provider.BedrockPlanner{Provider: m, Model: model, MaxItems: provider.DefaultMaxItems}, model, m, nil
}

// summarizePlan prints the gate: the split, WHERE THE MONEY GOES, and WHAT THE CAP
// EXCLUDES — §9's three things, in that order.
//
// THE EXCLUSIONS ARE NOT A FOOTNOTE. Under P9 the operator's real decision is usually
// "raise the cap or accept the reduced scope", and they can only make it if what the cap
// dropped is stated before spend rather than discovered in the record afterwards.
func summarizePlan(w *printer, art quarry.PlanArtifact, pv *quarry.PlanVariance, path string, calls int, fake bool) {
	w.println()
	if fake {
		w.println("── FAKE PLAN — no model was called. the split is mechanical and means nothing " +
			"about this problem ──")
	}
	w.printf("plan      %s\n", art.PlanID)
	w.printf("problem   %s\n", art.Problem.Statement)
	if len(art.Problem.Scope.Tags) > 0 {
		w.printf("scope     %s\n", scopeStr(art.Problem.Scope))
	}
	// THE CAP IS PRINTED AS PART OF THE PLAN'S IDENTITY, not as a setting, because that
	// is what D1 makes it: this artifact authorises a run under THIS cap and no other.
	w.printf("cap       %s   (the plan is valid ONLY for this cap — P9, D1)\n", art.Caps.Spend)
	if art.Caps.Latency > 0 {
		w.printf("deadline  %s\n", art.Caps.Latency)
	}
	w.printf("depth     %d      floor %s\n", art.Depth, art.Floor)
	w.println()

	if art.Plan.Declined {
		// D6: a decline is an OUTCOME, not a failure, and the summary has to say so plainly
		// or an operator reads "declined" as "quarry could not plan this".
		w.println("the planner DECLINED to decompose — a first-class outcome (P1):")
		w.printf("  %s\n\n", art.Plan.Reasoning)
		w.println("approving this artifact runs the problem as a SINGLE NODE. that is a decision,")
		w.println("not a fallback: decompose only where surface-to-volume favours it.")
	} else {
		strategy := "partition"
		if art.Plan.IsPortfolio() {
			strategy = "portfolio — the children are COMPETING ATTEMPTS at one problem, and the " +
				"reducer will SELECT one rather than combine them (§2)"
		}
		w.printf("strategy  %s\n", strategy)
		if art.Plan.Reasoning != "" {
			w.printf("reasoning %s\n", art.Plan.Reasoning)
		}
		w.printf("\n%s, and where the money goes:\n", plural(len(art.Plan.Items), "child", "children"))
		for i, it := range art.Plan.Items {
			share := quarry.Units(0)
			if i < len(art.Allocations) {
				share = art.Allocations[i].Spend
			}
			leaf := ""
			if it.ExpectLeaf {
				leaf = "  [expect leaf]"
			}
			w.printf("  %d. %s%s\n", i+1, it.Problem.Statement, leaf)
			w.printf("     weight %d → %s\n", it.Weight, share)
		}
	}

	// WHAT THE CAP EXCLUDES — §9's third thing, and the one an operator can act on.
	if len(art.Plan.Excluded) > 0 {
		w.printf("\nthe cap EXCLUDED %s from the split:\n", plural(len(art.Plan.Excluded), "sub-question", "sub-questions"))
		for _, ex := range art.Plan.Excluded {
			w.printf("  - %s\n", ex)
		}
		w.println("  raising --cap and re-planning is what recovers these. approving as-is accepts")
		w.println("  the reduced scope, which is planned degradation inside authority, not a gap (§3.1).")
	}

	// The estimate, WITH its caveat attached, and the caveat is printed every time. A
	// projection beside an approve decision is exactly where P4 gets violated.
	w.printf("\nestimate  ceiling %s   P50 %s   P90 %s   ~%.0f nodes\n",
		art.Estimate.Ceiling, art.Estimate.P50, art.Estimate.P90, art.Estimate.Nodes)
	w.printf("          %s\n", art.EstimateCaveat)

	if pv != nil {
		w.printf("\nplan variance across %d samples: mean m %.2f, stdev %.2f, child counts %v\n",
			pv.K, pv.MeanM, pv.StdevM, pv.ItemCounts)
		if pv.Underspecified {
			w.println("  the plans DISAGREE more than the instability threshold: the problem is")
			w.println("  underspecified, and a single estimate for it is theatre (§4). the emitted plan")
			w.println("  is one sample of a distribution, not the plan.")
		}
	}

	// D4: the number, with its own cap beside it.
	w.printf("\nplanning cost %s of its %s cap, across %s\n",
		art.PlanCost, art.PlanCap, plural(calls, "model call", "model calls"))
	if fake {
		w.println("  (--fake calls no model, so this is genuinely zero rather than unmeasured)")
	}

	w.printf("\nplan written to %s\n", path)
	w.printf("execute exactly this plan with:\n  quarry run --plan %s", path)
	if fake {
		w.printf(" --fake")
	}
	w.printf(" --cap %s --depth %d", art.Caps.Spend, art.Depth)
	if art.Floor != defaultFloor {
		// Only when it differs from what `run` would default to — Authorizes compares the
		// floor, so a non-default one that went unprinted would refuse the pasted command.
		w.printf(" --floor %s", art.Floor)
	}
	if len(art.Problem.Scope.Tags) > 0 {
		// The scope belongs here for the same reason the cap does: Authorizes compares it,
		// and an omitted --scope is not "unspecified" but the EMPTY scope, which is wider
		// than the plan's and refused as a P6 violation.
		w.printf(" --scope %s", shellQuote(scopeStr(art.Problem.Scope)))
	}
	// THE STATEMENT IS PART OF THE COMMAND, and leaving it out made this line a copy-paste
	// that exits 2 — found by running the binary, not by the suite, which called runCmd
	// with hand-built args and so could not notice that the text it prints is unusable.
	// `run --plan` requires it deliberately: Authorizes compares the statement, so the
	// caller restates what it asked for and the artifact confirms it, rather than the file
	// silently supplying both the question and its own authorisation.
	w.printf(" %s\n", shellQuote(art.Problem.Statement))
	w.println("  the cap, depth, floor and scope must MATCH: this artifact does not authorise a run")
	w.println("  under others (D1), and the statement is restated so the artifact can confirm it.")
}

// readPlanArtifact loads an artifact and REFUSES a file that does not hash to its own
// PlanID (#15).
//
// STRICTER THAN readRecord, and the asymmetry is the whole point. A record that fails
// its hash is history and is warned about — readable, not citable. An artifact is an
// AUTHORIZATION: honouring an edited one would spend money on a split nobody approved
// while recording a PlanID that says somebody did, which is worse than refusing to run.
// Approval is not editing (see the file header).
func readPlanArtifact(path string) (quarry.PlanArtifact, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return quarry.PlanArtifact{}, fmt.Errorf("read plan: %w", err)
	}
	// DecodePlanArtifact parses AND verifies in one call, so this function has no way to
	// hand back an unchecked artifact — the check cannot be forgotten here or by any
	// future caller.
	art, err := quarry.DecodePlanArtifact(b)
	if err != nil {
		return quarry.PlanArtifact{}, fmt.Errorf("%s: %w\n  a plan artifact is an AUTHORIZATION, so an "+
			"edited one is REFUSED rather than warned about: approving a plan is not editing it, and "+
			"running an amended split would record an approval nobody gave (#15)", path, err)
	}
	return art, nil
}

// planPrices is the price sheet for the planning call.
//
// THE SAME ENTRIES wireSeams holds, and the duplication is deliberate rather than
// overlooked: sharing one map would let a change made for the run's solver silently
// reprice a plan artifact that has already been approved. TODO(#15): if a third caller
// appears, this becomes a package-level table — two is not yet a pattern.
func planPrices() map[string]provider.Pricing {
	return map[string]provider.Pricing{
		"us.anthropic.claude-haiku-4-5-20251001-v1:0":  {InputPerMTok: 1.0, OutputPerMTok: 5.0},
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": {InputPerMTok: 3.0, OutputPerMTok: 15.0},
		"us.meta.llama3-3-70b-instruct-v1:0":           {InputPerMTok: 0.72, OutputPerMTok: 0.72},
	}
}
