# CLAUDE.md — working brief for quarry (Go)

Read `docs/design.md` before writing code. It is the source of truth; this file
is the operating summary. Where they disagree, the design doc wins — and say so
rather than quietly diverging.

There is a parallel Python implementation. **The design doc is shared; the code
is not.** Do not port idioms across — each language should express the design in
its own terms, which is the point of having both (agenkit is multi-language by
design). Behaviour must agree; structure need not.

## What quarry is

Takes a research-shaped problem, rewrites it into sub-problems dispatched to
independent agents, recurses to a bounded limit, and returns an answer
**together with a record of how it was produced and how much to trust it**. The
deliverable is a run record with a cost receipt and a stability estimate; the
prose answer is one field in it.

Not an agent framework (`agenkit`), not a governance layer (`agate`). quarry
decides **how many agents, arranged how, for how much**.

## The nine principles

Every design decision traces to one. If your change contradicts one, either the
change or the principle is wrong — name which in the PR.

| | Principle | What it forbids |
|---|---|---|
| **P1** | Decompose only where surface-to-volume favours it | Splitting by default; the planner must be able to decline |
| **P2** | Recurse only as deep as you have verifiers | Treating max depth as the design rather than a backstop |
| **P3** | Verification spend ∝ downstream exposure | Uniform verification; leaving the planner unchecked |
| **P4** | The cap is the contract, the estimate a courtesy | Gating any feature on estimate quality |
| **P5** | NO CLOCKS constrains the floor, not the ceiling | Provisioned capacity, warm pools, Redis/OpenSearch |
| **P6** | Scope never widens on descent | Cache keys without scope; children with broader authority |
| **P7** | A run is an estimate, not an answer | Treating n=1 as a result; caches that replace samples |
| **P8** | The record outlives the model | Model aliases; unpinned learned priors; nondeterministic arithmetic |
| **P9** | Planning is budget-conditioned | Planning first and discovering the budget later |

## Go-specific rules

1. **Time lives in `context.Context`; money lives in `*Ledger`.** This is the
   propagation duality (§3.1) made structural: a context deadline is inherited
   whole by every sibling and shortened as you descend, which is exactly what
   time does. Money is passed explicitly and divided. **Never put the budget in
   a context value** — it is load-bearing, not request-scoped metadata.
2. **Cancellation is `context.CancelFunc`, not a mechanism you build.** When the
   reducer prunes a branch, cancel the branch context and the subtree dies
   (§10). Every `ChildContext` caller must `defer cancel()`.
3. **`Units` is `int64` micro-units, never float.** Apportionment uses
   largest-remainder distribution so shares sum exactly and replay is bit-stable
   (P8). Float division is order-sensitive and would let two replays of the same
   tree diverge.
4. **Nothing in this package imports an AWS SDK, dials the network, or calls
   `time.Now()`.** `now` is a parameter. That keeps build steps 1–7 runnable
   with no AWS and no LLM.
5. **Errors wrap the sentinels** (`ErrCapExceeded`, `ErrPlanDoesNotFit`,
   `ErrScopeWidens`) so callers can `errors.Is`.
6. Small interfaces at the seams; concrete types in the core. `gofmt`, `go vet`
   and the race detector clean before every commit.

## Design rules that are not Go-specific

- **Cache keys are scope-qualified.** Never the statement hash alone.
- **Cache entries accumulate samples.** Never replace.
- **No efficiency metric without a quality denominator.** Cost per run is
  trivially gamed by doing less.
- **The reducer must tolerate partial input.** You can stop spending; you cannot
  stop time.
- **Planner and reducer are different agents.**
- **Model versions are explicit**, never aliases.

## Layout

`docs/` holds both documents: `design.md` (the source of truth) and
`integration-requirements.md` — what the neighbouring repos actually provide, as
opposed to what the design assumed. The doc moved there from the repo root on
2026-08-02 to match the ~20 source comments that already said `docs/design.md`;
if you find a bare `design.md` reference anywhere, it is stale.

