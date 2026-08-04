package quarry

import (
	"context"
	"sort"
)

// Order-independent claim clustering under a comparator that may be
// NON-TRANSITIVE, may cost money, and may fail (§7).
//
// THE DEFECT THIS FIXES, found by probing before the comparator existed. Stability
// compares each claim only against a cluster's REPRESENTATIVE and joins the first
// match. That is single-link clustering, and it is sound only if equivalence is
// transitive. Normalized-string equality is transitive, so no existing test could
// see the problem; NO model-backed comparator ever will be. The defect therefore
// activates at exactly the moment the comparator is upgraded — the failure mode
// this package keeps finding, where a change makes a latent assumption load-bearing.
//
// Concretely, with a ~ b, b ~ c, a !~ c, three replicates asserting a, b, c report:
//
//	order [a b c] -> 2 clusters, nothing stable
//	order [b a c] -> 1 cluster,  UNANIMOUS AND STABLE
//	order [c b a] -> 2 clusters, nothing stable
//
// Same three replicates, same comparator, opposite scientific conclusion, decided
// by the order the records happened to be passed in. Replicate order is an artifact
// of iteration and must carry no information (P7: the replicates are exchangeable
// draws). And the direction of the error is the dangerous one — the middle case
// reports unanimous agreement that no comparator ever asserted.
//
// TWO CHANGES MAKE IT ORDER-INDEPENDENT:
//
//  1. COMPLETE LINK, not single link. A claim joins a cluster only if it is
//     equivalent to EVERY member, not to the representative. Under a transitive
//     relation the two agree exactly, so the free path is unchanged; under a
//     non-transitive one, complete link cannot chain a into c through b. It is also
//     the conservative choice, which is the direction §7 requires: it splits
//     borderline clusters and so UNDERCOUNTS stability, never inflates it.
//
//  2. A CANONICAL PRE-PASS. Claims are first grouped by pinned normalized form —
//     free, exact, and order-independent by construction — and the comparator then
//     runs over GROUPS in a canonical order (sorted by normalized form), never in
//     encounter order. So the paid comparator sees the same pairs in the same
//     sequence no matter how the records were assembled.
//
// Complete link is O(members) per candidate cluster, which is why the pre-pass
// matters economically: comparisons run between distinct WORDINGS, not between
// claims, so k canonical forms cost O(k²) paid calls at worst and identical
// replicates cost none at all.

// ClusterResult is one equivalence cluster with its support and what it cost.
type ClusterResult struct {
	// Rep is the cluster's representative: the claim with the lexicographically
	// smallest normalized form, NOT the first encountered. A canonical choice, so the
	// representative — which is what a refine reads and a receipt cites — does not
	// depend on record order either.
	Rep Claim

	// Members are every claim in the cluster, in canonical order.
	Members []Claim

	// Replicates is the set of replicate indices that asserted a member, as a sorted
	// slice. Support is len(Replicates): DISTINCT replicates, so a run repeating
	// itself across nodes never inflates its own agreement (P7).
	Replicates []int
}

// Support is the number of distinct replicates asserting a member of this cluster.
func (c ClusterResult) Support() int { return len(c.Replicates) }

