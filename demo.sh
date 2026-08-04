#!/usr/bin/env bash
#
# demo.sh — a narrated walk through quarry's whole surface.
#
# quarry's deliverable is not the prose answer; it is a run record with a cost
# receipt, a statement of what was NOT checked, and a proof that it reproduces.
# So the demo is built around the three verbs that make that claim inspectable:
#
#     run     execute under a cap, drawing the tree as it grows
#     show    read the record back: receipt, trust summary, per-node detail
#     replay  re-execute against the record's own responses — must match exactly
#
# Two modes:
#     ./demo.sh              --fake: no credentials, no network, no money
#     ./demo.sh --live       real Bedrock; three observed runs cost USD 0.08–0.11
#
# That figure is measured, not estimated — and every beat prints its own receipt,
# so the demo reports what it actually spent rather than asking to be trusted.
#
# --live is the mode that matters. The fake provider has a UNIFORM per-call cost,
# and that difference is not cosmetic: three replay defects lived only on the
# live path, because a tree with SOME children priced out is a shape --fake
# cannot construct at all. See docs/design.md §13.

set -uo pipefail   # NOT -e: several beats below EXPECT a non-zero exit.

# ---------------------------------------------------------------- configuration

LIVE=0
[[ "${1:-}" == "--live" ]] && LIVE=1

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="${QUARRY_DEMO_DIR:-/tmp/quarry-demo}"
BIN="$WORK/quarry"

if (( LIVE )); then
    MODE_FLAGS=()
    export AWS_PROFILE="${AWS_PROFILE:-aws}"
else
    # --fake-latency makes the live tree watchable. It is a MEAN with a ±40%
    # deterministic spread, which is what allows a deadline to truncate PART of
    # a fanout — with one constant latency a deadline hits all siblings or none,
    # and the partial run (§3.1's entire subject) could not be demonstrated.
    MODE_FLAGS=(--fake --fake-latency 180ms)
fi

# ---------------------------------------------------------------- presentation

bold=$'\033[1m'; dim=$'\033[2m'; cyan=$'\033[36m'; red=$'\033[31m'; off=$'\033[0m'
[[ -t 1 ]] || { bold=""; dim=""; cyan=""; red=""; off=""; }

step=0
say() { printf '\n%s\n' "${dim}$*${off}"; }
beat() {
    step=$((step + 1))
    printf '\n\n%s\n' "${bold}${cyan}── ${step}. $* ${off}"
}
cmd() {
    printf '%s\n\n' "${dim}\$ $*${off}"
    "$@"
}
pause() {
    [[ -n "${QUARRY_DEMO_NOPAUSE:-}" ]] && return 0
    [[ -t 0 ]] || return 0
    printf '\n%s' "${dim}[enter]${off}"; read -r _
}

# ---------------------------------------------------------------- build

printf '%s\n' "${bold}quarry — bounded recursive decomposition with verified provenance${off}"
if (( LIVE )); then
    printf '%s\n' "mode: ${red}LIVE Bedrock${off} (profile ${AWS_PROFILE}, spends real money)"
else
    printf '%s\n' "mode: ${cyan}--fake${off} (no credentials, no network, no money)"
fi

mkdir -p "$WORK"
say "building…"
( cd "$REPO" && go build -o "$BIN" ./cmd/quarry ) || exit 1

# Run everything from the work directory so records land there, not in the repo.
cd "$WORK" || exit 1

# capture <flags...> — runs quarry, shows its output, and sets REC to the record
# path plus RC to the exit code. Records are named by content hash, so the name
# is read back out of the output rather than guessed.
#
# Output goes to a FILE and is then printed, rather than through `tee /dev/tty`:
# tee fails outright when the script is redirected to a log, which silently
# swallowed two whole beats the first time this ran.
REC=""
RC=0
capture() {
    printf '%s\n\n' "${dim}\$ quarry run $*${off}"
    "$BIN" run "$@" > "$WORK/.out" 2>&1
    RC=$?
    cat "$WORK/.out"
    REC="$(grep -o 'quarry-run-[0-9a-f]*\.json' "$WORK/.out" | head -1)"
}

# =============================================================================
beat "A run that decomposes"
# =============================================================================
say "The planner is shown a budget in RELATIVE terms and decides whether splitting
is worth it. The tree draws as it grows; each node reports its own cost."

