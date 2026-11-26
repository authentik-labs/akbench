package render

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

const (
	// postgresStorageGrowPercent is how full (used/capacity) a postgres
	// volume gets before auto-growth enlarges it.
	postgresStorageGrowPercent = 80
	// PostgresStorageHardLimitPercent is how full a postgres volume may get
	// before a Burn's load stops, whatever its size or maximum: past it,
	// growing the volume hasn't kept up or isn't happening.
	PostgresStorageHardLimitPercent = 95
	// postgresStorageGrowFactor is how much each auto-growth step enlarges
	// a volume by, as a fraction of its current size.
	postgresStorageGrowFactor = 1.5

	// defaultPostgresMaxStorageSize caps auto-growth when the deployment
	// sets no maxStorageSize of its own.
	defaultPostgresMaxStorageSize = "12Gi"

	gibibyte = 1 << 30

	// cnpgClusterLabel/cnpgPVCRoleLabel are the labels CNPG puts on every
	// PVC it creates for a Cluster; PG_DATA is the data volume (as opposed
	// to a separate WAL or tablespace volume).
	cnpgClusterLabel = "cnpg.io/cluster"
	cnpgPVCRoleLabel = "cnpg.io/pvcRole"
	cnpgPVCRoleData  = "PG_DATA"
)

// PostgresMaxStorageSize is the maxStorageSize set for the deployment's
// postgres mode, defaultPostgresMaxStorageSize when unset, or "" when
// auto-growth doesn't apply (External, whose storage the operator doesn't
// manage).
func PostgresMaxStorageSize(t *benchv1alpha1.AuthentikDeployment) string {
	size := ""
	switch t.Spec.Postgres.Mode {
	case benchv1alpha1.PostgresCNPG:
		if t.Spec.Postgres.CNPG != nil {
			size = t.Spec.Postgres.CNPG.MaxStorageSize
		}
	case benchv1alpha1.PostgresExternal:
		return ""
	default: // PostgresEmbedded
		if t.Spec.Postgres.Embedded != nil {
			size = t.Spec.Postgres.Embedded.MaxStorageSize
		}
	}
	if size == "" {
		return defaultPostgresMaxStorageSize
	}
	return size
}

// EmbeddedPostgresPVCName is the data PVC of the bundled subchart's
// StatefulSet: its "data" volumeClaimTemplate on the single pod, ordinal 0.
func EmbeddedPostgresPVCName(t *benchv1alpha1.AuthentikDeployment) string {
	return "data-" + EmbeddedPostgresStatefulSetName(t) + "-0"
}

// CNPGDataPVCLabels selects the data PVCs CNPG created for the deployment's
// Cluster, one per instance.
func CNPGDataPVCLabels(t *benchv1alpha1.AuthentikDeployment) map[string]string {
	return map[string]string{cnpgClusterLabel: CNPGClusterName(t), cnpgPVCRoleLabel: cnpgPVCRoleData}
}

// PostgresStorageFull reports whether a volume with used of capacity bytes in
// use is full enough to grow.
func PostgresStorageFull(used, capacity int64) bool {
	return capacity > 0 && used*100 >= capacity*postgresStorageGrowPercent
}

// PostgresStorageAtHardLimit reports whether a volume with used of capacity
// bytes in use is past PostgresStorageHardLimitPercent.
func PostgresStorageAtHardLimit(used, capacity int64) bool {
	return capacity > 0 && used*100 >= capacity*PostgresStorageHardLimitPercent
}

// PostgresStorageExhausted reports whether the deployment's postgres volume
// is too full to keep putting load on it: the deployment controller set
// ConditionPostgresStorageExhausted. When that is because the volume is full
// at its maximum size, the maximum must not have been raised since, so
// raising it resumes the load right away rather than at the next usage
// reading. Always false where auto-growth doesn't apply.
func PostgresStorageExhausted(t *benchv1alpha1.AuthentikDeployment) bool {
	cond := meta.FindStatusCondition(t.Status.Conditions, benchv1alpha1.ConditionPostgresStorageExhausted)
	maxSize := PostgresMaxStorageSize(t)
	if cond == nil || cond.Status != metav1.ConditionTrue || maxSize == "" {
		return false
	}
	if cond.Reason != benchv1alpha1.ReasonPostgresStorageAtMax {
		return true
	}
	st := t.Status.PostgresStorage
	if st == nil {
		return false
	}
	limit, err := resource.ParseQuantity(maxSize)
	if err != nil {
		return false
	}
	size, err := resource.ParseQuantity(st.Size)
	if err != nil {
		return false
	}
	return size.Cmp(limit) >= 0
}

// NextPostgresStorageSize decides whether a postgres volume of size current,
// with used of capacity bytes in use, should grow, and to what: by
// postgresStorageGrowFactor rounded up to a whole GiB, capped at limit. It
// reports false when the volume isn't full enough yet, or already at limit.
func NextPostgresStorageSize(current, limit resource.Quantity, used, capacity int64) (resource.Quantity, bool) {
	if !PostgresStorageFull(used, capacity) {
		return resource.Quantity{}, false
	}
	nextBytes := int64(float64(current.Value()) * postgresStorageGrowFactor)
	nextBytes = (nextBytes + gibibyte - 1) / gibibyte * gibibyte
	next := *resource.NewQuantity(nextBytes, resource.BinarySI)
	if next.Cmp(limit) > 0 {
		next = limit.DeepCopy()
	}
	if next.Cmp(current) <= 0 {
		return resource.Quantity{}, false
	}
	return next, true
}

// cnpgStorageSize is the CNPG Cluster's spec.storage.size: the configured
// size, or whatever auto-growth has grown it to since. Never smaller than
// the grown size - CNPG can't shrink a volume, and rejects a Cluster asking
// it to.
func cnpgStorageSize(t *benchv1alpha1.AuthentikDeployment) string {
	size := defaultCNPGStorageSize
	if cnpg := t.Spec.Postgres.CNPG; cnpg != nil && cnpg.StorageSize != "" {
		size = cnpg.StorageSize
	}
	st := t.Status.PostgresStorage
	if st == nil || st.Size == "" {
		return size
	}
	grown, err := resource.ParseQuantity(st.Size)
	if err != nil {
		return size
	}
	configured, err := resource.ParseQuantity(size)
	if err != nil || grown.Cmp(configured) > 0 {
		return st.Size
	}
	return size
}
