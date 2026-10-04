package dedup

import (
	"context"
	"sync"
	"testing"
	"time"
)

// clock returns a controllable now function and an advance helper.
func clock(start time.Time) (func() time.Time, func(time.Duration)) {
	cur := start
	return func() time.Time { return cur }, func(d time.Duration) { cur = cur.Add(d) }
}

func TestReserveRejectsUntilTTLExpires(t *testing.T) {
	now, advance := clock(time.Unix(0, 0))
	s := New(24 * time.Hour)
	s.now = now

	if !s.Reserve(42) {
		t.Fatal("first Reserve(42) = false, want true")
	}
	if s.Reserve(42) {
		t.Fatal("second Reserve(42) = true, want false within ttl")
	}
	advance(23 * time.Hour)
	if s.Reserve(42) {
		t.Fatal("Reserve(42) = true at 23h, want false")
	}
	advance(1 * time.Hour)
	if !s.Reserve(42) {
		t.Fatal("Reserve(42) = false at 24h, want true after ttl elapsed")
	}
	if s.Reserve(7) != true {
		t.Fatal("unrelated suffix should be free")
	}
}

func TestSweepDropsExpired(t *testing.T) {
	now, advance := clock(time.Unix(0, 0))
	s := New(time.Hour)
	s.now = now

	s.Reserve(1)
	advance(30 * time.Minute)
	s.Reserve(2)
	advance(45 * time.Minute) // t=75m: suffix 1 expired, suffix 2 not

	if got := s.sweep(now()); got != 1 {
		t.Fatalf("sweep removed %d entries, want 1", got)
	}
	if _, ok := s.seen[1]; ok {
		t.Error("expired suffix 1 still present after sweep")
	}
	if _, ok := s.seen[2]; !ok {
		t.Error("live suffix 2 was swept")
	}
}

// TestReserveIsExclusiveUnderConcurrency checks the mutual exclusion of
// Reserve: within the ttl window each key may be granted at most once, even
// when many goroutines race for it.
func TestReserveIsExclusiveUnderConcurrency(t *testing.T) {
	const keys, workers, rounds = 100, 8, 1000
	s := New(24 * time.Hour)

	var mu sync.Mutex
	grants := make(map[uint64]int, keys)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				k := (seed*rounds + uint64(i)) % keys
				if s.Reserve(k) {
					mu.Lock()
					grants[k]++
					mu.Unlock()
				}
			}
		}(uint64(w))
	}
	wg.Wait()

	total := 0
	for k, n := range grants {
		if n != 1 {
			t.Errorf("key %d granted %d times, want exactly 1", k, n)
		}
		total += n
	}
	if total != keys {
		t.Errorf("granted %d distinct keys, want %d", total, keys)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	now, advance := clock(time.Unix(0, 0))
	s := New(time.Minute)
	s.now = now
	s.Reserve(9)
	advance(2 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, time.Millisecond); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		_, present := s.seen[9]
		s.mu.Unlock()
		if !present {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run never swept the expired entry")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
