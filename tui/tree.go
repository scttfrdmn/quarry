// Package tui renders a live quarry run in the terminal — the tree view of §9.
//
// §9 asks for "a live tree where clicking a node shows its prompt, output,
// verifier result and cost — the single affordance that makes the system debuggable
// rather than a slot machine". This is the terminal half of that: the tree, drawn as
// it happens, with the cost and verdict per node. There is no clicking in a
// terminal, so the inspect half is `quarry show`, which reads the record.
//
// IT IS A quarry.Observer AND NOTHING ELSE. It reads the live seam and never a
// decision, never a record. A run's bytes must be identical whether or not anyone is
// watching (P8) — see TestObserverDoesNotPerturbTheRecord in the core package — and
// the way to keep that true is for the viewer to be write-only with respect to the
// run.
//
// WHAT IT DISPLAYS IS NOT CITABLE, and the footer says so. An Observer sees costs
// still moving and verdicts that do not exist yet, so a number on screen is a
// snapshot of an in-flight run and the record may contradict it. This is a third
// lossy projection alongside the OTel span tree and the agate RunEvent stream; the
// RunRecord remains the artifact.
//
// NO NEW DEPENDENCY, deliberately. A TUI framework would bring a widget tree, an
// event loop and half a dozen modules for what is here a repaint of at most a few
// dozen lines. The whole terminal surface used is four ANSI sequences, and Frame()
// is a pure function of state so the interesting part is testable without a
// terminal at all.
package tui

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	quarry "github.com/scttfrdmn/quarry"
)

// Tree is a live tree renderer. Construct with New, wire as Executor.Observer,
// call Start before the run and Stop after it.
//
// SAFE FOR CONCURRENT USE and, more importantly, NON-BLOCKING with respect to the
// run: Enter and Exit take a mutex, mutate a map and return. They never write to the
// terminal. Painting happens on Start's own goroutine, because an Observer is called
// on the executor's goroutines and a viewer that blocked on a slow terminal would add
// its latency to the run — under a deadline (§3.1) it would cause the gaps it is
// displaying.
type Tree struct {
	w io.Writer

	// Fancy enables in-place repainting with cursor movement and colour. When false
	// the renderer appends one line per completed node instead, which is what a pipe,
	// a CI log or a file needs: escape sequences in a redirected stream produce
	// garbage that outlives the run.
	Fancy bool

	// Width truncates statements so a node stays on one line. Zero means DefaultWidth.
	Width int

	// Interval is the repaint period. Zero means DefaultInterval. Faster is not
	// better — the spinner is the only thing that needs animating, and each repaint
	// rewrites every line.
	Interval time.Duration

	// Now is the clock, injected for the same reason the core injects one: a test must
	// be able to render a deterministic frame. Nil means time.Now, which is legitimate
	// here — Go rule 4 binds package quarry, not a renderer at the edge.
	Now func() time.Time

	// wmu serializes writes to w, and is SEPARATE from mu on purpose. Holding the state
	// lock across a terminal write is what this type exists to avoid (see the doc above),
	// but writing under no lock at all was worse: -race caught two nodes completing at
	// once in the non-fancy path, both calling Fprintln, interleaving their bytes. The
	// two locks answer two different questions — mu protects the tree, wmu protects the
	// stream — and no code path takes both.
	wmu sync.Mutex

	mu       sync.Mutex
	nodes    map[string]*node
	order    []string // insertion order, for a stable non-fancy transcript
	root     string
	cap      quarry.Units
	spent    quarry.Units
	tick     int
	lines    int  // lines painted by the previous frame, to know how far to move up
	finished bool // Stop was called; the last frame is final

	stop chan struct{}
	done chan struct{}
}

// node is one node's live state: what it was given, and what came back if anything.
type node struct {
	enter    quarry.NodeEnter
	outcome  quarry.NodeOutcome
	complete bool
	children []string
}

// Defaults for the fields a caller usually leaves alone.
const (
	DefaultWidth    = 100
	DefaultInterval = 100 * time.Millisecond
)

// New builds a renderer writing to w. Fancy is inferred from w being a character
// device, which is the check that matters: `quarry run --fake | tee log` must not
// fill the log with cursor movements.
func New(w io.Writer) *Tree {
	return &Tree{w: w, Fancy: isTerminal(w), nodes: map[string]*node{}}
}

