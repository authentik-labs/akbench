package controller

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

var _ = Describe("Postgres storage auto-growth", func() {
	const resourceName = "deployment-pg-grow"
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}
	var topo *benchv1alpha1.AuthentikDeployment

	BeforeEach(func() {
		topo = &benchv1alpha1.AuthentikDeployment{
			Name: resourceName, Namespace: "default",
			Spec: benchv1alpha1.AuthentikDeploymentSpec{
				ClusterRef: corev1.LocalObjectReference{Name: testClusterName},
				Namespace:  "authentik-" + resourceName,
				Server:     benchv1alpha1.WorkloadSpec{Replicas: 1},
				Worker:     benchv1alpha1.WorkloadSpec{Replicas: 1},
				Postgres: benchv1alpha1.PostgresSpec{
					Mode:     benchv1alpha1.PostgresEmbedded,
					Embedded: &benchv1alpha1.EmbeddedPostgresSpec{MaxStorageSize: "3Gi"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, topo)).To(Succeed())

		sc := &storagev1.StorageClass{
			Name:                 "expandable",
			Provisioner:          "example.com/test",
			AllowVolumeExpansion: ptr.To(true),
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, sc))).To(Succeed())
		Expect(ensureNamespace(ctx, k8sClient, topo.Spec.Namespace)).To(Succeed())
		pvc := &corev1.PersistentVolumeClaim{
			Name: render.EmbeddedPostgresPVCName(topo), Namespace: topo.Spec.Namespace,
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: ptr.To(sc.Name),
				VolumeName:       "pv-" + resourceName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		pvc.Status = corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		}
		Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
	})

	AfterEach(func() {
		forceDelete(ctx, &benchv1alpha1.AuthentikDeployment{Name: resourceName, Namespace: "default"})
		forceDelete(ctx, &corev1.PersistentVolumeClaim{Name: render.EmbeddedPostgresPVCName(topo), Namespace: topo.Spec.Namespace})
	})

	It("grows a full embedded postgres PVC, once per check interval", func() {
		usageReads := 0
		r := &AuthentikDeploymentReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker,
			VolumeUsage: func(_ context.Context, _ *benchv1alpha1.AuthentikDeployment, pvcs []corev1.PersistentVolumeClaim) (map[string]volumeUsage, error) {
				usageReads++
				Expect(pvcs).To(HaveLen(1))
				return map[string]volumeUsage{pvcs[0].Name: {used: 900 << 20, capacity: 1 << 30}}, nil
			},
		}
		res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(res.RequeueAfter).To(BeNumerically("<=", postgresStorageCheckInterval))

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.EmbeddedPostgresPVCName(topo), Namespace: topo.Spec.Namespace}, &pvc)).To(Succeed())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("2Gi"))

		var got benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.PostgresStorage).NotTo(BeNil())
		Expect(got.Status.PostgresStorage.Size).To(Equal("2Gi"))
		Expect(got.Status.PostgresStorage.UsedBytes).To(Equal(int64(900 << 20)))
		Expect(got.Status.PostgresStorage.LastChecked).NotTo(BeNil())

		// The status update re-triggers a reconcile; it must not read usage
		// again before the interval is up.
		_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(usageReads).To(Equal(1))
	})

	// usageReader is a volumeUsageFunc whose answer a test sets as it
	// goes: usage for every PVC, no entry at all (no running pod mounts
	// them), or err.
	type usageReader struct {
		usage   volumeUsage
		noEntry bool
		err     error
	}
	storageReconciler := func(reader *usageReader) *AuthentikDeploymentReconciler {
		return &AuthentikDeploymentReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker,
			VolumeUsage: func(_ context.Context, _ *benchv1alpha1.AuthentikDeployment, pvcs []corev1.PersistentVolumeClaim) (map[string]volumeUsage, error) {
				if reader.err != nil || reader.noEntry {
					return map[string]volumeUsage{}, reader.err
				}
				return map[string]volumeUsage{pvcs[0].Name: reader.usage}, nil
			},
		}
	}
	// check reads usage again with r (skipping the check interval) and
	// returns the deployment's ConditionPostgresStorageExhausted, nil when
	// render.PostgresStorageExhausted doesn't count it.
	check := func(r *AuthentikDeploymentReconciler) *metav1.Condition {
		var current benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, key, &current)).To(Succeed())
		if current.Status.PostgresStorage != nil {
			current.Status.PostgresStorage.LastChecked = nil
			Expect(k8sClient.Status().Update(ctx, &current)).To(Succeed())
		}
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &current)).To(Succeed())
		if !render.PostgresStorageExhausted(&current) {
			return nil
		}
		return meta.FindStatusCondition(current.Status.Conditions, benchv1alpha1.ConditionPostgresStorageExhausted)
	}
	pvcKey := func() types.NamespacedName {
		return types.NamespacedName{Name: render.EmbeddedPostgresPVCName(topo), Namespace: topo.Spec.Namespace}
	}
	// resized records that the volume has finished resizing to size.
	resized := func(size string) {
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, pvcKey(), &pvc)).To(Succeed())
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}
		Expect(k8sClient.Status().Update(ctx, &pvc)).To(Succeed())
	}

	It("reports the volume exhausted at its maximum only from a reading of the resized volume", func() {
		reader := &usageReader{usage: volumeUsage{used: 900 << 20, capacity: 1 << 30}}
		r := storageReconciler(reader)

		Expect(check(r)).To(BeNil(), "1Gi to 2Gi")
		resized("2Gi")
		reader.usage = volumeUsage{used: 1900 << 20, capacity: 2 << 30}
		// Grows to the 3Gi maximum. The status now holds the maximum size
		// with the 2Gi volume's usage, which is not the maximum being full.
		Expect(check(r)).To(BeNil(), "2Gi to the 3Gi maximum")
		var got benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.PostgresStorage.Size).To(Equal("3Gi"))
		Expect(check(r)).To(BeNil(), "resize to 3Gi still pending")
		resized("3Gi")
		// The kubelet's cached stats can still show the old 2Gi volume.
		Expect(check(r)).To(BeNil(), "a stale reading of the 2Gi volume")

		reader.usage = volumeUsage{used: 2900 << 20, capacity: 3 << 30}
		cond := check(r)
		Expect(cond).NotTo(BeNil(), "full at the 3Gi maximum")
		Expect(cond.Reason).To(Equal(benchv1alpha1.ReasonPostgresStorageAtMax))
		Expect(cond.Message).To(ContainSubstring("maxStorageSize 3Gi"))

		// No reading at all - postgres being rescheduled, or the kubelet
		// unreachable - says nothing about the volume having room.
		reader.noEntry = true
		Expect(check(r)).NotTo(BeNil(), "no pod mounts the volume")
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status.PostgresStorage.UsedBytes).To(Equal(int64(2900 << 20)))
		reader.noEntry, reader.err = false, errors.New("kubelet unreachable")
		Expect(check(r)).NotTo(BeNil(), "usage can't be read")

		reader.err = nil
		reader.usage = volumeUsage{used: 1 << 30, capacity: 3 << 30}
		Expect(check(r)).To(BeNil(), "room again after a cleanup")
	})

	It("reports a full volume that cannot grow", func() {
		var sc storagev1.StorageClass
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "expandable"}, &sc)).To(Succeed())
		sc.AllowVolumeExpansion = ptr.To(false)
		Expect(k8sClient.Update(ctx, &sc)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "expandable"}, &sc)).To(Succeed())
			sc.AllowVolumeExpansion = ptr.To(true)
			Expect(k8sClient.Update(ctx, &sc)).To(Succeed())
		})
		reader := &usageReader{usage: volumeUsage{used: 900 << 20, capacity: 1 << 30}}
		r := storageReconciler(reader)

		cond := check(r)
		Expect(cond).NotTo(BeNil(), "full, and the PVC can't grow")
		Expect(cond.Reason).To(Equal(benchv1alpha1.ReasonPostgresStorageGrowFailed))
		Expect(cond.Message).To(ContainSubstring("could not grow"))
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, pvcKey(), &pvc)).To(Succeed())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))

		reader.usage = volumeUsage{used: 500 << 20, capacity: 1 << 30}
		Expect(check(r)).To(BeNil(), "room again")
	})

	It("reports a volume past the hard limit whatever its size", func() {
		reader := &usageReader{usage: volumeUsage{used: 1000 << 20, capacity: 1 << 30}}
		r := storageReconciler(reader)

		// Well below the 3Gi maximum, and growing, but nearly out of space.
		cond := check(r)
		Expect(cond).NotTo(BeNil(), "97% full at 1Gi")
		Expect(cond.Reason).To(Equal(benchv1alpha1.ReasonPostgresStorageHardLimit))
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, pvcKey(), &pvc)).To(Succeed())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("2Gi"))
		// A resize that never completes (offline expansion) leaves it there.
		Expect(check(r)).NotTo(BeNil(), "resize to 2Gi still pending")

		resized("2Gi")
		reader.usage = volumeUsage{used: 1000 << 20, capacity: 2 << 30}
		Expect(check(r)).To(BeNil(), "room once resized")
	})

	It("drops the condition with no volume or no readable maximum", func() {
		setExhausted := func() {
			var current benchv1alpha1.AuthentikDeployment
			Expect(k8sClient.Get(ctx, key, &current)).To(Succeed())
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: benchv1alpha1.ConditionPostgresStorageExhausted, Status: metav1.ConditionTrue,
				Reason: benchv1alpha1.ReasonPostgresStorageHardLimit,
			})
			Expect(k8sClient.Status().Update(ctx, &current)).To(Succeed())
		}
		exhausted := func() bool {
			var current benchv1alpha1.AuthentikDeployment
			Expect(k8sClient.Get(ctx, key, &current)).To(Succeed())
			return meta.IsStatusConditionTrue(current.Status.Conditions, benchv1alpha1.ConditionPostgresStorageExhausted)
		}
		r := storageReconciler(&usageReader{usage: volumeUsage{used: 1000 << 20, capacity: 1 << 30}})

		setExhausted()
		forceDelete(ctx, &corev1.PersistentVolumeClaim{Name: pvcKey().Name, Namespace: pvcKey().Namespace})
		Expect(check(r)).To(BeNil())
		Expect(exhausted()).To(BeFalse(), "no PVC")

		setExhausted()
		var current benchv1alpha1.AuthentikDeployment
		Expect(k8sClient.Get(ctx, key, &current)).To(Succeed())
		current.Spec.Postgres.Embedded.MaxStorageSize = "plenty"
		Expect(k8sClient.Update(ctx, &current)).To(Succeed())
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(exhausted()).To(BeFalse(), "maxStorageSize can't be parsed")
	})

	It("leaves the PVC alone while it isn't full", func() {
		r := &AuthentikDeploymentReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100), Tracker: testTracker,
			VolumeUsage: func(_ context.Context, _ *benchv1alpha1.AuthentikDeployment, pvcs []corev1.PersistentVolumeClaim) (map[string]volumeUsage, error) {
				return map[string]volumeUsage{pvcs[0].Name: {used: 100 << 20, capacity: 1 << 30}}, nil
			},
		}
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: render.EmbeddedPostgresPVCName(topo), Namespace: topo.Spec.Namespace}, &pvc)).To(Succeed())
		Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))
	})
})
