package netx

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"light-proxy/internal/dedup"
)

func testPrefix(t *testing.T) netip.Prefix {
	t.Helper()
	return netip.MustParsePrefix("2001:db8:1234::/64")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestNextSrcStaysInPoolAndNeverRepeats(t *testing.T) {
	p := testPrefix(t)
	r := New(p, dedup.New(24*time.Hour), time.Second, quietLogger())
	anycast := netip.MustParseAddr("2001:db8:1234::") // all-zero IID

	seen := make(map[uint64]bool, 10000)
	for i := 0; i < 10000; i++ {
		src, suffix := r.nextSrc()
		if !p.Contains(src) {
			t.Fatalf("draw %d: %v is outside %v", i, src, p)
		}
		if src == anycast {
			t.Fatalf("draw %d: rejected all-zero IID was issued", i)
		}
		if seen[suffix] {
			t.Fatalf("draw %d: suffix %#x repeated within the ttl window", i, suffix)
		}
		seen[suffix] = true
	}
}

func TestNextSrcRejectsZeroAndReservedSuffix(t *testing.T) {
	p := testPrefix(t)
	set := dedup.New(time.Hour)
	if !set.Reserve(1) {
		t.Fatal("setup: could not reserve suffix 1")
	}
	r := New(p, set, time.Second, quietLogger())

	// RNG returns all zeros: the rotator must skip the anycast IID and the
	// already-reserved suffix 1 without hanging.
	zeros := 0
	r.randRead = func(b []byte) (int, error) {
		clear(b)
		zeros++
		return len(b), nil
	}
	done := make(chan netip.Addr, 1)
	go func() { a, _ := r.nextSrc(); done <- a }()
	select {
	case a := <-done:
		if a == netip.MustParseAddr("2001:db8:1234::") {
			t.Fatal("issued the anycast address")
		}
		if !p.Contains(a) {
			t.Fatalf("%v outside pool", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nextSrc hung on a degenerate RNG")
	}
}

func TestDrawFallsBackWhenRandFails(t *testing.T) {
	r := New(testPrefix(t), dedup.New(time.Hour), time.Second, quietLogger())
	r.randRead = func([]byte) (int, error) { return 0, errFake }
	if got := r.draw(); got == 0 {
		t.Fatal("draw returned the excluded zero IID after math/rand fallback")
	}
}

var errFake = &net.AddrError{Err: "fake", Addr: "x"}

// TestDialBindsNonLocalSource proves IPV6_FREEBIND is applied: without it the
// kernel rejects the bind with "cannot assign requested address", and a
// freebind-enabled socket instead proceeds to the connect (where this
// unroutable documentation target times out or errors).
func TestDialBindsNonLocalSource(t *testing.T) {
	r := New(testPrefix(t), dedup.New(time.Hour), time.Second, quietLogger())
	_, err := r.Dial(context.Background(), "tcp", "[2001:db8:1234::1]:9")
	if err == nil {
		t.Fatal("expected the unreachable target to fail")
	}
	if strings.Contains(err.Error(), "cannot assign requested address") {
		t.Fatalf("non-local source bind rejected (IPV6_FREEBIND missing): %v", err)
	}
	if !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("failure was neither a bind nor a connect failure: %v", err)
	}
}

// TestDialRetriesWithFreshSource checks the Q8 retry contract: a failed dial
// is retried exactly once and the retry uses a source other than the first.
// The attempted sources are the only observable of a failed dial, so they are
// read back out of the error.
func TestDialRetriesWithFreshSource(t *testing.T) {
	p := testPrefix(t)
	r := New(p, dedup.New(time.Hour), time.Second, quietLogger())
	_, err := r.Dial(context.Background(), "tcp", "[2001:db8:1234::1]:9")
	if err == nil {
		t.Fatal("expected dial failure")
	}
	distinct := map[netip.Addr]bool{}
	for _, m := range addrRE.FindAllString(err.Error(), -1) {
		a, perr := netip.ParseAddr(m)
		if perr == nil && p.Contains(a) {
			distinct[a] = true
		}
	}
	if len(distinct) < 2 {
		t.Fatalf("retry did not use a fresh source; saw %d distinct in-pool source(s) in %v", len(distinct), err)
	}
}

var addrRE = regexp.MustCompile(`[0-9a-fA-F]{1,4}(?::[0-9a-fA-F]{1,4}){7}`)

func TestDialHonoursContextCancellation(t *testing.T) {
	r := New(testPrefix(t), dedup.New(time.Hour), time.Second, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Dial(ctx, "tcp", "[2001:db8:1234::1]:9"); err == nil {
		t.Fatal("expected error from a cancelled context")
	}
}
