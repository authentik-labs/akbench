package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		if st != nil && st.Size == "" {
			topo.Status.PostgresStorage = nil
		} else if st != nil {
			topo.Status.PostgresStorage = &benchv1alpha1.PostgresStorageStatus{Size: st.Size}
		}
		return 0
	}
	limit, err := resource.ParseQuantity(maxSize)
	if err != nil {
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

	now := metav1.Now()
	st.LastChecked = &now
	st.UsedBytes, st.CapacityBytes = 0, 0
	var current resource.Quantity
	resizing := false
	for _, pvc := range pvcs {
		requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if requested.Cmp(current) > 0 {
			current = requested
		}
		if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok && capacity.Cmp(requested) < 0 {
			resizing = true
		}
		// Track the fullest volume by ratio; they all grow together.
		if u, ok := usage[pvc.Name]; ok && u.capacity > 0 &&
			(st.CapacityBytes == 0 || u.used*st.CapacityBytes > st.UsedBytes*u.capacity) {
			st.UsedBytes, st.CapacityBytes = u.used, u.capacity
		}
	}
	// Recover a grown size the status lost (or never had), so CNPGCluster
	// doesn't render one smaller than the volumes already are.
	if grown, err := resource.ParseQuantity(st.Size); err != nil || current.Cmp(grown) > 0 {
		st.Size = current.String()
	}
	if resizing {
		return postgresStorageCheckInterval
	}

	next, grow := render.NextPostgresStorageSize(current, limit, st.UsedBytes, st.CapacityBytes)
	if !grow {
		if render.PostgresStorageFull(st.UsedBytes, st.CapacityBytes) && current.Cmp(limit) >= 0 {
			r.Recorder.Eventf(topo, corev1.EventTypeWarning, "PostgresStorageAtMax",
				"postgres volume is %d%% full and already at maxStorageSize %s", st.UsedBytes*100/st.CapacityBytes, maxSize)
		}
		return postgresStorageCheckInterval
	}

	if topo.Spec.Postgres.Mode == benchv1alpha1.PostgresEmbedded {
		for i := range pvcs {
			pvc := &pvcs[i]
			patch := client.MergeFrom(pvc.DeepCopy())
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = next
			if err := rc.Patch(ctx, pvc, patch); err != nil {
				r.Recorder.Eventf(topo, corev1.EventTypeWarning, "PostgresStorageGrowFailed", "failed to grow PVC %s to %s: %v", pvc.Name, next.String(), err)
				return postgresStorageCheckInterval
			}
		}
	}
	// For CNPG this is what grows the volumes: resolvePostgres applies the
	// Cluster with it later in this same reconcile.
	st.Size = next.String()
	r.Recorder.Eventf(topo, corev1.EventTypeNormal, "PostgresStorageGrown", "growing postgres volumes from %s to %s, %d%% were used",
		current.String(), next.String(), st.UsedBytes*100/st.CapacityBytes)
	log.Info("Grew postgres PersistentVolumeClaims", "from", current.String(), "to", next.String())
	return postgresStorageCheckInterval
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
