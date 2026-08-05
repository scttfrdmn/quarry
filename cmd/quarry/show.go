package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// `quarry show` and `quarry replay` — the inspect half of §9.
//
// §9 wants "clicking a node shows its prompt, output, verifier result and cost — the
// single affordance that makes the system debuggable rather than a slot machine".
// There is no clicking in a terminal, so `show` is that affordance: the tree from the
// record, and `--node <id>` for one node in full.

func showCmd(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	var (
		node   = fs.String("node", "", "show one node in full: its problem, content, verdict and cost")
		asJSON = fs.Bool("json", false, "print the record as indented JSON (NOT the canonical bytes)")
		claims = fs.Bool("claims", false, "list extracted claims and which node produced each")
		width  = fs.Int("width", 100, "display width")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: quarry show [flags] <record.json>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("exactly one record file is required")
	}
	rec, err := readRecord(fs.Arg(0))
	if err != nil {
		return err
	}

	w := newPrinter(os.Stdout)
	switch {
	case *asJSON:
		// Explicitly not the canonical form — see jsonOut. A reader who needs the hashed
		// bytes already has them: the file itself.
		return jsonOut(w, rec)
	case *node != "":
		return showNode(w, rec, *node)
	case *claims:
		return showClaims(w, rec)
	}
	showRecord(w, rec, *width)
	// A write error is a real failure: `quarry show rec | head -5` closes the pipe, and
	// exiting 0 there would report success on output nobody received.
	return w.Err()
}

// readRecord loads a record and VERIFIES ITS HASH.
//
// The RunID is the content hash with the ID zeroed (P8), so a file whose contents no
// longer hash to its own ID has been edited or corrupted. Checking is cheap and the
// alternative is citing a record that says something its producer did not — which is
// the exact failure the content-addressing exists to prevent. A mismatch is a loud
// warning rather than a refusal: an altered record is still worth reading, it is just
// not citable.
func readRecord(path string) (quarry.RunRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return quarry.RunRecord{}, fmt.Errorf("read record: %w", err)
	}
	var rec quarry.RunRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return quarry.RunRecord{}, fmt.Errorf("parse record %s: %w", path, err)
	}
	if rec.RunID == "" {
		return rec, fmt.Errorf("%s has no RunID; it is not a quarry record", path)
	}
	if want := quarry.RecordHash(rec); want != rec.RunID {
		fmt.Fprintf(os.Stderr,
			"! %s does not hash to its own RunID — it has been edited or re-encoded.\n"+
				"  claims %s, computes %s. readable, but NOT citable (P8).\n\n",
			path, rec.RunID[:12], want[:12])
	}
	return rec, nil
}

