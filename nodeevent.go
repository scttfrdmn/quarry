package quarry

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// A wire format for the LIVE Observer seam (§9, issue #14) — per-node progress a host
// can watch while the run is still happening.
//
// # Why this cannot be part of the folded stream
//
// runevent.go folds a COMPLETED RunRecord. A host reading it learns nothing until the
// run ends, and the two things are different in kind: live progress is an in-flight
// number, a fold is a final one. Two independent reasons keep them apart rather than
// widening one into the other:
//
//   - agate's union has no `node` or `plan` event, so the decomposition's SHAPE has
//     nowhere to go in it (a standing §9 divergence). The right answer was NOT to ask
//     agate for a quarry-specific kind — that would push quarry's model of computation
//     into a layer that correctly scoped itself out of it.
//   - `cmd/quarry`'s record renderer and live renderer are already deliberately
//     separate code, so the display cannot confuse an in-flight number with a final
//     one. A single wire format for both would reintroduce exactly that confusion one
//     layer out.
//
// So this is quarry's own protocol, documented as such, and the §9 divergence stays
// intact rather than being quietly resolved in the wrong layer (#14 D1).
//
// # It rides the SAME stdout stream, and that needs no version bump (#14 D2)
//
// #9 froze the frame with two rules that were written for precisely this: ADDING a
// kind is a MINOR change, and a consumer must not key on line position — which is why
// TerminalOutcome scans backwards rather than reading the last line. Live kinds are
// therefore additive to StreamVersion 1: a v1 host that does not know them skips them
// (the `unknown-kind` corpus case is the fixture that pins that behaviour) and still
// folds every kind it does know.
//
// A second destination was the alternative and is worse. It would mean a host
// correlating two streams with no ordering guarantee between them, and the ordering is
// the whole value: a node's live entry must be readable as preceding the fold that
// summarises it. One ordered stream gives that for free.
//
// The one consequence, handled in cmd/quarry/run.go: when live events are emitted the
// StreamEvent must be written at run START rather than in front of the fold, because a
// host must be able to refuse a stream before it reads anything it would try to parse.
// The version frame's job is to come first, not to come with the fold.
//
// # NOT A RECORD, and nothing here is citable (P8)
//
// Same standing as the Observer seam itself: an in-flight node has costs still moving
// and verdicts that do not exist yet. The RunRecord remains the artifact and the
// artifact event's url points at it. A host that treats a live number as a result will
// report something the record contradicts.

// NodeStreamVersion is the contract version for the LIVE kinds, carried on each event
// rather than announced once.
//
// SEPARATE FROM StreamVersion, and the separation is the point. The two streams have
// different consumers with different tolerances: a dashboard drawing a live tree can be
// upgraded independently of a supervisor that folds the terminal outcome, and coupling
// them would mean any change to the live shape forced a major bump on hosts that never
// read a live event. StreamVersion still frames the transport; this versions the
// payload of these two kinds.
//
// Carried PER EVENT and not in the frame for the same reason: a host may begin reading
// mid-stream (attaching to a running job, tailing a log), where the frame has already
// gone past. An event that cannot state its own contract is unreadable out of context.
const NodeStreamVersion = 1

