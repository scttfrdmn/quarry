# Host event-stream conformance corpus

Vendored fixtures for a host that spawns `quarry run --events-json` and reads its
stdout. Frozen by [#9](https://github.com/scttfrdmn/quarry/issues/9); consumed by
**bucktooth** (Go) and **rustynail** (Rust).

Produced by `quarry` at commit **`f89f59f`** on branch `host-event-stream`, stream
version **1**, producer **`quarry-go`**.

## What is in here

Three files per case:

| file | what it is | who writes it |
|---|---|---|
| `<case>.json` | the run record, in quarry's canonical (hashed) encoding | **captured** — the binary, once, by hand |
| `<case>.ndjson` | the framed stream `--events-json` emits for that record | **derived** — `go test -run TestHostCorpus -update` |
| `<case>.expected.json` | the same stream restated in integers, for a host to check itself against | **derived** — same command |

The split matters and is not tidiness. **Records are captured and never
regenerated. Streams and expectations are derived and asserted byte-identical.**
`TestHostCorpus` folds each record and fails if either derived file differs, so the
fold is pinned; the runs are not. See "Why not a generator script" below — #9 asked
for one and two of its own required cases make that impossible.

## Reading the expectation

`*.expected.json` is `quarry.StreamExpectation` (`corpus.go`). Every figure in it
came **off the wire**, not out of the record, so a host comparing its own reading
against it is comparing two readings of the same bytes.

Three rules a conforming host must implement. Each has a case here that fails a host
that gets it wrong:

**1. Money is integers.** The stream prices in USD floats because agate does. The
expectation does not. `cost_micros` and `total_micros` are the ledger's own
`int64` micro-units, and the conversion is `parse float → round(× 1e6)` — round,
never truncate, or `0.000249` comes back as `248`.

*Do not sum the floats.* `complete` and `live-partition` both have rows whose float
sum disagrees with the stated total (`float_sum_equals_total: false`); on
`live-partition` the gap is 1.4e-17 across 25 rows. Convert each row to micro-units
first, then sum.

Two serialization shapes, both verified against Go's encoder rather than assumed:

- Go's `encoding/json` avoids exponent notation on `[1e-6, 1e21)`, and one
  micro-unit *is* `1e-6`. So no cost quarry can emit ever appears as `7.6e-05`; it
  appears as `0.000076`. (Python's `repr` of the same number shows the exponent form,
  which is how this got written down wrongly the first time.)
- A round dollar serializes **with no decimal point**: `"cost":1`. A parser demanding
  one rejects a valid receipt. `unknown-kind` is the case that carries this, because
  no real quarry run is expensive enough to.

**2. Absence is not zero — three places.**

- **Provenance.** agate's `stability` is a non-nullable number, so quarry says "not
  measured" by **omitting the whole provenance object** from the artifact event (#9
  D3). `provenance_present: false` means *quarry declined to publish an estimate*,
  never *stability was 0*. Every case here has it false: nothing in the CLI wires
  replication, and one run is one sample (P7). A host that rendered a missing
  stability as 0% would badge "nothing replicated" on a run where nobody could tell.
- **`cap_micros: -1`** means no spend cap (a deadline-only run). Not `0` — `0` reads
  as a cap of nothing, and would make every such run look overspent by its whole
  total.
- **`bound_by: ""`** means no cap bound this run. It is a measurement, so it is
  emitted rather than omitted.

**3. Gaps and unfunded are different denominations and must never be summed.**
Only **TIME** produces a gap. Spend exhaustion produces *unfunded* nodes, which is
planned degradation inside authority — not missing work. A host that added them
would offer more time where money was needed. `time-truncated` (3 gaps, 0 unfunded)
and `no-answer-spend` (0 gaps, 1 unfunded) are the pair that separates them.

Lengths are counted in **runes**. `answer_runes` and the 60-rune receipt-label limit
both are; a host counting bytes disagrees on every unicode case and, worse, splits a
multi-byte rune when it truncates.

## The cases

| case | exit | outcome | gaps | unfunded | micro-units | rows | what it is for |
|---|---|---|---|---|---|---|---|
| `complete` | 0 | complete | 0 | 0 | 218 | 3 | The baseline. Fold this correctly first. Its rows already fail to sum in float64. |
| `live-partition` | 0 | cap-bound-degradation | 0 | 5 | 80437 | 25 | **The most load-bearing file here.** See below. |
| `time-truncated` | 3 | time-truncated | 2 | 0 | 172 | 2 | A deadline cut it short: gaps, and a partial answer worth showing. Exit 3 is how a host tells a partial answer from a whole one without parsing prose. |
| `no-answer-time` | 4 | no-answer | 4 | 0 | 0 | 0 | Every node gapped. Classified `no-answer`, **not** `time-truncated`, even though `bound_by` is `latency` — a host told to extend this would be extending nothing. `gaps: 4` with `total_micros: 0`: the deadline bit before anything was spent. Also an **empty receipt**. |
| `no-answer-spend` | 4 | no-answer | 0 | 1 | 0 | 0 | The spend counterpart: `--cap 0.000001` priced out the root itself. Zero gaps. **Zero-spend receipt, and `"rows":[]` — never `null`.** |
| `unicode` | 0 | complete | 0 | 0 | 247 | 3 | Accents, an em-dash, CJK, a fullwidth question mark. Pins that HTML escaping is off (`&` and `<` stay literal). |
| `unicode-long` | 0 | complete | 0 | 0 | 421 | 4 | The same past the **truncation boundary**. Has both shapes: labels over 60 runes that must be cut, and one of 47 runes / 139 bytes that must **not** be. A byte-limiting host truncates that one and emits U+FFFD. |
| `unknown-kind` | 0 | complete | 0 | 0 | 3000000 | 2 | **Synthetic.** A `quarry_future_kind` event between the answer and the receipt: a host must skip the whole object and still fold every known kind. Also the only round-dollar costs. |
| `crashed` | — | — | — | — | — | — | **Synthetic.** A stream cut mid-line. No `.expected.json`, deliberately — see below. |