// showRecord prints the receipt and the tree, from the record alone.
//
// SEPARATE CODE FROM THE LIVE RENDERER, deliberately. The live view reads Observer
// events and this reads a record; making one render both would mean the display could
// not distinguish an in-flight number from a final one, which is precisely the
// distinction the Observer doc insists on. They agree because both read the same
// NodeOutcome fields, not because they share a code path.
func showRecord(w *printer, rec quarry.RunRecord, width int) {
	prov := quarry.ProvenanceOf(rec, nil)

	w.printf("record    %s\n", rec.RunID)
	w.printf("problem   %s\n", rec.Problem.Statement)
	if len(rec.Problem.Scope.Tags) > 0 {
		w.printf("scope     %s\n", scopeStr(rec.Problem.Scope))
	}
	w.printf("mode      %s", rec.Mode)
	if rec.ParentRun != "" {
		w.printf("  (from %s)", rec.ParentRun[:12])
	}
	w.println()
	w.printf("cost      %s of %s\n", rec.TotalCost(), rec.Caps.Spend)
	if cpc, ok := rec.CostPerVerifiedClaim(); ok {
		w.printf("          %s per verified claim\n", cpc)
	}
	w.printf("verified  %d of %d nodes\n", prov.Verified, len(rec.Outcomes))

	// What was NOT checked, named explicitly (§8). This is the field a bare cost
	// receipt cannot express and the reason the record exists in this shape.
	if len(rec.Unverified) > 0 {
		w.printf("unchecked %s: %s\n", plural(len(rec.Unverified), "node", "nodes"),
			strings.Join(rec.Unverified, " "))
	} else {
		w.println("unchecked none — every node was assessed")
	}
	if prov.StabilityKnown {
		w.printf("stability %.0f%%\n", prov.Stability*100)
	} else {
		w.println("stability not measured — needs replicates (P7)")
	}
	if prov.AdversarialFindings > 0 {
		w.printf("broken    %s refuted by an adversary (§5)\n",
			plural(prov.AdversarialFindings, "claim", "claims"))
	}
	if rec.BoundBy != "" {
		w.printf("bound by  %s\n", rec.BoundBy)
	}
	if rec.Truncated() {
		// Broader than Gaps and it has to be: under the standing ruling only TIME is a
		// gap, so a run that hit its spend cap has no gaps while being truncated.
		w.println("truncated yes — this run stopped short of what it set out to do (§8.1)")
	}

	w.println("\ntree")
	renderRecordTree(w, rec, width)

	w.printf("\ninspect a node:  quarry show --node <id> <record>\n")
}

// renderRecordTree draws the tree from the outcomes' parentage.
//
// Parentage comes from the Children lists, not from parsing node IDs — the same
// discipline the live seam follows. The ID encoding is quarry's business.
func renderRecordTree(w *printer, rec quarry.RunRecord, width int) {
	byID := make(map[string]quarry.NodeOutcome, len(rec.Outcomes))
	hasParent := map[string]bool{}
	for _, o := range rec.Outcomes {
		byID[o.NodeID] = o
		for _, c := range o.Children {
			hasParent[c] = true
		}
	}
	// Outcomes are pre-order, so the first parentless node is the root. Scanning for it
	// rather than assuming "n0" keeps this working for a record whose IDs came from
	// somewhere else.
	var roots []string
	for _, o := range rec.Outcomes {
		if !hasParent[o.NodeID] {
			roots = append(roots, o.NodeID)
		}
	}
	for i, r := range roots {
		drawRecordNode(w, byID, r, "", i == len(roots)-1, true, width)
	}
}

func drawRecordNode(w *printer, byID map[string]quarry.NodeOutcome, id, prefix string, last, isRoot bool, width int) {
	o, ok := byID[id]
	if !ok {
		return
	}
	branch, childPrefix := "", prefix
	if !isRoot {
		if last {
			branch, childPrefix = "└─ ", prefix+"   "
		} else {
			branch, childPrefix = "├─ ", prefix+"│  "
		}
	}
	indent := len([]rune(prefix)) + len([]rune(branch))
	left := fmt.Sprintf("%s %s %s", recordGlyph(o), o.NodeID, o.Problem.Statement)
	right := o.Cost.String()
	if v := recordVerdict(o); v != "" {
		right += "  " + v
	}
	w.printf("%s%s%s\n", prefix, branch, fitLine(left, right, width-indent))

	// Children in RECORDED order. The record's Children slice is the plan's order, so
	// this needs no sort — unlike the live view, where arrival order is a race.
	for i, c := range o.Children {
		drawRecordNode(w, byID, c, childPrefix, i == len(o.Children)-1, false, width)
	}
}

// recordGlyph mirrors the live renderer's distinctions exactly. They are separate
// code and must not drift: an unchecked node is not a pass (§8) and a gap is not a
// failure (§3.1).
func recordGlyph(o quarry.NodeOutcome) string {
	switch {
	case o.Gap:
		return "⊘"
	case o.CacheHit:
		return "⇢"
	case o.Verified == nil:
		return "○"
	case *o.Verified:
		return "✓"
	default:
		return "✗"
	}
}

