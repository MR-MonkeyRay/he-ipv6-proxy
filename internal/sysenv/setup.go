package sysenv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"light-proxy/internal/config"
)

// EnvSetup applies the guest-side prerequisites idempotently. It needs root. It
// never touches sysctls: IPV6_FREEBIND is per socket and unprivileged. The
// host-side route is printed, never executed, because it cannot be applied from
// inside the guest's network namespace.
func EnvSetup(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if os.Geteuid() != 0 {
		return errors.New("env-setup must run as root: sudo light-proxy env-setup -c <config>")
	}
	pool := cfg.Pool()

	has, err := hasLocalRoute(ctx, pool)
	if err != nil {
		return fmt.Errorf("inspecting the local routing table: %w", err)
	}
	switch {
	case has:
		log.Info("AnyIP local route already present, nothing to do", "route", "local "+pool.String())
	default:
		log.Info("adding AnyIP local route", "route", "local "+pool.String(), "dev", "lo")
		out, err := exec.CommandContext(ctx, "ip", "-6", "route", "add", "local", pool.String(), "dev", "lo").CombinedOutput()
		if err != nil {
			return fmt.Errorf("ip -6 route add local %s dev lo: %w: %s", pool, err, out)
		}
		has, err = hasLocalRoute(ctx, pool)
		if err != nil {
			return fmt.Errorf("re-checking the local routing table: %w", err)
		}
		if !has {
			return fmt.Errorf("`local %s` is still absent from `ip -6 route show table local` after adding it", pool)
		}
	}

	log.Info("guest-side prerequisites are in place")
	log.Warn("host-side route is still required and cannot be set from inside the guest",
		"command", HostRouteCommand(pool), "note", HostRouteNote)
	return nil
}