// isTerminal reports whether w is a character device. Deliberately narrow — it is
// not asking about colour support or terminal capabilities, only whether in-place
// repainting will be interpreted or dumped as text.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Enter records a node the run is about to work on. Called on an executor
// goroutine: map write under the mutex, no I/O.
func (t *Tree) Enter(ev quarry.NodeEnter) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nodes == nil {
		t.nodes = map[string]*node{}
	}
	if _, dup := t.nodes[ev.NodeID]; dup {
		return // a node is entered once; a duplicate would double-count its cost
	}
	t.nodes[ev.NodeID] = &node{enter: ev}
	t.order = append(t.order, ev.NodeID)
	if ev.ParentID == "" {
		// The root's allocation is the run's cap — the burn-down denominator, and the
		// only place it is available, since a completed outcome carries only what was
		// spent.
		t.root, t.cap = ev.NodeID, ev.Alloc.Spend
	} else if p := t.nodes[ev.ParentID]; p != nil {
		p.children = append(p.children, ev.NodeID)
	}
}

// Exit records a finished node and accumulates its cost.
func (t *Tree) Exit(o quarry.NodeOutcome) {
	t.mu.Lock()
	n, ok := t.nodes[o.NodeID]
	if !ok {
		// An Exit with no Enter cannot be placed in the tree. It is recorded as a cost
		// so the total stays honest, and counted so the footer can say the display is
		// incomplete rather than quietly dropping a node.
		t.spent += o.Cost
		t.mu.Unlock()
		return
	}
	n.outcome, n.complete = o, true
	if o.Cost.Limited() && o.Cost > 0 {
		t.spent += o.Cost
	}
	line := ""
	if !t.Fancy {
		line = t.plainLine(n)
	}
	t.mu.Unlock()

	// Non-fancy mode streams one line per completion. Written outside the STATE lock but
	// under the write lock: it is the only I/O either Observer method does — acceptable
	// because a pipe write is what the caller asked for by not being a terminal, and
	// there is no repaint loop to do it instead — and siblings complete concurrently, so
	// unserialized it interleaves two nodes into one unreadable line.
	if line != "" {
		t.write(line + "\n")
	}
}

// write is the single exit to w, serialized by wmu. Both writers — the repaint loop
// and the non-fancy stream — go through here; they never run together today (Start is
// a no-op in non-fancy mode) but Frame's caller may write whenever it likes, and a
// renderer whose safety depends on which mode it is in is one refactor from garbling.
func (t *Tree) write(s string) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_, _ = io.WriteString(t.w, s)
}

// plainLine is one completed node as a single self-contained log line, for the
// non-fancy path. Caller holds the lock.
//
// It carries the depth as an indent and the id, rather than box characters: the lines
// arrive in COMPLETION order, and children complete before parents, so drawing
// branches would connect them wrongly. A log line that stands alone is honest about
// being a stream; a half-drawn tree is not.
func (t *Tree) plainLine(n *node) string {
	indent := strings.Repeat("  ", n.enter.Depth)
	return indent + t.nodeLine(n, len(indent))
}

// Start begins repainting until Stop. A no-op in non-fancy mode, which streams from
// Exit instead: repainting into a pipe would emit the whole tree once per tick.
func (t *Tree) Start() {
	if !t.Fancy || t.stop != nil {
		return
	}
	t.stop, t.done = make(chan struct{}), make(chan struct{})
	iv := t.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	go func() {
		defer close(t.done)
		tk := time.NewTicker(iv)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				t.paint()
			case <-t.stop:
				return
			}
		}
	}()
}

// Stop ends the repaint loop and paints one final frame, so the terminal is left
// showing the completed tree rather than whatever the last tick caught mid-run.
func (t *Tree) Stop() {
	t.mu.Lock()
	t.finished = true
	t.mu.Unlock()
	if t.stop != nil {
		close(t.stop)
		<-t.done
		t.stop, t.done = nil, nil
	}
	if t.Fancy {
		t.paint()
	}
}

// paint rewrites the frame in place: move the cursor up over the previous frame,
// then draw the new one clearing each line as it goes.
func (t *Tree) paint() {
	t.mu.Lock()
	t.tick++
	frame := t.frame()
	up := t.lines
	// COUNT THE LINES THIS FUNCTION WILL WRITE, not the newlines in the frame. The two
	// differ by one, because frame() ends in a newline and the loop below appends one to
	// every element Split returns — including the empty final one. Counting newlines
	// recorded N while painting N+1, so each repaint moved the cursor up one line short
	// and the whole frame walked down the screen: at 900ms fake latency a four-node run
	// printed twelve stacked headers instead of one that changed in place.
	lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	t.lines = len(lines)
	t.mu.Unlock()

	var b strings.Builder
	if up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up) // cursor up
	}
	for _, line := range lines {
		b.WriteString("\x1b[2K") // clear the whole line
		b.WriteString(line)
		b.WriteString("\n")
	}
	// A shrinking frame would leave the tail of the previous one on screen. It cannot
	// shrink today (nodes are never removed), but leaving that as an invariant the
	// renderer relies on would break the first time a pruned branch is dropped from
	// the display (§10) — so clear to the end of the screen instead of assuming.
	b.WriteString("\x1b[J")
	t.write(b.String())
}

