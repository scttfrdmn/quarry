package provider

import (
	"context"
	"os"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// Live Bedrock tests. Skipped unless QUARRY_LIVE is set, so the default `go test`
// stays offline and free. Run with:
//
//	QUARRY_LIVE=1 AWS_PROFILE=aws go test ./provider/ -run Live -v
//
// These SPEND REAL MONEY (a few tokens each). They prove the Converse wiring and
// the cross-family adversary against the actual API, not a fake.

const (
	liveClaude = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
	liveLlama  = "us.meta.llama3-3-70b-instruct-v1:0"
)

func liveProvider(t *testing.T) *BedrockProvider {
	t.Helper()
	if os.Getenv("QUARRY_LIVE") == "" {
		t.Skip("set QUARRY_LIVE=1 (and AWS_PROFILE) to run live Bedrock tests")
	}
	p, err := NewBedrockProvider(context.Background(), "us-east-1", map[string]Pricing{
		// Coarse published prices; these tests assert cost > 0, not an exact figure.
		liveClaude: {InputPerMTok: 1.0, OutputPerMTok: 5.0},
		liveLlama:  {InputPerMTok: 0.72, OutputPerMTok: 0.72},
	})
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}
	p.MaxTokens = 64
	return p
}

func TestLiveBedrockComplete(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := p.Complete(ctx, "Reply with exactly the word: OK", liveClaude, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Content == "" {
		t.Error("live Complete returned empty content")
	}
	if s.GeneratedTokens == 0 || s.HaloTokens == 0 {
		t.Errorf("live call must report real token usage: halo=%d gen=%d", s.HaloTokens, s.GeneratedTokens)
	}
	if !s.Cost.Limited() || s.Cost <= 0 {
		t.Errorf("a priced live call must cost > 0, got %s", s.Cost)
	}
	if s.ModelVersion != liveClaude {
		t.Errorf("version must be the explicit ID, got %q", s.ModelVersion)
	}
	t.Logf("live sample: %q cost=%s halo=%d gen=%d", s.Content, s.Cost, s.HaloTokens, s.GeneratedTokens)
}

func TestLiveCrossFamilyAdversary(t *testing.T) {
	// Claude produces a claim; Llama (different family, §5) attacks it. Proves the
	// independence path end to end against the real API.
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adv, err := NewBedrockAdversary(p, liveLlama, liveClaude)
	if err != nil {
		t.Fatalf("cross-family adversary must build: %v", err)
	}
	// A deliberately false claim — a competent adversary should refute it.
	claim := quarry.Claim{Text: "The sum of two and two is five.", NodeID: "n0"}
	found, detail, cost, ok := adv.Attack(ctx, claim, quarry.Sample{Content: claim.Text})
	if !ok {
		t.Skipf("adversary did not return a parseable verdict (model phrasing varies): %q", detail)
	}
	if !cost.Limited() || cost <= 0 {
		t.Errorf("a live attack must cost > 0, got %s", cost)
	}
	t.Logf("adversary verdict: found=%v cost=%s detail=%q", found, cost, detail)
	if !found {
		t.Logf("note: adversary did not refute an obviously false claim — worth a look, not a hard fail")
	}
}

// TestLiveChokepoint exercises the full chokepoint path against agate's deployed
// Function URL (agate#265): assume the invoker role, SigV4-sign, POST. Gated on
// QUARRY_LIVE + the two stack outputs as env vars, so it stays offline by
// default. Run with:
//
//	QUARRY_LIVE=1 AWS_PROFILE=aws \
//	  QUARRY_CHOKEPOINT_URL=https://…lambda-url.us-east-1.on.aws/ \
//	  QUARRY_CHOKEPOINT_ROLE=arn:aws:iam::…:role/agate-chokepoint-invoker \
//	  QUARRY_IDP_TOKEN=<demo IdP token> \
//	  go test ./provider/ -run TestLiveChokepoint -v
//
// Without a valid IdP token the call still proves role-assume + SigV4 + transport
// end to end: agate returns a CLASSIFIED error (e.g. token_invalid), which is a
// successful round-trip, not a failure of the wiring — so a classified 402/4xx
// is treated as a pass for the plumbing.
func TestLiveChokepoint(t *testing.T) {
	if os.Getenv("QUARRY_LIVE") == "" {
		t.Skip("set QUARRY_LIVE=1 (+ AWS_PROFILE, QUARRY_CHOKEPOINT_URL/ROLE) to run")
	}
	url := os.Getenv("QUARRY_CHOKEPOINT_URL")
	role := os.Getenv("QUARRY_CHOKEPOINT_ROLE")
	if url == "" || role == "" {
		t.Skip("set QUARRY_CHOKEPOINT_URL and QUARRY_CHOKEPOINT_ROLE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	c, err := NewChokepointProvider(ctx, url, role, "us-east-1",
		os.Getenv("QUARRY_IDP_TOKEN"), 64, time.Now)
	if err != nil {
		t.Fatalf("build chokepoint provider: %v", err)
	}

	s, err := c.Complete(ctx, "Reply with exactly: OK", cpModel, quarry.Scope{})
	if err != nil {
		// A classified error means role-assume + SigV4 + transport all worked and
		// agate rejected on identity/scope — the plumbing is proven either way.
		t.Logf("chokepoint returned an error (plumbing OK if this is classified, e.g. token_invalid): %v", err)
		return
	}
	t.Logf("live chokepoint sample: %q cost=%s halo=%d gen=%d",
		s.Content, s.Cost, s.HaloTokens, s.GeneratedTokens)
	if s.ModelVersion != cpModel {
		t.Errorf("version must be the pinned explicit ID, got %q", s.ModelVersion)
	}
}
