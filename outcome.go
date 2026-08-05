package quarry

// Outcome is how a run ENDED, as a vocabulary a supervising host can branch on
// (#9 D4, §3.1, §8.2).
//
// A host cannot tell "finished", "ran out of time", "ran out of money" and "crashed"
// apart from a boolean exit status, and the difference decides what it does next.
// Offering a deadline raise where money was needed is the exact §3.1 mislabelling that
// ErrRecordedUnfunded exists to prevent, and a host is the consumer most likely to
// make it — it is choosing a remediation automatically, with no researcher reading the
// summary first.
//
// WHY THIS IS IN THE CORE AND NOT IN cmd/quarry. It is a reading of a RunRecord, so it
// must be testable against real records without a process, and `quarry show` has to
// agree with `quarry run` about whether a run finished — the two views disagreeing
// about that once already (see summarize's truncated note in cmd/quarry/run.go) is why
// the predicate lives in one place. The CLI maps this to a number; it does not decide it.
//
// THE CLASSIFICATION IS NOT A SEVERITY ORDERING. OutcomeDegraded is not a worse
// OutcomeComplete: under the standing ruling only TIME produces a gap, and spend
// exhaustion is planned degradation INSIDE authority. Labelling it a malfunction makes
// the cap look broken when it is working exactly as P4 promises.
type Outcome string

// The run outcomes. A fifth case — a usage error, e.g. refused flags — is not here
// because it is not a reading of a record: no run happened, so there is nothing to
// classify. The CLI owns that code.
const (
	// OutcomeComplete: the run finished inside its caps, with an answer.
	OutcomeComplete Outcome = "complete"

	// OutcomeTimeTruncated: a deadline cut the run short, so the record HAS GAPS —
	// work that was never done and is not recorded as having been declined (§3.1).
	// This is the only outcome that means information is missing rather than
	// deliberately forgone.
	OutcomeTimeTruncated Outcome = "time-truncated"

	// OutcomeDegraded: a cap bound the run and it returned less than it set out to,
	// but every omission was a priced decision inside authority. NOT A FAILURE.
	OutcomeDegraded Outcome = "cap-bound-degradation"

	// OutcomeNoAnswer: the run produced no answer at all — nothing was affordable, or
	// every node came back empty. Distinct from Degraded because a host has nothing to
	// show a user, and distinct from a fault because the record is still faithful and
	// still citable: it accurately records that nothing was affordable (§8).
	OutcomeNoAnswer Outcome = "no-answer"
)

// Classify reads how a run ended off the record (#9 D4).
//
// ORDER IS THE SEMANTICS, and each precedence below was chosen rather than fallen into:
//
//  1. NO ANSWER FIRST, ahead of every truncation signal. A run that returned nothing
//     is the one case where a host cannot proceed at all, and it is almost always ALSO
//     truncated — reporting the truncation instead would tell a host to extend a run
//     that has no partial answer to extend.
//
//  2. TIME BEFORE MONEY. A run can be both: a deadline expires while the spend cap is
//     also nearly gone. Gaps mean work that is missing and unrecorded, and that is the
//     more consequential fact — money spent is visible in the receipt, whereas a gap is
//     the absence of a line. Raising a deadline on a time-truncated run is also the
//     remediation that actually helps.
//
//  3. DEGRADED LAST, from Truncated(). It is the broad predicate: BoundBy set, or an
//     unfunded node. Deliberately checked after gaps so a time-truncated run is never
//     reported as merely degraded, which would understate it.
func (r RunRecord) Classify() Outcome {
	if !r.HasAnswer() {
		return OutcomeNoAnswer
	}
	// Only TIME produces a gap (standing ruling), so this branch and the next are not
	// two readings of one signal — they are the two denominations, and a host offering
	// the wrong remedy is the failure this vocabulary exists to prevent.
	if len(r.Gaps()) > 0 {
		return OutcomeTimeTruncated
	}
	if r.Truncated() {
		return OutcomeDegraded
	}
	return OutcomeComplete
}

// HasAnswer reports whether the run produced any answer text at all.
//
// The root's content, because Outcomes are pre-order so the root is first. An
// empty-outcome record — a run that failed before any node completed — has no answer
// either, and the length check is what makes that case reachable rather than a panic.
//
// Whitespace-only counts as no answer: it is what a model returns when it declines,
// and a host would otherwise render a blank pane as a result.
func (r RunRecord) HasAnswer() bool {
	if len(r.Outcomes) == 0 {
		return false
	}
	for _, c := range r.Outcomes[0].Content {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return true
		}
	}
	return false
}
