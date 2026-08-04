package quarry

import "context"

// Stability reporting (build step 7, §7). A run is an estimate, not an answer
// (P7): one run is a single draw from a stochastic instrument. The load-bearing
// scientific claim is REPLICABILITY — do independent re-derivations reach
// consistent conclusions? — and the honest output is not an agreement
// percentage but the LIST OF UNSTABLE CLAIMS, the places where the instrument is
// at its limit (§7, "report instability, not agreement").
//
// Comparing conclusions rather than text is exactly what the step-6 claim
// equivalence buys. Without it "these runs agree" is vibes with a percentage
// attached. Here equivalence is the mechanical normalized-string relation from
// claim.go, so stability inherits its limitation: agreement that differs in
// WORDING is missed, which UNDERCOUNTS stability — the safe direction, the same
// way the cache key under-hits. When semantic equivalence lands (the Python
// piece, §12), this file consumes it unchanged: it depends only on an equiv
// predicate, not on how equivalence is decided.

// ClaimStability is one claim cluster's support across the replicates compared.
//
// Support counts DISTINCT replicates that asserted an equivalent claim, never
// raw occurrences: a single run repeating a claim across nodes must not inflate
// its own agreement. Claim is the first occurrence encountered, with Stable set.
type ClaimStability struct {
	Claim   Claim
	Support int // distinct replicates asserting an equivalent claim
	Total   int // replicates compared
}

// Stable reports whether the cluster met the report's support threshold.
func (cs ClaimStability) Stable() bool { return cs.Claim.Stable != nil && *cs.Claim.Stable }

// StabilityReport is the cross-replicate summary (§7). Claims are in canonical
// content order (by normalized form), which is deterministic and — unlike the
// first-occurrence order this once used — independent of the order the replicates
// were passed in. See cluster.go for why that distinction is load-bearing.
type StabilityReport struct {
	Replicates int
	MinSupport int
	Claims     []ClaimStability

	// ComparedBy names the comparator that decided equivalence. A stability number
	// computed under normalized-string equality is not comparable with one computed
	// under a model, so the report must say which produced it — the P8 discipline
	// ("the record outlives the model") applied to a post-hoc analysis.
	ComparedBy string

	// Unassessed counts comparisons the comparator COULD NOT MAKE (§7). Its own
	// number, deliberately: folding it into agreement would inflate stability and
	// folding it into disagreement would inflate instability, and neither is a claim
	// any comparator actually made. Under the free mechanical comparator this is
	// large by construction — every differing wording is unassessed rather than
	// judged — which is the honest rendering of what claim.go's TODO describes.
	Unassessed int

	// ComparisonCost is what deciding equivalence cost (§7, P4). Comparison is the
	// first analysis in the system that can spend money, so it is metered like
	// everything else; zero on the free path.
	ComparisonCost Units

	// Truncated reports that the comparison pass ran out of budget before finishing.
	// The clusters below are then a PARTIAL clustering — real, but incomplete, and
	// under-merged in the safe direction. A partial result that says so beats a
	// complete-looking one that lies.
	Truncated bool
}

// Unstable is the valuable output (§7): claims that did not clear the support
// threshold — where the instrument is at its limit and a refine should spend.
func (r StabilityReport) Unstable() []ClaimStability {
	var out []ClaimStability
	for _, c := range r.Claims {
		if !c.Stable() {
			out = append(out, c)
		}
	}
	return out
}

// Stable is the complement — claims that held across replicates.
func (r StabilityReport) Stable() []ClaimStability {
	var out []ClaimStability
	for _, c := range r.Claims {
		if c.Stable() {
			out = append(out, c)
		}
	}
	return out
}

// StabilityRate is the fraction of distinct claims that were stable. Reported
// alongside the unstable list, never instead of it: a rate is a summary, the
// list is the actionable signal (§7). ok=false when there are no claims to rate.
func (r StabilityReport) StabilityRate() (float64, bool) {
	if len(r.Claims) == 0 {
		return 0, false
	}
	var stable int
	for _, c := range r.Claims {
		if c.Stable() {
			stable++
		}
	}
	return float64(stable) / float64(len(r.Claims)), true
}

// Stability clusters the claims of independent replicate records by equivalence
// and reports each cluster's support (§7).
//
// equiv is the equivalence relation; nil uses the mechanical default from
// claim.go. minSupport is the distinct-replicate count a claim must reach to be
// stable; a non-positive value defaults to unanimity (every replicate), the
// strict end — consistent with under-counting stability rather than over-
// claiming it.
//
// Deterministic: records, outcomes and claims are all walked in order, so the
// representative of each cluster and the cluster order are pure functions of the
// input (P8). The replicates MUST be independent draws — see Replicate. Passing
// runs that shared a serving cache collapses them to one sample and makes every
// claim look trivially stable, which is precisely the failure §6's "unstable
// nodes are always extended" rule exists to prevent.
func Stability(records []RunRecord, equiv func(a, b Claim) bool, minSupport int) StabilityReport {
	name := "mechanical-string-equality"
	if equiv == nil {
		equiv = MechanicalExtractor{}.Equivalent
	} else {
		name = "caller-supplied"
	}
	// The bool relation is adapted rather than reimplemented, so the free path and
	// the metered path share ONE clustering algorithm. Two clustering implementations
	// would drift, and the order-dependence bug cluster.go documents is precisely
	// what a drifting second copy reintroduces.
	rep := StabilityWith(context.Background(), records,
		boolComparator{name: name, equiv: equiv}, minSupport, nil)
	return rep
}

// StabilityWith is Stability under an explicit ClaimComparator, which may cost
// money and may decline to judge (§7).
//
// This is the seam semantic equivalence arrives through. The comparator decides
// equivalence; this function does the clustering, the support counting and the
// thresholding, none of which depend on HOW equivalence was decided — which is why
// the model can land in provider/ without touching this file.
//
// l is the ledger the comparison spends against, and may be nil ONLY for a free
// comparator. Comparison is real spend (P4): a paid comparator with no ledger would
// bill outside every cap, so ClusterClaims refuses it and reports Truncated rather
// than quietly spending.
//
// minSupport semantics are unchanged: non-positive means unanimity, the strict end,
// consistent with undercounting stability rather than overclaiming it.
func StabilityWith(
	ctx context.Context,
	records []RunRecord,
	cmp ClaimComparator,
	minSupport int,
	l *Ledger,
) StabilityReport {
	if cmp == nil {
		cmp = MechanicalComparator{}
	}
	total := len(records)
	if minSupport <= 0 {
		minSupport = total
	}

	byReplicate := make([][]Claim, len(records))
	for ri, rec := range records {
		byReplicate[ri] = rec.AllClaims()
	}

	clusters, unassessed, cost, exhausted := ClusterClaims(ctx, byReplicate, cmp, l)

	out := StabilityReport{
		Replicates:     total,
		MinSupport:     minSupport,
		ComparedBy:     cmp.Name(),
		Unassessed:     unassessed,
		ComparisonCost: cost,
		Truncated:      exhausted,
	}
	for _, c := range clusters {
		support := c.Support()
		stable := support >= minSupport
		rep := c.Rep
		rep.Stable = &stable // pin the verdict onto the representative (§7)
		out.Claims = append(out.Claims, ClaimStability{Claim: rep, Support: support, Total: total})
	}
	return out
}

// AllClaims flattens every node's claims in pre-order — the record's full claim
// set, the unit stability compares over.
func (r RunRecord) AllClaims() []Claim {
	var out []Claim
	for _, o := range r.Outcomes {
		out = append(out, o.Claims...)
	}
	return out
}
