# Vendored records

`runevents/` is the host event-stream conformance corpus and has its own README.

## `record-pre-planid.json` — a record from before the approval gate existed

A real `quarry run --fake` record, captured at `710e73c` (the commit before `PlanID`
was added to `RunRecord`), with:

```
quarry run --fake --cap 0.25 --depth 2 \
  "What does storage cost, how does it scale, and what dominates the bill?"
```

Four nodes, `RunID` `4f8ca451e412…`. Consumed by
`TestAnUngatedRecordHashesAsItDidBeforeThePlanFieldExisted` (`plan_test.go`) and by the
CLI's `show`/`replay` paths.

**Why a captured file rather than a constructed value.** The guarantee is that adding a
hashed field to `RunRecord` did not change what every record already on disk hashes to.
A `RunRecord` built by today's code carries today's field set by construction, so it
cannot witness that — only bytes written by the older code can. Removing `omitempty` from
`PlanID` adds `"PlanID":""` to the canonical bytes and this file stops hashing to its own
`RunID`, which is exactly the failure the test reports.

**Do not regenerate it.** Its value is entirely in being older than the field. Rewriting
it with the current binary would leave a green test that has stopped testing anything —
the same vacuity a hand-built fixture would have had from the start. If the record format
changes such that this file can no longer be loaded at all, that is a finding about
backward compatibility to report, not a fixture to refresh.