### `live-partition`

A real Bedrock run — 30 nodes, 25 spending, $0.0804 against a $0.25 cap. Three
properties live only in this file, and all three are things `--fake` cannot produce:

1. **Rows that do not sum in float64** (`float_sum_equals_total: false`). 25 real
   costs accumulate an error a 3-row fixture does not.
2. **A model-spend residual: `model_spend_micros` 38395 against
   `total_micros` 80437, leaving 42042 unexplained — over half the run.** This is
   **expected, not a bug in the fixture.** `executor.go`'s reduce path assigns `Cost`
   but never `Model`/`ModelVersion`, so a reduce node is itemised in the receipt and
   appears in no `ModelEvent`; 7 of the 25 spending nodes carry no version. A host
   rendering "spend by model" beside a total will show two numbers that do not tie.
   **Do not close the gap by inventing an untagged row.** Tracked as
   [#20](https://github.com/scttfrdmn/quarry/issues/20); if it is ever fixed, this
   figure and `runevent.go`'s `ModelEvent` doc both change and the twins need telling.
3. **5 unfunded nodes** — cap-bound degradation, which exits **0**. Under the
   standing ruling only time produces a gap; the cap did exactly what P4 promises,
   and a non-zero status would make that look like a malfunction. A host that wants
   to know reads `bound_by` and `unfunded` off the terminal event.

### `crashed`

The only case with no `.expected.json`, and the absence is the point: the bytes do
not parse into a complete set, so there is no expectation to state. It is a stream
cut **mid-line**, inside the artifact event's JSON — not between lines, which would
leave well-formed NDJSON merely lacking a terminal event and could be misread as a
clean early exit.

A conforming host must:

- return the 3 complete lines it did get, and be able to say what it got;
- **report** the truncation rather than tolerate it;
- find **no terminal outcome**, and not default the missing case to "complete".

This is what makes the terminal `quarry_outcome` event load-bearing. NDJSON yields
whole lines whether or not the producer finished, so its **absence** is the only
in-band signal that a run was killed — and a host reading a vendored fixture from a
file has no exit code to fall back on.

## The frame

```
{"type":"quarry_stream", ...}    first  — the version, so a host can refuse the stream
  ... agate's events, unchanged ...     — model* / answer / receipt / artifact
{"type":"quarry_outcome", ...}   last   — quarry's own; its absence means "crashed"
```

The frame is **purely additive**: the middle is byte-identical to what agate
receives, and a test asserts that rather than trusting it.

Both new kinds are namespaced `quarry_*` because agate's models declare
`extra="forbid"` and its schema has **no gap representation** — the one fact a
supervising host most needs cannot ride on an agate-accepted event, so it rides on
quarry's own.

Framing rules (#9 D2):

- Every line is `\n`-terminated, **including the last**.
- **Adding a kind is a minor bump.** A host must skip kinds it does not know
  (`unknown-kind`). Do not key on line position — a future kind may follow the
  outcome, which is why `TerminalOutcome` scans backwards rather than reading the
  last line.
- **Changing or removing a field is major**, and `stream_version` goes up.
- `type` is the discriminant. A line without a string `type` is an error, not an
  ignorable object.

## Exit codes

```
0  complete             finished inside its caps, with an answer
                        — AND cap-bound degradation, by ruling
1  fault                crash, provider error, unreadable record — a MALFUNCTION
2  usage error          bad flags, refused caps; nothing ran
3  time-truncated       a deadline cut it short; the record has gaps
4  no answer            nothing was affordable, or every node came back empty
```

1 and 2 keep their conventional meanings. The `exit_code` in each expectation is
**hand-written** from what the binary returned when the record was captured — not
computed from the record, which would only prove quarry agrees with itself.
`cmd/quarry/main_test.go` asserts the current mapping still produces those numbers.

Every code above was checked by running the binary, and doing so found one: `quarry
run --cap 0` — a plain flag mistake with nothing run — exited **1**, so this table
promised 2 for "bad flags, refused caps" while the binary reported a malfunction. A
host would have escalated a user's typo as a quarry fault. Fixed; if you are
implementing against an older build, verify rather than assume.

Note that `2` and `1` are both reachable from a bad argument, and the line between
them is whether anything was *attempted*: `quarry show nonexistent.json` is **1**, not
2 — the invocation was well-formed and the read failed.

`no-answer` is 4 and not 1 even though the cause is usually spend: the record is
faithful and citable — it accurately records that nothing was affordable — so it is
an outcome, not a fault.

## Regenerating

```
go test -run TestHostCorpus -update    # rewrites *.ndjson and *.expected.json
```

Records are **not** regenerated by this. If a derived file changes, that is either a
wire change needing a version bump or an accident — and either way **bucktooth and
rustynail need telling**.

### Why not a generator script

#9 asked for a script that produces the whole corpus under `--fake` and reproduces
byte-identically in CI. Two of its own required cases make that impossible, and
naming which is more useful than a script that quietly covers less:

- **The budget-degraded case is unreachable under `--fake` at all.** The fake's
  per-call cost is uniform, so affordability either funds every child or declines the
  split — a tree with *some* children priced out does not exist in that mode. Hence a
  real Bedrock run, which is also the only case carrying the float-sum and
  model-residual properties.
- **The time-truncated cases are wall-clock races**, and more sharply than "it might
  vary on another machine". Their shape depends on how many fake calls finished before
  the deadline, and *the same machine changed its answer during this work*: the pair
  was first captured at `--deadline 620ms` / `500ms` giving 3 and 4 gaps, and after
  `BudgetedSolver` began wrapping the leaf prompt — which changes the prompt hash, and
  the fake's per-call latency is derived from it — 620ms produced **zero** gaps and
  the whole band moved to ~185–195ms. So the invocations below are the *current* ones
  (2 and 4 gaps, 10/10 stable on the capturing machine) and they are expected to go
  stale again the next time anything upstream of the prompt changes. That is precisely
  why these records are captured rather than regenerated: in CI the same command would
  silently produce a different corpus, which #9 is explicit is worse than none.

  If a case here stops having its stated shape, re-sweep the band rather than trusting
  the number in this file:

  ```sh
  for d in 150ms 170ms 190ms 210ms; do
    quarry run --fake --quiet --cap 1.00 --deadline $d --depth 2 --out /tmp/sw.json "$Q"
    # then count Gap:true in /tmp/sw.json — you want SOME gaps and a non-empty root
  done
  ```

So determinism is claimed for the **fold**, which is pure. The invocations below let
a human reproduce a case deliberately; CI must not.

### How each record was captured

```sh
# complete
quarry run --fake --quiet --cap 0.25 --depth 2 --out complete.json \
  "What does storage cost, how does it scale, and what dominates the bill?"

# time-truncated — 2 gaps and a partial answer. RACE: 190ms was found by sweeping
# 60-800ms for a value that truncates SOME children rather than all or none. The
# usable band is narrow (~185-195ms) because the fake's siblings finish close
# together; below it everything gaps, above it nothing does.
quarry run --fake --quiet --cap 1.00 --deadline 190ms --depth 2 --out time-truncated.json \
  "What does storage cost, how does it scale, and what dominates the bill?"

# no-answer-time — same question, a deadline shorter than one call. Also a race,
# but a wide and stable one: anything from ~60ms to ~180ms gaps every node.
quarry run --fake --quiet --cap 1.00 --deadline 150ms --depth 2 --out no-answer-time.json \
  "What does storage cost, how does it scale, and what dominates the bill?"

# no-answer-spend — a cap below the floor. Deterministic.
quarry run --fake --quiet --cap 0.000001 --depth 2 --out no-answer-spend.json \
  "Probe the spend floor."

# unicode
quarry run --fake --quiet --cap 0.25 --depth 2 --out unicode.json \
  "Combien coûte le stockage, comment cela évolue-t-il, et qu'est-ce qui domine la facture — 存储成本如何？"

# unicode-long — long enough that receipt labels cross the 60-rune limit
quarry run --fake --quiet --cap 0.25 --depth 2 --out unicode-long.json \
  "Quel est le coût total de possession du stockage à l'échelle du pétaoctet, comment cela évolue-t-il avec le taux d'utilisation observé sur une année complète, et qu'est-ce qui domine réellement la facture finale ? 存储的总拥有成本是多少，它如何随着一整年观察到的利用率而变化，究竟是什么在真正主导最终账单？"

# live-partition — LIVE BEDROCK, ~$0.08 of real money. Not reproducible: a live
# model's answers are not deterministic. The file is the artifact.
quarry run --cap 0.25 --depth 2 --model us.anthropic.claude-haiku-4-5-20251001-v1:0 \
  "What does it cost to run a 200-node GPU cluster for a year, how does that scale with utilisation, and what dominates the bill?"
```

`unknown-kind` and `crashed` are built in `runevent_corpus_test.go` — they are facts
about the framing, not about a run, so no invocation produces them.
