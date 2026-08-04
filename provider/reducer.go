package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	quarry "github.com/scttfrdmn/quarry"
)

// The model-backed Reducer. §2 requires it be a DIFFERENT AGENT from the planner —
// "it needs to see what returned without inheriting the priors that produced the
// split" — and that requirement is why this is a separate type with its own model
// field rather than a second method on BedrockPlanner. Sharing one instance would
// make the separation a comment; separate types make it structural.
//
// The reducer does two different jobs depending on the strategy it is handed (§2):
// it MERGES a partition's differing sub-answers, and it SELECTS one arm of a
// portfolio's competing attempts. These are not variations on a theme — merging N
// attempts at one question yields N answers — so they get different prompts, and
// selection is asked to name an index rather than to write prose.

// ErrUnparseableSelection is returned when a selection reply names no arm. Distinct
// from ErrUnparseablePlan because the failure is recoverable in a way a bad plan is
// not: the arms already exist and one can be chosen mechanically.
var ErrUnparseableSelection = fmt.Errorf("reducer reply names no arm")

// BedrockReducer implements quarry.Reducer with one Converse call.
//
// PARTIAL TOLERANCE IS THE CONTRACT (§3.1): whatever children exist must fold into
// a returnable answer now. You can stop spending, but you cannot stop time. So a
// missing child is never an error here, and when the input is partial the prompt
// says so — an answer built from three of five sub-answers should be hedged, and
// only the reducer is in a position to hedge it.
type BedrockReducer struct {
	Provider *BedrockProvider

	// Model is the reducer's model. It MAY be the same ID as the planner's — §2
	// requires a different AGENT (a separate call with no shared priors), not
	// necessarily different weights. Independence of MODEL FAMILY is a §5
	// requirement for JUDGING, which is the adversary's job, not the reducer's.
	Model string

	// SelectFallback, when true, falls back to the mechanical quarry.SelectReducer if
	// the model's selection reply cannot be parsed. Off by default: a silent fallback
	// hides a broken selector, and the mechanical rule is weak (first verified arm),
	// so a caller should opt into it knowingly.
	SelectFallback bool
}

// NewBedrockReducer wires a reducer to a provider and an explicit model version (P8).
func NewBedrockReducer(p *BedrockProvider, model string) *BedrockReducer {
	return &BedrockReducer{Provider: p, Model: model}
}

// Reduce merges or selects, per the strategy.
func (br *BedrockReducer) Reduce(ctx context.Context, p quarry.Problem, children []quarry.NodeOutcome, alloc quarry.Allocation, partial bool, strategy quarry.Strategy) (quarry.Sample, error) {
	if br.Provider == nil {
		return quarry.Sample{}, fmt.Errorf("reducer has no provider")
	}

	// Usable children only. A gapped or empty child contributes nothing and must not
	// abort the merge (§3.1) — but it DOES mean the result is partial, so the flag is
	// raised here even if the executor did not set it. The executor sets it from tree
	// state; this catches the same condition from the reducer's own view of the input.
	usable := make([]quarry.NodeOutcome, 0, len(children))
	for _, c := range children {
		if c.Gap || strings.TrimSpace(c.Content) == "" {
			partial = true
			continue
		}
		usable = append(usable, c)
	}

	// Nothing came back. Spending a model call to summarize an empty set would bill
	// the run for producing nothing; an empty sample is the honest result and the
	// parent records the gap (§3.1).
	if len(usable) == 0 {
		return quarry.Sample{}, nil
	}

	if strategy == quarry.StrategyPortfolio {
		return br.selectArm(ctx, p, usable, partial)
	}
	return br.merge(ctx, p, usable, partial)
}

// merge folds a partition's sub-answers into one answer.
func (br *BedrockReducer) merge(ctx context.Context, p quarry.Problem, usable []quarry.NodeOutcome, partial bool) (quarry.Sample, error) {
	prompt := buildMergePrompt(p, usable, partial)
	s, err := br.Provider.Complete(ctx, prompt, br.Model, p.Scope)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("reduce call: %w", err)
	}
	return s, nil
}

