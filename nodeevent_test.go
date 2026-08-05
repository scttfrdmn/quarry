package quarry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Invariants for the live node stream (#14). What is pinned here is what a host
// branches on: the three-state fields surviving to the wire, gaps and unfunded staying
// separate denominations, and the record being unaffected by anyone watching.

// decodeLines parses NDJSON into generic objects, the way a host does — deliberately not
// into quarry's own structs, because unmarshalling into the producer's types would hide
// exactly the wire-level mistakes these tests exist to catch (a renamed key, an omitted
// field, a bool where a string enum belongs).
func decodeLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, line := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			t.Fatalf("line %d is not a JSON object (%q): %v", i+1, line, err)
		}
		out = append(out, obj)
	}
	return out
}

func TestLiveVerdictIsThreeStateOnTheWire(t *testing.T) {
	// D3, and the §8 rule this project has already paid for once at the provenance
	// projection: a nil verdict means UNCHECKED, not failed. Flattening it at the last hop
	// is a defect even when every intermediate layer honoured it — a dashboard painting
	// unchecked as failed reports a verification problem quarry never found.
	//
	// Unchecked is the COMMON case, not the exception: P2 makes verifier availability the
	// primary terminator, so most nodes in a real run were never checked.
	yes, no := true, false
	for _, tc := range []struct {
		name string
		in   *bool
		want string
	}{
		{"unchecked", nil, "not_assessed"},
		{"passed", &yes, "passed"},
		{"failed", &no, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := NodeExitEventOf(NodeOutcome{NodeID: "n0", Verified: tc.in}, false)
			if ev.Verdict != tc.want {
				t.Errorf("verdict for %v = %q, want %q", tc.in, ev.Verdict, tc.want)
			}
			// The wire must carry a STRING, never a bool: a JSON bool has two values and
			// this distinction has three, so the type itself has to hold the third.
			b, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(b, []byte(`"verdict":"`+tc.want+`"`)) {
				t.Errorf("verdict must reach the wire as a string enum, got %s", b)
			}
		})
	}
	// The vocabulary is shared with the OTel projection deliberately. If these diverge, a
	// consumer reading a trace and a live stream has to map between them — so the strings
	// are asserted, not just their distinctness.
	if WireVerdictPassed != "passed" || WireVerdictFailed != "failed" || WireVerdictNotAssessed != "not_assessed" {
		t.Error("the live verdict vocabulary must match otel/tracer.go's; one vocabulary per project")
	}
}

func TestUnmeasuredTimingIsNotZeroOnTheWire(t *testing.T) {
	// Absence is not zero, at a new site (D3). 0 is a real and plausible sub-millisecond
	// duration — precisely the number people build dashboards on — so an unmeasured
	// bracket must be a value no measurement can produce.
	unmeasured := NodeExitEventOf(NodeOutcome{NodeID: "n0"}, false)
	if unmeasured.DurationMicros != -1 {
		t.Errorf("an unmeasured duration must be -1, got %d: zero is a genuine "+
			"sub-millisecond latency and a consumer cannot tell the two apart",
			unmeasured.DurationMicros)
	}
	// A half-stamped bracket is unmeasured too — NodeTiming.Duration already reports
	// ok=false for it, and this asserts the wire honours that rather than computing a
	// duration from a zero end.
	half := NodeExitEventOf(NodeOutcome{
		NodeID: "n0", Timing: NodeTiming{StartedAt: time.Now()},
	}, false)
	if half.DurationMicros != -1 {
		t.Errorf("a half-stamped bracket is unmeasured, got %d", half.DurationMicros)
	}

	measured := NodeExitEventOf(NodeOutcome{NodeID: "n0", Timing: NodeTiming{
		StartedAt: time.Unix(0, 0), EndedAt: time.Unix(0, int64(3*time.Millisecond)),
	}}, false)
	if measured.DurationMicros != 3000 {
		t.Errorf("a measured bracket must carry its duration, got %d", measured.DurationMicros)
	}

	// The same discipline on the entry timestamp. A zero time.Time is year 1, whose Unix
	// micros are hugely negative — a consumer subtracting it reports two millennia of
	// latency, which is the concrete harm the mapping to 0 prevents.
	noClock := NodeEnterEventOf(NodeEnter{NodeID: "n0"})
	if noClock.AtUnixMicros != 0 {
		t.Errorf("an unstamped entry must be 0, not year 1: got %d", noClock.AtUnixMicros)
	}
}

