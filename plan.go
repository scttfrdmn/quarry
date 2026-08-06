package quarry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"
)

// The plan artifact (§9's first interaction, #15): the object a supervising host
// approves BEFORE any money is committed.
//
// WHY THIS IS A NEW TYPE RATHER THAN A USE OF PinnedPlanner. Pinning (§7) also
// replays a decomposition, and the two look alike from a distance, but PinPlan takes
// a COMPLETED RunRecord and reads each child's weight off the child itself — "a
// plan's weights are only observable through the nodes they paid for". A pre-spend
// gate has no nodes and no record, so there is nothing for it to read. Pinning is a
// replication control that freezes shape so variance is attributable to solving;
// approval is a different question asked at a different time, and the artifact it
// needs did not exist: Plan had no serialized form and no identity.
//
// THREE PROPERTIES MAKE IT A GATE RATHER THAN A SUGGESTION, and each is enforced
// here rather than requested of the host:
//
//   - It carries THE CAP IT WAS PLANNED UNDER (D1, P9). Planning is
//     budget-conditioned: the planner receives the balance and must return a split
//     that fits it, and it may decline. The same split under half the money is a
//     different plan — one the planner might have refused — so executing an approved
//     plan under a different cap is not the approved run. This is the integrity
//     property the whole gate rests on, and Authorizes refuses the mismatch rather
//     than silently re-apportioning.
//   - It carries THE SCOPE IT WAS PLANNED UNDER (D2, P6). Scope never widens on
//     descent, and a two-phase split is a new place that rule can break: approved
//     under one scope, executed under a broader one.
//   - It is IDENTIFIED BY A CONTENT HASH (D3, P8). "Which plan was approved" is a
//     fact of the original execution in exactly the sense BoundBy and RunBounds are,
//     so it cannot be re-derived from the tree's geometry afterwards. The record
//     names it (RunRecord.PlanID), and a file that does not hash to its own PlanID
//     is REFUSED — see ReadPlanArtifact's note on why this is stricter than a record.
//
// THE ESTIMATE IS ADVISORY AND NOTHING GATES ON IT (D5, P4). It is carried for the
// operator's judgement, with the caveat alongside it in the artifact so a host
// cannot render the number without the sentence that qualifies it. What binds is
// Caps.

// PlanArtifactVersion is the artifact's contract version, read by two hosts in two
// languages.
//
// FROZEN LIKE THE EVENT STREAM'S FRAME (integration-requirements §6 D2): a host must
// be able to REFUSE an artifact before it acts on one, and it can only do that if the
// version is declared. Adding a field is a minor change a v1 reader ignores; changing
// the meaning of a field is not, and needs this number.
const PlanArtifactVersion = 1

var (
	// ErrPlanNotAuthorized is returned when an approved plan is asked to execute
	// under conditions it was not planned for — a different cap (D1), a widened
	// scope (D2), a different problem, or an apportionment that no longer matches.
	//
	// A REFUSAL RATHER THAN A RE-PLAN, because the alternative silently converts an
	// approval into an authorization for something else. Under P9 the plan and the
	// budget are one decision; a host that wants a plan for a different cap runs
	// `quarry plan` again, which is cheap and says what it cost.
	ErrPlanNotAuthorized = errors.New("plan is not authorized for these conditions")

	// ErrPlanTampered is returned when an artifact does not hash to its own PlanID.
	//
	// STRICTER THAN THE RECORD'S EQUIVALENT, deliberately, and the asymmetry is the
	// point. A record that fails its hash is still worth reading — it is history, and
	// readRecord warns. An artifact is an AUTHORIZATION, and an approval gate whose
	// artifact can be edited in flight is not a gate: honouring an edited plan would
	// spend money on a split nobody approved while recording a PlanID that says
	// somebody did.
	ErrPlanTampered = errors.New("plan artifact does not hash to its own PlanID")
)

