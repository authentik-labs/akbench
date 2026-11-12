package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// postgresStorageCheckInterval is how often postgres volume usage is read
// while auto-growth is on. Filesystem usage isn't an API object a watch
// could see change, so it has to be polled.
const postgresStorageCheckInterval = time.Minute

// volumeUsage is one PVC's filesystem usage in bytes.
type volumeUsage struct {
	used, capacity int64
}

// volumeUsageFunc reads the filesystem usage of pvcs, keyed by PVC name. A
// PVC no running pod mounts has no entry.
type volumeUsageFunc func(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment, pvcs []corev1.PersistentVolumeClaim) (map[string]volumeUsage, error)

// reconcilePostgresStorage grows the deployment's postgres volumes once they
// fill up, up to spec.postgres.<mode>.maxStorageSize. Embedded mode patches
// the StatefulSet's PVC directly (its volumeClaimTemplates are immutable, so
// the chart's own persistence.size can't do it); CNPG mode records the new
// size in status.postgresStorage for CNPGCluster to render, and CNPG resizes
// its PVCs itself. It returns how long until usage should be read again, 0
// when auto-growth is off. Failures are logged rather than returned:
// auto-growth is best effort and must not block the rest of the reconcile.
func (r *AuthentikDeploymentReconciler) reconcilePostgresStorage(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment) time.Duration {
	log := logf.FromContext(ctx)

	st := topo.Status.PostgresStorage
	maxSize := render.PostgresMaxStorageSize(topo)
	if maxSize == "" {
		// Keep a grown size: CNPGCluster must never render a size below
		// what the volumes already are.
		meta.RemoveStatusCondition(&topo.Status.Conditions, benchv1alpha1.ConditionPostgresStorageExhausted)
		if st != nil && st.Size == "" {
			topo.Status.PostgresStorage = nil
		} else if st != nil {
			topo.Status.PostgresStorage = &benchv1alpha1.PostgresStorageStatus{Size: st.Size}
		}
		return 0
	}
	limit, err := resource.ParseQuantity(maxSize)
	if err != nil {
		// Nothing below can judge the volume against a maximum it can't
		// read, so a condition set under an earlier, valid one is stale.
		meta.RemoveStatusCondition(&topo.Status.Conditions, benchv1alpha1.ConditionPostgresStorageExhausted)
		r.Recorder.Eventf(topo, corev1.EventTypeWarning, "InvalidMaxStorageSize", "cannot parse postgres maxStorageSize %q: %v", maxSize, err)
		return 0
	}
	if st == nil {
		st = &benchv1alpha1.PostgresStorageStatus{}
		topo.Status.PostgresStorage = st
	}
	// Status updates re-trigger this reconcile; without this the fresh
	// usage written below would make every reconcile write it again.
	if st.LastChecked != nil {
		if since := time.Since(st.LastChecked.Time); since < postgresStorageCheckInterval {
			return postgresStorageCheckInterval - since
		}
	}

	pvcs, err := postgresPVCs(ctx, rc, topo)
	if err != nil {
		log.Error(err, "Failed to list postgres PVCs")
		return postgresStorageCheckInterval
	}
	if len(pvcs) == 0 {
		// Without a volume (not created yet, or deleted by a reset) there is
		// nothing to be full.
		meta.RemoveStatusCondition(&topo.Status.Conditions, benchv1alpha1.ConditionPostgresStorageExhausted)
		return postgresStorageCheckInterval
	}
	usageFunc := r.VolumeUsage
	if usageFunc == nil {
		usageFunc = r.kubeletVolumeUsage
	}
	usage, err := usageFunc(ctx, topo, pvcs)
	if err != nil {
		log.Error(err, "Failed to read postgres volume usage")
		return postgresStorageCheckInterval
	}

	reading := readPostgresStorage(pvcs, usage)
	// Recover a grown size the status lost (or never had), so CNPGCluster
	// doesn't render one smaller than the volumes already are.
	if grown, err := resource.ParseQuantity(st.Size); err != nil || reading.current.Cmp(grown) > 0 {
		st.Size = reading.current.String()
	}
	if reading.capacity == 0 {
		// No running pod mounts the volumes right now (postgres being
		// rescheduled, a CNPG switchover, a node drain). That says nothing
		// about how full they are, so it counts as a failed reading: the
		// last usage and ConditionPostgresStorageExhausted stay as they
		// were, rather than resuming a stopped burn only to stop it again
		// at the next reading.
		log.Info("Skipped postgres volume usage check, no running Pod mounts its PersistentVolumeClaims")
		return postgresStorageCheckInterval
	}

	now := metav1.Now()
	st.LastChecked = &now
	st.UsedBytes, st.CapacityBytes = reading.used, reading.capacity
	var growErr error
	if !reading.resizing {
		growErr = r.growPostgresStorage(ctx, rc, topo, pvcs, reading, limit)
	}
	setPostgresStorageExhausted(topo, reading, limit, growErr)
	return postgresStorageCheckInterval
}

