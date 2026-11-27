package v1alpha1

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/capacity"
	"github.com/authentik-labs/akbench/internal/multicluster"
	"github.com/authentik-labs/akbench/internal/render"
)

// capacityTimeout bounds reading a workload cluster's nodes and pods, well
// inside the API server's 10s webhook timeout.
const capacityTimeout = 8 * time.Second

// ClusterClients resolves a workload cluster to a live client
// (multicluster.Tracker).
type ClusterClients interface {
	GetUncachedClient(ctx context.Context, cluster client.ObjectKey) (client.Client, error)
}

// CapacityChecker rejects AuthentikDeployments and TestCases whose pods
// can't fit on their workload cluster, even once cluster-autoscaler has
// grown every pool it may. Anything that keeps it from knowing - the
// cluster isn't a WorkloadCluster, isn't reachable yet, or can't be read -
// yields a warning rather than a rejection.
type CapacityChecker struct {
	// Mgmt reads WorkloadClusters, MachineDeployments and
	// AuthentikDeployments on the management cluster.
	Mgmt     client.Reader
	Clusters ClusterClients
}

// newCapacityChecker is nil (no capacity check) without a tracker.
func newCapacityChecker(mgr ctrl.Manager, tracker *multicluster.Tracker) *CapacityChecker {
	if tracker == nil {
		return nil
	}
	return &CapacityChecker{Mgmt: mgr.GetClient(), Clusters: tracker}
}

// load reads the named WorkloadCluster's current state. A nil Cluster
// means "can't tell", and the returned warning says why.
func (c *CapacityChecker) load(ctx context.Context, namespace, name string) (*capacity.Cluster, string) {
	skipped := func(why string, err error) (*capacity.Cluster, string) {
		return nil, fmt.Sprintf("capacity of cluster %q was not checked: %s: %v", name, why, err)
	}
	var wc benchv1alpha1.WorkloadCluster
	if err := c.Mgmt.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &wc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("capacity of cluster %q was not checked: it is not a WorkloadCluster", name)
		}
		return skipped("could not read its WorkloadCluster", err)
	}
	remote, err := c.Clusters.GetUncachedClient(ctx, client.ObjectKey{Namespace: namespace, Name: name})
	if err != nil {
		return skipped("it is not reachable yet", err)
	}
	var nodes corev1.NodeList
	if err := remote.List(ctx, &nodes); err != nil {
		return skipped("could not list its nodes", err)
	}
	var pods corev1.PodList
	if err := remote.List(ctx, &pods); err != nil {
		return skipped("could not list its pods", err)
	}
	var mds clusterv1.MachineDeploymentList
	if err := c.Mgmt.List(ctx, &mds, client.InNamespace(namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: name}); err != nil {
		return skipped("could not list its MachineDeployments", err)
	}
	replicas := map[string]int32{}
	for _, md := range mds.Items {
		if md.Spec.Replicas != nil {
			replicas[md.Name] = *md.Spec.Replicas
		}
	}
	return &capacity.Cluster{WorkloadCluster: &wc, Nodes: nodes.Items, Pods: pods.Items, MachineDeploymentReplicas: replicas}, ""
}

// fit loads cluster and fits groups onto the nodes placement allows. It
// returns an error only when they definitely don't fit, and a warning when
// it couldn't tell.
func (c *CapacityChecker) fit(ctx context.Context, namespace, cluster string, placement capacity.Placement, ignore func(*corev1.Pod) bool, groups []capacity.Group) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, capacityTimeout)
	defer cancel()
	state, warning := c.load(ctx, namespace, cluster)
	if state == nil {
		return warning, nil
	}
	return "", capacity.Fit(state.Pool(placement, ignore), groups)
}

// effectiveRequests is what the scheduler reserves for a container with
// these resources: its requests, and for a resource with a limit but no
// request, the limit - Kubernetes defaults the request to it. Counting the
// requests alone would admit a limits-only spec that cannot be scheduled.
func effectiveRequests(r corev1.ResourceRequirements) corev1.ResourceList {
	if len(r.Limits) == 0 {
		return r.Requests
	}
	effective := corev1.ResourceList{}
	maps.Copy(effective, r.Limits)
	maps.Copy(effective, r.Requests)
	return effective
}

// deploymentPods is every pod an AuthentikDeployment schedules onto its
// cluster's authentik pool, grouped the way its PlacementMode constrains
// them (render.ServerWorkerAffinity): Dedicated spreads, Packed co-locates
// the release's own pods - server, worker and the bundled postgres.
func deploymentPods(d *benchv1alpha1.AuthentikDeployment) []capacity.Group {
	pod := func(name string, requests corev1.ResourceList) capacity.Pod {
		return capacity.Pod{Name: name, Requests: requests, Deployment: d.Name, Isolating: true}
	}
	var release []capacity.Pod
	for i := range d.Spec.Server.Replicas {
		release = append(release, pod(fmt.Sprintf("server pod %d", i+1), effectiveRequests(d.Spec.Server.Resources)))
	}
	for i := range d.Spec.Worker.Replicas {
		release = append(release, pod(fmt.Sprintf("worker pod %d", i+1), effectiveRequests(d.Spec.Worker.Resources)))
	}
	var cnpg []capacity.Pod
	switch d.Spec.Postgres.Mode {
	case benchv1alpha1.PostgresEmbedded, "":
		release = append(release, pod("postgres pod", nil))
	case benchv1alpha1.PostgresCNPG:
		instances := int32(3)
		if d.Spec.Postgres.CNPG != nil && d.Spec.Postgres.CNPG.Instances > 0 {
			instances = d.Spec.Postgres.CNPG.Instances
		}
		for i := range instances {
			cnpg = append(cnpg, pod(fmt.Sprintf("postgres instance %d", i+1), nil))
		}
	}
	groups := []capacity.Group{{
		Pods:   release,
		Spread: d.Spec.Placement.Mode == benchv1alpha1.PlacementDedicated,
		Packed: d.Spec.Placement.Mode == benchv1alpha1.PlacementPacked,
	}}
	if len(cnpg) > 0 {
		groups = append(groups, capacity.Group{Pods: cnpg})
	}
	return groups
}

// deploymentPlacement is where an AuthentikDeployment's pods may go.
func deploymentPlacement(d *benchv1alpha1.AuthentikDeployment) capacity.Placement {
	return capacity.Placement{
		Role:         benchv1alpha1.WorkerPoolRoleAuthentik,
		NodeSelector: render.NodeSelector(d.Spec.Placement),
		Tolerations:  render.Tolerations(d.Spec.Placement),
	}
}

// ownDeploymentPod matches an AuthentikDeployment's own running pods (not a
// k6 runner aimed at it), which an update replaces rather than adds to.
func ownDeploymentPod(name string) func(*corev1.Pod) bool {
	return func(p *corev1.Pod) bool {
		return p.Labels[benchv1alpha1.LabelDeployment] == name && p.Labels[benchv1alpha1.LabelTestCase] == ""
	}
}

// runnerPod is the k6 pod a TestCase runs per target - a Burn Deployment's
// one replica, or one Quick run's Job.
func runnerPod(tc *benchv1alpha1.TestCase, target string) capacity.Pod {
	resources := render.DefaultRunnerResources()
	if tc.Spec.RunnerResources != nil {
		resources = *tc.Spec.RunnerResources
	}
	return capacity.Pod{Name: fmt.Sprintf("k6 runner for %q", target), Requests: effectiveRequests(resources), Deployment: target}
}
