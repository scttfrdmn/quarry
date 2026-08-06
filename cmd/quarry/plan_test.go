package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	quarry "github.com/scttfrdmn/quarry"
)

// The two-phase approval gate as a HOST SEES IT (#15). plan_test.go in the root package
// pins the artifact's own guarantees; these pin the pair of commands, because four of the
// five defects in the first live round were invisible to a suite that tested the types
// alone.
//
// A WIRED SEAM THE CLI DOES NOT WIRE is one of the ways these tests have been vacuous
// before, so every case here goes through planCmd and runCmd rather than assembling an
// artifact and an Executor by hand.

const gateQuestion = "What does storage cost, how does it scale, and what dominates the bill?"

// planFile runs `quarry plan` and returns the written artifact's path.
func planFile(t *testing.T, dir string, extra ...string) string {
	t.Helper()
	path := filepath.Join(dir, "plan.json")
	args := append([]string{"--fake", "--cap", "1.00", "--depth", "2", "--out", path}, extra...)
	args = append(args, gateQuestion)
	if err := planCmd(context.Background(), args); err != nil {
		t.Fatalf("quarry plan: %v", err)
	}
	return path
}

// TWO PHASES, END TO END: the artifact `plan` writes is the one `run` executes, and the
// record names it (D3).
func TestTheTwoPhaseGateRunsTheApprovedPlanAndTheRecordNamesIt(t *testing.T) {
	dir := t.TempDir()
	planPath := planFile(t, dir)

	art, err := readPlanArtifact(planPath)
	if err != nil {
		t.Fatalf("the artifact quarry plan wrote must verify: %v", err)
	}

	recPath := filepath.Join(dir, "r.json")
	if err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--out", recPath, gateQuestion,
	}); err != nil {
		t.Fatalf("quarry run --plan: %v", err)
	}

	rec, err := readRecord(recPath)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PlanID != art.PlanID {
		t.Fatalf("the record must name the plan it was authorised to run (D3): %q != %q",
			rec.PlanID, art.PlanID)
	}
	// The record must still hash to its own RunID with the approval on it, or the citable
	// artifact arrives already flagged as edited.
	if quarry.RecordHash(rec) != rec.RunID {
		t.Fatal("a gated record must hash to its own RunID (P8)")
	}
	// THE SHAPE MUST BE THE APPROVED SHAPE. A run that named the plan while executing a
	// different tree is the exact failure the gate exists to prevent, and it would be
	// invisible in the PlanID alone.
	wantNodes := len(art.Plan.Items) + 1 // the children plus the root
	if len(rec.Outcomes) != wantNodes {
		t.Fatalf("the approved plan has %d children, so the run has %d nodes; got %d",
			len(art.Plan.Items), wantNodes, len(rec.Outcomes))
	}
	for i, it := range art.Plan.Items {
		// The children are the approved statements, in the approved order.
		if got := rec.Outcomes[i+1].Problem.Statement; got != it.Problem.Statement {
			t.Fatalf("child %d: ran %q, approved %q", i, got, it.Problem.Statement)
		}
	}
}

// P8 through the gate: a two-phase run must replay byte-identically, which is what makes
// the approval citable rather than merely recorded.
func TestAGatedRunReplaysByteIdentically(t *testing.T) {
	dir := t.TempDir()
	planPath := planFile(t, dir)
	recPath := filepath.Join(dir, "r.json")
	if err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--out", recPath, gateQuestion,
	}); err != nil {
		t.Fatal(err)
	}
	orig, err := readRecord(recPath)
	if err != nil {
		t.Fatal(err)
	}
	if orig.PlanID == "" {
		t.Fatal("fixture must be a GATED record, or this test does not exercise the gate at all")
	}

	// replayCmd is the CLI's own verb: replaying through it rather than through
	// quarry.Replayable is the point, since a seam the CLI does not wire is a seam these
	// tests cannot see. It replays with NO --plan flag — the record must carry everything
	// the re-execution needs, which is the whole of P8.
	//
	// WHERE THE FAILURE ACTUALLY SURFACES: replayCmd reports a divergence by printing the
	// field-level diff and calling os.Exit(1), so a broken gate kills the test binary with
	// that diff rather than reaching the assertions below. Verified by deleting PlanID from
	// ReplayRecord's inherited column, which produced `plan: "d2cf54c4..." recorded, ""
	// replayed`. The assertions below therefore only run on the success path; they are kept
	// because they say what the diff must be about, and because an os.Exit is a mechanism
	// this test does not own and should not depend on.
	replayedPath := filepath.Join(dir, "replayed.json")
	if err := replayCmd(context.Background(), []string{"--out", replayedPath, recPath}); err != nil {
		t.Fatalf("a gated run must replay: %v", err)
	}
	replayed, err := readRecord(replayedPath)
	if err != nil {
		t.Fatal(err)
	}
	// The approval is a FACT OF THE ORIGINAL EXECUTION, so a replay inherits it rather than
	// re-deriving it — the fourth instance of the lesson BoundBy taught. Nothing in a replay
	// reads a plan file, so a PlanID that did not survive into ReplayRecord's inherited
	// column would silently drop the approval and change the RunID.
	if replayed.PlanID != orig.PlanID {
		t.Errorf("a replay must INHERIT the approval it executed under: recorded %q, replayed %q",
			orig.PlanID, replayed.PlanID)
	}
	ob, err := orig.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := replayed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(ob) != string(rb) {
		t.Errorf("a gated run must replay BYTE-IDENTICALLY, not merely equal in RunID (P8)\n%s",
			diffRecords(orig, replayed))
	}
}

