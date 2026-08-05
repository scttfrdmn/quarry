package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	quarry "github.com/scttfrdmn/quarry"
	"github.com/scttfrdmn/quarry/provider"
	"github.com/scttfrdmn/quarry/tui"
)

// `quarry run` — the live half of §9.
//
// The order of operations is load-bearing and mirrors P9: parse the caps, build the
// ledger from them, and only then plan. A run that discovered its budget after
// planning would be planning against nothing.

func runCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		capS       = fs.String("cap", "1.00", "spend cap (the contract, P4); required unless --deadline is set")
		floorS     = fs.String("floor", "0.0002", "smallest allocation worth giving a child (§3)")
		deadline   = fs.Duration("deadline", 0, "latency cap; a run may be bound by time instead of money (§3.1)")
		depth      = fs.Int("depth", 3, "max recursion depth — a BACKSTOP, not the design (P2)")
		fake       = fs.Bool("fake", false, "use the built-in fake provider: no credentials, no money, synthetic answers")
		model      = fs.String("model", "us.anthropic.claude-haiku-4-5-20251001-v1:0", "explicit model version, never an alias (P8)")
		region     = fs.String("region", "us-east-1", "AWS region for Bedrock")
		out        = fs.String("out", "", "write the run record here (default: quarry-run-<hash>.json)")
		quiet      = fs.Bool("quiet", false, "no live tree; print the summary only")
		latency    = fs.Duration("fake-latency", 120*time.Millisecond, "per-call delay in --fake mode, so the live tree is watchable")
		scopeS     = fs.String("scope", "", "scope tags as k=v,k=v — carried into every cache key (P6)")
		retries    = fs.Int("retries", 1, "re-solves of a leaf that fails verification (§5)")
		eventsJSON = fs.Bool("events-json", false,
			"emit the framed RunEvent stream as NDJSON on stdout; human output moves to stderr (#9)")
		liveEvents = fs.Bool("live-events", false,
			"with --events-json, also emit per-node enter/exit events AS THEY HAPPEN (#14)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: quarry run [flags] \"<problem statement>\"\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	statement := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if statement == "" {
		fs.Usage()
		return usageErrf("a problem statement is required")
	}

	spend, err := capFlag(*capS)
	if err != nil {
		return err
	}
	floor, err := capFlag(*floorS)
	if err != nil {
		return err
	}
	if !floor.Limited() {
		floor = 0
	}
	scope, err := parseScope(*scopeS)
	if err != nil {
		return err
	}

	caps := quarry.Caps{Spend: spend, Latency: *deadline}
	// P9 enforced at the boundary, with the reason: a run with no cap has nothing to
	// plan against, so this is a design refusal rather than a missing flag.
	if err := caps.Validate(); err != nil {
		return usageErrf("%w\n  planning is budget-conditioned (P9): set --cap or --deadline", err)
	}

	// The clock is read HERE, at the edge, and passed in. The core never calls
	// time.Now() (Go rule 4) — that is what keeps it testable and replayable.
	start := time.Now()
	l, err := quarry.NewLedger(caps, scope)
	if err != nil {
		return err
	}

	e := &quarry.Executor{
		Floor:      floor,
		Now:        start,
		Clock:      time.Now,
		MaxDepth:   *depth,
		MaxRetries: *retries,
		// The mechanical oracle costs ~0 and always runs (§5). It is also what makes P2
		// a real terminator rather than a comment: with no verifier at all, recursion is
		// bounded by depth and budget alone.
		Verifier: quarry.NonEmptyOracle(),
		// Claim extraction feeds CostPerVerifiedClaim — the only cost ratio safe to
		// optimize against, because it has quality in the denominator (§8.2).
		Extractor: quarry.MechanicalExtractor{},
		// A cache within one run gives the DAG behaviour of §2: identical sub-problems
		// resolve to one call. It is in-memory and dies with the process; a persistent
		// store is a different decision (P5 forbids Redis on the floor).
		Cache: quarry.NewMemCache(time.Hour),
	}

	if err := wireSeams(ctx, e, *fake, *model, *region, *latency); err != nil {
		return err
	}

	// D1: --events-json gives the EVENTS stdout and moves every human byte to stderr.
	// The standard Unix contract — one machine-readable stream on fd 1 — rather than
	// events on fd 3, which is harder to consume from every host language for no gain.
	//
	// The redirect is a variable rather than a branch at each print site because there
	// are two of them — the live tree and the summary — and a missed one is not a visible
	// defect: it is a stray line in the middle of a host's NDJSON, which the host reports
	// as a parse error against quarry's contract. Everything else this command writes
	// already goes to stderr (usage, errors), so those two are the whole surface.
	human := io.Writer(os.Stdout)
	if *eventsJSON {
		human = os.Stderr
	}

	// --live-events is meaningless without --events-json, and refusing it is better than
	// picking a destination for it. The events would have to go to stdout, where the human
	// summary already is, so a host would get a stream with prose in the middle — the exact
	// defect D1 exists to prevent. Usage, not a fault: nothing ran.
	if *liveEvents && !*eventsJSON {
		return usageErrf("--live-events requires --events-json\n  live node events share the " +
			"framed stream's stdout, so there is nowhere to put them otherwise (#14 D2)")
	}

	// #14 D2: live node events INTERLEAVE with the folded stream on the same stdout, as
	// additive kinds under StreamVersion 1 — #9 froze "adding a kind is a minor change"
	// and "do not key on line position" for exactly this. A v1 host that does not know
	// them skips them, which the unknown-kind corpus case pins.
	//
	// THE VERSION FRAME MOVES TO RUN START when they are on, and that ordering is the
	// whole reason this block sits above the run rather than beside the fold. A host must
	// be able to REFUSE a stream before it reads anything it would try to parse; a frame
	// written after the first live event arrives is a frame that came too late.
	var live *quarry.NodeStreamObserver
	if *liveEvents {
		if err := quarry.WriteRunEvents(os.Stdout, []quarry.RunEvent{quarry.StreamFrame()}); err != nil {
			return fmt.Errorf("write stream frame: %w", err)
		}
		live = quarry.NewNodeStreamObserver(os.Stdout)
	}

	// The live tree. Wired as an Observer, which sees the run as it happens and can
	// never affect it: the record's bytes are identical whether or not anyone is
	// watching (P8).
	//
	// Both observers run when both are asked for, via MultiObserver — a person watching a
	// terminal and a host reading the pipe are not exclusive, and the human surface is on
	// stderr precisely so they can coexist.
	var view *tui.Tree
	if !*quiet {
		view = tui.New(human)
		view.Start()
	}
	switch {
	case view != nil && live != nil:
		e.Observer = quarry.MultiObserver{live, view}
	case view != nil:
		e.Observer = view
	case live != nil:
		e.Observer = live
	}

	runCtx, cancel := quarry.RootContext(ctx, caps, start)
	defer cancel()

	res, runErr := e.Run(runCtx, quarry.Problem{Statement: statement, Scope: scope}, l)
	if view != nil {
		view.Stop()
	}
	if runErr != nil {
		return explain(runErr)
	}

	rec := quarry.NewRunRecord(res, quarry.Problem{Statement: statement, Scope: scope}, caps, quarry.ModeFresh)
	path := *out
	if path == "" {
		path = fmt.Sprintf("quarry-run-%s.json", rec.RunID[:12])
	}
	if err := writeRecord(path, rec); err != nil {
		return err
	}

	summarize(newPrinter(human), rec, res, path, time.Since(start), deadlineNote(runCtx), *fake)

	// The events go out AFTER the record is written, and the ordering is the contract:
	// the artifact event's url names a file, so a host that read the stream and went
	// looking would race a file that did not exist yet. Emitted from the RECORD rather
	// than from res, because the record is the citable artifact and the stream is a
	// projection of it (§8) — folding res would let the two disagree.
	if *eventsJSON {
		// Provenance is passed only when it is PUBLISHABLE. agate's stability field is a
		// non-nullable float, so an unmeasured estimate reaches a host as a measured 0.0 and
		// badges "nothing replicated" (D3, provenance.go fabricatedZero). Omitting the whole
		// object is the only in-band way to say "unmeasured", and this is the caller the
		// rule is addressed to.
		var pass *quarry.Provenance
		if prov := quarry.ProvenanceOf(rec, nil); prov.StabilityKnown {
			pass = &prov
		}
		// EXACTLY ONE VERSION FRAME PER STREAM (#14 D2). With --live-events the frame went
		// out before the run, so the fold must not write a second one: two contract
		// declarations read as two concatenated streams.
		fold := quarry.HostRunEvents
		if live != nil {
			fold = quarry.HostRunEventsNoFrame
		}
		// A file URL, so the pointer back to the citable record actually resolves for a
		// host on the same machine — which a subprocess supervisor is by construction.
		if err := quarry.WriteRunEvents(os.Stdout, fold(rec, recordURL(path), pass)); err != nil {
			// A write failure here is a FAULT and must not be swallowed. A host reading a
			// truncated stream sees a run that crashed, which is the correct reading — so
			// reporting success while having written half a stream is the one outcome that
			// makes the exit code lie.
			return fmt.Errorf("write event stream: %w", err)
		}
	}

	// A live write failure is reported AFTER the fold, not when it happened. The observer
	// deliberately does not fail the run — an observer that killed the run it observes is
	// what P8's non-perturbation rule forbids — so the record is valid and written, and
	// this is the fault the host needs to hear about. Reported last so the terminal outcome
	// still reaches a host that is still reading.
	if live != nil {
		if err := live.Err(); err != nil {
			return err
		}
	}

	// The exit status, from the SAME classification that went out on the stream (#9 D4).
	//
	// THIS USED TO BE ITS OWN COPY of the precedence — an empty-answer check followed by a
	// gap check — which is how the comment in main.go's exitCode came to say the two
	// orderings "must agree". Two implementations of one rule can only be kept in
	// agreement by remembering to; one implementation cannot disagree with itself. So the
	// exit code and the terminal event are now the same fold, and a host comparing
	// quarry's status against quarry's own stream cannot find them contradicting.
	return statusErr(rec.Classify())
}

