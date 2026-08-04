package quarry

import (
	"context"
	"strconv"
)

// Replication and plan pinning (build step 7, §7). A run is an estimate, not an
// answer (P7). Replication is how the estimate gets error bars: independent
// re-derivations of the same problem, their conclusions compared by claim
// equivalence (step 6) to surface where the instrument is unstable (§7). Plan
// pinning is the experimental control that separates two sources of spread —
// planning versus solving — by freezing the decomposition and re-running only
// the leaves.

// Replicate runs the same problem n times as independent fresh draws and returns
// one record per run, ready to feed Stability (§7).
//
// INDEPENDENCE IS THE WHOLE POINT and it is the caller's responsibility to
// preserve. Each run gets its own ledger, but they share the executor's Cache if
// one is set. A serving cache collapses replicates into one stored answer and
// makes every claim look trivially stable — the exact failure §6's "unstable
// nodes are always extended" rule guards against. So for replication the
// executor must DRAW FRESH: leave Cache nil, or set ReadPolicy to return
// ReadExtend. Replicate does not mutate the executor to force this, because
// silently rewriting a caller's config is worse than a documented contract.
//
// The §7 independence ladder (resample → paraphrase → different model family →
// different decomposition) is a property of how the executor's Provider and
// Planner vary between calls, not of this loop. This drives repeated runs; how
// independent they are is configured above it. The strongest rung — an
// independent decomposition — is exactly what PinnedPlanner deliberately
// DESTROYS, which is why pinning is a control, not a replication mode.
//
// Deterministic given a deterministic executor: runs proceed in order and each
// record is assembled the same way (P8). A fault in any run aborts and returns
// it — a replicate that could not complete is not a weaker sample, it is absence
// of one, and averaging over it would understate variance.
func Replicate(ctx context.Context, e *Executor, root Problem, caps Caps, scope Scope, n int) ([]RunRecord, error) {
	records := make([]RunRecord, 0, n)
	for i := 0; i < n; i++ {
		l, err := NewLedger(caps, scope)
		if err != nil {
			return nil, err
		}
		res, err := e.Run(ctx, root, l)
		if err != nil {
			return records, err
		}
		records = append(records, NewRunRecord(res, root, caps, ModeFresh))
	}
	return records, nil
}

// PinnedPlanner replays a prior run's decomposition instead of planning afresh
// (§7 plan pinning). For each problem it returns the children that problem had
// in the pinned record; a problem the record solved as a leaf declines, so it
// solves directly again. Re-running under this planner freezes the tree shape
// and lets any spread be attributed to solving rather than planning — the cheap,
// genuinely useful experimental control §7 asks for.
//
// It pins SHAPE, STRATEGY and APPORTIONMENT — every input to the plan the executor
// acts on. The general hazard, learned twice: any field the pinned planner fails to
// carry becomes a silent difference between the run and its own control, and it
// differs in the direction that still looks like a faithful replay. Strategy was the
// first (a portfolio came back as a partition and collapsed to a third of the work);
// weight was the second (the shape was right and the money was split evenly across
// it, so a spread could come from the re-apportionment rather than from the solving
// pinning exists to isolate).
//
// What it still does NOT control is the SOLVER: children are re-solved live, which is
// the entire point — pinning holds the plan fixed so variance is attributable to
// solving. It also does not pin ExpectLeaf or Rationale, and deliberately: neither
// reaches Apportion or the tree shape, so neither can change what the re-run does.
type PinnedPlanner struct {
	// nodes maps a (depth, problem key) to the plan recorded at that position. A key
	// present with no items was a recorded leaf and declines.
	nodes map[string]pinnedNode
}

// pinnedNode is one recorded decomposition: its children, its shape, and how the
// budget divided across it.
//
// The strategy is carried, not re-derived. Without it a pinned portfolio comes back
// as a partition, and dedupePlan then collapses N identical arms into one child —
// a re-run doing a third of the work while reporting a faithful shape replay. A
// control that silently degrades what it controls for is worse than no control.
//
// The weights are carried for the same reason one level down: Apportion turns them
// into money (§3), so a plan restated with uniform weights funds the same children
// differently. Weights are stored PARALLEL to children rather than on a combined
// struct because they are recovered from a different place — the children's own
// PlanWeight fields, not the parent's outcome.
type pinnedNode struct {
	children []Problem
	weights  []int64
	strategy Strategy
}