// D1 as a HOST experiences it: the refusal is a usage error, so a host reads "you can fix
// this" rather than "quarry malfunctioned".
func TestRunRefusesAnApprovedPlanUnderADifferentCap(t *testing.T) {
	dir := t.TempDir()
	planPath := planFile(t, dir)

	err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "0.50", "--depth", "2",
		"--out", filepath.Join(dir, "r.json"), gateQuestion,
	})
	if !errors.Is(err, quarry.ErrPlanNotAuthorized) {
		t.Fatalf("a different cap must be refused (D1, P9), got %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Fatalf("a cap mismatch is fixable by the caller, so it must exit %d, got %d", exitUsage, got)
	}
	// NOTHING MAY HAVE RUN. A refusal that spent money first, or wrote a record, would be a
	// gate that gates nothing — and this is the assertion that catches the check being
	// placed after the run rather than before it.
	if _, statErr := os.Stat(filepath.Join(dir, "r.json")); statErr == nil {
		t.Fatal("a refused run must not write a record: the refusal is about authority, " +
			"and it must happen before anything is spent")
	}
	// The message must send the operator to the right flag.
	if !strings.Contains(err.Error(), "budget-conditioned") {
		t.Fatalf("the refusal must explain WHY the cap matters (P9), got %q", err.Error())
	}
}

// D2 as a host experiences it.
func TestRunRefusesAnApprovedPlanUnderAWiderScope(t *testing.T) {
	dir := t.TempDir()
	planPath := planFile(t, dir, "--scope", "team=a,project=x")

	err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--scope", "team=a", "--out", filepath.Join(dir, "r.json"), gateQuestion,
	})
	if !errors.Is(err, quarry.ErrScopeWidens) {
		t.Fatalf("a widened scope must be refused (D2, P6), got %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Fatalf("want exit %d, got %d", exitUsage, got)
	}
	// The SAME scope is accepted, which is what proves the refusal came from the widening
	// rather than from carrying a scope at all.
	if err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--scope", "team=a,project=x", "--out", filepath.Join(dir, "ok.json"), gateQuestion,
	}); err != nil {
		t.Fatalf("the scope it was planned under must be accepted: %v", err)
	}
}