// NodeEnterEvent announces a node about to be worked on — the live half of #14.
//
// EVERY FIELD HERE IS KNOWN AT ENTRY, and the type carries nothing else. That is D3
// made structural rather than promised in a comment: there is no verdict field to leave
// nil, no cost field to leave zero, and no duration to fabricate. A dashboard cannot
// render an in-flight zero as a measured zero if the zero was never sent.
//
// What a node HAS at entry is its place in the tree and its authority to spend. What it
// does not have is any result at all. Those are two different types on this wire for the
// same reason Observer.Enter and Observer.Exit are two methods.
type NodeEnterEvent struct {
	Type    string `json:"type"`
	Version int    `json:"node_stream_version"`

	// NodeID and ParentID place the node. ParentID is empty ONLY for the root, so a
	// consumer uses emptiness as the root test rather than tracking depth. Not omitempty:
	// an absent key would be indistinguishable from a producer that does not report
	// parentage, and a consumer would have to guess whether it had a forest or a tree.
	NodeID   string `json:"node_id"`
	ParentID string `json:"parent_id"`

	// Depth and Index are carried even though both are derivable from the ID, because
	// the ID's encoding is quarry's business and a renderer must not have to parse it.
	//
	// Index is load-bearing and the first renderer proved it: children are entered
	// CONCURRENTLY, so arrival order is a race and a tree drawn in it reorders itself
	// between runs. Sorting siblings by plan position is what makes a live display stable.
	Depth int `json:"depth"`
	Index int `json:"index"`

	// Statement is what this node was asked. Scope rides beside it rather than being
	// folded in, so a consumer can show that scope never widened on descent (P6) rather
	// than taking it on trust.
	Statement string            `json:"statement"`
	Scope     map[string]string `json:"scope,omitempty"`

	// AllocMicros is what this node MAY spend, in micro-units, or -1 for Unlimited —
	// the live burn-down's DENOMINATOR, and a number no completed outcome carries: by the
	// time a node finishes, what it was ALLOWED is gone and only what it SPENT remains.
	//
	// Integral like every quarry-owned figure (#18): the floats on this stream exist only
	// where agate's union demands them. -1 is Units' own Unlimited sentinel passed
	// through, not a swap — a consumer must render it as "no cap", never as a negative
	// budget.
	AllocMicros int64 `json:"alloc_micros"`

	// Arm marks this node as one competing attempt among N at the same problem (§2).
	// Not derivable from the tree: a portfolio's arms SHARE their parent's statement, so
	// without this a viewer shows N identical children and looks like it is stuck, when
	// the repetition IS the strategy.
	Arm bool `json:"arm"`

	// AtUnixMicros is when the node was entered, or 0 for UNMEASURED — the executor had
	// no clock wired. Absence, not an epoch (D3, the same discipline as NodeTiming).
	//
	// A consumer MUST NOT compute an elapsed time from 0; it would report roughly two
	// millennia of latency. Micros rather than RFC3339 because a live consumer subtracts
	// these, and a wire format that requires parsing a timestamp to do arithmetic invites
	// each host to get the timezone wrong differently.
	AtUnixMicros int64 `json:"at_unix_micros"`
}

func (NodeEnterEvent) eventType() string { return "quarry_node_enter" }

// NodeExitEvent announces a finished node.
//
// Carries the values the RECORD will hold, not a summary, for the reason Observer.Exit
// does: a live view showing a cost and a verdict should show the numbers that will be
// cited, or the display and the artifact disagree about the same run.
//
// THE THREE-STATE FIELDS SURVIVE TO THIS WIRE (D3, §8), and each is a pointer or a
// string enum rather than a zero-valued scalar, because this is the last hop before a
// dashboard and flattening a distinction at the projection is a defect even when every
// intermediate layer honoured it — the fabricated `stability: 0.0` in provenance.go is
// the same defect, already paid for once.
type NodeExitEvent struct {
	Type    string `json:"type"`
	Version int    `json:"node_stream_version"`

	NodeID string `json:"node_id"`
	Depth  int    `json:"depth"`

	// CostMicros is what this node ACTUALLY spent, integral, -1 for Unlimited. Read
	// against the entry event's AllocMicros for the burn-down.
	CostMicros int64 `json:"cost_micros"`

	// Verdict is THREE-STATE: "passed", "failed", or "not_assessed". A bool could not
	// hold the third, and the third is the common case — P2 makes verifier availability
	// the primary terminator, so most nodes are never checked. A nil verdict means
	// UNCHECKED, not failed (§8); a dashboard painting unchecked as failed reports a
	// verification problem quarry never found.
	//
	// The vocabulary matches otel/tracer.go's deliberately: one verdict vocabulary
	// across quarry's projections, so a consumer reading a trace and a live stream does
	// not have to map between them.
	Verdict string `json:"verdict"`

	// Gap is TIME truncation and ONLY time truncation (D4, §3.1 standing ruling).
	//
	// Unfunded is the money counterpart: the cap priced this node out. It is planned
	// degradation INSIDE authority, exactly what P4 promises, and a live view that paints
	// it red makes a cap that worked look like a malfunction. THE TWO MUST NEVER BE
	// SUMMED — a host that added them would offer more time where money was needed.
	//
	// Both always present, never omitempty: false is a measurement here.
	Gap      bool `json:"gap"`
	Unfunded bool `json:"unfunded"`

	// CacheHit means this node's answer came from the store, so the tokens were really
	// spent once and THIS run paid nothing (§6). A live view showing it as a fresh call
	// overstates the run's work.
	CacheHit bool `json:"cache_hit"`

	// BaseCase names why the node stopped recursing, or "" if it did not stop — it
	// planned and has children. Carried because P2's terminator is the fact a viewer most
	// wants and the one least reconstructable: "no_verifier" and "max_depth" look
	// identical in a tree and mean opposite things about the design.
	BaseCase string `json:"base_case"`

	// Children are the node IDs this node reduced over, so a consumer can close out a
	// subtree without inferring it from ID prefixes.
	Children []string `json:"children,omitempty"`

	// Retries is how many times the leaf was re-solved after a failed verification (§5).
	Retries int `json:"retries"`

	// ModelVersion is the explicit pinned version that produced a leaf's content, never
	// an alias (P8). Empty on an internal reduce node, a cache hit, a gap and an unfunded
	// node — see the ModelEvent doc and #20: a reduce node IS a real model call whose
	// version the executor does not currently assign, so a consumer summing live spend by
	// model will find the same residual the folded stream has.
	ModelVersion string `json:"model_version,omitempty"`

	// HaloTokens and GeneratedTokens are the input/output split (P1, §8.2).
	HaloTokens      int `json:"halo_tokens"`
	GeneratedTokens int `json:"generated_tokens"`

	// DurationMicros is wall-clock, or -1 for UNMEASURED — no clock was wired, or the
	// bracket was half-stamped. NOT 0, which is a real and plausible sub-millisecond
	// duration and exactly the number people build dashboards on; the same reason
	// otel/tracer.go carries a `quarry.timing.measured` bool where semconv has no key.
	//
	// This is the one field on this event that is NOT in the record's hash (NodeTiming is
	// `json:"-"`), and that asymmetry is correct: a record proves what was spent and
	// decided, never how long it took.
	DurationMicros int64 `json:"duration_micros"`
}

