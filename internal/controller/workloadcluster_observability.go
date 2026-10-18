package controller

import (
	"context"
	"fmt"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

// Grafana, Tempo and Pyroscope install from their upstream Helm charts
// (render.GrafanaHelmRelease & co.). Clusters set up by an older operator
// have them as hand-rendered objects instead, under names the charts reuse
// (the "grafana" Service, the ServiceMonitors) or that would clash with
// them (the "grafana-nodeport" Service holding the NodePort the chart's
// Service needs) - so those are deleted before the chart installs, the same
// way an older fixed-name add-on HelmRelease is migrated.

// legacyObservabilityObjects are the objects the hand-rendered Pyroscope,
// Tempo and Grafana consisted of, per add-on.
var legacyObservabilityObjects = map[string][]client.Object{
	render.PyroscopeHelmReleaseName: {
		legacyObject(&appsv1.Deployment{}, "pyroscope"),
		legacyObject(&corev1.Service{}, "pyroscope"),
		legacyServiceMonitor("pyroscope"),
		legacyObject(&corev1.PersistentVolumeClaim{}, "pyroscope-data"),
	},
	render.TempoHelmReleaseName: {
		legacyObject(&appsv1.Deployment{}, "tempo"),
		legacyObject(&corev1.Service{}, "tempo"),
		legacyObject(&corev1.ConfigMap{}, "tempo-config"),
		legacyServiceMonitor("tempo"),
		legacyObject(&corev1.PersistentVolumeClaim{}, "tempo-data"),
	},
	render.GrafanaHelmReleaseName: {
		legacyObject(&appsv1.Deployment{}, "grafana"),
		legacyObject(&corev1.Service{}, "grafana"),
		legacyObject(&corev1.Service{}, "grafana-nodeport"),
		legacyObject(&corev1.ConfigMap{}, "grafana-datasources"),
		legacyObject(&corev1.ConfigMap{}, "grafana-providers"),
		legacyObject(&corev1.ConfigMap{}, "grafana-dashboards"),
		legacyServiceMonitor("grafana"),
		legacyObject(&corev1.PersistentVolumeClaim{}, "grafana-data"),
	},
}

func legacyObject(obj client.Object, name string) client.Object {
	obj.SetName(name)
	obj.SetNamespace(render.ObservabilityNamespace)
	return obj
}

func legacyServiceMonitor(name string) client.Object {
	return legacyMonitoringObject("ServiceMonitor", name)
}

// legacyMonitoringObject is a prometheus-operator object (unstructured: its
// CRDs come from kube-prometheus-stack, not a typed import).
func legacyMonitoringObject(kind, name string) client.Object {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("monitoring.coreos.com/v1")
	u.SetKind(kind)
	return legacyObject(u, name)
}

// removeLegacyObservabilityObjects deletes the hand-rendered objects of the
// add-on installed by release, unless Helm manages them - by then they are
// the chart's own. Only runs until that release is Ready: after that,
// nothing legacy can be left.
func (r *WorkloadClusterReconciler) removeLegacyObservabilityObjects(ctx context.Context, wc *benchv1alpha1.WorkloadCluster, remote client.Client, release string) error {
	var hr helmv2.HelmRelease
	err := r.Get(ctx, types.NamespacedName{Namespace: wc.Namespace, Name: render.AddonHelmReleaseName(wc, release)}, &hr)
	if err == nil && meta.IsStatusConditionTrue(hr.Status.Conditions, "Ready") {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return removeLegacyObjects(ctx, remote, release, legacyObservabilityObjects[release])
}

// removeLegacyObjects deletes each of objs from remote that exists and isn't
// managed by Helm - once Helm manages an object of that name, it's a chart's
// own.
func removeLegacyObjects(ctx context.Context, remote client.Client, addon string, objs []client.Object) error {
	for _, template := range objs {
		obj := template.DeepCopyObject().(client.Object)
		switch err := remote.Get(ctx, client.ObjectKeyFromObject(obj), obj); {
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			continue
		case err != nil:
			return fmt.Errorf("get legacy %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
		if obj.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" {
			continue
		}
		if err := remote.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete legacy %T %s: %w", obj, obj.GetName(), err)
		}
		logf.FromContext(ctx).Info("Removed hand-rendered add-on object in favour of its Helm chart",
			"addon", addon, "kind", fmt.Sprintf("%T", obj), "name", obj.GetName())
	}
	return nil
}

// ensureObservabilityChart installs one of Grafana/Tempo/Pyroscope: legacy
// objects out of the way first, then its HelmRepository and HelmRelease.
func (r *WorkloadClusterReconciler) ensureObservabilityChart(ctx context.Context, wc *benchv1alpha1.WorkloadCluster, remote client.Client,
	repo *sourcev1.HelmRepository, hr *helmv2.HelmRelease, release string,
) error {
	if err := r.removeLegacyObservabilityObjects(ctx, wc, remote, release); err != nil {
		return err
	}
	return r.applyObservabilityChart(ctx, wc, repo, hr, release)
}

// applyObservabilityChart applies an observability add-on's HelmRepository
// and HelmRelease.
func (r *WorkloadClusterReconciler) applyObservabilityChart(ctx context.Context, wc *benchv1alpha1.WorkloadCluster,
	repo *sourcev1.HelmRepository, hr *helmv2.HelmRelease, release string,
) error {
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, repo); err != nil {
		return fmt.Errorf("apply %s HelmRepository: %w", release, err)
	}
	if err := applyOwned(ctx, r.Client, r.Scheme, wc, hr); err != nil {
		return fmt.Errorf("apply %s HelmRelease: %w", release, err)
	}
	return nil
}

// ensurePyroscope installs Grafana Pyroscope onto the workload cluster. No
// cross-cluster reachability needed - authentik's own
// AUTHENTIK_PYROSCOPE_HOST already points at this exact in-cluster DNS name
// (defaultPyroscopeHost in authentikdeployment_controller.go).
func (r *WorkloadClusterReconciler) ensurePyroscope(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	remote, err := r.remoteAddonClient(ctx, wc)
	if err != nil {
		return err
	}
	hr, err := render.PyroscopeHelmRelease(wc)
	if err != nil {
		return err
	}
	return r.ensureObservabilityChart(ctx, wc, remote, render.GrafanaHelmRepository(wc.Namespace), hr, render.PyroscopeHelmReleaseName)
}

// ensureTempo installs Grafana Tempo onto the workload cluster - same
// reasoning as ensurePyroscope, for AUTHENTIK_ERROR_REPORTING__OTEL_ENDPOINT
// (defaultTracingEndpoint).
func (r *WorkloadClusterReconciler) ensureTempo(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	remote, err := r.remoteAddonClient(ctx, wc)
	if err != nil {
		return err
	}
	hr, err := render.TempoHelmRelease(wc)
	if err != nil {
		return err
	}
	return r.ensureObservabilityChart(ctx, wc, remote, render.GrafanaCommunityHelmRepository(wc.Namespace), hr, render.TempoHelmReleaseName)
}

// ensureLoki installs Grafana Loki onto the workload cluster, where Alloy
// (ensureAlloy) ships every pod's logs. Like Tempo and Pyroscope, only
// reached in-cluster: by Alloy, and by Grafana's datasource proxy. Neither
// ever existed hand-rendered, so there are no legacy objects to remove.
func (r *WorkloadClusterReconciler) ensureLoki(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	hr, err := render.LokiHelmRelease(wc)
	if err != nil {
		return err
	}
	return r.applyObservabilityChart(ctx, wc, render.GrafanaCommunityHelmRepository(wc.Namespace), hr, render.LokiHelmReleaseName)
}

// ensureAlloy installs the Grafana Alloy DaemonSet that tails every pod's
// container logs into Loki (render.AlloyHelmRelease).
func (r *WorkloadClusterReconciler) ensureAlloy(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	hr, err := render.AlloyHelmRelease(wc)
	if err != nil {
		return err
	}
	return r.applyObservabilityChart(ctx, wc, render.GrafanaHelmRepository(wc.Namespace), hr, render.AlloyHelmReleaseName)
}

// ensureGrafana installs Grafana onto the workload cluster, pre-wired with
// Prometheus/Pyroscope/Tempo/Loki datasources and this project's own dashboards
// (render.GrafanaHelmRelease), and records the NodePort URL cmd/apiserver's
// reverse proxy (internal/api/grafana_proxy.go) connects to.
func (r *WorkloadClusterReconciler) ensureGrafana(ctx context.Context, wc *benchv1alpha1.WorkloadCluster) error {
	remote, err := r.remoteAddonClient(ctx, wc)
	if err != nil {
		return err
	}
	// Generated once on the management cluster (not the remote one): both
	// cmd/apiserver (which proxies to Grafana) and the chart (via the
	// HelmRelease's valuesFrom) need the exact same value, and only the
	// management cluster is somewhere both can read a Secret from directly -
	// see render.GrafanaAdminSecretName's own doc comment.
	if _, err := ensureRandomSecret(ctx, r.Client,
		types.NamespacedName{Name: render.GrafanaAdminSecretName(wc), Namespace: wc.Namespace},
		"password", 16); err != nil {
		return fmt.Errorf("ensure Grafana admin password: %w", err)
	}
	hr, err := render.GrafanaHelmRelease(wc)
	if err != nil {
		return err
	}
	if err := r.ensureObservabilityChart(ctx, wc, remote, render.GrafanaCommunityHelmRepository(wc.Namespace), hr, render.GrafanaHelmReleaseName); err != nil {
		return err
	}
	nodeIP, err := reachableNodeIP(ctx, remote)
	if err != nil {
		return fmt.Errorf("resolve reachable node IP: %w", err)
	}
	wc.Status.GrafanaURL = fmt.Sprintf("http://%s:%d", nodeIP, render.GrafanaNodePort)
	return nil
}
