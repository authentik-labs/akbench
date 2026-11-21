package contract

import (
	"os"
	"regexp"
	"testing"

	"github.com/authentik-labs/akbench/internal/render"
)

// firstGroups returns the capture groups of pattern's first match in source.
func firstGroups(t *testing.T, what, source, pattern string) []string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(source)
	if m == nil {
		t.Fatalf("%s not found (pattern %s)", what, pattern)
	}
	return m[1:]
}

// TestTenantLibraryNamesMatchSeedPy guards the naming contract between the
// seeder and the k6 tenant library. A tenant's users are never listed: a
// script works out the username of user 123 the same way seed.py named it.
// If either side changed its prefix, padding or email domain alone, every
// login of a tenant test would fail at run time, which reads as authentik
// failing.
func TestTenantLibraryNamesMatchSeedPy(t *testing.T) {
	seedPy, err := os.ReadFile("../../seeder/seed.py")
	if err != nil {
		t.Fatal(err)
	}
	libTS, err := os.ReadFile("../../k6/scripts/" + render.TenantLibFileName)
	if err != nil {
		t.Fatal(err)
	}
	seed, lib := string(seedPy), string(libTS)

	// seed.py: def username(index: int) -> str: ... return f"u{index:06d}"
	pyName := func(fn string) (prefix, digits string) {
		g := firstGroups(t, "seed.py's "+fn, seed,
			`(?s)def `+fn+`\(index: int\) -> str:.*?return f"(\w+)\{index:0(\d+)d\}"`)
		return g[0], g[1]
	}
	// tenant.ts: const userPrefix = "u";
	tsConst := func(name string) string {
		return firstGroups(t, render.TenantLibFileName+"'s "+name, lib, `(?m)^const `+name+` = "?([\w.]+)"?;$`)[0]
	}

	userPrefix, userDigits := pyName("username")
	groupPrefix, groupDigits := pyName("group_name")
	emailDomain := firstGroups(t, "seed.py's email domain", seed, `\{username\(index\)\}@([\w.]+)"`)[0]

	for _, c := range []struct{ what, seeder, library string }{
		{"user name prefix", userPrefix, tsConst("userPrefix")},
		{"group name prefix", groupPrefix, tsConst("groupPrefix")},
		{"digits in a user name", userDigits, tsConst("nameDigits")},
		{"digits in a group name", groupDigits, tsConst("nameDigits")},
		{"email domain", emailDomain, tsConst("emailDomain")},
	} {
		if c.seeder != c.library {
			t.Errorf("%s: seed.py has %q, %s has %q", c.what, c.seeder, render.TenantLibFileName, c.library)
		}
	}
}
