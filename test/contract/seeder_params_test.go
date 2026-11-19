package contract

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"testing"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// pythonDictKeys returns the quoted keys of the dict literal assigned to name
// at the top level of a Python source file.
func pythonDictKeys(t *testing.T, source, name string) []string {
	t.Helper()
	dict := regexp.MustCompile(`(?ms)^` + name + `\b[^=\n]*= \{(.*?)\}`).FindStringSubmatch(source)
	if dict == nil {
		t.Fatalf("seed.py has no %s dict", name)
	}
	matches := regexp.MustCompile(`"(\w+)"\s*:`).FindAllStringSubmatch(dict[1], -1)
	keys := make([]string, 0, len(matches))
	for _, m := range matches {
		keys = append(keys, m[1])
	}
	return keys
}

// TestSeederParamsMatchSeedPy guards the one place the operator's names for
// a tenant's numbers meet the seeder's (render.SeederParams): the seeder
// rejects an unknown param and requires every one in PARAMS, so a rename on
// either side would fail every seeding Job at run time. Params in
// LATER_PARAMS may be left out; the seeder counts them as 0.
func TestSeederParamsMatchSeedPy(t *testing.T) {
	content, err := os.ReadFile("../../seeder/seed.py")
	if err != nil {
		t.Fatal(err)
	}
	required := pythonDictKeys(t, string(content), "PARAMS")
	later := pythonDictKeys(t, string(content), "LATER_PARAMS")
	if len(required) == 0 {
		t.Fatal("found no keys in seed.py's PARAMS")
	}

	var sent map[string]int
	if err := json.Unmarshal([]byte(render.SeederParams(benchv1alpha1.TenantParams{})), &sent); err != nil {
		t.Fatal(err)
	}
	for _, key := range required {
		if _, ok := sent[key]; !ok {
			t.Errorf("render.SeederParams lacks %q, which seed.py requires", key)
		}
	}
	for key := range sent {
		if !slices.Contains(required, key) && !slices.Contains(later, key) {
			t.Errorf("render.SeederParams sends %q, which seed.py does not know", key)
		}
	}
}
