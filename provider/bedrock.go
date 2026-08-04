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
func (b *BedrockProvider) Complete(ctx context.Context, prompt, model string, _ quarry.Scope) (quarry.Sample, error) {
	in := &bedrockruntime.ConverseInput{
		ModelId: aws.String(model),
		Messages: []brtypes.Message{{
			Role:    brtypes.ConversationRoleUser,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: prompt}},
		}},
	}
	if b.MaxTokens > 0 {
		in.InferenceConfig = &brtypes.InferenceConfiguration{MaxTokens: aws.Int32(b.MaxTokens)}
	}

	out, err := b.Client.Converse(ctx, in)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("bedrock converse (%s): %w", model, err)
	}

	text := extractText(out)
	inTok, outTok := usage(out)
	cost := b.price(model, inTok, outTok)

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

var _ quarry.Provider = (*BedrockProvider)(nil)

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
