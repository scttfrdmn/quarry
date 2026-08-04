package quarry

import (
	"context"
	"testing"
	"time"
)

// These two tests were the probe that opened build step 8.1, kept because both
// findings were real and neither had a test. See cache.go (the expiry clock) and
// executor.go (the internal-node cache guard).

func TestATTLCacheStillServesWhatTheExecutorWrote(t *testing.T) {
	// The cache's expiry clock is the STORE's, not the sample's. MemCache used to
	// take entry.created from Sample.CreatedAt, which only a live provider stamps —
	// so every sample the executor wrote through a reducer, a fake, or any Solver
	// that left it zero was born already expired. With any TTL > 0 the cache then
	// served NOTHING it had written: N reported the sample, Get evicted it on read,
	// and the second run re-solved from scratch while looking like a working cache.
	// Every existing cache test used NewMemCache(0), which is why this survived.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(time.Hour)
	e := exec(t, DeclinePlanner{}, prov) // whole root as one leaf, then cached
	e.Cache = cache

	root := problem("root")
	if _, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000))); err != nil {
		t.Fatal(err)
	}
	if n := cache.N(root, now); n != 1 {
		t.Fatalf("the first run must leave one sample, got %d", n)
	}
	if got := cache.Get(root, now); len(got) != 1 {
		t.Fatalf("N says 1 but Get returns %d — a store may not disagree with itself "+
			"about what it holds", len(got))
	}
	if _, err := e.Run(context.Background(), root, ledger(t, FromFloat(1000))); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 {
		t.Errorf("the second run must be served from the warm entry: want 1 call total, got %d",
			prov.calls)
	}
}

func TestTheStoresClockIsNotTheSamplesClock(t *testing.T) {
	// The consequence stated directly: an entry expires by when it was STORED, and a
	// sample carrying no provenance timestamp is not thereby ancient. Written as its
	// own test because the executor path above could be made to pass by having the
	// executor stamp CreatedAt — which would fix the symptom by making the core read
	// a clock it is forbidden to read (Go rule 4), and would still leave any other
	// caller's unstamped sample born expired.
	c := NewMemCache(24 * time.Hour)
	p := Problem{Statement: "q"}
	c.Append(p, Sample{Content: "no timestamp"}, nil, now)

	if len(c.Get(p, now.Add(time.Hour))) != 1 {
		t.Error("an unstamped sample expired early — the store's clock is the store's")
	}
	if len(c.Get(p, now.Add(48*time.Hour))) != 0 {
		t.Error("entry outlived its TTL")
	}
	// And N agrees with Get about expiry, rather than counting what Get will not return.
	if n := c.N(p, now.Add(48*time.Hour)); n != 0 {
		t.Errorf("N must observe expiry like Get: want 0 past the TTL, got %d", n)
	}
}

func TestAPartialMergeIsNotCachedAsAnAnswer(t *testing.T) {
	// The internal-node path appended to the cache UNCONDITIONALLY while the leaf path
	// guarded on completeness, so a truncated tree's partial merge — content assembled
	// from a subset of its children — was stored under the parent's own key. A cache
	// entry has no way to say "partial": a served hit copies Content and sets CacheHit,
	// but nothing restores Gap. So the partial-ness is lost on serve, and the next run
	// over that problem is handed an incomplete answer as a complete one.
	//
	// That is precisely backwards for extend (§8.1), whose entire premise is that
	// completed subtrees serve from cache while the incomplete ones refill: the node
	// most needing a re-solve would be the one most confidently served.
	prov := &fakeProvider{cost: FromFloat(1)}
	cache := NewMemCache(0)
	e := exec(t, StaticPlanner{P: fanoutPlan("alpha", "beta")}, prov)
	e.MaxDepth = 1
	e.Cache = cache
	// Priced so the root can fund neither child: both come back empty, and the merge
	// over them is empty too.
	e.Estimate = func(Problem) Units { return FromFloat(40) }

	root := problem("root")
	res, err := e.Run(context.Background(), root, ledger(t, FromFloat(60)))
	if err != nil {
		t.Fatal(err)
	}
	// Non-vacuity: if the pricing above stops producing an incomplete merge, the
	// assertion below would pass for the wrong reason.
	if root := res.Outcomes[0]; root.Content != "" {
		t.Fatalf("this run was supposed to merge over unfunded children; got content %q — "+
			"the test no longer probes anything", root.Content)
	}
	if n := cache.N(root, now); n != 0 {
		t.Errorf("an incomplete merge must not be cached under the parent's key, got %d samples", n)
	}
}
