// Package render turns bench.goauthentik.io CRs into the Kubernetes/Flux/CNPG
// objects that implement them.
package render

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// ServerWorkerAffinity returns the raw pod affinity/anti-affinity the
// authentik chart's server.affinity/worker.affinity values render verbatim
// as the Pod's real corev1.Affinity. Rendered directly (rather than through
// the chart's own global.affinity.podAntiAffinity preset mechanism,
// _helpers.tpl's "authentik.affinity") for every mode, not just Packed,
// because setting server.affinity/worker.affinity to anything at all makes
// the chart skip its own preset entirely (`{{- with .component.affinity -}}
// ... {{- else -}} <preset> {{- end -}}` - confirmed against the chart's
// actual template source) - once any mode needs a custom term here (the
// cross-deployment one below, needed unconditionally), every mode has to
// render its own affinity fully rather than mixing the two mechanisms.
//
// Always includes a required anti-affinity term against any *other*
// deployment's pods (crossDeploymentAntiAffinityTerm) so two deployments never
// share a node regardless of mode - a guarantee PlacementMode itself never
// provided, since it only ever governed scheduling within one deployment.
// Additionally, by mode:
//   - Dedicated: a required anti-affinity term against this release's own
//     other pods (one pod of this release per node - previously the
//     chart's own "hard" preset; topologyKey fixed at kubernetes.io/hostname
//     to match that preset's own behavior, since PlacementSpec.TopologyKey
//     has no equivalent once we're not going through the chart's preset -
//     use ExtraValuesRef for a custom key).
//   - Packed: a required *affinity* term for this release's own pods (each
//     must join a node already running one; the scheduler lets the first
//     through) - unchanged from this function's previous Packed-only form.
//   - Default: no extra term - a deployment's own pods may share nodes with
//     each other (the noisy-neighbor case the benchmark compares against),
//     just never with another deployment's.
//
// The own-release terms match on app.kubernetes.io/instance, a label every
// pod the authentik chart or its bundled postgresql subchart produces
// carries (confirmed against both charts' own label helpers), so Dedicated/
// Packed still reach the bundled postgres pod too, not just server/worker.
func ServerWorkerAffinity(spec benchv1alpha1.PlacementSpec, releaseName, deploymentName string) *corev1.Affinity {
	ownReleaseSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{labelAppInstance: releaseName},
	}
	aff := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				crossDeploymentAntiAffinityTerm(deploymentName),
			},
		},
	}
	switch spec.Mode {
	case benchv1alpha1.PlacementDedicated:
		aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
			aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
			corev1.PodAffinityTerm{LabelSelector: ownReleaseSelector, TopologyKey: topologyKeyHostname},
		)
	case benchv1alpha1.PlacementPacked:
		aff.PodAffinity = &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{LabelSelector: ownReleaseSelector, TopologyKey: topologyKeyHostname},
			},
		}
	}
	return aff
}

// Tolerations returns the tolerations to apply in Dedicated mode, letting
// pods land on pre-tainted nodes. Empty in Default mode.
func Tolerations(spec benchv1alpha1.PlacementSpec) []corev1.Toleration {
	if spec.Mode != benchv1alpha1.PlacementDedicated {
		return nil
	}
	return spec.Tolerations
}

// NodeSelector returns the node selector to apply in Dedicated mode. Empty in
// Default mode.
func NodeSelector(spec benchv1alpha1.PlacementSpec) map[string]string {
	if spec.Mode != benchv1alpha1.PlacementDedicated {
		return nil
	}
	return spec.NodeSelector
}
