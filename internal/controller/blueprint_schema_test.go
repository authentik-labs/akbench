package controller

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"go.yaml.in/yaml/v3"

	"github.com/authentik-labs/akbench/internal/fixtures"
)

// TestBlueprintsMatchUpstreamSchema validates every blueprint the operator
// generates against authentik's published blueprint schema
// (https://goauthentik.io/blueprints/schema.json), which CI downloads and
// passes in as BLUEPRINT_SCHEMA; skipped without it. Each entry is checked
// against its own model's definition, with unknown attrs rejected (the
// upstream schema allows them, so a misspelled field would otherwise pass and
// only fail at import). Values given as YAML tags (!Find, !KeyOf, ...) are
// left out, since the schema can't type them (they only resolve at import),
// but the tag itself must be one authentik knows.
func TestBlueprintsMatchUpstreamSchema(t *testing.T) {
	path := os.Getenv("BLUEPRINT_SCHEMA")
	if path == "" {
		t.Skip("BLUEPRINT_SCHEMA not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions map[string]map[string]any `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}

	fx, err := fixtures.Generate(50, 1)
	if err != nil {
		t.Fatal(err)
	}
	blueprints := map[string][]byte{
		"dependencies": blueprintDocument(
			oauthProviderEntries("oauth-test-c-d", "oauth-test-c-d", "secret", "http://client/auth/callback"),
			samlProviderEntries("saml-test-c-d", "http://sp/saml/acs", "http://sp/saml/metadata"),
			oidcSourceEntries("oidc-idp-c-d", "oidc-idp-c-d", "secret", "http://dex/dex"),
			samlSourceEntries("saml-idp-c-d", "http://idp/sso"),
			oauthTokenEntries("oauth-cc-c-d", "secret", "oauth-te-c-d", "secret2"),
		),
		"fixtures": fx.Blueprint,
	}

	validators := map[string]*jsonschema.Resolved{}
	validator := func(model string) *jsonschema.Resolved {
		if v, ok := validators[model]; ok {
			return v
		}
		def, ok := schema.Definitions["model_"+model]
		if !ok {
			return nil
		}
		def["additionalProperties"] = false
		b, err := json.Marshal(map[string]any{"definitions": schema.Definitions, "$ref": "#/definitions/model_" + model})
		if err != nil {
			t.Fatal(err)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		v, err := s.Resolve(nil)
		if err != nil {
			t.Fatalf("resolve schema for %s: %v", model, err)
		}
		validators[model] = v
		return v
	}

	for name, content := range blueprints {
		var node yaml.Node
		if err := yaml.Unmarshal(content, &node); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, tag := range dropTagged(&node) {
			if !blueprintTags[tag] {
				t.Errorf("%s: unknown blueprint tag %s", name, tag)
			}
		}
		var doc struct {
			Version int `yaml:"version"`
			Entries []struct {
				Model       string         `yaml:"model"`
				Identifiers map[string]any `yaml:"identifiers"`
				Attrs       map[string]any `yaml:"attrs"`
			} `yaml:"entries"`
		}
		if err := node.Decode(&doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if doc.Version != 1 || len(doc.Entries) == 0 {
			t.Errorf("%s: want version 1 with entries, got version %d, %d entries", name, doc.Version, len(doc.Entries))
		}
		for i, e := range doc.Entries {
			v := validator(e.Model)
			if v == nil {
				t.Errorf("%s: entry %d: unknown model %q", name, i, e.Model)
				continue
			}
			for part, values := range map[string]map[string]any{"identifiers": e.Identifiers, "attrs": e.Attrs} {
				if values == nil {
					continue
				}
				// Validate wants JSON-decoded values (float64, not int).
				b, err := json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				var instance any
				if err := json.Unmarshal(b, &instance); err != nil {
					t.Fatal(err)
				}
				if err := v.Validate(instance); err != nil {
					t.Errorf("%s: entry %d (%s) %s: %v", name, i, e.Model, part, err)
				}
			}
		}
	}
}

// blueprintTags are the YAML tags authentik's blueprint loader registers
// (authentik/blueprints/v1/common.py).
var blueprintTags = map[string]bool{
	"!KeyOf": true, "!Find": true, "!FindObject": true, "!Context": true, "!Format": true,
	"!Condition": true, "!If": true, "!Env": true, "!File": true, "!Enumerate": true,
	"!Value": true, "!Index": true, "!AtIndex": true, "!ParseJSON": true,
}

// dropTagged removes every mapping value and sequence item carrying a custom
// YAML tag (blueprint tags like !KeyOf, not core ones like !!str) and
// returns the tags it removed.
func dropTagged(n *yaml.Node) []string {
	var tags []string
	tagged := func(c *yaml.Node) bool {
		if strings.HasPrefix(c.Tag, "!") && !strings.HasPrefix(c.Tag, "!!") {
			tags = append(tags, c.Tag)
			return true
		}
		return false
	}
	switch n.Kind {
	case yaml.MappingNode:
		kept := n.Content[:0]
		for i := 0; i < len(n.Content); i += 2 {
			if !tagged(n.Content[i+1]) {
				kept = append(kept, n.Content[i], n.Content[i+1])
			}
		}
		n.Content = kept
	case yaml.SequenceNode:
		kept := n.Content[:0]
		for _, c := range n.Content {
			if !tagged(c) {
				kept = append(kept, c)
			}
		}
		n.Content = kept
	}
	for _, c := range n.Content {
		tags = append(tags, dropTagged(c)...)
	}
	return tags
}
