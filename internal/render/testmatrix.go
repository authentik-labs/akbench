package render

import (
	"fmt"
	"hash/fnv"
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestMatrixObjects is everything a TestMatrix creates.
type TestMatrixObjects struct {
	Deployments []*benchv1alpha1.AuthentikDeployment
	TestCases   []*benchv1alpha1.TestCase
	// ExtraValues is nil when spec.extraValues is empty.
	ExtraValues *corev1.ConfigMap
}

// maxDeploymentNameLength mirrors AuthentikDeployment's CEL name limit:
// "<name>-postgresql-<10-char hash>" is a pod label value, capped at 63.
const maxDeploymentNameLength = 41

// TestMatrix builds one TestCase per template, each driving its own
// AuthentikDeployment per variant. Every object carries the matrix's own
// labels plus LabelTestMatrix, so stale ones can be found and removed.
func TestMatrix(m *benchv1alpha1.TestMatrix) (TestMatrixObjects, error) {
	labels := maps.Clone(m.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[benchv1alpha1.LabelTestMatrix] = m.Name
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: m.Namespace, Labels: maps.Clone(labels)}
	}

	var out TestMatrixObjects
	var extraValuesRef *corev1.LocalObjectReference
	if m.Spec.ExtraValues != "" {
		// Named after its content: the AuthentikDeployment controller doesn't
		// watch the ConfigMap, so a content change has to show up as a new
		// extraValuesRef on the deployments to be picked up.
		h := fnv.New32a()
		_, _ = h.Write([]byte(m.Spec.ExtraValues))
		out.ExtraValues = &corev1.ConfigMap{
			Kind: kindConfigMap, APIVersion: apiVersionV1,
			ObjectMeta: meta(fmt.Sprintf("%s-extra-values-%08x", m.Name, h.Sum32())),
			Data:       map[string]string{"values.yaml": m.Spec.ExtraValues},
		}
		extraValuesRef = &corev1.LocalObjectReference{Name: out.ExtraValues.Name}
	}

	baseline := m.BaselineVariant()
	for _, template := range m.Spec.Templates {
		tc := &benchv1alpha1.TestCase{
			Kind: "TestCase", APIVersion: benchv1alpha1.SchemeGroupVersion.String(),
			ObjectMeta: meta(m.TestCaseName(template)),
			Spec:       *m.Spec.TestCase.DeepCopy(),
		}
		tc.Spec.Template = template
		tc.Spec.DeploymentRefs = nil
		for _, v := range m.Spec.Variants {
			name := m.DeploymentName(template, v.Name)
			if len(name) > maxDeploymentNameLength {
				return TestMatrixObjects{}, fmt.Errorf("deployment name %q is longer than %d characters; shorten the matrix, variant or template name",
					name, maxDeploymentNameLength)
			}
			ad := &benchv1alpha1.AuthentikDeployment{
				Kind: "AuthentikDeployment", APIVersion: benchv1alpha1.SchemeGroupVersion.String(),
				ObjectMeta: meta(name),
				Spec:       *m.Spec.Deployment.DeepCopy(),
			}
			ad.Spec.Namespace = name
			ad.Spec.Authentik.Tag = v.Tag
			if v.Image != "" {
				ad.Spec.Authentik.Image = v.Image
			}
			ad.Spec.Authentik.ExtraValuesRef = extraValuesRef
			out.Deployments = append(out.Deployments, ad)

			tc.Spec.DeploymentRefs = append(tc.Spec.DeploymentRefs, corev1.LocalObjectReference{Name: name})
			if v.Name == baseline {
				tc.Spec.BaselineRef = corev1.LocalObjectReference{Name: name}
			}
		}
		out.TestCases = append(out.TestCases, tc)
	}
	return out, nil
}