// PinPlan builds a PinnedPlanner from a completed run's record. It indexes every
// internal node's problem to its recorded children, resolved through the node-ID
// map so the child PROBLEMS (not just IDs) are recovered for re-planning.
//
// Keyed by (DEPTH, problem key). The problem key alone is consistent with the DAG
// model (§2) — an identical sub-problem resolves the same way wherever it appears —
// but it cannot represent a node whose CHILD shares its key, and a portfolio's arms
// share their parent's key BY DEFINITION (§2: N attempts at the same problem). Under
// key-only indexing the arms, being leaves, overwrote their parent's entry and the
// portfolio vanished on pin. Depth separates them: the parent sits at d, its arms at
// d+1, so both survive.
//
// This subsumes the self-similarity ambiguity previously marked TODO here (a problem
// that was both an internal node and a leaf in one record). It remains a heuristic
// rather than exact positional identity: two nodes with the same problem at the same
// depth still collapse, which for a partition is the DAG behaviour anyway.
func PinPlan(r RunRecord) PinnedPlanner {
	byID := make(map[string]NodeOutcome, len(r.Outcomes))
	for _, o := range r.Outcomes {
		byID[o.NodeID] = o
	}
	nodes := make(map[string]pinnedNode, len(r.Outcomes))
	for _, o := range r.Outcomes {
		kids := make([]Problem, 0, len(o.Children))
		weights := make([]int64, 0, len(o.Children))
		for _, cid := range o.Children {
			if c, ok := byID[cid]; ok {
				kids = append(kids, c.Problem)
				// The weight is read off the CHILD, which recorded what funded it, not
				// off this parent — a plan's weights are only observable through the
				// nodes they paid for.
				weights = append(weights, c.PlanWeight)
			}
		}
		nodes[pinKey(o.Depth, o.Problem)] = pinnedNode{
			children: kids,
			weights:  weights,
			strategy: o.Strategy,
		}
	}
	return PinnedPlanner{nodes: nodes}
}

// pinKey identifies a recorded position by depth and problem. See PinPlan on why
// depth is part of it.
func pinKey(depth int, p Problem) string {
	return strconv.Itoa(depth) + "\x00" + p.Key()
}

// Plan returns the pinned decomposition for p at this depth: exactly the recorded
// children, under the recorded strategy, with the recorded weights. A recorded leaf,
// or a position the record never saw, declines so the executor solves it directly —
// pinning never invents a split the original did not make.
func (pp PinnedPlanner) Plan(_ context.Context, p Problem, _ Allocation, depth int, _ []NodeOutcome) (Plan, error) {
	n, ok := pp.nodes[pinKey(depth, p)]
	if !ok || len(n.children) == 0 {
		return Plan{Declined: true, Reasoning: "pinned: recorded as a leaf"}, nil
	}
	weights, weighted := n.pinnedWeights()
	items := make([]PlanItem, len(n.children))
	for i, k := range n.children {
		items[i] = PlanItem{Problem: k, Weight: weights[i]}
	}
	reasoning := "pinned decomposition (§7)"
	if !weighted {
		// Said out loud in the record, because it is the difference between a full
		// control and a shape-only one, and a reader comparing two runs needs to know
		// which they are holding.
		reasoning += " — weights unrecorded, apportioned uniformly"
	}
	return Plan{Items: items, Strategy: n.strategy, Reasoning: reasoning}, nil
}

// pinnedWeights returns the weights to re-plan with, and whether they are the
// recorded ones.
//
// Zero means UNRECORDED (a plan item's weight is always positive), so a record
// written before weights existed yields all zeros and falls back to uniform — the
// previous behaviour, retained deliberately so an old record still pins its shape
// rather than failing.
//
// The fallback is ALL-OR-NOTHING per node. Substituting 1 for only the missing
// entries would silently change the ratio between the recorded ones — a child weighted
// 5 against a substituted 1 gets a share the original plan never assigned it — and a
// wrong apportionment presented as a pinned one is worse than an honest uniform split.
func (n pinnedNode) pinnedWeights() (weights []int64, recorded bool) {
	recorded = len(n.weights) == len(n.children)
	for _, w := range n.weights {
		if w <= 0 {
			recorded = false
			break
		}
	}
	if recorded {
		return n.weights, true
	}
	uniform := make([]int64, len(n.children))
	for i := range uniform {
		uniform[i] = 1
	}
	return uniform, false
}

var _ Planner = PinnedPlanner{}
