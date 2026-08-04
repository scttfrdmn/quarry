# quarry ↔ agate integration (contract as discovered + quarry's decisions)

**Status:** contract discovered by reading the agate repo directly, then
**verified and corrected by agate in agate#265**. This document records what
agate exposes, the integration decisions that follow for quarry (Go), and the
work still blocked on agate. It supersedes the earlier request-form version.

Every point tags the governing principle in brackets, so a decision that
violates one can be rejected by name.

## Cross-refs

- **agate#265** — agate's verification of this contract. Confirms most of quarry's
  reading; issues three corrections (C1–C3 below) and lists four agate-side work
  items. Authoritative where it disagrees with quarry's original reading.
- **agenkit#711** — the OTel/telemetry convention (agenkit's domain, not agate's).

## Corrections from agate#265 (do NOT implement quarry's original reading here)

- **C1 — 402 is overloaded, so `402 → ErrCapExceeded` is WRONG.** agate returns
  `402 budget_rejected` for token-invalid, scoping-failure, AND missing-messages,
  not only cap breaches — 3 of 4 cases are not cap-exceeded. quarry must NOT map
  402 to `ErrCapExceeded` (§1 below is corrected). agate is adding a machine-
  readable code field so quarry stops string-matching `detail`; until it lands,
  `ChokepointProvider` treats an unclassified 402 as a transport fault (fails the
  run), not planned degradation — the conservative direction (a real cap breach
  wrongly failing the run is safe; a token error silently treated as degradation
  is not).
- **C2 — receipt `kind:"embedding"` is Python-only; the TS SPA union rejects it.**
  A Go emitter must NOT send `embedding` until agate adds it to the TS union.
  quarry only emits `kind:"llm"` today, so this does not bite yet — but do not
  widen to embedding/retrieval kinds before the SPA accepts them.
- **C3 — a run-record event already exists: `ArtifactEvent {run_id, url}`.**
  Provenance should EXTEND `ArtifactEvent` with an optional summary, not invent a
  parallel `provenance`/`record` event (§3 below is corrected to match).

---

## 0. The headline findings (these change Step 10's shape)

1. **The chokepoint is not an admission *check* — it is the whole call.** agate's
   chokepoint is a Python Lambda behind an IAM-authed Function URL. You POST
   `{idp_token, model, messages, max_tokens}` and it returns `{text, usage, cost,
   ...}`. It admits, calls the model, and meters in one request. So from quarry's
   side it is a **Provider that self-meters**, not a separate `Admitter`. [§3]

2. **agate emits no OpenTelemetry and has no decomposition-tree view.** OTel is
   explicitly agenkit's job ("it stops exactly where agate begins"), delegated at
   runtime to AWS AgentCore Observability. agate's SPA consumes a **flat event
   stream** (`RunEvent` union), not a span/trace tree. docs/design.md §9's "the OTel
   span tree *is* the decomposition tree, feeding the agate SPA" **does not hold
   against the agate that exists.** [§9 — divergence, see §5 below]

3. **agate's receipt has no verification/signature field.** It is
   `{type:"receipt", rows:[{label,kind,cost}], total}`, floats at 6 decimal
   places. quarry's verification receipt and content-hash have no home in it as-is
   — extending it is a real ask, not a field we can just populate. [§8 — see §3]

---

## 1. Admission / the chokepoint  [§3, §3.1, P4]

**What agate exposes** (Python, not importable from Go):
- Function URL Lambda: `chokepoint/handler.py:327` (`handler`), real logic at
  `:204` (`process`). Infra `infra/stacks/chokepoint.py:37`, auth
  `FunctionUrlAuthType.AWS_IAM` (SigV4), buffered, 30s.
- Request body: `{idp_token, model (optional; "auto" ok), messages, max_tokens}`.
  **Identity, tenant, and budget are derived server-side from the verified IdP
  token — never read from the body** (their SEC-1). Client token counts are
  ignored; input is re-estimated server-side (char/4).