// ClusterClaims groups claims from independent replicates into equivalence clusters,
// order-independently, metering what the comparator spent (§7).
//
// claimsByReplicate[i] is replicate i's full claim list. cmp may be nil, which uses
// the free MechanicalComparator — and note what that means for the result: the free
// comparator reports ok=false on every non-match, so with no paid rung wired, claims
// of differing wording are left in separate clusters and counted as UNASSESSED
// rather than as disagreements. unassessed is returned rather than folded into the
// clusters, because "we could not tell" is not a scientific finding and must not be
// laundered into one.
//
// Budget is enforced through the ledger, not by a comparison count: comparison is
// spend like any other (P4), so each paid pair is SIZED by Estimate and passes Admit
// BEFORE the call, and an unaffordable comparison ENDS the pass. What has been
// compared so far is kept — a partial clustering is a real result, provided it says it
// is partial, which is what exhausted reports. Pass a nil ledger for the free path; a
// nil ledger with a paid comparator would spend outside any cap, so it is refused
// rather than allowed.
//
// This paragraph described admit-first behaviour for a while before the code did it:
// the first implementation called Compare and then Debit, and a live run against a
// 1-micro-unit cap spent 200 while reporting Truncated. Worth naming because it is the
// inverse of the usual drift — the doc was right and the code was wrong, so reading
// the comment would have confirmed a guarantee that was not being met.
func ClusterClaims(
	ctx context.Context,
	claimsByReplicate [][]Claim,
	cmp ClaimComparator,
	l *Ledger,
) (clusters []ClusterResult, unassessed int, cost Units, exhausted bool) {
	if cmp == nil {
		cmp = MechanicalComparator{}
	}

	// ---- 1. the free, exact, order-independent pre-pass ----------------------
	//
	// Group by pinned normalized form. Equal norms are the same claim with no model
	// consulted (see equiv.go), so this collapses the bulk of the work for nothing
	// and does it identically regardless of input order.
	type group struct {
		norm    string
		claims  []Claim
		reps    map[int]bool
		members int
	}
	byNorm := map[string]*group{}
	var norms []string
	normalize := NormalizeText
	for ri, claims := range claimsByReplicate {
		for _, c := range claims {
			key := claimNorm(c, normalize)
			if key == "" {
				continue // punctuation-only: no assertion to cluster (claim.go)
			}
			g, ok := byNorm[key]
			if !ok {
				g = &group{norm: key, reps: map[int]bool{}}
				byNorm[key] = g
				norms = append(norms, key)
			}
			g.claims = append(g.claims, c)
			g.reps[ri] = true
			g.members++
		}
	}
	// Canonical order over groups. Everything downstream — which pairs the
	// comparator sees, in what sequence, and which claim represents a cluster — is
	// determined from here, so this sort is what makes the whole function
	// order-independent (and byte-stable for replay, P8).
	sort.Strings(norms)

	// ---- 2. complete-link agglomeration over GROUPS ---------------------------
	type cl struct {
		groups []*group
	}
	var built []*cl

	for _, n := range norms {
		g := byNorm[n]
		placed := false
		for _, c := range built {
			// COMPLETE LINK: g joins c only if equivalent to every group already in it.
			// One non-match or one unassessable pair disqualifies the whole cluster, which
			// is what keeps a non-transitive relation from chaining.
			all := true
			for _, member := range c.groups {
				if exhausted {
					all = false
					break
				}
				// ADMIT BEFORE SPENDING, in Budget(Retry(agent)) order (§3) — the order
				// RunSurplus follows. An earlier draft called Compare and then Debit, which
				// is not admission control at all: a live run against a 1-micro-unit cap
				// spent 200 and reported Truncated afterwards. Disclosing an overrun is not
				// the same as not overrunning, and P4 makes the cap the contract.
				est := cmp.Estimate(g.claims[0], member.claims[0])
				if est > 0 && l == nil {
					// A paid comparator with no ledger would spend outside every cap.
					exhausted = true
					all = false
					break
				}
				if est > 0 {
					if err := l.Admit(ctx, est); err != nil {
						// Cannot afford the next comparison (or time expired) — stop before
						// spending, leaving the clustering partial and saying so.
						exhausted = true
						all = false
						break
					}
				}
				eq, ok, spent := cmp.Compare(ctx, g.claims[0], member.claims[0])
				cost += spent
				if spent > 0 && l != nil {
					// The estimate authorized the call; the ACTUAL is what gets debited. A
					// refusal here means the actual exceeded what the estimate cleared — the
					// money is spent either way, so the pass ends. The returned cost reports
					// the actual regardless of whether the ledger could absorb it, which is
					// the same convention RunSurplus and solveLeaf follow for a post-call
					// overrun: the receipt must not be flattered by an accounting refusal.
					if err := l.Debit(ctx, spent); err != nil {
						exhausted = true
						all = false
						break
					}
				}
				if !ok {
					unassessed++
					all = false
					break
				}
				if !eq {
					all = false
					break
				}
			}
			if all {
				c.groups = append(c.groups, g)
				placed = true
				break
			}
		}
		if !placed {
			built = append(built, &cl{groups: []*group{g}})
		}
	}

	// ---- 3. canonical output --------------------------------------------------
	for _, c := range built {
		var members []Claim
		reps := map[int]bool{}
		for _, g := range c.groups {
			members = append(members, g.claims...)
			for ri := range g.reps {
				reps[ri] = true
			}
		}
		// Members in canonical order: normalized form, then text, so two claims with
		// the same norm but different surface wording still order deterministically.
		sort.SliceStable(members, func(i, j int) bool {
			ni, nj := claimNorm(members[i], normalize), claimNorm(members[j], normalize)
			if ni != nj {
				return ni < nj
			}
			return members[i].Text < members[j].Text
		})
		riList := make([]int, 0, len(reps))
		for ri := range reps {
			riList = append(riList, ri)
		}
		sort.Ints(riList)
		clusters = append(clusters, ClusterResult{
			Rep:        members[0], // smallest normalized form — canonical, not first-seen
			Members:    members,
			Replicates: riList,
		})
	}
	// Clusters ordered by their representative's normalized form: a total order over
	// content, independent of how the replicates were assembled.
	sort.SliceStable(clusters, func(i, j int) bool {
		return claimNorm(clusters[i].Rep, normalize) < claimNorm(clusters[j].Rep, normalize)
	})
	return clusters, unassessed, cost, exhausted
}