func recordVerdict(o quarry.NodeOutcome) string {
	switch {
	case o.Gap:
		return "gap (time)"
	case o.CacheHit:
		return "cached"
	case o.Verified == nil:
		return "unverified"
	case *o.Verified:
		return "verified"
	default:
		return "FAILED"
	}
}

// showNode is the inspect affordance: one node in full.
func showNode(w *printer, rec quarry.RunRecord, id string) error {
	var o quarry.NodeOutcome
	var found bool
	for _, c := range rec.Outcomes {
		if c.NodeID == id {
			o, found = c, true
			break
		}
	}
	if !found {
		ids := make([]string, 0, len(rec.Outcomes))
		for _, c := range rec.Outcomes {
			ids = append(ids, c.NodeID)
		}
		sort.Strings(ids)
		return fmt.Errorf("no node %q in this record. it has: %s", id, strings.Join(ids, " "))
	}

	w.printf("node      %s  (depth %d)\n", o.NodeID, o.Depth)
	w.printf("problem   %s\n", o.Problem.Statement)
	if len(o.Problem.Scope.Tags) > 0 {
		w.printf("scope     %s\n", scopeStr(o.Problem.Scope))
	}
	w.printf("cost      %s", o.Cost)
	if o.CacheHit {
		// The distinction §6 draws: the tokens were really spent once, and this run did
		// not pay for them. "Free" and "not charged to this run" are different claims.
		w.printf("  (served from cache — this run paid nothing; the tokens were spent once)")
	}
	w.println()
	if o.Model != "" {
		w.printf("model     %s  (version %s)\n", o.Model, o.ModelVersion)
	} else if len(o.Children) > 0 {
		w.println("model     none — this node reduced its children rather than solving")
	}
	if r, ok := o.SurfaceToVolume(); ok {
		// P1 made observable: a high ratio means the node paid for its parent's context
		// and did little with it — evidence the split was not worth making.
		w.printf("tokens    %d halo / %d generated   surface-to-volume %.2f\n",
			o.HaloTokens, o.GeneratedTokens, r)
	}
	if d, ok := o.Timing.Duration(); ok {
		// Timing is NOT in the hashed record (P8), so a loaded record has none. Saying so
		// beats printing a zero.
		w.printf("elapsed   %s\n", durStr(d, ok))
	} else {
		w.println("elapsed   not in the record — timing is deliberately unhashed (P8)")
	}
	w.printf("verdict   %s\n", recordVerdict(o))
	if o.Retries > 0 {
		w.printf("retries   %d (each one paid for; §3 Budget(Retry(agent)))\n", o.Retries)
	}
	if o.BaseCase != "" {
		w.printf("stopped   %s\n", baseCaseWhy(o.BaseCase))
	}
	if o.Strategy != "" {
		w.printf("strategy  %s\n", o.Strategy)
	}
	if o.PlanWeight > 0 {
		w.printf("weight    %d (relative; the parent's plan funded it by this share)\n", o.PlanWeight)
	}
	if len(o.Children) > 0 {
		w.printf("children  %s\n", strings.Join(o.Children, " "))
	}
	w.printf("\ncontent\n%s\n", indent(o.Content, "  "))
	if len(o.Claims) > 0 {
		w.printf("\nclaims (%d)\n", len(o.Claims))
		for _, c := range o.Claims {
			w.printf("  · %s\n", c.Text)
		}
	}
	return w.Err()
}

// baseCaseWhy expands a base case into the reason it encodes. The enum value alone
// says what happened; §2 cares about why, and P2 in particular.
func baseCaseWhy(b quarry.BaseCase) string {
	switch b {
	case quarry.BaseNoVerifier:
		return "no verifier was available for its children, so it did not recurse (P2 — the PRIMARY terminator)"
	case quarry.BasePlannerDeclined:
		return "the planner declined to split; surface-to-volume did not favour it (P1)"
	case quarry.BaseBelowFloor:
		return "the balance could not fund a child above the floor (§3)"
	case quarry.BaseMaxDepth:
		return "max depth — the BACKSTOP, not the design (P2). a run bounded by this is under-verified"
	default:
		return string(b)
	}
}

