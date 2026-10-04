// Package netx provides the source-address rotating dialer: every outbound
// connection binds a fresh random address drawn from the configured IPv6 /64.
package netx

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"syscall"
	"time"

	"light-proxy/internal/dedup"
)

// ipv6Freebind is the Linux socket option IPV6_FREEBIND (include/uapi/linux/
// in.h: 0x4e). Unlike IPV6_TRANSPARENT it requires no capability, so binding a
// non-local source address works unprivileged.
const ipv6Freebind = 0x4e

// Rotator dials TCP connections with a fresh, never-recently-reused source
// address from an IPv6 /64 pool.
type Rotator struct {
	prefix      netip.Prefix
	set         *dedup.Set
	dialTimeout time.Duration
	log         *slog.Logger
	randRead    func([]byte) (int, error)
}

// New returns a Rotator over prefix. prefix must be a valid IPv6 /64; anything
// else is a programming error and panics (config.Validate rejects it first).
// A fresh *net.Dialer is built per dial, so no state is shared between calls.
func New(prefix netip.Prefix, set *dedup.Set, dialTimeout time.Duration, log *slog.Logger) *Rotator {
	prefix = prefix.Masked()
	if !prefix.IsValid() || !prefix.Addr().Is6() || prefix.Addr().Is4In6() || prefix.Bits() != 64 {
		panic(fmt.Sprintf("netx: prefix %v is not an IPv6 /64", prefix))
	}
	return &Rotator{
		prefix:      prefix,
		set:         set,
		dialTimeout: dialTimeout,
		log:         log,
		randRead:    rand.Read,
	}
}

// draw returns a random non-zero 64-bit interface identifier. Zero is excluded
// because the all-zero IID is the RFC 4291 subnet-router anycast address.
func (r *Rotator) draw() uint64 {
	var buf [8]byte
	if _, err := r.randRead(buf[:]); err != nil {
		binary.BigEndian.PutUint64(buf[:], mathrand.Uint64())
	}
	s := binary.BigEndian.Uint64(buf[:])
	if s == 0 {
		s = 1
	}
	return s
}

// nextSrc returns an unused source address from the pool together with its IID
// suffix. It blocks only while collisions occur, which requires a broken RNG.
func (r *Rotator) nextSrc() (netip.Addr, uint64) {
	suffix := r.draw()
	for i := 0; !r.set.Reserve(suffix); i++ {
		if i >= 32 {
			// Unreachable with a working RNG (collision odds ~1e-13 per
			// draw). Probe forward so a degenerate RNG cannot hang a request;
			// the reserved set is finite, so this terminates.
			for suffix++; suffix == 0 || !r.set.Reserve(suffix); suffix++ {
			}
			break
		}
		suffix = r.draw()
	}
	b := r.prefix.Addr().As16()
	binary.BigEndian.PutUint64(b[8:], suffix)
	return netip.AddrFrom16(b), suffix
}

// setFreebind enables IPV6_FREEBIND on the freshly created socket. It runs
// after socket() and before bind(), which is what makes a non-local source
// address bindable without root.
func setFreebind(_ context.Context, _, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6Freebind, 1)
	}); err != nil {
		return err
	}
	return serr
}

// dialOne dials addr from the single source address src.
func (r *Rotator) dialOne(ctx context.Context, src netip.Addr, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:        r.dialTimeout,
		LocalAddr:      net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, 0)),
		ControlContext: setFreebind,
	}
	return d.DialContext(ctx, network, addr)
}

// Dial implements the http.Transport DialContext hook. It dials addr from a
// fresh source address and, on connect-phase failure only, retries once from a
// second fresh source before giving up.
func (r *Rotator) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	network = ipv6Only(network)

	src1, _ := r.nextSrc()
	c, err1 := r.dialOne(ctx, src1, network, addr)
	if err1 == nil {
		r.log.Debug("dial", "src", src1.String(), "target", addr)
		return c, nil
	}
	if ctx.Err() != nil {
		return nil, err1
	}

	r.log.Debug("dial retry with new source", "src", src1.String(), "target", addr, "err", err1)
	src2, _ := r.nextSrc()
	c2, err2 := r.dialOne(ctx, src2, network, addr)
	if err2 == nil {
		r.log.Debug("dial", "src", src2.String(), "target", addr, "attempt", 2)
		return c2, nil
	}
	return nil, fmt.Errorf("dial %s from %s: %w", addr, src1, errors.Join(err1, err2))
}

// ProbeBind verifies that a source address from the pool can be bound without
// privilege, i.e. that the kernel honours IPV6_FREEBIND. It creates a throwaway
// AF_INET6 socket, enables FREEBIND, binds a fresh in-pool address and closes
// it again: no packets are sent.
func (r *Rotator) ProbeBind() error {
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket(AF_INET6): %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, ipv6Freebind, 1); err != nil {
		return fmt.Errorf("setsockopt(IPV6_FREEBIND): %w", err)
	}
	src, _ := r.nextSrc()
	if err := syscall.Bind(fd, &syscall.SockaddrInet6{Addr: src.As16()}); err != nil {
		return fmt.Errorf("bind %s: %w", src, err)
	}
	return nil
}

// ProbeDial dials target from a fresh in-pool source and closes it. Success
// proves the whole path: bind, local AnyIP route, host nexthop route and
// return traffic.
func (r *Rotator) ProbeDial(ctx context.Context, target string) error {
	src, _ := r.nextSrc()
	c, err := r.dialOne(ctx, src, "tcp6", target)
	if err != nil {
		return fmt.Errorf("dial %s from %s: %w", target, src, err)
	}
	return c.Close()
}

// ipv6Only maps the TCP networks onto tcp6 so an IPv4-only target fails with
// "no suitable address" instead of a family mismatch: egress is IPv6-only.
func ipv6Only(network string) string {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return "tcp6"
	default:
		return network
	}
}