// selectArm picks one arm of a portfolio by asking the model for an index.
//
// An INDEX, not prose: a selector asked to "give the best answer" will rewrite it,
// which silently converts selection into generation — the run would then be billed
// for a synthesis nobody planned, and the returned answer would match no arm's
// recorded content, breaking the link between the answer and the node that produced
// it. Naming an index keeps the choice auditable.
func (br *BedrockReducer) selectArm(ctx context.Context, p quarry.Problem, usable []quarry.NodeOutcome, partial bool) (quarry.Sample, error) {
	prompt := buildSelectPrompt(p, usable)
	s, err := br.Provider.Complete(ctx, prompt, br.Model, p.Scope)
	if err != nil {
		return quarry.Sample{}, fmt.Errorf("select call: %w", err)
	}

	idx, ok := parseIndex(s.Content, len(usable))
	if !ok {
		if br.SelectFallback {
			// The mechanical rule, and the model call is still CHARGED: the money was
			// spent whether or not the reply was usable, and hiding that would make the
			// ledger wrong in the direction that flatters the system.
			fb, ferr := quarry.SelectReducer{}.Reduce(ctx, p, usable, quarry.Allocation{}, partial, quarry.StrategyPortfolio)
			if ferr != nil {
				return quarry.Sample{}, ferr
			}
			fb.Cost = s.Cost
			fb.Model, fb.ModelVersion = s.Model, s.ModelVersion
			fb.HaloTokens, fb.GeneratedTokens = s.HaloTokens, s.GeneratedTokens
			return fb, nil
		}
		return quarry.Sample{}, fmt.Errorf("%w: %.120q", ErrUnparseableSelection, s.Content)
	}

	// The chosen arm's CONTENT with the selection call's COST. The arm's own spend is
	// already recorded on the arm's node; charging it again here would double-count it
	// in the tree total. What this node legitimately costs is the selection itself.
	out := s
	out.Content = usable[idx].Content
	return out, nil
}

func buildMergePrompt(p quarry.Problem, children []quarry.NodeOutcome, partial bool) string {
	var b strings.Builder
	b.WriteString("You are a research synthesizer. Below is a QUESTION and the answers to its ")
	b.WriteString("sub-questions, each answered independently. Combine them into a single coherent ")
	b.WriteString("answer to the QUESTION.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Use ONLY what the sub-answers state. Do not add facts they do not contain.\n")
	b.WriteString("- Where two sub-answers conflict, say so rather than silently picking one.\n")
	if partial {
		// The honesty requirement of §3.1 and §8: a degraded answer must be visibly
		// degraded. The reducer is the only component positioned to hedge the prose,
		// and an unhedged partial answer reads exactly like a complete one.
		b.WriteString("- SOME SUB-ANSWERS ARE MISSING: the work was truncated before finishing. ")
		b.WriteString("Answer with what is present AND state plainly which aspects are unaddressed. ")
		b.WriteString("Do not present the result as complete.\n")
	}
	b.WriteString("\nQUESTION:\n")
	b.WriteString(p.Statement)
	b.WriteString("\n\nSUB-ANSWERS:\n")
	for i, c := range children {
		b.WriteString("\n[")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("] SUB-QUESTION: ")
		b.WriteString(c.Problem.Statement)
		b.WriteString("\n    ANSWER: ")
		b.WriteString(c.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func buildSelectPrompt(p quarry.Problem, arms []quarry.NodeOutcome) string {
	var b strings.Builder
	b.WriteString("Below is a QUESTION and several independent ATTEMPTS to answer it. ")
	b.WriteString("Choose the single best attempt.\n\n")
	b.WriteString("Reply with ONLY the number of the best attempt (for example: 2). ")
	b.WriteString("Do not rewrite, combine, or improve any attempt — select one as it stands.\n\n")
	b.WriteString("QUESTION:\n")
	b.WriteString(p.Statement)
	b.WriteString("\n\nATTEMPTS:\n")
	for i, c := range arms {
		b.WriteString("\n[")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("] ")
		b.WriteString(c.Content)
		b.WriteString("\n")
		// A verified arm is flagged, because that is real evidence the selector should
		// weigh and it cannot see verdicts otherwise. Unverified is left unsaid rather
		// than labelled: "not checked" and "checked and failed" are different facts
		// (§8), and a failed arm never reaches here as a usable arm anyway.
		if c.Verified != nil && *c.Verified {
			b.WriteString("    (this attempt passed an independent check)\n")
		}
	}
	b.WriteString("\nBest attempt number: ")
	return b.String()
}

// parseIndex reads a 1-based arm number from a reply and converts it to a 0-based
// index. Returns ok=false when the reply names no valid arm — including an
// out-of-range number, which means the selector answered about arms that do not
// exist and its choice cannot be honoured.
//
// Scans for the FIRST run of digits rather than requiring a bare number: replies
// arrive as "2", "2.", "Attempt 2" and "[2]", and all four name the same arm
// unambiguously. It stops well short of prose interpretation — "the second one"
// returns ok=false, because inferring an index from words is guessing.
func parseIndex(reply string, n int) (int, bool) {
	if n == 0 {
		return 0, false
	}
	start := -1
	for i, r := range reply {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	end := start
	for end < len(reply) && reply[end] >= '0' && reply[end] <= '9' {
		end++
	}
	v, err := strconv.Atoi(reply[start:end])
	if err != nil || v < 1 || v > n {
		return 0, false
	}
	return v - 1, true
}

var _ quarry.Reducer = (*BedrockReducer)(nil)
