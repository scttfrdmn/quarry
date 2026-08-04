package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	quarry "github.com/scttfrdmn/quarry"
)

// Unit tests for the Bedrock provider using a fake Converser — no network, so
// they run in CI with no AWS. The live path is exercised by TestLiveBedrock*,
// gated on QUARRY_LIVE so it only runs when a caller opts in with real creds.

// fakeConverser returns a canned response and records the input it was given.
type fakeConverser struct {
	reply       string
	inTok, out  int32
	err         error
	lastModelID string
	// lastMaxTokens is a POINTER so "no ceiling was sent" is distinguishable from "a
	// ceiling of zero was sent". Absence is not zero, and this is the field where the
	// difference is the whole assertion: maxOut 0 must leave InferenceConfig unset so
	// the model's own default applies.
	lastMaxTokens *int32
	calls         int
}

func (f *fakeConverser) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.calls++
	if in.ModelId != nil {
		f.lastModelID = *in.ModelId
	}
	f.lastMaxTokens = nil
	if in.InferenceConfig != nil {
		f.lastMaxTokens = in.InferenceConfig.MaxTokens
	}
	if f.err != nil {
		return nil, f.err
	}
	return &bedrockruntime.ConverseOutput{
		Output: &brtypes.ConverseOutputMemberMessage{Value: brtypes.Message{
			Role:    brtypes.ConversationRoleAssistant,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: f.reply}},
		}},
		Usage: &brtypes.TokenUsage{
			InputTokens:  aws.Int32(f.inTok),
			OutputTokens: aws.Int32(f.out),
			TotalTokens:  aws.Int32(f.inTok + f.out),
		},
	}, nil
}

const testModel = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

func testProvider(fc *fakeConverser) *BedrockProvider {
	return &BedrockProvider{
		Client: fc,
		Prices: map[string]Pricing{testModel: {InputPerMTok: 1.0, OutputPerMTok: 5.0}},
		Now:    func() time.Time { return time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC) },
	}
}

// --------------------------------------------------------------- Complete

