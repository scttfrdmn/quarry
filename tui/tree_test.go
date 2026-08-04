package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	quarry "github.com/scttfrdmn/quarry"
	"github.com/scttfrdmn/quarry/provider"
)

// The renderer's invariants are all of one kind: THE DISPLAY MUST NOT CLAIM MORE
// THAN THE RECORD DOES. The distinctions §8 spends its effort preserving — checked
// versus unchecked, unaffordable versus truncated, spent versus allowed — are all
// one careless glyph away from being flattened here, at the last hop, where nobody
// thinks a claim is being made.

func enter(id, parent string, depth, index int, stmt string, alloc quarry.Units) quarry.NodeEnter {
	return quarry.NodeEnter{
		NodeID: id, ParentID: parent, Depth: depth, Index: index,
		Problem: quarry.Problem{Statement: stmt},
		Alloc:   quarry.Allocation{Spend: alloc},
	}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// fixed builds a renderer over a buffer with a deterministic frame.
func fixed() (*Tree, *bytes.Buffer) {
	var buf bytes.Buffer
	t := New(&buf)
	t.Fancy = false // a bytes.Buffer is not a terminal; state it rather than infer
	return t, &buf
}

func TestAnUncheckedNodeIsNotDisplayedAsAPass(t *testing.T) {
	// THE ONE THAT MATTERS MOST. §8 requires a record to say what was NOT checked, and
	// a tick beside an unverified node destroys that distinction at the last hop —
	// after the executor, the outcome and the record have all preserved it.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Cost: quarry.FromFloat(1)}) // Verified nil

	f := tr.Frame()
	if strings.Contains(f, "✓") {
		t.Errorf("an unverified node must not render a pass glyph:\n%s", f)
	}
	if !strings.Contains(f, "unverified") {
		t.Errorf("an unverified node must be named as such:\n%s", f)
	}
	if !strings.Contains(f, "1 unverified") {
		t.Errorf("the footer must count it as unverified:\n%s", f)
	}
	if strings.Contains(f, "1 verified") {
		t.Errorf("the footer must not count it as verified:\n%s", f)
	}
}

func TestAFailedNodeIsDistinctFromAGap(t *testing.T) {
	// Under the standing ruling only TIME is a gap (§3.1). A failed check and a
	// truncation are opposite claims — one was assessed and lost, the other was never
	// finished — and rendering them alike would make an unaffordable run
	// indistinguishable from a deadline miss.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Enter(enter("n0.0", "n0", 1, 0, "failed one", quarry.FromFloat(5)))
	tr.Enter(enter("n0.1", "n0", 1, 1, "gapped one", quarry.FromFloat(5)))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0.0", Verified: no()})
	tr.Exit(quarry.NodeOutcome{NodeID: "n0.1", Gap: true})
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Verified: yes()})

	f := tr.Frame()
	if !strings.Contains(f, "FAILED") {
		t.Errorf("a failed check must be named:\n%s", f)
	}
	if !strings.Contains(f, "gap (time)") {
		t.Errorf("a gap must be named as a TIME truncation (§3.1):\n%s", f)
	}
	if !strings.Contains(f, "1 FAILED") || !strings.Contains(f, "1 gap") {
		t.Errorf("the footer must count the two separately:\n%s", f)
	}
}

func TestAnInFlightNodeShowsItsAllocationNotACost(t *testing.T) {
	// A node that has not finished has not spent anything, and showing the allocation
	// as though it were a cost would inflate the running total on screen. The ≤ is the
	// distinction, and it is the reason NodeEnter carries the allocation at all.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Enter(enter("n0.0", "n0", 1, 0, "in flight", quarry.FromFloat(4)))

	f := tr.Frame()
	if !strings.Contains(f, "≤ ") {
		t.Errorf("an in-flight node must show a bound, not a cost:\n%s", f)
	}
	// Both the root and the child are open — a parent is entered before it plans, so it
	// is in flight for its whole subtree's duration.
	if !strings.Contains(f, "2 in flight") {
		t.Errorf("the footer must say how many are still open:\n%s", f)
	}
	if !strings.Contains(f, "spent 0.0000") {
		t.Errorf("nothing has been spent yet; the burn-down must say so:\n%s", f)
	}
}