Q1="What does it cost to run a 200-node GPU cluster for a year, how does that scale with utilisation, and what dominates the bill?"
capture "${MODE_FLAGS[@]}" --cap 0.25 --depth 2 "$Q1"
REC1="$REC"
pause

# =============================================================================
beat "The record, read back"
# =============================================================================
say "Same record, different projection. 'show' recomputes the receipt from the
file — note it also reports what was NOT verified, which is a deliverable and
not an omission (§8): nil verdict means UNCHECKED, distinct from checked-and-failed."

cmd "$BIN" show "$REC1"
pause

# =============================================================================
beat "Per-node click-through, and the claims"
# =============================================================================
say "Every node is addressable: its problem, its answer, its verdict, its cost."

cmd "$BIN" show --node n0.0 "$REC1"

say "Claims are extracted mechanically and each is attributed to the node that
produced it. This is the floor, not a measurement: equivalence here is
normalised-string equality, so agreement phrased differently is MISSED —
undercounting, which is the safe direction.

(Truncated for the demo; a live run extracts hundreds.)"

printf '%s\n\n' "${dim}\$ quarry show --claims $REC1   ${off}"
"$BIN" show --claims "$REC1" | head -25
printf '%s\n' "${dim}…${off}"
pause

# =============================================================================
beat "Replay: the record reproduces, byte for byte"
# =============================================================================
say "This is P8, the load-bearing claim. Replay substitutes THREE seams — planner,
provider, reducer — because all three are stochastic, then re-executes the tree
and re-hashes it. No model is called. Identical bytes or it reports a divergence."

cmd "$BIN" replay "$REC1"
printf '%s\n' "${dim}exit ${?}${off}"
pause

# =============================================================================
beat "An edited record is not citable"
# =============================================================================
say "The RunID is the content hash. Editing an answer breaks it two ways at once:
the file no longer hashes to its own ID, and the replay re-extracts claims from
the tampered content that disagree with the claims the record states."

python3 - "$REC1" <<'PY'
import json, sys
rec = json.load(open(sys.argv[1]))
for o in rec["Outcomes"]:
    if o.get("Content"):
        o["Content"] = "Utilisation is irrelevant to cluster cost."
        break
json.dump(rec, open("tampered.json", "w"), indent=2)
print("wrote tampered.json (one answer replaced)")
PY

cmd "$BIN" replay tampered.json
printf '%s\n' "${dim}exit ${?} — a divergence is a failure, and scripts can see it${off}"
pause

# =============================================================================
beat "The cap is the contract, not a target"
# =============================================================================
say "A cap far below the cost of the work does not overspend and does not crash.
Planning is budget-conditioned (P9): the plan is checked against the money
BEFORE any of it is spent, and a split that cannot be funded is refused rather
than started and abandoned halfway."

capture "${MODE_FLAGS[@]}" --cap 0.0005 --depth 2 "$Q1"
REC2="$REC"
printf '%s\n' "${dim}exit ${RC} — nothing was spent; the refusal names the child that could
not be funded and what to do about it${off}"

if [[ -n "$REC2" ]]; then
    say "And it still replays. A cap-truncated record is the normal outcome under a
budget, not a broken file — refusing to replay one would make the records most
worth interrogating the ones the tool declines to look at."
    cmd "$BIN" replay "$REC2"
elif (( ! LIVE )); then
    say "Worth naming what --fake CANNOT show here: a tree where SOME children were
funded and others priced out. The fake's per-call cost is uniform, so
affordability either funds every child or declines the split — the partially
funded tree does not exist in this mode. It takes a real price sheet, where one
sub-question costs several times another.

That gap is not academic. Three replay defects lived in it, invisible to a green
test suite and to every --fake run. Use --live to see the shape."
fi
pause

# =============================================================================
beat "Money and time are different failures"
# =============================================================================
say "The distinction the record refuses to blur. A node the DEADLINE cut short is
a gap. A node the BUDGET could not afford is not — it is planned degradation
inside the authority granted, and calling it a gap would make a cap that worked
look like a malfunction. Only TIME produces a gap.

It matters downstream: extending a run must offer more of whatever actually
bound it, and offering money to a run that ran out of time refills nothing."