// statusErr turns a classified outcome into the sentinel main exits on (#9 D4).
//
// EXHAUSTIVE OVER Outcome AND DELIBERATELY WITHOUT A DEFAULT-TO-NIL. A new outcome that
// nobody mapped must surface as a fault rather than silently exit 0, because exiting 0
// is the one answer a host builds on. That is the same reasoning as exitCode's default,
// at the other end of the same mapping.
//
// CAP-BOUND DEGRADATION RETURNS nil, which is the ruling and not an omission: under the
// standing ruling only TIME produces a gap, so spend exhaustion is planned degradation
// INSIDE authority and a non-zero status would make a cap that worked exactly as P4
// promises look like a malfunction. A host that wants to know reads bound_by off the
// outcome event, which is why that event carries it.
func statusErr(out quarry.Outcome) error {
	switch out {
	case quarry.OutcomeComplete, quarry.OutcomeDegraded:
		return nil
	case quarry.OutcomeNoAnswer:
		// FIRST in Classify's own order, and the reason is the remedy: a run with nothing to
		// show is usually also time-truncated, and telling a host to extend it points at no
		// partial answer to extend. The record is still written and still citable — it
		// faithfully records that nothing was affordable.
		return errNoAnswer
	case quarry.OutcomeTimeTruncated:
		// A distinct status because the remedy is distinct: more time, not more money. This
		// is new — it used to exit 0, indistinguishable from a complete run, so a host could
		// not tell a partial answer from a whole one without parsing prose.
		return errTimeTruncated
	}
	return fmt.Errorf("unclassified run outcome %q: quarry cannot report a status it does "+
		"not have a code for (#9 D4)", out)
}

