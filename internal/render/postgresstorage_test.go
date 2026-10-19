package render

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestNextPostgresStorageSize(t *testing.T) {
	gi := func(n int64) int64 { return n << 30 }
	cases := []struct {
		name           string
		current, limit string
		used, capacity int64
		want           string
	}{
		{name: "below threshold", current: "10Gi", limit: "50Gi", used: gi(7), capacity: gi(10)},
		{name: "unknown capacity", current: "10Gi", limit: "50Gi", used: gi(7), capacity: 0},
		{name: "grows by half", current: "10Gi", limit: "50Gi", used: gi(8), capacity: gi(10), want: "15Gi"},
		{name: "rounds up to a whole GiB", current: "1Gi", limit: "50Gi", used: gi(1), capacity: gi(1), want: "2Gi"},
		{name: "capped at limit", current: "40Gi", limit: "50Gi", used: gi(39), capacity: gi(40), want: "50Gi"},
		{name: "already at limit", current: "50Gi", limit: "50Gi", used: gi(49), capacity: gi(50)},
		{name: "limit below current", current: "50Gi", limit: "20Gi", used: gi(49), capacity: gi(50)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, grow := NextPostgresStorageSize(resource.MustParse(tc.current), resource.MustParse(tc.limit), tc.used, tc.capacity)
			if tc.want == "" {
				if grow {
					t.Fatalf("grew to %s, want no growth", next.String())
				}
				return
			}
			if !grow || next.Cmp(resource.MustParse(tc.want)) != 0 {
				t.Fatalf("got %s (grow=%v), want %s", next.String(), grow, tc.want)
			}
		})
	}
}

func TestPostgresMaxStorageSize(t *testing.T) {
	topo := &benchv1alpha1.AuthentikDeployment{Spec: benchv1alpha1.AuthentikDeploymentSpec{Postgres: benchv1alpha1.PostgresSpec{
		Mode:     benchv1alpha1.PostgresCNPG,
		Embedded: &benchv1alpha1.EmbeddedPostgresSpec{MaxStorageSize: "10Gi"},
		CNPG:     &benchv1alpha1.CNPGPostgresSpec{MaxStorageSize: "20Gi"},
	}}}
	if got := PostgresMaxStorageSize(topo); got != "20Gi" {
		t.Errorf("CNPG: got %q, want 20Gi", got)
	}
	topo.Spec.Postgres.Mode = benchv1alpha1.PostgresEmbedded
	if got := PostgresMaxStorageSize(topo); got != "10Gi" {
		t.Errorf("Embedded: got %q, want 10Gi", got)
	}
	topo.Spec.Postgres.Mode = benchv1alpha1.PostgresExternal
	if got := PostgresMaxStorageSize(topo); got != "" {
		t.Errorf("External: got %q, want none", got)
	}
}

// TestCNPGClusterStorageNeverShrinks guards the size CNPGCluster renders once
// auto-growth has enlarged the volumes: rendering spec's smaller storageSize
// again would ask CNPG to shrink them, which it rejects.
func TestCNPGClusterStorageNeverShrinks(t *testing.T) {
	topo := &benchv1alpha1.AuthentikDeployment{
		Name: "authentik-1",
		Spec: benchv1alpha1.AuthentikDeploymentSpec{
			Namespace: "authentik-1",
			Postgres: benchv1alpha1.PostgresSpec{
				Mode: benchv1alpha1.PostgresCNPG,
				CNPG: &benchv1alpha1.CNPGPostgresSpec{StorageSize: "10Gi", MaxStorageSize: "50Gi"},
			},
		},
	}
	size := func() string {
		s, _, _ := unstructured.NestedString(CNPGCluster(topo, nil).Object, "spec", "storage", "size")
		return s
	}
	if got := size(); got != "10Gi" {
		t.Errorf("without growth: got %q, want 10Gi", got)
	}
	topo.Status.PostgresStorage = &benchv1alpha1.PostgresStorageStatus{Size: "15Gi"}
	if got := size(); got != "15Gi" {
		t.Errorf("after growth: got %q, want 15Gi", got)
	}
	topo.Spec.Postgres.CNPG.StorageSize = "20Gi"
	if got := size(); got != "20Gi" {
		t.Errorf("spec raised past the grown size: got %q, want 20Gi", got)
	}
}
