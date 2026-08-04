package provider

import (
	"context"
	"fmt"
	"strings"

	quarry "github.com/scttfrdmn/quarry"
)

// The real Adversary (build step 9, §5). Where the mechanical Verifier confirms,
// this REFUTES — and it does so through a DIFFERENT provider family than produced
// the claim, because same-family judging correlates errors (§5). That
// independence is the whole point, so it is enforced at construction, not merely
// documented: NewBedrockAdversary refuses a model in the same family as the
// solver it will judge.

// ErrSameFamily is returned when an adversary would judge claims from its own
// model family, which §5 forbids: correlated errors make the pass theatre.
var ErrSameFamily = fmt.Errorf("adversary model shares the solver's family (§5 requires independence)")

// BedrockAdversary attacks a claim by asking a model from a different family to
// find a defect. It is cost-metered like the provider so the surplus policy can
// admit it against the leftover budget (§3 Surplus).
type BedrockAdversary struct {
	Provider    *BedrockProvider
	Model       string // the adversary's model ID — a DIFFERENT family from the solver
	SolverModel string // what produced the claims, for the independence check and record

	// Ratio is the advisory verify:generate cost ratio (§5) reported to the
	// ladder. Adversarial review is the high, expensive rung.
	Ratio float64
}

// NewBedrockAdversary wires an adversary and ENFORCES §5 independence: advModel
// must be a different family from solverModel, or construction fails with
// ErrSameFamily. This is the check the core cannot make (it does not know model
// families) and the reason the requirement lives in provider/, not in quarry.
func NewBedrockAdversary(p *BedrockProvider, advModel, solverModel string) (*BedrockAdversary, error) {
	if SameFamily(advModel, solverModel) {
		return nil, fmt.Errorf("%w: adversary %q vs solver %q (both %q)",
			ErrSameFamily, advModel, solverModel, Family(advModel))
	}
	return &BedrockAdversary{Provider: p, Model: advModel, SolverModel: solverModel, Ratio: 0.5}, nil
}

// Name carries the adversary's PINNED model, so a refuted claim records which model
// refuted it. "An adversary disagreed" is not a citable finding; this one is (P8).
func (a *BedrockAdversary) Name() string { return "bedrock-adversary:" + a.Model }

// CostRatio is the configured fraction of the solve cost an attack is budgeted at
// (§3): verification spend is proportional to downstream exposure (P3), not uniform.
func (a *BedrockAdversary) CostRatio() float64 { return a.Ratio }

// Estimate sizes the attack for admission (§4, advisory).
func (a *BedrockAdversary) Estimate(quarry.Claim) quarry.Units {
	return a.Provider.Estimate("", a.Model)
}

// Attack asks the adversary model to refute the claim and reads a verdict off the
// reply. found=true means a defect was located — the asymmetric win (§5): one hit
// is enough. The prompt demands the reply START with REFUTED or SOUND so the
// verdict is mechanically readable; an unparseable reply is ok=false (the claim
// could not be assessed), distinct from attacked-and-survived (§8).
func (a *BedrockAdversary) Attack(ctx context.Context, c quarry.Claim, s quarry.Sample) (found bool, detail string, cost quarry.Units, ok bool) {
	prompt := buildAttackPrompt(c, s)
	sample, err := a.Provider.Complete(ctx, prompt, a.Model, quarry.Scope{})
	if err != nil {
		return false, "attack failed: " + err.Error(), 0, false
	}
	verdict := strings.TrimSpace(sample.Content)
	upper := strings.ToUpper(verdict)
	switch {
	case strings.HasPrefix(upper, "REFUTED"):
		return true, verdict, sample.Cost, true
	case strings.HasPrefix(upper, "SOUND"):
		return false, verdict, sample.Cost, true
	default:
		// Unparseable — reached the claim, could not assess it. The cost was still
		// incurred and must be reported so the ledger stays honest.
		return false, "unparseable verdict: " + verdict, sample.Cost, false
	}
}

func buildAttackPrompt(c quarry.Claim, s quarry.Sample) string {
	var b strings.Builder
	b.WriteString("You are an adversarial reviewer. Try to REFUTE the claim below. ")
	b.WriteString("Look for a factual error, an unsupported leap, or an internal contradiction. ")
	b.WriteString("Begin your reply with exactly one word: REFUTED if you found a real defect, ")
	b.WriteString("or SOUND if you could not. Then one sentence of justification.\n\n")
	b.WriteString("CLAIM: ")
	b.WriteString(c.Text)
	if s.Content != "" && s.Content != c.Text {
		b.WriteString("\n\nCONTEXT (the answer it came from):\n")
		b.WriteString(s.Content)
	}
	return b.String()
}

var _ quarry.Adversary = (*BedrockAdversary)(nil)
