// Package sysenv probes and prepares the guest environment he-ipv6-proxy needs:
// unprivileged source binding (IPV6_FREEBIND) and the AnyIP local route for the
// pool.
package sysenv

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"

	"github.com/MR-MonkeyRay/he-ipv6-proxy/internal/config"
	"github.com/MR-MonkeyRay/he-ipv6-proxy/internal/netx"
)

// Probe is the outcome of one prerequisite check.
type Probe struct {
	Name    string
	OK      bool
	Skipped bool
	Reason  string
	Fix     string
}

// Line renders a probe for terminal output.
func (p Probe) Line() string {
	switch {
	case p.Skipped:
		return fmt.Sprintf("[skip] %s: %s", p.Name, p.Reason)
	case p.OK:
		return fmt.Sprintf("[ok]   %s", p.Name)
	default:
		s := fmt.Sprintf("[FAIL] %s: %s", p.Name, p.Reason)
		if p.Fix != "" {
			for _, l := range strings.Split(p.Fix, "\n") {
				s += "\n         fix: " + l
			}
		}
		return s
	}
}

// Probes is a set of prerequisite checks.
type Probes []Probe

// Failed reports whether any probe failed.
func (ps Probes) Failed() bool {
	for _, p := range ps {
		if !p.OK && !p.Skipped {
			return true
		}
	}
	return false
}

// Err returns nil when every probe passed, otherwise one error per failure
// carrying the exact fix command.
func (ps Probes) Err() error {
	var errs []error
	for _, p := range ps {
		if p.OK || p.Skipped {
			continue
		}
		err := errors.New(p.Name + ": " + p.Reason)
		if p.Fix != "" {
			err = fmt.Errorf("%w\n  fix: %s", err, strings.ReplaceAll(p.Fix, "\n", "\n  fix: "))
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

const (
	probeBind  = "IPv6 source bind from the pool (IPV6_FREEBIND)"
	probeRoute = "AnyIP local route for the pool"
	probeDial  = "live dial from a rotated source"

	freebindFix = "no sysctl is involved (net.ipv6.ip_nonlocal_bind is not used and IPv6 has no rp_filter); this\n" +
		"should always succeed on Linux, so a failure means the kernel or a container security\n" +
		"profile blocks setsockopt(IPV6_FREEBIND)."

	routeFix = "sudo he-ipv6-proxy env-setup -c <config>\n" +
		"or directly: ip -6 route add local <pool> dev lo"
)

// HostRouteCommand is the command the hypervisor (KVM host) or the LXC host
// needs so that return traffic to a rotated source reaches the guest. It can
// never be run from inside the guest.
func HostRouteCommand(pool netip.Prefix) string {
	return fmt.Sprintf("ip -6 route add %s via <guest-link-local> dev <bridge-or-veth>", pool)
}

// HostRouteNote explains why the host-side route is required.
const HostRouteNote = "LXC's lxc.net.*.ipv6.route and libvirt's route/NAT networks install routes for " +
	"declared subnets only, which does not deliver traffic for arbitrary addresses inside the /64. Get the " +
	"guest link-local with `ip -6 addr show scope link` inside the guest, and pin it (lxc.net.*.hwaddr on " +
	"LXC, the domain's <mac> on KVM) so the address stays stable. A KVM hypervisor must also forward IPv6: " +
	"net.ipv6.conf.all.forwarding=1."

// Check runs the cheap, offline prerequisites for `run`.
func Check(ctx context.Context, cfg *config.Config, r *netx.Rotator) Probes {
	return probes(ctx, cfg, r, false)
}

// Doctor runs the full prerequisite set for the doctor subcommand, including
// the live dial when doctor.test_target is set.
func Doctor(ctx context.Context, cfg *config.Config, r *netx.Rotator) Probes {
	return probes(ctx, cfg, r, true)
}

func probes(ctx context.Context, cfg *config.Config, r *netx.Rotator, liveDial bool) Probes {
	pool := cfg.Pool()
	ps := make(Probes, 0, 3)

	if err := r.ProbeBind(); err != nil {
		ps = append(ps, Probe{Name: probeBind, Reason: err.Error(), Fix: freebindFix})
	} else {
		ps = append(ps, Probe{Name: probeBind, OK: true})
	}

	switch has, err := hasLocalRoute(ctx, pool); {
	case err != nil:
		ps = append(ps, Probe{Name: probeRoute, Skipped: true,
			Reason: fmt.Sprintf("cannot run ip(8): %v; probe skipped, run `ip -6 route show table local` by hand", err)})
	case has:
		ps = append(ps, Probe{Name: probeRoute, OK: true})
	default:
		ps = append(ps, Probe{Name: probeRoute,
			Reason: fmt.Sprintf("`ip -6 route show table local` has no `local %s` entry", pool),
			Fix:    strings.ReplaceAll(routeFix, "<pool>", pool.String())})
	}

	if !liveDial {
		return ps
	}
	if cfg.Doctor.TestTarget == "" {
		ps = append(ps, Probe{Name: probeDial, Skipped: true,
			Reason: "doctor.test_target is empty; set it to a reachable IPv6 host:port to verify the return path"})
		return ps
	}
	if err := r.ProbeDial(ctx, cfg.Doctor.TestTarget); err != nil {
		ps = append(ps, Probe{Name: probeDial, Reason: err.Error(),
			Fix: fmt.Sprintf("run this on the hypervisor/LXC host, outside the guest:\n%s\n%s", HostRouteCommand(pool), HostRouteNote)})
	} else {
		ps = append(ps, Probe{Name: probeDial + " (" + cfg.Doctor.TestTarget + ")", OK: true})
	}
	return ps
}

// hasLocalRoute reports whether `ip -6 route show table local` contains the
// pool's AnyIP route.
func hasLocalRoute(ctx context.Context, pool netip.Prefix) (bool, error) {
	out, err := exec.CommandContext(ctx, "ip", "-6", "route", "show", "table", "local").Output()
	if err != nil {
		return false, err
	}
	return parseLocalRoute(string(out), pool), nil
}

// parseLocalRoute finds `local <pool> dev ...` among the lines of a local
// routing table dump.
func parseLocalRoute(out string, pool netip.Prefix) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "local" {
			continue
		}
		if p, err := netip.ParsePrefix(f[1]); err == nil && p == pool {
			return true
		}
	}
	return false
}
