package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// #11: a supervising host mints the root ledger from outside the process.
//
// These tests are about the BOUNDARY, not about the ledger: that Units crosses it as an
// integer, that a deadline crosses it as an absolute instant, that a forgotten cap is
// refused rather than defaulted, and that scope survives to every cache key. The ledger's
// own arithmetic is ledger_test.go's subject.

// noEnv is the environment for a test that is not about the environment. A function rather
// than os.Getenv so precedence is testable without mutating process state, which the
// parallel tests in this package share.
func noEnv(string) string { return "" }

// env builds a getenv from a map, for the precedence tests.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// hostFlags parses argv through the SAME FlagSet shape runCmd uses and returns the
// resolver's inputs.
//
// THE POINT IS fs.Visit. A test that hand-populated rootInputs.set would be asserting on
// its own bookkeeping rather than on what the flag package reports, and set-ness is the
// entire substance of D3 — a hand-assigned field cannot discover that nothing produces it.
func hostFlags(t *testing.T, argv []string) rootInputs {
	t.Helper()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	var (
		capS       = fs.String("cap", "1.00", "")
		capMicros  = fs.Int64("cap-micros", 0, "")
		deadline   = fs.Duration("deadline", 0, "")
		due        = fs.String("due", "", "")
		depth      = fs.Int("depth", 3, "")
		scopeS     = fs.String("scope", "", "")
		eventsJSON = fs.Bool("events-json", false, "")
	)
	if err := fs.Parse(argv); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return rootInputs{
		capDecimal: *capS,
		capMicros:  *capMicros,
		deadline:   *deadline,
		due:        *due,
		depth:      *depth,
		scope:      *scopeS,
		set:        setFlags(fs),
		hostMode:   *eventsJSON,
	}
}

