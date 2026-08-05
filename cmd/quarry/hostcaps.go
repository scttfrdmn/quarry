package main

import (
	"flag"
	"strconv"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// The host-settable root ledger: caps and scope from flags and environment (#11).
//
// A supervising host spawns quarry as a subprocess and must mint the root ledger from
// OUTSIDE the process — the spend cap, the deadline, and the scope tags that travel to
// every node. That is a different caller from a person at a terminal, and the two want
// opposite defaults: a human wants `quarry run --fake "question"` to work with no
// ceremony, while a host that forgets a cap must be REFUSED rather than handed an
// implicit dollar of someone's money (D3).
//
// Resolution lives here, in a function runCmd calls, rather than inline in the flag
// block. Not tidiness: precedence and set-ness are the whole substance of this issue, and
// logic reachable only by spawning a process is logic nothing asserts directly.

// The environment fallbacks (#11 D5).
//
// PRECEDENCE IS EXPLICIT FLAG > ENVIRONMENT > DEFAULT, and THERE IS NO CONFIG FILE.
// Documented rather than left to be discovered, because adding a file later must not
// change this order — a config file that outranked the environment would silently
// re-point a host that had been setting the environment correctly for months.
//
// --deadline HAS NO ENVIRONMENT VARIABLE, deliberately, and the asymmetry is the point.
// It is the RELATIVE human form; D2 rules that a host's deadline is ABSOLUTE, because the
// host owns the clock — it knows when the request arrived and what it promised — and
// quarry must not resolve a duration against an instant it is not supposed to read (Go
// rule 4). Giving the relative form an environment variable would invite exactly the
// caller D2 addresses to reach for the wrong shape.
const (
	envCapMicros = "QUARRY_CAP_MICROS"
	envDue       = "QUARRY_DUE"
	envDepth     = "QUARRY_DEPTH"
	envScope     = "QUARRY_SCOPE"
)

// rootInputs is the raw knob state runCmd hands the resolver: every parsed flag value,
// plus WHICH FLAGS THE CALLER ACTUALLY TYPED.
//
// SET-NESS IS CARRIED SEPARATELY because the flag package cannot recover it. A --cap left
// at its default and a --cap explicitly set to 1.00 hold the same value, and D3 turns
// entirely on the difference: comparing against the default value would let a host that
// deliberately chose the default dollar be refused, and a host that forgot the flag be
// accepted, if the default ever changed. fs.Visit is the only thing that knows.
type rootInputs struct {
	capDecimal string
	capMicros  int64
	deadline   time.Duration
	due        string
	depth      int
	scope      string

	// set holds the flag names fs.Visit reported — the ones present in argv.
	set map[string]bool

	// hostMode is --events-json: a machine is reading this stream, so defaults are
	// refused (D3). Interactive `quarry run` keeps its defaults; a human at a terminal is
	// not the failure mode this guards.
	hostMode bool
}

// rootConfig is a resolved invocation: the three things that constitute the root ledger
// and the tree it is allowed to grow.
type rootConfig struct {
	Caps  quarry.Caps
	Depth int
	Scope quarry.Scope
}

// setFlags records which flags appeared in argv, for rootInputs.set.
//
// fs.Visit — NOT fs.VisitAll, which reports every defined flag whether or not it was
// given, and would make every default look explicit. That distinction is D3.
func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// resolveRoot builds the root caps, depth backstop and scope from flags, environment and
// defaults, in that order (D5).
//
// getenv is a parameter rather than os.Getenv so precedence is testable without mutating
// the process environment — which, in a package whose tests run in parallel, is shared
// mutable state.
func resolveRoot(in rootInputs, getenv func(string) string) (rootConfig, error) {
	var cfg rootConfig

	spend, spendSet, err := resolveSpend(in, getenv)
	if err != nil {
		return cfg, err
	}
	due, dueSet, err := resolveDue(in, getenv)
	if err != nil {
		return cfg, err
	}
	depth, err := resolveDepth(in, getenv)
	if err != nil {
		return cfg, err
	}
	scope, err := resolveScope(in, getenv)
	if err != nil {
		return cfg, err
	}

	// D3: in host mode a cap must have been CHOSEN. P9 requires at least one real cap, and
	// Caps.Validate() enforces that — but a defaulted --cap satisfies it silently, which is
	// the defect: the check passes while nobody decided anything.
	//
	// ANY denomination counts, because P9's requirement is that the run be
	// budget-conditioned in some denomination, not that money specifically be capped. A
	// host that sets only a deadline has conditioned the run on time, which §3.1 makes a
	// first-class cap rather than a lesser one.
	if in.hostMode && !spendSet && !in.set["deadline"] && !dueSet {
		return cfg, usageErrf("--events-json without an explicitly-set cap\n" +
			"  a host must choose the cap; the interactive default would spend a dollar nobody\n" +
			"  authorised (#11 D3). set --cap-micros (or " + envCapMicros + "), --deadline, or --due")
	}

	cfg.Caps = quarry.Caps{Spend: spend, Latency: in.deadline, Due: due}
	cfg.Depth = depth
	cfg.Scope = scope
	return cfg, nil
}

// resolveSpend picks the spend cap and reports whether anybody actually chose it.
//
// D1: --cap-micros IS THE HOST PATH and takes an integer. Units is int64 micro-units and
// never float (Go rule 3); apportionment uses largest-remainder distribution so shares
// sum exactly and replay is bit-stable (P8). A host passing "1.00" for a shell to parse
// reintroduces float at the one edge that must not have it. --cap keeps the decimal form
// for humans, who are not crossing that boundary.
func resolveSpend(in rootInputs, getenv func(string) string) (quarry.Units, bool, error) {
	// BOTH SPELLINGS SET IS A REFUSAL, not a precedence rule. They are two spellings of one
	// cap, so which wins is not a thing a host should have to know — and a silent winner is
	// how a run ships at a millionth of its intended cap with no error anywhere.
	if in.set["cap"] && in.set["cap-micros"] {
		return 0, false, usageErrf("--cap and --cap-micros are two spellings of one cap; set one\n" +
			"  --cap takes a decimal amount for a person; --cap-micros takes integer\n" +
			"  micro-units for a host, because Units is int64 and never float (#11 D1)")
	}
	if in.set["cap-micros"] {
		u, err := microUnits(in.capMicros, "--cap-micros")
		return u, true, err
	}
	if in.set["cap"] {
		u, err := capFlag(in.capDecimal)
		return u, true, err
	}
	// Environment, below both flags (D5).
	if s := getenv(envCapMicros); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false, usageErrf("%s=%q is not an integer\n"+
				"  micro-units are integral: 1000000 is one unit, and a decimal here means the\n"+
				"  caller confused this with --cap (#11 D1)", envCapMicros, s)
		}
		u, err := microUnits(n, envCapMicros)
		return u, true, err
	}
	// The default, and NOT reported as set: that is exactly what D3 refuses in host mode.
	u, err := capFlag(in.capDecimal)
	return u, false, err
}

