package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	quarry "github.com/scttfrdmn/quarry"
)

// ChokepointProvider integrates quarry with agate's chokepoint (build step 10,
// docs/design.md §3; contract in docs/integration-requirements.md, verified in
// agate#265). The chokepoint is an IAM-authed Lambda Function URL that ADMITS,
// CALLS, and METERS in one request — so from quarry's side it is a Provider that
// self-meters, not a separate Admitter. agate is authoritative for real money
// (institutional caps from the IdP token); quarry's local *Ledger stays
// authoritative for tree-shape apportionment. Both must pass.
//
// The transport is behind the Doer seam so the request/response logic — SigV4
// signing, body mapping, the fail-closed 402 handling — is fully testable with a
// fake and NO live AWS. Only exercising it against the real Function URL needs
// credentials. This mirrors how bedrock.go keeps the network behind Converser.

// Doer is the slice of *http.Client this provider uses. A fake satisfies it in
// tests so no request leaves the process.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// chokepointRequest is the Function URL body (agate#265 §1). Identity, tenant and
// budget are derived server-side from idp_token and are NEVER sent in the body
// (agate SEC-1), so quarry passes the raw token and holds its own Scope tags
// locally only.
type chokepointRequest struct {
	IDPToken  string         `json:"idp_token"`
	Model     string         `json:"model"` // an explicit versioned ID, never "auto" — P8 replay needs determinism
	Messages  []chokeMessage `json:"messages"`
	MaxTokens int            `json:"max_tokens"`
}

type chokeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chokepointResponse is the success body. Cost is USD; quarry converts it to
// integral micro-units with round(usd*1e6) — never int() truncation, which would
// desync the local debit from agate's meter by up to a micro-unit (agate#265 §4).
type chokepointResponse struct {
	Text  string `json:"text"`
	Usage struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"usage"`
	EstimatedCost float64 `json:"estimated_cost"`
	Cost          float64 `json:"cost"`
	Model         string  `json:"model"`
	ModelRoute    string  `json:"model_route,omitempty"`
}

// chokepointError is the failure body. The Code field is the machine-readable
// classifier agate is adding (agate#265 C1): 402 is OVERLOADED across
// budget_exceeded, token_invalid, scope_denied and bad_request, so the HTTP
// status alone cannot tell a cap breach from an auth failure. Only
// CodeBudgetExceeded maps to ErrCapExceeded; every other code — and any 402 with
// no code, until the field ships — is a transport fault that FAILS the run.
type chokepointError struct {
	Error  string `json:"error"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// The classifier codes (agate#265 C1). Only the first is planned degradation.
const (
	CodeBudgetExceeded = "budget_exceeded"
	CodeTokenInvalid   = "token_invalid"
	CodeScopeDenied    = "scope_denied"
	CodeBadRequest     = "bad_request"
)

// ChokepointProvider calls agate's chokepoint over the network.
type ChokepointProvider struct {
	// URL is the Function URL, discovered per-deploy from the ChokepointUrl stack
	// output (agate#265 §1).
	URL string

	// HTTP is the transport; nil defaults to http.DefaultClient. A fake Doer makes
	// the whole provider testable with no network.
	HTTP Doer

	// Signer, Creds and Region drive SigV4. The Function URL is AWS_IAM-authed, so
	// every request is signed with the credentials of the invoker role agate vends
	// for server-to-server callers (agate#265 work item 1). Signer nil disables
	// signing — used only by fake-transport tests, never in a live call.
	Signer *v4.Signer
	Creds  aws.CredentialsProvider
	Region string

	// IDPToken is the caller's verified identity token, passed through so the
	// chokepoint derives identity/tenant/budget server-side.
	IDPToken string

	// MaxTokens sizes the per-node cap: agate prices worst-case at
	// input + max_tokens*output_rate, so this bounds a node's spend (agate#265 §1).
	MaxTokens int

	// Now stamps CreatedAt; the core never reads the clock, an edge provider may.
	Now func() time.Time
}

// serviceLambda is the SigV4 service name for a Lambda Function URL.
const serviceLambda = "lambda"

// NewChokepointProvider wires a live provider that assumes agate's invoker role
// (agate#265: ChokepointInvokerRoleArn) and SigV4-signs requests to the Function
// URL. The ambient AWS config supplies the base credentials that assume the role;
// region is the deploy region (signing service is lambda). idpToken is the
// caller's verified identity, passed through so the chokepoint derives
// identity/tenant/budget server-side.
//
// This is the only constructor that dials AWS; the zero-value struct with a fake
// Doer stays the no-network test path.
func NewChokepointProvider(ctx context.Context, url, invokerRoleARN, region, idpToken string, maxTokens int, now func() time.Time) (*ChokepointProvider, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	stsClient := sts.NewFromConfig(cfg)
	creds := aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(stsClient, invokerRoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "quarry-chokepoint"
	}))
	return &ChokepointProvider{
		URL:       url,
		HTTP:      http.DefaultClient,
		Signer:    v4.NewSigner(),
		Creds:     creds,
		Region:    region,
		IDPToken:  idpToken,
		MaxTokens: maxTokens,
		Now:       now,
	}, nil
}

