// Command quarry runs a problem and inspects what came back.
//
// This is quarry's own interface — for testing, for demonstrating, and for using it.
// Before it existed, the only entry point was a Go test: every fake lived in a
// _test.go file, unreachable from a binary, so seeing quarry work at all required AWS
// credentials and real money. The cheapest way to understand the system was also the
// most expensive thing available.
//
// Three verbs, matching the two halves of §9:
//
//	quarry run     execute a problem under a cap, drawing the live tree
//	quarry show    read a saved record — the inspect half
//	quarry replay  re-execute a record and prove it reproduces byte-for-byte (P8)
//
// --fake needs no credentials, spends no money and calls no model. It demonstrates
// SHAPE, COST and PROVENANCE, which is what quarry is about, and demonstrates
// nothing at all about answer quality — the content is a hash.
//
// THE RECORD IS THE DELIVERABLE, and the CLI is arranged to say so. `run` writes a
// record and prints where it went; the prose answer is one field in it (§8, P8). What
// the live tree showed is a third lossy projection and is not citable.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// The exit-code vocabulary a supervising host branches on (#9 D4).
//
// A BOOLEAN STATUS IS NOT ENOUGH, and the reason is not tidiness. "finished", "ran out
// of time", "ran out of money" and "crashed" call for four different next moves, and a
// host choosing automatically — with no researcher reading the summary — will offer a
// deadline raise where money was needed unless the status tells it apart. That is the
// exact §3.1 mislabelling quarry.ErrRecordedUnfunded exists to prevent, one layer out.
//
// THE NUMBERS ARE PART OF THE CONTRACT and may not be reshuffled: two hosts in two
// languages branch on them, and a renumbering is a silent misread rather than a build
// error. Documented in docs/integration-requirements.md §6.
//
//	0  complete             finished inside its caps, with an answer
//	1  fault                crash, provider error, unreadable record — a MALFUNCTION
//	2  usage error          bad flags, refused caps; nothing ran
//	3  time-truncated       a deadline cut it short; the record has gaps (§3.1)
//	4  no answer            nothing was affordable or every node came back empty
//
// 1 AND 2 KEEP THEIR CONVENTIONAL MEANINGS because they were already load-bearing: `go
// test`-style tooling, shells and CI all read 1 as failure and 2 as misuse, and the
// pre-existing binary used exactly those. Only the new codes are new.
//
// CAP-BOUND DEGRADATION IS NOT IN THIS TABLE, and that is the ruling rather than an
// omission. Under the standing ruling only TIME produces a gap; spend exhaustion is
// planned degradation INSIDE authority, so a degraded run that produced an answer exits
// 0 — the cap did what P4 promises, and a non-zero status would make the contract look
// like a malfunction. A host that wants to know anyway reads bound_by off the outcome
// event, which is why that event carries it.
//
// A run that produced NO answer exits 4 even though the cause is usually spend, because
// a host has nothing to render either way. Not 1: the record is faithful and citable —
// it accurately records that nothing was affordable — so it is an outcome, not a fault.
const (
	exitComplete      = 0
	exitFault         = 1
	exitUsage         = 2
	exitTimeTruncated = 3
	exitNoAnswer      = 4
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	// Signals cancel the run's context rather than killing the process. §3.1's whole
	// argument is that you can stop spending but cannot stop time: on cancellation the
	// tree holds a returnable answer, so ^C must yield a partial record with its gaps
	// named — not nothing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(ctx, os.Args[2:])
	case "show":
		err = showCmd(os.Args[2:])
	case "replay":
		err = replayCmd(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "quarry: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}
	if err != nil {
		// errNoAnswer and errTimeTruncated are already fully explained by the summary the
		// run just printed — re-stating either as "quarry: no answer" would bury the reason
		// under a restatement. For those two the exit code IS the whole message.
		if !errors.Is(err, errNoAnswer) && !errors.Is(err, errTimeTruncated) {
			fmt.Fprintf(os.Stderr, "quarry: %v\n", err)
		}
		os.Exit(exitCode(err))
	}
}

// exitCode maps an error to the vocabulary above (#9 D4).
//
// A FUNCTION SO THE MAPPING IS TESTABLE, which is the whole point: it is a contract two
// other repos branch on, and a table that can only be exercised by spawning a process is
// a table nothing asserts. Bare os.Exit calls in a switch here would be unreachable from
// a test.
//
// The DEFAULT IS exitFault, deliberately. An error this function does not recognise is a
// malfunction until something says otherwise: mapping the unknown case to a softer code
// would let a new failure path be reported to a host as an ordinary outcome, and a host
// that believes a fault was an outcome will happily build on a broken answer.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitComplete
	// errTimeTruncated before errNoAnswer would be WRONG, and the order here mirrors
	// RunRecord.Classify for exactly that reason: a run with no answer at all is usually
	// also time-truncated, and telling a host to extend it points at nothing to extend.
	// The two orderings must agree, or `quarry run`'s status contradicts its own stream.
	case errors.Is(err, errNoAnswer):
		return exitNoAnswer
	case errors.Is(err, errTimeTruncated):
		return exitTimeTruncated
	// FOUND BY RUNNING THE BINARY against the table above. `quarry run --cap 0` — a plain
	// flag mistake, nothing ran — exited 1, so the code above documented 2 for "bad flags,
	// refused caps" while the binary reported a MALFUNCTION. A host would have escalated a
	// user's typo as a quarry fault. Only os.Exit(exitUsage) on the arg-parsing paths in
	// main was ever reached; every refusal returned as an error and fell through to the
	// default. errUsage is what the return paths mark themselves with.
	case errors.Is(err, errUsage):
		return exitUsage
	}
	return exitFault
}

