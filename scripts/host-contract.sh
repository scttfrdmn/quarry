#!/usr/bin/env bash
# host-contract.sh — assert the #9 host contract by SPAWNING THE BINARY.
#
# usage: scripts/host-contract.sh [path-to-quarry]   (default ./bin/quarry)
#
# WHY THIS IS A SCRIPT AND NOT A GO TEST. Two of #9's four decisions are about
# which file descriptor bytes land on and what the process returns, and no Go test
# can reach either: `go test` calls exitCode() as a function and writes to a
# buffer. A summary printed to stdout under --events-json is a parse error in a
# host's NDJSON and is invisible to the entire suite. cmd/quarry/main_test.go pins
# the MAPPING; this pins what a supervisor actually observes.
#
# bucktooth (Go) and rustynail (Rust) branch on these bytes and these numbers, so a
# regression here is a silent misread in two other repos rather than a build error
# anywhere. It runs in CI and is runnable by hand for the same reason demo.sh is.
#
# --fake throughout: no credentials, no network, no money.
set -euo pipefail

Q=${1:-./bin/quarry}
if [ ! -x "$Q" ]; then
	echo "no quarry binary at $Q — go build -o bin/quarry ./cmd/quarry" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

Q3="What does storage cost, how does it scale, and what dominates the bill?"

echo "== D1: the events own stdout, the humans move to stderr"

# WITHOUT --quiet on purpose. The live tree is the noisiest writer and the one most
# likely to be missed when a print site is added, so suppressing it here would
# remove the only thing this check can catch.
"$Q" run --fake --cap 0.25 --depth 2 --events-json --out "$work/h.json" "$Q3" \
	>"$work/h.ndjson" 2>"$work/h.err"

# STDOUT IS CHECKED FIRST, and the order is deliberate rather than incidental. The
# defect this pair exists to catch — a print site that forgot the redirect — makes
# stdout dirty AND stderr empty at the same time, so a script that checked stderr
# first would report "the human surface vanished" and send the reader looking for a
# suppressed writer instead of a misdirected one. A checker that names the wrong
# guarantee is the diffRecords defect (§9), one layer out.
#
# Every line on stdout must be a JSON object. Reported by line number, because "there
# is a stray line" without saying which one sends the reader to the wrong file.
if grep -vn '^{' "$work/h.ndjson"; then
	echo "FAIL: non-JSON on stdout under --events-json. A host reports this as a parse" >&2
	echo "      error against quarry's contract, not as a cosmetic problem — and the" >&2
	echo "      likely cause is a print site that writes to os.Stdout directly rather" >&2
	echo "      than to run.go's 'human' writer." >&2
	exit 1
fi

if [ ! -s "$work/h.err" ]; then
	echo "FAIL: the human surface vanished. --events-json MOVES it to stderr; it does" >&2
	echo "      not suppress it, and a host that shows a user nothing is worse than one" >&2
	echo "      that shows them a tree." >&2
	exit 1
fi

python3 - "$work/h.ndjson" <<'PY'
import json, sys

raw = open(sys.argv[1], "rb").read()

# D2 — every line \n-terminated INCLUDING THE LAST. Without it a host cannot tell a
# complete final event from a truncated one, which is the whole crashed-vs-finished
# distinction the terminal event exists to make.
assert raw.endswith(b"\n"), "the stream must end in a newline"
evs = [json.loads(l) for l in raw.splitlines()]

assert evs[0]["type"] == "quarry_stream", f"the version frame must come first, got {evs[0]}"
assert evs[0]["version"] == 1, (
    f"stream_version is now {evs[0]['version']}: that is a MAJOR bump, and bucktooth "
    "and rustynail need telling before it lands"
)
assert evs[0]["producer"] == "quarry-go", evs[0]

# Scanned BACKWARDS, not read off the last line: a future kind may legally follow the
# outcome, and keying on line position is what the contract forbids.
out = next(e for e in reversed(evs) if e["type"] == "quarry_outcome")
assert out["outcome"] == "complete", out
assert out["gaps"] == 0 and out["unfunded"] == 0, out

# Absence is not zero, at the wire. bound_by is EMITTED empty rather than omitted —
# a host seeing no key could not tell "nothing bound this run" from "this producer
# does not report it".
assert "bound_by" in out, "bound_by must be emitted even when empty"
assert out["bound_by"] == "", out
# The two integer figures. --cap 0.25 is 250000 micro-units exactly; a float here
# would mean the terminal event had started pricing in USD like agate's union does.
assert out["cap_micros"] == 250000, out
assert isinstance(out["total_micros"], int) and out["total_micros"] > 0, out

# Provenance is OMITTED, never a fabricated 0.0: nothing in the CLI wires
# replication, and one run is one sample (P7). A rendered 0% would badge "nothing
# replicated" on a run where nobody could tell.
art = next(e for e in evs if e["type"] == "artifact")
assert "provenance" not in art, "an unmeasured stability must be ABSENT, not zero"

# The frame is additive: agate's own events are unchanged in the middle, and the two
# quarry_* kinds are the only additions.
kinds = [e["type"] for e in evs]
assert kinds[0] == "quarry_stream" and kinds[-1] == "quarry_outcome", kinds
assert sum(1 for k in kinds if k.startswith("quarry_")) == 2, kinds
print(f"  ok: {len(evs)} events, version 1, {out['total_micros']} micro-units")
PY

echo "== D4: the exit code is a vocabulary, observed from outside the process"

