package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestLiveEventsWithoutTheFramedStreamIsRefusedAsUsage(t *testing.T) {
	// #14 D2's refusal, asserted through the BINARY's own entry point rather than by
	// re-deriving the condition. --live-events alone would have to put node events on
	// stdout, where the human summary lives when --events-json is off, so a host would get
	// a stream with prose in the middle — the exact defect #9 D1 exists to prevent.
	//
	// USAGE and not a fault: nothing ran, so there is no record to classify, and exiting 1
	// would escalate a flag combination a person can fix to a host as a quarry malfunction.
	err := runCmd(context.Background(), []string{"--fake", "--live-events", "--cap", "0.25", "anything"})
	if err == nil {
		t.Fatal("--live-events without --events-json must be refused: the events would land " +
			"on the human's stdout")
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("a refusable flag combination must exit %d, got %d (%v)", exitUsage, got, err)
	}
	// The message must name the OTHER flag. A refusal that says only "invalid combination"
	// leaves a person guessing which of the two to change.
	if !strings.Contains(err.Error(), "--events-json") {
		t.Errorf("the refusal must name the flag that fixes it, got %q", err.Error())
	}
	// NON-VACUITY: the combination this test refuses must be the ONLY thing refused, or the
	// check would pass against a binary that rejected --fake, --cap or the statement. The
	// accepted form is run below, which also proves the refusal came from the PAIR and not
	// from the parse.
}

func TestTheLiveStreamHasOneFrameAndPrecedesTheFold(t *testing.T) {
	// THE ORDERING IS THE WHOLE VALUE of putting both on one fd (#14 D2), so it is asserted
	// on the bytes a host actually receives rather than on the two fold functions in
	// isolation: a node's live entry must be readable as PRECEDING the fold that summarises
	// it, and there must be EXACTLY ONE version frame, first.
	//
	// Reachable from Go only because os.Stdout can be swapped — run.go writes the stream to
	// os.Stdout by name, since owning that fd is the contract. The redirect is also what
	// keeps a real run's NDJSON out of the test log.
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream.ndjson")
	f, err := os.Create(stream)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = f
	err = runCmd(context.Background(), []string{
		"--fake", "--quiet", "--cap", "0.25", "--depth", "2",
		"--events-json", "--live-events", "--out", filepath.Join(dir, "r.json"),
		"What does storage cost, how does it scale, and what dominates the bill?",
	})
	os.Stdout = saved
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatalf("--live-events WITH --events-json must be accepted, got %v", err)
	}

	raw, err := os.ReadFile(stream)
	if err != nil {
		t.Fatal(err)
	}
	// Parsed the way a HOST does — untyped, tolerating kinds it does not know — because
	// decoding into quarry's own closed union is what would hide an unparseable line.
	evs, err := quarry.ReadStreamEvents(raw)
	if err != nil {
		t.Fatalf("the interleaved stream must be readable as NDJSON: %v", err)
	}

	var frames, enters, exits int
	firstEnter, lastEnter, foldStart := -1, -1, -1
	for i, ev := range evs {
		switch ev["type"] {
		case "quarry_stream":
			frames++
			if i != 0 {
				t.Errorf("the version frame must be event 0, found one at %d: a host must be "+
					"able to refuse a stream before it parses anything", i)
			}
		case "quarry_node_enter":
			enters++
			if firstEnter < 0 {
				firstEnter = i
			}
			lastEnter = i
		case "quarry_node_exit":
			exits++
		default:
			// The folded stream's own kinds — agate's union plus the terminal outcome.
			if foldStart < 0 {
				foldStart = i
			}
		}
	}

	if frames != 1 {
		t.Errorf("exactly one version frame per stream, got %d: two contract declarations "+
			"read as two concatenated streams", frames)
	}
	if enters == 0 || exits == 0 {
		t.Fatalf("the live half must be present or this test pins nothing: %d enters, %d exits", enters, exits)
	}
	if enters != exits {
		t.Errorf("%d enters and %d exits: an unpaired enter shows a node as permanently in flight",
			enters, exits)
	}
	// A multi-node run, or the tree shape this format exists to carry is untested.
	if enters < 2 {
		t.Errorf("the fixture must decompose (got %d nodes) or the parent/child fields carry "+
			"nothing", enters)
	}
	if foldStart < 0 {
		t.Fatal("the fold must still be there: the live events ADD to the stream, they do not replace it")
	}
	// THE ORDERING. Every live event precedes the fold, which is what makes one stream worth
	// more than two: a host reading in order sees the run happen and then sees it summarised.
	if lastEnter > foldStart {
		t.Errorf("a live event at %d lands after the fold began at %d — the interleaving is "+
			"backwards, and a host cannot read the entry as preceding its summary",
			lastEnter, foldStart)
	}
	// The terminal outcome still closes the stream, scanned backwards rather than read off
	// the last line, per the frozen rule.
	if _, ok := quarry.TerminalOutcome(evs); !ok {
		t.Error("the stream must still close with a terminal outcome; its absence is how a " +
			"host detects a crash, so it must not be spent on ordinary live output")
	}
}
