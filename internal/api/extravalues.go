package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// extraValuesConfigMapName is deterministic from the deployment name, so the
// UI never has to invent or remember one.
func extraValuesConfigMapName(deploymentName string) string {
	return deploymentName + "-extra-values"
}

// extraValuesBody is both the request and response shape: a single raw YAML
// document merged over the generated Helm values last (see
// internal/render.MergeExtraValues) - the escape hatch for authentik/chart
// settings the CRD doesn't model as a dedicated field.
type extraValuesBody struct {
	ValuesYAML string `json:"valuesYAML"`
}

// owningMatrix returns the TestMatrix controlling topo, or nil. The matrix
// re-applies its deployments' whole spec, so a per-deployment extraValuesRef
// would be reverted: its extra values live in the matrix's spec.extraValues
// instead, shared by every variant.
func (s *Server) owningMatrix(r *http.Request, topo *benchv1alpha1.AuthentikDeployment) (*benchv1alpha1.TestMatrix, error) {
	owner := metav1.GetControllerOf(topo)
	if owner == nil || owner.Kind != "TestMatrix" {
		return nil, nil
	}
	var m benchv1alpha1.TestMatrix
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: owner.Name, Namespace: topo.Namespace}, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// getExtraValues returns the deployment's current extra-values YAML, or an
// empty string if none is set - not a 404, since "no extra values" is a
// normal, common state rather than a missing resource.
func (s *Server) getExtraValues(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var topo benchv1alpha1.AuthentikDeployment
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}, &topo); err == nil {
		m, err := s.owningMatrix(r, &topo)
		if err != nil {
			writeAPIError(w, r, err)
			return
		}
		if m != nil {
			writeJSON(w, r, http.StatusOK, extraValuesBody{ValuesYAML: m.Spec.ExtraValues})
			return
		}
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: extraValuesConfigMapName(name), Namespace: s.cfg.Namespace}
	if err := s.client.Get(r.Context(), key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSON(w, r, http.StatusOK, extraValuesBody{})
			return
		}
		writeAPIError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, extraValuesBody{ValuesYAML: cm.Data["values.yaml"]})
}

// putExtraValues upserts the deployment's extra-values ConfigMap and points
// spec.authentik.extraValuesRef at it, creating the reference the first time
// this is called. It always writes the AuthentikDeployment itself (touching an
// annotation even when extraValuesRef.name doesn't change) so
// AuthentikDeploymentReconciler - which does not separately watch this
// ConfigMap - picks up the new content: editing the ConfigMap alone would
// otherwise never re-trigger a reconcile.
func (s *Server) putExtraValues(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var body extraValuesBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if body.ValuesYAML != "" {
		var probe map[string]any
		if err := yaml.Unmarshal([]byte(body.ValuesYAML), &probe); err != nil {
			writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid YAML: %v", err))
			return
		}
	}

	var topo benchv1alpha1.AuthentikDeployment
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}, &topo); err != nil {
		writeAPIError(w, r, err)
		return
	}
	m, err := s.owningMatrix(r, &topo)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	if m != nil {
		m.Spec.ExtraValues = body.ValuesYAML
		if err := s.client.Update(r.Context(), m); err != nil {
			writeAPIError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, body)
		return
	}

	cmName := extraValuesConfigMapName(name)
	cm := &corev1.ConfigMap{
		Name: cmName, Namespace: s.cfg.Namespace,
		Data: map[string]string{"values.yaml": body.ValuesYAML},
	}
	var existing corev1.ConfigMap
	err = s.client.Get(r.Context(), types.NamespacedName{Name: cmName, Namespace: s.cfg.Namespace}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := s.client.Create(r.Context(), cm); err != nil {
			writeAPIError(w, r, err)
			return
		}
	case err != nil:
		writeAPIError(w, r, err)
		return
	default:
		existing.Data = cm.Data
		if err := s.client.Update(r.Context(), &existing); err != nil {
			writeAPIError(w, r, err)
			return
		}
	}

	if topo.Spec.Authentik.ExtraValuesRef == nil || topo.Spec.Authentik.ExtraValuesRef.Name != cmName {
		topo.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: cmName}
	}
	if topo.Annotations == nil {
		topo.Annotations = map[string]string{}
	}
	// Forces a spec-unrelated write every call, so the controller's watch on
	// AuthentikDeployment fires even when extraValuesRef.name is unchanged -
	// this ConfigMap's content is the only thing that changed.
	topo.Annotations["bench.goauthentik.io/extra-values-updated-at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.client.Update(r.Context(), &topo); err != nil {
		writeAPIError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, body)
}

// getHelmValues returns the fully generated chart values (extra values
// included) as last applied to the deployment's HelmRelease, or an empty
// string before the operator has created it.
func (s *Server) getHelmValues(w http.ResponseWriter, r *http.Request) {
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(helmv2.GroupVersion.WithKind("HelmRelease"))
	// render.HelmReleaseName is the deployment name.
	key := types.NamespacedName{Name: chi.URLParam(r, "name"), Namespace: s.cfg.Namespace}
	if err := s.client.Get(r.Context(), key, hr); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSON(w, r, http.StatusOK, extraValuesBody{})
			return
		}
		writeAPIError(w, r, err)
		return
	}
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	out, err := yaml.Marshal(values)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, extraValuesBody{ValuesYAML: string(out)})
}
