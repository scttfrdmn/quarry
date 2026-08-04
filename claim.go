package quarry

import (
	"context"
	"regexp"
	"strings"
)

// Claim extraction and equivalence (build step 6, §7). THE highest-risk unbuilt
// piece: without claim-level equivalence, "these runs agree" is a percentage
// attached to vibes, and everything in §7 (replicate independence, stability,
// the citation receipt) assumes it works.
//
// What lives here is a deliberately MECHANICAL spike — no model, no network, in
// keeping with the no-AWS/no-LLM discipline of steps 1-7. It splits a result's
// content into claim strings and normalizes each for comparison. That is
// arithmetic over text, not inference, so it is deterministic and replays
// byte-for-byte (P8) — but it only catches agreement that survives lowercasing,
// punctuation-stripping and whitespace collapse. Two conclusions that agree in
// MEANING but differ in WORDING are not matched. See the TODO on Equivalent.
//
// The real thing — semantic claim equivalence with source spans — is the one
// seam most likely to be implemented in Python behind a service boundary, where
// the embedding/NLI tooling lives (§12). This interface is kept narrow (Extract
// + Equivalent, both stateless) precisely so that hop stays cheap. Expect the
// Claim shape to change once that prototype exists; this is a spike, not a
// feature.

// MechanicalExtractor is the reference ClaimExtractor: a pure, model-free
// implementation of the §7 seam.
//
// It is configurable at the two points where a mechanical spike differs from the
// eventual model: how content is cut into candidate claims (Split) and what
// counts as the same claim (Normalize). Both default to sentence-level heuristics
// and are swappable so a caller can narrow the spike to a structured format
// without changing the wiring.
type MechanicalExtractor struct {
	// Split cuts content into candidate claim strings. Nil means SentenceSplit.
	// Must be a pure function of its input for replay determinism (P8).
	Split func(content string) []string

	// Normalize maps a claim's text to the canonical form two claims must share
	// to be judged equivalent. Nil means NormalizeText. Must be pure.
	Normalize func(text string) string
}

// Extract cuts a sample's content into claims, one per non-empty segment,
// deduplicated by normalized form and kept in first-occurrence order.
//
// The normalized form is stored on each Claim (Claim.Norm) rather than only
// recomputed at comparison time, so the record pins the normalization that was
// applied (P8): equivalence stays reproducible even if the normalizer later
// changes, and a downstream comparator — possibly across the Python service
// boundary — can match claims with a bare string compare and no second hop.
//
// Never errors in this mechanical form; the error return exists for the model
// implementation that will replace it.
func (m MechanicalExtractor) Extract(_ context.Context, s Sample, nodeID string) ([]Claim, error) {
	split := m.Split
	if split == nil {
		split = SentenceSplit
	}
	norm := m.Normalize
	if norm == nil {
		norm = NormalizeText
	}

	var claims []Claim
	seen := make(map[string]bool)
	for _, seg := range split(s.Content) {
		text := strings.TrimSpace(seg)
		if text == "" {
			continue
		}
		key := norm(text)
		if key == "" || seen[key] {
			// Empty-after-normalization (pure punctuation) carries no assertion;
			// a repeat within one result is one claim, not two (DAG spirit, §2).
			continue
		}
		seen[key] = true
		// TODO(§7, §12): Sources is unpopulated. Source spans are the undesigned
		// half of the claim format — a mechanical splitter has no retrieval to
		// attribute to. They arrive with the citation receipt's extraction, which
		// is the model/Python piece.
		claims = append(claims, Claim{Text: text, Norm: key, NodeID: nodeID})
	}
	return claims, nil
}

// Equivalent reports whether two claims assert the same thing.
//
// Mechanically that is equality of normalized forms: it compares the pinned
// Claim.Norm when both carry one (so a comparison over recorded claims uses the
// normalization that produced them, not today's), and re-normalizes Text
// otherwise. Empty normalized forms are never equivalent — there is nothing to
// agree on.
//
// TODO(§7): this is normalized-STRING equality, not SEMANTIC equivalence. "Prices
// rose in Q3" and "there was a third-quarter price increase" are the same
// conclusion and this returns false for them. Real claim-level equivalence needs
// the embedding/NLI tooling that lives in Python (§12); until then, stability
// estimates built on this UNDERCOUNT agreement — which is the safe direction to
// be wrong in, the same way the cache key under-hits (types.go Problem.Key).
func (m MechanicalExtractor) Equivalent(a, b Claim) bool {
	norm := m.Normalize
	if norm == nil {
		norm = NormalizeText
	}
	na := a.Norm
	if na == "" {
		na = norm(a.Text)
	}
	nb := b.Norm
	if nb == "" {
		nb = norm(b.Text)
	}
	if na == "" || nb == "" {
		return false
	}
	return na == nb
}

// sentenceBoundary is the mechanical claim delimiter: sentence terminators and
// line breaks. A period inside a decimal or abbreviation over-splits — an
// acceptable spike limitation, since over-splitting under-hits equivalence
// rather than falsely merging distinct claims.
var sentenceBoundary = regexp.MustCompile(`[.!?;\n]+`)

// SentenceSplit is the default Split: break content at sentence terminators and
// newlines. The delimiters are dropped; empty segments are handled by Extract.
func SentenceSplit(content string) []string {
	return sentenceBoundary.Split(content, -1)
}

// nonAlnum matches any run of characters that are neither Unicode letters nor
// digits — the separators normalization folds to a single space.
var nonAlnum = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// NormalizeText is the default Normalize: lowercase, fold every run of
// non-alphanumeric characters to one space, and trim. This erases case,
// punctuation and whitespace differences while PRESERVING word order — "dog
// bites man" and "man bites dog" are distinct claims, so token order is
// deliberately not sorted away.
func NormalizeText(text string) string {
	lower := strings.ToLower(text)
	collapsed := nonAlnum.ReplaceAllString(lower, " ")
	return strings.TrimSpace(collapsed)
}

var _ ClaimExtractor = MechanicalExtractor{}
