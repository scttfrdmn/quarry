package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// Unit tests for the chokepoint provider using a fake Doer — no network, no AWS.
// They pin the corrected contract from agate#265 (esp. C1, the overloaded 402)
// so the provider is ready to point at the real Function URL once agate vends the
// invoker role. A failing test means the contract changed — update
// docs/integration-requirements.md in the same commit or revert.

// fakeDoer returns a canned response and captures the request it was handed.
type fakeDoer struct {
	status   int
	body     string
	lastReq  *http.Request
	lastBody string
	err      error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.lastReq = req
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.lastBody = string(b)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func testChokepoint(d *fakeDoer) *ChokepointProvider {
	return &ChokepointProvider{
		URL:       "https://example.lambda-url.us-east-1.on.aws/",
		HTTP:      d,
		IDPToken:  "tok",
		MaxTokens: 512,
		Now:       func() time.Time { return time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC) },
		// Signer nil: fake transport, no signing.
	}
}

const cpModel = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// --------------------------------------------------------------- success

func TestChokepointCompleteMapsResponse(t *testing.T) {
	d := &fakeDoer{status: 200, body: `{
		"text":"the answer",
		"usage":{"inputTokens":40,"outputTokens":12},
		"cost":0.000123,
		"model":"` + cpModel + `"
	}`}
	c := testChokepoint(d)
	s, err := c.Complete(context.Background(), "q", cpModel, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Content != "the answer" {
		t.Errorf("content: got %q", s.Content)
	}
	if s.HaloTokens != 40 || s.GeneratedTokens != 12 {
		t.Errorf("token split: halo=%d gen=%d", s.HaloTokens, s.GeneratedTokens)
	}
	// 0.000123 USD * 1e6 = 123 micro-units, rounded (not truncated).
	if s.Cost != quarry.Units(123) {
		t.Errorf("cost: want 123 micro-units, got %s (%d)", s.Cost, int64(s.Cost))
	}
	if s.ModelVersion != cpModel {
		t.Errorf("version must be the explicit ID, got %q", s.ModelVersion)
	}
}

func TestChokepointCostRoundsNotTruncates(t *testing.T) {
	// agate#265 §4: round(usd*1e6), never int(). 0.0000005 USD = 0.5 micro-units
	// must round to 1, not truncate to 0 — truncation would desync the local debit.
	d := &fakeDoer{status: 200, body: `{"text":"x","usage":{"inputTokens":1,"outputTokens":1},"cost":0.0000005}`}
	c := testChokepoint(d)
	s, err := c.Complete(context.Background(), "q", cpModel, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Cost != quarry.Units(1) {
		t.Errorf("0.5 micro-units must round to 1, got %d", int64(s.Cost))
	}
}

func TestChokepointPassesTokenNotTags(t *testing.T) {
	// Identity travels as the raw idp_token; tags are NEVER in the body (agate
	// SEC-1). The request body must carry the token and no tag material.
	d := &fakeDoer{status: 200, body: `{"text":"x","usage":{"inputTokens":1,"outputTokens":1},"cost":0.000001}`}
	c := testChokepoint(d)
	_, err := c.Complete(context.Background(), "q", cpModel,
		quarry.Scope{Tags: map[string]string{"agate:tenant": "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.lastBody, `"idp_token":"tok"`) {
		t.Errorf("request must carry idp_token, got %s", d.lastBody)
	}
	if strings.Contains(d.lastBody, "acme") || strings.Contains(d.lastBody, "tenant") {
		t.Errorf("tags must NOT be sent in the body (SEC-1), got %s", d.lastBody)
	}
}

// ----------------------------------------------- the overloaded 402 (C1)

func TestChokepoint402BudgetExceededIsCapError(t *testing.T) {
	// ONLY the budget_exceeded code is planned degradation → ErrCapExceeded.
	d := &fakeDoer{status: http.StatusPaymentRequired,
		body: `{"error":"budget_rejected","code":"budget_exceeded","detail":"month cap"}`}
	c := testChokepoint(d)
	_, err := c.Complete(context.Background(), "q", cpModel, quarry.Scope{})
	if !errors.Is(err, quarry.ErrCapExceeded) {
		t.Errorf("budget_exceeded must map to ErrCapExceeded, got %v", err)
	}
}

func TestChokepoint402OtherCodesAreFaultsNotCapErrors(t *testing.T) {
	// agate#265 C1: 402 is overloaded. token_invalid / scope_denied / bad_request
	// must NOT be treated as cap misses — mistaking an auth error for "priced out,
	// continue" would hide a denial as a gap. They fail the run.
	for _, code := range []string{CodeTokenInvalid, CodeScopeDenied, CodeBadRequest} {
		d := &fakeDoer{status: http.StatusPaymentRequired,
			body: `{"error":"budget_rejected","code":"` + code + `","detail":"x"}`}
		c := testChokepoint(d)
		_, err := c.Complete(context.Background(), "q", cpModel, quarry.Scope{})
		if err == nil {
			t.Errorf("%s: expected an error", code)
		}
		if errors.Is(err, quarry.ErrCapExceeded) {
			t.Errorf("%s must NOT be a cap error — it is an auth/request fault", code)
		}
	}
}

func TestChokepoint402NoCodeFailsClosed(t *testing.T) {
	// Until the code field ships, a 402 with no code is treated as a fault, not a
	// cap miss — fail-closed in the safe direction (agate#265 C1).
	d := &fakeDoer{status: http.StatusPaymentRequired, body: `{"error":"budget_rejected"}`}
	c := testChokepoint(d)
	_, err := c.Complete(context.Background(), "q", cpModel, quarry.Scope{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, quarry.ErrCapExceeded) {
		t.Error("an uncoded 402 must not be assumed a cap breach")
	}
}

func TestChokepoint500IsFault(t *testing.T) {
	d := &fakeDoer{status: 500, body: `{"error":"chokepoint_error"}`}
	c := testChokepoint(d)
	_, err := c.Complete(context.Background(), "q", cpModel, quarry.Scope{})
	if err == nil || errors.Is(err, quarry.ErrCapExceeded) {
		t.Errorf("500 is a transport fault, not a cap error, got %v", err)
	}
}

// --------------------------------------------------------------- P8 guard

func TestChokepointRefusesAutoModel(t *testing.T) {
	// "auto" routing is observable but non-deterministic, so it breaks replay (P8).
	// The provider refuses it rather than silently accept a non-replayable route.
	c := testChokepoint(&fakeDoer{status: 200, body: `{}`})
	if _, err := c.Complete(context.Background(), "q", "auto", quarry.Scope{}); err == nil {
		t.Error("model \"auto\" must be refused for P8 replay")
	}
	if _, err := c.Complete(context.Background(), "q", "", quarry.Scope{}); err == nil {
		t.Error("empty model must be refused")
	}
}

// --------------------------------------------------------------- transport

func TestChokepointTransportErrorPropagates(t *testing.T) {
	d := &fakeDoer{err: errors.New("dial tcp: refused")}
	c := testChokepoint(d)
	if _, err := c.Complete(context.Background(), "q", cpModel, quarry.Scope{}); err == nil {
		t.Error("a transport error must propagate")
	}
}