func showClaims(w *printer, rec quarry.RunRecord) error {
	all := rec.AllClaims()
	if len(all) == 0 {
		w.println("no claims were extracted from this record")
		return w.Err()
	}
	w.printf("%d claims\n\n", len(all))
	for _, c := range all {
		stable := "not assessed"
		if c.Stable != nil {
			if *c.Stable {
				stable = "stable"
			} else {
				stable = "UNSTABLE"
			}
		}
		w.printf("  [%s] %s\n      %s\n", c.NodeID, c.Text, stable)
	}
	w.println("\nstability needs replicates: one run is one sample (P7)")
	return w.Err()
}

func scopeStr(s quarry.Scope) string {
	keys := make([]string, 0, len(s.Tags))
	for k := range s.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+s.Tags[k])
	}
	return strings.Join(parts, ",")
}

// fitLine right-aligns at width, rune-aware. Same job as the renderer's fit; kept
// local because show and tui are deliberately separate code (see showRecord).
func fitLine(left, right string, width int) string {
	lr, rr := []rune(left), []rune(right)
	maxLeft := width - len(rr) - 2
	if maxLeft < 8 {
		maxLeft = 8
	}
	if len(lr) > maxLeft {
		lr = append(lr[:maxLeft-1], '…')
	}
	pad := width - len(lr) - len(rr)
	if pad < 2 {
		pad = 2
	}
	return string(lr) + strings.Repeat(" ", pad) + right
}

// ------------------------------------------------------------------------- replay

// replayCmd re-executes a record against its own recorded responses and compares the
// canonical bytes (§7, P8).
//
// THIS IS THE DETERMINISM GUARANTEE, RUNNABLE. It is why Units is integral (largest-
// remainder apportionment replays bit-for-bit) and why nothing in the core reads the
// clock. A record that does not replay is not an artifact, so this needs to be a
// command anyone can run against any record, not only a test over a fixture.
//
// It substitutes THREE seams, because three things in a run are stochastic: the plan,
// each leaf answer, and each reduction. Replaying only the provider against a live
// planner would issue real plan calls during "replay" — spending money and producing a
// different tree.
func replayCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	var (
		out     = fs.String("out", "", "write the replayed record here (default: not written)")
		verbose = fs.Bool("v", false, "print the replayed tree")
		width   = fs.Int("width", 100, "display width")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: quarry replay [flags] <record.json>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("exactly one record file is required")
	}
	orig, err := readRecord(fs.Arg(0))
	if err != nil {
		return err
	}

	// The model to hand ProviderSolver: whatever the record says produced its leaves.
	// Guessing would make replay miss every sample, and the resulting "divergence" would
	// be an artifact of the CLI rather than of the run.
	model := recordedModel(orig)
	if err := replayableRecord(orig, model); err != nil {
		return err
	}
	// A RECORD THAT CALLED NO MODEL AT ALL IS STILL REPLAYABLE, and refusing it was the
	// same defect twice: treating a partial run as a broken record rather than a faithful
	// one. `quarry run --deadline 60ms` produces a record where every node gapped — the
	// most time-bound run the system can make, and the one most worth interrogating — and
	// this said "there is nothing to replay". `--cap 0.000001` produces the spend
	// counterpart, a single below-floor root, and it said the same thing.
	//
	// It is replayable either way: gaps and unfunded nodes are both indexed WITHOUT a model
	// (record.go), because neither ever reached a provider, so an empty model here misses
	// nothing. The solver is wired with a placeholder purely to name the seam; if it is
	// actually consulted the lookup misses and replay reports a divergence, which is the
	// correct outcome — that would mean the replay asked for an answer the record does not
	// contain.
	if model == "" {
		model = "(no model — no node reached one)"
	}

	e := replayExecutor(orig, model)

	l, err := quarry.NewLedger(orig.Caps, orig.Problem.Scope)
	if err != nil {
		return err
	}
	res, err := e.Run(ctx, orig.Problem, l)
	if err != nil {
		return fmt.Errorf("replay failed: %w\n  a replay that cannot re-execute has diverged; "+
			"the record names what it expected", explain(err))
	}
	// ReplayRecord, not NewRunRecord: it inherits the fields a replay cannot re-derive
	// (BoundBy above all — see record.go) while re-deriving the tree, which is the only
	// thing this command is actually testing.
	replayed := quarry.ReplayRecord(res, orig)

	ob, err := orig.Canonical()
	if err != nil {
		return err
	}
	rb, err := replayed.Canonical()
	if err != nil {
		return err
	}

	w := newPrinter(os.Stdout)
	if *verbose {
		w.println("replayed tree")
		renderRecordTree(w, replayed, *width)
		w.println()
	}
	if *out != "" {
		if err := writeRecord(*out, replayed); err != nil {
			return err
		}
	}

	if string(ob) == string(rb) {
		w.printf("✓ replay is byte-identical\n  %s\n  %s spent, %d nodes, no model was called\n",
			replayed.RunID, replayed.TotalCost(), len(replayed.Outcomes))
		return w.Err()
	}
	// A divergence is a real finding about the record, so report where rather than just
	// that. Non-zero exit: this is a failure, and a script must be able to see it.
	fmt.Fprintf(os.Stderr, "✗ REPLAY DIVERGED\n  recorded %s\n  replayed %s\n\n%s\n",
		orig.RunID, replayed.RunID, diffRecords(orig, replayed))
	os.Exit(1)
	return nil
}