// THE TAMPER CASE at the CLI, and the asymmetry with readRecord is the point: a record
// that fails its hash is warned about, an artifact is REFUSED. Honouring an edited plan
// would spend on a split nobody approved while recording an approval nobody gave.
func TestRunRefusesAnEditedPlanArtifact(t *testing.T) {
	dir := t.TempDir()
	planPath := planFile(t, dir)

	// Edit through JSON, the way a host or a person actually would, leaving the PlanID
	// alone — which is what makes this an approval forgery rather than a corrupt file.
	b, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	plan, _ := raw["Plan"].(map[string]any)
	items, _ := plan["Items"].([]any)
	if len(items) < 2 {
		t.Fatalf("fixture needs a split to tamper with, got %d items", len(items))
	}
	first, _ := items[0].(map[string]any)
	first["Weight"] = 99 // move the money without touching the ID
	edited, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	editedPath := filepath.Join(dir, "edited.json")
	if err := os.WriteFile(editedPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	err = runCmd(context.Background(), []string{
		"--plan", editedPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--out", filepath.Join(dir, "r.json"), gateQuestion,
	})
	if !errors.Is(err, quarry.ErrPlanTampered) {
		t.Fatalf("an edited artifact must be REFUSED, not warned about, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "r.json")); statErr == nil {
		t.Fatal("a tampered plan must not produce a record")
	}
}

// D6 end to end: `quarry plan` on a problem that should not be split emits a valid
// artifact, and `run --plan` executes it as a single node. §13 named this the open
// question that blocked the gate.
func TestADeclinedPlanRoundTripsThroughBothVerbs(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "declined.json")
	// A statement with no separable sub-questions: FakePlanner declines on clause count, so
	// this is reachable with --fake and needs no live provider.
	const narrow = "What does storage cost"
	if err := planCmd(context.Background(), []string{
		"--fake", "--cap", "1.00", "--depth", "2", "--out", planPath, narrow,
	}); err != nil {
		t.Fatalf("a decline must be an OUTCOME, not an error from quarry plan: %v", err)
	}
	art, err := readPlanArtifact(planPath)
	if err != nil {
		t.Fatalf("a declined artifact must verify: %v", err)
	}
	if !art.Plan.Declined {
		t.Fatalf("fixture did not decline, so this test does not exercise D6: %+v", art.Plan)
	}
	if art.Plan.Reasoning == "" {
		t.Fatal("a decline a host is asked to approve must say WHY (P1)")
	}

	recPath := filepath.Join(dir, "r.json")
	if err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--out", recPath, narrow,
	}); err != nil {
		t.Fatalf("an approved decline must EXECUTE: %v", err)
	}
	rec, err := readRecord(recPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Outcomes) != 1 {
		t.Fatalf("an approved decline runs as ONE node, got %d", len(rec.Outcomes))
	}
	if rec.PlanID != art.PlanID {
		t.Fatal("a declined run is still a GATED run: the record must name its approval (D3)")
	}
}

