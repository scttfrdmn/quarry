# CLAUDE.md — project and coding standards for quarry (Go)

This file holds **standards**: the rules a change must satisfy. It deliberately
holds **no status** — what is built, what is broken and what is next live in
GitHub, because a status table in a file nobody updates is worse than no status
table.

| Looking for | Go to |
|---|---|
| The design | `docs/design.md` — the source of truth |
| The build order (`build step N` in source comments) | `docs/design.md` §11 |
| What the neighbouring repos actually provide | `docs/integration-requirements.md` |
| What is broken, and why | [open issues](https://github.com/scttfrdmn/quarry/issues) |
| What is next | [milestones](https://github.com/scttfrdmn/quarry/milestones) |
| How to contribute | `CONTRIBUTING.md` |
| Session-to-session context | this project's memory directory (`MEMORY.md` indexes it) |

Read `docs/design.md` before writing code. Where it and this file disagree, the
design doc wins — and say so rather than quietly diverging.

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
change or the principle is wrong — name which in the PR. Issues carry `P1`–`P9`
labels, so `label:P9` is the list of open work where that principle is at stake.

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
   tree diverge. Floats appear only at a wire edge (`runevent.go`) and never feed
   back into ledger arithmetic.
4. **Nothing in the root package imports an AWS SDK, dials the network, or calls
   `time.Now()`.** `now` is a parameter. That is what keeps the core, and the
   whole CLI under `--fake`, runnable with no AWS and no LLM. The network edge is
   `provider/`; OTel lives in `otel/`.
5. **Errors wrap the sentinels** (`ErrCapExceeded`, `ErrPlanDoesNotFit`,
   `ErrScopeWidens`) so callers can `errors.Is`.
6. Small interfaces at the seams; concrete types in the core. `gofmt`, `go vet`
   and the race detector clean before every commit.

## Design rules that are not Go-specific

- **Cache keys are scope-qualified.** Never the statement hash alone.
- **Cache entries accumulate samples.** Never replace.
- **Only complete results are cacheable.** A partial merge served as a whole
  answer is the one cache defect that corrupts an answer instead of costing money.
- **The store's clock is the store's own** — never a sample's `CreatedAt`, which
  only a live provider stamps.
- **No efficiency metric without a quality denominator.** Cost per run is
  trivially gamed by doing less. A denominator that grows with verbosity is the
  same defect one step removed (issue
  [#4](https://github.com/scttfrdmn/quarry/issues/4)).
- **The reducer must tolerate partial input.** You can stop spending; you cannot
  stop time.
- **Planner and reducer are different agents** — and different types, so the
  separation is not a comment.
- **Model versions are explicit**, never aliases.
- **Absence is not zero.** A nil verdict means unchecked, not failed (§8); an
  unset timing means unmeasured, not instantaneous; an unassessable comparison is
  neither agreement nor disagreement. Every three-state distinction must survive
  to the last hop — flattening one at the projection is a defect even when every
  intermediate layer honoured it.
- **A fact of the original execution cannot be re-derived from the tree's
  geometry.** `BoundBy`, the depth bound and the floor are the three instances
  that taught this; they are recorded on the record (`RunBounds`) and a replay
  *inherits* them rather than recomputing them (P8).
- **Only TIME produces a gap.** Spend exhaustion is planned degradation inside
  authority; labelling it a gap makes the cap look like a malfunction.

## Layout

`docs/` holds both documents: `design.md` (the source of truth) and
`integration-requirements.md` — what the neighbouring repos actually provide, as
opposed to what the design assumed. If you find a bare `design.md` reference
anywhere, it is stale.

```
types.go      value types, Units arithmetic
ledger.go     apportionment, admission, contexts
cache.go      sample-accumulating store
seams.go      every interface
planner.go    Planner impls
solver.go     leaf solving
reducer.go    merge, incl. partial
verify.go     Verifier impls + mechanical oracles
executor.go   the tree driver
record.go     run record assembly + replay
telemetry.go  event emission
observer.go   live Enter/Exit seam (no OTel, no SDK)
claim.go      extraction; equiv.go + cluster.go equivalence
replicate.go  stability.go  probe.go  estimate.go  calibrate.go
iterate.go    extend / refine (§8.1)
adversary.go  surplus-budget passes (§3)
provenance.go runevent.go   the agate projections
provider/     Bedrock, chokepoint, FakeProvider (--fake)
otel/         span-tree exporter
tui/          live tree renderer (§9)
cmd/quarry/   run / show / replay (§9)
```

`cmd/quarry` is the third projection of a record (§9) and the only one whose
protocol quarry owns. Three verbs:

```
quarry run --fake --cap 1.00 "<question>"   live tree, record written
quarry show [--claims|--json] <record>      per-node click-through
quarry replay <record>                      proves it reproduces (P8)
```

`--fake` uses `provider.FakeProvider`: no credentials, no network, no money.
**Anything reachable with `--fake` must stay reachable with `--fake`.**

But `--fake` is *not* a small live provider, and treating it as one is how three
replay defects shipped. Its per-call cost is uniform, so a tree with *some*
children priced out is structurally unreachable, and its planner declines on
clause length long before it reaches the depth bound. A change to a spend or
depth path needs a live run, or an explicit note that it was not exercised.

## Testing

`*_test.go` encodes invariants, not behaviour. **A failing test means the design
changed** — update `docs/design.md` in the same commit or revert. Do not adjust a
test to make a change pass without saying why.

```
go build ./...
go test ./... && go test -race ./...
go vet ./... && gofmt -l . && golangci-lint run
```

All of it clean. **Then run the binary** — of the seven defects found in the first
live-run round, four were invisible to the suite and one was found only by writing
`demo.sh`. CI runs the binary, both degenerate cases and the demo for that reason.

Two rules about tests themselves, both learned by shipping their violation:

- **Reintroduce the defect behind the test's back** and confirm the test fails on
  the *guarantee*, not on a mechanism (not merely under `-race`, not merely a
  missing log line).
- **A test that constructs the state it means to detect cannot discover that
  nothing produces it.** A hand-assigned field, a fixture cleaner than the real
  input, a guard that refuses the work under test, a wired seam the CLI does not
  wire — each of these has made one of these tests vacuous while it passed.

**Partial runs are the normal outcome, not the edge case.** Under a deadline
(§3.1) most runs truncate, and every seam that assumed a complete tree broke on
the partial case. Anything that reads a record must be tested against a record
with gaps: `partialRun` and `childrenOutOfTimeRun` (`iterate_test.go` /
`record_test.go`) are the fixtures, and they differ in whether the *root* expired,
which matters.

**A gate that under-reports is worse than no gate.** `.golangci.yml` sets the
issue caps to `0` because the defaults silently truncate, and a check that claims
to cover a specific condition must verify it reached that condition.

## Comments and docs

The comment density is high and it is deliberate: a comment explains *why*,
naming the section or principle it comes from and often the defect that made it
necessary. A comment that restates the code is worse than none.

**Prefer a marked gap to a plausible guess.** `TODO(§n):` with a section
reference beats an implementation that quietly contradicts the doc.

**Name a divergence; never quietly fix one.** When the design doc is corrected,
the superseded text stays visible next to what replaced it — that convention is
why §9's wrong premise is legible today instead of lost.

## Tracking

Work is tracked in **GitHub issues, milestones and labels** — not in markdown
files, and not in this one. Before opening a PR, link the issue it closes; if
there isn't one, open it first so the reasoning is reviewable separately from the
diff. Labels: `P1`–`P9` for principles, plus `defect`, `design-decision`,
`measure-first`, `telemetry`, `cli`, `extraction`, `docs`, `integration`,
`blocked`.

Two standing cautions that are not issues because they are not fixable:

- **Cost estimation (§4) is deliberately advisory.** It depends on a corpus that
  does not exist until the system has run many times. Nothing may depend on it (P4).
- **Claim extraction is the highest technical risk** and the one seam likely to
  move to Python behind a service boundary — the ML tooling is there. Keep
  `ClaimExtractor` narrow enough that the hop stays cheap; expect `Claim` to change.