// PlanArtifact is a proposed decomposition, approvable before spend (#15).
//
// FIELD ORDER IS PART OF THE IDENTITY. canonical() relies on struct-field
// declaration order, so reordering these fields changes every artifact's hash. New
// fields go at the END, and a field whose absence is meaningful gets `omitempty` so
// artifacts written before it existed still hash to what they claim.
type PlanArtifact struct {
	// PlanID is the content hash with this field zeroed — the same construction as
	// RunRecord.RunID, so the identity is a function of the content (P8).
	PlanID  string
	Version int

	// Problem is what was planned, WITH ITS SCOPE (D2). The scope is not decoration
	// here: it is half of what the artifact authorizes.
	Problem Problem

	// Caps is the budget the plan was planned against (D1, P9). See Authorizes.
	Caps Caps

	// Floor and Depth are the two executor parameters the plan actually depends on,
	// carried for the same reason RunBounds is carried on a record: they are facts of
	// the planning act that cannot be re-derived from the plan's shape.
	//
	// NOT RunBounds ITSELF, though it would have fit. RunBounds also carries
	// MaxRetries, which no part of a plan depends on — pinning it here would make an
	// artifact refuse a run that differs in a way the plan cannot see.
	//
	//	Floor  the apportionment below was computed against it (Apportion), so a
	//	       different floor is a different set of allocations for the same weights
	//	Depth  the estimate below was projected over it, and it bounds the tree the
	//	       host is approving
	Floor Units
	Depth int

	// Plan is the planner's own output, unaltered — the same value the executor
	// consumes. Stored rather than translated into an artifact-specific shape, so
	// there is no second representation that can drift from the first.
	Plan Plan

	// Allocations is where the money goes, BY PLAN POSITION (§3). Under P9 the gate
	// shows three things rather than one — the split, where the money goes, and what
	// the cap excludes (Plan.Excluded) — because the operator's real decision is
	// usually "raise the cap or accept the reduced scope", and they can only make it
	// if the exclusions are stated before spend.
	//
	// Positional, never keyed by Problem.Key(): a key is not unique across a plan's
	// items, and a portfolio's arms are the same problem by definition (§2).
	Allocations []Allocation

	// Estimate is advisory and NOTHING MAY GATE ON IT (D5, P4). It depends on a
	// corpus that does not exist until the system has run many times.
	Estimate CostEstimate

	// EstimateCaveat is the qualification that travels WITH the number, so a host
	// cannot render the estimate without it.
	//
	// A FIELD RATHER THAN DOCUMENTATION because the failure it prevents is a UI
	// failure in someone else's repo: an approval screen that shows "$0.31" beside an
	// Approve button has presented an advisory projection as a commitment, and P4 says
	// the cap is the contract and the estimate a courtesy. Derived from the estimate's
	// own flags, so it sharpens when the projection is actually untrustworthy rather
	// than reading as boilerplate.
	EstimateCaveat string

	// PlanCost is what producing this artifact ACTUALLY cost, as metered (D4).
	//
	// MEASURED, NOT ESTIMATED. One planner call is not free — §4 calls the probe "one
	// call, ~1/N of the run" — and "near-zero spend" must be a stated number with its
	// own cap rather than a hope, or a host that budgeted zero for planning is
	// surprised by its first bill.
	PlanCost Units

	// PlanCap is the ceiling the planning phase itself ran under (D4, P4). Stated
	// because the total a host commits is this plus Caps.Spend, and a receipt that
	// names only one of them understates it.
	PlanCap Units

	// PlannerModel is the model that proposed this split — explicit, never an alias
	// (P8). "fake" for a plan produced by the no-model planner, which is why
	// Authorizes can refuse a synthetic plan being executed with real money: the two
	// modes' Units are not the same quantity.
	PlannerModel string
}

// NewPlanArtifact assembles an artifact and seals it with its content hash.
//
// The hash is computed LAST, over everything else, so the identity covers the cap,
// the scope, the weights and the apportionment together. Sealing the plan without
// the cap would produce an artifact that authorizes the same split under any budget,
// which is precisely what D1 forbids.
func NewPlanArtifact(p Problem, caps Caps, floor Units, depth int, plan Plan, allocs []Allocation,
	est CostEstimate, planCost, planCap Units, plannerModel string) PlanArtifact {
	a := PlanArtifact{
		Version:        PlanArtifactVersion,
		Problem:        p,
		Caps:           caps,
		Floor:          floor,
		Depth:          depth,
		Plan:           plan,
		Allocations:    allocs,
		Estimate:       est,
		EstimateCaveat: EstimateCaveat(est),
		PlanCost:       planCost,
		PlanCap:        planCap,
		PlannerModel:   plannerModel,
	}
	a.PlanID = planHash(a)
	return a
}