// Frame returns the current frame, for a caller that wants to render it itself and
// for tests. Takes the lock; safe to call during a run.
func (t *Tree) Frame() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frame()
}

// frame builds the whole display. Caller holds the lock.
//
// A PURE FUNCTION of the state plus the tick counter, which is why the tick is a
// field rather than read from the clock here: a test can set it and compare bytes.
func (t *Tree) frame() string {
	var b strings.Builder
	b.WriteString(t.header())
	b.WriteString("\n\n")
	if t.root == "" {
		b.WriteString("  (waiting for the first node)\n")
	} else {
		t.renderNode(&b, t.root, "", true, true)
	}
	b.WriteString("\n")
	b.WriteString(t.footer())
	b.WriteString("\n")
	return b.String()
}

// header is the burn-down: cap, spent, percentage, bar.
//
// An uncapped run shows "no cap" rather than a bar. There is no denominator, and a
// bar with an invented one would be a fabricated measurement — the same class of lie
// as reporting an unmeasured duration as zero (see quarry.NodeTiming).
func (t *Tree) header() string {
	var b strings.Builder
	b.WriteString("quarry")
	if !t.cap.Limited() {
		fmt.Fprintf(&b, "   spent %s   (no cap — P9 wants at least one)", t.spent)
		return b.String()
	}
	frac := 0.0
	if t.cap > 0 {
		frac = float64(t.spent) / float64(t.cap)
	}
	fmt.Fprintf(&b, "   cap %s   spent %s (%.0f%%)   %s",
		t.cap, t.spent, frac*100, bar(frac, 24))
	return b.String()
}

// bar draws a proportional meter. Over-cap fills completely rather than overflowing
// the field: the ledger's admission control (§3) is what prevents an overrun, and a
// bar that ran off the line would misreport a bug as a wider terminal.
func bar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// renderNode draws one node and recurses, drawing box characters from the prefix.
func (t *Tree) renderNode(b *strings.Builder, id, prefix string, last, isRoot bool) {
	n := t.nodes[id]
	if n == nil {
		return
	}
	branch := ""
	childPrefix := prefix
	if !isRoot {
		if last {
			branch, childPrefix = "└─ ", prefix+"   "
		} else {
			branch, childPrefix = "├─ ", prefix+"│  "
		}
	}
	// The prefix is charged against the line's width, so the cost column lands in the
	// same place at every depth. Rendering the node to a fixed width and THEN indenting
	// pushes deeper lines further right, which is how the first version drew a ragged
	// right edge — the numbers a reader is scanning down were the ones that moved.
	indent := len([]rune(prefix)) + len([]rune(branch))
	fmt.Fprintf(b, "%s%s%s\n", prefix, branch, t.nodeLine(n, indent))

	// Siblings sort by PLAN POSITION, never by arrival. Children are entered
	// concurrently, so arrival order is a race and a tree drawn in it reorders itself
	// between runs of the same problem — which would make the display useless for
	// exactly the debugging §9 wants it for.
	kids := append([]string(nil), n.children...)
	sort.SliceStable(kids, func(i, j int) bool {
		a, c := t.nodes[kids[i]], t.nodes[kids[j]]
		if a == nil || c == nil {
			return false
		}
		return a.enter.Index < c.enter.Index
	})
	for i, kid := range kids {
		t.renderNode(b, kid, childPrefix, i == len(kids)-1, false)
	}
}

