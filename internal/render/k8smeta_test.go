package render

import (
	"strings"
	"testing"
)

func TestBoundedName(t *testing.T) {
	if got := BoundedName("dex-short", maxNameLength); got != "dex-short" {
		t.Errorf("short name changed: %q", got)
	}
	long := "oidc-test-client-2026-10-29-main-oauth-token-exchange-2026-10-29-main-2026-8-5-oauth-token-exchange"
	got := BoundedName(long, maxNameLength)
	if len(got) > maxNameLength || !strings.HasPrefix(got, "oidc-test-client-") {
		t.Errorf("BoundedName(%q) = %q", long, got)
	}
	if other := BoundedName(long[:len(long)-1]+"x", maxNameLength); other == got {
		t.Error("names differing past the cut collided")
	}
}

func TestChartFullnamesFitLongReleaseNames(t *testing.T) {
	// Helm's own 53-character release name limit.
	release := strings.Repeat("a", 53)
	if got := AuthentikFullname("ak"); got != "ak-authentik" {
		t.Errorf("short release renamed: %q", got)
	}
	if got := AuthentikFullname(release) + "-server-metrics"; len(got) > maxNameLength {
		t.Errorf("metrics Service %q is longer than %d", got, maxNameLength)
	}
}