// EstimateCaveat states what the projection is worth, in the terms §4 uses.
//
// THREE SENTENCES FOR THREE REGIMES, because "advisory" is not equally true in each:
// under a subcritical branching process P50/P90 mean something, and at or above m=1
// they are theatre and only the ceiling is trustworthy. A single fixed caveat would
// under-warn on exactly the runs where the number is worst.
func EstimateCaveat(est CostEstimate) string {
	const p4 = "advisory only: the cap is the contract and the estimate a courtesy (P4), " +
		"and calibration depends on a corpus that does not exist until many runs have happened (§4)"
	switch {
	case est.Diverges:
		return "the projected branching factor is at or above 1, so the process is critical or " +
			"supercritical and ONLY the ceiling is trustworthy — " + p4
	case est.NearUnity:
		return "the projected branching factor is near 1, where variance dominates: treat P50 and " +
			"P90 as theatre and read the ceiling — " + p4
	}
	return "a projection, not a quote — " + p4
}

// Canonical returns the artifact's stable byte encoding: the bytes that are hashed,
// written, handed to a host and handed back.
//
// A HOST MUST RETURN THE FILE, NOT A RE-ENCODING. The same rule writeRecord obeys for
// records applies here for the same reason — a pretty-printed or re-serialized
// artifact hashes differently from the PlanID it contains, and the gate reads that as
// tampering. It is stated here because this artifact makes a round trip through
// somebody else's process, which a record never does.
func (a PlanArtifact) Canonical() ([]byte, error) { return canonical(a) }

// DecodePlanArtifact parses an artifact and VERIFIES IT, so a caller cannot hold an
// unchecked one (D3).
//
// VERIFICATION IS NOT SEPARABLE HERE, unlike for a record. readRecord parses and then
// warns, because history is worth reading even when it has been edited. An artifact is
// an authorization: a decode that returned an unverified value would let a caller
// forget the check, and the one caller who forgot would spend money on a split nobody
// approved. Refusing at the parse makes forgetting impossible.
func DecodePlanArtifact(b []byte) (PlanArtifact, error) {
	art, err := decodePlan(b)
	if err != nil {
		return PlanArtifact{}, err
	}
	if err := art.Verify(); err != nil {
		return PlanArtifact{}, err
	}
	return art, nil
}

// decodePlan parses without verifying — the round-trip tests need to compare bytes
// before asserting anything about the hash, and DecodePlanArtifact is built on it.
func decodePlan(b []byte) (PlanArtifact, error) {
	var art PlanArtifact
	if err := json.Unmarshal(b, &art); err != nil {
		return PlanArtifact{}, fmt.Errorf("parse plan artifact: %w", err)
	}
	return art, nil
}

// PlanArtifactHash recomputes an artifact's identity from its content, so a file read
// back from disk can be checked against the ID it carries (D3).
//
// Exported for the same reason RecordHash is: a file somebody may have edited is the
// case the hash exists for, and an in-process value trivially agrees with itself.
func PlanArtifactHash(a PlanArtifact) string { return planHash(a) }