// postgresStorageReading is one usage reading of a deployment's postgres
// volumes.
type postgresStorageReading struct {
	// current is the largest size any of the PVCs requests.
	current resource.Quantity
	// resizing is true while a PVC hasn't reached the size it requests.
	resizing bool
	// used and capacity are the fullest volume's filesystem usage in bytes,
	// both 0 when no PVC had a usage entry. pvcCapacity is that volume's
	// PVC status capacity.
	used, capacity int64
	pvcCapacity    resource.Quantity
}

// readPostgresStorage summarizes pvcs and their usage.
func readPostgresStorage(pvcs []corev1.PersistentVolumeClaim, usage map[string]volumeUsage) postgresStorageReading {
	var reading postgresStorageReading
	for _, pvc := range pvcs {
		requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if requested.Cmp(reading.current) > 0 {
			reading.current = requested
		}
		capacity, hasCapacity := pvc.Status.Capacity[corev1.ResourceStorage]
		if hasCapacity && capacity.Cmp(requested) < 0 {
			reading.resizing = true
		}
		// Track the fullest volume by ratio; they all grow together.
		if u, ok := usage[pvc.Name]; ok && u.capacity > 0 &&
			(reading.capacity == 0 || u.used*reading.capacity > reading.used*u.capacity) {
			reading.used, reading.capacity = u.used, u.capacity
			reading.pvcCapacity = capacity
		}
	}
	return reading
}

// percent is how full the reading's fullest volume is.
func (s postgresStorageReading) percent() int64 {
	return s.used * 100 / s.capacity
}

// atMax reports whether the reading shows a full volume that has finished
// growing to limit. The kubelet caches volume stats for about a minute, so
// a reading right after the last resize can still be of the old, smaller
// volume, full; it only counts once its capacity is about the PVC's
// (filesystem overhead keeps it a little below).
func (s postgresStorageReading) atMax(limit resource.Quantity) bool {
	return !s.resizing && s.current.Cmp(limit) >= 0 && render.PostgresStorageFull(s.used, s.capacity) &&
		s.capacity*10 >= s.pvcCapacity.Value()*9
}

// growPostgresStorage grows the deployment's postgres volumes when reading
// shows them full and they are below limit. It returns the error that kept
// a full volume from growing.
func (r *AuthentikDeploymentReconciler) growPostgresStorage(
	ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment, pvcs []corev1.PersistentVolumeClaim,
	reading postgresStorageReading, limit resource.Quantity,
) error {
	st := topo.Status.PostgresStorage
	next, grow := render.NextPostgresStorageSize(reading.current, limit, reading.used, reading.capacity)
	if !grow {
		if render.PostgresStorageFull(reading.used, reading.capacity) && reading.current.Cmp(limit) >= 0 {
			r.Recorder.Eventf(topo, corev1.EventTypeWarning, "PostgresStorageAtMax",
				"postgres volume is %d%% full and already at maxStorageSize %s", reading.percent(), limit.String())
		}
		return nil
	}

	if topo.Spec.Postgres.Mode == benchv1alpha1.PostgresEmbedded {
		for i := range pvcs {
			pvc := &pvcs[i]
			patch := client.MergeFrom(pvc.DeepCopy())
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = next
			if err := rc.Patch(ctx, pvc, patch); err != nil {
				r.Recorder.Eventf(topo, corev1.EventTypeWarning, "PostgresStorageGrowFailed", "failed to grow PVC %s to %s: %v", pvc.Name, next.String(), err)
				return fmt.Errorf("grow PVC %s to %s: %w", pvc.Name, next.String(), err)
			}
		}
	}
	// For CNPG this is what grows the volumes: resolvePostgres applies the
	// Cluster with it later in this same reconcile.
	st.Size = next.String()
	r.Recorder.Eventf(topo, corev1.EventTypeNormal, "PostgresStorageGrown", "growing postgres volumes from %s to %s, %d%% were used",
		reading.current.String(), next.String(), reading.percent())
	logf.FromContext(ctx).Info("Grew postgres PersistentVolumeClaims", "from", reading.current.String(), "to", next.String())
	return nil
}

