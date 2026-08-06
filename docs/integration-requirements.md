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

4. **The self-metering pattern generalizes — it is not an agate peculiarity.**
   Finding 1 was recorded as a fact about one Lambda. It turned out to be the
   shape of *every* gateway quarry talks to: the twin gateways (bucktooth in Go,
   rustynail in Rust) serve an OpenAI-compatible `POST /v1/chat/completions`,
   run fallback chains, and meter their own calls, which is finding 1 again with
   a different wire shape. So `OpenAIProvider` is **`ChokepointProvider`'s
   sibling, not `BedrockProvider`'s** — the axis that matters is not the vendor
   or the protocol but **who prices the call**. Where the remote prices, quarry
   debits what it is told and refuses to guess; where quarry holds the sheet
   (`BedrockProvider`) it prices locally. Getting that axis wrong is how a local
   price sheet ends up in front of a fallback chain, producing a number that is
   confidently wrong rather than absent. [§3 — see §1]

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

### A second self-metering gateway: the OpenAI-compatible wire  [#10]

The twin gateways serve `POST /v1/chat/completions`. Everything above about the
chokepoint applies unchanged — the remote admits, calls and meters in one
request (§0 finding 4) — so only the differences are recorded here.

**Two extension fields, and quarry CONSUMES this extension rather than defining
it.** Standard `/v1` carries token counts and no cost, so a gateway that meters
has to say so somewhere. Both names are written into the twins' gateway issue;
they are not quarry's to declare, and the shapes below are what quarry reads:

| Field | Type | Why it cannot be derived locally |
|---|---|---|
| `usage.cost_micros` | `int64`, **nullable** | The gateway runs a fallback chain, so quarry does not know which model served the call, let alone its rate |
| `served_model` | string | The model that *actually* answered — knowable only to the router that chose it |