// D4: the artifact states what planning cost, and it is MEASURED rather than assumed.
// "Near-zero spend" must be a stated number with its own cap, not a hope.
func TestThePlanArtifactStatesWhatPlanningCost(t *testing.T) {
	dir := t.TempDir()
	art, err := readPlanArtifact(planFile(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if !art.PlanCap.Limited() || art.PlanCap <= 0 {
		t.Fatalf("planning must run under its OWN cap so it cannot eat the run's budget "+
			"(D1/D4), got %s", art.PlanCap)
	}
	// --fake calls no model, so zero here is the truth. The assertion that matters is that
	// the cost cannot EXCEED the cap it states, which holds in both modes.
	if art.PlanCost > art.PlanCap {
		t.Fatalf("planning cost %s over its stated cap of %s", art.PlanCost, art.PlanCap)
	}
	if art.PlannerModel != quarry.FakePlannerModel {
		t.Fatalf("a --fake plan must be marked as synthetic so it cannot authorise real "+
			"spend, got %q", art.PlannerModel)
	}
}

// THE FILE ON DISK IS THE HASHED ARTIFACT, not a pretty re-encoding: a host that reads
// the file, shows it and hands it back must produce bytes that still verify.
func TestThePlanFileIsTheCanonicalBytes(t *testing.T) {
	dir := t.TempDir()
	planPath := planFile(t, dir)
	onDisk, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	art, err := quarry.DecodePlanArtifact(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := art.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(canonical) {
		t.Fatal("the written file must BE the canonical bytes: a re-encoding hashes " +
			"differently from the PlanID it carries, and the gate reads that as tampering")
	}
}

// `quarry plan` must not promise a split the run would never perform. A root the executor
// terminates before planning yields a declined artifact rather than a fiction.
func TestPlanDeclinesWhereTheExecutorWouldNotEvenPlan(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "p.json")
	// A cap below the floor: PrePlanBase reports BaseBelowFloor, and Executor.node solves
	// directly without consulting a planner.
	if err := planCmd(context.Background(), []string{
		"--fake", "--cap", "0.0003", "--floor", "0.0002", "--depth", "2",
		"--out", planPath, gateQuestion,
	}); err != nil {
		t.Fatal(err)
	}
	art, err := readPlanArtifact(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !art.Plan.Declined {
		t.Fatalf("a root the executor terminates before planning must yield a DECLINED "+
			"artifact, not a promised split: %+v", art.Plan)
	}
	if !strings.Contains(art.Plan.Reasoning, string(quarry.BaseBelowFloor)) {
		t.Fatalf("the decline must name which base case produced it, got %q", art.Plan.Reasoning)
	}
	// And it must RUN, as one node — the artifact is approvable like any other.
	recPath := filepath.Join(dir, "r.json")
	if err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "0.0003", "--floor", "0.0002",
		"--depth", "2", "--out", recPath, gateQuestion,
	}); err != nil {
		t.Fatalf("the artifact must execute under the conditions it was planned for: %v", err)
	}
}

// duplicateQuestion has two identical clauses, so FakePlanner proposes a repeated child
// and the DAG rule (§2) collapses it. Reachable under --fake, which matters: the whole
// point is that the gate and the executor agree on the shape, and a live-only fixture
// would leave that untested on the path everything else uses.
const duplicateQuestion = "How does it scale, what does storage cost, what does storage cost, and who pays?"

// THE ARTIFACT'S FANOUT MUST BE THE RUN'S FANOUT. `quarry plan` collapses duplicate
// children exactly as Executor.node does; an artifact that listed four children and ran
// three would divide the money differently from the division that was approved.
//
// THIS TEST EXISTS BECAUSE REMOVING THE COLLAPSE CHANGED NOTHING. With the CLI's
// DedupePlan call deleted, every other test here stayed green and the two-phase run
// completed with exit 0 — the artifact stored four un-collapsed shares and the gate
// re-derived four from the same un-collapsed items, so both sides of D1's comparison were
// wrong in the same direction. Apportion now collapses too (plan.go); this asserts the
// producer half, and plan_test.go's TestApportionCollapsesTheSameDuplicatesTheExecutorWill
// asserts the checker half. Either alone leaves the hole open.
func TestThePlanArtifactCarriesTheCollapsedFanoutTheRunWillUse(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "dup.json")
	if err := planCmd(context.Background(), []string{
		"--fake", "--cap", "1.00", "--depth", "2", "--out", planPath, duplicateQuestion,
	}); err != nil {
		t.Fatal(err)
	}
	art, err := readPlanArtifact(planPath)
	if err != nil {
		t.Fatal(err)
	}
	// FIXTURE CHECK, not decoration: if this planner ever stops proposing a duplicate the
	// test silently tests nothing, and the collapse is invisible either way from outside.
	seen := map[string]bool{}
	for _, it := range art.Plan.Items {
		if seen[it.Problem.Statement] {
			t.Fatalf("the artifact still lists a duplicate child %q — the collapse did not "+
				"happen, so the approved fanout is not the fanout the run will have",
				it.Problem.Statement)
		}
		seen[it.Problem.Statement] = true
	}
	if len(art.Plan.Items) != 3 {
		t.Fatalf("fixture: want 3 collapsed children from 4 proposed, got %d — this test "+
			"needs a duplicate to collapse or it proves nothing", len(art.Plan.Items))
	}
	if len(art.Allocations) != len(art.Plan.Items) {
		t.Fatalf("one share per approved child: %d children, %d allocations",
			len(art.Plan.Items), len(art.Allocations))
	}

	// And it must RUN, at that fanout. This is the end the defect actually reached: it
	// completed with exit 0 on a tree nobody approved.
	recPath := filepath.Join(dir, "r.json")
	if err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--out", recPath, duplicateQuestion,
	}); err != nil {
		t.Fatalf("the collapsed artifact must be authorized for its own run: %v", err)
	}
	rec, err := readRecord(recPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(art.Plan.Items) + 1; len(rec.Outcomes) != want {
		t.Errorf("the run must have the approved fanout: %d nodes, want %d", len(rec.Outcomes), want)
	}
}

// `show` must display the approval. A record that names a plan nobody can see is a gate
// whose evidence is unreadable — and every three-state or extra field added to a record
// so far has had to survive to the projection.
func TestShowNamesTheApprovedPlan(t *testing.T) {
	dir := t.TempDir()
	planPath := planFile(t, dir)
	recPath := filepath.Join(dir, "r.json")
	if err := runCmd(context.Background(), []string{
		"--plan", planPath, "--fake", "--quiet", "--cap", "1.00", "--depth", "2",
		"--out", recPath, gateQuestion,
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := readRecord(recPath)
	if err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	showRecord(newPrinter(&buf), rec, 100)
	if !strings.Contains(buf.String(), rec.PlanID[:12]) {
		t.Fatalf("show must name the approved plan (D3): %s", buf.String())
	}

	// NON-VACUITY: an UNGATED record must not claim an approval, or the check above would
	// pass against a projection that printed the line unconditionally.
	ungated := rec
	ungated.PlanID = ""
	var buf2 strings.Builder
	showRecord(newPrinter(&buf2), ungated, 100)
	if strings.Contains(buf2.String(), "APPROVED") {
		t.Fatal("an ungated record must not be shown as approved")
	}
}
