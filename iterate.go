package quarry

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

var (
	// ErrNothingToExtend is returned when a completed record is offered to Extend
	// (§8.1). A completed-but-coarse run is a refine; extending it would pin its
	// decomposition and pay for a copy of itself.
	ErrNothingToExtend = errors.New("nothing to extend")

	// ErrCapNotRaised is returned when an iteration's cap does not exceed its
	// predecessor's in the denomination that bound it (§8.1).
	ErrCapNotRaised = errors.New("cap not raised above the prior run's")
)

// Iteration: extend and refine (§8.1). A first run is usually a first stab, and
// because sub-problems are memoized (§6) a researcher can come back with more
// budget and pay only the DELTA — which is the economic justification for the
// cache's complexity.
//
// The two operations are not variants of each other. They answer different
// questions about what went wrong, and the run record already says which applies:
//
//	extend  the tree was TRUNCATED — the plan was sound, the money ran out.
//	        Same decomposition, more budget, fill the gaps.
//	refine  the tree COMPLETED but is too coarse — the PLAN was the problem.
//	        Re-plan from scratch, with the prior supplied as evidence.
//
// Getting that backwards is the expensive mistake in both directions. Extending a
// completed-but-coarse tree spends new budget deepening a decomposition that was
// split in the wrong places; refining a truncated one throws away a sound plan and
// re-derives it, paying twice for the same shape while the actual gaps — which were
// never about the plan — may well recur.
//
// WHY EXTEND IS THE CHEAP HALF. It is nearly free to build because it is composed
// of parts that already exist for other reasons: PinnedPlanner (§7) is exactly
// "the prior decomposition", and the cache's serve path is exactly "completed
// subtrees cost nothing". Extend is those two facts pointed at a bigger cap. That
// is also why its correctness rests on the cache never storing an incomplete
// answer — see appendCache. If a truncated node's empty merge were cacheable, the
// node most needing a re-solve would be the one most confidently served, and
// extend would reliably refill nothing.

// Iteration is a follow-up run derived from a predecessor (§8.1).
//
// Holds the seams the caller must wire and the lineage the record must carry.
// Returned by Extend and Refine rather than executed by them: neither operation
// runs anything, because what to run against — which models, which providers, what
// verification — is the caller's decision and not derivable from the prior record.
// Both operations are pure functions of a record, which also keeps them testable
// with no provider at all.
type Iteration struct {
	// Mode is ModeExtend or ModeRefine, and it goes into the record so the receipt
	// names which operation produced it (§8).
	Mode Mode

	// Problem is the root problem, carried from the predecessor unchanged. An
	// iteration is a further attempt at the SAME question — a different question is a
	// fresh run, not an iteration, and would break the lineage's meaning.
	Problem Problem

	// Caps is the new ceiling. Strictly greater than the predecessor's in the
	// denomination that bound it, or the iteration cannot do anything the prior run
	// could not (see Extend).
	Caps Caps

	// Planner is what the follow-up run must plan with.
	//
	// On EXTEND this is a PinnedPlanner holding the prior decomposition: the plan was
	// sound, so it is reused rather than re-derived, and re-planning would risk a
	// different shape that could not serve the prior run's completed subtrees from
	// cache — losing the delta pricing that is the entire point.
	//
	// On REFINE this is nil, and deliberately so: refine RE-PLANS, so the caller
	// supplies a live planner and passes Prior to it. A pinned planner here would be
	// the exact failure §8.1 names — spending new budget on a decomposition already
	// judged to be split in the wrong places.
	Planner *PinnedPlanner

	// Prior is the distilled predecessor for a refine's planner (§8.1), empty on
	// extend (which re-plans nothing and therefore needs no evidence). See Distill
	// for what is included and what is withheld.
	Prior []NodeOutcome

	// ParentRun and LineOfInquiry are the lineage (§8.1). ParentRun is the
	// predecessor's content hash, so a citable artifact may be a chain of records
	// rather than a single one (P8). LineOfInquiry is the accounting handle: a PI
	// cares what a QUESTION cost, not what an invocation cost.
	ParentRun     string
	LineOfInquiry string
}

