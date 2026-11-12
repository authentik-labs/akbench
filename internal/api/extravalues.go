package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// extraValuesConfigMapName is deterministic from the deployment name, so the
// UI never has to invent or remember one.
func extraValuesConfigMapName(deploymentName string) string {
	return deploymentName + "-extra-values"
}

// kindTestCase is the kind of the parent TestCase owner references name.
const kindTestCase = "TestCase"

// extraValuesBody is both the request and response shape: a single raw YAML
// document merged over the generated Helm values last (see
// internal/render.MergeExtraValues) - the escape hatch for authentik/chart
// settings the CRD doesn't model as a dedicated field.
type extraValuesBody struct {
	ValuesYAML string `json:"valuesYAML"`
}

// owningParent returns the parent TestCase controlling topo, or nil. The
// parent re-applies its deployments' whole spec, so a per-deployment
// extraValuesRef would be reverted: its extra values live in the parent's
// spec.extraValues instead, shared by every variant.
func (s *Server) owningParent(r *http.Request, topo *benchv1alpha1.AuthentikDeployment) (*benchv1alpha1.TestCase, error) {
	owner := metav1.GetControllerOf(topo)
	if owner == nil || owner.Kind != kindTestCase {
		return nil, nil
	}
	var m benchv1alpha1.TestCase
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: owner.Name, Namespace: topo.Namespace}, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// getExtraValues returns the extra-values YAML in effect for the deployment,
// or an empty string if none is set - not a 404, since "no extra values" is a
// normal, common state rather than a missing resource.
//
// "In effect" means the ConfigMap spec.authentik.extraValuesRef names, which
// is the one the operator merges. A ConfigMap that merely has the
// conventional name is not reported: showing it made a deployment look
// configured when the operator was ignoring it. A reference to a ConfigMap
// that doesn't exist reports none as well, so the form starts blank and
// saving repairs the reference (see extraValuesTarget).
//
// A deployment that doesn't exist is a 404, like saving its extra values:
// its own ConfigMap is garbage-collected with it, so there are no last
// values left to show.
func (s *Server) getExtraValues(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var topo benchv1alpha1.AuthentikDeployment
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}, &topo); err != nil {
		writeAPIError(w, r, err)
		return
	}
	m, err := s.owningParent(r, &topo)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	if m != nil {
		writeJSON(w, r, http.StatusOK, extraValuesBody{ValuesYAML: m.Spec.ExtraValues})
		return
	}
	ref := topo.Spec.Authentik.ExtraValuesRef
	if ref == nil || ref.Name == "" {
		writeJSON(w, r, http.StatusOK, extraValuesBody{})
		return
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: ref.Name, Namespace: s.cfg.Namespace}
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

// putExtraValues upserts the deployment's extra-values ConfigMap (see
// extraValuesTarget for which one) and points spec.authentik.extraValuesRef
// at it, creating the reference the first time this is called. The
// deployment itself is only written when that reference changes:
// AuthentikDeploymentReconciler watches ConfigMaps and reconciles every
// deployment whose extraValuesRef names one that changed, so new content in
// an already-referenced ConfigMap is picked up without it.
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
	m, err := s.owningParent(r, &topo)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	if m != nil {
		// Retried on conflict for the same reason as the deployment update
		// (setExtraValuesRef): the operator writes the parent's status while
		// it reconciles.
		parentKey := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var current benchv1alpha1.TestCase
			if err := s.client.Get(r.Context(), parentKey, &current); err != nil {
				return err
			}
			current.Spec.ExtraValues = body.ValuesYAML
			return s.client.Update(r.Context(), &current)
		})
		if err != nil {
			writeAPIError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, body)
		return
	}

	cmName, err := s.extraValuesTarget(r, &topo)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	if err := s.writeExtraValuesConfigMap(r, &topo, cmName, body.ValuesYAML); err != nil {
		writeAPIError(w, r, err)
		return
	}

	if err := s.setExtraValuesRef(r, name, cmName); err != nil {
		writeAPIError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, body)
}

// extraValuesTarget picks the ConfigMap a save writes: the one
// spec.authentik.extraValuesRef already names, which is the one
// getExtraValues showed and possibly one several deployments share. Always
// writing the deployment's own instead would quietly fork it off a shared
// ConfigMap whenever its edit form was saved.
//
// The deployment's own (extraValuesConfigMapName) is written instead when it
// references none, one that no longer exists, or one a parent TestCase
// created (see render.TestCaseChildren): that is named after its content and
// replaced whenever the parent's extra values change, and a child's values
// belong to its parent anyway.
func (s *Server) extraValuesTarget(r *http.Request, topo *benchv1alpha1.AuthentikDeployment) (string, error) {
	own := extraValuesConfigMapName(topo.Name)
	ref := topo.Spec.Authentik.ExtraValuesRef
	if ref == nil || ref.Name == "" || ref.Name == own {
		return own, nil
	}
	var cm corev1.ConfigMap
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: ref.Name, Namespace: s.cfg.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return own, nil
		}
		return "", err
	}
	if createdByParent(&cm) {
		return own, nil
	}
	return ref.Name, nil
}

