package provider

import (
	"context"
	"fmt"
	"strings"

	quarry "github.com/scttfrdmn/quarry"
)

// The model-backed ClaimComparator — the paid rung of the §7 comparison ladder, and
// the thing that finally makes stability a MEASUREMENT rather than a floor.
//
// quarry's mechanical comparator catches only agreement that survives lowercasing
// and punctuation-stripping, so "prices rose in Q3" and "there was a third-quarter
// price increase" are counted as two separate claims and stability UNDERCOUNTS. That
// is the safe direction to be wrong in, but it is still wrong, and §13 names it the
// highest technical risk in the design.
//
// TWO PROPERTIES MAKE THIS DIFFERENT FROM AN LLM JUDGE, and both are enforced here:
//
//   - IT IS ORDER-SYMMETRIC. Equivalence is a symmetric relation, but a model asked
//     "does A say the same as B" is not symmetric — the first claim frames the
//     comparison. So the pair is CANONICALIZED before the prompt is built: the
//     lexicographically smaller normalized form goes first, always. Two callers
//     comparing the same pair in opposite orders therefore send an identical prompt
//     and get an identical answer, which is what makes cluster.go's
//     order-independence guarantee hold all the way down to the model.
//
//   - IT ABSTAINS. The reply must begin SAME, DIFFERENT or UNSURE, and UNSURE is a
//     first-class outcome rather than a parse failure. A comparator with no way to
//     abstain converts every hard comparison into a confident verdict, and since a
//     false SAME inflates agreement — reporting replicates as unanimous when they
//     are not — that is the one direction §7 must not be wrong in.
//
// Independence is NOT enforced here, deliberately, and the distinction from
// BedrockAdversary is worth stating: §5 requires an adversary to be a different model
// family from the solver because it JUDGES the solver's output, and same-family error
// correlation makes the pass theatre. A comparator judges nothing about quality — it
// asks whether two strings assert the same thing — so family correlation has no
// mechanism to bias it. What WOULD bias it is comparing a model's claims using the
// same model that generated them, if that model has a stake in agreeing with itself;
// nothing in the current design gives it one, so this is recorded as a known
// non-requirement rather than an unenforced one.

// ErrUnparseableComparison is returned when a comparison reply names no verdict.
// Distinct from an UNSURE reply, which is a real verdict: this is a broken
// comparator, that is a comparator declining to guess.
var ErrUnparseableComparison = fmt.Errorf("comparator reply names no verdict")

// BedrockComparator implements quarry.ClaimComparator with one Converse call.
//
// Cost-metered like every other paid seam: Compare reports actual spend so
// ClusterClaims can debit the ledger, which is what keeps comparison inside the cap
// rather than beside it (P4).
type BedrockComparator struct {
	Provider *BedrockProvider

	// Model is the comparator's model, an explicit pinned version, never an alias
	// (P8). A stability number computed under one model is not comparable with one
	// computed under another, and StabilityReport.ComparedBy records which — so the
	// version has to be real for that record to mean anything.
	Model string

	// Ratio is the advisory comparison:generation cost ratio (§5). Comparison is a
	// short, cheap call — two sentences in, one word out — so it sits low on the
	// ladder, well below adversarial review.
	Ratio float64
}

// NewBedrockComparator wires a comparator to a provider and an explicit model
// version. Refuses "auto" and the empty string for the same reason the provider does:
// a record that cannot name what decided its equivalences is not replayable (P8).
func NewBedrockComparator(p *BedrockProvider, model string) (*BedrockComparator, error) {
	if p == nil {
		return nil, fmt.Errorf("comparator has no provider")
	}
	if model == "" || model == "auto" {
		return nil, fmt.Errorf("comparator model must be an explicit pinned version, got %q (P8)", model)
	}
	return &BedrockComparator{Provider: p, Model: model, Ratio: 0.15}, nil
}

// Name identifies the comparator AND its model version in the stability report.
// The version is part of the identity on purpose: "compared by a model" is not a
// citable provenance claim, "compared by this exact model" is (P8).
func (bc *BedrockComparator) Name() string { return "bedrock-comparator:" + bc.Model }

// CostRatio is the configured fraction of a solve that one comparison is budgeted at.
// Lower than the adversary's: comparing two short claims is a smaller call than
// attacking one, and §2's premise is that judging is cheaper than generating.
func (bc *BedrockComparator) CostRatio() float64 { return bc.Ratio }

// Estimate is the pre-call size for admission control, delegated to the provider's
// pinned price sheet exactly as BedrockAdversary.Estimate does.
//
// The provider's flat EstInputTokens/EstOutputTokens are used rather than a
// per-claim token count. A comparison prompt is two short claims plus a fixed
// preamble, so the flat estimate is already generous for the input and the output is
// ONE WORD — which makes this an over-estimate, the safe direction (§4): it refuses
// an affordable comparison rather than authorizing an unaffordable one. A nil
// provider estimates zero, matching Compare's refusal to call.
func (bc *BedrockComparator) Estimate(_, _ quarry.Claim) quarry.Units {
	if bc.Provider == nil {
		return 0
	}
	return bc.Provider.Estimate("", bc.Model)
}

