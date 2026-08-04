package quarry

import (
	"context"
	"regexp"
	"sort"
)

// Verifier implementations and the mechanical oracles at the top of the §5
// ladder. Mechanical oracles cost ~0 — they call no model — so they always run
// and are the cheapest way to make "good" a specifiable axis (P2, P3, §5).
//
// LLM judges, adversarial passes and debate live higher on the ladder and land
// in step 9. Nothing here dials a model, which keeps step 4 runnable with no
// AWS and no LLM.

// FuncVerifier is a mechanical oracle wrapping a pure predicate over the result.
//
// The general-purpose oracle: schema conformance, a residual check, "did it
// compile", "do the tests pass" all reduce to a boolean over the sample. Cost
// ratio is 0 by construction — it is arithmetic, not inference.
type FuncVerifier struct {
	Label string
	Check func(p Problem, s Sample) bool
	// Applies restricts the verifier to problems it can judge. Nil means it
	// applies everywhere. A verifier that is not AvailableFor a problem is not a
	// failed check — it is silence, which the receipt must distinguish (§8).
	Applies func(p Problem) bool
}

// Name is the caller's label, and it is what lands in the record as the identity of
// what checked a node. An unlabelled oracle produces a verdict nobody can attribute.
func (f FuncVerifier) Name() string { return f.Label }

// CostRatio is 0 by construction: a predicate over the sample is arithmetic, not
// inference. This is why mechanical oracles always run — they are the cheapest rung of
// the §5 ladder and the reason P2 has a terminator that costs nothing.
func (f FuncVerifier) CostRatio() float64 { return 0 }

// AvailableFor requires a Check to exist as well as Applies to permit it. A nil Check
// makes the verifier UNAVAILABLE rather than failing — a zero-value FuncVerifier is
// silence, not a verdict of false (§8).
func (f FuncVerifier) AvailableFor(p Problem) bool {
	return f.Check != nil && (f.Applies == nil || f.Applies(p))
}

// Verify returns (passed, ok). ok=false means NOT CHECKED, which the record keeps
// distinct from checked-and-failed — the distinction §8 exists to preserve, and the
// reason this returns two bools rather than one.
func (f FuncVerifier) Verify(ctx context.Context, p Problem, s Sample) (passed, ok bool) {
	if !f.AvailableFor(p) {
		return false, false
	}
	return f.Check(p, s), true
}

// NonEmptyOracle is the floor mechanical check: a result with no content is a
// hard failure. Cheap, catches truncation and empty completions, blind to
// everything unformalized (§5).
func NonEmptyOracle() FuncVerifier {
	return FuncVerifier{
		Label: "non-empty",
		Check: func(_ Problem, s Sample) bool { return s.Content != "" },
	}
}

// RegexOracle passes when the content matches pat. A schema/format oracle.
func RegexOracle(label string, pat *regexp.Regexp) FuncVerifier {
	return FuncVerifier{
		Label: label,
		Check: func(_ Problem, s Sample) bool { return pat.MatchString(s.Content) },
	}
}

// MultiVerifier composes verifiers and runs the available ones cheapest-first
// (the §5 ladder: mechanical oracles before anything that costs). It is itself a
// Verifier, so the executor holds a single seam.
//
// passed is the AND of every verifier that ran; ok is true if at least one ran.
// A set with nothing available reports ok=false — the node is unchecked, not
// checked-and-passed, and the record must say so (§8).
type MultiVerifier struct {
	Verifiers []Verifier
}

// Name is "multi" rather than a composition of its members' names. The members that
// actually RAN are per-problem and not known here, so a composed name would claim a
// ladder that may not have been walked; the executor records the members' own names.
func (m MultiVerifier) Name() string { return "multi" }

// CostRatio is the sum of the members' ratios — an upper bound on the overhead
// of running the whole ladder, since availability is per-problem and unknown
// here.
func (m MultiVerifier) CostRatio() float64 {
	var t float64
	for _, v := range m.Verifiers {
		t += v.CostRatio()
	}
	return t
}

// AvailableFor is true when ANY member is, matching Verify's ok semantics: one
// available oracle is enough to make the node checked.
func (m MultiVerifier) AvailableFor(p Problem) bool {
	for _, v := range m.Verifiers {
		if v.AvailableFor(p) {
			return true
		}
	}
	return false
}

// Verify runs the available members cheapest-first and ANDs their verdicts. ok is true
// if at least one ran; a set with nothing available reports unchecked, never passed.
func (m MultiVerifier) Verify(ctx context.Context, p Problem, s Sample) (passed, ok bool) {
	avail := make([]Verifier, 0, len(m.Verifiers))
	for _, v := range m.Verifiers {
		if v.AvailableFor(p) {
			avail = append(avail, v)
		}
	}
	// Cheapest first: a ~0 oracle that fails saves the cost of everything above
	// it on the ladder. Ties broken by name for deterministic replay (P8).
	sort.SliceStable(avail, func(a, b int) bool {
		if avail[a].CostRatio() != avail[b].CostRatio() {
			return avail[a].CostRatio() < avail[b].CostRatio()
		}
		return avail[a].Name() < avail[b].Name()
	})
	allPassed := true
	for _, v := range avail {
		p2, ok2 := v.Verify(ctx, p, s)
		if !ok2 {
			continue
		}
		ok = true
		if !p2 {
			allPassed = false
			break // one hard failure is enough; do not pay for the rest
		}
	}
	return allPassed && ok, ok
}

var (
	_ Verifier = FuncVerifier{}
	_ Verifier = MultiVerifier{}
)