func TestCompleteReadsContentAndCostsTokens(t *testing.T) {
	fc := &fakeConverser{reply: "the answer", inTok: 1_000_000, out: 1_000_000}
	p := testProvider(fc)
	s, err := p.Complete(context.Background(), "q", testModel, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Content != "the answer" {
		t.Errorf("want content from the reply, got %q", s.Content)
	}
	// 1M input @ $1 + 1M output @ $5 = $6.
	if s.Cost != quarry.FromFloat(6) {
		t.Errorf("want $6 cost from the price sheet, got %s", s.Cost)
	}
	// Halo/generated split is what P1's surface-to-volume ratio needs (§8.2).
	if s.HaloTokens != 1_000_000 || s.GeneratedTokens != 1_000_000 {
		t.Errorf("token split must be recorded: halo=%d gen=%d", s.HaloTokens, s.GeneratedTokens)
	}
}

func TestCompleteVersionIsExplicitNeverAlias(t *testing.T) {
	// P8: the record must name what produced it. The resolved model ID is the
	// version — never a floating alias.
	fc := &fakeConverser{reply: "x", inTok: 1, out: 1}
	p := testProvider(fc)
	s, _ := p.Complete(context.Background(), "q", testModel, quarry.Scope{})
	if s.ModelVersion != testModel {
		t.Errorf("ModelVersion must be the explicit resolved ID, got %q", s.ModelVersion)
	}
}

func TestCompletePropagatesConverseError(t *testing.T) {
	fc := &fakeConverser{err: errors.New("throttled")}
	p := testProvider(fc)
	if _, err := p.Complete(context.Background(), "q", testModel, quarry.Scope{}); err == nil {
		t.Error("a Converse error must propagate as a provider fault")
	}
}

func TestUnpricedModelCostsZeroNotCrash(t *testing.T) {
	// A model absent from the sheet surfaces as an implausibly cheap node, not a
	// panic — see the Prices field note.
	fc := &fakeConverser{reply: "x", inTok: 100, out: 100}
	p := testProvider(fc)
	s, err := p.Complete(context.Background(), "q", "us.meta.llama3-3-70b-instruct-v1:0", quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Cost != 0 {
		t.Errorf("an unpriced model prices at zero, got %s", s.Cost)
	}
}

// ------------------------------------------------------- CompleteBounded (P9, §2)

func TestCompleteBoundedSendsTheCeiling(t *testing.T) {
	// The half of P9-at-the-leaf that actually BINDS. A prompt asking for brevity is a
	// request the model may decline; InferenceConfig.MaxTokens is not.
	fc := &fakeConverser{reply: "short", inTok: 10, out: 10}
	p := testProvider(fc)
	if _, err := p.CompleteBounded(context.Background(), "q", testModel, quarry.Scope{}, 256); err != nil {
		t.Fatal(err)
	}
	if fc.lastMaxTokens == nil {
		t.Fatal("CompleteBounded must set InferenceConfig.MaxTokens")
	}
	if *fc.lastMaxTokens != 256 {
		t.Errorf("want the ceiling it was given, got %d", *fc.lastMaxTokens)
	}
}

func TestCompleteBoundedZeroMeansTheModelDefault(t *testing.T) {
	// Absence is not zero. maxOut 0 is MaxTokens' existing convention for "let the
	// model default apply", so it must leave InferenceConfig UNSET rather than send a
	// zero-token cap — which Bedrock would either reject or honour, and both are wrong.
	fc := &fakeConverser{reply: "x", inTok: 10, out: 10}
	p := testProvider(fc)
	if _, err := p.CompleteBounded(context.Background(), "q", testModel, quarry.Scope{}, 0); err != nil {
		t.Fatal(err)
	}
	if fc.lastMaxTokens != nil {
		t.Errorf("maxOut 0 must send no ceiling at all, sent %d", *fc.lastMaxTokens)
	}
}

func TestCompleteStillHonoursTheEndpointMaxTokens(t *testing.T) {
	// Complete now delegates to CompleteBounded, so this asserts the delegation did not
	// quietly drop the field-level ceiling. The planner and the reducer have no
	// allocation of their own and reach the provider through Complete, so they are the
	// callers this protects.
	fc := &fakeConverser{reply: "x", inTok: 10, out: 10}
	p := testProvider(fc)
	p.MaxTokens = 1024
	if _, err := p.Complete(context.Background(), "q", testModel, quarry.Scope{}); err != nil {
		t.Fatal(err)
	}
	if fc.lastMaxTokens == nil || *fc.lastMaxTokens != 1024 {
		t.Errorf("Complete must still apply the endpoint MaxTokens, got %v", fc.lastMaxTokens)
	}
}

// --------------------------------------------------------------- Family (§5)

func TestFamilyStripsCrossRegionPrefix(t *testing.T) {
	cases := map[string]string{
		"us.anthropic.claude-haiku-4-5-20251001-v1:0": "anthropic",
		"anthropic.claude-fable-5":                    "anthropic",
		"us.meta.llama3-3-70b-instruct-v1:0":          "meta",
		"global.amazon.nova-2-lite-v1:0":              "amazon",
	}
	for id, want := range cases {
		if got := Family(id); got != want {
			t.Errorf("Family(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSameFamilyAcrossRegionPrefixes(t *testing.T) {
	// The cross-region prefix must not fool the independence check: us.anthropic
	// and global.anthropic are the SAME family (§5).
	if !SameFamily("us.anthropic.claude-opus-4-8", "global.anthropic.claude-fable-5") {
		t.Error("same family across region prefixes must be detected")
	}
	if SameFamily("us.anthropic.claude-opus-4-8", "us.meta.llama3-3-70b-instruct-v1:0") {
		t.Error("anthropic and meta are different families")
	}
}

// --------------------------------------------------------- adversary (§5)

func TestAdversaryRefusesSameFamily(t *testing.T) {
	// §5 independence enforced at construction: an adversary may not judge its own
	// family. This is the check the core cannot make.
	p := testProvider(&fakeConverser{})
	_, err := NewBedrockAdversary(p, "us.anthropic.claude-opus-4-8", "us.anthropic.claude-haiku-4-5-20251001-v1:0")
	if !errors.Is(err, ErrSameFamily) {
		t.Errorf("same-family adversary must be refused, got %v", err)
	}
}

func TestAdversaryAcceptsDifferentFamily(t *testing.T) {
	p := testProvider(&fakeConverser{})
	adv, err := NewBedrockAdversary(p, "us.meta.llama3-3-70b-instruct-v1:0", testModel)
	if err != nil {
		t.Fatalf("a cross-family adversary must be accepted, got %v", err)
	}
	if adv == nil {
		t.Fatal("expected an adversary")
	}
}

func TestAdversaryReadsRefutedVerdict(t *testing.T) {
	fc := &fakeConverser{reply: "REFUTED the claim contradicts the source.", inTok: 10, out: 10}
	p := &BedrockProvider{Client: fc, Prices: map[string]Pricing{
		"us.meta.llama3-3-70b-instruct-v1:0": {InputPerMTok: 1, OutputPerMTok: 1},
	}, Now: func() time.Time { return time.Time{} }}
	adv, err := NewBedrockAdversary(p, "us.meta.llama3-3-70b-instruct-v1:0", testModel)
	if err != nil {
		t.Fatal(err)
	}
	found, _, _, ok := adv.Attack(context.Background(), quarry.Claim{Text: "a claim"}, quarry.Sample{})
	if !ok || !found {
		t.Errorf("a REFUTED reply must report found=true ok=true, got found=%v ok=%v", found, ok)
	}
}

func TestAdversaryUnparseableVerdictIsNotAssessed(t *testing.T) {
	// A reply that starts with neither REFUTED nor SOUND means the claim was
	// reached but not assessed: ok=false, distinct from survived (§8).
	fc := &fakeConverser{reply: "well, it depends...", inTok: 10, out: 10}
	p := &BedrockProvider{Client: fc, Prices: map[string]Pricing{
		"us.meta.llama3-3-70b-instruct-v1:0": {InputPerMTok: 1, OutputPerMTok: 1},
	}, Now: func() time.Time { return time.Time{} }}
	adv, _ := NewBedrockAdversary(p, "us.meta.llama3-3-70b-instruct-v1:0", testModel)
	found, _, _, ok := adv.Attack(context.Background(), quarry.Claim{Text: "a claim"}, quarry.Sample{})
	if ok || found {
		t.Errorf("an unparseable verdict must be ok=false found=false, got found=%v ok=%v", found, ok)
	}
}
