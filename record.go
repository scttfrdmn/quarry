package quarry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// The run record (§8) and replay (§7). The record is the citable artifact:
// content-hashed, self-sufficient, replayable without model access (P8).
//
// Two properties from §7, kept deliberately separate:
//
//   - Reproduce (replay). Re-execute the tree against recorded responses, no
//     model calls. Deterministic BY CONSTRUCTION — this is why Units is integral
//     (largest-remainder apportionment replays bit-for-bit) and why nothing here
//     reads the clock. This file's job is to make that determinism checkable:
//     assemble a record, replay it, and the canonical bytes must be identical.
//
//   - Replicate (re-derive). A fresh run with live models and its own variance.
//     That is step 7, not this one.
//
// Replay substitutes THREE seams, because three things in a run are stochastic:
// the plan, each leaf answer, and each reduction. RecordedProvider covers leaves
// only; PinnedPlanner and RecordedReducer cover the other two, and Replayable
// builds all three from one record so a caller cannot wire a partial replay. See
// the SCOPE note on RecordedProvider for why the reducer cannot be folded into it.

// NewRunRecord assembles the citable record from a completed run.
//
// The RunID is the content hash of the record with its own ID field zeroed, so
// the identity is a function of the content and is stable across replay (P8).
// Derives the Unverified list (§8): a record must be able to say what was NOT
// checked, distinct from what was checked and passed.
func NewRunRecord(res Result, root Problem, caps Caps, mode Mode) RunRecord {
	r := RunRecord{
		Problem:     root,
		Caps:        caps,
		Mode:        mode,
		Outcomes:    res.Outcomes,
		BoundBy:     res.BoundBy,
		Unverified:  unverified(res.Outcomes),
		Adversarial: res.Adversarial,
		Bounds:      res.Bounds,
	}
	r.RunID = contentHash(r)
	return r
}

// ReplayRecord assembles a REPLAYED record, inheriting from the original every field
// a replay cannot legitimately re-derive (§7).
//
// FOUND BY A LATENCY-BOUND FIXTURE. BoundBy comes from BoundBy(ctx, l) — a reading of
// the live execution environment, not a property of the tree — and a replay runs with
// no deadline on purpose. So a run the clock actually bound recorded "latency" and its
// replay recorded "", and the two differed by a field the replay had no way to
// reproduce: replay reported a divergence when nothing had diverged. Exactly the
// failure ErrRecordedGap fixed one layer down, and it bites the same records, because
// a run bound by time is the run most likely to have gaps.
//
// Four fields are inherited and three re-derived, and the split is the whole point:
//
//	inherited   Problem, Caps, Mode   the replay is ABOUT this run, not its own
//	            BoundBy               which cap bit is a fact of the original execution
//	            Adversarial           no seam replays the adversary (see below)
//	            PlanID                the approval is the original's, not the replay's
//	re-derived  Outcomes, Unverified  the tree IS what replay re-executes
//	            RunID                 must be recomputed, or this proves nothing
//
// Adversarial is inherited for the same reason and is currently LATENT: nothing wires
// an Adversary through the CLI, so no record carries findings yet. Replayable
// substitutes three seams — planner, provider, reducer — and the adversary is a fourth
// model call with no recorded counterpart, so a replay re-deriving it would either
// spend live money or (wired with no adversary, as now) silently drop findings the
// original record contains. TODO(§7): if an adversary seam is ever recorded, move
// Adversarial to the re-derived column and delete this paragraph.
//
// A single call rather than four assignments at the call site, for the reason
// Replayable exists: a caller who forgets one gets a divergence report about their own
// wiring, which is the least informative failure this package can produce.
func ReplayRecord(res Result, orig RunRecord) RunRecord {
	r := RunRecord{
		Problem:     orig.Problem,
		Caps:        orig.Caps,
		Mode:        orig.Mode,
		Outcomes:    res.Outcomes,
		BoundBy:     orig.BoundBy,
		Unverified:  unverified(res.Outcomes),
		Adversarial: orig.Adversarial,
		// INHERITED, and it must be. Bounds are what the replay was CONFIGURED FROM, so
		// re-deriving them from the replay's own executor would be circular: it would agree
		// by construction and prove nothing. They are a fact of the original execution in
		// exactly the sense BoundBy is.
		Bounds: orig.Bounds,
		// INHERITED (#15 D3). A replay re-executes a recorded tree off RecordedProvider;
		// it does not re-approve anything, and it has no artifact in hand. Re-deriving
		// this would mean either dropping it — making a replay of a gated run look
		// ungated, which is the fact the field exists to preserve — or asserting an
		// approval the replay never saw. The approval belongs to the original execution.
		PlanID: orig.PlanID,
	}
	r.RunID = contentHash(r)
	return r
}

