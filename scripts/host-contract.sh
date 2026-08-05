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

echo "== #14: the live node stream rides the same stdout, framed once, ahead of the fold"

# Again WITHOUT --quiet, and here that is the point rather than a habit: --live-events
# and the live tree write at the same time, one to stdout and one to stderr, and the
# defect this catches is the two surfaces crossing. A person watching a terminal and a
# host reading the pipe are not exclusive — MultiObserver exists so they coexist — so
# the check has to run them together.
"$Q" run --fake --cap 0.25 --depth 2 --events-json --live-events --out "$work/l.json" "$Q3" \
	>"$work/l.ndjson" 2>"$work/l.err"

# STDOUT FIRST, for the reason the D1 pair above documents at length: the pair is
# violated jointly, and checking the quieter surface first names the wrong cause.
if grep -vn '^{' "$work/l.ndjson"; then
	echo "FAIL: non-JSON on stdout under --events-json --live-events. The live events share" >&2
	echo "      the fold's fd, so any print site that forgot the redirect now corrupts a" >&2
	echo "      host's stream in the MIDDLE of a run rather than at the end of one." >&2
	exit 1
fi

if [ ! -s "$work/l.err" ]; then
	echo "FAIL: the human surface vanished with --live-events. The live tree and the node" >&2
	echo "      stream are different consumers of one seam, not alternatives." >&2
	exit 1
fi

python3 - "$work/l.ndjson" <<'PY'
import json, sys

raw = open(sys.argv[1], "rb").read()
assert raw.endswith(b"\n"), "the stream must end in a newline"
evs = [json.loads(l) for l in raw.splitlines()]
kinds = [e["type"] for e in evs]

# EXACTLY ONE FRAME, FIRST. With --live-events it is written at run START — a host must
# be able to refuse a stream before it reads anything it would parse — so the fold must
# not add a second. Two declarations read as two concatenated streams.
assert kinds.count("quarry_stream") == 1, f"want exactly one version frame, got {kinds.count('quarry_stream')}"
assert kinds[0] == "quarry_stream", f"the frame must come first, got {kinds[0]}"
# NO BUMP. Adding a kind is a MINOR change under #9's own frozen rule, which is the whole
# basis for putting the live events here rather than on a second fd.
assert evs[0]["version"] == 1, (
    f"stream_version moved to {evs[0]['version']} for an ADDITIVE change; #9 froze that as "
    "minor, and a bump makes every v1 host refuse a stream it could have read"
)

enters = [e for e in evs if e["type"] == "quarry_node_enter"]
exits = [e for e in evs if e["type"] == "quarry_node_exit"]
assert enters and exits, "no live events on the stream: --live-events did nothing"
assert len(enters) == len(exits), (
    f"{len(enters)} enters and {len(exits)} exits — an unpaired enter leaves a node drawn "
    "as permanently in flight"
)
assert len(enters) > 1, f"the fixture must decompose, got {len(enters)} node(s)"

# Every live event carries its OWN version, because a host may attach mid-stream where
# the frame has already gone past.
for e in enters + exits:
    assert e["node_stream_version"] == 1, e

# THE ORDERING IS THE WHOLE VALUE of one fd over two. A node's live entry must be
# readable as PRECEDING the fold that summarises it; two streams would give no such
# guarantee, which is why a second destination was rejected.
fold = [i for i, k in enumerate(kinds) if not k.startswith("quarry_")]
live = [i for i, k in enumerate(kinds) if k.startswith("quarry_node_")]
assert max(live) < min(fold), (
    f"a live event at {max(live)} lands after the fold began at {min(fold)}: the interleaving "
    "is backwards and a host cannot read an entry as preceding its summary"
)
# And the terminal outcome still closes it — scanned backwards, never read off the last
# line. Its ABSENCE is how a host detects a crash, so live output must not consume it.
assert kinds[-1] == "quarry_outcome", kinds[-6:]

# THREE-STATE FIELDS SURVIVE TO THE WIRE (D3), asserted on real bytes and not on the
# projection alone. Each is a value no measurement can produce, because the alternative
# — 0, or false — is a number a dashboard would render as measured.
for e in exits:
    assert e["verdict"] in ("passed", "failed", "not_assessed"), e
    # -1 is UNMEASURED; a real duration is positive. 0 must never appear: it is a
    # plausible sub-millisecond latency, which is exactly why it cannot mean absence.
    assert e["duration_micros"] == -1 or e["duration_micros"] > 0, e
    # false is a MEASUREMENT: both keys present on every event, so a host can tell "this
    # node was funded" from "this producer does not report funding".
    assert "gap" in e and "unfunded" in e, e
    # THE TWO DENOMINATIONS ARE NEVER THE SAME NODE (D4). Only TIME produces a gap; the
    # cap pricing a node out is planned degradation inside authority.
    assert not (e["gap"] and e["unfunded"]), f"a node cannot be both gapped and unfunded: {e}"