func TestSiblingsRenderInPlanOrderNotArrivalOrder(t *testing.T) {
	// Children are entered concurrently, so arrival order is a race. A tree that
	// reorders itself between runs of the same problem is useless for the debugging
	// §9 wants it for. Entering them backwards is the test.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Enter(enter("n0.2", "n0", 1, 2, "third", quarry.FromFloat(3)))
	tr.Enter(enter("n0.0", "n0", 1, 0, "first", quarry.FromFloat(3)))
	tr.Enter(enter("n0.1", "n0", 1, 1, "second", quarry.FromFloat(3)))

	f := tr.Frame()
	i0, i1, i2 := strings.Index(f, "first"), strings.Index(f, "second"), strings.Index(f, "third")
	if i0 < 0 || i1 < 0 || i2 < 0 {
		t.Fatalf("all three children must render:\n%s", f)
	}
	if i0 >= i1 || i1 >= i2 {
		t.Errorf("siblings must render in plan order, got positions %d/%d/%d:\n%s", i0, i1, i2, f)
	}
	// The last sibling gets the corner, the others the tee — otherwise the tree has no
	// visible end and nesting is ambiguous.
	if !strings.Contains(f, "├─") || !strings.Contains(f, "└─") {
		t.Errorf("branches must distinguish the last sibling:\n%s", f)
	}
}

func TestAPortfolioArmIsLabelledRatherThanRepeated(t *testing.T) {
	// Arms share their parent's statement by definition (§2), so rendering the
	// statement again shows N identical children and reads as a stuck loop — when the
	// repetition IS the strategy.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "the one question", quarry.FromFloat(10)))
	a := enter("n0.0", "n0", 1, 0, "the one question", quarry.FromFloat(5))
	a.Arm = true
	tr.Enter(a)

	f := tr.Frame()
	if !strings.Contains(f, "arm") {
		t.Errorf("an arm must be labelled as one:\n%s", f)
	}
}

func TestACacheHitIsNeitherAPassNorACharge(t *testing.T) {
	// A served node cost this run nothing and carries whatever verdict was stored (§6).
	// Rendering it as a fresh pass would overstate what this run established.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", CacheHit: true, Cost: 0, HaloTokens: 40, GeneratedTokens: 10})

	f := tr.Frame()
	if !strings.Contains(f, "cached") {
		t.Errorf("a served node must be named as cached:\n%s", f)
	}
	if !strings.Contains(f, "1 cached") {
		t.Errorf("the footer must count cache hits separately:\n%s", f)
	}
	if !strings.Contains(f, "spent 0.0000") {
		t.Errorf("a cache hit charges this run nothing:\n%s", f)
	}
}

func TestTheBurnDownUsesTheRootAllocationAsItsDenominator(t *testing.T) {
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(100)))
	tr.Enter(enter("n0.0", "n0", 1, 0, "a", quarry.FromFloat(50)))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0.0", Cost: quarry.FromFloat(25), Verified: yes()})

	f := tr.Frame()
	if !strings.Contains(f, "cap 100.0000") {
		t.Errorf("the cap must come from the root's allocation:\n%s", f)
	}
	if !strings.Contains(f, "(25%)") {
		t.Errorf("the percentage must be spent over cap:\n%s", f)
	}
	if !strings.Contains(f, "█") || !strings.Contains(f, "░") {
		t.Errorf("a capped run gets a proportional bar:\n%s", f)
	}
}

func TestAnUncappedRunGetsNoBarRatherThanAFabricatedOne(t *testing.T) {
	// A bar needs a denominator. Inventing one is the same class of lie as reporting an
	// unmeasured duration as zero (quarry.NodeTiming).
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.Unlimited))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Cost: quarry.FromFloat(3), Verified: yes()})

	f := tr.Frame()
	if strings.Contains(f, "█") || strings.Contains(f, "░") {
		t.Errorf("an uncapped run must not draw a bar:\n%s", f)
	}
	if !strings.Contains(f, "no cap") {
		t.Errorf("an uncapped run must say so:\n%s", f)
	}
	if strings.Contains(f, "-0.0000") {
		t.Errorf("Unlimited is a sentinel and must never render as a negative:\n%s", f)
	}
}

