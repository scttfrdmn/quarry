package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// OpenAIProvider targets an OpenAI-compatible `POST /v1/chat/completions` — the wire
// shape the twin gateways (bucktooth in Go, rustynail in Rust) serve, and the fourth
// provider beside FakeProvider, BedrockProvider and ChokepointProvider (#10).
//
// IT IS ChokepointProvider'S SIBLING, NOT BedrockProvider'S, and that is the whole
// structure of this file. docs/integration-requirements.md §0 records the finding that
// agate's chokepoint "is not an admission CHECK — it is the whole call": it admits,
// calls the model, and meters in one request, so from quarry's side it is a Provider
// that SELF-METERS rather than a separate Admitter. A twin gateway at localhost is the
// same pattern with a different wire shape. Same admission and metering discipline,
// therefore: the remote reports the cost, quarry debits it, and a refusal is classified
// conservatively.
//
// The transport is behind the Doer seam — the same interface chokepoint.go declares, in
// the same package, deliberately shared rather than duplicated — so every byte of
// request and response mapping is testable against an httptest server with nothing
// leaving the process.

// The extension fields. Standard /v1 carries token counts and NO cost, so a gateway
// that meters must say so somewhere, and these two are what the twins add.
//
// QUARRY CONSUMES THIS EXTENSION; IT DOES NOT DEFINE IT UNILATERALLY (#10). Both names
// are being written into the twins' gateway issue rather than chosen here, and the one
// field this file reads WITHOUT that agreement is called out at classifyOpenAIError.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`

	// MaxTokens is omitted when zero — "the model's own default", the convention
	// BedrockProvider.MaxTokens and Budgeter.CompleteBounded already share. Sending an
	// explicit 0 would ask for a zero-token answer, which is a different request.
	MaxTokens int32 `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIResponse is the success body.
type openAIResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`

		// CostMicros is the gateway's ACTUAL cost in integer micro-units, 1:1 with
		// quarry.Units (#10 D1).
		//
		// A POINTER because absence and zero are different facts and the difference is
		// money. A gateway serving from its own cache legitimately reports 0, and that
		// is a real measurement quarry should debit; a gateway that reports nothing has
		// told quarry nothing, and recording that as free would make the cost receipt
		// false rather than merely imprecise (§8). Complete refuses the second and
		// accepts the first.
		CostMicros *int64 `json:"cost_micros"`
	} `json:"usage"`

	// ServedModel is the pinned version that ACTUALLY answered (#10 D1/D2). The twins
	// run fallback chains, so the model that served a call is knowable only to the
	// gateway.
	ServedModel string `json:"served_model"`
}

// openAIError is the standard /v1 error envelope. `code` and `type` are both read
// because implementations disagree about which carries a machine-readable classifier
// and the conservative default is the same either way.
type openAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// OpenAIProvider calls an OpenAI-compatible chat-completions endpoint that meters its
// own calls.
type OpenAIProvider struct {
	// BaseURL is the gateway root; "/v1/chat/completions" is appended. A localhost
	// gateway is the intended deployment, but nothing here assumes it.
	BaseURL string

	// APIKey is sent as a bearer token when non-empty. Empty is legitimate and is the
	// common case for a gateway on the loopback interface — an unauthenticated
	// localhost socket is the twins' own decision, not quarry's to second-guess.
	APIKey string

	// HTTP is the transport; nil defaults to http.DefaultClient.
	HTTP Doer

	// MaxTokens caps generation per call, ENDPOINT-LEVEL — one value for every call,
	// so it is the wrong instrument for a leaf and the right one for the planner and
	// reducer calls that have no allocation of their own (solver.go). Zero lets the
	// gateway default apply.
	MaxTokens int32

	// EstPerCall is the pre-call admission prior, in micro-units (#10 D3).
	//
	// A CALLER-STATED FLAT PRIOR, NOT A PRICE SHEET, and the distinction is what keeps
	// this inside D1. D1 forbids quarry PRICING these calls — deriving a cost from
	// tokens times a rate quarry believes in — because the gateway runs fallback chains
	// and a local sheet would produce a confidently wrong number. It does not forbid a
	// caller who knows their own deployment from stating roughly what a call runs, for
	// admission only.
	//
	// Zero means no prior, and the consequence is stated rather than hidden: admission
	// then admits everything and the cap binds only after the fact, on the actual the
	// gateway reports. That is advisory by construction and nothing gates on it (P4).
	// This number NEVER becomes a recorded cost — Sample.Cost always comes from the
	// gateway, asserted by test.
	EstPerCall quarry.Units

	// Now stamps CreatedAt. The core never reads the clock; a provider at the network
	// edge legitimately does, and taking it as a field keeps the call deterministic
	// under test.
	Now func() time.Time
}

