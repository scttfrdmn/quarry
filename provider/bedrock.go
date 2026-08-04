// Package provider holds the real, network-dialing implementations of quarry's
// seams (build step 9+, §5). It is deliberately SEPARATE from package quarry:
// the core imports no AWS SDK, touches no network and never reads the clock (Go
// rule 4), which is what keeps steps 1-8 runnable with neither AWS nor an LLM.
// Everything that breaks those rules lives here, behind the interfaces.
//
// Bedrock is the one endpoint because its Converse API is uniform across model
// FAMILIES — Anthropic (Claude), Meta (Llama), Amazon (Nova) all answer the same
// call. That uniformity is not a convenience, it is what makes §5's judge
// independence cheap: an adversary routed through a different family than
// produced a claim is one different model-ID string away, not a second client.
package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	quarry "github.com/scttfrdmn/quarry"
)

// Converser is the slice of the Bedrock runtime client this package uses. Naming
// it as an interface keeps the provider unit-testable with a fake that never
// dials AWS — the network stays at the edge, mockable, exactly as the core
// intends.
type Converser interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// Pricing is the per-million-token cost of a model, in whatever currency the
// caller's Units denominate (§12 leaves the unit of account open; this package
// treats Units as micro-dollars via quarry.FromFloat). Both halves are needed
// because §4 splits the predictable input (halo) from the stochastic output.
type Pricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// BedrockProvider implements quarry.Provider against Bedrock's Converse API.
//
// It is the only thing in the system that spends real money, so it does exactly
// what the seam promises and no more: one Converse call per Complete, token usage
// read straight from the response, cost computed from a pinned price sheet. The
// model VERSION returned is explicit (the resolved model ID), never an alias —
// a record that cannot name what produced it is not replayable (P8).
type BedrockProvider struct {
	Client Converser

	// Prices maps a model ID to its price sheet. A model absent here priced at
	// zero would silently make expensive calls look free, so Complete records the
	// cost as zero AND the estimate returns zero — a missing price is visible as
	// an implausibly cheap node, not hidden. Populate it explicitly.
	Prices map[string]Pricing

	// MaxTokens caps generation per call. Zero lets the model default apply.
	//
	// ENDPOINT-LEVEL, and therefore the wrong instrument for a leaf: it is one value
	// for every call, while sibling leaves funded differently need different ceilings
	// (P9). It still applies to the calls that have no allocation of their own — the
	// planner's and the reducer's. A per-node ceiling comes through CompleteBounded,
	// which BudgetedSolver sizes from the node's allocation (solver.go).
	MaxTokens int32

	// EstInputTokens / EstOutputTokens size the pre-call admission estimate (§4).
	// Estimation is advisory (P4); these are coarse priors, not measurements.
	EstInputTokens  int
	EstOutputTokens int

	// Now stamps the sample's CreatedAt. The CORE never reads the clock; a
	// provider at the network edge legitimately does, but taking it as a field
	// keeps the call deterministic under test.
	Now func() time.Time
}

// NewBedrockProvider builds a provider from the ambient AWS config (profile,
// region and credentials from the environment — AWS_PROFILE, etc.). It dials
// nothing until the first Complete.
func NewBedrockProvider(ctx context.Context, region string, prices map[string]Pricing) (*BedrockProvider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &BedrockProvider{
		Client:          bedrockruntime.NewFromConfig(cfg),
		Prices:          prices,
		EstInputTokens:  512,
		EstOutputTokens: 512,
		Now:             time.Now,
	}, nil
}

// Complete runs one prompt through Converse and returns the sample with its real
// token cost (§2). The scope is accepted for interface conformance and P6
// discipline upstream; Bedrock itself is scope-blind, so entitlement is enforced
// before the call reaches here, not by it.
//
// Delegates at the endpoint-level MaxTokens, so the planner and reducer paths behave
// exactly as before this became two methods.
func (b *BedrockProvider) Complete(ctx context.Context, prompt, model string, scope quarry.Scope) (quarry.Sample, error) {
	return b.CompleteBounded(ctx, prompt, model, scope, b.MaxTokens)
}

// CompleteBounded is Complete with an explicit per-call output ceiling — the Budgeter
// half of P9 that actually binds (solver.go). maxOut of 0 means the model's own
// default, which is MaxTokens' existing convention: an absent ceiling, not a
// zero-token one.
func (b *BedrockProvider) CompleteBounded(ctx context.Context, prompt, model string, _ quarry.Scope, maxOut int32) (quarry.Sample, error) {
	in := &bedrockruntime.ConverseInput{
		ModelId: aws.String(model),
		Messages: []brtypes.Message{{
			Role:    brtypes.ConversationRoleUser,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: prompt}},
		}},
	}
	if maxOut > 0 {
		in.InferenceConfig = &brtypes.InferenceConfiguration{MaxTokens: aws.Int32(maxOut)}
	}

	out, err := b.Client.Converse(ctx, in)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("bedrock converse (%s): %w", model, err)
	}

	text := extractText(out)
	inTok, outTok := usage(out)
	cost := b.price(model, inTok, outTok)

	// TODO(§8): out.StopReason is brtypes.StopReasonMaxTokens when the ceiling cut the
	// answer off, so a truncated leaf is DETECTABLE here and is currently dropped. It
	// belongs on the record — an answer that stopped mid-sentence is a different claim
	// from one that finished — but neither Sample nor NodeOutcome has a field for it,
	// and adding a hashed field means every existing record stops hashing to its own
	// RunID. Carried by the reserve-measurement issue, which already accepts exactly
	// one hashed-field change; two independent ones in two commits would invalidate the
	// same records twice.

	now := time.Time{}
	if b.Now != nil {
		now = b.Now()
	}
	return quarry.Sample{
		Content:         text,
		Cost:            cost,
		Model:           model,
		ModelVersion:    model, // the resolved ID IS the version — explicit, never an alias (P8)
		CreatedAt:       now,
		HaloTokens:      inTok,  // input tokens are the halo — context replicated in (§8.2)
		GeneratedTokens: outTok, // output is what the node produced; the surface/volume ratio needs both
	}, nil
}

