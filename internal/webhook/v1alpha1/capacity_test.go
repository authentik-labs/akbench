package v1alpha1

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// fakeClusters hands out one fake workload cluster client, or err.
type fakeClusters struct {
	remote client.Client
	err    error
}

func (f fakeClusters) GetUncachedClient(context.Context, client.ObjectKey) (client.Client, error) {
	return f.remote, f.err
}

func requests(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}
}

func poolNode(name, pool string) *corev1.Node {
	alloc := requests("4", "8Gi")
	alloc[corev1.ResourcePods] = resource.MustParse("110")
	return &corev1.Node{
		Name:   name,
		Labels: map[string]string{render.NodePoolLabel: pool},
		Spec:   corev1.NodeSpec{Taints: []corev1.Taint{{Key: render.NodePoolLabel, Value: pool, Effect: corev1.TaintEffectNoSchedule}}},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

var _ = Describe("Capacity check", func() {
	var (
		wc      *benchv1alpha1.WorkloadCluster
		mgmt    []client.Object
		remote  []client.Object
		clusErr error
	)

	checker := func() *CapacityChecker {
		s := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(s)).To(Succeed())
		Expect(benchv1alpha1.AddToScheme(s)).To(Succeed())
		Expect(clusterv1.AddToScheme(s)).To(Succeed())
		return &CapacityChecker{
			Mgmt:     fake.NewClientBuilder().WithScheme(s).WithObjects(mgmt...).Build(),
			Clusters: fakeClusters{remote: fake.NewClientBuilder().WithScheme(s).WithObjects(remote...).Build(), err: clusErr},
		}
	}

	deployment := func(name string, replicas int32) *benchv1alpha1.AuthentikDeployment {
		return &benchv1alpha1.AuthentikDeployment{
			Name: name, Namespace: "default",
			Spec: benchv1alpha1.AuthentikDeploymentSpec{
				ClusterRef: corev1.LocalObjectReference{Name: "wc"},
				Server:     benchv1alpha1.WorkloadSpec{Replicas: replicas, Resources: corev1.ResourceRequirements{Requests: requests("2", "2Gi")}},
				Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1, Resources: corev1.ResourceRequirements{Requests: requests("1", "1Gi")}},
				Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresExternal},
			},
		}
	}

	BeforeEach(func() {
		clusErr = nil
		wc = &benchv1alpha1.WorkloadCluster{
			Name: "wc", Namespace: "default",
			Spec: benchv1alpha1.WorkloadClusterSpec{WorkerPools: []benchv1alpha1.WorkerPoolSpec{
				{Name: "authentik", Role: benchv1alpha1.WorkerPoolRoleAuthentik, Replicas: 1,
					Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c4-m8"}, Autoscaling: &benchv1alpha1.AutoscalingSpec{MaxReplicas: 2}},
				{Name: "k6", Role: benchv1alpha1.WorkerPoolRoleK6, Replicas: 1, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c2-m4"}},
			}},
		}
		replicas := int32(1)
		mgmt = []client.Object{wc, &clusterv1.MachineDeployment{
			Name: "wc-authentik", Namespace: "default",
			Labels: map[string]string{clusterv1.ClusterNameLabel: "wc"},
			Spec:   clusterv1.MachineDeploymentSpec{ClusterName: "wc", Replicas: &replicas},
		}}
		remote = []client.Object{poolNode("wc-authentik-a-1", "authentik"), poolNode("wc-k6-a-1", "k6")}
	})

	Context("AuthentikDeployment", func() {
		It("admits a deployment that fits once the autoscaler adds a node", func() {
			// 2x2 + 1 cpu = 5 cpu: more than the one 4-cpu node, within two.
			v := AuthentikDeploymentValidator{Capacity: checker()}
			Expect(v.ValidateCreate(ctx, deployment("ak", 2))).To(BeEmpty())
		})

		It("rejects a deployment that doesn't fit even at the autoscaler's max", func() {
			v := AuthentikDeploymentValidator{Capacity: checker()}
			_, err := v.ValidateCreate(ctx, deployment("ak", 4))
			Expect(err).To(MatchError(ContainSubstring("not enough capacity")))
			Expect(err).To(MatchError(ContainSubstring("1 cluster-autoscaler can add")))
		})

		It("counts a limit as the request when no request is set", func() {
			// Kubernetes defaults a missing request to the limit, so this
			// reserves as much as the rejected deployment above.
			d := deployment("ak", 4)
			for _, w := range []*benchv1alpha1.WorkloadSpec{&d.Spec.Server, &d.Spec.Worker} {
				w.Resources.Limits, w.Resources.Requests = w.Resources.Requests, nil
			}
			v := AuthentikDeploymentValidator{Capacity: checker()}
			_, err := v.ValidateCreate(ctx, d)
			Expect(err).To(MatchError(ContainSubstring("not enough capacity")))
		})

		It("won't place a deployment on another deployment's node", func() {
			remote = append(remote, &corev1.Pod{
				Name: "other-server", Namespace: "other",
				Labels: map[string]string{benchv1alpha1.LabelDeployment: "other"},
				Spec:   corev1.PodSpec{NodeName: "wc-authentik-a-1", Containers: []corev1.Container{{Name: "c"}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			})
			v := AuthentikDeploymentValidator{Capacity: checker()}
			// Only the one autoscaler node is left: 5 cpu won't fit on it.
			_, err := v.ValidateCreate(ctx, deployment("ak", 2))
			Expect(err).To(MatchError(ContainSubstring("not enough capacity")))
			// 1 server + 1 worker = 3 cpu does.
			Expect(v.ValidateCreate(ctx, deployment("ak", 1))).To(BeEmpty())
		})

		It("doesn't count the deployment's own running pods against an update", func() {
			remote = append(remote, &corev1.Pod{
				Name: "ak-server", Namespace: "ak",
				Labels: map[string]string{benchv1alpha1.LabelDeployment: "ak"},
				Spec: corev1.PodSpec{NodeName: "wc-authentik-a-1", Containers: []corev1.Container{
					{Name: "c", Resources: corev1.ResourceRequirements{Requests: requests("3", "2Gi")}},
				}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			})
			v := AuthentikDeploymentValidator{Capacity: checker()}
			old := deployment("ak", 1)
			Expect(v.ValidateUpdate(ctx, old, deployment("ak", 2))).To(BeEmpty())
		})

		It("skips the check when nothing that's scheduled changed", func() {
			v := AuthentikDeploymentValidator{Capacity: checker()}
			old := deployment("ak", 4)
			updated := old.DeepCopy()
			updated.Spec.Authentik.LogLevel = "debug"
			Expect(v.ValidateUpdate(ctx, old, updated)).To(BeEmpty())
		})

		It("only warns when the cluster isn't reachable", func() {
			clusErr = errors.New("cluster not connected")
			v := AuthentikDeploymentValidator{Capacity: checker()}
			warnings, err := v.ValidateCreate(ctx, deployment("ak", 4))
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("not reachable yet")))
		})
	})

	Context("TestCase", func() {
		testCase := func(targets ...string) *benchv1alpha1.TestCase {
			tc := &benchv1alpha1.TestCase{
				Name: "burn", Namespace: "default",
				Spec: benchv1alpha1.TestCaseSpec{Profile: benchv1alpha1.ProfileBurn},
			}
			for _, t := range targets {
				tc.Spec.DeploymentRefs = append(tc.Spec.DeploymentRefs, corev1.LocalObjectReference{Name: t})
			}
			return tc
		}

		BeforeEach(func() {
			mgmt = append(mgmt, deployment("a", 1), deployment("b", 1), deployment("c", 1))
		})

		It("admits runners that fit the k6 pool", func() {
			// The one 4-cpu k6 node fits up to four 1-cpu runners,
			// one per target (runners for different targets may share a node).
			v := TestCaseValidator{Capacity: checker()}
			Expect(v.ValidateCreate(ctx, testCase("a", "b", "c"))).To(BeEmpty())
		})

		It("rejects runners that don't fit", func() {
			tc := testCase("a", "b")
			tc.Spec.RunnerResources = &corev1.ResourceRequirements{Requests: requests("3", "1Gi")}
			v := TestCaseValidator{Capacity: checker()}
			_, err := v.ValidateCreate(ctx, tc)
			Expect(err).To(MatchError(ContainSubstring(`k6 runner for "b"`)))
		})

		It("counts a runner's limit as its request when no request is set", func() {
			tc := testCase("a", "b")
			tc.Spec.RunnerResources = &corev1.ResourceRequirements{Limits: requests("3", "1Gi")}
			v := TestCaseValidator{Capacity: checker()}
			_, err := v.ValidateCreate(ctx, tc)
			Expect(err).To(MatchError(ContainSubstring(`k6 runner for "b"`)))
		})

		It("skips targets that don't exist yet", func() {
			v := TestCaseValidator{Capacity: checker()}
			Expect(v.ValidateCreate(ctx, testCase("missing"))).To(BeEmpty())
		})
	})
})

var _ = Describe("effectiveRequests", func() {
	q := resource.MustParse
	It("is the requests when there are no limits", func() {
		requests := corev1.ResourceList{corev1.ResourceCPU: q("500m")}
		Expect(effectiveRequests(corev1.ResourceRequirements{Requests: requests})).To(Equal(requests))
		Expect(effectiveRequests(corev1.ResourceRequirements{})).To(BeEmpty())
	})

	It("takes a limit for each resource without a request", func() {
		got := effectiveRequests(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: q("2"), corev1.ResourceMemory: q("1Gi")},
		})
		Expect(got).To(HaveLen(2))
		Expect(got.Cpu().String()).To(Equal("500m"), "the request wins where both are set")
		Expect(got.Memory().String()).To(Equal("1Gi"), "the limit stands in for a missing request")
	})
})