// NewOpenAIProvider builds a provider against a gateway. It dials nothing until the
// first Complete.
//
// The model is NOT pinned here: the seam takes a model per call, and the planner,
// reducer and solver each name their own. Refusing an alias therefore has to happen at
// the CALL (see Complete), not at construction — unlike BedrockAdversary, whose
// family-independence rule is a property of the pair and can be checked once.
func NewOpenAIProvider(baseURL, apiKey string, maxTokens int32, estPerCall quarry.Units, now func() time.Time) *OpenAIProvider {
	return &OpenAIProvider{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTP:       http.DefaultClient,
		MaxTokens:  maxTokens,
		EstPerCall: estPerCall,
		Now:        now,
	}
}

// Complete sends one prompt and returns the sample the GATEWAY metered.
//
// Delegates at the endpoint-level MaxTokens, mirroring BedrockProvider.Complete, so the
// planner and reducer paths get the one configured ceiling and the leaf path gets its
// own through CompleteBounded.
func (o *OpenAIProvider) Complete(ctx context.Context, prompt, model string, scope quarry.Scope) (quarry.Sample, error) {
	return o.CompleteBounded(ctx, prompt, model, scope, o.MaxTokens)
}

// CompleteBounded is Complete with an explicit per-call output ceiling (the Budgeter
// half of P9, solver.go). maxOut of 0 means the gateway's own default — an absent
// ceiling, not a zero-token one.
//
// Scope is accepted for interface conformance. Entitlement on this path is the
// gateway's, as it is agate's on the chokepoint path; quarry holds its tags locally for
// cache keys and telemetry and enforces nothing with them (P6 — the boundary stays
// where it can be enforced).
func (o *OpenAIProvider) CompleteBounded(ctx context.Context, prompt, model string, _ quarry.Scope, maxOut int32) (quarry.Sample, error) {
	// An alias is refused at the call, the same refusal ChokepointProvider makes and for
	// the same reason: a non-deterministic route cannot be replayed (P8). "auto" is named
	// explicitly because it is the spelling agate's roster used and the one a caller
	// reaching for a fallback chain would try.
	if model == "" || model == "auto" {
		return quarry.Sample{}, fmt.Errorf("openai: model must be an explicit versioned ID, not %q (P8 replay)", model)
	}

	body, err := json.Marshal(openAIRequest{
		Model:     model,
		Messages:  []openAIMessage{{Role: "user", Content: prompt}},
		MaxTokens: maxOut,
	})
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("openai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	client := o.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("openai: transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("openai: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return quarry.Sample{}, classifyOpenAIError(resp.StatusCode, respBody)
	}

	var out openAIResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return quarry.Sample{}, fmt.Errorf("openai: unmarshal response: %w", err)
	}
	if len(out.Choices) == 0 {
		return quarry.Sample{}, fmt.Errorf("openai: response carried no choices")
	}

	served, err := o.servedModel(out, model)
	if err != nil {
		return quarry.Sample{}, err
	}
	cost, err := meteredCost(out)
	if err != nil {
		return quarry.Sample{}, err
	}

	// TODO(§8): finish_reason == "length" means the ceiling cut the answer off, so a
	// truncated leaf is DETECTABLE here and is dropped, exactly as it is on the Bedrock
	// path. Same blocker, same reason: neither Sample nor NodeOutcome has a field for it
	// and adding a hashed one stops every existing record hashing to its own RunID.
	// Carried by the issue that already accepts exactly one hashed-field change.

	now := time.Time{}
	if o.Now != nil {
		now = o.Now()
	}
	return quarry.Sample{
		Content:      out.Choices[0].Message.Content,
		Cost:         cost,
		Model:        model,
		ModelVersion: served, // what ANSWERED, not what was asked for — see servedModel
		CreatedAt:    now,
		// prompt/completion are the halo/volume split §8.2 needs: input is context
		// replicated in, output is what the node actually produced.
		HaloTokens:      out.Usage.PromptTokens,
		GeneratedTokens: out.Usage.CompletionTokens,
	}, nil
}

