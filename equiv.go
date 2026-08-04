package quarry

import (
	"context"
)

// Claim comparison as a METERED, THREE-STATE, CANCELLABLE judgement (§7, §12).
//
// claim.go ships normalized-string equality and says so: agreement phrased
// differently is missed, so stability UNDERCOUNTS. Closing that needs a model, and
// a model cannot be poured into the existing seam. ClaimExtractor.Equivalent is
//
//	Equivalent(a, b Claim) bool
//
// and every part of that signature forbids the thing it would have to become:
//
//   - NO ctx. A model call must be cancellable and deadline-bound (§3.1). A
//     comparison that cannot observe the deadline would outlive the run it is
//     reporting on.
//   - NO third state. A bool cannot say "I could not tell", so an unassessable
//     comparison — a refusal, a timeout, an exhausted budget — would have to be
//     reported as DISAGREEMENT. That is the same defect as a two-valued verifier
//     verdict (§8): it converts silence into a finding, and here it would convert a
//     billing event into a scientific result.
//   - NO cost. Comparison would be the first thing in the system to spend money
//     outside the ledger, and an O(n²) sweep of paid calls is the easiest way to
//     spend a lot of it. P4 says the cap is the contract; a seam that cannot report
//     what it spent cannot be capped.
//
// So the bool relation stays exactly what it is — the free rung — and the model
// arrives behind a wider seam. Nothing here dials a network or reads the clock
// (Go rule 4); the model implementation lives in provider/.
//
// THE COST ARGUMENT, which is why this is affordable at all. Normalized equality is
// SOUND as a sufficient condition: if two claims share a normalized form they are
// the same claim, and no model is needed to confirm it. It is only INCOMPLETE — it
// misses paraphrase. So the free rung pre-clusters exactly, for nothing, and the
// paid comparator runs only BETWEEN DISTINCT WORDINGS. n claims collapse to k
// canonical forms and the paid work is O(k²) over wordings rather than O(n²) over
// claims. Replicates of a deterministic pipeline collapse almost completely; that
// asymmetry is what Claim.Norm was pinned into the record for (claim.go).

// ClaimComparator decides whether two claims assert the same thing, and is allowed
// to cost money and to fail (§7).
//
// The three-state return is the load-bearing difference from
// ClaimExtractor.Equivalent. ok=false means the comparison COULD NOT BE MADE, which
// is not a claim about the claims: reporting it as inequivalence would inflate
// instability, and reporting it as equivalence would inflate agreement. It has to
// be its own outcome, and StabilityReport carries it as its own count.
//
// MUST be safe for concurrent use if handed to anything that fans out. The core's
// clustering is single-goroutine, so this is a forward-looking requirement rather
// than one the package currently exercises.
type ClaimComparator interface {
	// Name identifies the comparator in the report. A stability number computed
	// under one comparator is not comparable with one computed under another, so the
	// report records who decided (the P8 discipline applied to a post-hoc analysis:
	// the number outlives the model that produced it and must name it).
	Name() string

	// CostRatio is the advisory comparison:generation ratio (§5). A mechanical
	// comparator reports 0.
	CostRatio() float64

	// Estimate is the PRE-CALL cost, for admission control — the same role
	// Adversary.Estimate and Provider.Estimate play (§3).
	//
	// Without it a caller can only spend and then ask permission, which is not
	// admission control: the money is gone before the cap is consulted. A live run
	// against a 1-micro-unit cap spent 200 and reported Truncated afterwards — honest
	// about the outcome, and still a cap violation, because P4 makes the cap the
	// contract rather than a target. A comparator that cannot be sized cannot be
	// admitted, so this is on the interface rather than left to the caller.
	//
	// It may over-estimate — that is the safe direction, refusing an affordable
	// comparison rather than making an unaffordable one. Zero means free, and a free
	// comparator needs no ledger at all.
	Estimate(a, b Claim) Units

	// Compare reports whether a and b assert the same thing.
	//
	// ok=false means unassessable. cost is the actual spend, and MUST be reported
	// even when ok is false — a refused or unparseable model reply was still billed,
	// and a comparator that hid it would make the ledger wrong in the flattering
	// direction (the same rule RunSurplus follows for a failed attack).
	Compare(ctx context.Context, a, b Claim) (equivalent bool, ok bool, cost Units)
}