// errUsage marks a refusal the CALLER can fix — a bad flag, an unparseable cap, a
// missing statement. Nothing ran, so there is no record and no outcome to classify.
//
// A SENTINEL RATHER THAN A SEPARATE RETURN CHANNEL because these sites already return
// error and are spread across three commands; threading a second value through all of
// them to carry two bits would be a larger change than the distinction is worth. The
// message still prints, unlike the two outcome sentinels — a user who mistyped a flag
// needs to be told which one.
var errUsage = errors.New("usage")

// usageErrf builds a usage error: the message, marked so exitCode maps it to 2.
//
// A WRAPPER TYPE RATHER THAN errors.Join OR A %w OF errUsage, and the first draft used
// Join and was wrong in a way only running it showed: Join's Error() concatenates its
// operands with a newline, so `quarry run --cap 0` printed
//
//	quarry: --cap must be positive, got 0
//	usage
//
// with "usage" as a bare second line — a classification leaking into the text a person
// reads. %w-wrapping errUsage has the same defect one word further along. errors.Is
// still finds the sentinel through Unwrap; only Error() differs, which is the point.
func usageErrf(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

// usageError classifies without contributing to the message.
type usageError struct{ err error }

func (u usageError) Error() string { return u.err.Error() }

// Unwrap returns BOTH, so errors.Is finds errUsage and any sentinel the wrapped error
// itself carries — capFlag's messages wrap %w, and losing that would make a caller's
// errors.Is on the inner error stop working the moment the outer classification was added.
func (u usageError) Unwrap() []error { return []error{u.err, errUsage} }

func usage() {
	fmt.Fprint(os.Stderr, `quarry — bounded recursive decomposition with verified provenance

usage:
  quarry run [flags] "<problem statement>"
  quarry show [flags] <record.json>
  quarry replay [flags] <record.json>

  run     execute a problem under a cap, drawing the live tree (§9)
  show    read a saved record: cost receipt, trust summary, what was NOT checked
  replay  re-execute a record against its own responses; must reproduce exactly (P8)

Run "quarry <command> -h" for flags.

  quarry run --fake --cap 1.00 "What does X cost, how does it scale, and what dominates?"

--fake needs no credentials and spends nothing. It demonstrates shape, cost and
provenance; the answers are synthetic and mean nothing.
`)
}

// ---------------------------------------------------------------- shared plumbing

// capFlag parses a decimal cap into Units. A separate helper because "" and "0" are
// different answers: unset means the caller did not choose, and P9 requires a run to
// carry at least one real cap, so an unset spend cap is only acceptable alongside a
// deadline.
func capFlag(s string) (quarry.Units, error) {
	if strings.TrimSpace(s) == "" {
		return quarry.Unlimited, nil
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, usageErrf("--cap %q is not a number", s)
	}
	if f <= 0 {
		return 0, usageErrf("--cap must be positive, got %s", s)
	}
	return quarry.FromFloat(f), nil
}

// explain turns a sentinel into an actionable message. These three errors are
// design-level refusals rather than faults, and reporting them as bare wrapped text
// tells a user what failed without telling them what to do.
func explain(err error) error {
	switch {
	case errors.Is(err, quarry.ErrPlanDoesNotFit):
		return fmt.Errorf("%w\n  the planner proposed a split the cap cannot fund above the floor (P9).\n"+
			"  raise --cap, or lower --floor to allow smaller children", err)
	case errors.Is(err, quarry.ErrCapExceeded):
		return fmt.Errorf("%w\n  the cap is the contract (P4): the run stopped rather than overspend", err)
	case errors.Is(err, quarry.ErrScopeWidens):
		return fmt.Errorf("%w\n  a child was given authority its parent did not hold (P6). this is a bug, "+
			"not a configuration problem", err)
	}
	return err
}

// deadlineNote reports whether a context ended because time ran out, so the summary
// can name a truncated run rather than presenting a partial answer as a whole one.
func deadlineNote(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "the deadline expired: this is a partial answer and its gaps are named in the record (§3.1)"
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "interrupted: this is a partial answer and its gaps are named in the record (§3.1)"
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// durStr renders a duration for display, or "not measured" — the same absence-not-
// zero discipline as quarry.NodeTiming. A run reported as "0s" is a lie about a
// measurement that was never taken.
func durStr(d time.Duration, ok bool) string {
	if !ok {
		return "not measured"
	}
	return d.Round(time.Millisecond).String()
}
