package render

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// cnpgAPIVersion and cnpgKind identify the CloudNativePG Cluster CRD. Built
// as unstructured.Unstructured rather than importing CNPG's typed API, which
// would drag the whole cloudnative-pg module (and its own large dependency
// tree) in just for one CRD's shape.
const (
	cnpgAPIVersion = "postgresql.cnpg.io/v1"
	cnpgKind       = "Cluster"

	// defaultCNPGStorageSize is used when CNPGPostgresSpec.StorageSize is
	// unset - CNPG requires spec.storage.size, it has no built-in default.
	defaultCNPGStorageSize = "10Gi"

	// cnpgDatabase/cnpgOwner match the embedded chart's own defaults
	// (postgresql.auth.username/database: authentik), so the generated app
	// user looks the same regardless of which Postgres mode a deployment uses.
	cnpgDatabase = "authentik"
	cnpgOwner    = "authentik"
)

// CNPGClusterName is deterministic from the deployment name.
func CNPGClusterName(t *benchv1alpha1.AuthentikDeployment) string {
	return t.Name + "-postgres"
}

// CNPGAppSecretName is the connection Secret CNPG generates for the
// bootstrapped app user/database, named "<cluster-name>-app" by convention.
func CNPGAppSecretName(t *benchv1alpha1.AuthentikDeployment) string {
	return CNPGClusterName(t) + "-app"
}

// CNPGCluster builds the CloudNativePG Cluster CR for a deployment in CNPG
// postgres mode: instances (>1 for HA) and storage sized/classed from
// spec.postgres.cnpg (and status.postgresStorage, once auto-growth has
// enlarged it), bootstrapped with an "authentik" database/owner so its
// generated app secret matches the embedded mode's own username/database.
func CNPGCluster(t *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster) *unstructured.Unstructured {
	cnpg := t.Spec.Postgres.CNPG
	instances := int64(3)
	if cnpg != nil && cnpg.Instances > 0 {
		instances = int64(cnpg.Instances)
	}
	storage := map[string]any{"size": cnpgStorageSize(t)}
	if cnpg != nil && cnpg.StorageClassName != "" {
		storage["storageClass"] = cnpg.StorageClassName
	}

	u := &unstructured.Unstructured{}
	u.SetAPIVersion(cnpgAPIVersion)
	u.SetKind(cnpgKind)
	u.SetName(CNPGClusterName(t))
	u.SetNamespace(t.Spec.Namespace)
	_ = unstructured.SetNestedField(u.Object, instances, "spec", "instances")
	_ = unstructured.SetNestedMap(u.Object, storage, "spec", "storage")
	_ = unstructured.SetNestedField(u.Object, cnpgDatabase, "spec", "bootstrap", "initdb", "database")
	_ = unstructured.SetNestedField(u.Object, cnpgOwner, "spec", "bootstrap", "initdb", "owner")
	// enablePodMonitor makes CNPG's own operator create a PodMonitor for
	// this Cluster (confirmed against the live CRD: `kubectl explain
	// clusters.postgresql.cnpg.io.spec.monitoring` - a real boolean field,
	// not guessed) - CNPG's operator, not this file, owns building the
	// postgres_exporter sidecar and scrape config, the same "let the
	// chart/operator create its own native monitoring object" approach the
	// authentik chart's own server.metrics.serviceMonitor takes
	// (helmrelease.go). Only when clusterRef is set - see HelmValues'
	// hasServiceMonitorCRD doc comment for why: the PodMonitor CRD only
	// exists on a WorkloadCluster, never the management cluster.
	if t.Spec.ClusterRef.Name != "" {
		_ = unstructured.SetNestedField(u.Object, true, "spec", "monitoring", "enablePodMonitor")
	}
	// inheritedMetadata.labels propagates onto every pod CNPG creates for
	// this cluster (its real Go API, cluster_types.go's
	// Cluster.Spec.InheritedMetadata *EmbeddedObjectMetadata - built as a
	// plain map here to keep this file's existing no-typed-CNPG-API-import
	// approach), which is what lets the cross-deployment anti-affinity below
	// find these pods from another deployment's own affinity rule the same
	// way it finds server/worker/embedded-postgres pods.
	_ = unstructured.SetNestedStringMap(u.Object, DeploymentLabels(t.Name), "spec", "inheritedMetadata", "labels")
	// additionalPodAntiAffinity (Cluster.Spec.Affinity.AdditionalPodAntiAffinity)
	// is additive - it doesn't disturb CNPG's own built-in same-cluster
	// instance-spreading (EnablePodAntiAffinity/PodAntiAffinityType), unlike
	// server.affinity/worker.affinity's mutually-exclusive-with-the-chart's-
	// own-preset behavior. Built as a real corev1.PodAntiAffinity and
	// converted, so the term itself comes from the one shared builder
	// (isolation.go) rather than a second hand-written copy of its shape.
	antiAffinity := &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
			crossDeploymentAntiAffinityTerm(t.Name),
		},
	}
	antiAffinityMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(antiAffinity)
	if err == nil {
		_ = unstructured.SetNestedMap(u.Object, antiAffinityMap, "spec", "affinity", "additionalPodAntiAffinity")
	}
	// Same authentik-pool targeting as the chart's own server/worker pods
	// (helmrelease.go) - CNPG's real API (Cluster.Spec.Affinity.NodeSelector/
	// .Tolerations, confirmed against cloudnative-pg/cloudnative-pg's own
	// cluster_types.go) passes both straight through to every instance pod
	// it creates, alongside the anti-affinity term above.
	if poolSelector, poolTolerations := AuthentikPoolPlacement(wc); len(poolSelector) > 0 || len(poolTolerations) > 0 {
		if len(poolSelector) > 0 {
			_ = unstructured.SetNestedStringMap(u.Object, poolSelector, "spec", "affinity", "nodeSelector")
		}
		if len(poolTolerations) > 0 {
			tolerations := make([]any, 0, len(poolTolerations))
			for _, t := range poolTolerations {
				tm, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&t)
				if err != nil {
					continue
				}
				tolerations = append(tolerations, tm)
			}
			_ = unstructured.SetNestedSlice(u.Object, tolerations, "spec", "affinity", "tolerations")
		}
	}
	return u
}

// CNPGReady reports whether a CNPG Cluster's "Ready" status condition is
// True.
func CNPGReady(u *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == benchv1alpha1.ConditionReady {
			status, _ := cond["status"].(string)
			return status == string(metav1.ConditionTrue)
		}
	}
	return false
}
