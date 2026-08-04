package quarry

import (
	"context"
	"testing"
	"time"
)

// These tests ARE the specification for build step 3 (§2 recursion/DAG, §6
// cache). A failing test means the design changed — amend docs/design.md in the
// same commit or revert.

// ------------------------------------------------------------- recursion

func TestRecursionStopsAtMaxDepth(t *testing.T) {
	// A StaticPlanner returns the same 2-way split at every node, so without a
	// backstop it would recurse forever. MaxDepth is that backstop (P2): a
	// balanced binary tree of depth d has 2^(d+1)-1 nodes.
	prov := &fakeProvider{cost: 0} // free, so budget never terminates it
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.MaxDepth = 3

	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	// depths 0,1,2 branch (internal), depth 3 are leaves: 1+2+4+8 = 15 nodes.
	if len(res.Outcomes) != 15 {
		t.Errorf("want 15 nodes for a depth-3 binary tree, got %d", len(res.Outcomes))
	}
	// Every deepest node is a max-depth base case.
	leaves := 0
	for _, o := range res.Outcomes {
		if o.Depth == 3 {
			leaves++
			if o.BaseCase != BaseMaxDepth {
				t.Errorf("depth-3 node %s must stop on max depth, got %q", o.NodeID, o.BaseCase)
			}
		}
	}
	if leaves != 8 {
		t.Errorf("want 8 leaves, got %d", leaves)
	}
}

func TestOutcomesArePreOrder(t *testing.T) {
	// Self before children, so replay is deterministic (P8). The root is first.
	prov := &fakeProvider{cost: 0}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.MaxDepth = 2
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[0].NodeID != "n0" {
		t.Errorf("pre-order must lead with the root, got %s", res.Outcomes[0].NodeID)
	}
	// Each parent appears before its own children in the flat list.
	pos := map[string]int{}
	for i, o := range res.Outcomes {
		pos[o.NodeID] = i
	}
	for _, o := range res.Outcomes {
		for _, c := range o.Children {
			if pos[c] < pos[o.NodeID] {
				t.Errorf("child %s precedes parent %s — not pre-order", c, o.NodeID)
			}
		}
	}
}

func TestBelowFloorSolvesDirectly(t *testing.T) {
	// Base case 3 (§2): a node that cannot fund one child above floor plus its
	// reduce solves directly rather than splitting pointlessly.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.MaxDepth = 10
	e.Floor = FromFloat(100) // apportionable (65% of 10) = 6.5 < 100
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(10)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].BaseCase != BaseBelowFloor {
		t.Fatalf("want a single below-floor leaf, got %d outcomes", len(res.Outcomes))
	}
	if prov.calls != 1 {
		t.Errorf("below-floor node solves once, got %d calls", prov.calls)
	}
}

// ------------------------------------------------------------------ DAG

func TestIdenticalSiblingsCollapseToOneCall(t *testing.T) {
	// DAG, not tree (§2): identical sub-problems in one plan resolve to a single
	// child, with weights merged. Two "dup" siblings become one call.
	prov := &fakeProvider{cost: FromFloat(1)}
	e := exec(t, StaticPlanner{P: fanoutPlan("dup", "dup", "unique")}, prov)
	e.MaxDepth = 1
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	// root + 2 distinct children (dup collapsed), not root + 3.
	if len(res.Outcomes) != 3 {
		t.Errorf("identical siblings must collapse: want 3 nodes, got %d", len(res.Outcomes))
	}
	if prov.calls != 2 {
		t.Errorf("want 2 calls (dup deduped), got %d", prov.calls)
	}
}

func TestWarmCacheServesWithoutSpending(t *testing.T) {
	// §6: a warm entry serves the stored distribution and spends nothing. Pre-
	// seed the cache for one child; only the other should reach the provider.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	seeded := problem("a")
	cache.Append(seeded, Sample{Content: "cached:a", Cost: FromFloat(1), Model: "prior"}, nil, now)

	e := exec(t, StaticPlanner{P: fanoutPlan("a", "b")}, prov)
	e.MaxDepth = 1
	e.Cache = cache
	res, err := e.Run(context.Background(), problem("root"), ledger(t, FromFloat(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Errorf("the cached child must not call the provider: want 1 call, got %d", prov.calls)
	}
	// The served child is flagged a hit and carries the stored content.
	var hits int
	for _, o := range res.Outcomes {
		if o.CacheHit {
			hits++
			if o.Content != "cached:a" {
				t.Errorf("served content must be the stored sample, got %q", o.Content)
			}
		}
	}
	if hits != 1 {
		t.Errorf("want exactly one cache hit, got %d", hits)
	}
}

func TestFreshResultsAppendToCache(t *testing.T) {
	// §6/P7: entries accumulate. A second run over the same sub-problem, when the
	// read policy extends, must ADD a sample rather than replace — so re-running
	// tightens error bars instead of echoing the first answer.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	e := exec(t, DeclinePlanner{}, prov) // whole root solved as one leaf, then cached
	e.Cache = cache
	e.ReadPolicy = func(Problem, int) string { return ReadExtend } // always draw fresh

	root := problem("root")
	for i := 0; i < 3; i++ {
		if _, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000))); err != nil {
			t.Fatal(err)
		}
	}
	if n := cache.N(root, now); n != 3 {
		t.Errorf("three extending runs must accumulate 3 samples, got %d", n)
	}
	if prov.calls != 3 {
		t.Errorf("extend must draw fresh each time, got %d calls", prov.calls)
	}
}

func TestServePolicyStopsSpending(t *testing.T) {
	// The complement: with a serve policy, a warm entry is reused and the second
	// run spends nothing. Reuse across runs is the DAG win the cache buys (§6).
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	e := exec(t, DeclinePlanner{}, prov)
	e.Cache = cache // default serve-when-warm

	root := problem("root")
	if _, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000))); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Errorf("second run must serve from cache: want 1 call total, got %d", prov.calls)
	}
	if n := cache.N(root, now); n != 1 {
		t.Errorf("serve must not append: want 1 sample, got %d", n)
	}
}

func TestCacheKeyIsScopeQualified(t *testing.T) {
	// P6: the same statement under different scope must NOT be served across the
	// entitlement boundary. A hit for scope A must miss for scope B.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	scopedA := Problem{Statement: "s", Scope: Scope{Tags: map[string]string{"agate:dept": "a"}}}
	cache.Append(scopedA, Sample{Content: "for-a"}, nil, now)

	e := &Executor{
		Planner: DeclinePlanner{}, Solver: ProviderSolver{Provider: prov, Model: "fake"},
		Reducer: ConcatReducer{}, Now: now, MaxDepth: 1, Cache: cache,
	}
	scopedB := Problem{Statement: "s", Scope: Scope{Tags: map[string]string{"agate:dept": "b"}}}
	l, _ := NewLedger(Caps{Spend: FromFloat(1000), Latency: time.Hour},
		Scope{Tags: map[string]string{"agate:dept": "b"}})
	res, err := e.Run(context.Background(), scopedB, l)
	if err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Errorf("a different scope must miss the cache and solve: got %d calls", prov.calls)
	}
	if res.Outcomes[0].CacheHit {
		t.Error("must not serve scope A's answer to scope B (P6)")
	}
}