// replayableRecord rejects the ONE record that genuinely cannot be replayed: every node
// served from cache, so nothing was ever computed to reproduce.
//
// THE BAR IS DELIBERATELY LOW, because setting it higher was a defect twice. A record
// with no leaf model is not unreplayable — a gapped node and an unfunded node each
// reached no model, and both replay exactly (record.go indexes them without one). This
// check first excused gaps only, so `--deadline 60ms` was refused; then it excused gaps
// and forgot spend, so `--cap 0.000001` — a single below-floor root — was refused too.
// Both are faithful records of runs the caps cut short, and §3.1 makes those the normal
// outcome, not the edge case.
//
// A function rather than an inline check so the bar itself is testable, which is what it
// took to notice that "no model" and "nothing happened" are different claims.
func replayableRecord(r quarry.RunRecord, model string) error {
	if model != "" || len(r.Gaps()) > 0 || len(r.Unfunded()) > 0 {
		return nil
	}
	return fmt.Errorf("this record names no leaf model, no gaps and no unfunded nodes; " +
		"there is nothing to replay\n  (every node was served from cache, so no model call " +
		"was ever made)")
}

// replayExecutor builds the executor a replay re-executes the tree with: three recorded
// seams (§7) plus every executor knob the original run was configured with.
//
// A FUNCTION RATHER THAN AN INLINE LITERAL because that is where a defect hid. The floor
// was omitted here, and nothing in the package could assert on it — the wiring was
// unreachable from a test, so it was checked only end-to-end, by running the binary and
// reading a hash. Extracting it makes the one thing that actually went wrong — which
// fields get carried across — checkable directly.
func replayExecutor(orig quarry.RunRecord, model string) *quarry.Executor {
	seams := quarry.Replayable(orig)
	return &quarry.Executor{
		Planner: seams.Planner,
		// ProviderSolver, DELIBERATELY, even though the run used BudgetedSolver. Not an
		// oversight and not a mismatch: RecordedProvider indexes on the recorded Problem
		// and looks up on the prompt it is handed, so the solver a replay needs is the one
		// that passes the BARE STATEMENT. BudgetedSolver here would send a wrapped prompt,
		// miss every leaf, and report "replay diverged" against a faithful record.
		//
		// The corollary is the reason prompt construction lives in the Solver rather than
		// in the Provider: the budgeted prompt never enters the record, so the recorded key
		// stays stable no matter how the prompt is later reworded. Asserted in
		// provider/replay_budgeted_test.go, both directions.
		Solver:  quarry.ProviderSolver{Provider: seams.Provider, Model: model},
		Reducer: seams.Reducer,
		// Now is a FIXED instant and the Clock is deliberately nil. A replay that timed
		// itself would still produce identical bytes — timing is unhashed — but leaving it
		// unmeasured is the honest reading: these durations are not the original's.
		Now:      time.Time{},
		MaxDepth: maxDepthOf(orig),
		// Floor, for the same reason as MaxDepth and found the same way: it is an EXECUTOR
		// PARAMETER that shapes the tree, so a replay that leaves it zero re-executes under
		// a different rule. A node whose apportionable balance fell below the floor stopped
		// with BaseBelowFloor; with no floor, zero is never below it, so the replay planned
		// instead and recorded BasePlannerDeclined.
		//
		// The THIRD instance of one shape (BoundBy, the depth bound, this), which is why the
		// design doc now states it as a rule rather than three bug reports: a fact of the
		// original EXECUTION cannot be re-derived from the tree, and every knob the executor
		// was configured with is such a fact.
		Floor:      orig.Bounds.Floor,
		MaxRetries: orig.Bounds.MaxRetries,
		// The verifier RE-RUNS, and must: the record stores the verdict but replay has to
		// re-derive it, or a replay would prove only that the file can be read back.
		Verifier:  quarry.NonEmptyOracle(),
		Extractor: quarry.MechanicalExtractor{},
		// NO CACHE. A cache hit during replay would substitute a served answer for a
		// recorded one and the comparison would be against the wrong thing.
	}
}

