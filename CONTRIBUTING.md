# Contributing to quarry

## Read the design doc first

`docs/design.md` is the source of truth. `CLAUDE.md` is the coding standards.
Where they disagree, the design doc wins — and say so in the PR rather than
quietly diverging.

Every design decision in quarry traces to one of nine principles (P1–P9, listed in
`CLAUDE.md`). **If your change contradicts one, either the change or the principle
is wrong — name which in the PR.** That is not a formality: §9's premise was wrong
and the design doc records the correction rather than quietly rewriting it, which is
the convention here — a superseded claim stays visible next to what replaced it.

## The gate

```
go build ./...
go test ./... && go test -race ./...
go vet ./... && gofmt -l . && golangci-lint run
```

All of it must be clean. `golangci-lint` config is `.golangci.yml`; the issue caps
are deliberately set to `0` (unlimited) because the defaults silently truncate, and
a gate that under-reports is worse than no gate.

### And then run the binary

The suite is green far more often than the system works. Of the seven defects found
in the first live-run round, **four were invisible to the test suite** and one was
found only by writing the demo script. So:

```
go build -o bin/quarry ./cmd/quarry
bin/quarry run --fake --cap 0.25 "<a real question>"
bin/quarry show <record> && bin/quarry replay <record>
QUARRY_DEMO_NOPAUSE=1 ./demo.sh
```

CI runs all of these. `--fake` needs no credentials, no network and no money;
**anything reachable with `--fake` must stay reachable with `--fake`.**

`--fake` is *not* a small live provider, and treating it as one hid three replay
defects. Its per-call cost is uniform, so a tree with *some* children priced out is
structurally unreachable, and its planner declines on clause length long before it
reaches the depth bound. A change to a spend or depth path needs a live run, or an
explicit note that it was not exercised.

## Tests encode invariants, not behaviour

**A failing test means the design changed.** Update `docs/design.md` in the same
commit or revert. Do not adjust a test to make a change pass without saying why in
the commit message.

When you fix a defect, **reintroduce it behind the test's back and confirm the test
fails on the guarantee** rather than on a mechanism. This has caught inadequate
tests more than once: a repaint fix whose test still passed when only half the
defect was restored was not testing what it claimed to.

Two fixtures matter more than they look: `partialRun` and `childrenOutOfTimeRun`
(in `iterate_test.go` / `record_test.go`). **Partial runs are the normal outcome,
not the edge case** — under a deadline most runs truncate, and every seam that
assumed a complete tree broke on the partial case. Anything that reads a record
must be tested against a record with gaps.

## Go-specific rules

1. **Time lives in `context.Context`; money lives in `*Ledger`.** Never put the
   budget in a context value — it is load-bearing, not request-scoped metadata.
2. **`Units` is `int64` micro-units, never float.** Apportionment uses
   largest-remainder distribution so shares sum exactly and replay is bit-stable.
3. **Nothing in the root package imports an AWS SDK, dials the network, or calls
   `time.Now()`.** `now` is a parameter.
4. **Errors wrap the sentinels** (`ErrCapExceeded`, `ErrPlanDoesNotFit`,
   `ErrScopeWidens`) so callers can `errors.Is`.
5. Small interfaces at the seams; concrete types in the core.

There is a parallel Python implementation. **The design doc is shared; the code is
not.** Do not port idioms across — each language should express the design in its
own terms.

## Comments

The codebase has a high comment density and it is deliberate: comments explain *why*
a thing is the way it is, usually naming the section or principle it comes from, and
often naming the defect that made it necessary. Match that. A comment that restates
the code (`// Name returns the name`) is worse than none — it costs a reader a line
and tells them nothing.

## Tracking

Work is tracked in **GitHub issues, milestones and labels**, not in markdown files.
Issues carry `P1`–`P9` labels where a principle is at stake. Before opening a PR,
link the issue it closes; if there isn't one, open it first so the reasoning is
reviewable separately from the diff.

## Marked gaps beat plausible guesses

`TODO(§n):` with a section reference beats an implementation that quietly
contradicts the doc. If you don't know what the design intends, leave the gap
marked and say so.