// Complete sends one prompt through the chokepoint and returns the metered
// sample. A model MUST be an explicit versioned ID — the caller passes it, and
// "auto" is refused here rather than silently accepted, because a non-
// deterministic route cannot be replayed (P8). Scope is accepted for interface
// conformance; entitlement is agate's, derived from the token, not from tags.
func (c *ChokepointProvider) Complete(ctx context.Context, prompt, model string, _ quarry.Scope) (quarry.Sample, error) {
	if model == "" || model == "auto" {
		return quarry.Sample{}, fmt.Errorf("chokepoint: model must be an explicit versioned ID, not %q (P8 replay)", model)
	}

	body, err := json.Marshal(chokepointRequest{
		IDPToken:  c.IDPToken,
		Model:     model,
		Messages:  []chokeMessage{{Role: "user", Content: prompt}},
		MaxTokens: c.MaxTokens,
	})
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("chokepoint: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("chokepoint: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.sign(ctx, req, body); err != nil {
		return quarry.Sample{}, fmt.Errorf("chokepoint: sign: %w", err)
	}

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("chokepoint: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("chokepoint: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return quarry.Sample{}, classifyError(resp.StatusCode, respBody)
	}

	var out chokepointResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return quarry.Sample{}, fmt.Errorf("chokepoint: unmarshal response: %w", err)
	}

	now := time.Time{}
	if c.Now != nil {
		now = c.Now()
	}
	return quarry.Sample{
		Content:         out.Text,
		Cost:            usdToUnits(out.Cost),
		Model:           model,
		ModelVersion:    model, // the explicit ID IS the version; model_route is observability, not identity (P8)
		CreatedAt:       now,
		HaloTokens:      out.Usage.InputTokens,
		GeneratedTokens: out.Usage.OutputTokens,
	}, nil
}

// Estimate is the advisory pre-call cost (§4, P4). The chokepoint prices
// server-side and quarry does not know the price sheet, so this returns the
// worst-case estimate only if a per-model sheet is wired; absent that it returns
// zero and relies on the post-call actual. Kept simple: nothing depends on the
// estimate being right (a bad one yields a worse-scoped run, not a truncated one).
func (c *ChokepointProvider) Estimate(_ string, _ string) quarry.Units {
	return 0
}

// classifyError turns a non-200 into the right error kind. This is the corrected
// 402 handling (agate#265 C1): 402 is overloaded, so only an explicit
// budget_exceeded code is planned degradation (ErrCapExceeded); any other code,
// and any 402 with no code yet, is a transport fault that fails the run. The
// conservative direction — a real cap breach wrongly failing the run is safe; an
// auth/scope error silently treated as "priced out, continue" is not.
func classifyError(status int, body []byte) error {
	var ce chokepointError
	_ = json.Unmarshal(body, &ce) // best-effort; a missing/garbage body just leaves fields empty

	if status == http.StatusPaymentRequired { // 402
		if ce.Code == CodeBudgetExceeded {
			return fmt.Errorf("%w: chokepoint budget_exceeded: %s", quarry.ErrCapExceeded, ce.Detail)
		}
		// Overloaded 402 without the cap-breach code (or with none yet): NOT a cap
		// miss. Fail the run rather than mistake an auth/scope error for degradation.
		return fmt.Errorf("chokepoint 402 (code=%q): %s: %s", ce.Code, ce.Error, ce.Detail)
	}
	return fmt.Errorf("chokepoint http %d (code=%q): %s: %s", status, ce.Code, ce.Error, ce.Detail)
}

// sign applies SigV4 to the request. A nil Signer skips signing — for fake-
// transport tests only; a live call always signs, because the Function URL is
// AWS_IAM-authed and an unsigned request is rejected at the edge.
func (c *ChokepointProvider) sign(ctx context.Context, req *http.Request, body []byte) error {
	if c.Signer == nil {
		return nil
	}
	if c.Creds == nil {
		return fmt.Errorf("signer set but no credentials provider")
	}
	creds, err := c.Creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	// SigV4 requires a real signing time; a live call must have Now set. Guard rather
	// than sign at the zero time, which the edge would reject as skewed.
	if c.Now == nil {
		return fmt.Errorf("live signing requires Now to be set")
	}
	signTime := c.Now()
	return c.Signer.SignHTTP(ctx, creds, req, payloadHash, serviceLambda, c.Region, signTime)
}

// usdToUnits converts agate's 6-dp USD to integral micro-units with rounding, not
// truncation (agate#265 §4): int() would drop the last micro-unit and desync the
// local debit from agate's meter.
func usdToUnits(usd float64) quarry.Units {
	return quarry.Units(math.Round(usd * 1e6))
}

var _ quarry.Provider = (*ChokepointProvider)(nil)