// recordedModel returns the model that produced this record's leaves. Takes the first
// non-empty one: a record with several is a legitimate case (§5's ladder routes
// different nodes to different models) but replay keys on (problem, scope, model), so
// a mixed record needs per-node models — a real limitation, named here rather than
// silently producing a divergence.
func recordedModel(r quarry.RunRecord) string {
	models := map[string]bool{}
	first := ""
	for _, o := range r.Outcomes {
		if o.Model == "" {
			continue
		}
		if first == "" {
			first = o.Model
		}
		models[o.Model] = true
	}
	if len(models) > 1 {
		fmt.Fprintf(os.Stderr, "! this record names %d different leaf models; replaying against %q.\n"+
			"  a mixed-model record needs per-node replay, which is not built (see cmd/quarry/show.go).\n\n",
			len(models), first)
	}
	return first
}

// maxDepthOf recovers the depth bound so replay does not truncate a tree the original
// was allowed to grow — from RunBounds when the record carries it, by inference otherwise.
//
// FOUND BY THE FIRST LIVE BEDROCK RUN, where `--depth 2` produced 22 leaves all recording
// BaseMaxDepth: inferring the bound as deepest+1 handed replay a limit of 3, so those
// nodes were no longer at the bound, called the pinned planner, and came back
// BasePlannerDeclined. Twenty-two BaseCase fields differed and replay reported a
// divergence against a faithful record.
//
// It was the second instance of one shape — after ReplayRecord's BoundBy — and the floor
// was the third, which is what moved the answer into the record itself (RunBounds). The
// rule: a fact of the original EXECUTION cannot be re-derived from the tree's geometry.
// deepest+1 is a lower bound on the cap, not the cap; the two coincide only when nothing
// hit it, which is why every `--fake` record replayed clean — the fake planner declines on
// clause length long before depth, so no `--fake` record has a max_depth leaf at all.
//
// THE INFERENCE REMAINS, for records written before RunBounds existed. Both branches are
// exercised: a max_depth node names the bound exactly, and failing that, any limit at
// least as deep as the tree reproduces the same shape because nothing reached it.
func maxDepthOf(r quarry.RunRecord) int {
	if r.Bounds.MaxDepth > 0 {
		return r.Bounds.MaxDepth
	}
	max := 0
	for _, o := range r.Outcomes {
		if o.BaseCase == quarry.BaseMaxDepth {
			// Depth is compared with >=, so a node that stopped AT depth d means the cap is d.
			return o.Depth
		}
		if o.Depth > max {
			max = o.Depth
		}
	}
	return max + 1
}

