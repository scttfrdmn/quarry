package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	quarry "github.com/scttfrdmn/quarry"
)

// The exit-code vocabulary is a CONTRACT TWO OTHER REPOS BRANCH ON (#9 D4), and these
// tests exist because a renumbering is a silent misread there rather than a build error.
// exitCode was extracted from main precisely so it is reachable from here — a switch of
// bare os.Exit calls would be untestable, and this table would then be pinned by nothing.

func TestExitCodesAreDistinguishable(t *testing.T) {
	// The requirement in #9's own words: the run outcomes must be DISTINGUISHABLE. Asserted
	// as pairwise distinctness rather than by restating the numbers, so the test says what
	// the contract needs instead of duplicating the constants — a test that hard-coded
	// `3` would pass a renumbering that swapped two meanings.
	codes := map[string]int{
		"complete":       exitComplete,
		"fault":          exitFault,
		"usage":          exitUsage,
		"time-truncated": exitTimeTruncated,
		"no-answer":      exitNoAnswer,
	}
	seen := map[int]string{}
	for name, c := range codes {
		if prev, dup := seen[c]; dup {
			t.Errorf("%q and %q share exit code %d, so a host cannot tell them apart", name, prev, c)
		}
		seen[c] = name
	}
	// Only success is zero. A non-zero "complete" would make every shell and CI step read a
	// finished run as a failure; a zero anything-else would make a host build on it.
	if exitComplete != 0 {
		t.Errorf("a complete run must exit 0, got %d", exitComplete)
	}
	for name, c := range codes {
		if name != "complete" && c == 0 {
			t.Errorf("%q must not exit 0: a host would treat it as success", name)
		}
	}
	// 1 and 2 keep their conventional meanings, which is why they were not renumbered when
	// the vocabulary grew — shells, CI and go tooling already read them that way.
	if exitFault != 1 || exitUsage != 2 {
		t.Errorf("fault must stay 1 and usage 2 (conventional), got %d and %d", exitFault, exitUsage)
	}
}

func TestExitCodeMapsTheSentinels(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"nil is complete", nil, exitComplete},
		{"no answer", errNoAnswer, exitNoAnswer},
		{"time truncated", errTimeTruncated, exitTimeTruncated},
		// WRAPPED, not bare. Both sentinels travel up through fmt.Errorf on some paths, and
		// a mapping that compared with == would silently degrade a known outcome to a fault
		// the first time a caller added context.
		{"wrapped no answer", fmt.Errorf("run: %w", errNoAnswer), exitNoAnswer},
		{"wrapped time truncated", fmt.Errorf("run: %w", errTimeTruncated), exitTimeTruncated},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("%s: want %d, got %d", tc.name, tc.want, got)
		}
	}
}

func TestARefusableFlagIsAUsageErrorNotAFault(t *testing.T) {
	// FOUND BY RUNNING THE BINARY, and invisible to the table test above because it was a
	// gap between the table and the CODE PATHS: `quarry run --cap 0` exited 1, so the
	// documented "2 usage error — bad flags, refused caps" was true of main's arg parsing
	// and false of every refusal that returned an error. A host would have escalated a
	// user's typo as a quarry malfunction.
	//
	// Sampled through capFlag rather than asserted on a hand-built error, so the test
	// exercises a real refusal site — a jointly-constructed usageError would pass while
	// the CLI still returned a bare fmt.Errorf.
	for _, bad := range []string{"0", "-1", "abc", "0.0"} {
		_, err := capFlag(bad)
		if err == nil {
			t.Errorf("--cap %q must be refused", bad)
			continue
		}
		if got := exitCode(err); got != exitUsage {
			t.Errorf("--cap %q: a refusable flag must exit %d, got %d", bad, exitUsage, got)
		}
	}
	// A valid cap and the unset case are NOT usage errors, or the classification would
	// swallow every run.
	for _, ok := range []string{"", "1.00", "0.0002"} {
		if _, err := capFlag(ok); err != nil {
			t.Errorf("--cap %q must be accepted, got %v", ok, err)
		}
	}
}

func TestAUsageErrorDoesNotPrintItsOwnClassification(t *testing.T) {
	// The FIRST FIX FOR THE ABOVE WAS ITSELF WRONG, which is why this is asserted
	// separately: errors.Join(msg, errUsage) classifies correctly and its Error() is
	// "…got 0\nusage", so the binary printed a bare "usage" line under the real message.
	// The classification must be invisible in the text and visible to errors.Is.
	err := usageErrf("--cap must be positive, got %s", "0")
	if got := err.Error(); got != "--cap must be positive, got 0" {
		t.Errorf("a usage error's message must be only the message; a host and a person both "+
			"read this line, got %q", got)
	}
	if !errors.Is(err, errUsage) {
		t.Error("the classification must still be findable by errors.Is")
	}
	// And a wrapped sentinel underneath survives, so adding the classification cannot break
	// a caller's existing errors.Is on the inner error.
	wrapped := usageError{fmt.Errorf("refused: %w", quarry.ErrPlanDoesNotFit)}
	if !errors.Is(wrapped, quarry.ErrPlanDoesNotFit) {
		t.Error("wrapping for classification must not hide the error's own sentinel")
	}
}