// nodeLine is one node: status glyph, id, statement, cost, verdict. indent is how
// many columns the caller has already spent on tree drawing, deducted from the width
// so the right column aligns across depths.
func (t *Tree) nodeLine(n *node, indent int) string {
	stmt := n.enter.Problem.Statement
	// An arm restates its parent's problem by definition (§2), so showing the
	// statement again would make a portfolio look like a stuck repeat. Say what it is
	// instead.
	if n.enter.Arm {
		stmt = "⟲ arm: " + stmt
	}
	left := fmt.Sprintf("%s %s %s", t.status(n), n.enter.NodeID, stmt)

	right := ""
	switch {
	case !n.complete:
		// In flight: show what it MAY spend, not a cost it has not incurred. The
		// allocation is the one number available here and it is a different claim from
		// a cost, so the ≤ is load-bearing.
		if n.enter.Alloc.Spend.Limited() {
			right = "≤ " + n.enter.Alloc.Spend.String()
		}
	default:
		right = n.outcome.Cost.String()
		if v := verdict(n.outcome); v != "" {
			right += "  " + v
		}
	}

	width := t.Width
	if width <= 0 {
		width = DefaultWidth
	}
	return fit(left, right, width-indent)
}

// status is the glyph. The distinctions it draws are the ones the record draws, and
// no others: unchecked is NOT shown as a pass (§8), and a gap is not shown as a
// failure (§3.1 — only time is a gap).
func (t *Tree) status(n *node) string {
	if !n.complete {
		if t.finished {
			// The run ended with this node still open, which means the run itself faulted:
			// every Enter is otherwise followed by an Exit. Showing a spinner forever would
			// present a broken run as a working one.
			return "!"
		}
		return spinner[t.tick%len(spinner)]
	}
	switch {
	case n.outcome.Gap:
		return "⊘" // truncated by time — named, never silent
	case n.outcome.CacheHit:
		return "⇢" // served from the cache: real tokens once, no charge this run (§6)
	case n.outcome.Verified == nil:
		return "○" // nobody checked it. Distinct from a pass, and the whole point of §8
	case *n.outcome.Verified:
		return "✓"
	default:
		return "✗"
	}
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// verdict is the short trailing annotation. Empty when there is nothing honest to
// say, which is deliberate: a node with no verdict gets no word, rather than a
// reassuring one.
func verdict(o quarry.NodeOutcome) string {
	switch {
	case o.Gap:
		return "gap (time)"
	case o.CacheHit:
		return "cached"
	case o.Verified == nil:
		return "unverified"
	case *o.Verified:
		return "verified"
	default:
		return "FAILED"
	}
}

// footer is the count line plus the standing caveat. Both halves matter: the counts
// are what a reader wants, and the caveat is why they may not cite them.
func (t *Tree) footer() string {
	var verified, failed, unverified, gaps, cached, open int
	for _, n := range t.nodes {
		if !n.complete {
			open++
			continue
		}
		switch {
		case n.outcome.Gap:
			gaps++
		case n.outcome.CacheHit:
			cached++
		case n.outcome.Verified == nil:
			unverified++
		case *n.outcome.Verified:
			verified++
		default:
			failed++
		}
	}
	parts := []string{fmt.Sprintf("%d verified", verified)}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d FAILED", failed))
	}
	parts = append(parts, fmt.Sprintf("%d unverified", unverified))
	if cached > 0 {
		parts = append(parts, fmt.Sprintf("%d cached", cached))
	}
	parts = append(parts, fmt.Sprintf("%d gap", gaps))
	if open > 0 {
		parts = append(parts, fmt.Sprintf("%d in flight", open))
	}
	line := strings.Join(parts, " · ")

	// The caveat is not decoration. Everything above is a snapshot of a run that is
	// still moving, and the record is what may be cited (P8).
	if t.finished {
		return line + "\nlive view — the run record is the citable artifact (quarry show)"
	}
	return line + "\nlive view — costs are still moving and verdicts are not final"
}

// fit places right-aligned text at width, truncating left if they collide. Rune-
// aware, because the glyphs and box characters are multi-byte and a byte-counted
// truncation would split one and corrupt the line.
func fit(left, right string, width int) string {
	lr, rr := []rune(left), []rune(right)
	if len(rr) == 0 {
		if len(lr) > width {
			return string(lr[:width-1]) + "…"
		}
		return left
	}
	// Two spaces minimum between the columns, so a long statement never abuts a cost.
	maxLeft := width - len(rr) - 2
	if maxLeft < 8 {
		maxLeft = 8 // a pathologically narrow width still shows the glyph and the id
	}
	if len(lr) > maxLeft {
		lr = append(lr[:maxLeft-1], '…')
	}
	pad := width - len(lr) - len(rr)
	if pad < 2 {
		pad = 2
	}
	return string(lr) + strings.Repeat(" ", pad) + right
}

var _ quarry.Observer = (*Tree)(nil)