// MechanicalComparator is the FREE RUNG of the comparison ladder: exact normalized
// equality, no model, no network, no clock.
//
// IT REPORTS A MATCH AND NOTHING ELSE. On equal normalized forms it returns
// (true, ok=true) — sound, as argued above. On unequal forms it returns
// (false, ok=FALSE), because that is the truth: this comparator cannot distinguish
// "different conclusions" from "the same conclusion worded differently", and
// claiming the former would be exactly the overstatement claim.go's TODO warns
// about.
//
// That is deliberately NOT the behaviour of MechanicalExtractor.Equivalent, which
// returns a plain false and is used as a TOTAL relation by the free Stability path.
// The two are separate types because they answer different questions — "is this the
// same claim?" versus "can I tell?" — and one type serving both roles is the defect
// class this package has now been bitten by three times.
type MechanicalComparator struct {
	// Normalize maps text to its canonical form. Nil means NormalizeText.
	Normalize func(text string) string
}

// Name identifies this rung in a LadderComparator's composed name.
func (MechanicalComparator) Name() string { return "mechanical" }

// CostRatio is zero: string normalization calls no model. This is what makes the
// free rung free, and what lets §3 spend the comparison budget on the paid rung only.
func (MechanicalComparator) CostRatio() float64 { return 0 }

// Estimate is zero for the same reason CostRatio is. Reported separately because the
// two are different questions — a cost multiplier and an absolute prediction — and a
// comparator that called a model would answer them differently.
func (MechanicalComparator) Estimate(_, _ Claim) Units { return 0 }

// Compare returns (equivalent, assessable, cost). It ABSTAINS rather than guessing —
// see the type doc: a normalized mismatch means "cannot tell", not "different".
func (m MechanicalComparator) Compare(_ context.Context, a, b Claim) (bool, bool, Units) {
	norm := m.Normalize
	if norm == nil {
		norm = NormalizeText
	}
	na, nb := claimNorm(a, norm), claimNorm(b, norm)
	if na == "" || nb == "" {
		// Nothing to compare. Not a disagreement — an absence.
		return false, false, 0
	}
	if na == nb {
		return true, true, 0
	}
	return false, false, 0
}

// claimNorm prefers the PINNED normalized form and re-normalizes only when absent,
// so a comparison over recorded claims uses the normalization that produced them
// rather than today's (P8, same rule as MechanicalExtractor.Equivalent).
func claimNorm(c Claim, norm func(string) string) string {
	if c.Norm != "" {
		return c.Norm
	}
	return norm(c.Text)
}

// LadderComparator is the §5 ladder applied to comparison: try free, then paid.
//
// Free runs first and a MATCH SHORT-CIRCUITS — the sufficient condition is sound,
// so there is nothing a paid call could add and every micro-unit it spent would be
// waste. A free non-match escalates, because a free non-match is not a verdict.
//
// With Paid nil this is just the free rung, and it reports ok=false on every
// non-match: an honest "unassessed" rather than a fabricated disagreement. A caller
// who wants the old total relation should use Stability, which is documented as
// undercounting; a caller who wires a Paid comparator gets the real measurement.
type LadderComparator struct {
	// Free is the cheap rung. Nil means MechanicalComparator{}.
	Free ClaimComparator

	// Paid is the model-backed rung, typically provider.BedrockComparator. Nil
	// leaves the ladder mechanical.
	Paid ClaimComparator
}

// Name composes the rungs it will actually use, so a record says which ladder ran
// rather than just "ladder" — a mechanical-only ladder and a model-backed one are
// different evidence and must not share a label (P8).
func (l LadderComparator) Name() string {
	name := l.free().Name()
	if l.Paid != nil {
		name += "+" + l.Paid.Name()
	}
	return name
}

// CostRatio reports the PAID rung's ratio, not the sum. The free rung is 0 by
// construction and, unlike MultiVerifier's ladder, the rungs here are mutually
// exclusive per comparison — a match never pays — so summing would overstate.
func (l LadderComparator) CostRatio() float64 {
	if l.Paid == nil {
		return 0
	}
	return l.Paid.CostRatio()
}