func (NodeExitEvent) eventType() string { return "quarry_node_exit" }

// NodeEnterEventOf projects a live entry onto the wire (#14 D3).
//
// A pure function of the seam's own value, so the projection is testable with no run,
// no clock and no writer — the same discipline as RunEvents.
func NodeEnterEventOf(ev NodeEnter) NodeEnterEvent {
	return NodeEnterEvent{
		Type:      "quarry_node_enter",
		Version:   NodeStreamVersion,
		NodeID:    ev.NodeID,
		ParentID:  ev.ParentID,
		Depth:     ev.Depth,
		Index:     ev.Index,
		Statement: ev.Problem.Statement,
		// Tags directly: Scope's map IS the scope, and copying it into a differently
		// shaped object would let the two drift. Nil stays nil, so an unscoped run emits
		// no key rather than an empty object claiming an empty scope was set.
		Scope: ev.Problem.Scope.Tags,
		// int64(Units) directly. Unlimited is already -1, so this is the value and not a
		// sentinel translation.
		AllocMicros:  int64(ev.Alloc.Spend),
		Arm:          ev.Arm,
		AtUnixMicros: unixMicros(ev.At),
	}
}

// NodeExitEventOf projects a completed node onto the wire (#14 D3, D4).
//
// unfunded is passed in rather than derived, because the predicate is a property of the
// RECORD's set (RunRecord.Unfunded walks every outcome) and this function sees one node.
// Deriving it here would be a fourth copy of a predicate that was already wrong in its
// third — see the Unfunded doc.
func NodeExitEventOf(o NodeOutcome, unfunded bool) NodeExitEvent {
	e := NodeExitEvent{
		Type:            "quarry_node_exit",
		Version:         NodeStreamVersion,
		NodeID:          o.NodeID,
		Depth:           o.Depth,
		CostMicros:      int64(o.Cost),
		Verdict:         WireVerdict(o.Verified),
		Gap:             o.Gap,
		Unfunded:        unfunded,
		CacheHit:        o.CacheHit,
		BaseCase:        string(o.BaseCase),
		Children:        o.Children,
		Retries:         o.Retries,
		ModelVersion:    o.ModelVersion,
		HaloTokens:      o.HaloTokens,
		GeneratedTokens: o.GeneratedTokens,
		DurationMicros:  -1,
	}
	// Absence is not zero: only a measured bracket produces a duration, and Duration
	// already reports ok=false on an unset, half-stamped or reversed one.
	if d, ok := o.Timing.Duration(); ok {
		e.DurationMicros = d.Microseconds()
	}
	return e
}

// The live verdict vocabulary. Deliberately the same three strings otel/tracer.go
// uses, so quarry has ONE verdict vocabulary across its projections rather than one per
// wire — a consumer reading both should not have to map between them.
const (
	WireVerdictPassed      = "passed"
	WireVerdictFailed      = "failed"
	WireVerdictNotAssessed = "not_assessed"
)

// WireVerdict renders the three-state verdict (§8).
//
// nil is "not_assessed" and NOT "failed", which is the whole reason this function
// exists rather than a `*bool` on the wire that every host would flatten its own way.
// Unchecked is the common case, not the exception: P2 makes verifier availability the
// primary terminator, so most nodes in a real run were never checked at all.
func WireVerdict(v *bool) string {
	switch {
	case v == nil:
		return WireVerdictNotAssessed
	case *v:
		return WireVerdictPassed
	default:
		return WireVerdictFailed
	}
}