# Each code is taken from a REAL invocation rather than a constructed error, because
# the gap this check exists to close was exactly between the documented table and the
# code paths: `--cap 0` returned an unclassified error and exited 1 while the table
# promised 2, and cmd/quarry/main_test.go's table test could not see it.
set +e
# 0 — and the whole ruling with it: a run that finished inside its caps.
"$Q" run --fake --quiet --cap 0.25 --depth 2 --out "$work/c.json" "$Q3" >/dev/null 2>&1
complete=$?
# 4 — nothing was affordable. --cap 0.000001 prices out the root itself. Deterministic:
# no clock is involved, so this is the reliable half of the two no-answer causes.
"$Q" run --fake --quiet --cap 0.000001 --depth 2 --events-json --out "$work/z.json" \
	"Probe the spend floor." >"$work/z.ndjson" 2>/dev/null
noanswer=$?
# 4 again by a different route — every node gapped. Included because it reaches the
# code through TIME rather than money, and the two must not diverge.
"$Q" run --fake --quiet --cap 1.00 --deadline 150ms --fake-latency 100ms \
	--depth 2 --events-json --out "$work/t.json" \
	"How much does storage cost, how does it scale, and what dominates the bill?" \
	>"$work/t.ndjson" 2>/dev/null
gapped=$?
# 2 — a refused flag. Nothing ran, so it is USAGE, not a fault.
"$Q" run --fake --cap 0 "anything" >/dev/null 2>&1
usage=$?
# 1 — a well-formed invocation whose read failed. The line between 1 and 2 is whether
# anything was ATTEMPTED.
"$Q" show "$work/absent.json" >/dev/null 2>&1
fault=$?
set -e

echo "  complete=$complete no-answer(spend)=$noanswer no-answer(time)=$gapped usage=$usage fault=$fault"
[ "$complete" = "0" ] || {
	echo "FAIL: a complete run must exit 0 or every shell reads it as failure, got $complete" >&2
	exit 1
}
[ "$noanswer" = "4" ] || {
	echo "FAIL: a run that could afford nothing must exit 4, got $noanswer. Not 1: the record" >&2
	echo "      is faithful and citable, so it is an OUTCOME and not a fault." >&2
	exit 1
}
[ "$gapped" = "4" ] || {
	echo "FAIL: a run whose every node gapped must exit 4, not 3 — a host told to extend it" >&2
	echo "      would be extending nothing. Got $gapped." >&2
	exit 1
}
[ "$usage" = "2" ] || {
	echo "FAIL: a refused flag must exit 2, got $usage. 1 would escalate a user's typo" >&2
	echo "      to a host as a quarry MALFUNCTION — the defect this check was written for." >&2
	exit 1
}
[ "$fault" = "1" ] || {
	echo "FAIL: an unreadable record must exit 1, got $fault" >&2
	exit 1
}

# WHAT THIS SCRIPT DOES NOT COVER, said out loud rather than left to be assumed from a
# passing step. Exit 3 (time-truncated: SOME children gapped, an answer still returned)
# is not reachable here. --fake's per-call latency is uniform, so a deadline either
# gaps every sibling or none of them — the mixed case that distinguishes 3 from 4 does
# not exist in that mode, exactly as budget degradation does not. It is pinned instead
# by testdata/runevents/time-truncated.{json,expected.json}, a captured record, and by
# cmd/quarry/main_test.go's mapping. See testdata/runevents/README.md.
echo "  not covered here: exit 3 — a PARTIAL time truncation is unreachable under --fake"
echo "                    (uniform per-call latency: all siblings gap or none)."
echo "                    Pinned by testdata/runevents/time-truncated.* instead."

# THE PAIR IS THE CONTRACT. A host branches on (outcome, exit code) together, so the
# two agreeing is the thing to assert — either alone can be right while the run reports
# one story to a parser and another to a supervisor.
#
# Both no-answer cases are checked, because they are the pair that separates the
# denominations: same outcome, same exit code, and gaps/unfunded swapped.
python3 - "$work/z.ndjson" "$work/t.ndjson" <<'PY'
import json, sys


def outcome(path):
    evs = [json.loads(l) for l in open(path)]
    return next(e for e in reversed(evs) if e["type"] == "quarry_outcome")


spend, time_ = outcome(sys.argv[1]), outcome(sys.argv[2])

for name, o in (("spend", spend), ("time", time_)):
    assert o["outcome"] == "no-answer", f"{name}: {o}"

# GAPS AND UNFUNDED ARE DIFFERENT DENOMINATIONS and must never be summed. Only TIME
# produces a gap; spend exhaustion produces unfunded nodes. A host that added them
# would offer more time where money was needed.
assert spend["gaps"] == 0, f"spend exhaustion is not a gap: {spend}"
assert spend["unfunded"] > 0, f"the spend case must price something out: {spend}"
assert spend["total_micros"] == 0, spend

assert time_["gaps"] > 0, f"the deadline case must gap something or it tests nothing: {time_}"
assert time_["unfunded"] == 0, f"a deadline does not price nodes out: {time_}"
assert time_["bound_by"] == "latency", time_

# The two cases must be DISTINGUISHABLE on the stream even though they share an exit
# code, or the vocabulary has collapsed one layer in: a host has the same status and
# needs different remedies.
assert (spend["gaps"], spend["unfunded"]) != (time_["gaps"], time_["unfunded"]), (
    "both no-answer causes report the same counts, so a host cannot tell money from time"
)
print(f"  ok: spend {spend['gaps']}g/{spend['unfunded']}u vs time "
      f"{time_['gaps']}g/{time_['unfunded']}u — same code, distinguishable cause")
PY

echo "host contract ok"