// WithPlan names the approved plan artifact this run was authorised to execute, and
// RE-DERIVES THE RUNID so the approval is inside the run's identity (#15 D3).
//
// A METHOD RATHER THAN A NewRunRecord PARAMETER, for the reason Iteration.Record is
// also a second assembly path: NewRunRecord has some thirty call sites and a gated run
// is the exception, so a fifth parameter would make every caller state that it is not
// using the gate. The shape is Iteration.Record's exactly — set the field, then hash —
// "so the predecessor's identity is inside the successor's identity".
//
// THE RE-HASH IS THE POINT, not an implementation detail. PlanID is a hashed field, so
// setting it without recomputing would leave a record that does not hash to its own
// RunID: readRecord would warn that the citable artifact had been edited, and the
// approval would arrive attached to a record nothing would cite. The re-hash lives here
// rather than at the CLI so no caller has to remember it.
func (r RunRecord) WithPlan(planID string) RunRecord {
	r.PlanID = planID
	r.RunID = contentHash(r)
	return r
}

// unverified lists the nodes no verifier assessed — Verified is nil. A cache hit
// carries the stored verdict, so a nil there is genuinely unchecked too. Gaps
// are excluded: an absent node is a gap, not an unverified answer (§3.1, §8).
func unverified(outs []NodeOutcome) []string {
	var ids []string
	for _, o := range outs {
		if o.Verified == nil && !o.Gap {
			ids = append(ids, o.NodeID)
		}
	}
	return ids
}

// RecordHash recomputes a record's identity from its content, so a loaded record can
// be checked against the ID it carries.
//
// Exported because a record READ BACK FROM DISK is the case the hash exists for. The
// RunID is set at assembly and never rechecked in-process, which means every
// in-process use trivially agrees — and the one place the guarantee actually bites,
// loading a file somebody may have edited, had no way to ask. A record that does not
// hash to its own ID is readable but NOT citable (P8): it says something its producer
// did not.
//
// Equal to r.RunID for any record this package produced. A caller comparing them is
// checking the FILE, not the arithmetic.
func RecordHash(r RunRecord) string { return contentHash(r) }