// unixMicros stamps a time for the wire, mapping the zero time to 0 — UNMEASURED.
//
// A zero time.Time is year 1, whose Unix microseconds are a large negative number that
// a consumer would happily subtract into a two-millennium latency. Mapping it to 0
// keeps "unmeasured" a single obvious value on the wire, which the field doc names.
func unixMicros(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

// NodeStreamObserver writes live node events to a stream as they happen — the
// Observer-to-wire adapter, and the only consumer of these types quarry ships (#14 D2).
//
// # It must not slow the run it is watching
//
// An Observer is called on the EXECUTOR's own goroutines, in the path of the run, so a
// writer that blocks adds its latency to the run — and under a deadline (§3.1) an
// observer can cause the gaps it is displaying. The mitigation here is deliberately the
// simple one: hold a mutex, encode, write, return. That is bounded work against a pipe
// and it is what `tui.Tree` already does for a terminal.
//
// What it does NOT do is buffer into a channel with a background flusher. A flusher
// needs either a clock (Go rule 4 forbids one in this package) or a lifecycle Stop the
// Observer seam has no method for — and a dropped event is worse here than a slow one:
// a consumer would see an Enter with no Exit and show a node as permanently in flight.
// A host that needs decoupling should read from the pipe faster, which is the one place
// with a buffer that can grow.
//
// # It must not perturb the record (D2, P8)
//
// Write-only with respect to the run: it reads the seam's values, encodes them, and
// returns nothing to the executor. A run's bytes are identical whether or not anyone is
// watching, and that is asserted on the emitted record rather than argued here.
//
// A write error is RECORDED AND THE RUN CONTINUES, which is the one judgement call in
// this type. The alternative — failing the run because a viewer's pipe closed — would
// let an observer kill the thing it observes, which is precisely what P8's
// non-perturbation rule forbids. The error is retrievable with Err so a caller can
// report it after the run, and the truncated stream is itself the honest signal: a host
// finds no terminal outcome and reports a crash, exactly as the `crashed` corpus case
// specifies.
type NodeStreamObserver struct {
	mu  sync.Mutex
	enc *json.Encoder
	err error
}

// NewNodeStreamObserver writes live events to w as NDJSON.
//
// Encoding matches WriteRunEvents exactly — HTML escaping off, one object per line,
// every line \n-terminated including the last — because these events INTERLEAVE with
// that stream on the same fd and a consumer splitting on newlines must not meet two
// dialects.
func NewNodeStreamObserver(w io.Writer) *NodeStreamObserver {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &NodeStreamObserver{enc: enc}
}

// Enter writes the entry event.
func (o *NodeStreamObserver) Enter(ev NodeEnter) {
	o.write(NodeEnterEventOf(ev))
}

// Exit writes the completion event.
//
// The unfunded flag is derived HERE from the single outcome, and it is the one place
// this file departs from "the record decides". The reason is that a live consumer needs
// the money-versus-time distinction at the moment the node finishes, and the record
// does not exist yet — so the predicate is applied per node and the shared helper keeps
// it from becoming a fourth divergent copy.
func (o *NodeStreamObserver) Exit(out NodeOutcome) {
	o.write(NodeExitEventOf(out, wireUnfunded(out)))
}

// write encodes one event under the lock. Held across the write deliberately: two
// sibling goroutines finishing at once would otherwise interleave their bytes into one
// unparseable line, which is the defect `-race` found in tui.Tree and which here would
// corrupt a host's stream rather than a human's display.
func (o *NodeStreamObserver) write(ev RunEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		// Already broken. Continuing to write into a failed stream would append events
		// after a partial line, turning a detectable truncation into a corrupt stream.
		return
	}
	if err := o.enc.Encode(ev); err != nil {
		o.err = fmt.Errorf("write live node event %s: %w", ev.eventType(), err)
	}
}

// Err reports the first write failure, or nil. A caller should check it AFTER the run
// and report it as a fault: the run itself is unaffected and its record is valid, but
// the host watching it saw a truncated stream.
func (o *NodeStreamObserver) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// wireUnfunded reports whether one outcome is a node the cap priced out (D4).
//
// THE SAME PREDICATE AS RunRecord.Unfunded, applied to a single node, and it must stay
// that way — the discriminator is the absence of a MODEL together with empty content
// and no verdict, because an empty ANSWER is a result (§8) and content-emptiness alone
// conflates the two. That predicate has already been derived independently three times
// and the third copy was wrong, which is why RunRecord.Unfunded became an accessor.
//
// It cannot simply CALL that accessor: the accessor walks a record's outcomes and a live
// observer has one node and no record. So the shape is duplicated in the one place that
// cannot avoid it, named, and pinned by a test asserting the two agree on every node of
// a real run.
func wireUnfunded(o NodeOutcome) bool {
	if o.Gap || o.CacheHit || len(o.Children) > 0 {
		return false
	}
	return o.Model == "" && o.Content == "" && o.Verified == nil
}
