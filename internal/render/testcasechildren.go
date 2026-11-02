package render

import (
	"fmt"
	"hash/fnv"
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// TestCaseChildObjects is everything a parent TestCase creates.
type TestCaseChildObjects struct {
	Deployments []*benchv1alpha1.AuthentikDeployment
	TestCases   []*benchv1alpha1.TestCase
	// ExtraValues is nil when spec.extraValues is empty.
	ExtraValues *corev1.ConfigMap
}

// maxDeploymentNameLength mirrors AuthentikDeployment's CEL name limit.
const maxDeploymentNameLength = 53

// TestCaseChildren builds a parent TestCase's child TestCase per template,
// each driving its own AuthentikDeployment per variant. Every object carries
// the parent's own labels plus LabelParent, so stale ones can be found and
// removed.
func TestCaseChildren(m *benchv1alpha1.TestCase) (TestCaseChildObjects, error) {
	if m.Spec.Deployment == nil || len(m.Spec.Variants) == 0 {
		return TestCaseChildObjects{}, fmt.Errorf("spec.templates needs spec.variants and spec.deployment")
	}
	labels := maps.Clone(m.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[benchv1alpha1.LabelParent] = m.Name
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: m.Namespace, Labels: maps.Clone(labels)}
	}

	var out TestCaseChildObjects
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

	baseline := m.BaselineVariantName()
	for _, template := range m.Spec.Templates {
		if name := m.ChildName(template); len(name) > maxNameLength {
			return TestCaseChildObjects{}, fmt.Errorf("test case name %q is longer than %d characters; shorten the test case or template name",
				name, maxNameLength)
		}
		tc := &benchv1alpha1.TestCase{
			Kind: "TestCase", APIVersion: benchv1alpha1.SchemeGroupVersion.String(),
			ObjectMeta: meta(m.ChildName(template)),
			Spec:       *m.Spec.DeepCopy(),
		}
		tc.Spec.Templates, tc.Spec.Variants, tc.Spec.BaselineVariant, tc.Spec.Deployment, tc.Spec.ExtraValues = nil, nil, "", nil, ""
		tc.Spec.Template = template
		tc.Spec.DeploymentRefs = nil
		for _, v := range m.Spec.Variants {
			name := m.ChildDeploymentName(template, v.Name)
			if len(name) > maxDeploymentNameLength {
				return TestCaseChildObjects{}, fmt.Errorf("deployment name %q is longer than %d characters; shorten the test case, variant or template name",
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
