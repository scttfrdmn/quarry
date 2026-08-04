package main

import (
	"fmt"
	"io"
)

// printer is the CLI's writer: an io.Writer that remembers the first write error so the
// printers can stay unbroken by error handling.
//
// WHY A TYPE RATHER THAN `_ =` ON EVERY CALL. `summarize` and `showRecord` are ~90
// consecutive Fprintf lines whose whole job is to lay out a receipt. Checking each
// one individually would triple their length and bury the layout — which is the part
// a reader is here to read — and the standard alternative, discarding every error with
// `_ =`, is worse than either: it silences the one case that matters along with the 89
// that do not. A CLI writing a record to a closed pipe (`quarry show rec | head -5`)
// should exit non-zero rather than report success on output nobody received.
//
// THIS ALSO MAKES THE PRINTERS TESTABLE. They previously took *os.File — concretely,
// not as an interface — so no test could capture their output, which is why every
// display function in show.go is untested. A printer wraps a bytes.Buffer just as
// happily as a terminal.
//
// Deliberately NOT a general error-accumulating writer: it records the FIRST error and
// keeps writing. A layout is not transactional, half a receipt is better than none,
// and stopping at the first short write would make a truncated pipe produce different
// output than a full one — a difference a reader would have to diagnose.
type printer struct {
	w   io.Writer
	err error
}

func newPrinter(w io.Writer) *printer { return &printer{w: w} }

// Write satisfies io.Writer, so a *printer can be passed to anything taking one —
// including the record tree renderers and json.Encoder.
func (o *printer) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	if err != nil && o.err == nil {
		o.err = err
	}
	return n, err
}

// The two printers. Each discards the returned error at exactly one place in the
// program, because Write above has already recorded it — this is the whole reason the
// type exists, and the only spot where discarding is correct rather than lazy.
//
// No unformatted print(): nothing needed one, and an unused third method is a method a
// reader has to work out the purpose of.
func (o *printer) printf(format string, a ...any) { _, _ = fmt.Fprintf(o, format, a...) }
func (o *printer) println(a ...any)               { _, _ = fmt.Fprintln(o, a...) }

// Err returns the first write error, for a caller that wants to exit non-zero on one.
func (o *printer) Err() error { return o.err }

var _ io.Writer = (*printer)(nil)