// Record assembles the follow-up run's record with its lineage attached.
//
// Use this instead of NewRunRecord for an iteration: NewRunRecord cannot set
// ParentRun or LineOfInquiry, so a record built with it is indistinguishable from
// a fresh run — the chain silently breaks at exactly the point it was supposed to
// prove continuity. The hash is computed after the lineage fields are set, so the
// predecessor's identity is inside the successor's identity.
func (it Iteration) Record(res Result) RunRecord {
	r := RunRecord{
		Problem:       it.Problem,
		Caps:          it.Caps,
		Mode:          it.Mode,
		Outcomes:      res.Outcomes,
		BoundBy:       res.BoundBy,
		Unverified:    unverified(res.Outcomes),
		Adversarial:   res.Adversarial,
		ParentRun:     it.ParentRun,
		LineOfInquiry: it.LineOfInquiry,
	}
	r.RunID = contentHash(r)
	return r
}

// Extend prepares a follow-up run over the SAME decomposition with a larger cap
// (§8.1): the plan was sound and the money ran out, so refill the gaps and serve
// the completed subtrees from cache at no cost.
//
// THE CALLER MUST SUPPLY THE PRIOR RUN'S CACHE. Extend cannot check this — the
// cache is on the Executor, not in the record — and without it every completed
// subtree is re-solved at full price, which turns the delta into the total while
// still producing a correct answer. That failure is silent and shows up only as a
// bill, so it is stated here and asserted in the tests rather than left to be
// discovered.
//
// Refuses a record that was not truncated. Extending a completed run would pin its
// decomposition and hand it more money to do nothing with: every node serves from
// cache, nothing refills, and the result is a paid-for copy of the predecessor.
// A completed-but-unsatisfying run is a REFINE, and returning an error here rather
// than an empty iteration is what makes the extend/refine choice explicit at the
// call site instead of a guess.
//
// Refuses a cap that is not larger, for the same reason: an extend at the same
// ceiling re-derives the same truncation. Compared only in the denomination that
// actually bound the prior run (§8.2's BoundBy), because raising the OTHER one
// changes nothing — more money will not refill a node that ran out of time.
func Extend(prior RunRecord, caps Caps) (Iteration, error) {
	if len(prior.Gaps()) == 0 && !prior.Truncated() {
		return Iteration{}, fmt.Errorf("%w: the prior run completed, so there is nothing to fill — "+
			"a coarse-but-complete result is a refine", ErrNothingToExtend)
	}
	if err := capsAllowExtend(prior, caps); err != nil {
		return Iteration{}, err
	}
	planner := PinPlan(prior)
	return Iteration{
		Mode:    ModeExtend,
		Problem: prior.Problem,
		Caps:    caps,
		Planner: &planner,
		// No Prior: extend re-plans nothing, so there is no planner to inform. Passing
		// the distilled prior anyway would be inert at best and, if some future planner
		// read it, would mean an "extend" had quietly started re-planning.
		ParentRun:     prior.RunID,
		LineOfInquiry: lineOf(prior),
	}, nil
}

// capsAllowExtend checks the new ceiling exceeds the old one where it matters.
//
// BoundBy names the denomination that actually bit (§8.2). When it is empty — the
// prior run has gaps but neither cap was reported as binding — any increase is
// accepted: the record is telling us it does not know what bound the run, and
// refusing on that basis would block a legitimate extend on missing telemetry.
func capsAllowExtend(prior RunRecord, caps Caps) error {
	switch prior.BoundBy {
	case DenomSpend:
		if !caps.Spend.Limited() || caps.Spend > prior.Caps.Spend {
			return nil
		}
		return fmt.Errorf("%w: prior run was bound by spend at %s and the new cap is %s",
			ErrCapNotRaised, prior.Caps.Spend, caps.Spend)
	case DenomLatency:
		if caps.Latency <= 0 || caps.Latency > prior.Caps.Latency {
			return nil
		}
		return fmt.Errorf("%w: prior run was bound by latency at %s and the new cap is %s",
			ErrCapNotRaised, prior.Caps.Latency, caps.Latency)
	}
	// Unknown binding: accept any raise in either denomination, refuse a plain repeat.
	if capsExceed(caps, prior.Caps) {
		return nil
	}
	return fmt.Errorf("%w: neither cap exceeds the prior run's (%s / %s)",
		ErrCapNotRaised, prior.Caps.Spend, prior.Caps.Latency)
}

// capsExceed reports whether caps is larger in at least one denomination, treating
// an unlimited value as larger than any limited one.
func capsExceed(caps, prior Caps) bool {
	if !caps.Spend.Limited() && prior.Spend.Limited() {
		return true
	}
	if caps.Spend.Limited() && prior.Spend.Limited() && caps.Spend > prior.Spend {
		return true
	}
	if caps.Latency <= 0 && prior.Latency > 0 {
		return true
	}
	return caps.Latency > prior.Latency
}

