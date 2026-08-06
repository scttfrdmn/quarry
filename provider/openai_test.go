package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// #10's invariants. A LOCAL httptest SERVER rather than a fake Doer, unlike
// chokepoint_test.go, and the difference is deliberate: chokepoint.go's hard part is
// SigV4 signing, which a fake Doer lets you inspect, while this provider's hard part is
// the request and response BODIES. httptest exercises real JSON over a real socket with
// a real client, so a marshalling bug that a hand-written fake response would paper over
// still fails here. Offline either way — the socket is loopback and the suite dials
// nothing.

// testModel is bedrock_test.go's — one package, one pinned test model, so a version bump
// moves both provider suites together rather than leaving one asserting against a model
// nothing else in the package uses.

// gateway is a stub /v1 server. It records the last request body and returns a canned
// response, so a test can assert on both directions of one call.
//
// THE MUTEX IS NOT DEFENSIVE. An httptest handler runs on the server's own goroutine, and
// the executor solves siblings CONCURRENTLY, so any fixture that accumulates across calls
// is shared mutable state between goroutines by construction. The reconciliation test
// below found this with -race, which is the second time a stub written as though calls
// arrived one at a time has been wrong about that.
type gateway struct {
	srv *httptest.Server

	status int
	body   string

	mu      sync.Mutex
	gotBody []byte
	gotAuth string
	gotPath string
	calls   int
}

