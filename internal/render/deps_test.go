package render

import (
	"strings"
	"testing"
)

func TestPinnedDeps(t *testing.T) {
	if got := pinnedChart("kube-prometheus-stack").Repository; got != "https://prometheus-community.github.io/helm-charts" {
		t.Errorf("kube-prometheus-stack repository = %q", got)
	}
	if !strings.HasPrefix(K6Image, "grafana/k6:") {
		t.Errorf("K6Image = %q, want grafana/k6:<tag>", K6Image)
	}
	for key, img := range imageDeps {
		repo, tag, _ := strings.Cut(img, ":")
		if repo == "" || tag == "" {
			t.Errorf("image %q = %q, want repository:tag", key, img)
		}
	}
}
