package quarry

import (
	"context"
	"math"
)

// The probe (build step 8, §4). One planner call — ~1/N of a full run — is the
// highest information-per-dollar action available: it yields the measured
// depth-1 branching factor that feeds the Galton-Watson projection (estimate.go),
// and, sampled k times, it doubles as the plan-variance diagnostic below. Paid
// for once, it serves both §4 (estimation) and §7 (a divergence signal).
//
// Advisory only (P4): the probe informs a cap choice, it does not gate anything.
// It DOES spend — it is a real planner call — so a caller running it is choosing
// to pay ~1/N up front for a better-scoped run. That is the one deliberate cost
// in this otherwise free section, and it is the researcher's to opt into.

// Probe runs only the top-level planner and reads the offspring distribution off
// its plan (§4). It does NOT fan out — no child is solved — so it costs one
// planner call, not a run.
//
// Offspring per child: a child tagged ExpectLeaf contributes 0 (it will not
// recurse); any other child contributes 1 (it is expected to split further). The
// mean of that indicator over the plan's children, scaled by the branching count,
// is the depth-1 branching factor m. A declined plan has m = 0 — the problem does
// not decompose, which the projection reads as a single node.
//
// TODO(§4): §4 asks the planner to tag each child with a DIFFICULTY SCORE, from
// which a real offspring distribution would be read. PlanItem carries only
// ExpectLeaf today, so this uses the leaf/recurse indicator as a coarse proxy.
// Enriching PlanItem with a difficulty score is the fix; it changes this function
// and nothing downstream, since Project consumes only (mean, variance).
func Probe(ctx context.Context, planner Planner, p Problem, alloc Allocation) (mean, variance float64, plan Plan, err error) {
	plan, err = planner.Plan(ctx, p, alloc, 0, nil)
	if err != nil {
		return 0, 0, Plan{}, err
	}
	m, v := offspringMoments(plan)
	return m, v, plan, nil
}

// PlanMoments reads a plan's offspring moments without making a call — Probe's second
// half, separated for the plan gate (#15 D5).
//
// THE GATE ALREADY HOLDS A PLAN. Probe couples the call and the arithmetic, which is
// right for §4's "pay ~1/N up front" framing, but `quarry plan` has paid for its plan
// already and calling Probe would buy a SECOND one — spending twice for a projection
// that is advisory either way (P4), and projecting a plan other than the one being
// approved.
func PlanMoments(plan Plan) (mean, variance float64) { return offspringMoments(plan) }

// offspringMoments reads the mean and variance of the offspring count off a
// single plan. Each recursing child (not ExpectLeaf) is one unit of offspring; a
// leaf child is zero. The plan's total offspring is the count of recursing
// children — this is the m the branching process propagates. Variance is the
// spread of the per-child indicator scaled to that population, a coarse but
// honest measure of how uneven the split is.
func offspringMoments(plan Plan) (mean, variance float64) {
	if plan.Declined || len(plan.Items) == 0 {
		return 0, 0
	}
	// m = expected number of children that themselves recurse. A plan that fans
	// out to k children all of which recurse has m = k; all leaves gives m = 0.
	var recursing float64
	for _, it := range plan.Items {
		if !it.ExpectLeaf {
			recursing++
		}
	}
	mean = recursing
	// Variance of the offspring across this generation's children: treat each
	// child's contribution to the next generation as recursing (1) or not (0) and
	// take the population variance, scaled by the number of children so it sizes
	// with the fanout rather than staying a bare fraction.
	n := float64(len(plan.Items))
	pRec := recursing / n
	variance = n * pRec * (1 - pRec) // Binomial(n, pRec) variance: spread of the recursing-child count
	return mean, variance
}

// PlanVariance is the plan-variance diagnostic (§4): sample the planner k times
// and measure how much the plans disagree. High divergence means two things at
// once — the estimate is unreliable AND the problem is underspecified — both of
// which the researcher wants to know BEFORE spending. Same probe, paid once,
// also serving §7's independent-decomposition signal.
type PlanVariance struct {
	K              int     // plans sampled
	MeanM          float64 // average offspring mean across the k plans
	StdevM         float64 // spread of the offspring mean — the divergence measure
	ItemCounts     []int   // per-sample child count, for the researcher to eyeball
	Underspecified bool    // StdevM exceeds the instability threshold
}

// UnstableThreshold is the offspring-mean stdev above which plans are judged to
// disagree enough that a single estimate is theatre. Heuristic; TODO(§4):
// calibrate against the corpus.
const UnstableThreshold = 0.5

// SamplePlans runs the planner k times and reports how much its plans diverge
// (§4). k ~ 5 is the suggested sample. Each call is an independent planner sample
// — for the divergence to be real the planner must be stochastic; a deterministic
// planner (the no-LLM doubles) reports zero divergence, which is correct: it has
// none.
//
// Advisory (P4). Spends k planner calls; like Probe, that is a deliberate,
// opt-in cost paid up front for a better-scoped run and an underspecification
// warning.
func SamplePlans(ctx context.Context, planner Planner, p Problem, alloc Allocation, k int) (PlanVariance, error) {
	if k < 1 {
		k = 1
	}
	ms := make([]float64, 0, k)
	counts := make([]int, 0, k)
	for i := 0; i < k; i++ {
		plan, err := planner.Plan(ctx, p, alloc, 0, nil)
		if err != nil {
			return PlanVariance{}, err
		}
		m, _ := offspringMoments(plan)
		ms = append(ms, m)
		counts = append(counts, len(plan.Items))
	}
	meanM, stdevM := meanStdev(ms)
	return PlanVariance{
		K:              k,
		MeanM:          meanM,
		StdevM:         stdevM,
		ItemCounts:     counts,
		Underspecified: stdevM > UnstableThreshold,
	}, nil
}

// meanStdev returns the mean and population standard deviation of xs.
func meanStdev(xs []float64) (mean, stdev float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return mean, math.Sqrt(ss / float64(len(xs)))
}
