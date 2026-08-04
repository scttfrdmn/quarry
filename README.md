# quarry (Go)

Bounded recursive decomposition with verified provenance.

Takes a research-shaped problem, rewrites it into sub-problems dispatched to
independent agents, recurses to a bounded limit, and returns an answer together
with a record of how it was produced and how much to trust it.

**Status:** steps 1-9 of the build order (`docs/design.md` §11) are done — planner,
solver, reducer, verifiers, run record with byte-identical replay, claim
extraction, stability, cost estimation and adversarial passes, plus a CLI and a
live tree view. Step 10 (agate integration) is in progress.

Everything else — what is broken, what is next, what is blocked and on what — is
tracked in [issues](https://github.com/scttfrdmn/quarry/issues) and
[milestones](https://github.com/scttfrdmn/quarry/milestones) rather than in
markdown. `docs/design.md` is the design and the source of truth; `CLAUDE.md` is
the coding standards; `CONTRIBUTING.md` is how to work on it.

**Maturity (agate matrix):** Seam.

A parallel Python implementation exists. The design doc is shared; the code is
not — each language expresses the design in its own idiom.

## Position

- [`agenkit`](https://github.com/scttfrdmn/agenkit) builds the agent.
- [`agate`](https://github.com/scttfrdmn/agate) governs it.
- quarry decides **how many agents, arranged how, for how much.**

## Why Go for this one

The core is a concurrency problem, not a text problem: fanout, join, deadline
propagation, subtree cancellation, partial results on timeout.

`context.Context` carries the design's time semantics directly. A deadline is
inherited whole by every sibling and shortened as you descend — which is exactly
what §3.1 specifies time doing — and cancelling a branch context kills its
subtree, which is §10 for free rather than as a mechanism to build.

That leaves money as an explicit `*Ledger` parameter, divided across siblings and
inherited whole down depth. The propagation duality the design spends a table
explaining becomes the shape of the function signature:

```go
func (n *Node) Process(ctx context.Context, lg *quarry.Ledger, p quarry.Problem) (quarry.NodeOutcome, error)
```

## Quick look

```go
caps := quarry.Caps{Spend: quarry.FromFloat(40), Due: friday} // slack is cheap
ctx, cancel := quarry.RootContext(context.Background(), caps, time.Now())
defer cancel()

lg, err := quarry.NewLedger(caps, quarry.Scope{Tags: map[string]string{"agate:dept": "bio"}})

lg.Apportionable() // what may go to children; the rest funds the reduce
caps.Deferrable()  // true -> batch inference is available
```

## Try it

`--fake` needs no credentials, no network and no money. Anything reachable with
`--fake` stays reachable with `--fake` — it is how the system is demonstrated, and
how five of the defects on record were found.

```
go build -o bin/quarry ./cmd/quarry

bin/quarry run --fake --cap 0.25 "<question>"   # live tree, record written
bin/quarry show <record.json>                   # per-node click-through
bin/quarry replay <record.json>                 # proves it reproduces (P8)
```

`./demo.sh` walks the whole thing in ten beats; `./demo.sh --live` does the same
against real Bedrock, which costs roughly USD 0.08-0.11 per run (measured, not
estimated).

## Development

```
go build ./...
go test ./... && go test -race ./...
go vet ./... && gofmt -l . && golangci-lint run
```

Steps 1-7 of the build order, and the whole CLI with `--fake`, run with no AWS and
no LLM. Nothing in the root package imports an AWS SDK, dials the network, or calls
`time.Now()` — `now` is a parameter.

A failing test means the design changed: update `docs/design.md` in the same commit
or revert. The tests encode invariants, not behaviour.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