// createdByParent reports whether cm is a parent TestCase's extra-values
// ConfigMap, which carries the parent label and is owned by the parent.
func createdByParent(cm *corev1.ConfigMap) bool {
	if cm.Labels[benchv1alpha1.LabelParent] != "" {
		return true
	}
	for _, owner := range cm.OwnerReferences {
		if owner.Kind == kindTestCase && strings.HasPrefix(owner.APIVersion, benchv1alpha1.GroupVersion.Group+"/") {
			return true
		}
	}
	return false
}

// writeExtraValuesConfigMap creates or updates ConfigMap cmName with the
// extra values, owned by topo so it is garbage-collected with it - unless
// another deployment needs it too (see mayOwnExtraValues). Without an owner
// every deleted deployment left its ConfigMap behind. Updating also adopts a
// ConfigMap saved before these had an owner.
//
// Retried when someone else wrote the ConfigMap since it was read, and when
// it was deleted in between: one still owned by an earlier deployment of the
// same name is garbage-collected at any moment, so the next attempt creates
// it afresh.
func (s *Server) writeExtraValuesConfigMap(r *http.Request, topo *benchv1alpha1.AuthentikDeployment, cmName, valuesYAML string) error {
	key := types.NamespacedName{Name: cmName, Namespace: s.cfg.Namespace}
	retriable := func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsNotFound(err) || apierrors.IsAlreadyExists(err)
	}
	return retry.OnError(retry.DefaultBackoff, retriable, func() error {
		var cm corev1.ConfigMap
		err := s.client.Get(r.Context(), key, &cm)
		exists := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if !exists {
			cm = corev1.ConfigMap{Name: cmName, Namespace: s.cfg.Namespace}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["values.yaml"] = valuesYAML
		own, err := s.mayOwnExtraValues(r, topo, cmName)
		if err != nil {
			return err
		}
		if own {
			if err := controllerutil.SetOwnerReference(topo, &cm, s.client.Scheme()); err != nil {
				return err
			}
		}
		if exists {
			return s.client.Update(r.Context(), &cm)
		}
		return s.client.Create(r.Context(), &cm)
	})
}

// mayOwnExtraValues reports whether topo may own ConfigMap cmName. Only its
// own, conventionally named one: any other a save writes was made by someone
// else (see extraValuesTarget), and deleting it with this deployment would
// come as a surprise. And not when another deployment's extraValuesRef names
// it as well: deleting topo would then delete the ConfigMap from under that
// deployment, which stops reconciling until it is back.
func (s *Server) mayOwnExtraValues(r *http.Request, topo *benchv1alpha1.AuthentikDeployment, cmName string) (bool, error) {
	if cmName != extraValuesConfigMapName(topo.Name) {
		return false, nil
	}
	var list benchv1alpha1.AuthentikDeploymentList
	if err := s.client.List(r.Context(), &list, client.InNamespace(topo.Namespace)); err != nil {
		return false, err
	}
	for _, other := range list.Items {
		if other.Name == topo.Name {
			continue
		}
		if ref := other.Spec.Authentik.ExtraValuesRef; ref != nil && ref.Name == cmName {
			return false, nil
		}
	}
	return true, nil
}

// setExtraValuesRef points the deployment's spec.authentik.extraValuesRef at
// cmName, writing the deployment only if it points elsewhere.
//
// Retried on conflict, re-reading the deployment each time: the operator
// writes its status while it reconciles, most of all right after it is
// created - which is when the comparison wizard saves extra values. A
// conflict here used to fail the request after the ConfigMap was already
// written, leaving it behind with no deployment referencing it.
func (s *Server) setExtraValuesRef(r *http.Request, name, cmName string) error {
	key := types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current benchv1alpha1.AuthentikDeployment
		if err := s.client.Get(r.Context(), key, &current); err != nil {
			return err
		}
		if ref := current.Spec.Authentik.ExtraValuesRef; ref != nil && ref.Name == cmName {
			return nil
		}
		current.Spec.Authentik.ExtraValuesRef = &corev1.LocalObjectReference{Name: cmName}
		return s.client.Update(r.Context(), &current)
	})
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