// recordURL makes the artifact event's pointer resolvable: an absolute file:// URL.
//
// The path alone would be relative to a working directory the host does not know it
// shares, and the artifact event's whole purpose is to lead from the lossy stream back
// to the citable record (P8). Falls back to the bare path if the absolute form is
// unavailable — a pointer that needs interpreting beats no pointer.
func recordURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return "file://" + abs
}

// errNoAnswer marks a run that returned nothing at all. Sentinel rather than a
// formatted error so main can exit non-zero WITHOUT reprinting a message the summary
// already made at length.
var errNoAnswer = errors.New("no answer")

// errTimeTruncated marks a run a deadline cut short — a partial answer with named gaps,
// which §3.1 makes the normal outcome rather than the edge case.
//
// NOT AN ERROR IN THE ORDINARY SENSE, and the sentinel exists to keep it from being
// treated as one. Partial tolerance is the design: the run returned what it had and the
// record names what is missing. It travels as an error only because that is how the CLI
// carries an exit code, and main suppresses its message for exactly that reason — the
// summary has already said it at length, in the sentence a person reads.
var errTimeTruncated = errors.New("time-truncated")

// wireSeams installs the three stochastic seams — planner, solver, reducer.
//
// They are wired TOGETHER because they must agree about what backs them: a fake
// planner over a live solver would spend real money on a mechanical split, and a live
// planner over a fake solver would pay for a decomposition of answers that mean
// nothing. Neither combination is useful, so neither is reachable.
func wireSeams(ctx context.Context, e *quarry.Executor, fake bool, model, region string, latency time.Duration) error {
	if fake {
		fp := &provider.FakeProvider{Latency: latency, Now: time.Now}
		e.Planner = provider.FakePlanner{}
		// BudgetedSolver in the fake branch too, not only the live one. The whole point
		// of --fake is that a path reachable there stays reachable there; wiring the
		// unbudgeted solver here would leave P9's spend-site half exercised only by a run
		// that costs money.
		e.Solver = provider.BudgetedSolver{Provider: fp, Model: "fake"}
		e.Reducer = quarry.ConcatReducer{Sep: "\n"}
		// Keyed on the bare STATEMENT while the solver now sends a wrapped prompt, so the
		// estimate understates the halo by the preamble. Advisory either way (P4) — it
		// sizes admission and is not served as an answer — and the alternative is worse:
		// pricing the real prompt here would duplicate leafPrompt's construction in a
		// second place, where the two could drift apart unnoticed.
		e.Estimate = func(p quarry.Problem) quarry.Units { return fp.Estimate(p.Statement, "fake") }
		return nil
	}

	// Live. Prices must be stated per model: a model absent from the sheet is priced at
	// zero, which would make expensive calls look free.
	prices := map[string]provider.Pricing{
		"us.anthropic.claude-haiku-4-5-20251001-v1:0":  {InputPerMTok: 1.0, OutputPerMTok: 5.0},
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": {InputPerMTok: 3.0, OutputPerMTok: 15.0},
		"us.meta.llama3-3-70b-instruct-v1:0":           {InputPerMTok: 0.72, OutputPerMTok: 0.72},
	}
	if _, priced := prices[model]; !priced {
		// Refusing is right. An unpriced model produces a record whose cost receipt reads
		// zero — a receipt that is not merely imprecise but actively false, and P8 says the
		// record outlives the model.
		return usageErrf("no price sheet for model %q\n  an unpriced model records every call as free, "+
			"which makes the cost receipt a lie (§8). add it to run.go's price table", model)
	}
	p, err := provider.NewBedrockProvider(ctx, region, prices)
	if err != nil {
		return fmt.Errorf("build bedrock provider (is AWS_PROFILE set?): %w", err)
	}
	e.Planner = provider.NewBedrockPlanner(p, model)
	// BudgetedSolver, not ProviderSolver: the leaf is the only thing that spends money,
	// so it is where P9 has to hold. Its allocation reaches the model as a word budget
	// and as a token ceiling sized from this price sheet (provider/solver.go).
	e.Solver = provider.BudgetedSolver{Provider: p, Model: model}
	// Planner and reducer are DIFFERENT agents by design (§2): the reducer must see what
	// returned without inheriting the priors that produced the split. Same provider,
	// separate call, separate prompt.
	e.Reducer = provider.NewBedrockReducer(p, model)
	e.Estimate = func(prob quarry.Problem) quarry.Units { return p.Estimate(prob.Statement, model) }
	return nil
}

