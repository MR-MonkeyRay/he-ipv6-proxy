// Package logx builds the process loggers. Every line is text-formatted to
// stdout; when a log file is configured the same lines are appended to it.
// The file can be reopened in place, so an external rotator (logrotate +
// SIGHUP) can move it without restarting the proxy.
package logx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/MR-MonkeyRay/he-ipv6-proxy/internal/config"
)

// Sink owns the log destinations of a running process.
type Sink struct {
	log  *slog.Logger
	file *fileSink // nil when file logging is disabled
}

// Open returns the logger for cfg plus the destinations behind it. The log
// directory is created when missing.
func Open(cfg config.LogConfig) (*Sink, error) {
	handlers := []slog.Handler{console(cfg.Level)}
	s := &Sink{}
	if path := cfg.Path(); path != "" {
		f, err := openFile(path)
		if err != nil {
			return nil, err
		}
		s.file = f
		handlers = append(handlers, slog.NewTextHandler(f, &slog.HandlerOptions{Level: level(cfg.Level)}))
	}
	s.log = slog.New(fanout(handlers))
	return s, nil
}

// Console returns a stdout-only logger. The one-shot subcommands (doctor,
// env-setup) print their report there and never touch the log file.
func Console(lvl string) *slog.Logger {
	return slog.New(console(lvl))
}

// Logger returns the process logger.
func (s *Sink) Logger() *slog.Logger { return s.log }

// Path returns the log file path, or "" when file logging is disabled.
func (s *Sink) Path() string {
	if s.file == nil {
		return ""
	}
	return s.file.path
}

// Reopen closes and reopens the log file. It is a no-op without a log file.
func (s *Sink) Reopen() error {
	if s.file == nil {
		return nil
	}
	return s.file.reopen()
}

// Close closes the log file.
func (s *Sink) Close() error {
	if s.file == nil {
		return nil
	}
	return s.file.close()
}

// level maps the validated config value onto a slog level.
func level(lvl string) slog.Level {
	if lvl == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func console(lvl string) slog.Handler {
	return slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level(lvl)})
}

// fileSink is an append-only log file whose descriptor can be swapped under
// the writers, which is what makes reopen possible without losing lines.
type fileSink struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func openFile(path string) (*fileSink, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &fileSink{path: path, f: f}, nil
}

func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

func (s *fileSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return 0, os.ErrClosed
	}
	return s.f.Write(p)
}

// reopen opens the path again and swaps it in, so lines written from now on
// land in the new file. The old descriptor is closed afterwards: a line being
// written concurrently completes against it.
func (s *fileSink) reopen() error {
	f, err := openAppend(s.path)
	if err != nil {
		return fmt.Errorf("reopen %s: %w", s.path, err)
	}
	s.mu.Lock()
	old := s.f
	s.f = f
	s.mu.Unlock()
	if old == nil {
		return nil
	}
	return old.Close()
}

func (s *fileSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// fanout sends one record to every handler. slog ships no combining handler;
// each handler gets its own clone of the record.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, lvl slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		errs = append(errs, h.Handle(ctx, r.Clone()))
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

var _ io.Writer = (*fileSink)(nil)