# census <record> — prints the three-way split and reports whether the mixture
# appeared. Exits 3 when there are no unfunded nodes, so the caller can try a
# tighter cap rather than silently claim a shape the record does not show.
census() {
    python3 - "$1" <<'PY'
import json, sys
rec = json.load(open(sys.argv[1]))
gap = unfunded = funded = 0
for o in rec["Outcomes"]:
    if o.get("Gap"):
        gap += 1
    elif not o.get("CacheHit") and not o.get("Children") \
            and not o.get("Model") and not o.get("Content"):
        unfunded += 1
    else:
        funded += 1
print(f"  {funded} completed   {unfunded} unfunded (spend)   {gap} gaps (time)")
sys.exit(0 if unfunded else 3)
PY
}

if [[ -n "$REC1" ]] && census "$REC1"; then
    :
elif (( LIVE )); then
    say "That cap covered the whole tree. Tightening it until some children are
funded and others are not — the mixture is what matters:"
    capture --cap 0.03 --depth 3 --quiet "$Q1"
    [[ -n "$REC" ]] && census "$REC"
fi

if (( LIVE )); then
    say "A mixture of funded and unfunded children is UNREACHABLE with --fake, whose
per-call cost is uniform: affordability there funds every child or declines the
split outright. It takes a real price sheet, where one sub-question costs several
times another. Three replay defects lived in exactly that gap."
fi
pause

# =============================================================================
beat "Bound by time instead of money"
# =============================================================================
say "The other cap. Nodes the deadline cut short are GAPS — named, never hidden,
and never silently confused with nodes the budget could not afford. Only TIME
produces a gap; a priced-out node is planned degradation inside authority."

if (( LIVE )); then
    capture --cap 0.25 --deadline 25s --depth 2 "$Q1"
else
    capture "${MODE_FLAGS[@]}" --cap 1.00 --deadline 400ms --depth 2 "$Q1"
fi
REC3="$REC"
printf '%s\n' "${dim}exit ${RC}${off}"

if [[ -n "$REC3" ]]; then
    say "A partial record replays as partial — the gaps replay AS GAPS, rather than
being asked for again and reported as a divergence."
    cmd "$BIN" replay "$REC3"
fi
pause

# =============================================================================
beat "Scope never widens on descent"
# =============================================================================
say "Scope tags are carried into every cache key (P6), so an answer computed for
one entitlement can never be served to another. Children inherit scope; the
planner cannot name it, because a planner that could would be able to widen it."

capture "${MODE_FLAGS[@]}" --cap 0.15 --depth 1 --scope "team=alpha,course=chem-101" \
    "Which storage tier suits a 40TB working set?"
pause

# =============================================================================
beat "The canonical bytes"
# =============================================================================
say "What is actually hashed. Note Timing is absent from the encoding: it is
deliberately unhashed, because it differs every run. The trade, stated plainly —
a record proves what was spent and decided, never how long it took."

printf '%s\n\n' "${dim}\$ quarry show --json $REC1   ${off}"
"$BIN" show --json "$REC1" | head -40
printf '%s\n' "${dim}…${off}"
printf '\n%s\n' "${dim}(--json is indented for reading; the hash is over the canonical encoding.
 Full record: $WORK/$REC1)${off}"

# =============================================================================
printf '\n\n%s\n' "${bold}${cyan}── done${off}"
# =============================================================================
printf '%s\n' "records in ${WORK}:"
ls -1 "$WORK"/*.json 2>/dev/null | sed 's|^|  |'
cat <<EOF

  ${bold}What was demonstrated${off}
    a run under a cap, decomposed, with a per-node cost receipt
    a record that reproduces byte-for-byte with no model access  (P8)
    an edited record detected, and the divergence NAMED
    two caps — money and time — each degrading without crashing
    gaps and unfunded nodes kept distinct                        (§3.1)
    scope carried into every key                                 (P6)

  ${bold}Next${off}
    $(basename "$0") --live      the same walk against real Bedrock
    quarry show --node <id> <record.json>
EOF
if (( ! LIVE )); then
cat <<EOF

  ${dim}--fake is not a small live provider. Its per-call cost is uniform, so a
  tree with SOME children priced out is unreachable, and its planner declines on
  clause length before it ever reaches the depth bound. Three replay defects
  lived only on the far side of that difference (docs/design.md §13).${off}
EOF
fi
printf '\n'