func TestGapsAndUnfundedStaySeparateDenominationsOnTheWire(t *testing.T) {
	// D4 and the standing ruling. Only TIME produces a gap; spend exhaustion produces an
	// unfunded node, which is planned degradation INSIDE authority. A live view that
	// painted the second red would make a cap that worked exactly as P4 promises look like
	// a malfunction, and a host that summed them would offer more time where money was
	// needed.
	gapped := NodeExitEventOf(NodeOutcome{NodeID: "n0.0", Gap: true}, false)
	if !gapped.Gap || gapped.Unfunded {
		t.Errorf("a time-truncated node is a gap and NOT unfunded: %+v", gapped)
	}
	priced := NodeExitEventOf(NodeOutcome{NodeID: "n0.1"}, true)
	if priced.Gap || !priced.Unfunded {
		t.Errorf("a priced-out node is unfunded and NOT a gap: %+v", priced)
	}

	// Both keys are always present. False is a MEASUREMENT here — a consumer seeing no key
	// could not tell "this node was funded" from "this producer does not report funding".
	b, err := json.Marshal(NodeExitEventOf(NodeOutcome{NodeID: "n0"}, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"gap":false`, `"unfunded":false`} {
		if !bytes.Contains(b, []byte(key)) {
			t.Errorf("%s must be emitted, not omitted: %s", key, b)
		}
	}
}

func TestWireUnfundedAgreesWithTheRecordOnEveryNode(t *testing.T) {
	// THE DUPLICATION GUARD. wireUnfunded restates RunRecord.Unfunded's predicate for one
	// node, because a live observer has no record to consult. That predicate has already
	// been derived independently three times and the THIRD COPY WAS WRONG, which is why
	// the accessor exists — so the fourth copy is pinned against the accessor rather than
	// trusted to stay in step.
	//
	// TWO records, and the second one is the whole test. The first draft used the
	// spend-truncated fixture alone and it was VACUOUS against the exact mutation this
	// guards: deleting the model check from the predicate left every verdict in that record
	// unchanged, because nothing in it was solved and answered emptily. A fixture that
	// contains only the state you expect cannot discover the state you forgot.
	//
	// Both are real runs, not hand-assigned outcomes — the accessor's own test
	// (TestUnfundedNamesNodesThatReachedNoModelButNotEmptyAnswers) already pins the
	// predicate against a built record; what is owed HERE is that the two agree on outcomes
	// the executor actually produces.
	spendTruncated, _, _ := truncRun(t, FromFloat(60))
	answeredEmptily := emptyAnswerRun(t)

	for _, rec := range []RunRecord{spendTruncated, answeredEmptily} {
		want := map[string]bool{}
		for _, o := range rec.Unfunded() {
			want[o.NodeID] = true
		}
		for _, o := range rec.Outcomes {
			if got := wireUnfunded(o); got != want[o.NodeID] {
				t.Errorf("node %s (%q): wireUnfunded=%v but the record says %v — the live wire "+
					"and the record disagree about whether the cap priced this node out",
					o.NodeID, o.Problem.Statement, got, want[o.NodeID])
			}
		}
	}

	// NON-VACUITY, asserted rather than assumed, because both halves have failed to
	// materialise once: the first record must contain a node the cap priced out, and the
	// second a node that reached a model and answered emptily. Without the second, the
	// model check in the predicate is untested and its absence is what was wrong before.
	if len(spendTruncated.Unfunded()) == 0 {
		t.Error("fixture invariant: the spend-truncated record must contain unfunded nodes")
	}
	var solvedEmpty int
	for _, o := range answeredEmptily.Outcomes {
		if o.Model != "" && o.Content == "" && !o.Gap && !o.CacheHit && len(o.Children) == 0 {
			solvedEmpty++
		}
	}
	if solvedEmpty == 0 {
		t.Error("fixture invariant: a node that reached a model and answered emptily must " +
			"exist, or nothing here distinguishes an empty ANSWER from an unfunded node (§8)")
	}
	if n := len(answeredEmptily.Unfunded()); n != 0 {
		t.Errorf("a node that was SOLVED and answered emptily is a RESULT, not unfunded: "+
			"the record counts %d unfunded in a fully funded run", n)
	}
}

// emptyAnswerRun produces the case that distinguishes the unfunded predicate from a naive
// content-emptiness check: leaves that reached a model, spent money, and returned nothing.
//
// An empty answer is a RESULT (§8) — the model was asked and had nothing to say. Reading
// it as unfunded would report a cap failure on a run that stayed comfortably inside its
// cap, which is the D4 mislabelling in the other direction.
func emptyAnswerRun(t *testing.T) RunRecord {
	t.Helper()
	e := exec(t, StaticPlanner{P: fanoutPlan("alpha", "beta")}, &fakeProvider{
		cost: FromFloat(1), emptyContent: true,
	})
	e.MaxDepth = 1
	caps := Caps{Spend: FromFloat(1000)}
	l, err := NewLedger(caps, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Run(context.Background(), problem("root"), l)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunRecord(res, problem("root"), caps, ModeFresh)
}

func TestLiveStreamIsNDJSONMatchingTheFoldedStreamsDialect(t *testing.T) {
	// The live events INTERLEAVE with the folded stream on one fd, so a consumer splitting
	// on newlines must not meet two dialects: one object per line, every line
	// \n-terminated INCLUDING the last, HTML escaping off.
	var buf bytes.Buffer
	obs := NewNodeStreamObserver(&buf)
	obs.Enter(NodeEnter{NodeID: "n0", Problem: problem("a & b < c")})
	obs.Exit(NodeOutcome{NodeID: "n0"})

	raw := buf.Bytes()
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Error("the stream must end in a newline, or a host cannot tell a complete final " +
			"event from a truncated one")
	}
	if lines := decodeLines(t, raw); len(lines) != 2 {
		t.Fatalf("want 2 events, got %d", len(lines))
	}
	// Escaping off, like WriteRunEvents: an escaped ampersand in a statement is a
	// different string, and the two streams share a decoder.
	if !bytes.Contains(raw, []byte("a & b < c")) {
		t.Errorf("HTML escaping must be off so content round-trips: %s", raw)
	}
	if obs.Err() != nil {
		t.Errorf("a clean write must not record an error: %v", obs.Err())
	}
}

func TestLiveKindsAreAdditiveToStreamVersion1(t *testing.T) {
	// #14 D2 rests on #9's frozen compatibility rule: ADDING a kind is a MINOR change, so
	// live events ride StreamVersion 1 rather than bumping it. If someone bumps
	// StreamVersion while adding only kinds, this fails and says why — bucktooth and
	// rustynail branch on that number.
	if StreamVersion != 1 {
		t.Errorf("StreamVersion is now %d: adding the live kinds is a MINOR change under "+
			"#9's own rule, so this must not have moved. If a FIELD changed, that is major "+
			"and the twins need telling before it lands", StreamVersion)
	}
	// The live kinds are namespaced and distinct from the frame's two, so a v1 host's
	// skip-unknown path handles them (the unknown-kind corpus case pins that behaviour).
	for _, ev := range []RunEvent{NodeEnterEvent{}, NodeExitEvent{}} {
		kind := ev.eventType()
		if !strings.HasPrefix(kind, "quarry_node_") {
			t.Errorf("a live kind must be namespaced quarry_node_*, got %q", kind)
		}
		if kind == "quarry_stream" || kind == "quarry_outcome" {
			t.Errorf("a live kind must not collide with the frame: %q", kind)
		}
	}
	// The live payload carries its OWN version, per event rather than in the frame: a host
	// may attach mid-stream, where the frame has already gone past.
	b, err := json.Marshal(NodeEnterEventOf(NodeEnter{NodeID: "n0"}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"node_stream_version":1`)) {
		t.Errorf("every live event must state its own contract version: %s", b)
	}
}