- Response: `{text, usage:{inputTokens,outputTokens}, estimated_cost, cost,
  model, budget:{period,spend_usd,budget_usd}, model_route?}`.
- HTTP: **402 `budget_rejected` is OVERLOADED** (agate#265 C1) — returned for a
  cap breach AND for token-invalid, scoping-failure, and missing-messages. Only
  one of the four is a cap breach. 500 otherwise. Fail-closed — never falls
  through to an unmetered call.
- Pure decision core (the part a Go port would mirror): `cost/precall.py:121`
  `evaluate_cascade(model_id, input_tokens, max_tokens, nodes, ...)`, where
  `nodes` is an ordered `(label, spend, budget)` list broad→specific (user, then
  each scope ancestor). First node with `spend+est > budget` rejects. Spend keys
  live in DynamoDB as `tenant#user#period`, `tenant#period`,
  `tenant#scope#<node>#period`, `period = YYYY-MM` (`meter/parse.py`).

**quarry's decision (corrected per agate#265 C1):** implement
`provider.ChokepointProvider`, a `quarry.Provider` that SigV4-POSTs to the
Function URL. It is **Provider and Admitter fused**, matching agate's design.
- **402 does NOT map to `ErrCapExceeded`** — it is overloaded across four
  conditions, only one of which is a cap breach. Until agate adds the machine-
  readable code field (agate#265 work item), quarry treats an unclassified 402 as
  a **transport fault that fails the run**, not planned degradation. Rationale: a
  genuine cap breach wrongly failing the run is safe (a run that can't afford the
  call was going to degrade anyway); a token/scope error silently mistaken for
  "priced out and continue" is not — it would hide an auth failure as a gap. When
  the code field lands, map only the true cap-breach code → `ErrCapExceeded`.
- quarry's existing `BedrockProvider` stays as the standalone/no-agate path; the
  two are swappable because both satisfy `Provider`.

**How the two budget systems compose** (this was the open question; now
answered): they are **different axes and both must pass.**
- quarry's `*Ledger` does **apportionment** — dividing one run's budget across the
  tree so a node cannot starve its siblings or the reducer. agate does **not** do
  this.
- agate does **aggregate institutional caps** — user/tenant/scope spend vs budget
  for the month. quarry does not do this and must not try to (identity and budget
  are agate's, derived from the token).
- So a call passes quarry's local apportionment ledger **and** agate's cascade.
  quarry's ledger stays authoritative for tree shape; agate's chokepoint is
  authoritative for real money. quarry treats the chokepoint's `cost` as the
  actual to debit locally after the call.

**Unit mapping — confirmed by agate#265:** agate rounds USD to 6 dp
(`round(spend+est, 6)`), i.e. micro-dollars, 1:1 with quarry's int64 micro-units.
Convert with **`round(usd × 1e6)`, never `int()`** (truncation loses the last
micro-unit and would desync the local debit from agate's meter). quarry treats
`Units` as micro-dollars at the agate seam. [P8]

**Answered by agate#265:** discover the Function URL from the `ChokepointUrl`
stack output per deploy; size a per-node cap via `max_tokens` (agate prices
worst-case at `input + max_tokens×output_rate`); **pin an explicit versioned
model per node — not `auto`** — since `auto` routing is observable but
non-deterministic, which breaks P8 replay. **Blocked:** agate must vend a
`lambda:InvokeFunctionUrl` role for server-to-server callers (none exists today;
the browser uses Cognito). This is the single item blocking `ChokepointProvider`.

---

## 2. Scope / entitlement tags  [P6]

**What agate exposes:** `SessionTags` (`agate/tags.py:78`, frozen) —
`affiliation, tenant, courses, tier, role, admin_scope, scope`. Namespace
`agate:` (`agate/names.py`). Wire form `SessionTags.to_sts_tags()` →
`[{"Key":"agate:tenant","Value":...}, ...]`, `agate:scope` present only when
non-empty. **Scope narrowing is enforced by AWS IAM**, not app code: the
chokepoint does `assume_user_role(tags, user)` → `sts.assume_role` with tags as
session tags, and an IAM policy denies S3 reads outside `{tenant}/{scope}/`.
Hierarchy via `ancestors("chemistry/chem-101") → ["chemistry",
"chemistry/chem-101"]` (`agate/rag.py:96`).

**quarry's decision:** quarry's `Scope{Tags map[string]string}` maps onto agate's
tags directly — key `agate:tenant` etc. quarry keeps treating tags as **opaque**:
hash them into cache keys (§6), compare with `NarrowsTo`, nothing more. But note
the mismatch to resolve:
- quarry's `NarrowsTo` is subset-of-tags. agate's real narrowing for the
  `scope` path is **prefix/ancestor** (`chemistry` ⊇ `chemistry/chem-101`), and
  the true enforcement is IAM, which quarry cannot reproduce in-process.
- **Decision:** quarry will NOT try to reproduce IAM narrowing. For the
  scope-path tag specifically, quarry's local check uses agate's ancestor
  relation (prefix), and the authoritative denial still happens at the chokepoint
  via AssumeRole. quarry's local check is a fast-fail courtesy, not the security
  boundary. [P6 — the boundary stays with agate]

**Confirmed by agate#265:** quarry sends the raw `idp_token`; the chokepoint
re-derives tags. quarry holds `SessionTags` locally only, for its own
cache-key/telemetry use, and does NOT reproduce IAM/AssumeRole narrowing. Its
local `NarrowsTo` is a fast-fail courtesy; the security boundary stays with agate.

---

## 3. Receipt  [§8, P8]

**What agate exposes:** `Receipt` (`cost/meter.py:39`) = `{rows: [CostRow],
total}`; `CostRow` = `{label, kind, cost, model_id, input_tokens,
output_tokens}` but `to_event()` **drops model_id and tokens on the wire** →
`{type:"receipt", rows:[{label, kind∈{llm,embedding,retrieval,compute}, cost}],
total}`, 6 dp. TS twin `web/src/events/protocol.ts:113`. **No signature/hash/
verification field anywhere.** Integrity comes from the assumed-role ARN's
session name, not receipt contents.

**quarry's decision (built — `runevent.go`):** quarry emits agate's `receipt`
event shape as the **cost-receipt surface**, so quarry runs are legible to the
agate SPA's cost view unchanged. One refinement over the original plan: rows are
**one per SPENDING node, not per leaf** — internal reduce nodes cost money too, and
a leaves-only receipt would leave the reducer's spend inside the total with no line
explaining it. A receipt that does not add up is worse than no receipt (§8), so the
rows sum to `RunRecord.TotalCost()` exactly, asserted by test. But
quarry's `RunRecord` carries strictly more — verification receipt, unverified
list, tree, ledger, stability, content-hash `RunID`, adversarial findings — and
agate's receipt has nowhere to put it.
- **Decision:** the agate `receipt` event is a **projection** of quarry's
  `RunRecord`, not a replacement. quarry keeps `RunRecord` as its citable
  artifact (P8) and additionally emits the agate receipt event for the SPA. We do
  NOT flatten quarry's provenance into agate's lossy wire form.

**Provenance — corrected per agate#265 C3, and now LANDED on both sides.** A
run-record event already exists: `ArtifactEvent {run_id, url}`, so quarry's trust
summary **extends it with an optional `provenance` field** rather than inventing a
parallel event. agate has since **merged the field on both twins** —
`RunProvenance` in `web/src/events/protocol.ts` and in `agate/artifact.py`, folded
by `build_artifact` (`elif etype == "artifact" and isinstance(ev.get("provenance"),
dict)`). quarry emits it from `runevent.go`; the shape is
`{record_hash, verified, unverified, stability, adversarial_findings}`. This is
what lets the SPA badge trust beside cost — quarry's whole reason to exist.
**No longer blocked.**

Two properties of agate's twin constrain quarry and are pinned by tests:
- `RunProvenance` is pydantic `extra="forbid"`, so an **extra key is a hard
  validation error**, not an ignored field. `runevent_test.go` asserts the emitted
  key set EXACTLY, not just that the required keys are present.
- `stability` is `float`/`number`, **not nullable**. quarry cannot signal "not
  measured" in-band, so `StabilityKnown` is quarry-side only (`json:"-"`) and a
  caller whose stability is unpublishable should pass a **nil provenance** rather
  than a fabricated 0.0 — omitting the whole object is the only in-band way to
  say "not measured".

  This bullet used to name only one such case ("a caller with n=1"). Building the
  `ClaimComparator` seam produced **three**, all of which render as the same `0.0`
  and all of which the SPA would badge as "nothing replicated":

  1. **n=1** — no estimate exists at all (P7).
  2. **a rate of 0 with unassessed comparisons** — the free comparator declined to
     judge some pairs, so the truth is "nobody could tell", not "nothing
     replicated". Found by probing `ProvenanceOf` against the report's new fields:
     two replicates asserting one conclusion in different words give 2 clusters,
     0 stable, `unassessed=1`, and `StabilityRate()` returns `(0.0, ok=true)`. The
     three-state distinction survives the comparator seam and the report and was
     being flattened at the last hop to the UI.
  3. **a truncated comparison pass** — the clustering is admittedly under-merged,
     so any rate off it is provisional, non-zero rates included.

  The rule stops there deliberately (`fabricatedZero` in `provenance.go`). Under
  the free comparator almost every multi-replicate report has unassessed pairs, so
  "unassessed > 0 → omit" would suppress nearly every provenance object and throw
  away the well-defined `verified`/`unverified` counts with it — precisely the
  over-broad omission this non-nullable field already costs us. A **floor above
  zero is still published**, flagged `StabilityIsFloor`; a zero reached by asking
  about every pair is a real finding and is published too.

  `StabilityIsFloor`, `Unassessed` and `ComparedBy` are likewise `json:"-"`: a
  floor from normalized-string equality and a measurement from a model reach agate
  as the same bare float, so the attribution cannot travel in-band either. Adding
  a field is a coordinated two-repo change because of `extra="forbid"` above —
  which is the strengthened case for `stability: float|None` in agate#265.

**Verified cross-language.** quarry's NDJSON was fed through agate's real
`build_artifact` + `receipt_to_csv`: the artifact validates, `models` dedupes to
the two pinned versions, the receipt rows and `cost_total` agree with quarry's
ledger to the micro-unit, and `provenance` round-trips intact.

**Also (agate#265 C2):** `kind:"embedding"` exists in the Python receipt but not
the TS SPA union, so an emitter using it produces events the SPA rejects. quarry
emits only `kind:"llm"` today, so this does not bite — do not widen to
embedding/retrieval kinds until agate adds them to the TS union.

---

## 4. Telemetry / tree view  [§8.2, §9 — DIVERGENCE]

**Finding:** agate authors no OTel and has no tree view; its SPA is a flat
`RunEvent` stream (`route, model, answer, divergence, citation, artifact, code,
chart, cost, receipt, guardrail, policy_denied`). §9's premise — one OTel span
tree feeding Jaeger and the agate SPA — describes an agate that does not exist.
OTel is agenkit's, delegated to AWS AgentCore Observability.

**quarry's decision (records a divergence from docs/design.md §9):**
- quarry owns its telemetry emission regardless (`TelemetrySink` already exists).
  Emit **OTel spans, one per node, parent-span = parent-node**, so the trace IS
  the tree — but target the **OTel GenAI semconv / AgentCore** convention, not the
  agate SPA, because that is where OTel actually lives.
- To make a quarry run visible in the **agate SPA specifically**, quarry emits
  agate `RunEvent`s (§3, `runevent.go`). The **tree structure** has no
  representation in agate's flat protocol today — surfacing the decomposition tree
  in the agate SPA would need a new agate event type (`node`/`plan`) or is deferred
  to an agenkit-owned viewer.
- **Recommendation, unchanged:** do not build a quarry-specific tree view inside
  agate. Emit OTel for the tree, and agate `RunEvent`s for the cost/answer surface.

**Resolved, and not by any of the three integrations: `cmd/quarry`.** The
recommendation above is still right about agate, but the conclusion drawn from it —
that a live tree view therefore had to wait on agenkit/AgentCore — surveyed only
viewers quarry does not own. quarry's own CLI is a **third projection**, and it is
the only one whose protocol quarry controls:

- `quarry run` renders the tree live via the `Observer` seam, with per-node spend and
  verdict. Observer-only: the record's bytes are identical with and without
  `--quiet`, which is asserted by comparing run hashes (P8).
- `quarry show` is §9's click-through affordance — per-node prompt, output, verdict,
  cost, claims, gaps — over any record on disk.
- `quarry replay` proves the record reproduces, which no external viewer offers.
- `--fake` needs no credentials, no network and no money, so the surface is
  demonstrable before agate, agenkit or AgentCore integration exists.

The divergence about agate's protocol stands unchanged. What is withdrawn is the
inference that it blocked quarry from having a tree view at all.

**Answered by agenkit#711, now CLOSED — the exporter is NOT blocked.** The ask was
for agenkit's span/attribute convention, an importable `agenkit-go` tracing helper,
and a collector endpoint convention. The answer was the opposite of "wait":

- agenkit's own tracing is an **ad-hoc namespace** (`agent.{name}.process`, tracer
  scope `agenkit.observability`) carrying **none** of the seven attributes quarry
  needs — no model version, cost, token split, cache flag, verifier verdict or
  retry count exists as a span attribute in any of its five language
  implementations. There is nothing to conform to.
- No importable Go tracing helper. Its Go path sets `service.name` only; its Rust
  path **hardcodes `service.name=agenkit`, ignoring the caller** — so quarry must
  set its own resource attributes rather than inherit agenkit's.
- Explicit guidance: **"don't wait on us — emit raw OTel now,"** following **OTel
  GenAI semconv**, which is where agenkit intends to converge (tracked separately
  as **agenkit#715**) and what makes AgentCore/CloudWatch tooling work with no
  translation layer.
- Endpoint: agenkit's docs contradict themselves between `OTEL_EXPORTER_OTLP_
  ENDPOINT` and `OTLP_ENDPOINT`, and its `InitTracing` reads neither. quarry
  settles on the OTel-spec name, which is also what AgentCore expects.

**Built — `otel/` (subpackage, per Go rule 4).** `otel.Tracer` satisfies
`quarry.TelemetrySink`, so the core imports no SDK. **Wiring takes two steps, not
one:** `Executor.Sink` is necessary but not sufficient, because the executor calls
`Sink.Node` per node and **never calls `Sink.Run`** — that is the caller's job after
`NewRunRecord`, per `RunMetrics`' contract. For `AggregateSink` the omission is
benign (node data accumulates, `Snapshot` works); for a `Tracer` it is total silent
failure, since `Node` only buffers, so the field alone yields zero spans and no
error. Pinned by `TestNodeWithoutRunExportsNothing` and documented on the type;
deliberately not defended against in code, because a `Tracer` cannot know a run has
ended and a flush timer would need a clock. Standard `gen_ai.*` keys wherever one
exists; a documented `quarry.*` key only
where semconv has none, each justified on its own line in `otel/tracer.go` — an
unlisted custom key is a bug, because a private key is invisible to generic tooling
and silently loses what it was meant to record. Three load-bearing choices:

- **The tracer buffers, and builds the tree at `Run()`.** A `TelemetrySink` fires
  when a node *completes*, and children complete before parents — so the natural
  span nesting is simply unavailable at emission time. Buffering makes span
  parentage real; the price is that the trace is post-hoc, not live. The live
  alternative needs the executor to thread spans through `node()`, i.e. the core
  importing OTel, which Go rule 4 forbids.
- **Durations are real when a clock was injected, and labelled when it wasn't.**
  This bullet previously read "timestamps are not durations… per-node latency
  requires `NodeOutcome` to record it first; that is a core change, deliberately not
  smuggled in here." The core change was then made properly: `NodeOutcome.Timing`
  brackets each node, fed by `Executor.Clock`, and the exporter sets real span
  timestamps via `trace.WithTimestamp`, so a post-hoc trace still reads as a flame
  graph. An *untimed* node keeps a span but carries
  `quarry.timing.measured=false` — a custom key precisely because semconv has none
  (normal instrumentation always measures), and because `false` is the load-bearing
  value: without it, a trace-assembly artifact is indistinguishable from a genuine
  sub-millisecond latency. Timing is deliberately **excluded from the record's
  hash**: a duration is the one field replay cannot reproduce, and hashing it would
  make every replay report a divergence that never happened.
- **A gap sets `codes.Error`; a failed verification does not.** A truncated node
  did not complete, which is what OTel's error status means. A failed verification
  *completed* and returned a verdict — the check worked, the answer was bad.
  Conflating them would make a working verifier look like a broken run. The verdict
  is a three-value enum (`passed`/`failed`/`not_assessed`) for the same reason: a
  bool cannot hold "nobody checked" (§8). agenkit has no cross-language verdict
  vocabulary to align to, so quarry defines one and reconciliation is deferred to
  agenkit#715.

A trace is **not** the citable artifact: it carries wall-clock times and random
span IDs, so it is not byte-reproducible. The `RunRecord` remains the artifact
(P8); the trace is a second, lossy view — exactly like the agate `RunEvent`
projection — and nothing in quarry may read a decision back out of a span.

Tested with `tracetest.NewInMemoryExporter`: no collector, no network, no env var,
so `go test ./...` stays offline like the core. The invariants pinned are that span
parentage equals node parentage (asserted on span IDs, not names), the three-state
verdict, cost as int64 micro-units, gap-vs-verdict status, every key being either
`gen_ai.*` or `quarry.*`, and `Node` under `-race`.

**The gap this section recorded is now closed.** It read: "`NodeOutcome` carries no
token counts — they live on `Sample` — so a plain sink cannot report
surface-to-volume (§8.2, P1). `SampleAttributes` exists for a caller that holds the
`Sample`; closing it properly is a core telemetry-shape change." That change was
made. `NodeOutcome` carries `HaloTokens`/`GeneratedTokens`, the span emits
`gen_ai.usage.input_tokens`/`output_tokens` plus `quarry.surface_to_volume` straight
off the outcome, and `SampleAttributes` was **deleted rather than kept as an alias** —
two paths to the same attributes drift, and a span disagreeing with the record is
worse than a span with no tokens. Absence stays absence: a node that called no model
emits no usage keys at all, because semconv would read a zero as a measured zero.

---

## 5. Language / boundary summary

| Component | Language | quarry access |
|---|---|---|
| Chokepoint (admit+call+meter) | Python Lambda | **network only** — SigV4 POST to Function URL |
| Pure gate logic `cost/precall.py` | Python | reference only; Go re-port if in-process gate ever needed |
| Scope/ABAC `agate/tags.py` | Python | map to `Scope.Tags` with `agate:` keys; IAM does real narrowing |
| Receipt `cost/meter.py` | Python + TS twin | emit the `receipt` event shape |
| SPA | TypeScript, flat event stream | emit `RunEvent`s; no tree type today |
| CLI | Go (deploy-plan only) | not relevant to runtime |

**Bottom line:** quarry integrates over the **network**, as a
`ChokepointProvider` (Provider+Admitter fused) plus agate `RunEvent` emission,
and owns its own OTel/RunRecord. There is no Go package to import and no shared
IDL — the JSON shapes above are the contract, and they must be kept in lockstep
by hand.

---

## Status of the work

Built (quarry-side):
- ✅ **`Admitter` seam** (seams.go) — the in-process `*Ledger` satisfies it;
  documents the swap point. The fused chokepoint means the network path is a
  `Provider`, so the executor wiring waits — see the TODO(§10) on the interface.
- ✅ **`telemetry.go` — the aggregator half.** Pure, concurrency-safe
  `AggregateSink` + `Metrics`, Goodhart guardrail structural. Now reports the
  aggregate `SurfaceToVolume()` (admissible where cost-per-run is not: both terms are
  work done, so it cannot be improved by verifying less) and a `TimedNodes` **count**
  rather than a duration sum — parent brackets contain their children's, so summing
  would multiply-count the same wall-clock.
- ✅ **`otel/` — the exporter half.** Unblocked by agenkit#711's answer ("emit raw
  OTel now, GenAI semconv"), so built: one span per node, parent-span =
  parent-node, GenAI semconv keys plus a justified `quarry.*` namespace. §4 above
  has the reasoning and the trade-offs it accepts.
- ✅ **`provider.ChokepointProvider`** — **live-validated end to end** against the
  deployed Function URL: assume `agate-chokepoint-invoker` → SigV4 → POST → **200**
  with `{text, usage, cost}`, mapped to a `Sample` with the halo/generated split,
  `round(usd × 1e6)` cost, and the model version pinned. The C1 classifier was
  also verified live (a tokenless call returned `402 code="token_invalid"`, which
  quarry correctly treats as a fault, not a cap miss).
- ✅ **`provenance.go` + `runevent.go`** — the agate `RunEvent` projection. A
  completed `RunRecord` folds to `model` / `answer` / `receipt` / `artifact`
  events as NDJSON, the format agate's transport decodes. **Cross-validated
  against agate's real Python `build_artifact` and `receipt_to_csv`**, not a
  hand-written fake of them.

Resolved from the agate#265 work items:
- ✅ **invoker role** — vended; the live 200 above is the proof.
- ✅ **402 machine code field** — shipped; `classifyError` maps only
  `budget_exceeded` → `ErrCapExceeded`, everything else fails the run (C1).
- ✅ **`embedding` in the TS receipt union** — added by agate (C2). quarry stays
  `kind:"llm"`-only anyway, on purpose: it makes no embedding calls, and emitting
  a kind it does not incur would be fiction.
- ✅ **Provenance on `ArtifactEvent`** — merged on both twins (C2/C3, §3 above).

Still open, and NOT agate's:
- **Convention convergence with agenkit** — **agenkit#711 is closed**; the open
  item is **agenkit#715**, and it is not a blocker. quarry picked GenAI semconv and
  its own three-value verdict enum on agenkit's explicit invitation ("pick your own
  and we'll reconcile"); if agenkit later lands a different verdict vocabulary,
  `AttrVerified`'s three constants are the only thing to change. Note that #715's
  own proposed-work list — a normative `OTEL_CONVENTION.md`, model/version/cost/
  token/cache/retry attributes, a Go tree-node span helper, and a three-state (not
  boolean) verdict — is substantially what `otel/` now implements, so quarry's
  package is a working reference for it rather than a consumer waiting on it.
- **Live streaming of the two EXPORTS.** Per-node latency is **done**
  (`NodeOutcome.Timing` + `Executor.Clock`, §4 above), and the "second seam that fires
  on node entry" named here as the prerequisite is **also done** — `Observer.Enter`,
  which carries no OTel and no SDK. What remains is only that the OTel trace and the
  agate event stream still fold from a *completed* `RunRecord`; a live span emitter
  would need the core to import an OTel SDK, which the rules forbid. quarry's own TUI
  consumes the entry seam and streams today.
- **The decomposition tree has no representation in agate's flat protocol.** A
  quarry run's *cost* and *trust* surface in the SPA; its *shape* does not. That
  remains a recorded divergence from docs/design.md §9 — but it is no longer a blocker for
  anyone wanting to watch a run: see the `cmd/quarry` resolution in §4 above.
