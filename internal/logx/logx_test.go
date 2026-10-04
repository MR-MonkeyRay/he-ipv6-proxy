package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MR-MonkeyRay/he-ipv6-proxy/internal/config"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestWritesToConfiguredFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "log")
	sink, err := Open(config.LogConfig{Level: "info", Dir: dir, File: "he-ipv6-proxy.log"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sink.Logger().Info("request", "status", 200)
	sink.Logger().Debug("hidden at info level")
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if want := filepath.Join(dir, "he-ipv6-proxy.log"); sink.Path() != want {
		t.Errorf("Path() = %q, want %q", sink.Path(), want)
	}
	got := readFile(t, sink.Path())
	if !strings.Contains(got, "msg=request") || !strings.Contains(got, "status=200") {
		t.Errorf("log file misses the record:\n%s", got)
	}
	if strings.Contains(got, "hidden at info level") {
		t.Errorf("debug record reached an info-level file:\n%s", got)
	}
}

func TestReopenWritesToTheNewFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.LogConfig{Level: "info", Dir: dir, File: "he-ipv6-proxy.log"}
	sink, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sink.Close()
	sink.Logger().Info("before rotation")

	// What logrotate does between its two moves.
	rotated := filepath.Join(dir, "he-ipv6-proxy.log.1")
	if err := os.Rename(cfg.Path(), rotated); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := sink.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	sink.Logger().Info("after rotation")

	if got := readFile(t, rotated); !strings.Contains(got, "before rotation") || strings.Contains(got, "after rotation") {
		t.Errorf("rotated file holds the wrong lines:\n%s", got)
	}
	got := readFile(t, cfg.Path())
	if !strings.Contains(got, "after rotation") || strings.Contains(got, "before rotation") {
		t.Errorf("reopened file holds the wrong lines:\n%s", got)
	}
}

func TestFileLoggingDisabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")
	for _, cfg := range []config.LogConfig{
		{Level: "info"},
		{Level: "info", Dir: dir},
		{Level: "info", File: "he-ipv6-proxy.log"},
	} {
		sink, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open(%+v): %v", cfg, err)
		}
		sink.Logger().Info("stdout only")
		if sink.Path() != "" {
			t.Errorf("Open(%+v).Path() = %q, want empty", cfg, sink.Path())
		}
		if err := sink.Reopen(); err != nil {
			t.Errorf("Reopen(%+v): %v", cfg, err)
		}
		if err := sink.Close(); err != nil {
			t.Errorf("Close(%+v): %v", cfg, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("disabled file logging created %s (err = %v)", dir, err)
	}
}