// Refine prepares a follow-up run that RE-PLANS the problem, informed by what the
// predecessor measured (§8.1). Use it when the tree completed but the result is
// too coarse: the plan was the problem, so a bigger budget spent on the same plan
// would deepen a decomposition split in the wrong places.
//
// Returns an Iteration with Planner NIL and Prior populated. The caller wires its
// own live planner and passes Prior to Plan's prior argument. That asymmetry with
// Extend is the whole distinction between the two operations, made structural: an
// extend carries a planner and no evidence, a refine carries evidence and no
// planner.
//
// unstable is the cross-replicate instability from Stability (§7), and may be nil.
// It is the highest-value targeting signal in the system — the first run measures
// where the instrument is at its limit and the second spends precisely there — but
// it requires REPLICATES, which a single run does not have. Nil is therefore the
// honest input for a refine after one run, not a degraded one, and Distill records
// the difference rather than presenting an unmeasured claim as a stable one.
//
// Unlike Extend this refuses nothing. A truncated run is a strange thing to refine
// — its plan was never given a fair test — but it is not incoherent: a researcher
// who looks at a partial result and concludes the decomposition is wrong is making
// a legitimate judgement, and it is not this function's place to overrule it. The
// asymmetry is deliberate: Extend's refusals catch cases where the operation would
// PROVABLY do nothing (a pinned plan that all serves from cache), whereas a refine
// always does real work.
func Refine(prior RunRecord, caps Caps, unstable []ClaimStability) Iteration {
	return Iteration{
		Mode:          ModeRefine,
		Problem:       prior.Problem,
		Caps:          caps,
		Planner:       nil, // refine RE-PLANS; a pinned planner here would defeat the operation
		Prior:         Distill(prior, unstable),
		ParentRun:     prior.RunID,
		LineOfInquiry: lineOf(prior),
	}
}

// Distill reduces a run record to what a re-planning planner should see (§8.1).
//
// NOT the prior transcript: that is the halo problem (P1) and is expensive — the
// planner would pay in context for content it is not being asked to reproduce. The
// distillate is the four signals §8.1 names:
//
//   - Actual difficulty versus the node's own prior weight. A direct correction on
//     the planner's weighting (§2), operating within one research question rather
//     than across the calibration corpus (§4). This is what NodeOutcome.PlanWeight
//     exists for: without it "expensive relative to what it was expected to cost"
//     is not computable, only "expensive".
//   - Which claims came back unstable (§7), from the caller's replicate comparison.
//   - Which nodes failed verification, and where the regress stopped at a stated
//     residual risk.
//   - Any gaps from truncation.
//
// THE TREE SHAPE IS WITHHELD, which is the unresolved §8.1/§12 anchoring tension
// handled by declining to guess. Showing the planner the prior decomposition biases
// it toward that decomposition, and §7 names an INDEPENDENT decomposition as the
// strongest replication signal available — so a refine that anchors on the prior
// plan would quietly destroy the best evidence the system has, in the name of
// improving it. Concretely, withholding means: no Children, no NodeID, no Depth,
// no Strategy. What survives is per-node evidence about DIFFICULTY and RELIABILITY,
// which is what §8.1 asks the planner to correct on, and which does not describe a
// shape.
//
// The cost of that choice, stated rather than hidden: the planner cannot tell that
// two distilled nodes were siblings, so it cannot learn "this split was wrong",
// only "this sub-problem was expensive and unreliable". That is strictly less
// information than §8.1's ideal, and it is the price of not biasing the shape. The
// alternative §8.1 floats — sample plans both with and without the prior and
// compare — needs a planner-level experiment this function has no way to run, and
// nothing here forecloses it: a caller wanting the anchored arm can build the prior
// itself from the record, which is public.
//
// Deterministic: nodes in record order, then sorted by a total order on the
// distilled fields alone. Replay must be byte-stable (P8), and a map iteration or
// a cost-only sort with ties would not be.
func Distill(prior RunRecord, unstable []ClaimStability) []NodeOutcome {
	// Index instability by the node that asserted the claim, so a node's own
	// unreliability travels with it rather than as a detached global list.
	unstableByNode := make(map[string][]Claim, len(unstable))
	for _, cs := range unstable {
		unstableByNode[cs.Claim.NodeID] = append(unstableByNode[cs.Claim.NodeID], cs.Claim)
	}

	out := make([]NodeOutcome, 0, len(prior.Outcomes))
	for _, o := range prior.Outcomes {
		// A node is worth showing the planner only if it carries a signal: it cost
		// something measurable against its weight, it failed a check, it was unstable, or
		// it was a gap. A node that behaved exactly as planned teaches nothing and would
		// only add halo.
		failed := o.Verified != nil && !*o.Verified
		unstableHere := unstableByNode[o.NodeID]
		if o.Cost == 0 && !failed && !o.Gap && len(unstableHere) == 0 {
			continue
		}

		d := NodeOutcome{
			// The PROBLEM and what it cost against what it was expected to cost. These
			// three fields are the difficulty correction, and they are all of it.
			Problem:    o.Problem,
			Cost:       o.Cost,
			PlanWeight: o.PlanWeight,

			// Reliability: what was checked and what it said, whether the node was
			// truncated, and how many re-solves it took. Retries are difficulty evidence
			// too — a node that needed three attempts was harder than its cost alone shows.
			Verified: o.Verified,
			Gap:      o.Gap,
			Retries:  o.Retries,

			// The halo split, so the planner can see surface-to-volume (P1): a node that
			// paid for a large context and produced little is direct evidence the split was
			// not worth making — which is a claim about THIS node, not about the shape.
			HaloTokens:      o.HaloTokens,
			GeneratedTokens: o.GeneratedTokens,

			// Children, NodeID, Depth and Strategy are all deliberately ABSENT. See the
			// anchoring note above; together they are the tree shape.
		}
		// Only the unstable claims, not every claim. The stable ones are the part of the
		// prior a refine does NOT need to revisit, and including them would spend halo
		// re-describing settled ground.
		if len(unstableHere) > 0 {
			d.Claims = append([]Claim(nil), unstableHere...)
		}
		out = append(out, d)
	}

	// A total order over the distilled fields, so the same record always distills to
	// the same bytes (P8). Cost descending puts the expensive nodes first, which is
	// the order a budget-conditioned planner (P9) should read them in; the statement
	// breaks ties, because two nodes of equal cost must not depend on map or slice
	// happenstance for their order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Problem.Key() < out[j].Problem.Key()
	})
	return out
}