func newGateway(t *testing.T, status int, body string) *gateway {
	t.Helper()
	g := &gateway{status: status, body: body}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.gotBody, g.gotAuth, g.gotPath = b, r.Header.Get("Authorization"), r.URL.Path
		g.calls++
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		_, _ = w.Write([]byte(g.body))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// seen returns what the last call carried, under the lock.
func (g *gateway) seen() (body []byte, auth, path string, calls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gotBody, g.gotAuth, g.gotPath, g.calls
}

func (g *gateway) provider() *OpenAIProvider {
	return &OpenAIProvider{
		BaseURL: g.srv.URL,
		HTTP:    g.srv.Client(),
		Now:     func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
	}
}

// okBody builds a success response with the extension fields present.
func okBody(t *testing.T, content string, prompt, completion int, costMicros int64, served string) string {
	t.Helper()
	body := map[string]any{
		"model": served,
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"cost_micros":       costMicros,
		},
		"served_model": served,
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The halo/generated split and the gateway's cost land on the Sample (#10 D1).
func TestTheGatewaysUsageAndCostReachTheSample(t *testing.T) {
	g := newGateway(t, 200, okBody(t, "an answer", 512, 128, 4321, testModel))
	p := g.provider()

	s, err := p.Complete(context.Background(), "a question", testModel, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Content != "an answer" {
		t.Errorf("content: got %q", s.Content)
	}
	// The split §8.2's surface-to-volume needs: input is the halo, output the volume.
	if s.HaloTokens != 512 || s.GeneratedTokens != 128 {
		t.Errorf("token split: got halo=%d gen=%d, want 512/128", s.HaloTokens, s.GeneratedTokens)
	}
	// Integer micro-units straight through, 1:1 with Units and no float anywhere.
	if s.Cost != quarry.Units(4321) {
		t.Errorf("cost must be the gateway's reported micro-units: got %d, want 4321", int64(s.Cost))
	}
	if s.ModelVersion != testModel {
		t.Errorf("version: got %q", s.ModelVersion)
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt must be stamped from Now")
	}
	if _, _, path, _ := g.seen(); path != "/v1/chat/completions" {
		t.Errorf("path: got %q", path)
	}
}

// D1's real content: NO LOCAL PRICING. The recorded cost is the gateway's number even
// when it disagrees with anything quarry could have computed, and the local prior never
// becomes a cost.
//
// The fixture makes them impossible to confuse: a huge token count with a tiny reported
// cost, plus an EstPerCall three orders of magnitude away. Any local price sheet or
// fallback to the estimate produces a different number than 7.
func TestTheGatewaysCostIsDebitedNotAPriceQuarryComputed(t *testing.T) {
	g := newGateway(t, 200, okBody(t, "x", 900_000, 800_000, 7, testModel))
	p := g.provider()
	p.EstPerCall = quarry.Units(50_000)

	s, err := p.Complete(context.Background(), "a question", testModel, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Cost != quarry.Units(7) {
		t.Errorf("the gateway prices this call, not quarry (#10 D1): got %d micro-units for "+
			"1.7M tokens, want exactly the reported 7", int64(s.Cost))
	}
	if s.Cost == p.EstPerCall {
		t.Error("the advisory estimate must never become a recorded cost")
	}
}

// A gateway reporting no cost is REFUSED, not debited as zero — the same call run.go
// already makes for an unpriced Bedrock model, arriving from the other direction.
//
// And the case that makes the pointer necessary: an explicit ZERO is accepted, because a
// gateway serving from its own cache legitimately reports 0 and that is a real
// measurement. Both halves in one test so the distinction cannot be optimized away by
// treating absence as zero.
func TestAnUnreportedCostIsRefusedButAnExplicitZeroIsAccepted(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		// No cost_micros key at all.
		g := newGateway(t, 200, `{"model":"`+testModel+`","served_model":"`+testModel+`",
			"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		_, err := g.provider().Complete(context.Background(), "q", testModel, quarry.Scope{})
		if err == nil {
			t.Fatal("an unreported cost must fail the call, not enter the record as free")
		}
		if !strings.Contains(err.Error(), "cost_micros") {
			t.Errorf("the error must name the missing field: %v", err)
		}
	})
	t.Run("explicitly zero", func(t *testing.T) {
		g := newGateway(t, 200, okBody(t, "x", 1, 1, 0, testModel))
		s, err := g.provider().Complete(context.Background(), "q", testModel, quarry.Scope{})
		if err != nil {
			t.Fatalf("a reported zero is a real measurement (a cache hit at the gateway): %v", err)
		}
		if s.Cost != 0 {
			t.Errorf("cost: got %d, want 0", int64(s.Cost))
		}
	})
	t.Run("negative", func(t *testing.T) {
		g := newGateway(t, 200, okBody(t, "x", 1, 1, -5, testModel))
		if _, err := g.provider().Complete(context.Background(), "q", testModel, quarry.Scope{}); err == nil {
			t.Fatal("a negative cost is a credit; the ledger should not have to model one")
		}
	})
}

// D2, the P8 requirement: a silent fallback substitution FAILS THE CALL.
//
// The table's three rows are the three cases servedModel distinguishes, and the third —
// a response naming no model at all — is the one where the strict direction was a real
// choice: accepting it would record quarry's pinned model as the producer on no
// evidence, which is indistinguishable in the record from a call that really was served
// by it.
func TestAFallbackSubstitutionFailsTheCall(t *testing.T) {
	other := "us.meta.llama3-3-70b-instruct-v1:0"
	tests := []struct {
		name    string
		body    string
		wantErr bool
		wantVer string
	}{
		{
			name:    "served_model matches the pinned model",
			body:    okBody(t, "x", 1, 1, 10, testModel),
			wantVer: testModel,
		},
		{
			name:    "served_model is a different model — the fallback chain fired",
			body:    okBody(t, "x", 1, 1, 10, other),
			wantErr: true,
		},
		{
			name: "no served_model, but the standard model field agrees — a stock /v1 server",
			body: `{"model":"` + testModel + `","choices":[{"message":{"content":"x"}}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"cost_micros":10}}`,
			wantVer: testModel,
		},
		{
			name: "no model named anywhere — refused rather than assumed",
			body: `{"choices":[{"message":{"content":"x"}}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"cost_micros":10}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGateway(t, 200, tt.body)
			s, err := g.provider().Complete(context.Background(), "q", testModel, quarry.Scope{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("want a failed call: an unconfirmed producer makes an unreplayable " +
						"record look faithful (#10 D2)")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.ModelVersion != tt.wantVer {
				t.Errorf("recorded version: got %q, want %q", s.ModelVersion, tt.wantVer)
			}
		})
	}
}

// An alias is refused at the CALL, and refused before any request is made — a run that
// dialled the gateway and then rejected the answer would have spent money to learn what
// the model string already said (P8).
func TestAnAliasIsRefusedBeforeTheCallIsMade(t *testing.T) {
	for _, model := range []string{"", "auto"} {
		g := newGateway(t, 200, okBody(t, "x", 1, 1, 10, testModel))
		if _, err := g.provider().Complete(context.Background(), "q", model, quarry.Scope{}); err == nil {
			t.Fatalf("model %q must be refused (P8 replay)", model)
		}
		if _, _, _, calls := g.seen(); calls != 0 {
			t.Errorf("model %q: refused after %d call(s); the refusal must precede the spend", model, calls)
		}
	}
}

// D5: refusals classify CONSERVATIVELY. Only an unambiguous cap-breach code is planned
// degradation; everything else fails the run.
//
// This is agate#265 C1 generalized — its 402 was overloaded across four conditions and
// only one was a cap breach. An auth error recorded as "priced out and continued" would
// hide it as a gap, and only TIME produces a gap.
func TestOnlyAnUnambiguousCapBreachIsDegradationEverythingElseIsAFault(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantCap bool
	}{
		{
			name:    "code names the cap breach",
			status:  402,
			body:    `{"error":{"message":"over budget","code":"budget_exceeded"}}`,
			wantCap: true,
		},
		{
			name:    "type names the cap breach — implementations disagree about which field carries it",
			status:  402,
			body:    `{"error":{"message":"over budget","type":"budget_exceeded"}}`,
			wantCap: true,
		},
		{
			name:   "402 with no classifier at all — the overloaded case, fails closed",
			status: 402,
			body:   `{"error":{"message":"payment required"}}`,
		},
		{
			name:   "an auth failure must NOT read as priced-out",
			status: 401,
			body:   `{"error":{"message":"bad key","code":"invalid_api_key"}}`,
		},
		{
			name:   "a rate limit is not a cap breach",
			status: 429,
			body:   `{"error":{"message":"slow down","code":"rate_limit_exceeded"}}`,
		},
		{
			name:   "a server fault",
			status: 500,
			body:   `{"error":{"message":"upstream exploded"}}`,
		},
		{
			name:   "a body that is not even JSON",
			status: 502,
			body:   `<html>bad gateway</html>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGateway(t, tt.status, tt.body)
			_, err := g.provider().Complete(context.Background(), "q", testModel, quarry.Scope{})
			if err == nil {
				t.Fatal("a non-200 must be an error")
			}
			if got := errors.Is(err, quarry.ErrCapExceeded); got != tt.wantCap {
				t.Errorf("ErrCapExceeded: got %v, want %v — an unclassified refusal is a fault, "+
					"not planned degradation (#10 D5): %v", got, tt.wantCap, err)
			}
		})
	}
}

// D3: Estimate is local, advisory, and never the recorded cost. Zero is the honest
// answer with no prior, and it means admission cannot bind rather than that a call is
// free.
func TestTheEstimateIsLocalAdvisoryAndSeparateFromTheActual(t *testing.T) {
	p := &OpenAIProvider{EstPerCall: quarry.Units(2500)}
	if got := p.Estimate("a very long prompt indeed", testModel); got != quarry.Units(2500) {
		t.Errorf("estimate: got %d, want the stated prior 2500", int64(got))
	}
	// Deliberately insensitive to prompt and model: pricing either needs the sheet D1
	// forbids. Pinned so a later "improvement" that prices the prompt fails here and has
	// to argue with D1 rather than slip through.
	if p.Estimate("x", testModel) != p.Estimate(strings.Repeat("x", 10_000), "another-model") {
		t.Error("Estimate must not price the prompt or the model on this path (#10 D1/D3)")
	}
	var none OpenAIProvider
	if none.Estimate("x", testModel) != 0 {
		t.Error("no prior must estimate zero — admission cannot bind, which is not the same " +
			"as a free call")
	}
}

// Ceiling cannot price one, so it returns 0 — the gateway's own default — and the LOSS
// is asserted rather than left to be discovered.
//
// A future change that invents a local rate here would satisfy P9's generation-length
// half and violate D1, so this test's job is to make that trade explicit: it fails, and
// whoever changes it has to say which of the two they are overriding.
func TestNoCeilingCanBePricedWithoutAPriceSheet(t *testing.T) {
	p := &OpenAIProvider{}
	for _, spend := range []quarry.Units{0, 1, quarry.FromFloat(0.001), quarry.FromFloat(100), quarry.Unlimited} {
		if got := p.Ceiling(testModel, spend); got != 0 {
			t.Errorf("Ceiling(%d) = %d; the gateway prices these calls, so a local rate would cap "+
				"generation on a number nothing supports (#10 D1)", int64(spend), got)
		}
	}
	// The honest degradation, asserted at the seam it degrades: BudgetedSolver's prompt
	// half must say "no stated limit" rather than name a fabricated budget.
	prompt := leafPrompt(quarry.Problem{Statement: "does soil moisture matter?"}, 0, false)
	if !strings.Contains(prompt, "no stated length limit") {
		t.Errorf("an unpriceable ceiling must degrade to an unstated budget, not a guessed "+
			"number:\n%s", prompt)
	}
}

// The endpoint-level ceiling reaches the wire, and the zero convention holds: 0 is an
// ABSENT ceiling, so max_tokens must be omitted rather than sent as a zero-token
// request.
func TestTheOutputCeilingReachesTheWireAndZeroMeansAbsent(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		g := newGateway(t, 200, okBody(t, "x", 1, 1, 10, testModel))
		if _, err := g.provider().CompleteBounded(context.Background(), "q", testModel, quarry.Scope{}, 256); err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		body, _, _, _ := g.seen()
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req["max_tokens"] != float64(256) {
			t.Errorf("max_tokens: got %v, want 256", req["max_tokens"])
		}
	})
	t.Run("absent", func(t *testing.T) {
		g := newGateway(t, 200, okBody(t, "x", 1, 1, 10, testModel))
		// Complete delegates at MaxTokens, which is zero here.
		if _, err := g.provider().Complete(context.Background(), "q", testModel, quarry.Scope{}); err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		body, _, _, _ := g.seen()
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if _, present := req["max_tokens"]; present {
			t.Error("a zero ceiling means the gateway's own default; sending max_tokens:0 asks " +
				"for a zero-token answer, which is a different request")
		}
	})
	t.Run("Complete delegates at the endpoint ceiling", func(t *testing.T) {
		g := newGateway(t, 200, okBody(t, "x", 1, 1, 10, testModel))
		p := g.provider()
		p.MaxTokens = 900
		if _, err := p.Complete(context.Background(), "q", testModel, quarry.Scope{}); err != nil {
			t.Fatal(err)
		}
		var req map[string]any
		body, _, _, _ := g.seen()
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req["max_tokens"] != float64(900) {
			t.Errorf("Complete must carry the endpoint MaxTokens for the planner and reducer "+
				"paths: got %v, want 900", req["max_tokens"])
		}
	})
}

// A bearer token is sent when set and omitted when not — an unauthenticated loopback
// gateway is the twins' own decision, and sending "Bearer " would be a malformed header
// rather than an absent one.
func TestTheAPIKeyIsSentOnlyWhenSet(t *testing.T) {
	g := newGateway(t, 200, okBody(t, "x", 1, 1, 10, testModel))
	p := g.provider()
	p.APIKey = "sk-test"
	if _, err := p.Complete(context.Background(), "q", testModel, quarry.Scope{}); err != nil {
		t.Fatal(err)
	}
	if _, auth, _, _ := g.seen(); auth != "Bearer sk-test" {
		t.Errorf("auth header: got %q", auth)
	}

	g2 := newGateway(t, 200, okBody(t, "x", 1, 1, 10, testModel))
	if _, err := g2.provider().Complete(context.Background(), "q", testModel, quarry.Scope{}); err != nil {
		t.Fatal(err)
	}
	if g2.gotAuth != "" {
		t.Errorf("no key set: header must be absent, got %q", g2.gotAuth)
	}
}

// A response with no choices is an error, not an empty answer. An empty answer is a
// RESULT (§8) and the non-empty oracle judges it; a response carrying no choices at all
// is a malformed reply, and collapsing the two would let a broken gateway look like a
// model that had nothing to say.
func TestAResponseWithNoChoicesIsAFaultNotAnEmptyAnswer(t *testing.T) {
	g := newGateway(t, 200, `{"model":"`+testModel+`","choices":[],
		"usage":{"prompt_tokens":1,"completion_tokens":0,"cost_micros":10}}`)
	if _, err := g.provider().Complete(context.Background(), "q", testModel, quarry.Scope{}); err == nil {
		t.Fatal("no choices must be an error")
	}
}

// A transport failure propagates as a fault. It is neither a cap breach nor a gap.
func TestATransportFailurePropagates(t *testing.T) {
	p := &OpenAIProvider{BaseURL: "http://127.0.0.1:1", HTTP: http.DefaultClient}
	_, err := p.Complete(context.Background(), "q", testModel, quarry.Scope{})
	if err == nil {
		t.Fatal("a refused dial must be an error")
	}
	if errors.Is(err, quarry.ErrCapExceeded) {
		t.Error("a transport failure is not planned degradation")
	}
}

// D4: THE DEBITS RECONCILE TO THE RECORD'S TOTAL.
//
// A full tree through the executor with every leaf metered by the stub gateway, then the
// record's TotalCost() checked against the sum of what the gateway actually reported.
// runevent.go's receipt rows already have to sum to TotalCost() exactly, because "a
// receipt that does not add up is worse than no receipt" (§8) — this asserts the same
// property one layer earlier, where the number enters the ledger.
//
// The gateway reports a DISTINCT cost per call (a counter, not a constant) so the sum is
// only reachable by carrying each one through. A provider that debited a constant, or
// the estimate, or dropped a call's cost would produce a total that still looked
// plausible.
//
// The counter is under a mutex because THIS is the test that exercises concurrency: the
// executor solves siblings in parallel, so the handler runs on several goroutines at once
// and an unguarded `nextCost += 37` is a data race that -race caught. Which cost goes to
// which node is not asserted and cannot be — only the SUM is a property of the run.
func TestEveryReportedCostReachesTheRecordTotal(t *testing.T) {
	var mu sync.Mutex
	var reported []int64
	var nextCost int64 = 100

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		nextCost += 37 // distinct per call, so a dropped or constant debit cannot sum right
		cost := nextCost
		reported = append(reported, cost)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody(t, "answer to: "+req.Messages[0].Content, 40, 20, cost, testModel)))
	}))
	defer srv.Close()

	p := &OpenAIProvider{
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
		Now:     func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
	}

	caps := quarry.Caps{Spend: quarry.FromFloat(1), Latency: time.Hour}
	root := quarry.Problem{Statement: "How much does storage cost, how does it scale, " +
		"and what dominates the bill?"}
	l, err := quarry.NewLedger(caps, root.Scope)
	if err != nil {
		t.Fatal(err)
	}
	e := &quarry.Executor{
		Planner:  FakePlanner{},
		Solver:   BudgetedSolver{Provider: p, Model: testModel},
		Reducer:  quarry.ConcatReducer{Sep: "\n"},
		Now:      time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		MaxDepth: 2,
	}
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	rec := quarry.NewRunRecord(res, root, caps, quarry.ModeFresh)

	mu.Lock()
	defer mu.Unlock()

	// Non-vacuity: a one-call run would reconcile trivially, and the defect this guards
	// against (a dropped or constant debit) needs several distinct costs to be visible.
	if len(reported) < 3 {
		t.Fatalf("fixture made only %d gateway calls; the reconciliation is trivial below 3", len(reported))
	}
	var want int64
	for _, c := range reported {
		want += c
	}
	if got := int64(rec.TotalCost()); got != want {
		t.Errorf("the record's total must be the sum of what the gateway reported (#10 D4): "+
			"got %d, want %d across %d calls", got, want, len(reported))
	}
	// And the sum is not reachable by any constant: assert the costs really differed, or
	// the test above would pass against a provider that debited the first cost every time.
	if reported[0] == reported[len(reported)-1] {
		t.Error("fixture bug: every call reported the same cost, so a constant debit would pass")
	}
}

// The gateway sees the wrapped leaf prompt, and the wrapping stays in the SOLVER.
//
// record.go indexes recorded samples by the recorded Problem and looks them up by the
// prompt it is handed, so those two coincide only while what the executor RECORDS is the
// bare statement. This is the same guarantee replay_budgeted_test.go pins for the
// Bedrock path, re-asserted at the new provider because the failure mode is a replay
// that reports a divergence against a faithful record.
func TestTheBudgetPreambleGoesOverTheWireButNotIntoTheRecord(t *testing.T) {
	g := newGateway(t, 200, okBody(t, "x", 1, 1, 10, testModel))
	p := g.provider()

	prob := quarry.Problem{Statement: "does soil moisture matter?"}
	s := BudgetedSolver{Provider: p, Model: testModel}
	if _, err := s.Solve(context.Background(), prob, quarry.Allocation{Spend: quarry.FromFloat(0.01)}); err != nil {
		t.Fatal(err)
	}
	var req openAIRequest
	body, _, _, _ := g.seen()
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	sent := req.Messages[0].Content
	if !strings.Contains(sent, "BUDGET:") {
		t.Errorf("the leaf prompt must reach the gateway wrapped:\n%s", sent)
	}
	if !strings.Contains(sent, prob.Statement) {
		t.Errorf("the statement must survive the wrapping:\n%s", sent)
	}
	// No currency in the prompt (§2) — a model told a dollar amount prices tokens it
	// cannot see. Re-asserted here because this provider constructs no prompt of its own
	// and a future one might.
	if strings.Contains(sent, "0.01") || strings.Contains(sent, "$") {
		t.Errorf("no currency may appear in a leaf prompt (§2):\n%s", sent)
	}
}