// Estimate sizes the WORST CASE: free rung plus paid rung.
//
// Not the expected cost, deliberately. Admission has to cover the branch that
// actually spends, and a comparison that escalates is exactly that branch — sizing
// by the average would admit a pair the ladder cannot afford to finish. The free
// rung's own estimate is included rather than assumed zero, since Free is
// caller-supplied and a "cheap" rung is not necessarily a free one.
func (l LadderComparator) Estimate(a, b Claim) Units {
	est := l.free().Estimate(a, b)
	if l.Paid != nil {
		est += l.Paid.Estimate(a, b)
	}
	return est
}

func (l LadderComparator) free() ClaimComparator {
	if l.Free == nil {
		return MechanicalComparator{}
	}
	return l.Free
}

// Compare escalates only when it must: a free-rung MATCH is a sound sufficient
// condition, so paying to confirm it would buy nothing. Both rungs' costs are summed
// when it does escalate — the free call was still made, and §3 charges what was spent.
func (l LadderComparator) Compare(ctx context.Context, a, b Claim) (bool, bool, Units) {
	eq, ok, cost := l.free().Compare(ctx, a, b)
	if ok && eq {
		return true, true, cost // sound sufficient condition; do not pay to confirm it
	}
	if l.Paid == nil {
		return eq, ok, cost
	}
	peq, pok, pcost := l.Paid.Compare(ctx, a, b)
	return peq, pok, cost + pcost
}

// FuncComparator wraps a pure predicate as a free comparator — the test double and
// the escape hatch for a caller with a domain-specific equivalence (unit-aware
// numeric comparison, say) that costs nothing.
//
// Assess reports whether the pair was assessable at all; nil means always. This
// keeps the three-state contract available to a mechanical comparator, so a
// domain oracle can abstain on pairs outside its competence instead of guessing —
// the same distinction FuncVerifier.Applies draws for verification (§8).
type FuncComparator struct {
	Label  string
	Eq     func(a, b Claim) bool
	Assess func(a, b Claim) bool
}

// Name is the caller's label. Empty is allowed and appears as such in the record: a
// comparator this package cannot name is better recorded anonymously than under an
// invented name.
func (f FuncComparator) Name() string { return f.Label }

// CostRatio and Estimate are zero because a FuncComparator is a pure predicate. A
// caller who wraps something that spends is using the wrong type — the ladder's paid
// rung is the seam for that.
func (f FuncComparator) CostRatio() float64 { return 0 }

// Estimate is zero; see CostRatio.
func (FuncComparator) Estimate(_, _ Claim) Units { return 0 }

// Compare abstains when there is no predicate, or when Assess says the pair is
// outside the wrapped oracle's competence. A missing Eq returns NOT-ASSESSABLE rather
// than false, since "no oracle" and "the oracle said no" are different findings (§8).
func (f FuncComparator) Compare(_ context.Context, a, b Claim) (bool, bool, Units) {
	if f.Eq == nil || (f.Assess != nil && !f.Assess(a, b)) {
		return false, false, 0
	}
	return f.Eq(a, b), true, 0
}

// boolComparator adapts a total bool relation to the ClaimComparator seam for the
// free Stability path: every comparison is assessable and free, which is precisely
// the (over)claim the mechanical relation makes. Unexported because it is a
// compatibility shim, not a thing to build on.
type boolComparator struct {
	name  string
	equiv func(a, b Claim) bool
}

func (b boolComparator) Name() string            { return b.name }
func (boolComparator) CostRatio() float64        { return 0 }
func (boolComparator) Estimate(_, _ Claim) Units { return 0 }
func (b boolComparator) Compare(_ context.Context, x, y Claim) (bool, bool, Units) {
	return b.equiv(x, y), true, 0
}

var (
	_ ClaimComparator = MechanicalComparator{}
	_ ClaimComparator = LadderComparator{}
	_ ClaimComparator = FuncComparator{}
	_ ClaimComparator = boolComparator{}
)
