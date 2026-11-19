package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
	"github.com/authentik-labs/akbench/seeder"
)

// GetUncachedClient stands in for a workload cluster's uncached client, the
// way GetClient (suite_test.go) does for its cached one.
func (envtestClusterCache) GetUncachedClient(context.Context, client.ObjectKey) (client.Client, error) {
	return k8sClient, nil
}

func TestSeederFailureMessage(t *testing.T) {
	failed := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: batchv1.JobReasonBackoffLimitExceeded, Message: "Job has reached the specified backoff limit"}
	deadline := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: batchv1.JobReasonDeadlineExceeded, Message: "Job was active longer than specified deadline"}
	exited := func(reason string, code int32, log string) *corev1.ContainerStateTerminated {
		return &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code, Message: log}
	}
	for name, tc := range map[string]struct {
		term *corev1.ContainerStateTerminated
		job  batchv1.JobCondition
		want string
	}{
		"the seeder's own line": {
			term: exited("Error", 1, "Loaded config\nseeding failed: tenant already seeded with other params\n"),
			job:  failed,
			want: "seeding failed: tenant already seeded with other params",
		},
		"the seeder's line, not a later one": {
			term: exited("Error", 1, "seeding failed: boom\nsome shutdown noise"),
			job:  failed,
			want: "seeding failed: boom",
		},
		// The Job's `sh -c` execs ak, which execs Python: a kill reaches
		// the seeder itself, and no shell is left to log "Killed".
		"killed for using too much memory": {
			term: exited("OOMKilled", 137, "Loaded config\n{\"event\": \"Booting authentik\"}\n"),
			job:  failed,
			want: `container OOMKilled (exit 137): {"event": "Booting authentik"}`,
		},
		"killed without a word": {
			term: exited("OOMKilled", 137, ""),
			job:  failed,
			want: "container OOMKilled (exit 137)",
		},
		"a crash before the seeder could catch it": {
			term: exited("Error", 1, "Traceback (most recent call last):\nModuleNotFoundError: No module named 'pgtrigger'\n"),
			job:  failed,
			want: "container exited with code 1: ModuleNotFoundError: No module named 'pgtrigger'",
		},
		"the deadline, which killed it mid-run": {
			term: exited("Error", 137, "seeding users\n"),
			job:  deadline,
			want: "Job was active longer than specified deadline",
		},
		"no pod left": {
			job:  deadline,
			want: "Job was active longer than specified deadline",
		},
		"nothing at all": {want: "the tenant seeding Job failed"},
	} {
		if got := seederFailureMessage(tc.term, tc.job); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}

	long := exited("Error", 1, "seeding failed: "+strings.Repeat("é", tenantMessageLimit))
	got := seederFailureMessage(long, failed)
	if len(got) > tenantMessageLimit+len("...") || !strings.HasSuffix(got, "...") || !strings.HasPrefix(got, "seeding failed: ") {
		t.Errorf("a long message is cut to %d bytes: %q", len(got), got)
	}
	if !strings.HasSuffix(strings.TrimSuffix(got, "..."), "é") {
		t.Errorf("a long message is cut inside a character: %q", got[len(got)-8:])
	}
}

// failingMapper stands in for an API server that can't be asked which
// kinds it serves.
type failingMapper struct{ meta.RESTMapper }

func (failingMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("connection refused")
}

func TestTenantPresetsInstalled(t *testing.T) {
	gv := benchv1alpha1.GroupVersion
	without := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	if watch, err := tenantPresetsInstalled(without); err != nil || watch {
		t.Errorf("without the CRD: watch %v, err %v; want no watch and no error", watch, err)
	}
	with := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	with.Add(gv.WithKind("TenantPreset"), meta.RESTScopeNamespace)
	if watch, err := tenantPresetsInstalled(with); err != nil || !watch {
		t.Errorf("with the CRD: watch %v, err %v; want a watch", watch, err)
	}
	if _, err := tenantPresetsInstalled(failingMapper{}); err == nil {
		t.Error("an API server that can't be asked must fail setup, not skip the watch")
	}
}

