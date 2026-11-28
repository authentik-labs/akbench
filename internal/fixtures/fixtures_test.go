package fixtures

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type blueprintEntry struct {
	Model       string            `yaml:"model"`
	ID          string            `yaml:"id"`
	Identifiers map[string]string `yaml:"identifiers"`
	Attrs       yaml.Node         `yaml:"attrs"`
}

func parse(t *testing.T, f *Fixtures) ([]blueprintEntry, []Credential) {
	t.Helper()
	var doc struct {
		Version int              `yaml:"version"`
		Entries []blueprintEntry `yaml:"entries"`
	}
	if err := yaml.Unmarshal(f.Blueprint, &doc); err != nil {
		t.Fatalf("blueprint isn't valid YAML: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("version = %d", doc.Version)
	}
	var creds []Credential
	if err := json.Unmarshal(f.Credentials, &creds); err != nil {
		t.Fatalf("credentials aren't valid JSON: %v", err)
	}
	return doc.Entries, creds
}

// keyOfRefs returns every !KeyOf reference under n.
func keyOfRefs(n *yaml.Node) []string {
	if n.Tag == "!KeyOf" {
		return []string{n.Value}
	}
	var refs []string
	for _, c := range n.Content {
		refs = append(refs, keyOfRefs(c)...)
	}
	return refs
}

func TestGenerateIsConsistent(t *testing.T) {
	for _, users := range []int{1, 7, 50, 500} {
		f, err := Generate(users, 42)
		if err != nil {
			t.Fatal(err)
		}
		entries, creds := parse(t, f)
		if len(creds) != users {
			t.Fatalf("%d users: %d credentials", users, len(creds))
		}

		groups := map[string]bool{}
		userPasswords := map[string]string{}
		for _, e := range entries {
			switch e.Model {
			case "authentik_core.group":
				groups[e.ID] = true
			case "authentik_core.user":
				var attrs struct {
					Email    string `yaml:"email"`
					Password string `yaml:"password"`
				}
				if err := e.Attrs.Decode(&attrs); err != nil {
					t.Fatal(err)
				}
				userPasswords[e.Identifiers["username"]] = attrs.Password
			default:
				t.Fatalf("unexpected model %q", e.Model)
			}
		}
		if len(userPasswords) != users {
			t.Fatalf("%d users: %d user entries", users, len(userPasswords))
		}
		// Every credential logs in as a user the blueprint creates, with the
		// same password - and usernames are unique.
		seen := map[string]bool{}
		for _, c := range creds {
			if seen[c.Username] {
				t.Errorf("duplicate username %q", c.Username)
			}
			seen[c.Username] = true
			if userPasswords[c.Username] != c.Password || c.Email != c.Username+"@"+EmailDomain {
				t.Errorf("credential %+v doesn't match its blueprint user", c)
			}
			if len(c.Password) != 24 || !strings.ContainsAny(c.Password, specialChars) ||
				!strings.ContainsAny(c.Password, digitChars) || !strings.ContainsAny(c.Password, upperChars) {
				t.Errorf("weak password %q", c.Password)
			}
		}
		// Every group reference resolves to a group defined earlier in the
		// blueprint (authentik resolves !KeyOf in entry order).
		defined := map[string]bool{}
		for _, e := range entries {
			for _, ref := range keyOfRefs(&e.Attrs) {
				if !defined[ref] {
					t.Errorf("%s references %q before it's defined", e.ID, ref)
				}
			}
			if e.Model == "authentik_core.group" {
				defined[e.ID] = true
			}
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	a, _ := Generate(50, 7)
	b, _ := Generate(50, 7)
	c, _ := Generate(50, 8)
	if !bytes.Equal(a.Blueprint, b.Blueprint) || !bytes.Equal(a.Credentials, b.Credentials) {
		t.Error("same seed, different output")
	}
	if bytes.Equal(a.Credentials, c.Credentials) {
		t.Error("different seeds, same credentials")
	}
	if _, err := Generate(0, 1); err == nil {
		t.Error("expected an error for zero users")
	}
}

func TestGenerateEmitsKeyOfTags(t *testing.T) {
	f, err := Generate(3, 1)
	if err != nil {
		t.Fatal(err)
	}
	// authentik's blueprint loader needs the literal tag, unquoted.
	if !bytes.Contains(f.Blueprint, []byte("- !KeyOf group-all-employees\n")) {
		t.Errorf("no plain !KeyOf reference in:\n%s", f.Blueprint[:min(len(f.Blueprint), 800)])
	}
}

// The oauth-policies test dependency binds its application to AccessRoot and
// to an expression policy reading employee_number, and its tests log in as
// these users: each must be a member of AccessRoot, only through a child
// group (so that the binding has a group tree to walk), and carry the
// attribute. A fixture change that breaks this would otherwise only show as
// refused logins at run time.
func TestEveryUserReachesAccessRootThroughAChildGroup(t *testing.T) {
	for _, seed := range []uint64{1, 42, 1337} {
		f, err := Generate(50, seed)
		if err != nil {
			t.Fatal(err)
		}
		entries, _ := parse(t, f)

		parents := map[string][]string{}
		root := ""
		for _, e := range entries {
			if e.Model != "authentik_core.group" {
				continue
			}
			var attrs struct {
				Parents yaml.Node `yaml:"parents"`
			}
			if err := e.Attrs.Decode(&attrs); err != nil {
				t.Fatal(err)
			}
			parents[e.ID] = keyOfRefs(&attrs.Parents)
			if e.Identifiers["name"] == AccessRoot {
				root = e.ID
			}
		}
		if root == "" {
			t.Fatalf("seed %d: no group named %q", seed, AccessRoot)
		}
		var under func(id string, seen map[string]bool) bool
		under = func(id string, seen map[string]bool) bool {
			if seen[id] {
				return false
			}
			seen[id] = true
			for _, p := range parents[id] {
				if p == root || under(p, seen) {
					return true
				}
			}
			return false
		}

		users := 0
		for _, e := range entries {
			if e.Model != "authentik_core.user" {
				continue
			}
			users++
			name := e.Identifiers["username"]
			var attrs struct {
				Groups     yaml.Node         `yaml:"groups"`
				Attributes map[string]string `yaml:"attributes"`
			}
			if err := e.Attrs.Decode(&attrs); err != nil {
				t.Fatal(err)
			}
			if attrs.Attributes["employee_number"] == "" {
				t.Errorf("seed %d: user %s has no employee_number", seed, name)
			}
			through := false
			for _, g := range keyOfRefs(&attrs.Groups) {
				if g == root {
					t.Errorf("seed %d: user %s is a direct member of %q", seed, name, AccessRoot)
				}
				through = through || under(g, map[string]bool{})
			}
			if !through {
				t.Errorf("seed %d: user %s is not under %q through any group", seed, name, AccessRoot)
			}
		}
		if users != 50 {
			t.Fatalf("seed %d: %d users, want 50", seed, users)
		}
	}
}