// microUnits validates an integer micro-unit cap.
//
// STRICTLY INTEGER, which is why the flag is an int64 rather than a string parsed as a
// float and multiplied. A decimal reaching this flag is an ERROR rather than a rounded
// value: the caller confused the two spellings, and guessing which they meant is how a
// cap silently becomes a millionth of itself. Go's flag package rejects "1.00" for an
// Int64 flag already; this function's job is the range check that follows.
func microUnits(n int64, name string) (quarry.Units, error) {
	// Unlimited is representable and meaningful — Caps.Validate() accepts it as long as
	// another denomination is real — but it must be spelled deliberately rather than
	// arrived at by a negative number. Any other negative is a mistake, and a negative cap
	// admitted as Unlimited would turn a typo into an uncapped run.
	if n < 0 && quarry.Units(n) != quarry.Unlimited {
		return 0, usageErrf("%s must be positive (or exactly %d for unlimited), got %d",
			name, int64(quarry.Unlimited), n)
	}
	if n == 0 {
		return 0, usageErrf("%s must be positive, got 0\n"+
			"  a cap of zero funds nothing; omit the flag to leave the run uncapped in this\n"+
			"  denomination, which P9 permits only if another cap is real", name)
	}
	return quarry.Units(n), nil
}

// resolveDue parses the absolute deadline (D2) and reports whether one was given.
//
// RFC3339, RESOLVED BY THE HOST. This is what makes Caps.Due reachable from outside the
// process and therefore Caps.Deferrable() true — §3.1's price control, where slack is
// convertible into money because a run not needed for three days can go to batch
// inference or off-peak at a discount.
//
// TODO(§3.1): NOTHING PRICES OFF Deferrable() YET, and saying so here is more honest than
// letting this comment imply otherwise. Caps.Due has one real consumer today —
// RootContext, which takes the earlier of Latency and Due as the context deadline — so
// --due genuinely bounds a run. What it does not yet do is buy anything cheaper: the
// design names ember as the executor for the deferred mode and no provider consults
// Deferrable(). This flag makes the denomination reachable; the discount is a separate
// build.
func resolveDue(in rootInputs, getenv func(string) string) (time.Time, bool, error) {
	s := in.due
	src := "--due"
	if !in.set["due"] {
		if s = getenv(envDue); s == "" {
			return time.Time{}, false, nil
		}
		src = envDue
	}
	// AN EXPLICITLY EMPTY FLAG CLEARS, and it must not fall through to the environment.
	// `--due ""` is a host saying "no due date for this run" — and if that fell through,
	// a host with QUARRY_DUE exported could not turn it off for one invocation without
	// unsetting a variable it may not control. This is the case that makes set-ness
	// genuinely different from emptiness here rather than an equivalent spelling of it.
	if s == "" {
		return time.Time{}, false, nil
	}
	// RFC3339 and nothing else. A permissive parser accepting several layouts would make
	// the boundary's meaning depend on which one a host happened to emit, and an instant
	// misread by a timezone is a deadline wrong by hours.
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false, usageErrf("%s %q is not an RFC3339 instant\n"+
			"  a host's deadline is absolute because the host owns the clock (#11 D2):\n"+
			"  e.g. 2026-08-06T17:00:00Z or 2026-08-06T13:00:00-04:00", src, s)
	}
	// A DUE DATE IN THE PAST IS ACCEPTED, deliberately, and this is a ruling rather than an
	// omission. It is not malformed — it is expired, which a host that queued a request can
	// reach by ordinary delay — and §3.1 grants partial tolerance to exactly this: whatever
	// exists must be returnable now. The faithful outcome is a truncated record whose gaps
	// are named, not a refusal that produces no artifact at all.
	return t, true, nil
}

