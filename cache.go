package quarry

import (
	"sync"
	"time"
)

// Read policy for the cache (§6, §8.1).
const (
	// ReadServe returns the existing distribution and spends nothing.
	ReadServe = "serve"

	// ReadExtend draws a fresh sample and appends. Repeated runs increase n and
	// tighten error bars rather than echoing the first result back. Nodes
	// flagged unstable are ALWAYS extended — serving a stored answer to a
	// stability check defeats the check.
	ReadExtend = "extend"
)

type entry struct {
	samples []Sample
	sources map[string]struct{}

	// created is when the STORE took the entry, which is not when the sample was
	// produced. It was originally Sample.CreatedAt, and that conflated two clocks
	// with different owners: CreatedAt is provenance stamped by whoever made the
	// call, and only a live provider stamps it (Go rule 4 forbids the core reading a
	// clock). Every sample written by a reducer, a fake, or any Solver leaving it
	// zero was therefore born in 0001-01-01 and already past any TTL — so with any
	// TTL > 0 the cache served nothing it had written while still counting it in N.
	created time.Time
}

// MemCache is the reference Cache implementation.
//
// The DynamoDB + S3 backing for the integrated deployment implements the same
// interface; keep this one as the test double.
//
// NO PROVISIONED CAPACITY, ever (P5). This cache is empty most of the time and
// is exactly the thing that tempts you toward Redis or OpenSearch — either is a
// permanent idle floor for a cache that mostly is not there. Per-request and
// per-byte only.
type MemCache struct {
	// TTL keeps the idle floor from creeping upward run after run (P5).
	TTL time.Duration

	mu      sync.RWMutex
	entries map[string]*entry
}

// NewMemCache builds an empty cache. A zero TTL means entries do not expire.
func NewMemCache(ttl time.Duration) *MemCache {
	return &MemCache{TTL: ttl, entries: map[string]*entry{}}
}

// Get returns every sample recorded for this (problem, scope) key.
func (c *MemCache) Get(p Problem, now time.Time) []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[p.Key()]
	if !ok {
		return nil
	}
	if c.expired(e, now) {
		delete(c.entries, p.Key())
		return nil
	}
	out := make([]Sample, len(e.samples))
	copy(out, e.samples)
	return out
}

// Append adds a sample. Never replaces — see the Cache interface doc.
//
// now is when the store takes the sample, and it starts the TTL clock. Passed in
// rather than read here for the reason every clock in this package is passed in
// (Go rule 4), and taken from the CALLER rather than from Sample.CreatedAt because
// those are different clocks: see entry.created.
func (c *MemCache) Append(p Problem, s Sample, sources []string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := p.Key()
	e, ok := c.entries[k]
	if !ok {
		e = &entry{sources: map[string]struct{}{}, created: now}
		c.entries[k] = e
	}
	e.samples = append(e.samples, s)
	for _, src := range sources {
		e.sources[src] = struct{}{}
	}
}

// expired reports whether an entry has outlived its TTL as of now.
//
// A zero now means the caller has no clock, and an unclocked caller gets no
// expiry rather than immediate expiry — the same "absence is not zero" discipline
// the rest of the package follows. A zero created means the same thing for an
// entry stored by such a caller.
func (c *MemCache) expired(e *entry, now time.Time) bool {
	if c.TTL <= 0 || now.IsZero() || e.created.IsZero() {
		return false
	}
	return now.Sub(e.created) > c.TTL
}

// Invalidate drops every entry that depended on a changed document version.
//
// Without this a re-ingest silently serves stale sub-results (§6).
func (c *MemCache) Invalidate(source string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.entries {
		if _, ok := e.sources[source]; ok {
			delete(c.entries, k)
			n++
		}
	}
	return n
}

// N is the sample count — how many independent draws this sub-problem has.
//
// n=1 is one sample, not a result (P7).
//
// Takes now and observes expiry, because a store must not disagree with itself
// about what it holds: N counting samples that Get will refuse to return is how
// the CreatedAt confusion above stayed hidden — the sample was visibly there and
// unreachable. Read-only, so an expired entry is reported as absent but not
// deleted; Get does the eviction.
func (c *MemCache) N(p Problem, now time.Time) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.entries[p.Key()]; ok && !c.expired(e, now) {
		return len(e.samples)
	}
	return 0
}

var _ Cache = (*MemCache)(nil)