// Compare asks whether two claims assert the same thing.
//
// Returns (equivalent, ok, cost). ok=false covers three distinct situations that all
// mean "no verdict": the call failed, the reply was unparseable, or the model
// explicitly answered UNSURE. They are collapsed because the CALLER's obligation is
// the same in each — count it as unassessed, never as a finding — while remaining
// separable to a human through the cost: a failed call before any tokens is free, an
// UNSURE was billed.
//
// Cost is reported even when ok is false. A refused or unparseable reply was still
// metered by Bedrock, and hiding it would make the ledger wrong in the flattering
// direction — the rule RunSurplus already follows for a failed attack.
func (bc *BedrockComparator) Compare(ctx context.Context, a, b quarry.Claim) (bool, bool, quarry.Units) {
	if bc.Provider == nil {
		return false, false, 0
	}

	// Canonicalize the pair so the prompt does not depend on argument order. This is
	// the whole order-symmetry guarantee, and it is two lines because Claim.Norm is
	// pinned into the record for exactly this kind of use (quarry/claim.go).
	first, second := a, b
	if normOf(b) < normOf(a) {
		first, second = b, a
	}

	sample, err := bc.Provider.Complete(ctx, buildComparePrompt(first, second), bc.Model, quarry.Scope{})
	if err != nil {
		// No verdict, and typically no tokens either — a context cancellation or a
		// transport fault. Whatever the provider reported as cost is passed through
		// rather than assumed zero.
		return false, false, sample.Cost
	}

	verdict := strings.ToUpper(strings.TrimSpace(sample.Content))
	switch {
	case strings.HasPrefix(verdict, "SAME"):
		return true, true, sample.Cost
	case strings.HasPrefix(verdict, "DIFFERENT"):
		return false, true, sample.Cost
	case strings.HasPrefix(verdict, "UNSURE"):
		// A REAL VERDICT, not a failure: the model was asked and declined. Reporting
		// this as DIFFERENT would inflate instability and as SAME would inflate
		// agreement, so it becomes the third state the seam exists to carry.
		return false, false, sample.Cost
	default:
		// Unparseable. Same obligation as UNSURE for the caller, but a defect rather
		// than a judgement — the prompt or the model changed under us.
		return false, false, sample.Cost
	}
}

// normOf prefers the pinned normalized form, falling back to the raw text. Text is a
// sufficient fallback for CANONICAL ORDERING (any total order will do, as long as it
// is the same one on both calls); it would not be sufficient for equivalence, which
// is why claim.go re-normalizes there and this does not.
func normOf(c quarry.Claim) string {
	if c.Norm != "" {
		return c.Norm
	}
	return c.Text
}

// buildComparePrompt asks for one word.
//
// The prompt is deliberately narrow: it asks about SAME CONCLUSION, not about truth,
// quality, or overlap. A comparator drawn into judging correctness would be doing the
// adversary's job with none of the adversary's independence guarantees (§5), and its
// answers would then depend on facts about the world rather than on the two strings —
// which would make stability unreproducible for a reason nothing in the record could
// explain.
//
// Two spelled-out cases carry most of the work: same conclusion in different words is
// SAME, and same TOPIC with a different conclusion is DIFFERENT. The second is the
// error a naive similarity measure makes — "prices rose" and "prices fell" are
// lexically and semantically close and are opposite conclusions — and it is the error
// that inflates agreement, so it is named explicitly rather than left to inference.
func buildComparePrompt(a, b quarry.Claim) string {
	var s strings.Builder
	s.WriteString("Do these two statements assert the SAME CONCLUSION?\n\n")
	s.WriteString("Answer with exactly one word on the first line:\n")
	s.WriteString("  SAME       - the same conclusion, even if worded differently\n")
	s.WriteString("  DIFFERENT  - different conclusions, INCLUDING opposite conclusions\n")
	s.WriteString("               about the same topic (\"prices rose\" vs \"prices fell\")\n")
	s.WriteString("  UNSURE     - you cannot tell. Prefer this over guessing.\n\n")
	s.WriteString("Judge only whether the conclusions match. Do NOT judge whether either ")
	s.WriteString("statement is true, well-supported, or better written.\n\n")
	s.WriteString("A: ")
	s.WriteString(a.Text)
	s.WriteString("\nB: ")
	s.WriteString(b.Text)
	return s.String()
}

var _ quarry.ClaimComparator = (*BedrockComparator)(nil)
