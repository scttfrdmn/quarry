package quarry

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// These tests pin the LIVE seam's contract (§9). A viewer is written against these
// guarantees, so each is an invariant rather than an observation about the current
// implementation:
//
//   - every Enter is followed by exactly one Exit (a missing Exit shows a node as
//     permanently in-flight);
//   - a parent's Enter precedes every descendant's Enter (a viewer cannot place a
//     node whose parent it has not seen);
//   - the entry event carries the ALLOCATION, which no completed outcome holds.

// recObserver records the event sequence. Locked because siblings enter and exit on
// separate goroutines — the requirement the Observer doc states, enforced here by
// -race.
type recObserver struct {
	mu      sync.Mutex
	entered []NodeEnter
	exited  []NodeOutcome
	seq     []string // interleaved: "+id" entered, "-id" exited
}

func (r *recObserver) Enter(ev NodeEnter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entered = append(r.entered, ev)
	r.seq = append(r.seq, "+"+ev.NodeID)
}

func (r *recObserver) Exit(o NodeOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exited = append(r.exited, o)
	r.seq = append(r.seq, "-"+o.NodeID)
}

func (r *recObserver) enterOf(id string) (NodeEnter, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.entered {
		if ev.NodeID == id {
			return ev, true
		}
	}
	return NodeEnter{}, false
}

func (r *recObserver) posOf(tok string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.seq {
		if s == tok {
			return i
		}
	}
	return -1
}

func (r *recObserver) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entered), len(r.exited)
}

func TestEveryEnteredNodeAlsoExits(t *testing.T) {
	// The pairing is what lets a viewer show progress rather than a set of spinners
	// that never stop. It holds structurally because each event fires from one funnel,
	// but a future path returning early without emitting would break it, and this is
	// the test that would say so.
	obs := &recObserver{}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 2
	e.Observer = obs

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100))); err != nil {
		t.Fatal(err)
	}

	in, out := obs.counts()
	if in == 0 {
		t.Fatal("no nodes were announced; the seam is not wired")
	}
	if in != out {
		t.Fatalf("%d entered but %d exited — a viewer would show %d nodes in flight forever",
			in, out, in-out)
	}
	for _, ev := range obs.entered {
		enter, exit := obs.posOf("+"+ev.NodeID), obs.posOf("-"+ev.NodeID)
		if exit < 0 {
			t.Errorf("node %s entered and never exited", ev.NodeID)
			continue
		}
		if exit < enter {
			t.Errorf("node %s exited before it entered", ev.NodeID)
		}
	}
}

func TestAParentIsAnnouncedBeforeItsChildren(t *testing.T) {
	// THE GUARANTEE THAT MAKES A LIVE TREE POSSIBLE, and the one docs/design.md recorded as
	// missing. A viewer places a node under its parent at entry; if a child could be
	// announced first the viewer would have to buffer orphans — which is exactly what
	// the OTel exporter must do, because ITS seam fires on completion and children
	// complete before parents.
	//
	// It holds for a structural reason worth stating: a parent is entered before it
	// plans, and it cannot spawn a child it has not planned.
	obs := &recObserver{}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: 0})
	e.MaxDepth = 3 // 15 nodes, three levels of parentage
	e.Observer = obs

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000))); err != nil {
		t.Fatal(err)
	}

	var checked int
	for _, ev := range obs.entered {
		if ev.ParentID == "" {
			continue
		}
		parent, child := obs.posOf("+"+ev.ParentID), obs.posOf("+"+ev.NodeID)
		if parent < 0 {
			t.Errorf("node %s names parent %s, which was never announced", ev.NodeID, ev.ParentID)
			continue
		}
		if parent > child {
			t.Errorf("child %s announced at %d before its parent %s at %d — a viewer cannot "+
				"place it", ev.NodeID, child, ev.ParentID, parent)
		}
		checked++
	}
	if checked < 6 {
		t.Fatalf("fixture must be several levels deep; only %d parented nodes seen", checked)
	}
}