// parseScope reads k=v,k=v into a Scope. Scope tags are part of every cache key
// (P6), so they are a correctness input, not a label.
func parseScope(s string) (quarry.Scope, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return quarry.Scope{}, nil
	}
	tags := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(k) == "" {
			return quarry.Scope{}, usageErrf("--scope %q: want k=v,k=v", s)
		}
		tags[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return quarry.Scope{Tags: tags}, nil
}

// writeRecord persists the CANONICAL bytes — the hashed artifact — not a re-encoding.
// Writing pretty-printed JSON would produce a file whose hash does not match the
// RunID it contains, which is the whole identity of the record (P8).
func writeRecord(path string, rec quarry.RunRecord) error {
	b, err := rec.Canonical()
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	return nil
}

// summarize prints the receipt: the answer, what it cost, and how much to trust it.
//
// THE ORDER IS THE ARGUMENT. The answer is one field in a record (§8), so the trust
// summary and the cost sit beside it rather than below a fold. A CLI that printed the
// prose and stopped would be the slot machine §9 is written against.
func summarize(w *printer, rec quarry.RunRecord, res quarry.Result, path string, elapsed time.Duration, note string, fake bool) {
	w.println()
	if fake {
		w.println("── FAKE RUN — no model was called. shape, cost and provenance are real; " +
			"the answers are synthetic ──")
	}
	w.printf("answer:\n%s\n\n", indent(res.Answer.Content, "  "))

	// The caveat goes ABOVE the numbers and immediately below the prose, because that is
	// where it is read. Two sources, and the second was missing: `note` comes from the
	// root CONTEXT, which only expires if the run as a whole ran out of time — but a
	// deadline usually cuts CHILDREN while the root finishes comfortably inside it. A
	// 10-node run with 3 gaps printed its partial answer with no caveat at all, and the
	// only hint was a "gaps" line five rows further down, after the cost. The record
	// knows; ask it.
	if note == "" && len(rec.Gaps()) > 0 {
		note = fmt.Sprintf("PARTIAL: %s did not finish in time. this answer covers part of the "+
			"question — the missing nodes are named in the record (§3.1)",
			plural(len(rec.Gaps()), "node", "nodes"))
	}
	if note != "" {
		w.printf("! %s\n\n", note)
	}

	prov := quarry.ProvenanceOf(rec, nil)
	w.printf("cost      %s of %s   (%s, %s)\n",
		rec.TotalCost(), rec.Caps.Spend, plural(len(rec.Outcomes), "node", "nodes"), elapsed.Round(time.Millisecond))
	if cpc, ok := rec.CostPerVerifiedClaim(); ok {
		// The only ratio safe to report: cost per RUN is trivially gamed by doing less.
		w.printf("          %s per verified claim\n", cpc)
	}
	w.printf("verified  %d of %d nodes; %s\n",
		prov.Verified, len(rec.Outcomes), plural(prov.Unverified, "node unverified", "nodes unverified"))

	// Stability is ABSENT rather than zero for a single run. A stable-claim fraction is
	// not defined for n=1 (P7), and printing 0.0 would read as "measured, nothing
	// replicated" — silence converted into a finding.
	if prov.StabilityKnown {
		w.printf("stability %.0f%% of claims stable\n", prov.Stability*100)
	} else {
		w.printf("stability not measured — one run is one sample, not a distribution (P7)\n")
	}

	if gaps := rec.Gaps(); len(gaps) > 0 {
		w.printf("gaps      %s truncated by time (§3.1)\n", plural(len(gaps), "node", "nodes"))
	}
	// Truncation is BROADER than gaps and must be said here, not only by `quarry show`.
	// Under the standing ruling only TIME is a gap, so a run priced out of every child
	// has no gaps at all — and this summary printed "cost 0.0000 of 0.0001" beside an
	// empty answer and exited 0, which reads as a cheap success. Truncated() is exactly
	// the predicate that catches it, and show was already using it; the two views must
	// not disagree about whether a run finished.
	if rec.Truncated() && len(rec.Gaps()) == 0 {
		w.println("truncated this run stopped short of what it set out to do — the cap bit " +
			"before the work was done (§8.1)")
	}
	if len(res.Plan.Excluded) > 0 {
		// P9: degradation is disclosed, not discovered. This is what the cap could not
		// cover, named before the user goes looking for it.
		w.printf("excluded  the cap did not cover %s:\n", plural(len(res.Plan.Excluded), "sub-problem", "sub-problems"))
		for _, x := range res.Plan.Excluded {
			w.printf("            · %s\n", x)
		}
	}
	if rec.BoundBy != "" {
		w.printf("bound by  %s\n", rec.BoundBy)
	}
	w.printf("\nrecord    %s  (%s)\n", path, rec.RunID[:12])
	w.printf("          quarry show %s\n", path)
}

// indent prefixes every line, so a multi-line answer stays visibly one block.
func indent(s, pre string) string {
	if s == "" {
		return pre + "(empty)"
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pre + l
	}
	return strings.Join(lines, "\n")
}

// jsonOut writes a value as indented JSON, for the --json paths in show/replay.
// Distinct from writeRecord: this one is for HUMAN reading, so it is pretty-printed
// and explicitly NOT the hashed form.
func jsonOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