func TestTheFooterAlwaysSaysTheViewIsNotTheRecord(t *testing.T) {
	// P8: a live view is a third lossy projection. Every frame carries the caveat,
	// because a screenshot outlives the context in which it was taken.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	if !strings.Contains(tr.Frame(), "live view") {
		t.Error("a mid-run frame must state that it is not the record")
	}
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Verified: yes()})
	tr.Stop()
	final := tr.Frame()
	if !strings.Contains(final, "citable artifact") {
		t.Errorf("the final frame must point at the record:\n%s", final)
	}
}

func TestAnUnfinishedNodeAtStopIsNotAPermanentSpinner(t *testing.T) {
	// Every Enter is followed by an Exit, so a node still open when the run ends means
	// the run faulted. A spinner would present a broken run as a working one.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Stop()
	f := tr.Frame()
	for _, s := range spinner {
		if strings.Contains(f, s) {
			t.Errorf("a finished run must not show a spinner:\n%s", f)
		}
	}
	if !strings.Contains(f, "!") {
		t.Errorf("an unfinished node must be flagged after Stop:\n%s", f)
	}
}

func TestFrameIsAPureFunctionOfState(t *testing.T) {
	// Two frames with no intervening event must be identical, or a repaint flickers
	// and a test cannot compare bytes. The tick advances only in paint().
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Cost: quarry.FromFloat(1), Verified: yes()})
	// Two SEPARATE calls, bound to variables rather than compared inline: `a != a` is
	// what a linter flags as a tautology, and here it is not one — Frame() is only pure
	// if the two evaluations agree, which is the whole assertion. Naming them says that
	// to a reader as well as to staticcheck.
	first, second := tr.Frame(), tr.Frame()
	if first != second {
		t.Errorf("two frames over the same state must be byte-identical:\n%s\n---\n%s", first, second)
	}
}

func TestNonFancyModeStreamsAndEmitsNoEscapes(t *testing.T) {
	// Escape sequences in a redirected stream produce garbage that outlives the run.
	// `quarry run --fake | tee log` must leave a readable log.
	tr, buf := fixed()
	tr.Start() // must be a no-op, not a repaint loop into the buffer
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Enter(enter("n0.0", "n0", 1, 0, "child", quarry.FromFloat(5)))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0.0", Cost: quarry.FromFloat(2), Verified: yes()})
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Cost: quarry.FromFloat(1), Verified: yes()})
	tr.Stop()

	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("non-fancy output must contain no escape sequences:\n%q", out)
	}
	if !strings.Contains(out, "n0.0") || !strings.Contains(out, "n0 ") {
		t.Errorf("both completions must be streamed:\n%s", out)
	}
	// Completion order, not tree order: children complete before parents, so the child
	// line comes first. Drawing branches here would connect them wrongly.
	if strings.Index(out, "n0.0") > strings.Index(out, "\nn0 ") && strings.Contains(out, "\nn0 ") {
		t.Errorf("the child completes first and must be logged first:\n%s", out)
	}
}

func TestTheCostColumnAlignsAcrossDepths(t *testing.T) {
	// Found by looking at the first rendered frame. Rendering a node to a fixed width
	// and THEN indenting pushes deeper lines further right, so the numbers a reader
	// scans down the page are the ones that move. The tree drawing has to be charged
	// against the line's width, not added to it.
	tr, _ := fixed()
	tr.Width = 60
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(10)))
	tr.Enter(enter("n0.0", "n0", 1, 0, "child at depth one", quarry.FromFloat(5)))
	tr.Enter(enter("n0.0.0", "n0.0", 2, 0, "grandchild at depth two", quarry.FromFloat(2)))
	tr.Exit(quarry.NodeOutcome{NodeID: "n0.0.0", Cost: quarry.FromFloat(1), Verified: yes()})
	tr.Exit(quarry.NodeOutcome{NodeID: "n0.0", Cost: quarry.FromFloat(1), Verified: yes()})
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Cost: quarry.FromFloat(1), Verified: yes()})

	var checked int
	for _, line := range strings.Split(tr.Frame(), "\n") {
		if !strings.Contains(line, "✓") { // node lines only, not the footer's counts
			continue
		}
		checked++
		if w := len([]rune(line)); w != tr.Width {
			t.Errorf("every node line must be exactly %d columns wide, got %d: %q",
				tr.Width, w, line)
		}
	}
	if checked != 3 {
		t.Fatalf("all three depths must have been checked, got %d", checked)
	}
}