func TestAnUnrecognisedErrorIsAFaultNotASofterOutcome(t *testing.T) {
	// THE DEFAULT IS THE DECISION. A new failure path that this function has never seen
	// must read as a MALFUNCTION, because a host that believes a fault was an ordinary
	// outcome will build on a broken answer. Sampled across the shapes quarry's own
	// sentinels take, so the check is not just about one anonymous error.
	for _, err := range []error{
		errors.New("something nobody anticipated"),
		quarry.ErrCapExceeded,
		quarry.ErrPlanDoesNotFit,
		quarry.ErrScopeWidens,
		fmt.Errorf("wrapped: %w", errors.New("unknown")),
	} {
		if got := exitCode(err); got != exitFault {
			t.Errorf("%v: an unrecognised error must be a fault (%d), got %d", err, exitFault, got)
		}
	}
}

func TestStatusErrIsExhaustiveOverTheOutcomeVocabulary(t *testing.T) {
	// runCmd no longer reimplements Classify's precedence — it maps the outcome — so the
	// mapping has to cover the vocabulary. An outcome that fell through to nil would exit
	// 0, and exiting 0 is the one answer a host BUILDS ON.
	for out, want := range map[quarry.Outcome]int{
		quarry.OutcomeComplete:      exitComplete,
		quarry.OutcomeDegraded:      exitComplete, // the ruling: degradation is not a failure
		quarry.OutcomeTimeTruncated: exitTimeTruncated,
		quarry.OutcomeNoAnswer:      exitNoAnswer,
	} {
		if got := exitCode(statusErr(out)); got != want {
			t.Errorf("outcome %q must exit %d, got %d", out, want, got)
		}
	}
	// The unmapped case, which is the one this test exists for. A value Classify does not
	// return today but a later one might: it must be a FAULT, not a silent success.
	if got := exitCode(statusErr(quarry.Outcome("something-added-later"))); got != exitFault {
		t.Errorf("an unmapped outcome must be a fault (%d), got %d — a new outcome that "+
			"nobody mapped would otherwise be reported to a host as a finished run", exitFault, got)
	}
}

func TestTheExitCodeAgreesWithTheStreamOnEveryCorpusCase(t *testing.T) {
	// THE LOOP CLOSED. testdata/runevents/*.expected.json states an exit code per case,
	// HAND-WRITTEN from what the binary returned when the record was captured (#9). This
	// asserts the current mapping still produces those numbers from the outcome on the same
	// stream — so the corpus two other repos vendor cannot drift away from the binary that
	// is supposed to have produced it.
	//
	// It reads the FIXTURES rather than reclassifying the records, deliberately: the twins
	// branch on the pair (outcome, exit code) as they appear on disk, and a test that
	// recomputed both from a record would only prove quarry agrees with itself.
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "runevents", "*.expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 6 {
		t.Fatalf("expected the host corpus at testdata/runevents, found %d expectations", len(paths))
	}
	seen := map[int]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var exp quarry.StreamExpectation
		if err := json.Unmarshal(b, &exp); err != nil {
			t.Fatal(err)
		}
		if got := exitCode(statusErr(exp.Outcome)); got != exp.ExitCode {
			t.Errorf("%s: the stream says outcome %q but the corpus records exit %d, and the "+
				"binary now maps that outcome to %d.\n  the corpus is a CONTRACT: either this "+
				"mapping regressed or the fixture needs regenerating AND the twins need telling",
				filepath.Base(p), exp.Outcome, exp.ExitCode, got)
		}
		seen[exp.ExitCode] = true
	}
	// A corpus in which every case exited 0 would pass the loop above while pinning
	// nothing, which is the vacuity CLAUDE.md warns about one layer out.
	if len(seen) < 2 {
		t.Errorf("every corpus case has the same exit code %v, so this test cannot detect a "+
			"collapsed mapping", seen)
	}
}

func TestTheDegradedRunIsDeliberatelyNotAnErrorCode(t *testing.T) {
	// THE RULING, asserted where it could regress (#9 D4). Under the standing ruling only
	// TIME produces a gap; spend exhaustion is planned degradation INSIDE authority, so a
	// cap-bound run that produced an answer exits 0. A non-zero code here would make the
	// contract P4 promises look like a malfunction every time it worked.
	//
	// The record used is the SHAPE the live 30-node run has — five unfunded children, no
	// gaps, an answer at the root — because that is the common outcome under a real budget
	// rather than an edge case.
	rec := quarry.RunRecord{Outcomes: []quarry.NodeOutcome{
		{NodeID: "n0", Content: "a merged answer", Cost: 13911, Children: []string{"n0.0", "n0.1"}},
		{NodeID: "n0.0", Content: "a", Cost: 3509, Model: "m", ModelVersion: "m@1", Depth: 1},
		{NodeID: "n0.1", Depth: 1}, // unfunded: the cap could not reach it
	}}
	if got := rec.Classify(); got != quarry.OutcomeDegraded {
		t.Fatalf("fixture must be cap-bound degraded or this test is vacuous, got %q", got)
	}
	// runCmd returns nil for this shape — it checks only for an empty answer and for gaps —
	// so the code is exitComplete. Asserted through exitCode rather than by reading runCmd,
	// because the mapping is the part a host sees.
	if got := exitCode(nil); got != exitComplete {
		t.Errorf("cap-bound degradation with an answer must exit %d, got %d", exitComplete, got)
	}
	// And the host is not left blind: the terminal event names the degradation even though
	// the status does not. Without this the ruling would be indistinguishable from a bug.
	var reported bool
	for _, ev := range quarry.HostRunEvents(rec, "", nil) {
		if o, ok := ev.(quarry.OutcomeEvent); ok {
			reported = o.Outcome == quarry.OutcomeDegraded && o.Unfunded == 1 && o.Gaps == 0
		}
	}
	if !reported {
		t.Error("a degraded run exits 0, so the STREAM must say it degraded and by which " +
			"denomination — otherwise the ruling hides the fact instead of classifying it")
	}
}
