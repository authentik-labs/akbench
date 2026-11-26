package render

import (
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestKubeadmControlPlaneBindsDualStack(t *testing.T) {
	wc := &benchv1alpha1.WorkloadCluster{
		Name: "wc-a", Namespace: "default",
		Spec: benchv1alpha1.WorkloadClusterSpec{
			KubernetesVersion: "v1.31.0",
			Provider:          benchv1alpha1.InfrastructureIncus,
			ControlPlane:      benchv1alpha1.MachineSpec{Replicas: 1},
			WorkerPools:       []benchv1alpha1.WorkerPoolSpec{{Name: "workers", Replicas: 2}},
		},
	}

	kcp := KubeadmControlPlane(wc)

	var bindAddress *string
	for _, arg := range kcp.Spec.KubeadmConfigSpec.ClusterConfiguration.APIServer.ExtraArgs {
		if arg.Name == "bind-address" {
			bindAddress = arg.Value
		}
	}
	if bindAddress == nil || *bindAddress != "::" {
		t.Fatalf("apiServer.extraArgs bind-address = %v, want \"::\" - kube-apiserver must listen on "+
			"both address families since Incus's load balancer address family isn't under our control",
			bindAddress)
	}
}

func TestLXCClusterOverridesHAProxyForDualStack(t *testing.T) {
	wc := &benchv1alpha1.WorkloadCluster{
		Name: "wc-a", Namespace: "default",
		Spec: benchv1alpha1.WorkloadClusterSpec{
			KubernetesVersion: "v1.31.0",
			Provider:          benchv1alpha1.InfrastructureIncus,
			ControlPlane:      benchv1alpha1.MachineSpec{Replicas: 1},
			WorkerPools:       []benchv1alpha1.WorkerPoolSpec{{Name: "workers", Replicas: 2}},
			Incus: &benchv1alpha1.IncusProviderSpec{
				LXCSecretRef: corev1.LocalObjectReference{Name: "lxc-secret"},
			},
		},
	}

	lxcCluster := LXCCluster(wc)

	tmpl, found, err := unstructured.NestedString(lxcCluster.Object, "spec", "loadBalancer", "lxc", "customHAProxyConfigTemplate")
	if err != nil || !found {
		t.Fatalf("customHAProxyConfigTemplate not set: found=%v err=%v", found, err)
	}
	// cluster-api-provider-incus's own default template only adds this line
	// when a configData.IPv6 flag is set, which its own code never sets - see
	// dualStackHAProxyConfigTemplate's doc comment. Our override must not
	// depend on that flag at all.
	if !strings.Contains(tmpl, "bind :::{{ .FrontendControlPlanePort }}") {
		t.Fatalf("customHAProxyConfigTemplate missing the unconditional IPv6 bind line:\n%s", tmpl)
	}
	if !strings.Contains(tmpl, "bind *:{{ .FrontendControlPlanePort }}") {
		t.Fatalf("customHAProxyConfigTemplate missing the IPv4 bind line:\n%s", tmpl)
	}
	// Must stay compatible with the exact fields/funcs
	// internal/loadbalancer.renderHaproxyConfiguration feeds into the
	// template today.
	for _, want := range []string{"{{ if .EnableHealthzCheck", "{{range $server, $backend := .BackendServers}}", "JoinHostPort"} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("customHAProxyConfigTemplate missing expected upstream template fragment %q:\n%s", want, tmpl)
		}
	}
}

func wcWithPools(pools ...benchv1alpha1.WorkerPoolSpec) *benchv1alpha1.WorkloadCluster {
	return &benchv1alpha1.WorkloadCluster{
		Name: "wc-a", Namespace: "default",
		Spec: benchv1alpha1.WorkloadClusterSpec{
			KubernetesVersion: "v1.31.0",
			Provider:          benchv1alpha1.InfrastructureIncus,
			ControlPlane:      benchv1alpha1.MachineSpec{Replicas: 1},
			WorkerPools:       pools,
			Incus: &benchv1alpha1.IncusProviderSpec{
				LXCSecretRef: corev1.LocalObjectReference{Name: "lxc-secret"},
				Image:        "ubuntu:24.04",
			},
		},
	}
}

