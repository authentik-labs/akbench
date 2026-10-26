package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// runRequest is the body of POST /testcases/{name}/runs. Everything is
// optional: "run now, as defined" is the common case, and the overrides
// mirror TestRunSpec.overrides so the UI can offer a one-off load shape
// without editing the TestCase.
type runRequest struct {
	Overrides *benchv1alpha1.TestRunOverrides `json:"overrides,omitempty"`
}

// createRun is the UI's "Run now" button: it creates a TestRun for a Quick
// TestCase and lets TestRunReconciler do the rest.
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var tc benchv1alpha1.TestCase
	key := types.NamespacedName{Name: name, Namespace: s.cfg.Namespace}
	if err := s.client.Get(r.Context(), key, &tc); err != nil {
		writeAPIError(w, r, err)
		return
	}
	if tc.Spec.Archived {
		writeError(w, r, http.StatusBadRequest, "TestCase is archived; unarchive it to start new TestRuns")
		return
	}
	// Burn TestCases run continuously via their own Deployment; a TestRun
	// against one would only be rejected by the reconciler, so say so here
	// where the message can reach the user who pressed the button.
	if tc.Spec.Profile != benchv1alpha1.ProfileQuick {
		writeError(w, r, http.StatusBadRequest,
			"TestRuns execute Quick TestCases only; a Burn TestCase is already running via its own Deployment")
		return
	}

	var body runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	deployments := tc.Deployments()
	if len(deployments) == 0 {
		writeError(w, r, http.StatusBadRequest, "TestCase has no deployment set (neither deploymentRef nor deploymentRefs)")
		return
	}

	// One TestRun per target deployment, each pinned via Spec.DeploymentRef -
	// matches this project's "immutable, individually-inspectable record"
	// TestRun philosophy (one TestRun = one Job = one target) rather than
	// teaching a single TestRun to fan out N Jobs itself.
	runs := make([]*benchv1alpha1.TestRun, 0, len(deployments))
	for _, dep := range deployments {
		run := &benchv1alpha1.TestRun{
			GenerateName: name + "-",
			Namespace:    s.cfg.Namespace,
			Labels:       map[string]string{benchv1alpha1.LabelTestCase: name},
			Spec: benchv1alpha1.TestRunSpec{
				TestCaseRef:   corev1.LocalObjectReference{Name: name},
				DeploymentRef: &dep,
				Overrides:     body.Overrides,
			},
		}
		setCreatedBy(r, run)
		if err := s.client.Create(r.Context(), run); err != nil {
			writeAPIError(w, r, err)
			return
		}
		runs = append(runs, run)
	}
	writeJSON(w, r, http.StatusCreated, runs)
}