func TestFitIsRuneAwareAndTruncatesLeft(t *testing.T) {
	// The glyphs and box characters are multi-byte; a byte-counted truncation would
	// split one and corrupt the line.
	long := strings.Repeat("é", 200)
	got := fit("✓ n0 "+long, "1.0000", 40)
	if len([]rune(got)) > 40 {
		t.Errorf("fit must respect the rune width, got %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "1.0000") {
		t.Errorf("the cost must survive truncation, got %q", got)
	}
	if !strings.Contains(got, "✓ n0") {
		t.Errorf("the glyph and id must survive truncation, got %q", got)
	}
	if !strings.ContainsRune(got, '…') {
		t.Errorf("truncation must be visible, got %q", got)
	}
}

// ------------------------------------------------------------- against a real run

func TestAgainstARealFakeRun(t *testing.T) {
	// The renderer wired to an actual Executor over the fake provider — the demo path,
	// exercised. Everything above tests the renderer against synthetic events; this
	// checks the two agree about a real tree.
	tr, _ := fixed()
	prov := &provider.FakeProvider{}
	e := &quarry.Executor{
		Planner:  provider.FakePlanner{},
		Solver:   quarry.ProviderSolver{Provider: prov, Model: "fake"},
		Reducer:  quarry.ConcatReducer{Sep: "\n"},
		Verifier: quarry.NonEmptyOracle(),
		MaxDepth: 3,
		Observer: tr,
	}
	caps := quarry.Caps{Spend: quarry.FromFloat(1)}
	l, err := quarry.NewLedger(caps, quarry.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	root := quarry.Problem{Statement: "What does the deployment cost per month, " +
		"how does that scale with users, and what dominates the total"}
	res, err := e.Run(context.Background(), root, l)
	if err != nil {
		t.Fatal(err)
	}
	tr.Stop()

	f := tr.Frame()
	if len(res.Outcomes) < 3 {
		t.Fatalf("the fake planner must produce a real tree, got %d nodes", len(res.Outcomes))
	}
	// Every node in the record appears in the display: a viewer that dropped one would
	// draw a tree with a missing branch.
	for _, o := range res.Outcomes {
		if !strings.Contains(f, o.NodeID) {
			t.Errorf("node %s is in the record and not on screen:\n%s", o.NodeID, f)
		}
	}
	// The displayed total must equal the record's. Two independent accumulations of the
	// same costs — one from Exit events, one from the record — and a mismatch means the
	// screen and the artifact disagree about the same run.
	rec := quarry.NewRunRecord(res, root, caps, quarry.ModeFresh)
	if !strings.Contains(f, "spent "+rec.TotalCost().String()) {
		t.Errorf("the display total must equal the record's %s:\n%s", rec.TotalCost(), f)
	}
	// No spinner: the run finished and every node exited.
	for _, s := range spinner {
		if strings.Contains(f, s) {
			t.Errorf("a completed run must show no spinner:\n%s", f)
		}
	}
	t.Logf("\n%s", f)
}

func TestConcurrentCompletionsEachEmitAWholeLine(t *testing.T) {
	// The defect -race found, stated as the thing a reader would actually notice: two
	// siblings completing at once both wrote to w with no lock, so their bytes
	// interleaved and one log line contained halves of two nodes. "No race report" is
	// the mechanism; "every line describes exactly one node" is the guarantee.
	tr, buf := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(100)))
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("n0.%d", i)
			tr.Enter(enter(id, "n0", 1, i, fmt.Sprintf("child number %d", i), quarry.FromFloat(1)))
			tr.Exit(quarry.NodeOutcome{NodeID: id, Cost: quarry.FromFloat(1), Verified: yes()})
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 24 {
		t.Errorf("want 24 streamed lines, one per completion, got %d", len(lines))
	}
	for _, l := range lines {
		// Exactly one node id per line. A spliced line carries two.
		if n := strings.Count(l, "n0."); n != 1 {
			t.Errorf("line names %d nodes, so two writes interleaved: %q", n, l)
		}
		if n := strings.Count(l, "child number"); n != 1 {
			t.Errorf("line carries %d statements: %q", n, l)
		}
	}
}

