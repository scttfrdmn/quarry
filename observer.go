package quarry

import "time"

// Observer is the live-observation seam (§9). Everything else that reads a run reads
// a COMPLETED record; this is the one seam that fires while the run is still
// happening, and that difference is the whole reason it exists as its own
// interface rather than as two more methods on TelemetrySink.
//
// WHY NOT TelemetrySink. That seam is documented as an aggregator over artifacts
// kept anyway (§8.2) — its contract is "a second reader of records", it is called
// once per node with a finished NodeOutcome, and AggregateSink, the OTel tracer and
// every test double satisfy it on those terms. Adding an entry method would:
//
//   - break every existing implementation, including two in this repo;
//   - hand an aggregator a half-built node it has no use for; and
//   - blur a real distinction — a sink may assume its input is final, and an
//     Observer may assume nothing of the kind.
//
// So Observer is separate and independently optional. A run may have neither, one,
// or both.
//
// WHAT THIS BUYS. §9 asks for a live tree, and docs/design.md records the blocker as
// "the telemetry seam fires on node COMPLETION, and children complete before
// parents, so a live emitter would have to thread span contexts down through the
// recursion." That is true of an OTel emitter and NOT true of a renderer that owns
// its own state: given (node id, parent id) at ENTRY, a viewer can place a node in
// the tree before it has an answer, which is exactly the "planning / spending /
// verified / pruned" progression §9 describes. The parent is always announced
// before its children, because a parent is entered before it plans.
//
// STILL NOT A RECORD. Nothing in quarry may read a decision back out of an
// Observer, and no Observer output is citable (P8). It is a third lossy view
// alongside the OTel span tree and the agate RunEvent stream. The RunRecord remains
// the artifact. In particular an Observer sees costs that are still moving and
// verdicts that do not exist yet; a consumer that treats an in-flight number as a
// result will report something the record contradicts.
//
// MUST be safe for concurrent use, for the same reason TelemetrySink must and more
// urgently: sibling subtrees run on separate goroutines, so Enter and Exit
// interleave across branches with no ordering guarantee between siblings. The only
// ordering guaranteed is per-node (a node's Enter precedes its own Exit) and
// ancestral (a parent's Enter precedes any descendant's Enter).
//
// MUST NOT BLOCK. An Observer is called on the executor's own goroutines, in the
// path of the run. A renderer that blocks on a slow terminal or a full channel adds
// its latency to the run it is watching, and under a deadline (§3.1) that means an
// observer can cause the gaps it is displaying. Buffer, drop, or render
// asynchronously — never make the run wait.
type Observer interface {
	// Enter announces a node that is about to be worked on, BEFORE it plans, solves
	// or reads the cache. This is the seam that did not exist: the moment is real in
	// the executor (it already takes a start timestamp there) and nothing was
	// published from it.
	//
	// The allocation is what this node may spend, which is the number a live
	// burn-down needs and which no completed outcome carries — by the time a node
	// finishes, what it WAS ALLOWED is gone and only what it SPENT remains.
	Enter(ev NodeEnter)

	// Exit announces a finished node. Deliberately carries the same NodeOutcome the
	// record will hold, not a summary: a viewer showing a node's cost and verdict
	// should show the values that will be cited, or the display and the artifact
	// disagree about the same run.
	//
	// Every Enter is followed by exactly one Exit, including for a cache hit, a gap,
	// and a node that failed. A missing Exit means the run itself faulted.
	Exit(o NodeOutcome)
}

// NodeEnter is the entry event. Deliberately a struct rather than positional
// parameters: this is the surface a viewer is written against, and adding a field
// to a struct does not break an implementation the way a new parameter would.
type NodeEnter struct {
	// NodeID and ParentID place the node in the tree. ParentID is empty for the root
	// — the ONLY node with no parent, so a viewer can use emptiness as the root test
	// rather than tracking depth.
	//
	// Positional IDs mean a viewer can also derive parentage from the ID itself, but
	// ParentID is passed explicitly because that encoding is quarry's business and a
	// renderer should not have to parse it (§2 records that the execution DAG is a
	// tree in the record precisely so no node has two parents).
	NodeID   string
	ParentID string

	// Depth is the recursion depth, 0 at the root. Carried even though it is
	// derivable from the ID, because a viewer indenting by depth should not have to
	// count separators.
	Depth int

	// Index is this node's position in its parent's plan, 0-based; 0 on the root,
	// which no plan funded. Carried for the same reason ParentID is, and the FIRST
	// renderer proved the need: children are entered CONCURRENTLY, so arrival order
	// is a race and a tree drawn in it reorders itself between runs. Sorting siblings
	// by plan position is what makes the display stable — and the only other source of
	// that number is the node ID's last segment, which the ParentID note above
	// promises a renderer will not have to parse.
	Index int

	// Problem is what this node was asked. Carries the scope tags, so a viewer can
	// show that scope never widened on descent (P6) rather than asserting it.
	Problem Problem

	// Alloc is what this node MAY spend and by when — the live burn-down's
	// denominator, and unavailable from any completed outcome. Spend is Unlimited on
	// an uncapped run; a viewer must render that as "no cap" rather than as a
	// negative number, since Unlimited is a sentinel (see Units.Limited).
	Alloc Allocation

	// Arm marks this node as one competing attempt among N at the same problem (§2).
	// Load-bearing for display and not derivable from the tree: a portfolio's arms
	// share their parent's problem statement, so without this flag a viewer shows N
	// identical children and looks like it is repeating itself, when in fact the
	// repetition IS the strategy.
	Arm bool

	// At is when the node was entered, from the executor's injected clock. Zero when
	// no clock is wired — absence, not an epoch (the same absence-not-zero discipline
	// as NodeTiming). A viewer must not compute an elapsed time from a zero At; it
	// would report ~2026 years of latency.
	At time.Time
}

// ObserverFunc adapts two functions into an Observer, for a viewer that does not
// need a type of its own — and for tests, which is most of this package's callers.
// A nil half is a no-op rather than a panic: an observer that only cares about
// completions is a legitimate consumer, and forcing it to write an empty method is
// the kind of friction that leads to not wiring the seam at all.
type ObserverFunc struct {
	OnEnter func(NodeEnter)
	OnExit  func(NodeOutcome)
}

// Enter calls OnEnter when set. A nil half is a no-op; see the type doc.
func (f ObserverFunc) Enter(ev NodeEnter) {
	if f.OnEnter != nil {
		f.OnEnter(ev)
	}
}

// Exit calls OnExit when set.
func (f ObserverFunc) Exit(o NodeOutcome) {
	if f.OnExit != nil {
		f.OnExit(o)
	}
}

// MultiObserver fans one run out to several observers, in order. Used to watch a
// run in the terminal while also recording it, without either consumer knowing
// about the other.
//
// A nil member is skipped, so a caller can build the slice conditionally
// (MultiObserver{tui, maybeNilRecorder}) without a nil check at every site.
type MultiObserver []Observer

// Enter fans out in slice order, skipping nil members. Order is deliberate and not
// an implementation detail: a caller putting a recorder before a renderer is entitled
// to have the record written first.
func (m MultiObserver) Enter(ev NodeEnter) {
	for _, o := range m {
		if o != nil {
			o.Enter(ev)
		}
	}
}

// Exit fans out in slice order, skipping nil members.
//
// It does NOT recover from a panicking member, deliberately: a viewer that panics has
// a defect, and swallowing it here would leave the run apparently fine while its
// display silently stopped updating.
func (m MultiObserver) Exit(out NodeOutcome) {
	for _, o := range m {
		if o != nil {
			o.Exit(out)
		}
	}
}