// resolveDepth picks the recursion backstop.
//
// A BACKSTOP, NOT THE DESIGN (P2). Recursion is meant to be bounded by verifier
// availability — recurse only as deep as you have verifiers — and a run bounded by depth
// is UNDER-VERIFIED rather than complete. It is host-settable because a host must be able
// to bound the tree it pays for, not because raising it is how you get a better answer.
func resolveDepth(in rootInputs, getenv func(string) string) (int, error) {
	if in.set["depth"] {
		return checkDepth(in.depth, "--depth")
	}
	if s := getenv(envDepth); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, usageErrf("%s=%q is not an integer", envDepth, s)
		}
		return checkDepth(n, envDepth)
	}
	return in.depth, nil
}

// checkDepth rejects a negative bound. Zero is LEGITIMATE — it means solve the root and
// do not decompose, which is a real request (P1: the planner must be able to decline, and
// so must the caller) and the degenerate case the suite exercises.
func checkDepth(n int, name string) (int, error) {
	if n < 0 {
		return 0, usageErrf("%s must be zero or more, got %d\n"+
			"  zero means solve the root without decomposing, which is a real request", name, n)
	}
	return n, nil
}

// resolveScope picks the scope tags (D4, P6).
//
// quarry HASHES THEM AND PROPAGATES THEM; it does not interpret them. They are part of
// every cache key — never the statement hash alone — because two callers can pose a
// hash-identical sub-problem while holding different entitlements, and one's cached answer
// may derive from documents the other cannot see.
//
// The AUTHORITATIVE narrowing belongs elsewhere and stays there: for agate's scope path
// the real check is IAM's, and quarry's local NarrowsTo is "a fast-fail courtesy, not the
// security boundary" (integration-requirements §2). A twin gateway is the same shape — it
// enforces, quarry propagates — so nothing here validates a tag's meaning.
func resolveScope(in rootInputs, getenv func(string) string) (quarry.Scope, error) {
	s := in.scope
	// SET-NESS, NOT EMPTINESS, for the same reason as resolveDue: `--scope ""` must clear
	// rather than fall through to QUARRY_SCOPE. Here the consequence is sharper — falling
	// through would attach tags a host explicitly declined, and a tag it did not choose is
	// authority it did not grant (P6).
	if !in.set["scope"] {
		s = getenv(envScope)
	}
	return parseScope(s)
}