// Estimate is the advisory pre-call cost for admission control (§4, P4). It
// prices the configured token priors, not the actual prompt — a real prompt
// tokenizer would tighten it, but nothing depends on the number being right (a
// bad estimate yields a worse-scoped run, not a truncated one).
func (b *BedrockProvider) Estimate(_ string, model string) quarry.Units {
	return b.price(model, b.EstInputTokens, b.EstOutputTokens)
}

// price converts token counts to Units from the pinned sheet. A model with no
// price sheet costs zero here — see the Prices field note; the absence surfaces
// as an implausibly cheap node rather than a crash.
func (b *BedrockProvider) price(model string, inTok, outTok int) quarry.Units {
	p, ok := b.Prices[model]
	if !ok {
		return 0
	}
	dollars := float64(inTok)/1e6*p.InputPerMTok + float64(outTok)/1e6*p.OutputPerMTok
	return quarry.FromFloat(dollars)
}

// Ceiling prices an output-token ceiling from a spend allowance, inverting the same
// sheet price uses — the one Units<->tokens conversion in the system, and the reason
// the ceiling is sized here rather than in the core (Go rule 4 forbids the core a
// price sheet).
//
// Returns 0 — "the model's own default" — in the two cases where no honest number
// exists: an unlimited allowance, and a model absent from the sheet. The second
// matches how price already treats a missing sheet, and matters more than it looks: an
// unpriced model would otherwise divide by a zero rate and cap generation on a number
// nothing supports. Absence stays absence rather than becoming a value.
//
// Prices output only. Input is the halo, already spent by the time a ceiling could
// bound anything, so charging the ceiling for it would shrink every answer to pay for
// a prompt that was never optional.
func (b *BedrockProvider) Ceiling(model string, spend quarry.Units) int32 {
	if !spend.Limited() {
		return 0
	}
	p, ok := b.Prices[model]
	if !ok || p.OutputPerMTok <= 0 {
		return 0
	}
	// spend is micro-units; OutputPerMTok is per 1e6 tokens. tokens =
	// (spend/1e6) / (rate/1e6) = spend/rate, which keeps the arithmetic in one step
	// and out of float round-trips through Units.
	tokens := float64(spend) / p.OutputPerMTok
	return clampCeiling(tokens)
}

// clampCeiling bounds a priced token count into the usable range. Shared by every
// Budgeter so the two implementations cannot disagree about what a ceiling may be —
// they already disagree about price, which is the only thing they should.
func clampCeiling(tokens float64) int32 {
	if tokens < float64(MinLeafOutputTokens) {
		return MinLeafOutputTokens
	}
	if tokens > float64(MaxLeafOutputTokens) {
		return MaxLeafOutputTokens
	}
	return int32(tokens)
}

func extractText(out *bedrockruntime.ConverseOutput) string {
	if out == nil || out.Output == nil {
		return ""
	}
	msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, block := range msg.Value.Content {
		if t, ok := block.(*brtypes.ContentBlockMemberText); ok {
			b.WriteString(t.Value)
		}
	}
	return b.String()
}

func usage(out *bedrockruntime.ConverseOutput) (in, gen int) {
	if out == nil || out.Usage == nil {
		return 0, 0
	}
	if out.Usage.InputTokens != nil {
		in = int(*out.Usage.InputTokens)
	}
	if out.Usage.OutputTokens != nil {
		gen = int(*out.Usage.OutputTokens)
	}
	return in, gen
}

var (
	_ quarry.Provider = (*BedrockProvider)(nil)
	_ Budgeter        = (*BedrockProvider)(nil)
)

// Family returns the provider family of a Bedrock model ID — the segment after
// any cross-region prefix (us./global.) and before the model name. "us.anthropic.
// claude-haiku-4-5" and "anthropic.claude-fable-5" both return "anthropic";
// "us.meta.llama3-3-70b" returns "meta". This is the whole mechanism §5's judge
// independence needs: same family means correlated errors.
func Family(modelID string) string {
	parts := strings.Split(modelID, ".")
	for _, p := range parts {
		switch p {
		case "us", "global", "eu", "apac", "":
			continue
		default:
			return p
		}
	}
	return ""
}

// SameFamily reports whether two model IDs come from the same provider family.
func SameFamily(a, b string) bool { return Family(a) == Family(b) }