func TestExactlyOneVersionFramePerStream(t *testing.T) {
	// With --live-events the frame is written at run START, because a host must be able to
	// refuse a stream before it parses anything. So the fold must NOT write a second one:
	// two contract declarations read as two concatenated streams, which is a different and
	// wrong thing.
	rec := wholeRun(t)

	framed := HostRunEvents(rec, "u", nil)
	unframed := HostRunEventsNoFrame(rec, "u", nil)

	if n := countKind(framed, "quarry_stream"); n != 1 {
		t.Errorf("HostRunEvents must carry exactly one frame, got %d", n)
	}
	if n := countKind(unframed, "quarry_stream"); n != 0 {
		t.Errorf("HostRunEventsNoFrame must carry NO frame, got %d", n)
	}
	// The TERMINAL outcome is present in both: only the OPENING frame is conditional,
	// because its absence is the only in-band signal that a run was killed.
	if n := countKind(unframed, "quarry_outcome"); n != 1 {
		t.Errorf("the unframed fold must still close with the outcome, got %d", n)
	}
	// And the unframed fold is otherwise byte-identical, so the two orderings cannot drift.
	if len(framed) != len(unframed)+1 {
		t.Errorf("the frame must be the ONLY difference: %d vs %d events", len(framed), len(unframed))
	}
}

