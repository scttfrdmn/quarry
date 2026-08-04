package quarry

import (
	"sort"
	"sync"
)

// Calibration corpus (build step 8, §4). Every run deposits
// (problem shape, plan shape) -> ACTUAL spend, keyed by the same content hash the
// cache uses. Nearest-neighbour over accumulated runs beats any a-priori model
// quickly — the SLURM-walltime situation: user estimates are poor, historical
// per-application data is decent, so prefer the latter and allow override (§4).
//
// This is where §4's advisory estimate earns its keep over time: the a-priori
// Galton-Watson projection (estimate.go) is what you quote on a COLD problem;
// once similar problems have run, their measured actuals are a better predictor.
// Both stay advisory (P4) — nothing gates on either.
//
// The corpus stores MEASURED ACTUALS ONLY, never projections: it deposits what a
// run truly cost, read from the RunRecord (P8's measured half). It also keys on
// SHAPE AND METRICS, NEVER CONTENT, and carries scope in the key exactly as the
// cache does (P6) — otherwise cross-tenant learning leaks scoped material (§8.2
// guardrail). The unit of learning is the problem CLASS, not the run (§8.2).

// CalibrationSample is one deposited observation: a problem class and the plan
// shape that ran under it, with the actual spend it incurred.
type CalibrationSample struct {
	ProblemKey string // scope-qualified content hash (P6) — the class key, not content
	Fanout     int    // top-level plan breadth (plan shape)
	Depth      int    // realized max depth
	Nodes      int    // realized node count
	Actual     Units  // MEASURED total spend, never a projection
}

// Calibrator accumulates calibration samples and answers nearest-neighbour cost
// queries (§4). In-memory reference implementation; the deployed corpus is the
// same telemetry store §4 and §8.2 share, and implements the same query.
//
// Safe for concurrent Deposit — run records land from separate goroutines like
// telemetry (§8.2).
type Calibrator struct {
	mu      sync.RWMutex
	samples []CalibrationSample
}

// NewCalibrator builds an empty corpus.
func NewCalibrator() *Calibrator { return &Calibrator{} }

// Deposit records a completed run's measured shape and actual spend (§4). It
// reads the actuals straight off the record — the corpus never stores what a run
// was projected to cost, only what it did. A run with no outcomes deposits
// nothing.
func (c *Calibrator) Deposit(r RunRecord) {
	if len(r.Outcomes) == 0 {
		return
	}
	s := CalibrationSample{
		ProblemKey: r.Problem.Key(), // scope-qualified (P6)
		Fanout:     len(r.Outcomes[0].Children),
		Depth:      maxDepthOf(r.Outcomes),
		Nodes:      len(r.Outcomes),
		Actual:     r.TotalCost(),
	}
	c.mu.Lock()
	c.samples = append(c.samples, s)
	c.mu.Unlock()
}

// EstimateFor answers a cost query for a problem from accumulated actuals (§4).
// It prefers exact-class matches — samples sharing the scope-qualified key — and
// returns their median actual, the SLURM-walltime "historical per-application
// data" that beats an a-priori model. ok=false when the corpus has no matching
// class, which is the signal to fall back to the Galton-Watson projection
// (estimate.go) for a cold problem.
//
// Median, not mean: run cost is right-skewed (a few expensive tails), so the
// median is the honest central estimate and does not chase outliers.
func (c *Calibrator) EstimateFor(p Problem) (median Units, n int, ok bool) {
	key := p.Key()
	c.mu.RLock()
	defer c.mu.RUnlock()
	var actuals []Units
	for _, s := range c.samples {
		if s.ProblemKey == key {
			actuals = append(actuals, s.Actual)
		}
	}
	if len(actuals) == 0 {
		return 0, 0, false
	}
	return medianUnits(actuals), len(actuals), true
}

// N is the number of samples deposited for a problem's class.
func (c *Calibrator) N(p Problem) int {
	key := p.Key()
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, s := range c.samples {
		if s.ProblemKey == key {
			n++
		}
	}
	return n
}

// maxDepthOf reports the deepest node depth in an outcome list.
func maxDepthOf(outs []NodeOutcome) int {
	max := 0
	for _, o := range outs {
		if o.Depth > max {
			max = o.Depth
		}
	}
	return max
}

// medianUnits returns the median of a non-empty Units slice. Integral throughout
// (the even-count case averages the two middle values with integer division), so
// it never introduces the float non-determinism the ledger forbids (P8) — though
// this value is advisory and off the record regardless.
func medianUnits(xs []Units) Units {
	cp := make([]Units, len(xs))
	copy(cp, xs)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}
