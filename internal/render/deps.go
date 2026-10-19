package render

import (
	_ "embed"
	"fmt"

	"sigs.k8s.io/yaml"
)

// deps/Chart.yaml and deps/values.yaml pin every Helm chart and container
// image the operator installs onto workload clusters. They're shaped as a
// Helm chart only so Dependabot's helm ecosystem can bump them; nothing
// ever installs that chart.
var (
	//go:embed deps/Chart.yaml
	depsChartYAML []byte
	//go:embed deps/values.yaml
	depsValuesYAML []byte

	chartDeps = mustParseChartDeps(depsChartYAML)
	imageDeps = mustParseImageDeps(depsValuesYAML)
)

type chartDep struct {
	Version    string `json:"version"`
	Repository string `json:"repository"`
}

func mustParseChartDeps(raw []byte) map[string]chartDep {
	var chart struct {
		Dependencies []struct {
			Name string `json:"name"`
			chartDep
		} `json:"dependencies"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		panic(fmt.Sprintf("parse deps/Chart.yaml: %v", err))
	}
	deps := make(map[string]chartDep, len(chart.Dependencies))
	for _, d := range chart.Dependencies {
		deps[d.Name] = d.chartDep
	}
	return deps
}

func mustParseImageDeps(raw []byte) map[string]string {
	var values map[string]struct {
		Image struct {
			Repository string `json:"repository"`
			Tag        string `json:"tag"`
		} `json:"image"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		panic(fmt.Sprintf("parse deps/values.yaml: %v", err))
	}
	images := make(map[string]string, len(values))
	for key, v := range values {
		images[key] = v.Image.Repository + ":" + v.Image.Tag
	}
	return images
}

// pinnedChart returns the version and repository URL deps/Chart.yaml pins
// for chart, panicking at package init if it isn't listed.
func pinnedChart(chart string) chartDep {
	d, ok := chartDeps[chart]
	if !ok || d.Version == "" || d.Repository == "" {
		panic(fmt.Sprintf("chart %q is not pinned in deps/Chart.yaml", chart))
	}
	return d
}

// pinnedImage returns the repository:tag deps/values.yaml pins under key,
// panicking at package init if it isn't listed.
func pinnedImage(key string) string {
	img, ok := imageDeps[key]
	if !ok || img == ":" {
		panic(fmt.Sprintf("image %q is not pinned in deps/values.yaml", key))
	}
	return img
}
