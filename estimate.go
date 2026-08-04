package quarry

import "math"

// Cost estimation (build step 8, §4). EVERYTHING HERE IS ADVISORY (P4). Under P9
// the planner fits its plan to the stated cap, so no number this file produces is
// on the critical path: a bad estimate yields a worse-SCOPED run, not a truncated
// one. Read it as decision support for a researcher choosing a cap, never as a
// component the system depends on. Nothing here may gate a feature (the standing
// P4 rule), and — unlike the ledger — this math is float and is deliberately kept
// OUT of the RunRecord, so it cannot perturb the byte-for-byte replay (P8). The
// record stores measured actuals (the calibration corpus); it never stores a
// projection.
//
// Three estimators, ordered by what they cost to produce:
//
//   1. Structural ceiling — free, always true, wildly pessimistic. Quote it as
//      the CAP, not the estimate.
//   2. Probe — one planner call, ~1/N of the run (probe.go). Yields the depth-1
//      branching factor.
//   3. Branching-process projection — Galton-Watson from the probe's mean
//      offspring m. This file.
//
// The report is THREE NUMBERS, NEVER ONE (§4): P50, P90, and the ceiling. Near
// m = 1 the variance dominates the mean and any single number is theatre — the
// estimate says so via NearUnity rather than quoting a confident point.

// NearUnityBand is the half-width around m = 1 within which the Galton-Watson
// mean is numerically unstable and the variance swamps it (§4). Heuristic, not
// derived; flagged so the UI can say "too close to call" instead of quoting a
// number that is theatre. TODO(§4): calibrate against the corpus once it exists.
const NearUnityBand = 0.1

// z90 is the standard-normal 90th-percentile quantile, used only to read a P90
// off the fitted lognormal below.
const z90 = 1.2815515594

// CostEstimate is the advisory projection (§4). Three numbers, never one.
type CostEstimate struct {
	Ceiling Units // structural upper bound; always true (§4)
	P50     Units // median projected cost under the fitted distribution
	P90     Units // 90th-percentile projected cost — the informative number under skew

	Mean      float64 // mean offspring m from the probe
	Nodes     float64 // expected node count (truncated Galton-Watson)
	Diverges  bool    // m >= 1: the process is critical/supercritical; only the ceiling is trustworthy
	NearUnity bool    // |m-1| < NearUnityBand: variance dominates, treat P50/P90 as theatre (§4)
}

// StructuralCeiling is the free, guaranteed upper bound (§4): with max branching
// b, max depth D and per-node cost c, total <= c·(b^(D+1) − 1)/(b − 1). Depth is
// counted in edges, so a root with no children is D=0 and one node. b <= 1 is the
// degenerate chain: exactly D+1 nodes. Wildly pessimistic and ALWAYS TRUE —
// quote it as the cap.
func StructuralCeiling(b, depth int, perNode Units) Units {
	if b < 0 || depth < 0 || !perNode.Limited() {
		return Unlimited
	}
	if b <= 1 {
		return perNode * Units(depth+1) // a chain: D+1 nodes
	}
	// Geometric sum (b^(D+1) − 1)/(b − 1), in float to avoid int64 overflow on a
	// deep pessimistic tree; the result is advisory so the rounding is immaterial.
	nodes := (math.Pow(float64(b), float64(depth+1)) - 1) / float64(b-1)
	return Units(math.Ceil(nodes * float64(perNode)))
}

// Project runs the Galton-Watson projection from a probe's offspring statistics
// (§4). mean and variance are the offspring distribution's first two moments
// (probe.go derives them from the depth-1 split); depth bounds the recursion;
// perNode is the advisory per-node cost.
//
// Expected node count is the truncated branching-process sum Σ_{k=0}^{D} m^k,
// which converges toward 1/(1−m) for m < 1 and is reported honestly as divergent
// for m >= 1 (§4). P50/P90 come from a lognormal fitted to the projected cost's
// mean and variance — lognormal because cost is positive and right-skewed, so a
// symmetric band would understate the tail that matters. The node-count variance
// is the exact branching-process second moment (see gwStats), not a guess.
//
// Advisory only (P4). When m >= 1 or m is near 1, P50/P90 are still computed but
// the corresponding flags warn that only the ceiling is trustworthy.
func Project(mean, variance float64, depth int, perNode Units) CostEstimate {
	est := CostEstimate{
		Ceiling: StructuralCeiling(int(math.Ceil(mean)), depth, perNode),
		Mean:    mean,
	}
	est.Diverges = mean >= 1
	est.NearUnity = math.Abs(mean-1) < NearUnityBand

	en, vn := gwStats(mean, variance, depth)
	est.Nodes = en

	c := float64(perNode)
	costMean := c * en
	costVar := c * c * vn // per-node cost treated as fixed; output-token spread is a TODO below
	p50, p90 := lognormalQuantiles(costMean, costVar)
	est.P50 = Units(math.Ceil(p50))
	est.P90 = Units(math.Ceil(p90))
	return est
}

// gwStats returns the expected count and variance of the total node population of
// a Galton-Watson process with the given mean and variance offspring, starting
// from one root and truncated at generation depth.
//
// Per-generation moments follow the standard recursion (E[Z_{k+1}] = m·E[Z_k],
// Var[Z_{k+1}] = σ²·E[Z_k] + m²·Var[Z_k]). The total is N = Σ_k Z_k, and its
// variance uses the exact cross-generation covariance Cov(Z_i,Z_j) = m^{j−i}·
// Var[Z_i] for i<j — so this is the true second moment of the model, not an
// independence approximation.
func gwStats(m, sigma2 float64, depth int) (expected, variance float64) {
	ez := make([]float64, depth+1) // E[Z_k]
	vz := make([]float64, depth+1) // Var[Z_k]
	ez[0], vz[0] = 1, 0            // one root, known exactly
	for k := 1; k <= depth; k++ {
		ez[k] = m * ez[k-1]
		vz[k] = sigma2*ez[k-1] + m*m*vz[k-1]
	}
	for k := 0; k <= depth; k++ {
		expected += ez[k]
		variance += vz[k]
	}
	// Positive cross-generation covariances: 2·Σ_{i<j} m^{j-i}·Var[Z_i].
	for i := 0; i <= depth; i++ {
		for j := i + 1; j <= depth; j++ {
			variance += 2 * math.Pow(m, float64(j-i)) * vz[i]
		}
	}
	// TODO(§4): per-node OUTPUT-token cost is itself stochastic (§4 says split the
	// predictable halo from the stochastic generation). This treats per-node cost
	// as fixed, so the variance captures TREE-SHAPE uncertainty only. Folding in
	// per-node output variance needs the corpus and is deferred to calibration.
	return expected, variance
}

// lognormalQuantiles fits a lognormal to a positive quantity with the given mean
// and variance and returns its median (P50) and 90th percentile (P90). Lognormal
// because projected cost is positive and right-skewed: the median sits below the
// mean and P90 carries the tail, which is the number worth quoting (§4). A
// non-positive mean or variance collapses to the mean with no spread.
func lognormalQuantiles(mean, variance float64) (p50, p90 float64) {
	if mean <= 0 {
		return 0, 0
	}
	if variance <= 0 {
		return mean, mean
	}
	s2 := math.Log(1 + variance/(mean*mean)) // shape², grows without bound as variance dominates near m=1
	a := math.Log(mean) - s2/2               // location
	s := math.Sqrt(s2)
	return math.Exp(a), math.Exp(a + z90*s)
}