// TestMultiplePoolsGetDistinctNames guards the "add another pool of workers
// with a different instance type" feature: two pools with different names
// must render distinct MachineDeployments/LXCMachineTemplates, not collide
// or overwrite each other.
func TestMultiplePoolsGetDistinctNames(t *testing.T) {
	wc := wcWithPools(
		benchv1alpha1.WorkerPoolSpec{Name: "small", Replicas: 1, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c1-m2"}},
		benchv1alpha1.WorkerPoolSpec{Name: "large", Replicas: 3, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c8-m16"}},
	)

	small := MachineDeployment(wc, wc.Spec.WorkerPools[0])
	large := MachineDeployment(wc, wc.Spec.WorkerPools[1])

	if small.Name == large.Name {
		t.Fatalf("both pools rendered the same MachineDeployment name %q", small.Name)
	}
	if *small.Spec.Replicas != 1 || *large.Spec.Replicas != 3 {
		t.Fatalf("replicas = %d/%d, want 1/3", *small.Spec.Replicas, *large.Spec.Replicas)
	}
	if small.Spec.Template.Spec.InfrastructureRef.Name == large.Spec.Template.Spec.InfrastructureRef.Name {
		t.Fatalf("both pools' MachineDeployments reference the same LXCMachineTemplate %q - a flavor edit to one pool would roll the other too",
			small.Spec.Template.Spec.InfrastructureRef.Name)
	}
}

// TestWorkerKubeadmConfigTemplateTaintsByRole guards the node-pool
// taint/label scheme itself: a non-Generic pool's worker nodes must register
// with both the matching taint (what a workload's toleration permits) and
// the matching node label (what a workload's nodeSelector actually matches
// against) - and a Generic pool (the zero value, and every pool created
// before this field existed) must get neither, so it schedules exactly as
// it always did.
func TestWorkerKubeadmConfigTemplateTaintsByRole(t *testing.T) {
	wc := &benchv1alpha1.WorkloadCluster{Name: "wc-a", Namespace: "default"}

	k6Pool := benchv1alpha1.WorkerPoolSpec{Name: "k6", Role: benchv1alpha1.WorkerPoolRoleK6, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c2-m4"}}
	tmpl := WorkerKubeadmConfigTemplate(wc, k6Pool)
	nodeReg := tmpl.Spec.Template.Spec.JoinConfiguration.NodeRegistration
	if nodeReg.Taints == nil || len(*nodeReg.Taints) != 1 {
		t.Fatalf("taints = %v, want exactly one", nodeReg.Taints)
	}
	taint := (*nodeReg.Taints)[0]
	if taint.Key != NodePoolLabel || taint.Value != "k6" || taint.Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("taint = %+v, want {%s k6 NoSchedule}", taint, NodePoolLabel)
	}
	var sawLabel bool
	for _, arg := range nodeReg.KubeletExtraArgs {
		if arg.Name == "node-labels" {
			sawLabel = true
			if arg.Value == nil || *arg.Value != NodePoolLabel+"=k6" {
				t.Errorf("node-labels = %v, want %s=k6", arg.Value, NodePoolLabel)
			}
		}
	}
	if !sawLabel {
		t.Error("no --node-labels kubelet arg set for a K6-role pool")
	}

	genericPool := benchv1alpha1.WorkerPoolSpec{Name: "generic", Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c2-m4"}}
	generic := WorkerKubeadmConfigTemplate(wc, genericPool)
	genericReg := generic.Spec.Template.Spec.JoinConfiguration.NodeRegistration
	if genericReg.Taints == nil || len(*genericReg.Taints) != 0 {
		t.Fatalf("Generic pool taints = %v, want an explicit empty slice", genericReg.Taints)
	}
	for _, arg := range genericReg.KubeletExtraArgs {
		if arg.Name == "node-labels" {
			t.Errorf("Generic pool got a --node-labels arg: %v", arg.Value)
		}
	}
}

// TestWorkerMachineTemplateNameChangesWithFlavor guards the day-2 "change a
// pool's instance type" path: WorkloadClusterReconciler relies on the
// template's *name* changing to get Cluster API to roll the pool's machines
// (see workerMachineTemplateName's doc comment) - editing the template
// object's content in place would silently do nothing.
func TestWorkerMachineTemplateNameChangesWithFlavor(t *testing.T) {
	small := benchv1alpha1.WorkerPoolSpec{Name: "workers", Replicas: 2, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c1-m2"}}
	large := benchv1alpha1.WorkerPoolSpec{Name: "workers", Replicas: 2, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c8-m16"}}

	nameForSmall := workerMachineTemplateName(wcWithPools(small), small)
	nameForLarge := workerMachineTemplateName(wcWithPools(large), large)

	if nameForSmall == nameForLarge {
		t.Fatalf("workerMachineTemplateName did not change across a flavor edit: both = %q", nameForSmall)
	}

	// A no-op reconcile (same flavor) must render the exact same name, or
	// applyCreateOnly would create a redundant new template every time.
	again := workerMachineTemplateName(wcWithPools(small), small)
	if again != nameForSmall {
		t.Fatalf("workerMachineTemplateName is not stable across identical input: %q vs %q", nameForSmall, again)
	}
}

// TestMachineDeploymentAutoscalingAnnotations guards the two annotations
// cluster-autoscaler itself watches for (cloudprovider/clusterapi's own
// README, verified directly - see ClusterAutoscalerHelmRelease's doc
// comment) - without them on the rendered object, cluster-autoscaler would
// never recognize this pool as a node group to manage at all, regardless of
// how the rest of its Helm install is configured.
func TestMachineDeploymentAutoscalingAnnotations(t *testing.T) {
	pool := benchv1alpha1.WorkerPoolSpec{
		Name:        "workers",
		Replicas:    2,
		Incus:       benchv1alpha1.IncusMachineSpec{Flavor: "c1-m2"},
		Autoscaling: &benchv1alpha1.AutoscalingSpec{MinReplicas: 1, MaxReplicas: 5},
	}
	md := MachineDeployment(wcWithPools(pool), pool)

	if got := md.Annotations[ClusterAutoscalerMinSizeAnnotation]; got != "1" {
		t.Errorf("min-size annotation = %q, want %q", got, "1")
	}
	if got := md.Annotations[ClusterAutoscalerMaxSizeAnnotation]; got != "5" {
		t.Errorf("max-size annotation = %q, want %q", got, "5")
	}

	// A pool with no autoscaling set must render neither annotation - a
	// present-but-empty annotation would still make cluster-autoscaler
	// treat it as a managed node group.
	fixed := benchv1alpha1.WorkerPoolSpec{Name: "fixed", Replicas: 2, Incus: benchv1alpha1.IncusMachineSpec{Flavor: "c1-m2"}}
	fixedMD := MachineDeployment(wcWithPools(fixed), fixed)
	if _, ok := fixedMD.Annotations[ClusterAutoscalerMinSizeAnnotation]; ok {
		t.Errorf("non-autoscaled pool got the min-size annotation anyway: %v", fixedMD.Annotations)
	}
}

func TestLoadBalancerInstanceName(t *testing.T) {
	// Pinned against cluster-api-provider-incus's own
	// LXCCluster.GetLoadBalancerInstanceName for namespace "default".
	wc := &benchv1alpha1.WorkloadCluster{}
	wc.Name, wc.Namespace = "bench-prod-incus", "default"
	if got := LoadBalancerInstanceName(wc); got != "bench-prod-incus-37a8e-lb" {
		t.Errorf("got %q", got)
	}
}

// addonTestCluster is a WorkloadCluster in "default", whose kubeconfig
// Secret is "<name>-kubeconfig".
func addonTestCluster(name string) *benchv1alpha1.WorkloadCluster {
	return &benchv1alpha1.WorkloadCluster{Name: name, Namespace: "default"}
}

func TestAddonHelmReleaseName(t *testing.T) {
	if got := AddonHelmReleaseName(addonTestCluster("c1"), CNIHelmReleaseName); got != "flannel-c1" {
		t.Errorf("got %q", got)
	}
	long := addonTestCluster(strings.Repeat("a", 50))
	other := addonTestCluster(strings.Repeat("a", 49) + "b")
	got := AddonHelmReleaseName(long, KubePrometheusStackHelmReleaseName)
	if len(got) > 63 || !strings.HasPrefix(got, "kube-prometheus-stack-") {
		t.Errorf("long name %q (%d chars)", got, len(got))
	}
	if got == AddonHelmReleaseName(other, KubePrometheusStackHelmReleaseName) {
		t.Error("truncated names of different clusters collide")
	}
}

func TestMachineDeploymentScaleFromZeroAnnotations(t *testing.T) {
	wc := addonTestCluster("c1")
	pool := benchv1alpha1.WorkerPoolSpec{
		Name: "ak", Replicas: 2, Role: benchv1alpha1.WorkerPoolRoleAuthentik,
		Incus:       benchv1alpha1.IncusMachineSpec{Flavor: "c2-m4"},
		Autoscaling: &benchv1alpha1.AutoscalingSpec{MinReplicas: 0, MaxReplicas: 10},
	}
	got := MachineDeployment(wc, pool).Annotations
	want := map[string]string{
		ClusterAutoscalerMinSizeAnnotation:                  "0",
		ClusterAutoscalerMaxSizeAnnotation:                  "10",
		"capacity.cluster-autoscaler.kubernetes.io/cpu":     "2",
		"capacity.cluster-autoscaler.kubernetes.io/memory":  "4Gi",
		"capacity.cluster-autoscaler.kubernetes.io/maxPods": "110",
		"capacity.cluster-autoscaler.kubernetes.io/labels":  "bench.goauthentik.io/pool=authentik",
		"capacity.cluster-autoscaler.kubernetes.io/taints":  "bench.goauthentik.io/pool=authentik:NoSchedule",
	}
	if !maps.Equal(got, want) {
		t.Errorf("annotations = %v, want %v", got, want)
	}
	for key := range want {
		if !IsClusterAutoscalerAnnotation(key) {
			t.Errorf("%s not recognised as a cluster-autoscaler annotation", key)
		}
	}
	if IsClusterAutoscalerAnnotation("machinedeployment.clusters.x-k8s.io/revision") {
		t.Error("Cluster API's revision annotation claimed as ours")
	}

	// A Generic pool with an unknown flavor: no label/taint/size, still
	// scalable from zero, just without size annotations.
	pool.Role, pool.Incus.Flavor = benchv1alpha1.WorkerPoolRoleGeneric, "custom"
	got = MachineDeployment(wc, pool).Annotations
	for _, key := range []string{"cpu", "memory", "labels", "taints"} {
		if _, ok := got["capacity.cluster-autoscaler.kubernetes.io/"+key]; ok {
			t.Errorf("unexpected %s annotation: %v", key, got)
		}
	}

	pool.Autoscaling = nil
	if got := MachineDeployment(wc, pool).Annotations; len(got) != 0 {
		t.Errorf("non-autoscaled pool annotations = %v", got)
	}
}

func TestMachineHealthCheck(t *testing.T) {
	wc := addonTestCluster("c1")
	pool := benchv1alpha1.WorkerPoolSpec{Name: "monitoring", Replicas: 1}
	mhc := MachineHealthCheck(wc, pool)
	md := MachineDeployment(wc, pool)
	if mhc.Name != md.Name || mhc.Spec.ClusterName != "c1" || !maps.Equal(mhc.Spec.Selector.MatchLabels, md.Spec.Selector.MatchLabels) {
		t.Errorf("MachineHealthCheck doesn't target the pool's machines: %+v", mhc)
	}
	if len(mhc.Spec.Checks.UnhealthyNodeConditions) != 2 || mhc.Spec.Checks.NodeStartupTimeoutSeconds == nil {
		t.Errorf("checks = %+v", mhc.Spec.Checks)
	}
	// A one-node pool must stay remediable.
	if mhc.Spec.Remediation.TriggerIf.UnhealthyLessThanOrEqualTo != nil {
		t.Error("an unhealthy percentage limit would block remediating a single-node pool")
	}
}

func TestObservabilityStorage(t *testing.T) {
	wc := addonTestCluster("c1")
	wc.Spec.Incus = &benchv1alpha1.IncusProviderSpec{}
	values, err := KubePrometheusStackHelmValues(wc)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(values.Raw)
	for _, want := range []string{`"storageSpec":{"volumeClaimTemplate"`, `"storage":{"volumeClaimTemplate"`, `"storage":"100Gi"`, `"retentionSize":"87040MB"`, `"retention":"60d"`, `"storageClassName":"incus-block"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("kube-prometheus-stack values missing %s", want)
		}
	}

	wc.Spec.Prometheus = &benchv1alpha1.PrometheusSpec{Retention: "1y", StorageSize: "500Gi"}
	values, err = KubePrometheusStackHelmValues(wc)
	if err != nil {
		t.Fatal(err)
	}
	raw = string(values.Raw)
	for _, want := range []string{`"storage":"500Gi"`, `"retentionSize":"435200MB"`, `"retention":"1y"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("spec.prometheus override missing %s", want)
		}
	}
}