func TestOnlyTheRootHasNoParent(t *testing.T) {
	// A viewer uses an empty ParentID as the root test rather than tracking depth, so
	// exactly one node may have one.
	obs := &recObserver{}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 2
	e.Observer = obs

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100))); err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, ev := range obs.entered {
		if ev.ParentID != "" {
			continue
		}
		roots = append(roots, ev.NodeID)
		if ev.Depth != 0 {
			t.Errorf("node %s has no parent but depth %d", ev.NodeID, ev.Depth)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("exactly one parentless node, got %v", roots)
	}
	if roots[0] != "n0" {
		t.Errorf("the root is n0, got %q", roots[0])
	}
}

func TestEntryCarriesThePlanPositionOfEachSibling(t *testing.T) {
	// Children are entered CONCURRENTLY, so arrival order is a race: a viewer ordering
	// siblings by the sequence it saw them draws a different tree on every run. Index
	// is the stable order, and the alternative — parsing the node ID's last segment —
	// is exactly what the NodeEnter doc promises a renderer will not have to do.
	obs := &recObserver{}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b", "c")}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 1
	e.Observer = obs

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100))); err != nil {
		t.Fatal(err)
	}

	// Every child of the root must carry a distinct position covering 0..n-1, so the
	// set is orderable with no gaps and no ties.
	seen := map[int]string{}
	for _, ev := range obs.entered {
		if ev.ParentID != "n0" {
			continue
		}
		if prior, dup := seen[ev.Index]; dup {
			t.Errorf("nodes %s and %s share plan position %d", prior, ev.NodeID, ev.Index)
		}
		seen[ev.Index] = ev.NodeID
	}
	if len(seen) != 3 {
		t.Fatalf("want 3 positioned children, got %d", len(seen))
	}
	for i := 0; i < 3; i++ {
		want := childID("n0", i)
		if seen[i] != want {
			t.Errorf("position %d must be %s, got %s", i, want, seen[i])
		}
	}
	// The root is not in anyone's plan, so its position is a zero meaning "unset" —
	// the same convention as PlanWeight.
	root, ok := obs.enterOf("n0")
	if !ok {
		t.Fatal("the root was not announced")
	}
	if root.Index != 0 {
		t.Errorf("the root has no plan position, want 0 got %d", root.Index)
	}
}

func TestEntryCarriesTheAllocationThatNoOutcomeHolds(t *testing.T) {
	// THE REASON Enter TAKES ITS OWN TYPE. A live burn-down needs the denominator —
	// what a node MAY spend — and by the time a node completes that number is gone and
	// only the actual spend remains. If entry carried a NodeOutcome there would be
	// nothing to divide by.
	obs := &recObserver{}
	const cap = 100
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 2
	e.Observer = obs

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(cap))); err != nil {
		t.Fatal(err)
	}

	root, ok := obs.enterOf("n0")
	if !ok {
		t.Fatal("the root was not announced")
	}
	if root.Alloc.Spend != FromFloat(cap) {
		t.Errorf("the root's allocation is the whole cap, want %s got %s",
			FromFloat(cap), root.Alloc.Spend)
	}
	// A child's allocation must be a real subdivision — neither the parent's balance
	// nor zero — or a per-node burn-down bar would be meaningless.
	child, ok := obs.enterOf("n0.0")
	if !ok {
		t.Fatal("child n0.0 was not announced")
	}
	if !child.Alloc.Spend.Limited() || child.Alloc.Spend <= 0 {
		t.Errorf("a funded child must carry a positive limited allocation, got %s",
			child.Alloc.Spend)
	}
	if child.Alloc.Spend >= root.Alloc.Spend {
		t.Errorf("a child's allocation must be a share of its parent's (%s), got %s",
			root.Alloc.Spend, child.Alloc.Spend)
	}
}