// TestHostModeRefusesAnUnsetCap is #11 D3, and the criterion the issue states in bold: a
// forgotten cap must be an ERROR, not an implicit dollar of someone's money.
//
// P9 is what makes this more than input validation. Caps.Validate() already requires at
// least one real cap — but a DEFAULTED --cap satisfies it silently, so the gate passes
// while nobody has decided anything. The distinction is set-ness, and only fs.Visit knows
// it.
func TestHostModeRefusesAnUnsetCap(t *testing.T) {
	cases := []struct {
		name  string
		argv  []string
		want  bool // refused?
		hasIt string
	}{
		{
			name:  "host mode with no cap at all is refused",
			argv:  []string{"--events-json"},
			want:  true,
			hasIt: "--cap-micros",
		},
		{
			// THE ONE THAT MAKES THE TEST NON-VACUOUS. This argv reaches Validate() with
			// exactly the same Caps as the refused case above — a defaulted 1.00 — so a check
			// comparing against the default value would accept one and refuse the other only
			// by accident. What differs is that a human typed it.
			name: "the same defaulted value, chosen explicitly, is accepted",
			argv: []string{"--events-json", "--cap", "1.00"},
			want: false,
		},
		{
			name: "micro-units satisfy it, being the host spelling",
			argv: []string{"--events-json", "--cap-micros", "250000"},
			want: false,
		},
		{
			// P9 asks for a real cap in SOME denomination, not for money specifically. §3.1
			// makes time a first-class cap, so a host that conditioned the run on a deadline
			// has conditioned it.
			name: "a deadline alone satisfies it: P9 is about denominations, not about money",
			argv: []string{"--events-json", "--deadline", "5s"},
			want: false,
		},
		{
			name: "an absolute due date alone satisfies it too",
			argv: []string{"--events-json", "--due", "2030-01-01T00:00:00Z"},
			want: false,
		},
		{
			// Interactive runs keep their defaults. A human at a terminal is not the failure
			// mode D3 guards, and refusing them would make the demo require ceremony.
			name: "interactive mode still defaults",
			argv: []string{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveRoot(hostFlags(t, tc.argv), noEnv)
			if tc.want {
				if err == nil {
					t.Fatal("a host that set no cap must be refused: the interactive default " +
						"would spend a dollar nobody authorised (#11 D3)")
				}
				if !errors.Is(err, errUsage) {
					t.Errorf("a refused cap is a USAGE error — nothing ran, so it is not a fault: %v", err)
				}
				if !strings.Contains(err.Error(), tc.hasIt) {
					t.Errorf("the message must name the flag that fixes it, want %q in: %v", tc.hasIt, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

// TestTheEnvironmentSatisfiesTheHostCapToo: D5's precedence has three levels, and the
// middle one has to satisfy D3 as well.
//
// A host that exports QUARRY_CAP_MICROS in a systemd unit or a container spec HAS chosen a
// cap — refusing it because the choice arrived by environment rather than argv would make
// the environment level decorative.
func TestTheEnvironmentSatisfiesTheHostCapToo(t *testing.T) {
	cfg, err := resolveRoot(hostFlags(t, []string{"--events-json"}),
		env(map[string]string{envCapMicros: "500000"}))
	if err != nil {
		t.Fatalf("an environment cap is a chosen cap: %v", err)
	}
	if cfg.Caps.Spend != quarry.Units(500_000) {
		t.Errorf("want 500000 micro-units from the environment, got %d", cfg.Caps.Spend)
	}
}

// TestPrecedenceIsFlagThenEnvironmentThenDefault pins D5's order.
//
// Documented AND asserted because adding a config file later must not change it: a file
// that outranked the environment would silently re-point a host that had been setting the
// environment correctly for months.
func TestPrecedenceIsFlagThenEnvironmentThenDefault(t *testing.T) {
	all := map[string]string{
		envCapMicros: "111111",
		envDue:       "2030-06-01T00:00:00Z",
		envDepth:     "7",
		envScope:     "lab=fromenv",
	}

	// Flag beats environment, for all four knobs at once.
	cfg, err := resolveRoot(hostFlags(t, []string{
		"--cap-micros", "222222", "--due", "2031-06-01T00:00:00Z",
		"--depth", "9", "--scope", "lab=fromflag",
	}), env(all))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.Spend != quarry.Units(222_222) {
		t.Errorf("--cap-micros must beat %s, got %d", envCapMicros, cfg.Caps.Spend)
	}
	if cfg.Caps.Due.Year() != 2031 {
		t.Errorf("--due must beat %s, got %v", envDue, cfg.Caps.Due)
	}
	if cfg.Depth != 9 {
		t.Errorf("--depth must beat %s, got %d", envDepth, cfg.Depth)
	}
	if cfg.Scope.Tags["lab"] != "fromflag" {
		t.Errorf("--scope must beat %s, got %v", envScope, cfg.Scope.Tags)
	}

	// Environment beats default, for the same four.
	cfg, err = resolveRoot(hostFlags(t, nil), env(all))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.Spend != quarry.Units(111_111) {
		t.Errorf("%s must beat the --cap default, got %d", envCapMicros, cfg.Caps.Spend)
	}
	if cfg.Caps.Due.Year() != 2030 {
		t.Errorf("%s must be read when --due is unset, got %v", envDue, cfg.Caps.Due)
	}
	if cfg.Depth != 7 {
		t.Errorf("%s must beat the --depth default, got %d", envDepth, cfg.Depth)
	}
	if cfg.Scope.Tags["lab"] != "fromenv" {
		t.Errorf("%s must beat the empty --scope default, got %v", envScope, cfg.Scope.Tags)
	}

	// Default when neither is present. Asserted so a resolver that read the environment
	// unconditionally could not pass the two cases above by ignoring set-ness.
	cfg, err = resolveRoot(hostFlags(t, nil), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.Spend != quarry.FromFloat(1.00) {
		t.Errorf("the interactive default is 1.00, got %s", cfg.Caps.Spend)
	}
	if cfg.Depth != 3 {
		t.Errorf("the default depth backstop is 3, got %d", cfg.Depth)
	}
	if !cfg.Caps.Due.IsZero() {
		t.Errorf("an unset due date must stay zero, got %v", cfg.Caps.Due)
	}
}

// TestAnExplicitlyEmptyFlagClearsRatherThanFallingThrough is the half of D5 that a
// mutation found: precedence is about SET-NESS at every level, not emptiness.
//
// `--due ""` and `--scope ""` are a host saying "not for this run". If either fell through
// to its environment variable, a host that had one exported could not turn it off for a
// single invocation without unsetting a variable it may not control — and for scope the
// consequence is sharper than inconvenience: it would attach tags the host explicitly
// declined, which is authority it did not grant (P6).
func TestAnExplicitlyEmptyFlagClearsRatherThanFallingThrough(t *testing.T) {
	jar := env(map[string]string{envDue: "2030-06-01T00:00:00Z", envScope: "lab=fromenv"})

	cfg, err := resolveRoot(hostFlags(t, []string{"--due", "", "--scope", ""}), jar)
	if err != nil {
		// Named rather than reported bare, because a resolver that dropped the clearing check
		// fails HERE — trying to parse "" as an instant — and the mechanism ("not an RFC3339
		// instant") reads as a bad input rather than as the missing guarantee it is.
		t.Fatalf("an explicitly empty flag must CLEAR, not be parsed and not fall through: %v", err)
	}
	if !cfg.Caps.Due.IsZero() {
		t.Errorf(`--due "" must clear, not fall through to %s: got %v`, envDue, cfg.Caps.Due)
	}
	if len(cfg.Scope.Tags) != 0 {
		t.Errorf(`--scope "" must clear, not fall through to %s: got %v — a tag the host `+
			`declined is authority it did not grant (P6)`, envScope, cfg.Scope.Tags)
	}
	// Non-vacuity: the same environment DOES apply when the flags are absent, so the
	// assertions above are about the empty flag rather than about a jar nothing reads.
	cfg, err = resolveRoot(hostFlags(t, nil), jar)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.Due.IsZero() || cfg.Scope.Tags["lab"] != "fromenv" {
		t.Fatalf("the environment must apply when the flags are unset, or this test pins "+
			"nothing: due=%v scope=%v", cfg.Caps.Due, cfg.Scope.Tags)
	}
}

// TestTheSpendCapCrossesTheBoundaryAsAnInteger is D1.
//
// Units is int64 micro-units and never float (Go rule 3): apportionment uses
// largest-remainder distribution so shares sum exactly and two replays of one tree cannot
// diverge (P8). A host passing "1.00" for a shell to parse reintroduces float at the one
// edge that must not have it, so the host spelling takes an integer and the flag package
// refuses a decimal for it.
func TestTheSpendCapCrossesTheBoundaryAsAnInteger(t *testing.T) {
	// The exact micro-unit value survives, with no float round-trip. 333_333 is chosen
	// because it is not representable as a clean decimal fraction of a dollar: a resolver
	// that went through float64 and back would be visible here.
	cfg, err := resolveRoot(hostFlags(t, []string{"--cap-micros", "333333"}), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.Spend != quarry.Units(333_333) {
		t.Errorf("want exactly 333333 micro-units across the boundary, got %d — a float\n"+
			"round-trip at this edge is what makes two replays of one tree diverge (P8)",
			cfg.Caps.Spend)
	}

	// A decimal is refused BY THE FLAG PACKAGE, before the resolver sees it. Asserted here
	// rather than assumed, because "the flag type rejects it" is the mechanism the integer
	// guarantee rests on.
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	fs.Int64("cap-micros", 0, "")
	if err := fs.Parse([]string{"--cap-micros", "1.00"}); err == nil {
		t.Error("--cap-micros must refuse a decimal: a caller writing 1.00 confused it with " +
			"--cap, and silently truncating gives them a run at a millionth of the cap")
	}

	// Both spellings at once is a REFUSAL, not a precedence rule. Which wins is not
	// something a host should have to know, and a silent winner is how a run ships at a
	// millionth of its intended cap with no error anywhere.
	_, err = resolveRoot(hostFlags(t, []string{"--cap", "1.00", "--cap-micros", "1000000"}), noEnv)
	if err == nil {
		t.Error("--cap and --cap-micros together must be refused")
	} else if !errors.Is(err, errUsage) {
		t.Errorf("two spellings of one cap is a usage error, got %v", err)
	}

	// Zero is refused in both spellings: a cap of zero funds nothing, and P9's "at least one
	// real cap" is not satisfied by a cap that cannot buy a call.
	for _, argv := range [][]string{{"--cap-micros", "0"}, {"--cap", "0"}} {
		if _, err := resolveRoot(hostFlags(t, argv), noEnv); err == nil {
			t.Errorf("%v: a zero cap must be refused", argv)
		}
	}
}

// TestAnAbsoluteDueDateMakesTheRunDeferrable is D2, and the acceptance criterion asks for
// exactly this: that Deferrable() becomes true and the off-peak path is therefore
// reachable.
//
// §3.1: a due date without a latency cap means the run is not needed soon, so batch
// inference, off-peak and deferred execution become available — giving up FAST mechanically
// buys CHEAP. The deadline field is a price control, not merely a scheduling field. Before
// this flag, Caps.Due had no way in from the command line at all, so the whole denomination
// was dark from outside the process.
func TestAnAbsoluteDueDateMakesTheRunDeferrable(t *testing.T) {
	cfg, err := resolveRoot(hostFlags(t, []string{"--due", "2030-08-06T17:00:00Z"}), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.Due.IsZero() {
		t.Fatal("--due must populate Caps.Due")
	}
	if !cfg.Caps.Deferrable() {
		t.Error("a due date with no --deadline must be deferrable: that is what converts " +
			"slack into money (§3.1), and it was unreachable from the CLI before #11")
	}

	// A LATENCY CAP DESTROYS DEFERRABILITY, and this half is why the assertion above is not
	// merely "Due is set". Deferrable() is !Due.IsZero() && Latency == 0: the run must be
	// bound by WHEN, not by HOW LONG. A resolver that helpfully derived a latency from the
	// due date would satisfy the first assertion and silently price the run as on-demand.
	cfg, err = resolveRoot(hostFlags(t, []string{"--due", "2030-08-06T17:00:00Z", "--deadline", "30s"}), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.Deferrable() {
		t.Error("--due with --deadline is NOT deferrable: the run is needed within a window, " +
			"so there is no slack to sell")
	}
	if cfg.Caps.Latency != 30*time.Second {
		t.Errorf("--deadline must survive alongside --due, got %v", cfg.Caps.Latency)
	}

	// The timezone is honoured rather than assumed. An instant misread by an offset is a
	// deadline wrong by hours, which is why the parser is RFC3339 and nothing else.
	cfg, err = resolveRoot(hostFlags(t, []string{"--due", "2030-08-06T13:00:00-04:00"}), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2030, 8, 6, 17, 0, 0, 0, time.UTC); !cfg.Caps.Due.Equal(want) {
		t.Errorf("an offset must be honoured: want %v, got %v", want, cfg.Caps.Due)
	}

	// A non-RFC3339 instant is refused. A permissive parser accepting several layouts would
	// make the boundary's meaning depend on which one a host happened to emit.
	for _, bad := range []string{"tomorrow", "2030-08-06", "1785913439"} {
		if _, err := resolveRoot(hostFlags(t, []string{"--due", bad}), noEnv); err == nil {
			t.Errorf("--due %q must be refused: the host's deadline is an absolute instant", bad)
		}
	}
}

// TestAnExpiredDueDateIsAcceptedNotRefused pins the ruling in resolveDue.
//
// A due date in the past is not malformed — it is EXPIRED, which a host that queued a
// request reaches by ordinary delay. §3.1 grants partial tolerance to exactly this case:
// whatever exists must be returnable now, so the faithful outcome is a truncated record
// whose gaps are named, not a refusal that produces no artifact at all.
func TestAnExpiredDueDateIsAcceptedNotRefused(t *testing.T) {
	cfg, err := resolveRoot(hostFlags(t, []string{"--due", "2001-01-01T00:00:00Z"}), noEnv)
	if err != nil {
		t.Fatalf("an expired due date is a run that produces a truncated record, not a "+
			"usage error: %v", err)
	}
	if cfg.Caps.Due.IsZero() {
		t.Error("the expired instant must still reach Caps.Due, so RootContext expires " +
			"immediately and the record is honest about it")
	}
}

// TestTheDepthBackstopIsHostSettableAndZeroIsLegitimate covers the depth criterion.
//
// A BACKSTOP, NOT THE DESIGN (P2): recursion is meant to be bounded by verifier
// availability, and a run bounded by depth is under-verified rather than complete. Zero is
// a real request — solve the root, do not decompose — and is the degenerate case the suite
// and CI both exercise, so it must not be confused with "unset".
func TestTheDepthBackstopIsHostSettableAndZeroIsLegitimate(t *testing.T) {
	cfg, err := resolveRoot(hostFlags(t, []string{"--depth", "0"}), noEnv)
	if err != nil {
		t.Fatalf("--depth 0 is a real request: solve the root without decomposing: %v", err)
	}
	if cfg.Depth != 0 {
		t.Errorf("--depth 0 must survive as 0, not fall back to the default: got %d", cfg.Depth)
	}
	if _, err := resolveRoot(hostFlags(t, []string{"--depth", "-1"}), noEnv); err == nil {
		t.Error("a negative depth must be refused")
	}
	if _, err := resolveRoot(hostFlags(t, nil), env(map[string]string{envDepth: "notanumber"})); err == nil {
		t.Errorf("%s must be refused when it is not an integer", envDepth)
	}
}

// TestHostScopeTagsAreOpaqueAndReachEveryCacheKey is D4 and P6.
//
// quarry hashes scope tags and propagates them; it does not interpret them. The
// authoritative narrowing belongs elsewhere — for agate's path the real check is IAM's and
// quarry's local one is "a fast-fail courtesy, not the security boundary" — so this asserts
// PROPAGATION, which is quarry's half of the contract.
func TestHostScopeTagsAreOpaqueAndReachEveryCacheKey(t *testing.T) {
	cfg, err := resolveRoot(hostFlags(t, nil),
		env(map[string]string{envScope: "lab=example,project=hydrology"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Scope.Tags["project"]; got != "hydrology" {
		t.Fatalf("scope tags must survive the boundary, got %v", cfg.Scope.Tags)
	}

	// Scope-qualified cache keys, never the statement hash alone: two callers can pose a
	// hash-identical sub-problem while holding different entitlements, and one's cached
	// answer may derive from documents the other cannot see (P6).
	same := quarry.Problem{Statement: "what does it cost", Scope: cfg.Scope}
	other := quarry.Problem{Statement: "what does it cost", Scope: quarry.Scope{
		Tags: map[string]string{"lab": "example", "project": "geochem"},
	}}
	if same.Key() == other.Key() {
		t.Error("one statement under two scopes must not share a cache key (P6)")
	}
	// What makes that inequality meaningful rather than an artifact of two different
	// statements: the SAME statement under the SAME resolved scope keys identically, so the
	// only thing that moved above was the tag. Problem.Key is a hash, so the tag itself is
	// asserted one layer down, on the scope rendering that feeds it.
	if same.Key() != (quarry.Problem{Statement: "what does it cost", Scope: cfg.Scope}).Key() {
		t.Error("one statement under one scope must key identically, or the inequality above " +
			"says nothing about scope")
	}
	if !strings.Contains(cfg.Scope.Key(), "hydrology") {
		t.Errorf("the host's tag must reach the scope rendering that every cache key hashes, "+
			"got %q", cfg.Scope.Key())
	}

	// OPAQUE. A tag whose value looks like a number, a path or a nested structure is carried
	// verbatim — quarry interpreting one would be reproducing a host's or IAM's narrowing,
	// which D4's non-goals exclude.
	cfg, err = resolveRoot(hostFlags(t, []string{"--scope", "tier=3,path=/a/b,empty="}), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"tier": "3", "path": "/a/b", "empty": ""} {
		if got := cfg.Scope.Tags[k]; got != want {
			t.Errorf("tag %q must be carried verbatim: want %q, got %q", k, want, got)
		}
	}

	// A malformed tag list is refused rather than partially applied. A dropped tag WIDENS
	// the effective scope, which is the one direction P6 forbids.
	if _, err := resolveRoot(hostFlags(t, []string{"--scope", "notakeyvalue"}), noEnv); err == nil {
		t.Error("--scope must refuse a malformed list: silently dropping a tag widens scope (P6)")
	}
}

// TestHostModeRefusalReachesTheExitCode runs D3's refusal through the BINARY's entry point.
//
// The resolver's own test asserts the refusal; this one asserts a host can SEE it. A
// resolver that returns the right error into a runCmd that swallows it — or maps it to
// exit 1 — leaves a host escalating a forgotten flag as a quarry malfunction. That is the
// same defect `--cap 0` had before errUsage existed, which only running the binary found.
func TestHostModeRefusalReachesTheExitCode(t *testing.T) {
	err := runCmd(context.Background(), []string{"--fake", "--quiet", "--events-json", "anything"})
	if err == nil {
		t.Fatal("--events-json with no explicitly-set cap must be refused before anything runs")
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("a forgotten cap in host mode must exit %d, got %d (%v)", exitUsage, got, err)
	}
	// NOTHING RAN, so nothing was written. Asserted because "refused" and "refused after
	// spending" are different promises, and D3's whole point is that the money was never
	// authorised.
	if _, statErr := os.Stat("quarry-run-"); statErr == nil {
		t.Error("a refused run must not have written a record")
	}
}

// TestAHostMintedLedgerStillReplaysByteIdentically is the last acceptance criterion, and
// the one that would let this ship broken (P8).
//
// A cap minted in micro-units, a scope from the environment and a depth bound from a flag
// are all inputs to the tree's SHAPE: the cap decides what is affordable, the scope enters
// every cache key, the bound decides where recursion stops. If any of them failed to reach
// the record, a replay would re-execute a different algorithm and report the difference as
// a divergence in the record rather than as a defect in the CLI — which is exactly the
// class of defect RunBounds was added for.
func TestAHostMintedLedgerStillReplaysByteIdentically(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "host.json")

	// Host spelling throughout: integer micro-units, an absolute due date, scope from the
	// environment. --events-json is deliberately ON, so this run also travels the host-mode
	// path that D3 guards rather than an interactive one.
	t.Setenv(envScope, "lab=example,project=hydrology")
	// DEPTH COMES FROM THE ENVIRONMENT HERE, not from a flag, and that is deliberate. With
	// --depth set, cfg.Depth and the raw flag value coincide, so an executor wired from the
	// raw flag instead of the resolved config passes — the mutation that does exactly that
	// went undetected until this line moved. Only the environment path separates them.
	t.Setenv(envDepth, "2")
	stream := filepath.Join(dir, "s.ndjson")
	f, err := os.Create(stream)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = f
	err = runCmd(context.Background(), []string{
		"--fake", "--quiet", "--cap-micros", "250000",
		"--due", "2030-08-06T17:00:00Z", "--events-json", "--out", rec,
		"What does storage cost, how does it scale, and what dominates the bill?",
	})
	os.Stdout = saved
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatalf("a host-minted run must succeed: %v", err)
	}

	// The caps ARRIVED as minted. Read off the record rather than off cfg, because the
	// record is the citable artifact, and a correct resolver feeding a run that
	// recorded something else is the failure this catches.
	got, err := readRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Caps.Spend != quarry.Units(250_000) {
		t.Errorf("the record must carry the minted cap: want 250000 micro-units, got %d",
			got.Caps.Spend)
	}
	if got.Caps.Due.IsZero() {
		t.Error("the record must carry the absolute due date, or a replay cannot re-execute " +
			"under the same contract")
	}
	if got.Problem.Scope.Tags["project"] != "hydrology" {
		t.Errorf("the environment's scope must reach the record, got %v", got.Problem.Scope.Tags)
	}
	if got.Bounds.MaxDepth != 2 {
		t.Errorf("the host's depth bound must be RECORDED, not inferred: want 2, got %d",
			got.Bounds.MaxDepth)
	}
	// Non-vacuity: a single-node tree would satisfy every assertion above while exercising
	// none of the apportionment that makes replay bit-stability worth asserting.
	if len(got.Outcomes) < 2 {
		t.Fatalf("the run must have decomposed, got %d nodes", len(got.Outcomes))
	}

	// THE GUARANTEE. Replay through the same entry point a user would use, which re-derives
	// the tree from the record alone — no flags, no environment, no clock.
	jsonOutDir := filepath.Join(dir, "replayed.json")
	if err := replayCmd(context.Background(), []string{"--out", jsonOutDir, rec}); err != nil {
		t.Fatalf("a host-minted record must replay: %v", err)
	}
	replayed, err := readRecord(jsonOutDir)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RunID != got.RunID {
		t.Errorf("replay diverged: recorded %s, replayed %s — a cap or scope that did not "+
			"survive into the record makes the replay re-execute a different algorithm (P8)",
			got.RunID, replayed.RunID)
	}
	ob, err := got.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	rb, err := replayed.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(ob) != string(rb) {
		t.Error("the replayed record must be byte-identical, not merely equal in RunID")
	}
}