// servedModel enforces #10 D2 — NO SILENT FALLBACK SUBSTITUTION — and returns the
// version to record.
//
// This is a P8 REQUIREMENT, NOT A PREFERENCE, and it is the reason the extension exists.
// The twins run fallback chains, so a gateway can answer a request for one model with
// another. A record naming a model that did not produce the answer is not replayable,
// and the failure is INVISIBLE: the record looks entirely faithful. It is the same
// reasoning that already refused agate's `auto` routing (integration-requirements §1),
// one layer further out — quarry refuses the alias at its own end, so it must also
// refuse an answer that silently came from somewhere else.
//
// THREE CASES, and the third is the one worth arguing about:
//
//   - the served model matches what was pinned — recorded, nothing to do;
//   - it differs — the call FAILS, rather than being recorded under either name;
//   - neither `served_model` nor the standard `model` is present — the call also FAILS.
//
// The third refuses on ABSENCE, which is the strict direction, chosen because the
// alternative is to record quarry's own pinned model as the producer on no evidence at
// all. That is indistinguishable from case one in the record and is precisely the
// invisible failure D2 exists to prevent, so silence here cannot be read as agreement.
// The cost of the strict direction is one clear error against a gateway that reports
// neither field; the cost of the lax direction is an unreplayable record that looks
// fine.
func (o *OpenAIProvider) servedModel(out openAIResponse, pinned string) (string, error) {
	// served_model first, then the standard field. A stock /v1 server returns the
	// resolved model in `model` and knows nothing of the extension, so reading it is
	// what lets a conforming server be targeted at all.
	served := out.ServedModel
	if served == "" {
		served = out.Model
	}
	if served == "" {
		return "", fmt.Errorf("openai: response names no model (neither served_model nor model): "+
			"cannot confirm %q answered, and recording it unconfirmed would make an unreplayable "+
			"record look faithful (#10 D2, P8)", pinned)
	}
	if served != pinned {
		return "", fmt.Errorf("openai: gateway served %q for a call pinned to %q: a record naming a model "+
			"that did not produce the answer is not replayable, so a fallback substitution fails the "+
			"call rather than being recorded (#10 D2, P8)", served, pinned)
	}
	return served, nil
}

// meteredCost reads the gateway's actual (#10 D1).
//
// QUARRY DOES NOT PRICE THIS CALL. The twins run fallback chains, so quarry cannot know
// what a call cost or even which model served it — only the gateway knows, and that is
// the entire substance of the self-metering pattern. A local price sheet here would
// produce a confidently wrong number.
//
// Taken as an INTEGER at the wire, not a float USD, which sidesteps a conversion the
// agate seam had to specify carefully: there, cost arrives as 6-dp USD and must be
// converted with round(usd*1e6) rather than int(), because truncation drops the last
// micro-unit and desyncs the local debit from the remote meter. Micro-units on both
// sides removes the arithmetic rather than getting it right.
//
// A missing field is REFUSED rather than debited as zero. run.go already makes this call
// for an unpriced Bedrock model — "an unpriced model records every call as free, which
// makes the cost receipt a lie" — and a metering gateway that reports no cost is the same
// defect arriving from the other direction. A NEGATIVE cost is refused too: a debit below
// zero is a credit, and a gateway topping up quarry's ledger is not a thing the ledger
// should have to model.
func meteredCost(out openAIResponse) (quarry.Units, error) {
	if out.Usage.CostMicros == nil {
		return 0, fmt.Errorf("openai: response reported no usage.cost_micros: this provider does not " +
			"price calls locally (#10 D1), so an unreported cost would enter the record as free and " +
			"make the receipt false (§8)")
	}
	c := *out.Usage.CostMicros
	if c < 0 {
		return 0, fmt.Errorf("openai: gateway reported a negative cost of %d micro-units", c)
	}
	return quarry.Units(c), nil
}