# A run this size verifies nothing at most nodes (P2 makes verifier availability the
# terminator), so not_assessed must actually OCCUR here — otherwise the three-state check
# above has only ever seen two states and would pass a wire that dropped the third.
verdicts = {e["verdict"] for e in exits}
assert "not_assessed" in verdicts, (
    f"no unchecked node in a {len(exits)}-node run ({verdicts}): the third state is the "
    "common case, and a check that never meets it cannot detect its collapse"
)

# The entry event's own absence-not-zero site. --cap is set, so every allocation is a
# real positive figure; -1 is the Unlimited sentinel and 0 would be a node allowed
# nothing, which the floor refuses rather than funds.
for e in enters:
    assert e["alloc_micros"] == -1 or e["alloc_micros"] > 0, e
    assert e["at_unix_micros"] > 0, f"a wired clock must stamp the entry: {e}"

print(f"  ok: 1 frame, {len(enters)} live pairs ahead of the fold, verdicts {sorted(verdicts)}")
PY

# --live-events without --events-json is REFUSED, and it is usage rather than a fault:
# nothing ran. Checked from outside the process because the pair of numbers is what a
# host branches on, and cmd/quarry/main_test.go can only see the mapping.
set +e
"$Q" run --fake --quiet --live-events --cap 0.25 "anything" >"$work/refused.out" 2>/dev/null
refused=$?
set -e
[ "$refused" = "2" ] || {
	echo "FAIL: --live-events without --events-json must exit 2 (usage), got $refused. The" >&2
	echo "      events would have to land on the human's stdout, so refusing is better than" >&2
	echo "      choosing a destination — and 1 would report a fixable flag pair as a fault." >&2
	exit 1
}
[ ! -s "$work/refused.out" ] || {
	echo "FAIL: the refused invocation wrote to stdout. Nothing ran, so there is nothing a" >&2
	echo "      host should be able to parse." >&2
	exit 1
}

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

echo "== #11: the host mints the root ledger from outside the process"

# THE ENVIRONMENT HALF IS ONLY REACHABLE FROM HERE, and that is why this section exists
# rather than leaving #11 to cmd/quarry/hostcaps_test.go. A Go test passes a getenv
# function — deliberately, so precedence is testable without mutating state the parallel
# tests share — which means the wiring from the REAL environment to the real resolver is
# exactly the seam a Go test cannot see. A resolver that read the right variables through
# an os.Getenv nobody passed it would pass every unit test in the package.
set +e
QUARRY_CAP_MICROS=250000 QUARRY_SCOPE="lab=example,project=hydrology" QUARRY_DEPTH=2 \
	"$Q" run --fake --quiet --events-json --out "$work/h11.json" "$Q3" \
	>"$work/h11.ndjson" 2>/dev/null
env_run=$?
set -e
[ "$env_run" = "0" ] || {
	echo "FAIL: a cap set ONLY in the environment must satisfy host mode, got exit $env_run." >&2
	echo "      A host exporting QUARRY_CAP_MICROS in a unit file or container spec HAS" >&2
	echo "      chosen a cap; refusing it makes the environment level decorative (#11 D5)." >&2
	exit 1
}

# D3 — the forgotten cap. Observed as an EXIT CODE from outside, because that is what a
# supervisor branches on: a host that forgot the flag must be told, not handed a dollar.
set +e
"$Q" run --fake --quiet --events-json "anything" >"$work/nocap.out" 2>/dev/null
nocap=$?
set -e
[ "$nocap" = "2" ] || {
	echo "FAIL: --events-json with no explicitly-set cap must exit 2 (usage), got $nocap." >&2
	echo "      The interactive default would spend a dollar nobody authorised (#11 D3)," >&2
	echo "      and 1 would report a fixable omission as a quarry malfunction." >&2
	exit 1
}
[ ! -s "$work/nocap.out" ] || {
	echo "FAIL: the refused run wrote to stdout. Nothing ran, so a host has nothing to parse." >&2
	exit 1
}

