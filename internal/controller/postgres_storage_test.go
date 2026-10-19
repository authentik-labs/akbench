package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