// setPostgresStorageExhausted records, as ConditionPostgresStorageExhausted,
// whether the postgres volume is too full to keep putting load on: full at
// its maximum size (see postgresStorageReading.atMax), full and growing it
// failed (growErr), or past the hard limit whatever its size - which is what
// catches a volume whose resize never completes, such as one that needs
// offline expansion or a CNPG Cluster that can't resize.
func setPostgresStorageExhausted(topo *benchv1alpha1.AuthentikDeployment, reading postgresStorageReading, limit resource.Quantity, growErr error) {
	var reason, message string
	switch {
	case reading.atMax(limit):
		reason = benchv1alpha1.ReasonPostgresStorageAtMax
		message = fmt.Sprintf("postgres volume is %d%% full and already at maxStorageSize %s (raise it or reset the deployment)",
			reading.percent(), limit.String())
	case growErr != nil:
		reason = benchv1alpha1.ReasonPostgresStorageGrowFailed
		message = fmt.Sprintf("postgres volume is %d%% full and could not grow: %v", reading.percent(), growErr)
	case render.PostgresStorageAtHardLimit(reading.used, reading.capacity):
		reason = benchv1alpha1.ReasonPostgresStorageHardLimit
		message = fmt.Sprintf("postgres volume is %d%% full, past the %d%% hard limit (growing it hasn't kept up or isn't completing)",
			reading.percent(), render.PostgresStorageHardLimitPercent)
	default:
		meta.RemoveStatusCondition(&topo.Status.Conditions, benchv1alpha1.ConditionPostgresStorageExhausted)
		return
	}
	meta.SetStatusCondition(&topo.Status.Conditions, metav1.Condition{
		Type:               benchv1alpha1.ConditionPostgresStorageExhausted,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: topo.Generation,
	})
}

// postgresPVCs lists the data PVCs of the deployment's operator-managed
// postgres.
func postgresPVCs(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment) ([]corev1.PersistentVolumeClaim, error) {
	if topo.Spec.Postgres.Mode == benchv1alpha1.PostgresCNPG {
		var list corev1.PersistentVolumeClaimList
		if err := rc.List(ctx, &list, client.InNamespace(topo.Spec.Namespace), client.MatchingLabels(render.CNPGDataPVCLabels(topo))); err != nil {
			return nil, err
		}
		return list.Items, nil
	}
	var pvc corev1.PersistentVolumeClaim
	err := rc.Get(ctx, types.NamespacedName{Name: render.EmbeddedPostgresPVCName(topo), Namespace: topo.Spec.Namespace}, &pvc)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []corev1.PersistentVolumeClaim{pvc}, nil
}

// kubeletSummary is the part of the kubelet's /stats/summary response
// (k8s.io/kubelet/pkg/apis/stats/v1alpha1.Summary) volume usage needs,
// declared here rather than pulling in k8s.io/kubelet for it.
type kubeletSummary struct {
	Pods []struct {
		Volume []struct {
			PVCRef *struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
			UsedBytes     *uint64 `json:"usedBytes"`
			CapacityBytes *uint64 `json:"capacityBytes"`
		} `json:"volume"`
	} `json:"pods"`
}

// kubeletVolumeUsage is the default volumeUsageFunc: it asks the kubelet of
// each node running a pod that mounts one of pvcs, through the workload
// cluster API server's node proxy.
func (r *AuthentikDeploymentReconciler) kubeletVolumeUsage(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment, pvcs []corev1.PersistentVolumeClaim) (map[string]volumeUsage, error) {
	if r.Tracker == nil || topo.Spec.ClusterRef.Name == "" {
		return nil, fmt.Errorf("no workload cluster to read volume usage from")
	}
	cfg, err := r.Tracker.GetRESTConfig(ctx, client.ObjectKey{Namespace: topo.Namespace, Name: topo.Spec.ClusterRef.Name})
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	wanted := make(map[string]bool, len(pvcs))
	for _, pvc := range pvcs {
		wanted[pvc.Name] = true
	}
	// Listed through the clientset rather than rc so this doesn't start a
	// cluster-wide Pod informer just to find a few nodes.
	pods, err := cs.CoreV1().Pods(topo.Spec.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	nodes := map[string]bool{}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && wanted[v.PersistentVolumeClaim.ClaimName] {
				nodes[pod.Spec.NodeName] = true
			}
		}
	}

	usage := map[string]volumeUsage{}
	for node := range nodes {
		raw, err := cs.CoreV1().RESTClient().Get().Resource("nodes").Name(node).SubResource("proxy").Suffix("stats/summary").DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("get stats summary of node %s: %w", node, err)
		}
		var summary kubeletSummary
		if err := json.Unmarshal(raw, &summary); err != nil {
			return nil, fmt.Errorf("decode stats summary of node %s: %w", node, err)
		}
		for _, pod := range summary.Pods {
			for _, v := range pod.Volume {
				if v.PVCRef == nil || v.PVCRef.Namespace != topo.Spec.Namespace || !wanted[v.PVCRef.Name] ||
					v.UsedBytes == nil || v.CapacityBytes == nil {
					continue
				}
				usage[v.PVCRef.Name] = volumeUsage{used: int64(*v.UsedBytes), capacity: int64(*v.CapacityBytes)}
			}
		}
	}
	return usage, nil
}