func TestACacheHitIsStillAnnounced(t *testing.T) {
	// A served node did no work and spent nothing, but it is still part of the shape
	// (§6) and a viewer that skipped it would draw a tree with a missing branch. Enter
	// fires BEFORE the cache read for exactly this reason.
	obs := &recObserver{}
	c := NewMemCache(0)
	warm := problem("a")
	c.Append(warm, Sample{Content: "stored"}, nil, now)

	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 2
	e.Cache = c
	e.Observer = obs

	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100)))
	if err != nil {
		t.Fatal(err)
	}

	// Non-vacuity: the fixture must really have produced a hit.
	var hits int
	for _, o := range res.Outcomes {
		if o.CacheHit {
			hits++
		}
	}
	if hits == 0 {
		t.Fatal("fixture produced no cache hit; the check is vacuous")
	}

	var announced int
	for _, o := range obs.exited {
		if o.CacheHit {
			announced++
		}
	}
	if announced != hits {
		t.Errorf("the run had %d cache hits and the observer saw %d — a viewer would draw a "+
			"tree with a missing branch", hits, announced)
	}
	if in, out := obs.counts(); in != out {
		t.Errorf("a served node must still pair: %d entered, %d exited", in, out)
	}
}

func TestPortfolioArmsAreFlaggedAtEntry(t *testing.T) {
	// Arms share their parent's problem statement by definition (§2), so without this
	// flag a viewer shows N identical children and looks like it is repeating itself —
	// when the repetition IS the strategy. Not derivable from the tree.
	obs := &recObserver{}
	e := exec(t, StaticPlanner{P: portfolioOf("attempt", 3)}, &fakeProvider{cost: FromFloat(1)})
	e.MaxDepth = 1
	e.Observer = obs

	if _, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(100))); err != nil {
		t.Fatal(err)
	}
	var arms, nonArms int
	for _, ev := range obs.entered {
		if ev.Arm {
			arms++
		} else {
			nonArms++
		}
	}
	if arms != 3 {
		t.Errorf("three arms must be flagged at entry, got %d", arms)
	}
	if nonArms != 1 { // the root, which no plan funded
		t.Errorf("only the root is not an arm, got %d non-arms", nonArms)
	}
}

func TestObserverAndSinkAreIndependent(t *testing.T) {
	// The two seams stayed separate so an aggregator is never handed a half-built node
	// and no existing implementation broke. Wiring either must not require the other.
	obs := &recObserver{}
	e1 := exec(t, DeclinePlanner{}, &fakeProvider{cost: FromFloat(1)})
	e1.Observer = obs
	if _, err := e1.Run(context.Background(), problem("q"), ledger(t, FromFloat(10))); err != nil {
		t.Fatal(err)
	}
	if in, _ := obs.counts(); in == 0 {
		t.Error("an observer must work with no sink wired")
	}

	// Sink only — the pre-existing configuration, which must be unaffected.
	sink := NewAggregateSink()
	e2 := exec(t, DeclinePlanner{}, &fakeProvider{cost: FromFloat(1)})
	e2.Sink = sink
	if _, err := e2.Run(context.Background(), problem("q"), ledger(t, FromFloat(10))); err != nil {
		t.Fatal(err)
	}
	if sink.Snapshot().Nodes == 0 {
		t.Error("a sink must work with no observer wired")
	}
}

func TestObserverDoesNotPerturbTheRecord(t *testing.T) {
	// A live view is a THIRD lossy projection, never the artifact (P8). Watching a run
	// must not change its bytes — otherwise the citable record would depend on whether
	// somebody happened to be looking.
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
	unwatched, watched := run(nil), run(&recObserver{})
	if unwatched.RunID != watched.RunID {
		t.Errorf("observing a run changed its content hash: %s vs %s", unwatched.RunID, watched.RunID)
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
		t.Error("observing a run changed its canonical bytes (P8)")
	}
}

func TestNilObserverHalvesAreNoOps(t *testing.T) {
	// An observer that only cares about completions is a legitimate consumer, and
	// forcing it to write an empty method is the friction that leads to not wiring the
	// seam at all.
	var exits int
	f := ObserverFunc{OnExit: func(NodeOutcome) { exits++ }}
	f.Enter(NodeEnter{NodeID: "n0"}) // must not panic
	f.Exit(NodeOutcome{NodeID: "n0"})
	if exits != 1 {
		t.Errorf("the wired half must still fire, got %d", exits)
	}

	m := MultiObserver{nil, f, nil}
	m.Enter(NodeEnter{NodeID: "n0"}) // a nil member is skipped, not a panic
	m.Exit(NodeOutcome{NodeID: "n0"})
	if exits != 2 {
		t.Errorf("MultiObserver must skip nils and call the rest, got %d", exits)
	}
}
