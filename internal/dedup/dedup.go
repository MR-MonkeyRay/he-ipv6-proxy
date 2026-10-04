// Package dedup tracks recently used source-address suffixes so that no source
// address is reused within a configurable window. State is in-memory by design:
// the data is explicitly non-critical and is allowed to reset on restart.
package dedup

import (
	"context"
	"sync"
	"time"
)

// Set records the last use time of each 64-bit interface-identifier suffix.
// The /64 prefix is fixed, so the suffix uniquely identifies a source address.
type Set struct {
	mu   sync.Mutex
	seen map[uint64]time.Time
	ttl  time.Duration
	now  func() time.Time
}

// New returns an empty Set that refuses to re-issue a suffix until ttl has
// elapsed since it was last reserved.
func New(ttl time.Duration) *Set {
	return &Set{
		seen: make(map[uint64]time.Time),
		ttl:  ttl,
		now:  time.Now,
	}
}

// Reserve records suffix as used and reports whether it was still free. It
// returns false when suffix was reserved within the last ttl.
func (s *Set) Reserve(suffix uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if t, ok := s.seen[suffix]; ok && now.Sub(t) < s.ttl {
		return false
	}
	s.seen[suffix] = now
	return true
}

// sweep drops every suffix whose reservation has expired. It returns the
// number of entries removed.
func (s *Set) sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for suffix, t := range s.seen {
		if now.Sub(t) >= s.ttl {
			delete(s.seen, suffix)
			n++
		}
	}
	return n
}

// Run sweeps expired entries every interval until ctx is done. It returns when
// ctx is cancelled.
func (s *Set) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweep(now)
		}
	}
}
