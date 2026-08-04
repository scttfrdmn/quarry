package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	quarry "github.com/scttfrdmn/quarry"
)

// errWriter fails after letting n bytes through, so a test can put the failure in the
// middle of a receipt rather than at the start — the case where "keep writing after the
// first error" and "stop at the first error" behave differently.
type errWriter struct {
	allow int
	err   error
	n     int
}

func (w *errWriter) Write(p []byte) (int, error) {
	if w.n >= w.allow {
		return 0, w.err
	}
	w.n += len(p)
	return len(p), nil
}

func TestPrinterRemembersTheFirstWriteErrorAndKeepsGoing(t *testing.T) {
	// THE REASON THIS TYPE EXISTS. `summarize` and `showRecord` are ~90 consecutive
	// print calls; checking each one inline would bury the layout, and discarding every
	// error with `_ =` would silence the one that matters — a CLI writing a record into
	// a closed pipe must not exit 0 on output nobody received.
	boom := errors.New("pipe closed")
	w := newPrinter(&errWriter{allow: 10, err: boom})

	w.printf("first line is short\n")    // gets through
	w.printf("second line fails\n")      // fails
	w.printf("third line fails too\n")   // fails, and must not overwrite the first error
	w.println("and the layout finishes") // still called: a layout is not transactional

	if !errors.Is(w.Err(), boom) {
		t.Fatalf("Err() must report the first write error, got %v", w.Err())
	}
}

func TestPrinterErrIsNilWhenEveryWriteSucceeded(t *testing.T) {
	// Non-vacuity for the test above: the error path is only meaningful if the happy
	// path is genuinely clean, and an accumulating writer that reported a phantom error
	// would make every CLI command exit non-zero.
	w := newPrinter(&bytes.Buffer{})
	w.printf("a %d\n", 1)
	w.println("b")
	if w.Err() != nil {
		t.Fatalf("a successful run must report no write error, got %v", w.Err())
	}
}

// TestTheDisplayFunctionsWriteToAnyWriter is what the *os.File signatures prevented.
//
// Every display function in show.go was previously untestable: they took *os.File
// concretely, so nothing could capture their output and no test asserted that a record
// renders at all. This is the assertion that was unavailable — and it is a real one,
// since `show` is how a run is read (§9) and a receipt that omits the cost or the
// unchecked list would still have passed every other test in this package.
func TestTheDisplayFunctionsWriteToAnyWriter(t *testing.T) {
	verified := true
	rec := quarry.RunRecord{
		RunID:   strings.Repeat("a", 64),
		Problem: quarry.Problem{Statement: "does it render"},
		Caps:    quarry.Caps{Spend: quarry.FromFloat(1)},
		Mode:    quarry.ModeFresh,
		// An unchecked node alongside a verified one: the two must be displayed
		// differently, which is the §8 distinction the whole record exists to keep.
		Unverified: []string{"n0"},
		Outcomes: []quarry.NodeOutcome{
			{NodeID: "n0", Problem: quarry.Problem{Statement: "the root"},
				Children: []string{"n0.0"}},
			{NodeID: "n0.0", Depth: 1, Problem: quarry.Problem{Statement: "a child"},
				Cost: quarry.FromFloat(0.25), Verified: &verified},
		},
	}

	var buf bytes.Buffer
	w := newPrinter(&buf)
	showRecord(w, rec, 100)
	if err := w.Err(); err != nil {
		t.Fatalf("writing to a buffer must not error: %v", err)
	}
	got := buf.String()

	// The glyphs, not just the text: an unchecked node showing a tick would be the one
	// display defect that actively misleads a reader (§8).
	for _, want := range []string{
		"does it render",       // the problem
		"0.2500 of 1.0000",     // the cost receipt
		"unchecked 1 node: n0", // what was NOT checked
		"needs replicates",     // stability absent, not zero (P7)
		"○ n0",                 // unchecked glyph on the root
		"✓ n0.0",               // verified glyph on the child
		"└─",                   // the tree was actually drawn
	} {
		if !strings.Contains(got, want) {
			t.Errorf("showRecord output is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "✓ n0 ") {
		t.Errorf("the root is unchecked and must not render as verified:\n%s", got)
	}
}

func TestShowNodeReportsAMissingNodeRatherThanPrintingAnEmptyOne(t *testing.T) {
	rec := quarry.RunRecord{
		RunID:    strings.Repeat("b", 64),
		Outcomes: []quarry.NodeOutcome{{NodeID: "n0"}, {NodeID: "n0.1"}},
	}
	err := showNode(newPrinter(&bytes.Buffer{}), rec, "n0.9")
	if err == nil {
		t.Fatal("asking for a node the record does not have must be an error")
	}
	// The available ids belong in the message: a bare "not found" leaves the reader
	// guessing at an id encoding the record already knows.
	if !strings.Contains(err.Error(), "n0.1") {
		t.Errorf("the error should name the ids the record does have, got: %v", err)
	}
}

func TestJSONOutIsIndentedAndDoesNotEscapeHTML(t *testing.T) {
	// Explicitly NOT the canonical hashed form — that is the file itself. This output is
	// for human reading, so indentation is the point, and escaping `&` into `\u0026`
	// would corrupt a statement a reader is trying to read.
	var buf bytes.Buffer
	if err := jsonOut(&buf, map[string]string{"q": "cost & scale"}); err != nil {
		t.Fatalf("jsonOut: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "cost & scale") {
		t.Errorf("HTML escaping must be off, got %s", got)
	}
	if !strings.Contains(got, "\n  ") {
		t.Errorf("output must be indented for reading, got %s", got)
	}
}