# NON-VACUITY FOR THE LINE ABOVE: the same invocation WITH a cap must succeed, or the
# refusal could be coming from anything — the statement, --fake, the missing --out.
set +e
"$Q" run --fake --quiet --events-json --cap-micros 250000 --out "$work/withcap.json" \
	"anything" >/dev/null 2>&1
withcap=$?
set -e
[ "$withcap" = "0" ] || {
	echo "FAIL: the same invocation with --cap-micros must succeed, got $withcap — the" >&2
	echo "      refusal above is then not about the cap at all." >&2
	exit 1
}

# The caps and scope a host minted must reach the RECORD, which is the citable artifact.
# Read off the written file rather than off the stream: the record is what replays, and a
# resolver feeding a run that recorded something else is the defect this catches (P8).
python3 - "$work/h11.json" <<'PY'
import json, sys

rec = json.load(open(sys.argv[1]))

# D1 — INTEGER MICRO-UNITS ACROSS THE BOUNDARY. Units is int64 and never float (Go rule
# 3): apportionment uses largest-remainder distribution so shares sum exactly and two
# replays of one tree cannot diverge. A float round-trip at this edge is what would break
# that, so the assertion is on the exact integer and on its TYPE.
spend = rec["Caps"]["Spend"]
assert isinstance(spend, int), f"the spend cap must cross as an integer, got {type(spend)}"
assert spend == 250000, f"want exactly 250000 micro-units from the environment, got {spend}"

# D4/P6 — the scope tags reached the record, and therefore every cache key. quarry hashes
# and propagates them; it does not interpret them. The authoritative narrowing is the
# host's or IAM's and stays there.
tags = rec["Problem"]["Scope"]["Tags"]
assert tags.get("project") == "hydrology", f"the host's scope must reach the record: {tags}"
assert tags.get("lab") == "example", tags

# P2 — the depth BACKSTOP is recorded rather than inferred. A fact of the original
# execution: the bound is only visible from the tree's geometry if some node hit it, which
# is the third instance of that defect and why RunBounds exists.
assert rec["Bounds"]["MaxDepth"] == 2, f"the environment's depth bound must be recorded: {rec['Bounds']}"

# Non-vacuity: a single-node tree would satisfy all of the above while exercising none of
# the apportionment that makes any of it worth asserting.
assert len(rec["Outcomes"]) >= 2, f"the run must have decomposed, got {len(rec['Outcomes'])} nodes"

print(f"  ok: {spend} micro-units, scope {sorted(tags)}, depth bound "
      f"{rec['Bounds']['MaxDepth']}, {len(rec['Outcomes'])} nodes — all from the environment")
PY

# D2 — the absolute deadline, and the ONE THING IT DOES NOT YET BUY. --due populates
# Caps.Due, which makes Caps.Deferrable() true and therefore §3.1's price control
# reachable: a run not needed for three days can go to batch or off-peak at a discount.
# Nothing prices off it yet, so this checks the denomination arrives — not that it was
# cheaper, which would be asserting a build that has not happened.
set +e
QUARRY_DUE="2030-08-06T17:00:00Z" \
	"$Q" run --fake --quiet --events-json --out "$work/due.json" "$Q3" >/dev/null 2>&1
due_run=$?
set -e
[ "$due_run" = "0" ] || {
	echo "FAIL: a due date alone must satisfy P9's at-least-one-cap, got exit $due_run." >&2
	echo "      §3.1 makes time a first-class cap, not a lesser one." >&2
	exit 1
}
python3 - "$work/due.json" <<'PY'
import json, sys

caps = json.load(open(sys.argv[1]))["Caps"]
assert caps["Due"].startswith("2030-08-06T17:00:00"), f"the absolute due date must be recorded: {caps}"
# DEFERRABLE IS Due SET AND Latency ZERO. A resolver that helpfully derived a latency from
# the due date would record a due date and silently price the run as on-demand — the run
# would look right and cost more.
assert caps["Latency"] == 0, (
    f"a due date must NOT imply a latency cap: {caps} — deferrability is what converts "
    f"slack into money (§3.1), and a derived latency destroys it"
)
print("  ok: --due recorded with no derived latency, so the run is deferrable (§3.1)")
PY

# A refusal a host can actually hit: two spellings of one cap. Neither wins, because a
# silent winner is how a run ships at a millionth of its intended cap with no error.
set +e
"$Q" run --fake --quiet --events-json --cap 1.00 --cap-micros 1000000 "anything" >/dev/null 2>&1
both=$?
set -e
[ "$both" = "2" ] || {
	echo "FAIL: --cap and --cap-micros together must exit 2, got $both" >&2
	exit 1
}

echo "host contract ok"