// contentHash is the record's identity: sha256 over the canonical encoding with
// the RunID field zeroed. Deterministic because Canonical is.
func contentHash(r RunRecord) string {
	r.RunID = ""
	b, err := canonical(r)
	if err != nil {
		// Canonical encoding of a plain value struct cannot fail; treat as a bug.
		panic(fmt.Sprintf("quarry: canonical encoding failed: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Canonical returns the record's stable byte encoding — the artifact that is
// hashed, stored and compared. Two records with equal content encode to equal
// bytes on every machine and every replay (P8).
func (r RunRecord) Canonical() ([]byte, error) { return canonical(r) }

// canonical is deterministic JSON: struct fields serialize in declaration order
// and encoding/json sorts map keys, so the output is a pure function of the
// value. HTML escaping is disabled so content bytes round-trip unaltered.
func canonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RecordedProvider replays leaf samples from a record instead of calling a model
// (§7). It is the Provider seam with the stochastic part pinned: a hit returns
// exactly what was recorded; a miss is an error, because being asked for a call
// the record does not contain means the replay diverged from the run — a real
// signal, not something to paper over.
//
// SCOPE, now that the planner and reducer are model calls (provider.BedrockPlanner,
// provider.BedrockReducer): this replays LEAVES ONLY, and it cannot be extended to
// cover the other two. A reduce call reaches Complete with the MERGE PROMPT, not the
// problem statement, so replayKey can never match it — and a plan arrives as JSON,
// not as an answer. Replay therefore substitutes three seams, not one:
//
//	Planner → PinnedPlanner (PinPlan)   replays the recorded decomposition (§7)
//	Solver  → RecordedProvider          replays recorded leaf samples
//	Reducer → RecordedReducer           replays recorded merge/selection output
//
// RecordedSeams wires all three together. Replaying only the provider against a
// model-backed planner would issue live plan calls during "replay" — spending money
// and producing a different tree — which is why that combination is named as a trap
// rather than left to be discovered.
type RecordedProvider struct {
	samples map[string]Sample // scope-qualified: keyed by problem key + model
	costs   map[string]Units  // estimate index: statement + model, no scope (§ note)
	gaps    map[string]bool   // nodes the record shows were cut short by time (§3.1)
	// unfunded is the THIRD category, and it is not a gap. A node the cap could not
	// afford made no model call either, so it has no sample — but under the standing
	// ruling only TIME is a gap, so it carries no Gap flag and must replay as spend
	// degradation rather than as time truncation. Keyed without the model for the same
	// reason gaps are: an unfunded node never reached a provider, so it has no Model.
	unfunded map[string]bool
}

// NewRecordedProvider indexes a record's leaf outcomes by their replay key.
// Internal (reduced) nodes and cache hits are skipped: they are not model calls and
// are recomputed by the deterministic executor during replay.
//
// GAPS ARE INDEXED SEPARATELY rather than skipped. A gapped node made no model call,
// so it has no sample to replay — but the replay still VISITS it, because the pinned
// plan reproduces the shape, and a lookup miss is the signal that the replay diverged.
// Skipping them meant every partial record was unreplayable: `quarry replay` on a run
// with three time-truncated nodes failed with "no recorded sample", reporting a
// divergence when the replay was in fact faithful. Since §3.1 makes partial runs the
// normal outcome under a deadline, that made replay unavailable for exactly the
// records most worth interrogating. A recorded gap now replays AS A GAP.
//
// UNFUNDED NODES ARE INDEXED THE SAME WAY, and missing them was the same defect wearing
// the other cap. FOUND BY THE FIRST LIVE BEDROCK RUN: a 28-node run that hit its spend
// cap left 4 nodes with empty content and no Gap flag — planned degradation, exactly as
// §3.1 specifies — and `quarry replay` failed with "no recorded sample", again reporting
// a divergence against a faithful record. `--fake` could not surface it: the fake's
// per-call cost is uniform and tiny, so the planner's affordability check either funds
// every child or declines the split, and a tree with SOME children priced out is not
// reachable. Only a real price sheet, where one sub-question costs 5x another, produces
// it — and spend is the cap researchers actually set, so this was the common case.
func NewRecordedProvider(r RunRecord) *RecordedProvider {
	m := make(map[string]Sample, len(r.Outcomes))
	costs := make(map[string]Units, len(r.Outcomes))
	gaps := make(map[string]bool)
	// Via RunRecord.Unfunded rather than a predicate inlined below, because this test —
	// no model, no content, no verdict — is subtle enough that three copies of it drifted.
	// One of the three, `quarry replay`'s guard, had it wrong. Keyed WITHOUT the model for
	// the same reason gaps are: an unfunded node never reached a provider, so it has none.
	unfunded := make(map[string]bool)
	for _, o := range r.Unfunded() {
		unfunded[o.Problem.Key()] = true
	}
	for _, o := range r.Outcomes {
		if o.Gap {
			// Keyed WITHOUT the model, unlike a sample. A gapped node never reached a
			// provider, so it has no Model recorded — keying it like a sample produced
			// "\x00" and never matched a lookup for the actual model, which is why the
			// first attempt at this fix changed nothing. The problem key still carries the
			// scope, so P6 holds: a gap cannot be served across an entitlement boundary
			// any more than an answer can.
			gaps[o.Problem.Key()] = true
			continue
		}
		if o.CacheHit || len(o.Children) > 0 {
			continue
		}
		// Already indexed above, and it has no sample to record: it never reached a model.
		if unfunded[o.Problem.Key()] {
			continue
		}
		s := Sample{
			Content:      o.Content,
			Cost:         o.Cost,
			Model:        o.Model,
			ModelVersion: o.ModelVersion,
			Verified:     nil, // the executor re-runs verification; do not pre-seed it
			// Token counts replay too: they are hashed into the record, so a replay
			// that dropped them would re-derive a different hash and read as a
			// divergence when nothing had actually diverged.
			HaloTokens:      o.HaloTokens,
			GeneratedTokens: o.GeneratedTokens,
		}
		m[replayKey(o.Problem, o.Model)] = s
		costs[estimateKey(o.Problem.Statement, o.Model)] = o.Cost
	}
	return &RecordedProvider{samples: m, costs: costs, gaps: gaps, unfunded: unfunded}
}

// ErrRecordedGap reports that the record shows this call was cut short by time rather
// than answered. Returned so the executor's existing time-miss path handles it — that
// path already turns a failed call under an expired context into a Gap, which is
// precisely the outcome to reproduce.
//
// A sentinel, not a formatted error, because the executor must be able to distinguish
// it from a provider fault with errors.Is. Replaying a gap as a fault would fail the
// whole run (§3.1 grants partial tolerance to time and budget only), and replaying it
// as an empty ANSWER would be worse: it would convert a node that was never asked into
// a node that was asked and said nothing.
var ErrRecordedGap = errors.New("recorded as a time gap, not answered")

// ErrRecordedUnfunded reports that the record shows the cap could not afford this call.
// The spend counterpart to ErrRecordedGap, and DELIBERATELY A SEPARATE SENTINEL: the
// executor's time path sets Gap, and reusing it here would relabel spend degradation as
// time truncation, which is the one distinction §3.1's standing ruling turns on. A
// replayed record whose unfunded nodes came back flagged Gap would report more time
// pressure than the run experienced, and Extend would then offer it a deadline raise
// when what it needed was money.
var ErrRecordedUnfunded = errors.New("recorded as unfunded by the cap, not answered")

// Complete serves a recorded sample, or reports WHY there is none: ErrRecordedGap for
// a node time cut short, ErrRecordedUnfunded for one the cap priced out, and a plain
// miss otherwise. The three are distinct because a replay that conflated them would
// relabel spend degradation as time truncation (§3.1) — see ErrRecordedUnfunded above.
func (rp *RecordedProvider) Complete(ctx context.Context, prompt, model string, scope Scope) (Sample, error) {
	key := replayKey(Problem{Statement: prompt, Scope: scope}, model)
	if s, ok := rp.samples[key]; ok {
		return s, nil
	}
	pk := Problem{Statement: prompt, Scope: scope}.Key()
	if rp.gaps[pk] {
		return Sample{}, ErrRecordedGap
	}
	if rp.unfunded[pk] {
		return Sample{}, ErrRecordedUnfunded
	}
	return Sample{}, fmt.Errorf("replay diverged: no recorded sample for %q under %s", prompt, model)
}

// Estimate mirrors the recorded cost so admission control makes the same
// decisions on replay as on the run. The Provider interface gives Estimate no
// scope, so estimates are keyed by (statement, model) only — acceptable because
// an estimate merely sizes admission and is not served as an answer, so it does
// not cross the P6 boundary the way a Complete result would.
func (rp *RecordedProvider) Estimate(prompt, model string) Units {
	return rp.costs[estimateKey(prompt, model)]
}

// replayKey binds a recorded sample to the problem AND model that produced it,
// scope-qualified like the cache key (P6): a replay must not cross the same
// entitlement boundary a live run could not.
func replayKey(p Problem, model string) string {
	return p.Key() + "\x00" + model
}

func estimateKey(statement, model string) string {
	return statement + "\x00" + model
}

var _ Provider = (*RecordedProvider)(nil)

// RecordedReducer replays an internal node's recorded output instead of calling a
// model (§7). The reducer's counterpart to RecordedProvider, and necessary for the
// same reason: once reducing is a model call, its output is stochastic, so a replay
// that re-reduced live would produce different bytes and read as a divergence when
// nothing had diverged (P8).
//
// It cannot be folded into RecordedProvider. A reduce call reaches Complete with the
// merge PROMPT rather than the problem statement, so the provider's replay key —
// (problem, scope, model) — never matches it. Substituting at the Reducer seam keys
// off the thing that is actually stable across a replay: the node's position.
type RecordedReducer struct {
	// byPosition maps (depth, problem key) to the recorded output. Position, not
	// problem alone, for the reason PinPlan gives: a portfolio's arms share their
	// parent's problem key by definition (§2), so key-only indexing would let a leaf
	// arm overwrite the internal node that selected it.
	byPosition map[string]Sample
}

// NewRecordedReducer indexes a record's INTERNAL nodes — those with children. Leaves
// are the provider's job and cache hits performed no reduce.
func NewRecordedReducer(r RunRecord) *RecordedReducer {
	m := make(map[string]Sample, len(r.Outcomes))
	for _, o := range r.Outcomes {
		if len(o.Children) == 0 || o.CacheHit {
			continue
		}
		m[pinKey(o.Depth, o.Problem)] = Sample{
			Content: o.Content,
			Cost:    o.Cost,
			// Model and version are NOT replayed onto the sample: the executor leaves
			// them empty on an internal node (see executor.go), so seeding them here
			// would add bytes the original record does not have and break the very
			// equality this type exists to preserve.
			HaloTokens:      o.HaloTokens,
			GeneratedTokens: o.GeneratedTokens,
		}
	}
	return &RecordedReducer{byPosition: m}
}

// Reduce returns the recorded output for this node's position.
//
// A miss is an ERROR, exactly as it is for RecordedProvider: being asked to reduce a
// node the record does not contain means the replay produced a different tree, which
// is a real signal about the pinned plan and must not be papered over by folding
// children live. The strategy argument is unused for the same reason the content is
// not recomputed — whatever the original reducer did with it is already in the
// recorded output.
func (rr *RecordedReducer) Reduce(_ context.Context, p Problem, children []NodeOutcome, _ Allocation, _ bool, _ Strategy) (Sample, error) {
	// Depth is not a Reduce parameter, but it is recoverable: an internal node sits one
	// level above its children, and every child carries its own depth. That keeps the
	// lookup exact rather than a scan — no ambiguity to resolve by convention.
	depth := 0
	if len(children) > 0 {
		depth = children[0].Depth - 1
	}
	if s, ok := rr.byPosition[pinKey(depth, p)]; ok {
		return s, nil
	}
	return Sample{}, fmt.Errorf("replay diverged: no recorded reduction for %q at depth %d",
		p.Statement, depth)
}

// RecordedSeams are the three substitutions a full replay needs when the planner and
// reducer are model calls. Wiring only some of them means part of the "replay" runs
// live — spending money and producing a tree that will not match.
//
//	e.Planner = seams.Planner
//	e.Solver  = ProviderSolver{Provider: seams.Provider, Model: <the recorded model>}
//	e.Reducer = seams.Reducer
type RecordedSeams struct {
	Planner  PinnedPlanner
	Provider *RecordedProvider
	Reducer  *RecordedReducer
}

// Replayable builds all three seams from one record, so a caller cannot wire a
// partial replay by forgetting one.
func Replayable(r RunRecord) RecordedSeams {
	return RecordedSeams{
		Planner:  PinPlan(r),
		Provider: NewRecordedProvider(r),
		Reducer:  NewRecordedReducer(r),
	}
}

var _ Reducer = (*RecordedReducer)(nil)