// diffRecords names the first substantive disagreement. Field-level rather than a
// byte diff, because "byte 4192 differs" does not tell anyone which guarantee broke.
func diffRecords(a, b quarry.RunRecord) string {
	var out []string
	if len(a.Outcomes) != len(b.Outcomes) {
		out = append(out, fmt.Sprintf("  node count: %d recorded, %d replayed — the SHAPE diverged, "+
			"which points at the pinned plan (§7)", len(a.Outcomes), len(b.Outcomes)))
	}
	if a.TotalCost() != b.TotalCost() {
		out = append(out, fmt.Sprintf("  total cost: %s recorded, %s replayed — apportionment diverged, "+
			"which should be impossible with integral Units (P8)", a.TotalCost(), b.TotalCost()))
	}
	byID := map[string]quarry.NodeOutcome{}
	for _, o := range a.Outcomes {
		byID[o.NodeID] = o
	}
	for _, o := range b.Outcomes {
		orig, ok := byID[o.NodeID]
		if !ok {
			out = append(out, fmt.Sprintf("  node %s: replayed but not recorded", o.NodeID))
			continue
		}
		if orig.Content != o.Content {
			out = append(out, fmt.Sprintf("  node %s: content differs\n    recorded %.60q\n    replayed %.60q",
				o.NodeID, orig.Content, o.Content))
		}
		if orig.Cost != o.Cost {
			out = append(out, fmt.Sprintf("  node %s: cost %s vs %s", o.NodeID, orig.Cost, o.Cost))
		}
		// EVERY OTHER HASHED FIELD, because checking only content and cost is what made this
		// function report "no field-level difference was found" on a record with TWENTY-TWO
		// differing BaseCase fields (the maxDepthOf defect above). That fallback message
		// blames the ENCODER — "likely a field ordering or encoding change, which is itself
		// a P8 break" — so a diff this function merely failed to look for was reported as a
		// deeper defect elsewhere. A divergence reporter that cannot name the divergence
		// sends the reader to the wrong file.
		//
		// The list is every field Canonical() hashes. Timing is excluded because it is
		// `json:"-"` and deliberately unhashed (P8).
		//
		// CLAIMS ARE NOT COVERED BY CONTENT, which is what this comment used to claim, and
		// the assumption made this function fall through a THIRD time — found by scripting a
		// tamper demo, where editing a node's content is the whole point. Content is REPLAYED
		// from the record while claims are RE-EXTRACTED from it, so an edited record diverges
		// in claims *only*: the replay faithfully returns the tampered content, then extracts
		// what that content actually says, which is not what the record's Claims field says.
		// That is precisely the case a citable artifact must be able to name.
		if len(orig.Claims) != len(o.Claims) {
			out = append(out, fmt.Sprintf("  node %s: %d claims recorded, %d re-extracted — content and "+
				"claims disagree, which is what an EDITED record looks like (§8, P8)",
				o.NodeID, len(orig.Claims), len(o.Claims)))
		} else {
			for i := range orig.Claims {
				if orig.Claims[i].Norm != o.Claims[i].Norm {
					out = append(out, fmt.Sprintf("  node %s: claim %d differs\n    recorded %.60q\n"+
						"    re-extracted %.60q\n    (claims are re-derived from content, so this is "+
						"the shape an edited record takes)", o.NodeID, i, orig.Claims[i].Text, o.Claims[i].Text))
					break
				}
			}
		}
		if orig.BaseCase != o.BaseCase {
			out = append(out, fmt.Sprintf("  node %s: base case %q recorded, %q replayed — the node "+
				"stopped recursing for a DIFFERENT reason, which points at the depth bound or the "+
				"pinned plan (§2, §7)", o.NodeID, orig.BaseCase, o.BaseCase))
		}
		if orig.Gap != o.Gap {
			out = append(out, fmt.Sprintf("  node %s: gap %v recorded, %v replayed — time truncation "+
				"and spend degradation are being confused (§3.1)", o.NodeID, orig.Gap, o.Gap))
		}
		if !sameVerdict(orig.Verified, o.Verified) {
			out = append(out, fmt.Sprintf("  node %s: verdict %s recorded, %s replayed — the verifier "+
				"re-ran and disagreed (§5)", o.NodeID, verdictStr(orig.Verified), verdictStr(o.Verified)))
		}
		if orig.Model != o.Model || orig.ModelVersion != o.ModelVersion {
			out = append(out, fmt.Sprintf("  node %s: model %q/%q recorded, %q/%q replayed (P8)",
				o.NodeID, orig.Model, orig.ModelVersion, o.Model, o.ModelVersion))
		}
		if orig.Depth != o.Depth || orig.CacheHit != o.CacheHit || orig.Retries != o.Retries ||
			orig.Strategy != o.Strategy || orig.PlanWeight != o.PlanWeight ||
			orig.HaloTokens != o.HaloTokens || orig.GeneratedTokens != o.GeneratedTokens ||
			len(orig.Children) != len(o.Children) {
			out = append(out, fmt.Sprintf("  node %s: differs in depth/cache/retries/strategy/weight/"+
				"tokens/children — recorded (%d,%v,%d,%q,%d,%d/%d,%d children), replayed "+
				"(%d,%v,%d,%q,%d,%d/%d,%d children)", o.NodeID,
				orig.Depth, orig.CacheHit, orig.Retries, orig.Strategy, orig.PlanWeight,
				orig.HaloTokens, orig.GeneratedTokens, len(orig.Children),
				o.Depth, o.CacheHit, o.Retries, o.Strategy, o.PlanWeight,
				o.HaloTokens, o.GeneratedTokens, len(o.Children)))
		}
		if len(out) > 8 {
			out = append(out, "  … (further differences suppressed)")
			break
		}
	}
	// Record-level fields last: a node difference is the more useful finding, but a
	// record whose nodes all agree can still diverge in its own fields, and reaching the
	// "no difference found" fallback with one of these unchecked would blame the encoder.
	if a.BoundBy != b.BoundBy {
		out = append(out, fmt.Sprintf("  bound by: %q recorded, %q replayed — this is inherited, "+
			"not re-derived, so a difference means ReplayRecord was bypassed (§7)", a.BoundBy, b.BoundBy))
	}
	if a.Mode != b.Mode {
		out = append(out, fmt.Sprintf("  mode: %q recorded, %q replayed", a.Mode, b.Mode))
	}
	if len(a.Unverified) != len(b.Unverified) {
		out = append(out, fmt.Sprintf("  unverified: %d recorded, %d replayed — the list of what was "+
			"NOT checked is part of the deliverable (§8)", len(a.Unverified), len(b.Unverified)))
	}
	if len(out) == 0 {
		return "  the canonical bytes differ but no field-level difference was found —\n" +
			"  likely a field ordering or encoding change, which is itself a P8 break"
	}
	return strings.Join(out, "\n")
}

// sameVerdict compares two three-state verdicts. A *bool, not a bool: nil means NOT
// CHECKED, which is distinct from checked-and-failed (§8), so a nil-vs-false difference
// is a real divergence and == on the pointers would miss it entirely.
func sameVerdict(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func verdictStr(v *bool) string {
	switch {
	case v == nil:
		return "unchecked"
	case *v:
		return "verified"
	default:
		return "failed"
	}
}
