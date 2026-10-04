package sysenv

import (
	"net/netip"
	"strings"
	"testing"
)

const sampleLocalTable = `local ::1 dev lo proto kernel metric 0 pref medium
local 2001:db8:1234::/64 dev lo proto kernel metric 0 pref medium
local fe80::42:6eff:fe15:6fb3 dev docker0 proto kernel metric 0 pref medium
2001:db8:9999::/64 dev eth0 proto kernel metric 256 pref medium
`

func TestParseLocalRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool string
		want bool
	}{
		{"present as local route", "2001:db8:1234::/64", true},
		{"absent", "2001:db8:5678::/64", false},
		{"present but not a local route entry", "2001:db8:9999::/64", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := netip.MustParsePrefix(tc.pool)
			if got := parseLocalRoute(sampleLocalTable, pool); got != tc.want {
				t.Errorf("parseLocalRoute(%s) = %v, want %v", tc.pool, got, tc.want)
			}
		})
	}
	if parseLocalRoute("", netip.MustParsePrefix("2001:db8:1234::/64")) {
		t.Error("empty table must not match")
	}
}

func TestProbesErr(t *testing.T) {
	ok := Probes{
		{Name: "a", OK: true},
		{Name: "b", Skipped: true, Reason: "no target"},
	}
	if err := ok.Err(); err != nil {
		t.Fatalf("all-passing probes returned %v", err)
	}

	bad := append(ok, Probe{Name: "route", Reason: "missing", Fix: "ip -6 route add local 2001:db8::/64 dev lo\nsudo light-proxy env-setup"})
	err := bad.Err()
	if err == nil {
		t.Fatal("failing probe set returned nil")
	}
	msg := err.Error()
	for _, want := range []string{"route", "missing", "ip -6 route add local 2001:db8::/64 dev lo", "sudo light-proxy env-setup"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if line := bad[2].Line(); !strings.HasPrefix(line, "[FAIL] route: missing") {
		t.Errorf("Line() = %q", line)
	}
	if line := ok[1].Line(); !strings.HasPrefix(line, "[skip] b: no target") {
		t.Errorf("Line() = %q", line)
	}
	if line := ok[0].Line(); !strings.HasPrefix(line, "[ok]") {
		t.Errorf("Line() = %q", line)
	}
}