- **Integer micro-units at the wire, 1:1 with `Units`.** This deliberately
  sidesteps the conversion the agate seam had to specify carefully (§1, "unit
  mapping": `round(usd × 1e6)`, never `int()`). Micro-units on both sides
  *removes* the arithmetic rather than getting it right.
- **`cost_micros` is nullable because absence and zero are different facts and
  the difference is money.** A gateway serving from its own cache legitimately
  reports `0`, and that is a real measurement to debit; a gateway that reports
  nothing has told quarry nothing, and recording that as free makes the receipt
  **false rather than merely imprecise** (§8). An unreported cost therefore
  fails the call. `run.go` already makes the same call for an unpriced Bedrock
  model — "an unpriced model records every call as free, which makes the cost
  receipt a lie" — and this is that defect arriving from the other direction.
- **A fallback substitution fails the call.** If `served_model` differs from the
  pinned model, quarry does not record it under either name. A record naming a
  model that did not produce the answer is not replayable, and the failure is
  **invisible** — the record looks entirely faithful. Same reasoning as refusing
  agate's `auto` (above), one layer further out: quarry refuses the alias at its
  own end, so it must also refuse an answer that silently came from elsewhere.
  **Absence of both `served_model` and the standard `model` also fails**, the
  strict direction, because the alternative records quarry's own pinned model as
  the producer on no evidence and that is indistinguishable in the record from a
  call really served by it. Silence cannot be read as agreement (P8).
- **The cap-breach signal is the one field read without a frozen agreement.**
  The gateway issue specifies the two fields above and says nothing about how a
  breach is signalled, so quarry reuses agate's `budget_exceeded` spelling as a
  *proposal in code*. Until it is confirmed, a gateway that spells it
  differently degrades to a transport fault — the safe direction under agate#265
  C1, and not a silent one.
- **What this costs: no output ceiling can be priced on this path.** Sizing a
  token ceiling from a spend allowance means inverting a price sheet, which is
  exactly what a self-metering remote forbids quarry from holding. So
  `Ceiling` returns 0 ("the gateway's own default") always, `BudgetedSolver`'s
  token-ceiling half does nothing here, and its prompt half degrades honestly to
  "no stated length limit". **P9 holds at the plan gate and at admission; it
  does not reach generation length on this path.** Closing that needs the
  gateway to report a rate, or to accept a spend allowance and derive the
  ceiling itself — the self-metering pattern applied one field further, and an
  ask for the gateway issue rather than an invented local sheet.
- **Admission uses a caller-stated flat prior, not a price sheet.** The
  distinction is what keeps a pre-call number legal at all: quarry may not
  *price* the call, but a caller who knows their own deployment may state
  roughly what one runs. Zero means no prior, and then admission admits
  everything and the cap binds after the fact on the actual — advisory by
  construction, and nothing gates on it (P4).

### Minting the root ledger from outside the process  [#11, P9, P4]

Everything above is about a call quarry makes *outward*. This is the other
direction: a supervising host spawns `quarry run` as a subprocess and has to
mint the root ledger from outside, because the cap, the deadline and the scope
tags are the host's to choose and not quarry's to assume.

**Precedence is explicit flag > environment > default, and THERE IS NO CONFIG
FILE.** That is stated here rather than left to be discovered, because adding a
file later must not change the order: a config file that outranked the
environment would silently re-point a host that had been setting the
environment correctly for months. If one is ever added it goes *below* the
environment, above the default.

| Knob | Flag | Environment | Default |
|---|---|---|---|
| Spend cap, integer micro-units | `--cap-micros` | `QUARRY_CAP_MICROS` | — |
| Spend cap, decimal (people) | `--cap` | — | `1.00` |
| Deadline, relative (people) | `--deadline` | — | unset |
| Deadline, absolute | `--due` | `QUARRY_DUE` | unset |
| Depth backstop | `--depth` | `QUARRY_DEPTH` | `3` |
| Scope tags | `--scope` | `QUARRY_SCOPE` | none |

Four rulings that a host integration depends on:

- **The spend cap crosses the boundary as an integer.** `--cap-micros` takes
  micro-units directly, because `Units` is `int64` and never float (Go rule 3)
  and apportionment uses largest-remainder distribution so shares sum exactly
  and replay is bit-stable (P8). A host that hands `1.00` to a shell to parse
  reintroduces float at the one edge that must not have it — and this seam is
  already micro-unit-native on the agate side (§1, "unit mapping"), so the two
  spellings meet without a conversion. `--cap` keeps the decimal form for
  humans, who are not crossing that boundary. **Setting both is a refusal, not
  a precedence rule**: they are two spellings of one cap, so a silent winner is
  how a run ships at a millionth of its intended cap with no error anywhere.
- **A host's deadline is absolute.** `--due` takes RFC3339 and nothing else,
  because the host owns the clock — it knows when the request arrived and what
  it promised — and quarry must not resolve a relative duration against an
  instant it is not supposed to read (Go rule 4). A due date with no
  `--deadline` makes the run *deferrable* (§3.1). **A due date must not imply a
  latency cap**, or a deferrable run would be recorded as due and priced as
  on-demand. **An expired due date is accepted, not refused** — it is not
  malformed, it is expired, which a queued request reaches by ordinary delay,
  and §3.1 says whatever exists must be returnable now. Nothing prices off
  deferrability yet; see design.md §3.1.
- **In host mode the defaults are refused.** Under `--events-json`, a run with
  no *explicitly set* cap in any denomination exits `2` and writes nothing to
  stdout. `Caps.Validate()` already requires one real cap (P9), but a defaulted
  `--cap` satisfies it silently — the gate passes while nobody decided
  anything, and the interactive default would spend a dollar of someone's money
  nobody authorised. Set-ness, not value: choosing `--cap 1.00` deliberately is
  accepted even though it is the default. Any denomination counts — a host that
  set only a deadline has conditioned the run on time, which §3.1 makes a
  first-class cap rather than a lesser one. Interactive `quarry run` keeps its
  defaults; a person at a terminal is not the failure mode this guards.
- **`--depth` is a backstop, not the design.** It is host-settable because a
  host must be able to bound the tree it pays for — not because raising it is
  how you get a better answer. Recursion is meant to be bounded by verifier
  availability (P2), and a tree that stopped because it hit the depth number is
  **under-verified rather than complete**; `RunBounds.BoundBy` records which of
  the two it was, and a replay inherits that rather than recomputing it (P8).
  Zero is legitimate: solve the root and do not decompose.

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

**Where the tags come from when there is no agate**  [#11 D4]: a supervising host
sets them with `--scope k=v,k=v` or `QUARRY_SCOPE` (the precedence table is in
§1, with the rest of the root-ledger contract — this half belongs to the same
decision). Nothing about the treatment changes: quarry hashes them into every
cache key and propagates them, and **never interprets one**. `--scope ""` clears
rather than falling through to the environment variable, because falling through
would attach tags the host explicitly declined, and a tag it did not choose is
authority it did not grant (P6). Scope never widens on descent, whoever set it.

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

**How to reconcile a receipt: in micro-units, never in floats.** That per-row
agreement "to the micro-unit" is exact — `unitsToUSD` emits a 6-dp decimal and Go
writes the shortest float64 that round-trips, so every `cost` and the `total` map
1:1 back to a `Units` value. **The rows do not sum to the `total` in float64**, and
that is arithmetic, not a bug in the emitter: each row is divided by `1e6`
separately while `TotalCost()` sums integers and converts once, so the errors
accumulate. A real 25-node run gives `0.08043700000000000849` against a total of
`0.08043699999999999461`.

So a consumer converts each value back with **`round(cost × 1e6)`** and compares
`int64`. `quarry.USDToUnits` and `quarry.ReceiptReconciles` are exported for exactly
this, so a host implements the rule rather than inventing one. Two constraints make
this normative rather than advisory:

- **`round`, not truncation.** `FromFloat` truncates and fails to round-trip 2884 of
  the first 200000 micro-unit values (`0.000249` → `248`). It is the same defect
  `provider.usdToUnits` already documents on the chokepoint seam, where `int()`
  would desync the local debit from the remote meter.
- **No float tolerance.** Any epsilon is a guess about tree size, because the error
  grows with the row count. There is no correct constant.

Found by bucktooth's consumer-side ask on quarry#9 (it requested a non-summing
receipt as a *malformed-input* fixture; it is what quarry emits for an ordinary
run). quarry's own test asserted this and passed for the whole life of the file,
because its fixture summed `1 + 2 + 3` — exact in binary floating point. See #18 and
`TestReceiptReconcilesOnRealCosts`, which carries a vacuity guard so a cleaner
fixture cannot silently disarm it again.

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
| Twin gateways (admit+call+meter) | Go / Rust | **network only** — `POST /v1/chat/completions` + two extension fields (§1) |

**Bottom line:** quarry integrates over the **network**, as a
`ChokepointProvider` (Provider+Admitter fused) plus agate `RunEvent` emission,
and owns its own OTel/RunRecord. There is no Go package to import and no shared
IDL — the JSON shapes above are the contract, and they must be kept in lockstep
by hand.

**Which contracts quarry owns, and which it only consumes** — worth stating
plainly because the two are maintained differently:

| | Owner | quarry's obligation |
|---|---|---|
| agate request/response, receipt, `RunEvent` | agate | Track it. A change there breaks quarry silently; only §3's hand-kept notes catch it |
| `usage.cost_micros`, `served_model` | the twins' gateway issue | **Consume, do not define.** Proposed there, read here; the cap-breach code is proposed *in code* and still unconfirmed |
| `quarry run --events-json` (§6) | **quarry** | Version it and keep it compatible — the only protocol here whose breakage is quarry's fault |

---

## 6. The host event stream: `quarry run --events-json`  [§9, P8 — quarry's own protocol]

**This section is not about agate.** Everything above records a contract someone
else owns, discovered by reading their code. This one quarry owns outright: it is
the stream a **supervising host** reads when it spawns quarry as a subprocess.
Frozen by [#9](https://github.com/scttfrdmn/quarry/issues/9); consumed by
**bucktooth** (Go) and **rustynail** (Rust), which is why it is written down as a
contract rather than left as a flag's behaviour.

It sits here rather than in §4 because it is the *fourth* projection of a record,
and the second one quarry controls. §4's table needs no revision — a host stream is
not a viewer — but the reason this exists at all is §4's finding restated one layer
out: **agate's protocol has no gap representation**, so the one fact a supervising
host most needs (did this answer cover the question, or part of it?) cannot ride on
any event agate accepts.

### D0 — verify the binary before you spawn it  [#13, P8]

**Where this landed, and a divergence named rather than fixed quietly.**
[#13](https://github.com/scttfrdmn/quarry/issues/13) asks for this in
"`docs/host-integration.md`'s capability-manifest section". **Neither exists** — there
is no `docs/host-integration.md` in this repo and no capability-manifest section
anywhere in it. Rather than create a second host-facing document beside this one, it
goes here, which is the section a host author is already reading. If a
capability-manifest section is ever written, this is the text to move.

Releases are signed with **cosign keyless**: no long-lived private key exists, and the
signing identity *is* `.github/workflows/release.yml` at the release tag, certified by
GitHub's OIDC issuer.

**The identity constraints are part of the contract, not decoration.** A
`cosign verify-blob` without `--certificate-identity` and `--certificate-oidc-issuer`
succeeds against *any* valid Sigstore signature by *anyone* — it proves a file was
signed, not that quarry signed it. A host that spawns on the strength of an unpinned
verify has a gate that always says yes:

```
cosign verify-blob quarry_v0.1.0_linux_amd64 \
  --bundle quarry_v0.1.0_linux_amd64.cosign.bundle \
  --certificate-identity "https://github.com/scttfrdmn/quarry/.github/workflows/release.yml@refs/tags/v0.1.0" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

To accept any release rather than one tag — the usual case for a host that upgrades —
keep the workflow pin and relax only the tag:

```
  --certificate-identity-regexp '^https://github\.com/scttfrdmn/quarry/\.github/workflows/release\.yml@refs/tags/v'
```

Every binary and `SHA256SUMS` ships a `.cosign.bundle` beside it (signature,
certificate and inclusion proof in one file, verifiable offline). SLSA build provenance
is attested per binary: `gh attestation verify <binary> --repo scttfrdmn/quarry`.

**The workflow verifies its own published one-liner**, in a job that neither built nor
signed the assets and that downloads them from the release — and it asserts that a
*foreign* identity is **refused**, because a constraint that accepts everything is
indistinguishable from no constraint if you only ever test the success path.

**Which build wrote a record is on the record, not on the wire.** `RunRecord.Producer`
carries `"quarry-go/v0.1.0 (abc1234)"`; `StreamEvent.Producer` stays `"quarry-go"` and
is **not** getting a version — D2 froze the frame, and appending to it would make every
release a wire change for bucktooth and rustynail in order to carry a fact the citable
artifact already carries. **An unstamped development build sets no `Producer` at all**
rather than `"dev"`: absence is not zero, and the field is `omitempty` so every record
written before it existed still hashes to its own `RunID` (P8).

### D1 — the events own stdout; humans move to stderr

`--events-json` makes NDJSON **the only thing on stdout**. The live tree and the
run summary go to stderr, unchanged in content.

fd 3 was considered and rejected: it is harder to consume from every host language
(Rust's `std::process` has no third-pipe affordance at all) for no gain over the
standard Unix contract. The redirect in `cmd/quarry/run.go` is a `human io.Writer`
variable rather than a branch at each print site, because a missed site is not a
visible defect — it is a stray line inside a host's NDJSON, reported as a parse
error against quarry.

**Ordering is part of the contract.** The events are written *after* the record
file, because `ArtifactEvent.url` names that file and a host that read the stream
and went looking would otherwise race a file that did not exist yet. They fold
from the **record**, never from the executor's result, so the stream cannot
disagree with the artifact it points at (§8).

### D2 — a versioned, frozen frame

```
{"type":"quarry_stream","version":1,"producer":"quarry-go"}   first
  ... agate's events, byte-identical to what agate receives ...
{"type":"quarry_outcome", ...}                                last
```

| rule | why |
|---|---|
| every line `\n`-terminated, **including the last** | a host reading line-wise otherwise cannot tell a complete final event from a truncated one — the crashed-vs-finished distinction |
| **adding a kind is a MINOR bump** | agate's reducer and `build_artifact` both dispatch on `type` and skip the unknown (verified against the real Python), so a conforming consumer cannot break |
| **changing or removing a field is MAJOR**, and `version` goes up | that is the change a consumer cannot absorb |
| `type` is the discriminant; a line with no string `type` is an **error**, not an ignorable object | |
| **do not key on line position** | a future kind may follow the outcome, which is why `TerminalOutcome` scans backwards |

`HostRunEvents` is a **separate fold** from `RunEvents`, not a flag on it: #9's
non-goals forbid changing what agate receives, and the middle of the frame is
asserted byte-identical rather than assumed. Both new kinds are namespaced
`quarry_*` — partly because agate's models declare `extra="forbid"`, and partly
because a bare `version` is exactly the name a second producer would also pick
with a different meaning.

**The two frames are not symmetric in what they prove.** The version lets a host
**refuse** a stream; the outcome lets it **trust** one it read to EOF. NDJSON
yields whole lines whether or not the producer finished, so the outcome event's
**absence** is the only in-band signal that a run was killed — and a host reading a
*vendored fixture from a file* has no exit code to fall back on.

### D3 — provenance extends `ArtifactEvent`; absence is not zero

No parallel provenance event (agate#265 C3, §3 above). And because agate's
`stability` is non-nullable, **quarry says "not measured" by omitting the whole
provenance object.**

> **A host must treat absent provenance as UNMEASURED, never as zero.** A rendered
> 0% badges "nothing replicated" on a run where nobody could tell. `cmd/quarry`
> passes provenance only when `StabilityKnown`, and every corpus case today has it
> absent — nothing in the CLI wires replication, and one run is one sample (P7).

Two more sites where absence is not zero, both on the outcome event:

- **`cap_micros: -1`** means no spend cap. Not `0` — `0` reads as *a cap of
  nothing*, which would make an unlimited run look infinitely overspent. `-1` is
  `Units(Unlimited)`'s own value, not a sentinel swap at the wire.
- **`bound_by: ""`** means no denomination bit. It is a measurement, so it is
  **emitted, not omitted**: a host that saw no key could not tell "nothing bound
  this run" from "this producer does not report it".

`total_micros` and `cap_micros` are the only figures on the stream that are **not
floats** — this event is quarry's own, so it carries the ledger's `int64`
micro-units and a host has nothing to reconcile. Everything in agate's union prices
in USD because agate does; reconcile those per §3, in micro-units, `round` never
truncate, and never with a float epsilon.

**Gaps and unfunded are different denominations and must never be summed.** Only
TIME produces a gap; spend exhaustion produces *unfunded* nodes, which is planned
degradation inside authority. A host that added them would offer more time where
money was needed — the §3.1 mislabelling `ErrRecordedUnfunded` exists to prevent,
one layer out.

### D4 — the exit codes are a vocabulary, not a boolean

```
0  complete             finished inside its caps, with an answer
                        — AND cap-bound degradation, by ruling
1  fault                crash, provider error, unreadable record — a MALFUNCTION
2  usage error          bad flags, refused caps; nothing ran
3  time-truncated       a deadline cut it short; the record has gaps (§3.1)
4  no answer            nothing was affordable, or every node came back empty
```

**The numbers are part of the contract and may not be reshuffled.** Two hosts in
two languages branch on them, so a renumbering is a silent misread rather than a
build error. `cmd/quarry/main.go` holds the constants; `cmd/quarry/main_test.go`
pins them, and pins the mapping against every corpus case's hand-written
`exit_code`.

- **1 and 2 keep their conventional meanings**, because they were already
  load-bearing: shells, CI and `go test`-style tooling all read 1 as failure and 2
  as misuse. Only the new codes are new.
- **The line between 1 and 2 is whether anything was *attempted*.**
  `quarry show nonexistent.json` is **1**, not 2 — the invocation was well-formed
  and the read failed.
- **`no-answer` is 4, not 1**, even though the cause is usually spend: the record
  is faithful and citable — it accurately records that nothing was affordable — so
  it is an outcome, not a fault.
- **The default is `fault`.** An error the mapping does not recognise is a
  malfunction until something says otherwise; a softer default would let a new
  failure path reach a host as an ordinary outcome, and a host that believes a
  fault was an outcome will build on a broken answer. The same holds for an
  unmapped `Outcome` value: `statusErr` has no fall-through to nil.

**Cap-bound degradation is deliberately not in this table**, and that is the ruling
rather than an omission. A degraded run that produced an answer exits **0** — the
cap did exactly what P4 promises, and a non-zero status would make the contract
look like a malfunction every time it worked. A host that wants to know reads
`bound_by`, `unfunded` and `total_micros` off the outcome event, which is why that
event carries them.

One of these was **found by running the binary**, not by the table: `quarry run
--cap 0` — a plain flag mistake, nothing run — exited **1**, so the documented "2
usage error" was true of `main`'s arg parsing and false of every refusal that
returned an error. A host would have escalated a user's typo as a quarry fault.
Fixed with an `errUsage` sentinel (`usageErrf`); if you implement against an older
build, verify rather than assume.

### D5 — live per-node events, on the same stdout, additive under version 1

Added by [#14](https://github.com/scttfrdmn/quarry/issues/14), which the frame above was
written to permit. `quarry run --events-json --live-events` emits two more kinds **as the
run happens**, ahead of the fold:

```
{"type":"quarry_stream","version":1,"producer":"quarry-go"}   at run START
{"type":"quarry_node_enter","node_stream_version":1, ...}     as each node begins
{"type":"quarry_node_exit","node_stream_version":1, ...}      as each node finishes
  ... then the fold, with NO second frame ...
{"type":"quarry_outcome", ...}                                last
```

The fold is unchanged. This is a projection of the **`Observer` seam** (§9), which fires on
node *entry* and completion, not a widening of the record fold — a host learns nothing from
a fold until the run ends, and that is what live events are for.

**No version bump, by D2's own rule**: adding a kind is MINOR. If you already skip unknown
kinds you already conform, and ignoring both live kinds costs you nothing but live progress.

| kind | version field | since | ignoring it |
|---|---|---|---|
| `quarry_node_enter` | `node_stream_version` | 1 | conforms |
| `quarry_node_exit` | `node_stream_version` | 1 | conforms |

**Why one stream and not a second destination.** A second fd or file forces a host to
correlate two streams with no ordering guarantee between them, and the ordering *is* the
value: a node's live entry must be readable as preceding the fold that summarises it. One
ordered stream gives that for free. `--live-events` without `--events-json` is **refused**
(exit 2) rather than given a destination — its only other home is the human's stdout, which
is the defect D1 exists to prevent.

**Two consequences a host must implement:**

- **Exactly one `quarry_stream` per stream, and with live events on it arrives at run
  start** — a host must be able to refuse a stream before it parses anything, and a frame
  written after the first live event came too late. The fold therefore omits it
  (`HostRunEventsNoFrame`). Two frames mean two concatenated streams.
- **The live kinds carry their own version**, separate from `version`, and carried **per
  event** rather than in the frame. Separate because a live dashboard and a supervisor
  folding the terminal outcome are different consumers with different tolerances, and
  coupling them would force a major bump on hosts that never read a live event. Per-event
  because a host may attach mid-stream — tailing a log, attaching to a running job — where
  the frame has already gone past.

**Absence is not zero, at three new sites**, and each value is one no measurement can
produce:

- **`duration_micros: -1`** is unmeasured. **Not `0`**, which is a genuine sub-millisecond
  duration and exactly the figure a dashboard would render as measured.
- **`at_unix_micros: 0`** is an unstamped entry. A zero `time.Time` is year 1, whose Unix
  micros a host would subtract into roughly two millennia of latency.
- **`alloc_micros: -1`** is Unlimited — `Units(Unlimited)`'s own value, as `cap_micros` is
  on the outcome event. Render it as "no cap", never as a negative budget.

**`verdict` is a three-state string**: `"passed" | "failed" | "not_assessed"`. A bool cannot
hold the third, and the third is the **common** case — P2 makes verifier availability the
primary terminator, so most nodes in a real run were never checked. `not_assessed` means
UNCHECKED, never failed; a host painting it as a failure reports a verification problem
quarry never found. The vocabulary is deliberately identical to the OTel projection's, so a
consumer reading a trace and a live stream does not map between them.

**`gap` and `unfunded` are separate keys on every exit event, always emitted, and never both
true of one node** — D3's ruling at a live site. A view that painted a priced-out node red
would make P4's contract look like a malfunction while it worked exactly as promised.

**Nothing on this wire is citable, and the record's bytes do not depend on who is watching.**
An in-flight node has costs still moving and verdicts that do not exist yet; the `artifact`
event's url names the record, which is the artifact. A live *write failure* does not fail the
run — an observer that killed the run it observes is what P8's non-perturbation rule forbids
— and the resulting truncated stream is the honest signal: no terminal outcome, so a host
reports a crash, per the `crashed` case.

The fixture is `live-nodes`, and unlike every other case it is **hand-built and cannot be
derived**: a record holds no entry events, no allocations (once a node finishes, what it was
*allowed* is gone) and no wall-clock. `scripts/host-contract.sh` checks the same properties
on a real spawned run, since fd ownership and ordering are unreachable from `go test`.

### The conformance corpus

`testdata/runevents/` is the vendorable fixture set, eleven cases, with its own
[README](../testdata/runevents/README.md) naming the producing commit and the
invocation per case. **Records are captured and never regenerated; streams and
expectations are derived and asserted byte-identical**, so what is claimed
deterministic is the *fold*, which is pure — not the runs.

`live-nodes` is the one case that **breaks that split, deliberately**: a live stream is
not a projection of a record, so there is nothing to derive it from (see D5). It is
hand-built with fixed timestamps, and `StreamExpectation` covers only its folded half —
the per-node assertions live in the test.

#9 asked for a script producing the whole corpus under `--fake`, reproducible in
CI. Two of its own required cases make that impossible, and naming which is more
useful than a script that quietly covers less:

- **Budget degradation is structurally unreachable under `--fake`.** Its per-call
  cost is uniform, so affordability either funds every child or declines the split;
  a tree with *some* children priced out does not exist in that mode. Hence
  `live-partition`, a real Bedrock run — which is also the only case carrying the
  float-sum and model-residual properties below.
- **The time-truncated cases are wall-clock races**, and the same machine changed
  its answer *during* this work: the pair was captured at 620ms/500ms, and once
  `BudgetedSolver` began wrapping the leaf prompt — which changes the prompt hash,
  and the fake's per-call latency derives from it — the whole usable band moved to
  ~185–195ms. In CI the same command would silently produce a *different* corpus,
  which #9 is explicit is worse than none.

Two host-side properties are pinned only by that live case and are worth naming
here because a host will hit them:

- **The receipt rows do not sum to the total in float64** (§3), and that is
  arithmetic, not an emitter bug.
- **`model_spend_micros` does not tie to `total_micros`** — 38395 against 80437 on
  `live-partition`, leaving 42042 unexplained, over half the run. `executor.go`'s
  reduce path assigns `Cost` but no `Model`/`ModelVersion`, so a reduce node is
  itemised in the receipt and appears in no `ModelEvent`; 7 of 25 spending nodes
  carry no version. A host rendering "spend by model" beside a total will show two
  numbers that disagree. **Do not close the gap by inventing an untagged row.**
  Tracked as [#20](https://github.com/scttfrdmn/quarry/issues/20).

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
