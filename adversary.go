package quarry

import (
	"context"
	"sort"
)

// Adversarial passes and the surplus-budget policy (build step 9, §3 Surplus,
// §5). Two ideas that meet here:
//
//   - Adversarial verification is the high rung of the §5 ladder. Where a
//     Verifier confirms, an Adversary REFUTES — and it is asymmetric: one hit is
//     enough, so it is scored on defects found. Its independence is load-bearing
//     (§5): route it through a DIFFERENT provider family than produced the claim,
//     or same-family error correlation makes the pass theatre. The core names the
//     requirement (seams.go); the wiring enforces it (provider/).
//
//   - Surplus (§3). A run that finishes UNDER cap does not let the remainder
//     evaporate — it spends it on adversarial passes over the HIGHEST-EXPOSURE
//     claims. Budget converts to quality rather than vanishing, and it is active
//     work inside the already-authorized ceiling, so it is consistent with P5.
//
// Exposure is P3 made concrete: verification spend proportional to downstream
// exposure. A claim from a node many others depend on, or from an UNVERIFIED
// node, is worth attacking first. Nothing here reads the clock or dials a
// network — the Adversary seam does that, behind the interface (Go rule 4).

// AdversarialFinding records one adversarial pass for the receipt (§8). A run
// record gains a list of these; a found defect is the signal a refine should act
// on, alongside the unstable-claim list from §7.
type AdversarialFinding struct {
	Claim  Claim
	Broke  bool   // an attack located a defect — the asymmetric win (§5)
	Detail string // human-facing note
	Cost   Units
	By     string // adversary name; the receipt says who attacked and where the regress stopped (§5)
}

// ExposureOf scores a claim's downstream exposure for surplus targeting (P3).
//
// Higher means attack-first. Two signals, both already in the record: a claim
// from a node with more dependents carries more weight downstream, and a claim
// from an UNVERIFIED node has had nothing check it yet, so an adversarial pass is
// the first scrutiny it gets. The node is located by Claim.NodeID.
//
// Deterministic and content-free — it reads structure, not text — so surplus
// targeting is reproducible and does not smuggle content into a decision (§8.2).
func ExposureOf(c Claim, outcomes []NodeOutcome) int {
	var self *NodeOutcome
	dependents := 0
	for i := range outcomes {
		o := &outcomes[i]
		if o.NodeID == c.NodeID {
			self = o
		}
		for _, child := range o.Children {
			if child == c.NodeID {
				dependents++
			}
		}
	}
	score := dependents
	if self != nil && self.Verified == nil {
		// An unverified node has had no scrutiny; weight it up so surplus reaches
		// it before claims a verifier already passed (P3).
		score += 2
	}
	return score
}

// SurplusPlan orders a run's claims by exposure and greedily selects those an
// adversarial budget can afford, most-exposed first (§3 Surplus, P3). It spends
// NOTHING — it decides what to attack, given a budget; RunSurplus does the
// spending. Kept separate so the choice is testable without a live adversary and
// visible at a gate before any spend, exactly like the plan gate (§9).
//
// budget is the surplus available (typically the completed run's leftover
// balance). adv.Estimate sizes each candidate; selection stops when the next
// most-exposed claim will not fit, rather than skipping ahead to a cheaper one —
// exposure order is the point, and reordering to pack the budget would defeat P3.
func SurplusPlan(claims []Claim, outcomes []NodeOutcome, adv Adversary, budget Units) []Claim {
	type scored struct {
		claim    Claim
		exposure int
		order    int // original index — stable tie-break, no clock, no content (P8)
	}
	ranked := make([]scored, len(claims))
	for i, c := range claims {
		ranked[i] = scored{claim: c, exposure: ExposureOf(c, outcomes), order: i}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].exposure != ranked[b].exposure {
			return ranked[a].exposure > ranked[b].exposure // most exposed first (P3)
		}
		return ranked[a].order < ranked[b].order
	})

	var selected []Claim
	remaining := budget
	for _, r := range ranked {
		cost := adv.Estimate(r.claim)
		if remaining.Limited() {
			if cost > remaining {
				break // most-exposed-first: stop at the first claim that will not fit
			}
			remaining -= cost
		}
		selected = append(selected, r.claim)
	}
	return selected
}

// RunSurplus attacks the selected claims within the ledger's remaining balance
// and returns the findings for the receipt (§3 Surplus, §8).
//
// Each attack passes admission first (Budget(Retry(agent)) order, §3): surplus is
// active spend inside the authorized ceiling, never a bypass of it. A claim that
// cannot be afforded ends the pass — the remaining claims are simply not attacked,
// which is planned degradation, not a gap (only time is a gap; the standing
// ruling). A claim the adversary cannot assess (ok=false) is recorded as a
// non-breaking finding so the receipt can still say it was reached.
//
// Independence is the adversary's responsibility: this driver does not know which
// family produced the claim. Wiring an adversary from the SAME family as the
// solver satisfies the types and violates §5 — the check that matters is in
// provider/, not here.
func RunSurplus(ctx context.Context, l *Ledger, adv Adversary, claims []Claim, samples map[string]Sample) []AdversarialFinding {
	var findings []AdversarialFinding
	for _, c := range claims {
		est := adv.Estimate(c)
		if err := l.Admit(ctx, est); err != nil {
			// Cannot afford (or time expired) — stop. Unattacked claims are left
			// unattacked, disclosed by their absence from the findings list.
			break
		}
		found, detail, cost, ok := adv.Attack(ctx, c, samples[c.NodeID])
		if cost.Limited() && cost > 0 {
			_ = l.Debit(ctx, cost)
		}
		if !ok {
			findings = append(findings, AdversarialFinding{Claim: c, Broke: false, Detail: "not assessable", Cost: cost, By: adv.Name()})
			continue
		}
		findings = append(findings, AdversarialFinding{Claim: c, Broke: found, Detail: detail, Cost: cost, By: adv.Name()})
	}
	return findings
}
