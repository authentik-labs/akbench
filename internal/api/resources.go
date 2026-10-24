package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// resourceKind ties a URL path segment to the CRD kind behind it. The three
// bench kinds are handled generically as unstructured objects: the CRD schema
// on the API server is the single source of truth for validation and
// defaulting, so re-stating either one here would only let the two drift.
type resourceKind struct {
	kind     string
	listKind string
}

// resourceKinds maps /api/v1/<path> onto the kind it serves.
var resourceKinds = map[string]resourceKind{
	"deployments":       {kind: "AuthentikDeployment", listKind: "AuthentikDeploymentList"},
	"testcases":         {kind: "TestCase", listKind: "TestCaseList"},
	"testcasetemplates": {kind: "TestCaseTemplate", listKind: "TestCaseTemplateList"},
	"testmatrices":      {kind: "TestMatrix", listKind: "TestMatrixList"},
	"testruns":          {kind: "TestRun", listKind: "TestRunList"},
	"workloadclusters":  {kind: "WorkloadCluster", listKind: "WorkloadClusterList"},
}

func (k resourceKind) gvk() schema.GroupVersionKind {
	return benchv1alpha1.SchemeGroupVersion.WithKind(k.kind)
}

func (k resourceKind) newObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.gvk())
	return obj
}

func (k resourceKind) newList() *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(benchv1alpha1.SchemeGroupVersion.WithKind(k.listKind))
	return list
}

func (s *Server) listResource(k resourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list := k.newList()
		opts := []client.ListOption{client.InNamespace(s.cfg.Namespace)}
		if selector := r.URL.Query().Get("labelSelector"); selector != "" {
			parsed, err := labels.Parse(selector)
			if err != nil {
				writeError(w, r, http.StatusBadRequest, err.Error())
				return
			}
			opts = append(opts, client.MatchingLabelsSelector{Selector: parsed})
		}
		if err := s.client.List(r.Context(), list, opts...); err != nil {
			writeAPIError(w, r, err)
			return
		}
		writeItems(w, r, list.Items)
	}
}

func (s *Server) getResource(k resourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		obj, err := s.fetch(r, k)
		if err != nil {
			writeAPIError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, obj)
	}
}

// getResourceStatus serves just .status, which is all the UI's polling
// components need - a deployment's status is small and changes far more often
// than its spec.
func (s *Server) getResourceStatus(k resourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		obj, err := s.fetch(r, k)
		if err != nil {
			writeAPIError(w, r, err)
			return
		}
		status, _, err := unstructured.NestedMap(obj.Object, "status")
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		if status == nil {
			status = map[string]any{}
		}
		writeJSON(w, r, http.StatusOK, status)
	}
}

func (s *Server) createResource(k resourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		obj, err := s.decodeInto(w, r, k)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if obj.GetName() == "" && obj.GetGenerateName() == "" {
			writeError(w, r, http.StatusBadRequest, "metadata.name is required")
			return
		}
		if err := s.client.Create(r.Context(), obj); err != nil {
			writeAPIError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, obj)
	}
}

// replaceResource updates only .spec and .metadata.labels/annotations of an
// existing object. Taking the resourceVersion from the stored object rather
// than the request body means the UI can PUT a form's contents without
// implementing read-modify-write, at the cost of last-write-wins between two
// concurrent editors - an acceptable trade for a single-operator tool.
func (s *Server) replaceResource(k resourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		submitted, err := s.decodeInto(w, r, k)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		existing, err := s.fetch(r, k)
		if err != nil {
			writeAPIError(w, r, err)
			return
		}
		spec, found, err := unstructured.NestedMap(submitted.Object, "spec")
		if err != nil || !found {
			writeError(w, r, http.StatusBadRequest, "body must contain a spec object")
			return
		}
		if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
			writeError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if labels := submitted.GetLabels(); labels != nil {
			existing.SetLabels(labels)
		}
		if annotations := submitted.GetAnnotations(); annotations != nil {
			existing.SetAnnotations(annotations)
		}
		if err := s.client.Update(r.Context(), existing); err != nil {
			writeAPIError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, existing)
	}
}

func (s *Server) deleteResource(k resourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		obj := k.newObject()
		obj.SetName(chi.URLParam(r, "name"))
		obj.SetNamespace(s.cfg.Namespace)
		if err := s.client.Delete(r.Context(), obj); err != nil {
			writeAPIError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// kickedAtAnnotation is the one this handler sets - a fresh RFC3339Nano
// timestamp every call, so the Update always has new content even if the
// object's own reconcile loop is otherwise fully settled (an Update with an
// unchanged object wouldn't necessarily generate a new resourceVersion/event
// on every apiserver, but a changing value always will).
const kickedAtAnnotation = "bench.goauthentik.io/kicked-at"

// kickResource nudges the operator into reconciling this object right now,
// instead of waiting for its next naturally-triggered Update (a dependency
// becoming ready, a periodic resync, etc.) - the same thing this project's
// own session notes describe doing by hand via
// `kubectl annotate ... bench.goauthentik.io/kicked-at=$(date ...)`. No
// controller in internal/controller filters reconcile-triggering Update
// events by generation (confirmed: none of them import controller-runtime's
// predicate package), so any annotation change already causes an immediate
// Reconcile() - there is nothing to wire up controller-side, this only needs
// to perform the same Update a human would.
func (s *Server) kickResource(k resourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		obj, err := s.fetch(r, k)
		if err != nil {
			writeAPIError(w, r, err)
			return
		}
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[kickedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		obj.SetAnnotations(annotations)
		if err := s.client.Update(r.Context(), obj); err != nil {
			writeAPIError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, obj)
	}
}

func (s *Server) fetch(r *http.Request, k resourceKind) (*unstructured.Unstructured, error) {
	obj := k.newObject()
	key := types.NamespacedName{Name: chi.URLParam(r, "name"), Namespace: s.cfg.Namespace}
	if err := s.client.Get(r.Context(), key, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// decodeInto reads a request body as the given kind, pinning apiVersion/kind
// and the server's namespace so a client can neither write into another
// namespace nor smuggle in a different kind. On a named route the URL's name
// wins over the body's.
func (s *Server) decodeInto(w http.ResponseWriter, r *http.Request, k resourceKind) (*unstructured.Unstructured, error) {
	obj := k.newObject()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := decoder.Decode(&obj.Object); err != nil {
		return nil, err
	}
	obj.SetGroupVersionKind(k.gvk())
	obj.SetNamespace(s.cfg.Namespace)
	if name := chi.URLParam(r, "name"); name != "" {
		obj.SetName(name)
	}
	// Status is owned by the operator; a client that sends one is ignored
	// rather than rejected, since round-tripping a GET is the obvious way to
	// build an edit form.
	unstructured.RemoveNestedField(obj.Object, "status")
	unstructured.RemoveNestedField(obj.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(obj.Object, "metadata", "uid")
	return obj, nil
}

// maxBodyBytes caps request bodies. CRs are small; anything larger is a bug
// or an attack.
const maxBodyBytes = 1 << 20