// noPodListClient is the manager's cached client, as far as listing pods
// goes: listing them through it would start a Pod informer for the whole
// cluster, which RBAC doesn't allow.
type noPodListClient struct{ client.Client }

func (c noPodListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		return errors.New("pods listed through the cached client")
	}
	return c.Client.List(ctx, list, opts...)
}

func TestTenantOutcomeBlocksReady(t *testing.T) {
	for _, tc := range []struct {
		outcome tenantOutcome
		blocks  bool
	}{
		{tenantOutcome{status: metav1.ConditionTrue, reason: benchv1alpha1.ReasonReady}, false},
		{tenantOutcome{status: metav1.ConditionFalse, reason: benchv1alpha1.ReasonTenantResetRequired}, false},
		{tenantOutcome{status: metav1.ConditionFalse, reason: benchv1alpha1.ReasonReconciling}, true},
		{tenantOutcome{status: metav1.ConditionFalse, reason: benchv1alpha1.ReasonSeedingFailed}, true},
	} {
		if got := tc.outcome.blocksReady(); got != tc.blocks {
			t.Errorf("%s/%s: blocksReady = %v, want %v", tc.outcome.status, tc.outcome.reason, got, tc.blocks)
		}
	}
}

var _ = Describe("AuthentikDeployment tenant", func() {
	const (
		resourceName = "deployment-tenant"
		presetName   = "tenant-test"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	targetNamespace := "authentik-" + resourceName
	presetSpec := benchv1alpha1.TenantPresetSpec{Seed: 3, Users: 10, Groups: 4, GroupDepth: 2, GroupsPerUser: 2}
	var r *AuthentikDeploymentReconciler

	reconcileDeployment := func() reconcile.Result {
		GinkgoHelper()
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	getDeployment := func() *benchv1alpha1.AuthentikDeployment {
		GinkgoHelper()
		var topo benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, key, &topo)).To(Succeed())
		return &topo
	}
	condition := func(condType string) *metav1.Condition {
		GinkgoHelper()
		cond := meta.FindStatusCondition(getDeployment().Status.Conditions, condType)
		Expect(cond).NotTo(BeNil())
		return cond
	}
	// No helm-controller runs in envtest: fake the install finishing.
	markHelmReleaseReady := func() {
		GinkgoHelper()
		var hr helmv2.HelmRelease
		Expect(k8sClient.Get(ctx, key, &hr)).To(Succeed())
		hr.Status.ObservedGeneration = hr.Generation
		meta.SetStatusCondition(&hr.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "InstallSucceeded", ObservedGeneration: hr.Generation,
		})
		Expect(k8sClient.Status().Update(ctx, &hr)).To(Succeed())
	}
	// A chart upgrade in progress.
	markHelmReleaseNotReady := func() {
		GinkgoHelper()
		var hr helmv2.HelmRelease
		Expect(k8sClient.Get(ctx, key, &hr)).To(Succeed())
		meta.SetStatusCondition(&hr.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Progressing", Message: "upgrading", ObservedGeneration: hr.Generation,
		})
		Expect(k8sClient.Status().Update(ctx, &hr)).To(Succeed())
	}
	setTenant := func(preset string) {
		GinkgoHelper()
		topo := getDeployment()
		topo.Spec.Tenant = nil
		if preset != "" {
			topo.Spec.Tenant = &benchv1alpha1.TenantSpec{PresetRef: corev1.LocalObjectReference{Name: preset}}
		}
		Expect(k8sClient.Update(ctx, topo)).To(Succeed())
	}
	resetDeployment := func() {
		GinkgoHelper()
		topo := getDeployment()
		topo.Annotations = map[string]string{benchv1alpha1.AnnotationResetRequested: "now"}
		Expect(k8sClient.Update(ctx, topo)).To(Succeed())
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(getDeployment().Annotations).NotTo(HaveKey(benchv1alpha1.AnnotationResetRequested))
		}).Should(Succeed())
	}
	// failPod gives the seeding Job a pod that ended the way term says.
	failPod := func(jobName string, term corev1.ContainerStateTerminated) {
		GinkgoHelper()
		pod := &corev1.Pod{
			Name: jobName + "-abcde", Namespace: targetNamespace,
			Labels: map[string]string{batchv1.JobNameLabel: jobName},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "seed-tenant", Image: "authentik"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "seed-tenant", Image: "authentik", State: corev1.ContainerState{Terminated: &term},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	seedJobName := func() string {
		GinkgoHelper()
		topo := getDeployment()
		Expect(topo.Status.Tenant).NotTo(BeNil())
		return render.TenantSeedJobName(topo, topo.Status.Tenant.Params, seeder.Script)
	}
	updatePreset := func(change func(*benchv1alpha1.TenantPresetSpec)) {
		GinkgoHelper()
		var preset benchv1alpha1.TenantPreset
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: presetName, Namespace: "default"}, &preset)).To(Succeed())
		change(&preset.Spec)
		Expect(k8sClient.Update(ctx, &preset)).To(Succeed())
	}
	// No Job controller runs in envtest either: set the conditions it
	// would, with what the API server requires alongside them.
	finishJob := func(name string, condType batchv1.JobConditionType, message string) {
		GinkgoHelper()
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		if condType == batchv1.JobComplete {
			job.Status.Succeeded = 1
			job.Status.CompletionTime = &now
			job.Status.Conditions = append(job.Status.Conditions,
				batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now})
		} else {
			job.Status.Failed = 1
			job.Status.Conditions = append(job.Status.Conditions,
				batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: message, LastTransitionTime: now})
		}
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: condType, Status: corev1.ConditionTrue, Reason: "Test", Message: message, LastTransitionTime: now})
		Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
	}
	// startSeeding takes the deployment to the point where its seeding Job
	// exists, and returns the Job's name.
	startSeeding := func() string {
		GinkgoHelper()
		reconcileDeployment()
		markHelmReleaseReady()
		reconcileDeployment()
		return seedJobName()
	}

	BeforeEach(func() {
		r = &AuthentikDeploymentReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Recorder: &record.FakeRecorder{}, Tracker: testTracker}
		Expect(k8sClient.Create(ctx, &benchv1alpha1.TenantPreset{Name: presetName, Namespace: "default", Spec: presetSpec})).To(Succeed())
		Expect(k8sClient.Create(ctx, &benchv1alpha1.AuthentikDeployment{
			Name: resourceName, Namespace: "default",
			Spec: benchv1alpha1.AuthentikDeploymentSpec{
				ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
				Namespace:  targetNamespace,
				Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
				Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
				Postgres:   benchv1alpha1.PostgresSpec{Mode: benchv1alpha1.PostgresEmbedded},
				Tenant:     &benchv1alpha1.TenantSpec{PresetRef: corev1.LocalObjectReference{Name: presetName}},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: resourceName, Namespace: "default"})
		forceDelete(ctx, &benchv1alpha1.TenantPreset{Name: presetName, Namespace: "default"})
		forceDelete(ctx, &helmv2.HelmRelease{Name: resourceName, Namespace: "default"})
		// The target namespace outlives each spec (envtest never finishes
		// deleting a namespace), so clear out what the next one would trip on.
		var jobs batchv1.JobList
		Expect(k8sClient.List(ctx, &jobs, client.InNamespace(targetNamespace))).To(Succeed())
		for i := range jobs.Items {
			forceDelete(ctx, &jobs.Items[i])
		}
		Expect(k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(targetNamespace))).To(Succeed())
	})

	It("reports no tenant when none is configured, without holding Ready back", func() {
		topo := getDeployment()
		topo.Spec.Tenant = nil
		Expect(k8sClient.Update(ctx, topo)).To(Succeed())
		reconcileDeployment()
		markHelmReleaseReady()
		reconcileDeployment()

		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionTrue))
		Expect(tenant.Message).To(Equal("no tenant configured"))
		Expect(condition(benchv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(getDeployment().Status.Tenant).To(BeNil())
	})

	It("waits for the HelmRelease before seeding", func() {
		reconcileDeployment()
		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionFalse))
		Expect(tenant.Message).To(ContainSubstring("waiting for the HelmRelease"))
		Expect(getDeployment().Status.Tenant).To(BeNil())
	})

	It("waits for a preset that doesn't exist yet", func() {
		forceDelete(ctx, &benchv1alpha1.TenantPreset{Name: presetName, Namespace: "default"})
		reconcileDeployment()
		markHelmReleaseReady()
		result := reconcileDeployment()
		Expect(result.RequeueAfter).To(And(BeNumerically(">", 0), BeNumerically("<=", tenantPresetRecheck)),
			"an install whose TenantPreset CRD came late has no preset watch")

		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionFalse))
		Expect(tenant.Reason).To(Equal(benchv1alpha1.ReasonReconciling))
		Expect(tenant.Message).To(Equal(`TenantPreset "tenant-test" not found`))
		ready := condition(benchv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(Equal(tenant.Message))
		Expect(getDeployment().Status.Tenant).To(BeNil())
	})

	It("records the tenant and starts a seeding Job with its numbers and password", func() {
		reconcileDeployment()
		markHelmReleaseReady()
		result := reconcileDeployment()
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "nothing watches the Job on the workload cluster")

		var password corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: "default"}, &password)).To(Succeed())
		value := password.Data[render.TenantPasswordKey]
		Expect(value).To(HaveLen(64))

		seeded := getDeployment().Status.Tenant
		Expect(seeded).NotTo(BeNil())
		Expect(seeded.State).To(Equal(benchv1alpha1.TenantSeeding))
		Expect(seeded.Preset).To(Equal(presetName))
		Expect(seeded.Params).To(Equal(render.TenantParamsFromPreset(presetSpec)))
		Expect(seeded.PasswordFingerprint).To(Equal(passwordFingerprint(string(value))))

		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seedJobName(), Namespace: targetNamespace}, &job)).To(Succeed())
		env := envMap(job.Spec.Template.Spec.Containers[0].Env)
		Expect(env["TENANT_PARAMS"]).To(Equal(render.SeederParams(seeded.Params)))

		var mirrored corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: targetNamespace}, &mirrored)).To(Succeed())
		Expect(mirrored.Data[render.TenantPasswordKey]).To(Equal(value))
		var script corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.TenantSeederConfigMapName, Namespace: targetNamespace}, &script)).To(Succeed())
		Expect(script.Data[render.TenantSeederScriptKey]).To(Equal(seeder.Script))

		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Reason).To(Equal(benchv1alpha1.ReasonReconciling))
		Expect(tenant.Message).To(ContainSubstring("seeding tenant from preset"))
		ready := condition(benchv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Message).To(Equal(tenant.Message))
	})

	It("records a finished Job as seeded and never re-creates it", func() {
		name := startSeeding()
		finishJob(name, batchv1.JobComplete, "")
		reconcileDeployment()

		seeded := getDeployment().Status.Tenant
		Expect(seeded.State).To(Equal(benchv1alpha1.TenantSeeded))
		Expect(seeded.SeededAt).NotTo(BeNil())
		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionTrue))
		Expect(tenant.Message).To(Equal(`seeded 10 users, 4 groups from preset "tenant-test"`))
		Expect(condition(benchv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(getDeployment().Status.Phase).To(Equal(benchv1alpha1.PhaseReady))

		// The TTL controller deletes the finished Job; that must not
		// start seeding again.
		forceDelete(ctx, &batchv1.Job{Name: name, Namespace: targetNamespace})
		reconcileDeployment()
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the seeding Job was re-created: %v", err)
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeded))
	})

	It("asks for a reset once the preset changes after seeding, staying Ready", func() {
		finishJob(startSeeding(), batchv1.JobComplete, "")
		reconcileDeployment()
		updatePreset(func(s *benchv1alpha1.TenantPresetSpec) { s.Users = 20 })
		reconcileDeployment()

		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionFalse))
		Expect(tenant.Reason).To(Equal(benchv1alpha1.ReasonTenantResetRequired))
		Expect(tenant.Message).To(ContainSubstring("reset the deployment"))
		Expect(condition(benchv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(getDeployment().Status.Tenant.Params.Users).To(BeEquivalentTo(10), "status keeps what was seeded")
	})

	It("asks for a reset once spec.tenant names another preset", func() {
		finishJob(startSeeding(), batchv1.JobComplete, "")
		reconcileDeployment()
		topo := getDeployment()
		topo.Spec.Tenant.PresetRef.Name = "another-preset"
		Expect(k8sClient.Update(ctx, topo)).To(Succeed())
		reconcileDeployment()

		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Reason).To(Equal(benchv1alpha1.ReasonTenantResetRequired))
		Expect(tenant.Message).To(ContainSubstring(`seeded from "tenant-test"`))
		Expect(condition(benchv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("fails the deployment with the seeder's own message", func() {
		name := startSeeding()
		failPod(name, corev1.ContainerStateTerminated{
			ExitCode: 1, Message: "Loaded config\nseeding failed: this authentik build has no PasswordDevice\n",
		})
		finishJob(name, batchv1.JobFailed, "Job has reached the specified backoff limit")
		recorder := record.NewFakeRecorder(100)
		r.Recorder = recorder
		reconcileDeployment()

		topo := getDeployment()
		Expect(topo.Status.Tenant.State).To(Equal(benchv1alpha1.TenantFailed))
		Expect(topo.Status.Tenant.Message).To(Equal("seeding failed: this authentik build has no PasswordDevice"))
		Expect(topo.Status.Phase).To(Equal(benchv1alpha1.PhaseFailed))
		ready := condition(benchv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal(benchv1alpha1.ReasonSeedingFailed))
		Expect(ready.Message).To(Equal(topo.Status.Tenant.Message))
		var events []string
		for len(recorder.Events) > 0 {
			events = append(events, <-recorder.Events)
		}
		Expect(events).To(ContainElement(HavePrefix("Warning SeedingFailed")))

		// Not retried by itself: it would fail the same way.
		r.Recorder = &record.FakeRecorder{}
		reconcileDeployment()
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantFailed))
	})

	It("tries again once the preset is changed after a failure", func() {
		failed := startSeeding()
		finishJob(failed, batchv1.JobFailed, "Job has reached the specified backoff limit")
		reconcileDeployment()
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantFailed))
		Expect(getDeployment().Status.Tenant.Message).To(Equal("Job has reached the specified backoff limit"))

		updatePreset(func(s *benchv1alpha1.TenantPresetSpec) { s.Users = 5 })
		reconcileDeployment()
		seeded := getDeployment().Status.Tenant
		Expect(seeded.State).To(Equal(benchv1alpha1.TenantSeeding))
		Expect(seeded.Params.Users).To(BeEquivalentTo(5))
		Expect(seedJobName()).NotTo(Equal(failed))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seedJobName(), Namespace: targetNamespace}, &batchv1.Job{})).To(Succeed())
	})

	It("forgets the tenant and deletes its seeding Job on reset, then seeds it again", func() {
		name := startSeeding()
		finishJob(name, batchv1.JobComplete, "")
		reconcileDeployment()
		Expect(getDeployment().Status.Tenant).NotTo(BeNil())

		resetDeployment()
		Expect(getDeployment().Status.Tenant).To(BeNil())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the seeding Job survived the reset: %v", err)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: "default"}, &corev1.Secret{})).
			To(Succeed(), "the password is kept, like every generated Secret")

		// The reinstall: the chart comes back, then seeding starts afresh.
		Expect(startSeeding()).To(Equal(name))
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeding))
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &job)).To(Succeed())
		Expect(job.Status.Conditions).To(BeEmpty(), "a new seeding Job")
		finishJob(name, batchv1.JobComplete, "")
		reconcileDeployment()
		Expect(condition(benchv1alpha1.ConditionTenantSeeded).Status).To(Equal(metav1.ConditionTrue))
	})

	It("tries a failed tenant again after a reset", func() {
		name := startSeeding()
		finishJob(name, batchv1.JobFailed, "Job has reached the specified backoff limit")
		reconcileDeployment()
		Expect(getDeployment().Status.Phase).To(Equal(benchv1alpha1.PhaseFailed))

		resetDeployment()
		Expect(startSeeding()).To(Equal(name), "the same tenant, from the same preset")
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeding))
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &job)).To(Succeed())
		Expect(job.Status.Conditions).To(BeEmpty(), "a new seeding Job")
	})

	It("asks for a reset once the password is replaced, and seeds with the new one after it", func() {
		finishJob(startSeeding(), batchv1.JobComplete, "")
		reconcileDeployment()
		var password corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: "default"}, &password)).To(Succeed())
		password.Data[render.TenantPasswordKey] = []byte("replaced")
		Expect(k8sClient.Update(ctx, &password)).To(Succeed())
		reconcileDeployment()

		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Reason).To(Equal(benchv1alpha1.ReasonTenantResetRequired))
		Expect(tenant.Message).To(ContainSubstring("no longer holds the password the users were given"))
		Expect(condition(benchv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))

		resetDeployment()
		name := startSeeding()
		Expect(getDeployment().Status.Tenant.PasswordFingerprint).To(Equal(passwordFingerprint("replaced")))
		var mirrored corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: targetNamespace}, &mirrored)).To(Succeed())
		Expect(string(mirrored.Data[render.TenantPasswordKey])).To(Equal("replaced"))
		finishJob(name, batchv1.JobComplete, "")
		reconcileDeployment()
		Expect(condition(benchv1alpha1.ConditionTenantSeeded).Status).To(Equal(metav1.ConditionTrue))
	})

	It("asks for a reset once the password Secret is deleted", func() {
		finishJob(startSeeding(), batchv1.JobComplete, "")
		reconcileDeployment()
		forceDelete(ctx, &corev1.Secret{Name: render.TenantPasswordSecretName, Namespace: "default"})
		reconcileDeployment()

		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Reason).To(Equal(benchv1alpha1.ReasonTenantResetRequired))
		Expect(tenant.Message).To(ContainSubstring("no longer holds the password"))
	})

	It("re-creates a seeding Job deleted before it was seen to finish", func() {
		name := startSeeding()
		forceDelete(ctx, &batchv1.Job{Name: name, Namespace: targetNamespace})
		reconcileDeployment()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &batchv1.Job{})).To(Succeed())
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeding))
	})

	It("tries the same preset again once it is removed and chosen again after a failure", func() {
		name := startSeeding()
		finishJob(name, batchv1.JobFailed, "Job has reached the specified backoff limit")
		reconcileDeployment()

		setTenant("")
		reconcileDeployment()
		Expect(getDeployment().Status.Tenant).To(BeNil(), "a failure leaves nothing behind to record")
		Expect(condition(benchv1alpha1.ConditionTenantSeeded).Message).To(Equal("no tenant configured"))
		Expect(getDeployment().Status.Phase).To(Equal(benchv1alpha1.PhaseReady))

		// The failed Job is still there (until its TTL), under the very
		// name the new attempt gets.
		setTenant(presetName)
		reconcileDeployment()
		seeded := getDeployment().Status.Tenant
		Expect(seeded.State).To(Equal(benchv1alpha1.TenantSeeding))
		Expect(seeded.JobName).To(BeEmpty())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the earlier attempt's failed Job was kept: %v", err)

		reconcileDeployment()
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeding))
		Expect(getDeployment().Status.Tenant.JobName).To(Equal(name))
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: targetNamespace}, &job)).To(Succeed())
		Expect(job.Status.Conditions).To(BeEmpty(), "a new seeding Job")
	})

	It("tries again once the image changes after a failure", func() {
		failed := startSeeding()
		finishJob(failed, batchv1.JobFailed, "seeding failed: the seeder does not support this authentik build")
		reconcileDeployment()

		topo := getDeployment()
		topo.Spec.Authentik.Tag = "2026.8.3"
		Expect(k8sClient.Update(ctx, topo)).To(Succeed())
		reconcileDeployment()
		markHelmReleaseReady()
		reconcileDeployment()

		seeded := getDeployment().Status.Tenant
		Expect(seeded.State).To(Equal(benchv1alpha1.TenantSeeding))
		Expect(seeded.JobName).NotTo(BeEmpty())
		Expect(seeded.JobName).NotTo(Equal(failed))
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seeded.JobName, Namespace: targetNamespace}, &job)).To(Succeed())
		Expect(job.Spec.Template.Spec.Containers[0].Image).To(HaveSuffix(":2026.8.3"))
	})

	It("records the new choice if spec.tenant changes before the Job is created", func() {
		reconcileDeployment()
		markHelmReleaseReady()
		// A reconcile that recorded the tenant, then went no further.
		topo := getDeployment()
		topo.Status.Tenant = &benchv1alpha1.TenantStatus{
			Preset: presetName, Params: render.TenantParamsFromPreset(presetSpec), State: benchv1alpha1.TenantSeeding,
		}
		Expect(k8sClient.Status().Update(ctx, topo)).To(Succeed())
		updatePreset(func(s *benchv1alpha1.TenantPresetSpec) { s.Users = 20 })
		reconcileDeployment()

		seeded := getDeployment().Status.Tenant
		Expect(seeded.State).To(Equal(benchv1alpha1.TenantSeeding))
		Expect(seeded.Params.Users).To(BeEquivalentTo(20))
		Expect(seeded.PasswordFingerprint).NotTo(BeEmpty())
		Expect(seeded.JobName).To(Equal(seedJobName()))
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seeded.JobName, Namespace: targetNamespace}, &job)).To(Succeed())
		Expect(envMap(job.Spec.Template.Spec.Containers[0].Env)["TENANT_PARAMS"]).To(Equal(render.SeederParams(seeded.Params)))
	})

	It("keeps following a seeding run after spec.tenant is removed", func() {
		name := startSeeding()
		setTenant("")
		reconcileDeployment()
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeding))
		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Reason).To(Equal(benchv1alpha1.ReasonReconciling))
		Expect(tenant.Message).To(ContainSubstring("seeding tenant from preset"))

		finishJob(name, batchv1.JobComplete, "")
		reconcileDeployment()
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeded))
		tenant = condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionTrue))
		Expect(tenant.Message).To(ContainSubstring("none is selected now"))
		Expect(condition(benchv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("keeps the record of a seeded tenant after spec.tenant is removed", func() {
		finishJob(startSeeding(), batchv1.JobComplete, "")
		reconcileDeployment()
		setTenant("")
		reconcileDeployment()
		Expect(getDeployment().Status.Tenant.State).To(Equal(benchv1alpha1.TenantSeeded))
		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionTrue))
		Expect(tenant.Message).To(Equal(`holds the tenant seeded from preset "tenant-test"; none is selected now`))
	})

	It("reports a seeded tenant as seeded while the chart upgrades", func() {
		finishJob(startSeeding(), batchv1.JobComplete, "")
		reconcileDeployment()
		markHelmReleaseNotReady()
		reconcileDeployment()
		tenant := condition(benchv1alpha1.ConditionTenantSeeded)
		Expect(tenant.Status).To(Equal(metav1.ConditionTrue))
		Expect(tenant.Message).To(HavePrefix("seeded 10 users"))
	})

	It("stays Failed while the chart upgrades", func() {
		finishJob(startSeeding(), batchv1.JobFailed, "Job has reached the specified backoff limit")
		reconcileDeployment()
		markHelmReleaseNotReady()
		reconcileDeployment()
		Expect(condition(benchv1alpha1.ConditionTenantSeeded).Reason).To(Equal(benchv1alpha1.ReasonSeedingFailed))
		Expect(condition(benchv1alpha1.ConditionReady).Reason).To(Equal(benchv1alpha1.ReasonSeedingFailed))
		Expect(getDeployment().Status.Phase).To(Equal(benchv1alpha1.PhaseFailed))
	})

	It("reads a local deployment's seeding pods without the manager's cache", func() {
		name := startSeeding()
		failPod(name, corev1.ContainerStateTerminated{
			Reason: "OOMKilled", ExitCode: 137, Message: "Loaded config\n",
		})
		finishJob(name, batchv1.JobFailed, "Job has reached the specified backoff limit")

		// The API refuses a deployment on the management cluster, but the
		// controller still handles an empty clusterRef as one; reconciled
		// in memory, with rc the management cluster itself.
		local := getDeployment()
		local.Spec.ClusterRef.Name = ""
		r.Client = noPodListClient{k8sClient}
		outcome, err := r.reconcileTenant(ctx, k8sClient, local, nil, metav1.ConditionTrue)
		Expect(err).NotTo(HaveOccurred())
		Expect(outcome.reason).To(Equal(benchv1alpha1.ReasonSeedingFailed))
		Expect(outcome.message).To(Equal("container OOMKilled (exit 137): Loaded config"))
	})

	It("maps a tenant password Secret to the deployments holding a tenant", func() {
		password := &corev1.Secret{Name: render.TenantPasswordSecretName, Namespace: "default"}
		Expect(r.deploymentsForTenantPassword(ctx, password)).NotTo(ContainElement(reconcile.Request{NamespacedName: key}),
			"nothing seeded yet")
		startSeeding()
		Expect(r.deploymentsForTenantPassword(ctx, password)).To(ContainElement(reconcile.Request{NamespacedName: key}))
		Expect(r.deploymentsForTenantPassword(ctx, &corev1.Secret{Name: render.TenantPasswordSecretName, Namespace: "elsewhere"})).
			NotTo(ContainElement(reconcile.Request{NamespacedName: key}))
	})

	It("maps a preset to the deployments naming it", func() {
		reqs := r.deploymentsForTenantPreset(ctx, &benchv1alpha1.TenantPreset{Name: presetName, Namespace: "default"})
		Expect(reqs).To(ContainElement(reconcile.Request{NamespacedName: key}))
		Expect(r.deploymentsForTenantPreset(ctx, &benchv1alpha1.TenantPreset{Name: "unrelated", Namespace: "default"})).
			NotTo(ContainElement(reconcile.Request{NamespacedName: key}))
	})
})

func TestKeepsFailedTenantWithoutPreset(t *testing.T) {
	topo := &benchv1alpha1.AuthentikDeployment{Name: "d"}
	topo.Spec.Tenant = &benchv1alpha1.TenantSpec{PresetRef: corev1.LocalObjectReference{Name: "small"}}
	topo.Status.Tenant = &benchv1alpha1.TenantStatus{Preset: "small", State: benchv1alpha1.TenantFailed, JobName: "seed-x"}
	dropStaleTenant(topo, nil)
	if topo.Status.Tenant == nil {
		t.Error("a failure was forgotten although nothing else can be tried: its preset is gone")
	}
}
