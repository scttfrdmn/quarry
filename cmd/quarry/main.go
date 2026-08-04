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

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
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
		os.Exit(2)
	}
	if err != nil {
		// errNoAnswer is already fully explained by the summary the run just printed —
		// re-stating it as "quarry: no answer" would bury the reason under a restatement.
		// The exit code is the whole message.
		if !errors.Is(err, errNoAnswer) {
			fmt.Fprintf(os.Stderr, "quarry: %v\n", err)
		}
		os.Exit(1)
	}
}

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
		return 0, fmt.Errorf("--cap %q is not a number", s)
	}
	if f <= 0 {
		return 0, fmt.Errorf("--cap must be positive, got %s", s)
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
