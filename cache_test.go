package quarry

import (
	"testing"
	"time"
)

// The two invariants that matter most are scope in the key (a security property)
// and sample accumulation (an epistemic one). Both are easy to "optimize" away
// by someone who has not read the design doc; that is why they are tests.

func sample(content string) Sample {
	return Sample{
		Content: content, Cost: FromFloat(1), Model: "m",
		ModelVersion: "m-2026-01-01", CreatedAt: now,
		HaloTokens: 800, GeneratedTokens: 200,
	}
}

func TestIdenticalStatementsUnderDifferentScopeDoNotCollide(t *testing.T) {
	// Two users can pose a hash-identical sub-problem while holding different
	// entitlements. Serving one's cached answer to the other walks straight
	// through the ABAC boundary (P6).
	a := Problem{"summarize the cohort", Scope{map[string]string{"agate:dept": "bio"}}}
	b := Problem{"summarize the cohort", Scope{map[string]string{"agate:dept": "chem"}}}
	if a.Key() == b.Key() {
		t.Fatal("scope must be part of the cache key")
	}

	c := NewMemCache(0)
	c.Append(a, sample("bio answer"), nil, now)
	if got := c.Get(b, now); len(got) != 0 {
		t.Errorf("cross-scope hit leaked %d samples", len(got))
	}
}

func TestSameStatementSameScopeHits(t *testing.T) {
	s := Scope{map[string]string{"agate:dept": "bio"}}
	c := NewMemCache(0)
	c.Append(Problem{"q", s}, sample("a"), nil, now)
	if len(c.Get(Problem{"q", s}, now)) != 1 {
		t.Error("same statement and scope must hit")
	}
}

func TestEntriesAccumulateRatherThanReplace(t *testing.T) {
	// A cache that returns a stored answer saves money by destroying
	// replication — the second run stops being an independent sample, which is
	// exactly what P7 needs it to be.
	c, p := NewMemCache(0), Problem{Statement: "q"}
	c.Append(p, sample("first"), nil, now)
	c.Append(p, sample("second"), nil, now)
	if c.N(p, now) != 2 {
		t.Fatalf("want n=2, got %d", c.N(p, now))
	}
	got := c.Get(p, now)
	if got[0].Content != "first" || got[1].Content != "second" {
		t.Error("samples must accumulate in order")
	}
}

func TestInvalidationDropsEntriesDependingOnAChangedSource(t *testing.T) {
	// Without this a re-ingest silently serves stale sub-results (§6).
	c := NewMemCache(0)
	c.Append(Problem{Statement: "a"}, sample("x"), []string{"doc1@v1"}, now)
	c.Append(Problem{Statement: "b"}, sample("y"), []string{"doc2@v1"}, now)
	if n := c.Invalidate("doc1@v1"); n != 1 {
		t.Fatalf("want 1 invalidated, got %d", n)
	}
	if len(c.Get(Problem{Statement: "a"}, now)) != 0 {
		t.Error("stale entry survived invalidation")
	}
	if len(c.Get(Problem{Statement: "b"}, now)) != 1 {
		t.Error("unrelated entry was dropped")
	}
}

func TestTTLExpiresEntries(t *testing.T) {
	// TTL keeps the idle floor from creeping upward run after run (P5).
	c := NewMemCache(24 * time.Hour)
	p := Problem{Statement: "q"}
	c.Append(p, sample("x"), nil, now)
	if len(c.Get(p, now.Add(time.Hour))) != 1 {
		t.Error("entry expired early")
	}
	if len(c.Get(p, now.Add(48*time.Hour))) != 0 {
		t.Error("entry outlived its TTL")
	}
}

func TestSurfaceToVolumeIsObservable(t *testing.T) {
	// P1's ratio becomes a measured number rather than a judgement call (§8.2).
	// A high value means the node paid for its parent's context and did little
	// with it — evidence the split was not worth making.
	s := sample("x")
	if r, ok := s.SurfaceToVolume(); !ok || r != 4.0 {
		t.Errorf("want 4.0, got %v (ok=%v)", r, ok)
	}
	s.GeneratedTokens = 0
	if _, ok := s.SurfaceToVolume(); ok {
		t.Error("no generated tokens means no ratio")
	}
}
