// Command light-proxy is a dependency-light IPv6 source-rotating forward proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"light-proxy/internal/config"
	"light-proxy/internal/dedup"
	"light-proxy/internal/logx"
	"light-proxy/internal/netx"
	"light-proxy/internal/proxy"
	"light-proxy/internal/sysenv"
)

const usage = `light-proxy — IPv6 source-rotating forward proxy (plain HTTP + CONNECT)

usage:
  light-proxy [run] [-c config.toml]       serve (default when no command is given)
  light-proxy doctor [-c config.toml]      probe every prerequisite, print exact fixes
  light-proxy env-setup [-c config.toml]   apply the guest-side AnyIP route (needs root)

flags:
  -c, --config path   TOML config file (default "config.toml")

exit codes:
  0 success   1 runtime failure   2 config error   3 prerequisite check failed
`

const (
	exitRuntime  = 1
	exitConfig   = 2
	exitSelfTest = 3

	shutdownGrace = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("light-proxy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := fs.String("c", "config.toml", "path to the TOML config file")
	fs.StringVar(cfgPath, "config", "config.toml", "path to the TOML config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitConfig
	}

	switch cmd {
	case "run":
		return runServe(ctx, *cfgPath)
	case "doctor":
		return runDoctor(ctx, *cfgPath)
	case "env-setup":
		return runEnvSetup(ctx, *cfgPath)
	default:
		fmt.Fprintf(os.Stderr, "light-proxy: unknown command %q\n\n%s", cmd, usage)
		return exitConfig
	}
}

func runServe(ctx context.Context, cfgPath string) int {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return fail(exitConfig, err)
	}
	// A log destination that cannot be opened is not retryable: fail like a
	// config error so the unit's RestartPreventExitStatus=2 3 stops the loop.
	sink, err := logx.Open(cfg.Log)
	if err != nil {
		return fail(exitConfig, err)
	}
	defer sink.Close()
	log := sink.Logger()
	if path := sink.Path(); path != "" {
		log.Info("logging to file", "file", path)
		// logrotate moves the file and sends SIGHUP; without a log file the
		// default HUP behaviour (terminate) is left alone.
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go func() {
			for range hup {
				if err := sink.Reopen(); err != nil {
					log.Error("log reopen failed, keeping the current file", "err", err)
					continue
				}
				log.Info("log file reopened", "file", path)
			}
		}()
	}

	set := dedup.New(cfg.Dedup.TTL.Std())
	rot := netx.New(cfg.Pool(), set, cfg.Network.DialTimeout.Std(), log)

	if cfg.Runtime.SelfCheck {
		ps := sysenv.Check(ctx, cfg, rot)
		if err := ps.Err(); err != nil {
			for _, p := range ps {
				fmt.Fprintln(os.Stderr, p.Line())
			}
			fmt.Fprintf(os.Stderr, "light-proxy: prerequisite check failed; see `light-proxy doctor -c %s`\n", cfgPath)
			return exitSelfTest
		}
		log.Debug("prerequisite check passed")
	}

	// Bind before serving so a port clash fails fast with a clear message.
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fail(exitRuntime, fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err))
	}

	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go set.Run(sweepCtx, cfg.Dedup.SweepInterval.Std())

	srv := &http.Server{
		Handler: proxy.New(cfg, rot.Dial, log),
		// No upstream deadline: responses may be large and slow. Rotated
		// sources are only used for outbound dials, so a stalled client
		// cannot pin one.
		ReadTimeout:       0,
		WriteTimeout:      0,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       cfg.Network.IdleTimeout.Std(),
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	log.Info("listening", "addr", ln.Addr().String(), "pool", cfg.Pool().String(),
		"auth", cfg.Auth.Enabled, "dedup_ttl", cfg.Dedup.TTL.Std().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fail(exitRuntime, fmt.Errorf("serve: %w", err))
		}
		return 0
	case <-ctx.Done():
	}

	log.Info("shutting down", "grace", shutdownGrace.String())
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("graceful shutdown failed, closing", "err", err)
		_ = srv.Close()
		return exitRuntime
	}
	return 0
}

func runDoctor(ctx context.Context, cfgPath string) int {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return fail(exitConfig, err)
	}
	log := logx.Console(cfg.Log.Level)
	set := dedup.New(cfg.Dedup.TTL.Std())
	rot := netx.New(cfg.Pool(), set, cfg.Network.DialTimeout.Std(), log)

	ps := sysenv.Doctor(ctx, cfg, rot)
	for _, p := range ps {
		fmt.Println(p.Line())
	}
	if ps.Failed() {
		return exitRuntime
	}
	return 0
}

func runEnvSetup(ctx context.Context, cfgPath string) int {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return fail(exitConfig, err)
	}
	if err := sysenv.EnvSetup(ctx, cfg, logx.Console(cfg.Log.Level)); err != nil {
		return fail(exitRuntime, err)
	}
	return 0
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func fail(code int, err error) int {
	fmt.Fprintf(os.Stderr, "light-proxy: %v\n", err)
	return code
}
