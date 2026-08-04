package quarry

import (
	"context"
	"strings"
)

// Reference Reducer for build step 2.
//
// The production Reducer is a distinct LLM agent from the Planner (§2): it must
// see what returned without inheriting the priors that produced the split. This
// deterministic double lets the executor run with no model.
//
// The contract that matters even for the double is PARTIAL TOLERANCE (§3.1).
// Budget exhaustion lets you stop spending; a deadline does not let you return
// later. Whatever children exist must reduce into a returnable answer at any
// moment, degrading in quality rather than sitting in an unreturnable
// intermediate state. So Reduce never fails on missing children — it folds what
// it has and the caller records the gaps.

// ConcatReducer joins child content in stable order.
//
// Deterministic and model-free, so it underwrites the replay determinism test
// (step 5) as well as the depth-1 fanout test here. It carries no cost of its
// own — the real reducer's cost is what the parent's reserve exists to fund.
type ConcatReducer struct {
	Sep string
}

// Reduce joins child content in stable order, and DELEGATES portfolios to
// SelectReducer rather than concatenating them — see the body.
//
// It tolerates partial input, as every reducer must: you can stop spending, you
// cannot stop time (§3.1). A missing child is skipped, not an error.
func (cr ConcatReducer) Reduce(ctx context.Context, p Problem, children []NodeOutcome, alloc Allocation, partial bool, strategy Strategy) (Sample, error) {
	// A portfolio's arms are competing attempts at ONE problem, so joining them
	// would return N answers where the caller asked for one (§2). Delegate rather
	// than concatenate: being the default reducer, silently mangling every portfolio
	// is the likeliest way this shape would appear to "work" while being useless.
	if strategy == StrategyPortfolio {
		return SelectReducer{}.Reduce(ctx, p, children, alloc, partial, strategy)
	}
	sep := cr.Sep
	if sep == "" {
		sep = "\n"
	}
	parts := make([]string, 0, len(children))
	for _, c := range children {
		// A gapped child contributes nothing but must not abort the merge — the
		// answer degrades, it does not disappear (§3.1).
		if c.Gap || c.Content == "" {
			continue
		}
		parts = append(parts, c.Content)
	}
	return Sample{Content: strings.Join(parts, sep)}, nil
}

// SelectReducer picks ONE arm of a portfolio (§2). The model-free counterpart to
// ConcatReducer: where that one merges a partition, this one selects among
// competing attempts, which is the operation a portfolio actually needs.
//
// The selection rule is deliberately MECHANICAL and weak — first verified arm,
// else first arm with content — for the same reason the mechanical oracles exist:
// it costs nothing, it is deterministic (so it underwrites replay, P8), and it
// makes the shape testable with no LLM. Real selection is a model judgement and
// belongs in provider/; §2's premise for the whole strategy is that selection is
// EASIER than generation, not that it is free.
//
// Preferring a verified arm is the one piece of real judgement available for free,
// and it is the right one: a portfolio whose arms are individually verified turns
// selection into "take one that passed", which is precisely the case §2 says
// portfolio is strictly better for.
type SelectReducer struct{}

// Reduce picks one arm: a verified one if any arm was checked and passed, else the
// first usable one. Deterministic in both cases, which is what lets a portfolio replay
// (P8). Costs nothing — see the type doc on why free selection is the honest default.
func (SelectReducer) Reduce(_ context.Context, _ Problem, children []NodeOutcome, _ Allocation, _ bool, _ Strategy) (Sample, error) {
	// First pass: an arm that was checked AND passed. Scanning in order keeps the
	// choice deterministic across replays.
	for _, c := range children {
		if c.Gap || c.Content == "" {
			continue
		}
		if c.Verified != nil && *c.Verified {
			return selected(c), nil
		}
	}
	// Second pass: any arm that produced content. An UNVERIFIED arm is not the same
	// as a failed one (§8), so a portfolio with no verifier still returns an answer
	// rather than nothing — it just cannot claim the answer was checked.
	for _, c := range children {
		if c.Gap || c.Content == "" {
			continue
		}
		return selected(c), nil
	}
	// Every arm gapped or came back empty. Returning an empty sample rather than an
	// error is partial tolerance (§3.1): the parent records the gap, and a run that
	// produced nothing must still be returnable and citable.
	return Sample{}, nil
}

// selected carries the chosen arm's content forward WITHOUT its cost or tokens.
// The arms already recorded their own spend as separate nodes; re-reporting the
// winner's here would double-count it in the tree total, exactly as rolling up a
// subtree's tokens into its parent would (see executor.go's internal-node note).
//
// It deliberately does NOT carry the arm's Verified verdict either: that verdict
// belongs to the arm's own node, and copying it onto the selection would let the
// parent claim a check it never performed.
func selected(c NodeOutcome) Sample {
	return Sample{Content: c.Content}
}

var (
	_ Reducer = ConcatReducer{}
	_ Reducer = SelectReducer{}
)