// Estimate is the advisory pre-call cost for admission control (#10 D3, §4, P4).
//
// LOCAL BY NECESSITY: admission needs a number before the call exists, so it cannot come
// from the response. It returns the caller's flat prior and ignores both prompt and
// model, because pricing either would require the sheet D1 forbids. Zero — the zero
// value, and the honest answer absent a prior — means admission cannot bind, and the cap
// then binds on the actual instead.
func (o *OpenAIProvider) Estimate(_ string, _ string) quarry.Units {
	return o.EstPerCall
}

// Ceiling cannot price one, and returns 0 — "the gateway's own default" — always.
//
// THIS IS THE COST OF D1, AND IT IS A REAL LOSS RATHER THAN A DETAIL. Sizing an
// output-token ceiling from a spend allowance means inverting a price sheet, which is
// exactly what this provider is forbidden to hold: the gateway prices, and a fabricated
// rate would cap generation on a number nothing supports. BedrockProvider.Ceiling
// already returns 0 for a model absent from its sheet, for the same reason — absence
// stays absence rather than becoming a value.
//
// The consequence, stated plainly because it is invisible otherwise: on this path
// BudgetedSolver's TOKEN CEILING half does nothing, and its prompt half degrades
// honestly — leafPrompt says "no stated limit" rather than naming a budget, which is
// what it already does for an unlimited allowance. So a leaf here is bounded by the
// ledger and by MaxTokens, not by its own allocation. P9 holds at the plan gate and at
// admission; it does NOT reach the generation length on this path.
//
// TODO(#10): closing that needs the gateway to report a rate, or to accept a spend
// allowance and derive the ceiling itself — which is the self-metering pattern applied
// one field further and belongs in the twins' gateway issue, not in an invented local
// sheet.
func (o *OpenAIProvider) Ceiling(_ string, _ quarry.Units) int32 {
	return 0
}

// classifyOpenAIError turns a non-200 into the right KIND of error (#10 D5).
//
// CONSERVATIVE BY RULING, from agate#265 C1: agate's 402 turned out to be overloaded
// across four conditions and only one was a cap breach, so an unclassified refusal is a
// TRANSPORT FAULT THAT FAILS THE RUN, never planned degradation. The asymmetry is the
// argument — a genuine cap breach wrongly failing a run is safe, because a run that
// cannot afford the call was going to degrade anyway; a token or scope error silently
// recorded as "priced out and continued" hides an auth failure as a gap, and by the
// standing ruling a gap means TIME and nothing else.
//
// THE ONE FIELD READ WITHOUT A FROZEN AGREEMENT is the cap-breach code, and it reuses
// CodeBudgetExceeded — agate's spelling, already exported by chokepoint.go — rather than
// inventing a second vocabulary for one pattern. The twins' gateway issue specifies
// usage.cost_micros and served_model but says nothing about how a cap breach is
// signalled, so this is a proposal in code: until it is confirmed there, a gateway that
// spells it differently degrades to a fault, which is the safe direction and not a
// silent one.
func classifyOpenAIError(status int, body []byte) error {
	var oe openAIError
	_ = json.Unmarshal(body, &oe) // best-effort; a missing or garbage body leaves the fields empty

	code, kind := oe.Error.Code, oe.Error.Type
	if code == CodeBudgetExceeded || kind == CodeBudgetExceeded {
		return fmt.Errorf("%w: gateway %s: %s", quarry.ErrCapExceeded, CodeBudgetExceeded, oe.Error.Message)
	}
	return fmt.Errorf("openai http %d (type=%q code=%q): %s", status, kind, code, oe.Error.Message)
}

var (
	_ quarry.Provider = (*OpenAIProvider)(nil)
	_ Budgeter        = (*OpenAIProvider)(nil)
)