// lineOf returns the line of inquiry a record belongs to, starting one at the
// record's own identity when it has none (§8.1).
//
// A first run has no line because nothing has iterated on it yet; the first
// iteration is what creates one, and naming it after the ROOT run means the whole
// chain shares a stable handle no matter how long it grows. Cumulative spend is
// then a sum over records carrying the same handle — which is what a PI is asking
// for when they ask what a question cost.
func lineOf(prior RunRecord) string {
	if prior.LineOfInquiry != "" {
		return prior.LineOfInquiry
	}
	return prior.RunID
}

// LineCost sums spend across every record in a line of inquiry (§8.1): the
// cumulative cost of a QUESTION rather than of an invocation.
//
// Records not belonging to the line are ignored rather than rejected, so a caller
// can pass a whole store. Cache hits contribute zero by construction — the tokens
// were paid for once, in the run that produced them, and the second run's receipt
// says so — which is exactly the delta pricing that justifies the cache (§8.1).
func LineCost(line string, records []RunRecord) (total Units, n int) {
	for _, r := range records {
		if lineOf(r) != line {
			continue
		}
		total += r.TotalCost()
		n++
	}
	return total, n
}

// Extendable reports whether a record is a candidate for extend rather than
// refine (§8.1): it was truncated, so its plan never got a fair test.
//
// Advisory. The record says which operation APPLIES; it cannot say which the
// researcher wants, and a truncated run whose plan is also wrong is a legitimate
// refine. Extend enforces the hard half of this (it refuses a completed record);
// this is for a caller that wants to ask before being refused.
func Extendable(r RunRecord) bool { return len(r.Gaps()) > 0 || r.Truncated() }

// runIteration is a convenience for the common case: wire the iteration's planner
// onto an executor, run it, and assemble the lineage-carrying record.
//
// Deliberately unexported and narrow. It only handles EXTEND, because extend is
// the operation whose planner is fully determined by the record — a refine needs a
// live planner and a prior wired by the caller, and a helper that accepted one
// would have to choose how to pass the prior, which is the anchoring question this
// file declines to answer.
func runIteration(ctx context.Context, e *Executor, it Iteration, l *Ledger) (RunRecord, error) {
	if it.Planner == nil {
		return RunRecord{}, fmt.Errorf("iteration in mode %q carries no planner; "+
			"a refine must be run with the caller's own planner and Prior", it.Mode)
	}
	e.Planner = *it.Planner
	res, err := e.Run(ctx, it.Problem, l)
	if err != nil {
		return RunRecord{}, err
	}
	return it.Record(res), nil
}