// planHash is sha256 over the canonical encoding with PlanID zeroed.
func planHash(a PlanArtifact) string {
	a.PlanID = ""
	b, err := canonical(a)
	if err != nil {
		// Canonical encoding of a plain value struct cannot fail; treat as a bug.
		panic(fmt.Sprintf("quarry: canonical encoding failed: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Verify checks the artifact against itself: version and hash (D3).
//
// SEPARATE FROM Authorizes, which checks it against a run. This one answers "is this
// a quarry artifact and is it intact"; that one answers "may this run execute it".
// Both are refusals, but a caller can only give a useful message by knowing which
// failed — a tampered file and a mismatched cap have nothing to do with each other.
func (a PlanArtifact) Verify() error {
	if a.Version != PlanArtifactVersion {
		return fmt.Errorf("%w: artifact version %d, this build speaks %d",
			ErrPlanNotAuthorized, a.Version, PlanArtifactVersion)
	}
	if a.PlanID == "" {
		return fmt.Errorf("%w: no PlanID; this is not a plan artifact", ErrPlanTampered)
	}
	if want := planHash(a); want != a.PlanID {
		return fmt.Errorf("%w: claims %s, computes %s", ErrPlanTampered, trunc12(a.PlanID), trunc12(want))
	}
	return nil
}

// commonPrefixLen is the byte index at which two strings first differ, or their shared
// length if one is a prefix of the other.
func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// quoteAround renders s clipped to a WINDOW AROUND ITS DIVERGENCE FROM other, so two
// long statements sharing a long prefix show the part that actually differs.
//
// Elides with a leading "…" when the window does not start at the beginning, so a reader
// is never shown a fragment that looks like the whole statement. Clipped on a rune
// boundary: cutting a multi-byte character mid-way would print a replacement char and
// make a message about text differences itself unreadable.
func quoteAround(s, other string) string {
	const window = 60
	d := commonPrefixLen(s, other)
	start := d - window/3
	if start < 0 {
		start = 0
	}
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	end := start + window
	if end > len(s) {
		end = len(s)
	}
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}
	out := strconv.Quote(s[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(s) {
		out += "…"
	}
	return out
}

// Authorizes reports whether this artifact permits a run of p under caps, floor and
// depth — the D1/D2 gate (P9, P6).
//
// FIVE REFUSALS, and the order is chosen so the message names the most fundamental
// disagreement first: a plan for a different QUESTION is not a cap problem, and
// reporting it as one sends the operator to the wrong flag.
//
//	problem     a plan for another statement is not this plan at all
//	scope       a run may narrow the planned scope, never widen it (D2, P6)
//	caps        the cap the plan was conditioned on (D1, P9)
//	floor       the apportionment was computed against it, so it changes the money
//	depth       the tree the host approved was bounded by it
//
// THE MODEL MODE IS CHECKED TOO, and it belongs to D1 rather than being a new rule.
// A --fake plan's Units are synthetic; executing it with real money would compare a
// synthetic budget against a real one and call them equal, which is the cap-integrity
// property D1 exists to protect, arriving from an unexpected direction.
func (a PlanArtifact) Authorizes(p Problem, caps Caps, floor Units, depth int, plannerModel string) error {
	if a.Problem.Statement != p.Statement {
		// SHOWN FROM WHERE THEY DIVERGE, not from the start. A plain %.60q truncated both
		// sides to a COMMON PREFIX and printed two identical-looking lines under the words
		// "a different problem" — which is the one case this error is for. Found by pasting
		// the command `plan` prints and reading the refusal: the statements differed in a
		// trailing fragment that neither line reached.
		return fmt.Errorf("%w: the plan was made for a different problem\n  planned %s\n  asked   %s\n"+
			"  they first differ at byte %d; the run must restate the statement the plan was made for",
			ErrPlanNotAuthorized, quoteAround(a.Problem.Statement, p.Statement),
			quoteAround(p.Statement, a.Problem.Statement), commonPrefixLen(a.Problem.Statement, p.Statement))
	}
	// P6, and the SAME DIRECTION Ledger.Child enforces: every tag the plan was
	// planned under must be present and equal in the run's scope. Dropping a tag is
	// widening — the run would reach documents the approved plan could not — so both
	// sentinels are wrapped: this is a plan refusal AND a scope violation, and a
	// caller checking either finds it.
	if !a.Problem.Scope.NarrowsTo(p.Scope) {
		return fmt.Errorf("%w: %w\n  planned under %q, asked for %q",
			ErrPlanNotAuthorized, ErrScopeWidens, a.Problem.Scope.Key(), p.Scope.Key())
	}
	if !a.Caps.SameAs(caps) {
		return fmt.Errorf("%w: the plan was planned against a different budget (D1, P9)\n"+
			"  planned cap %s, deadline %s, due %s\n  asked   cap %s, deadline %s, due %s\n"+
			"  planning is budget-conditioned: the same split under a different cap is a plan "+
			"the planner might have declined. re-run `quarry plan` for this budget",
			ErrPlanNotAuthorized, a.Caps.Spend, a.Caps.Latency, dueStr(a.Caps.Due),
			caps.Spend, caps.Latency, dueStr(caps.Due))
	}
	if a.Floor != floor {
		return fmt.Errorf("%w: the apportionment was computed against floor %s, asked for %s\n"+
			"  the floor decides how the money divides, so the approved shares no longer hold",
			ErrPlanNotAuthorized, a.Floor, floor)
	}
	if a.Depth != depth {
		return fmt.Errorf("%w: the plan bounded the tree at depth %d, asked for %d\n"+
			"  the depth backstop is part of what was approved (P2)", ErrPlanNotAuthorized, a.Depth, depth)
	}
	// Exactly one side synthetic. Both fake or both live is fine; the artifact does
	// not pin WHICH live model, because below the approved root the planner is the
	// run's own and choosing it is not the gate's business.
	if (a.PlannerModel == FakePlannerModel) != (plannerModel == FakePlannerModel) {
		return fmt.Errorf("%w: the plan was proposed by %q and the run's planner is %q\n"+
			"  a synthetic plan's costs are synthetic, so its cap is not the same quantity as a "+
			"real one (D1)", ErrPlanNotAuthorized, a.PlannerModel, plannerModel)
	}
	return nil
}

// FakePlannerModel is the PlannerModel an artifact carries when no model was called.
//
// A CONSTANT IN THE CORE, though the fake planner lives in provider/, because
// Authorizes must be able to tell a synthetic plan from a real one and the core may
// not import provider/. Named rather than a bare "fake" literal at two call sites in
// two packages.
const FakePlannerModel = "fake"

// Apportion re-derives the allocations from the plan against a live ledger and
// checks they are the ones that were approved.
//
// A MECHANICAL CHECK ON TOP OF THE HASH, and it earns its place by catching a
// different class of defect. The hash catches an EDITED file. This catches a plan
// that is internally consistent but no longer apportions the way it did — a change
// in Apportion's own arithmetic, a ledger whose reserve differs, a balance already
// partly spent. Any of those would fund the approved children differently while the
// artifact still hashes perfectly.
//
// Returns the allocations to use, which are the RE-DERIVED ones rather than the
// stored ones. Deliberate: the executor must apportion through the ledger it is
// actually holding, and a stored slice injected past it would bypass Reserve. The
// stored values are the assertion, not the source.
//
// SPEND ONLY is compared. Apportion never sets a Deadline — time divides through
// ChildContext, not through the apportionment (§3.1) — so a Deadline here would be a
// wall-clock instant from planning time that no later run could reproduce.
//
// IT DEDUPES FIRST, AND THAT WAS A DEFECT UNTIL IT DID. Executor.node collapses the
// plan (executor.go, the DAG rule) BEFORE apportioning, so a check that apportioned the
// artifact's items verbatim was comparing against a shape the run would never adopt: an
// artifact carrying two identical children passed the check, then ran as one merged
// child on a different division of the money — the gate approving one tree and the
// executor running another, which is the single thing it exists to prevent. Found by
// removing the CLI's own DedupePlan call behind the tests' backs and watching nothing
// fail. Deduping HERE rather than only at the writer is what makes the property hold for
// every artifact, including ones this CLI did not write.
func (a PlanArtifact) Apportion(l *Ledger) ([]Allocation, error) {
	allocs, err := l.Apportion(dedupePlan(a.Plan), a.Floor)
	if err != nil {
		return nil, err // ErrPlanDoesNotFit
	}
	if len(allocs) != len(a.Allocations) {
		return nil, fmt.Errorf("%w: the approved plan apportions into %d children, this run derives %d",
			ErrPlanNotAuthorized, len(a.Allocations), len(allocs))
	}
	for i := range allocs {
		if allocs[i].Spend != a.Allocations[i].Spend {
			return nil, fmt.Errorf("%w: child %d was approved at %s and this run derives %s\n"+
				"  the approved apportionment no longer holds, so the money would not go where "+
				"it was approved to go (D1, §3)",
				ErrPlanNotAuthorized, i, a.Allocations[i].Spend, allocs[i].Spend)
		}
	}
	return allocs, nil
}

// Planner returns the Planner that executes this artifact and nothing else (D6).
//
// THE APPROVED PLAN GOVERNS THE ROOT; the delegate plans below it. That split is the
// non-goal "approving anything below the root" made structural rather than
// documented: what the host approved is the split of the whole problem and the
// division of the money across it, and each child then works inside an allocation
// that WAS approved. A child re-planning within its own share is not spending
// authority nobody granted — it is spending the share the gate assigned it.
//
// A DECLINED ARTIFACT NEEDS NO DELEGATE and gets one call: the root declines, the
// executor solves it as a single node, and the delegate is never reached (D6, P1).
// That is the case --fake produces routinely, since its planner declines on clause
// length, and it must round-trip as a valid approval rather than as an error.
func (a PlanArtifact) Planner(delegate Planner) ApprovedPlanner {
	return ApprovedPlanner{artifact: a, delegate: delegate}
}

// ApprovedPlanner replays one approved root plan and delegates below it.
//
// Concurrent-safe by construction: it holds no mutable state, which the Planner seam
// requires because sibling subtrees plan on separate goroutines.
type ApprovedPlanner struct {
	artifact PlanArtifact
	delegate Planner
}

// Plan returns the approved decomposition at the root and the delegate's below it.
//
// A MISMATCH AT THE ROOT IS AN ERROR, NOT A FALL-THROUGH TO THE DELEGATE. Falling
// through would spend money on an unapproved split while the record still named the
// artifact, and a gate that quietly does the ungated thing is worse than no gate.
// Authorizes has already compared the statement, so reaching this is a wiring defect
// rather than a user error — which is exactly why it must be loud.
func (ap ApprovedPlanner) Plan(ctx context.Context, p Problem, alloc Allocation, depth int, prior []NodeOutcome) (Plan, error) {
	if depth == 0 {
		if p.Key() != ap.artifact.Problem.Key() {
			return Plan{}, fmt.Errorf("%w: the approved plan is for another problem key at the root",
				ErrPlanNotAuthorized)
		}
		return ap.artifact.approvedPlan(), nil
	}
	if ap.delegate == nil {
		// Declining here would silently make every child a leaf — a different tree than
		// the one approved, reported as a faithful one. The approved plan says these
		// children exist; nothing here is entitled to decide they cannot decompose.
		return Plan{}, fmt.Errorf("no planner is wired below the approved root: the plan gate "+
			"governs depth 0 only, and depth %d has nothing to plan with (#15)", depth)
	}
	return ap.delegate.Plan(ctx, p, alloc, depth, prior)
}

// approvedPlan hands out a COPY with its own Items array.
//
// The executor mutates a plan it receives — dedupePlan merges weights into the items
// it returns — and while today's dedupePlan appends into a fresh slice, an artifact
// that can be altered by the run executing it would make the PlanID a claim about
// something that no longer exists. Cheap insurance on the one value the whole gate is
// about.
func (a PlanArtifact) approvedPlan() Plan {
	out := a.Plan
	out.Items = append([]PlanItem(nil), a.Plan.Items...)
	out.Excluded = append([]string(nil), a.Plan.Excluded...)
	return out
}

// SameAs compares two cap sets for the purposes of the plan gate (D1).
//
// NOT ==, AND THE REASON IS THAT ONE INSTANT HAS MANY SPELLINGS. Caps is comparable, so
// == compiles and looks right — but time.Time holds a *Location, and == compares that
// pointer. `--due 2026-08-06T17:00:00Z` and `--due 2026-08-06T13:00:00-04:00` are the
// same moment and resolveDue accepts both (its own error message offers both spellings),
// yet == calls them different. So would a value carrying a monotonic reading, which is
// how the same defect reaches anything derived from time.Now().
//
// == would therefore refuse an artifact whose due date IS the one it was planned with,
// turning the gate into a source of spurious refusals — the failure mode that makes an
// approval gate something operators route around. Time.Equal compares the instant, which
// is the thing anybody means by a deadline.
func (c Caps) SameAs(other Caps) bool {
	return c.Spend == other.Spend && c.Latency == other.Latency && c.Due.Equal(other.Due)
}

// PrePlanBase reports the base case that terminates a node BEFORE any planner call,
// and whether one fires (§2).
//
// SHARED BY THE EXECUTOR AND THE PLAN GATE, which is the whole reason it is a
// function and exported. `quarry plan` must not emit an artifact promising a split
// that the run would never perform: at depth 0 with a zero depth bound, or with a
// balance that cannot fund one child above the floor, the executor solves directly
// and never asks the planner anything. A second copy of that ordering in the CLI
// would be a second thing to keep in agreement, and this codebase has three comments
// about exactly that failure.
//
// THE VERIFIER CHECK IS NOT HERE, though it is also pre-plan (P2). It needs the
// executor's Verifier, and a nil verifier means no verification rather than no
// availability — folding it in would make this function's contract depend on a seam
// the plan gate does not hold.
func PrePlanBase(l *Ledger, floor Units, depth, maxDepth int) (BaseCase, bool) {
	if depth >= maxDepth {
		return BaseMaxDepth, true
	}
	if ap := l.Apportionable(); ap.Limited() && ap < floor {
		// Cannot fund even one child above floor plus the reduce — splitting is
		// pointless, so the node solves directly (base case 3).
		return BaseBelowFloor, true
	}
	return "", false
}

// dueStr renders a due date for an error message, saying "unset" rather than printing
// the zero time — "0001-01-01T00:00:00Z" in a refusal reads as a corrupt value.
func dueStr(t time.Time) string {
	if t.IsZero() {
		return "unset"
	}
	return t.Format(time.RFC3339)
}

// trunc12 shortens a hash for a message. Guards the length rather than slicing
// blindly, for the reason truncKey does: a malformed artifact must produce an error,
// not a panic inside the code reporting it.
func trunc12(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