func TestLiveObserverDoesNotPerturbTheRecord(t *testing.T) {
	// #14 D2's acceptance criterion, and P8's rule: an observer that changes the artifact
	// is not an observer. The record's bytes must be identical whether or not a host is
	// watching, or the citable artifact would depend on who happened to be looking.
	run := func(obs Observer) RunRecord {
		t.Helper()
		e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: FromFloat(1)})
		e.MaxDepth = 2
		e.Observer = obs
		res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
		if err != nil {
			t.Fatal(err)
		}
		return NewRunRecord(res, problem("root"), Caps{Spend: FromFloat(100)}, ModeFresh)
	}

	var buf bytes.Buffer
	unwatched := run(nil)
	watched := run(NewNodeStreamObserver(&buf))

	if unwatched.RunID != watched.RunID {
		t.Errorf("streaming a run changed its content hash: %s vs %s", unwatched.RunID, watched.RunID)
	}
	a, err := unwatched.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	b, err := watched.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("streaming a run changed its canonical bytes (P8)")
	}
	// NON-VACUITY: the stream must actually have carried the run, or this proves only that
	// a silent observer is silent.
	if lines := decodeLines(t, buf.Bytes()); len(lines) < 2*len(watched.Outcomes) {
		t.Errorf("the observer wrote %d events for %d nodes; every node must produce an "+
			"enter AND an exit or the non-perturbation claim is untested",
			len(lines), len(watched.Outcomes))
	}
}

func TestEveryEnterIsPairedWithAnExitOnTheWire(t *testing.T) {
	// The seam guarantees exactly one Exit per Enter, including for a cache hit, a gap and
	// a failed node. A consumer builds a live tree on that pairing: an unpaired Enter shows
	// a node as permanently in flight, which is the symptom a dropped event would produce.
	var buf bytes.Buffer
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 2
	e.Observer = NewNodeStreamObserver(&buf)
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}

	entered, exited := map[string]int{}, map[string]int{}
	for _, obj := range decodeLines(t, buf.Bytes()) {
		id, _ := obj["node_id"].(string)
		switch obj["type"] {
		case "quarry_node_enter":
			entered[id]++
		case "quarry_node_exit":
			exited[id]++
		default:
			t.Errorf("unexpected kind on the live stream: %v", obj["type"])
		}
	}
	if len(entered) != len(res.Outcomes) {
		t.Errorf("entered %d distinct nodes, the run produced %d", len(entered), len(res.Outcomes))
	}
	for id, n := range entered {
		if n != 1 {
			t.Errorf("node %s entered %d times, want 1", id, n)
		}
		if exited[id] != 1 {
			t.Errorf("node %s has %d exits for 1 enter — an unpaired enter shows as "+
				"permanently in flight", id, exited[id])
		}
	}
}

func TestConcurrentLiveWritesDoNotInterleaveBytes(t *testing.T) {
	// Siblings run on separate goroutines, so two Exits can land at once. The guarantee is
	// that EVERY LINE IS ONE WHOLE EVENT — the -race report is only the mechanism, and the
	// defect it catches here corrupts a HOST's stream rather than a human's display.
	//
	// Asserted on the parse, not on the race detector, so it fails on the guarantee even
	// under a plain `go test`.
	var buf bytes.Buffer
	obs := NewNodeStreamObserver(&buf)
	const n = 60
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			obs.Enter(NodeEnter{NodeID: "n0." + string(rune('a'+i%26)), Depth: 1, Index: i})
			obs.Exit(NodeOutcome{NodeID: "n0." + string(rune('a'+i%26)), Depth: 1})
		}(i)
	}
	wg.Wait()

	lines := decodeLines(t, buf.Bytes())
	if len(lines) != 2*n {
		t.Fatalf("got %d parsed events from %d writes: a short count means two writers "+
			"interleaved into one unparseable line", len(lines), 2*n)
	}
}

func TestALiveWriteFailureDoesNotFailTheRun(t *testing.T) {
	// The one judgement call in the observer, asserted rather than left in a comment. A
	// viewer whose pipe closed must not kill the run it is watching — that is exactly what
	// P8's non-perturbation rule forbids — so the error is RECORDED and retrievable, and
	// the run completes with a valid record.
	//
	// The truncated stream is itself the honest signal: a host finds no terminal outcome
	// and reports a crash, which is the `crashed` corpus case.
	obs := NewNodeStreamObserver(brokenWriter{})
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 2
	e.Observer = obs
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatalf("a failed observer write must not fail the run: %v", err)
	}
	if len(res.Outcomes) == 0 {
		t.Fatal("the run must still have produced a tree")
	}
	if obs.Err() == nil {
		t.Error("the write failure must be retrievable, or a host's truncated stream is " +
			"a silent fault")
	}
	if !errors.Is(obs.Err(), errBrokenPipe) {
		t.Errorf("the recorded error must wrap the cause, got %v", obs.Err())
	}
}

var errBrokenPipe = errors.New("broken pipe")

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

func countKind(events []RunEvent, kind string) int {
	n := 0
	for _, e := range events {
		if e.eventType() == kind {
			n++
		}
	}
	return n
}