```
types.go      value types, Units arithmetic          [DONE]
ledger.go     apportionment, admission, contexts     [DONE, tested]
cache.go      sample-accumulating store              [DONE, in-memory]
seams.go      every interface                        [DONE]
planner.go    Planner impls                          [DONE]
solver.go     leaf solving                           [DONE]
reducer.go    merge, incl. partial                   [DONE]
verify.go     Verifier impls + mechanical oracles    [DONE]
executor.go   the tree driver                        [DONE]
record.go     run record assembly + replay           [DONE]
telemetry.go  event emission                         [DONE]
observer.go   live Enter/Exit seam (no OTel, no SDK) [DONE]
claim.go      extraction; equiv.go equivalence       [DONE]
replicate.go  stability.go  probe.go  estimate.go    [DONE]
iterate.go    extend / refine (§8.1)                 [DONE]
adversary.go  surplus-budget passes (§3)             [DONE]
provider/     Bedrock + FakeProvider (--fake)        [DONE]
otel/         span-tree exporter                     [DONE]
tui/          live tree renderer (§9)                [DONE]
cmd/quarry/   run / show / replay (§9)               [DONE]
```

`cmd/quarry` is the third projection of a record (§9) and the only one whose
protocol quarry owns — see §9's correction. Three verbs:

```
quarry run --fake --cap 1.00 "<question>"   live tree, record written
quarry show [--claims|--json] <record>      per-node click-through
quarry replay <record>                      proves it reproduces (P8)
```

`--fake` uses `provider.FakeProvider`: no credentials, no network, no money.
Anything reachable with `--fake` must stay reachable with `--fake` — it is how the
system is demonstrated and how four defects in this list were found.

## Build order

Follow it. Each step is shippable and testable without the next.

1. ~~Ledger arithmetic + admission control~~ **DONE.** Node-level telemetry
   records from here on — nearly free, and the corpus only accumulates.
2. ~~Planner / fanout / Reducer at fixed depth 1~~ **DONE.**
3. ~~Cache wired in, recursion on, DAG behaviour~~ **DONE.**
4. ~~Verifier interface + mechanical oracles~~ **DONE.** P2 is a real terminator.
5. ~~Run record + replay~~ **DONE**, including the partial case — see the note below,
   which is the part that was wrong for a while.
6. ~~Claim extraction and equivalence~~ **DONE** mechanically. `Claim.Sources` is
   still empty: source spans need retrieval, which does not exist yet.
7. ~~Replicates, stability reporting, plan pinning~~ **DONE.**
8. ~~Probe + Galton-Watson estimator~~ **DONE**, and still advisory (P4) — the corpus
   it wants does not exist until the system has run many times.
9. ~~Adversarial passes, surplus-budget policy~~ **DONE.**
10. Integration: agate chokepoint, scope tags, stack. **IN PROGRESS.** The tree view
    is *not* part of this — it is `tui/` + `cmd/quarry`, and needed nothing from agate
    (§9). Remaining: the agate chokepoint, blocked on a sync-vs-lease answer
    (`seams.go`'s `Admitter` note).

Steps 1–7 need no AWS and no LLM beyond `Provider`. So does the whole CLI, with
`--fake`.

**Partial runs are the normal outcome, not the edge case.** Under a deadline (§3.1)
most runs truncate, and every seam that assumed a complete tree broke on the partial
case *in a way that blamed the record*: gaps were skipped when indexing a replay,
`BoundBy` was recomputed from an environment the replay does not reproduce, and an
all-gap record was refused as unreplayable. When adding anything that reads a record,
test it against a record with gaps. `partialRun` and `childrenOutOfTimeRun` in
`iterate_test.go` / `record_test.go` are the two fixtures — they differ in whether the
*root* expired, which turned out to matter.

## Risk notes

**Claim extraction (step 6) is the highest technical risk**, and it is the one
seam likely to live in Python behind a service boundary — the ML tooling is
there. Keep `ClaimExtractor` narrow enough that the hop is cheap. Spike it from
day one; expect `Claim` to change.

**Cost estimation (§4) is deliberately advisory.** It depends on a corpus that
does not exist until the system has run many times. Nothing may depend on it.

**Anchoring on refine (§8.1) is unresolved.** Showing the planner a prior
decomposition biases it toward that decomposition, and §7 names an independent
decomposition as the strongest replication signal — so iteration is the
mechanism most likely to destroy the best evidence the system has.

## Testing

`*_test.go` encodes invariants, not behaviour. A failing test means the design
changed — update `docs/design.md` in the same commit or revert. Do not adjust a
test to make a change pass without saying why.

```
go test ./...
go test -race ./...
go vet ./... && gofmt -l .
```

Prefer leaving a marked gap to guessing. `TODO(§n):` with a section reference
beats a plausible implementation that quietly contradicts the doc.