func TestConcurrentEventsDoNotRace(t *testing.T) {
	// Enter and Exit are called from sibling goroutines with no ordering between them.
	// This is the -race target; the assertion is that it completes at all.
	tr, _ := fixed()
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(100)))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "n0." + string(rune('a'+i%26)) + string(rune('0'+i/26))
			tr.Enter(enter(id, "n0", 1, i, "child", quarry.FromFloat(1)))
			_ = tr.Frame() // a repaint concurrent with the events
			tr.Exit(quarry.NodeOutcome{NodeID: id, Cost: quarry.FromFloat(1), Verified: yes()})
		}(i)
	}
	wg.Wait()
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Verified: yes()})
	if !strings.Contains(tr.Frame(), "spent 32.0000") {
		t.Errorf("all 32 costs must accumulate exactly:\n%s", tr.Frame())
	}
}

func TestARepaintRewritesExactlyTheLinesItLastWrote(t *testing.T) {
	// FOUND BY RUNNING IT IN A REAL TERMINAL, which is the only place this was visible:
	// `--fake-latency 900ms` printed TWELVE stacked headers instead of one that changed
	// in place. Every other fancy-path test compares Frame() — the frame was always
	// right; what was wrong was the cursor arithmetic around it, and no test looked at
	// the bytes paint() emits.
	//
	// The invariant, and the only one that keeps in-place repainting honest: the number
	// of lines paint() REMEMBERS must equal the number it WRITES. frame() ends in a
	// newline and the paint loop appends one to every element Split returns, including
	// the empty final one, so counting newlines undercounted by exactly one. The cursor
	// then rose one line short each tick and the frame walked down the screen.
	//
	// A drift of one per repaint is the worst possible size: invisible in a fast test,
	// unmissable in a slow run.
	tr, buf := fixed()
	tr.Fancy = true // the arithmetic only exists on this path
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(1)))
	tr.Enter(enter("n0.0", "n0", 1, 0, "child", quarry.FromFloat(1)))

	tr.paint()
	remembered := tr.lines
	written := strings.Count(buf.String(), "\n")
	if remembered != written {
		t.Fatalf("paint remembered %d lines and wrote %d; the difference is how far the "+
			"frame drifts down the screen on every tick", remembered, written)
	}

	// And the drift is cumulative, so assert it over repeated paints: the cursor-up
	// count of the second frame must cover the whole of the first.
	buf.Reset()
	tr.paint()
	if want := fmt.Sprintf("\x1b[%dA", remembered); !strings.HasPrefix(buf.String(), want) {
		t.Errorf("the second frame must rise over ALL %d lines of the first, got prefix %.10q",
			remembered, buf.String())
	}
	if got := strings.Count(buf.String(), "\n"); got != tr.lines {
		t.Errorf("the second paint wrote %d lines but remembered %d", got, tr.lines)
	}
}

func TestTheRepaintLoopDoesNotRaceWithTheRun(t *testing.T) {
	// The fancy path has a SECOND writer — Start's own goroutine — and every other test
	// runs with Fancy false, so the repaint loop was untested against concurrent events.
	// That is exactly where the interleaving defect would be least visible: escape
	// sequences garbling each other look like terminal noise rather than a bug.
	tr, _ := fixed()
	tr.Fancy = true                      // force the repaint path over a buffer
	tr.Interval = 100 * time.Microsecond // repaint hard, to collide on purpose
	tr.Start()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("n0.%d", i)
			tr.Enter(enter(id, "n0", 1, i, "child", quarry.FromFloat(1)))
			tr.Exit(quarry.NodeOutcome{NodeID: id, Cost: quarry.FromFloat(1), Verified: yes()})
		}(i)
	}
	tr.Enter(enter("n0", "", 0, 0, "root", quarry.FromFloat(100)))
	wg.Wait()
	tr.Exit(quarry.NodeOutcome{NodeID: "n0", Verified: yes()})
	tr.Stop()
	// Stop paints a final frame, so the last thing written must be the finished tree.
	if !strings.Contains(tr.Frame(), "spent 16.0000") {
		t.Errorf("all 16 costs must accumulate exactly:\n%s", tr.Frame())
	}
}
