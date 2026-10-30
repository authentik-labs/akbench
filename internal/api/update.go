package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/authentik-labs/akbench/internal/appliance"
	"github.com/authentik-labs/akbench/internal/registry"
)

// Updating akbench itself (operator chart/image + apiserver image) from the
// UI, inside the appliance: the appliance image rarely changes, so instead
// of replacing the container the UI pins the latest published build in
// appliance.VersionConfigMap and the appliance's sync loop rolls it out.

// versionLabel is set on every image by CI's docker/metadata-action; on
// :latest it's the version-numbered tag of the same build.
const versionLabel = "org.opencontainers.image.version"

type updateView struct {
	// Managed is true inside the appliance, the only place akbench can be
	// updated from here.
	Managed bool `json:"managed"`
	// Current is the running apiserver's version.
	Current string `json:"current"`
	// Operator is the operator Deployment's image tag: Flux upgrades it,
	// and a failed upgrade leaves it behind Target.
	Operator      string `json:"operator,omitempty"`
	OperatorError string `json:"operatorError,omitempty"`
	// Appliance is the appliance image's own version.
	Appliance string `json:"appliance,omitempty"`
	// Target is the version the appliance is rolling out (or running).
	Target string `json:"target,omitempty"`
	// Latest is the newest published build; LatestError why it's unknown.
	Latest      string `json:"latest,omitempty"`
	LatestError string `json:"latestError,omitempty"`
}

func (s *Server) latestVersion(ctx context.Context) (string, error) {
	checker := registry.Checker{HTTP: s.http}
	if s.imageRegistry != "" {
		checker.Endpoints = map[string]string{imageRegistryHost: s.imageRegistry}
	}
	return checker.Label(ctx, s.cfg.ApplianceImage, "latest", versionLabel)
}

func (s *Server) getUpdate(w http.ResponseWriter, r *http.Request) {
	view := updateView{Managed: s.cfg.Appliance, Current: Version}
	if !s.cfg.Appliance {
		writeJSON(w, r, http.StatusOK, view)
		return
	}
	var pin corev1.ConfigMap
	err := s.client.Get(r.Context(), types.NamespacedName{Namespace: appliance.SystemNamespace, Name: appliance.VersionConfigMap}, &pin)
	if err != nil && !apierrors.IsNotFound(err) {
		writeAPIError(w, r, err)
		return
	}
	view.Target = appliance.EffectiveVersion(s.cfg.ApplianceVersion, pin.Data)
	view.Appliance = s.cfg.ApplianceVersion
	if view.Operator, err = s.operatorVersion(r.Context()); err != nil {
		view.OperatorError = err.Error()
	}
	if view.Latest, err = s.latestVersion(r.Context()); err != nil {
		view.LatestError = err.Error()
	}
	writeJSON(w, r, http.StatusOK, view)
}

// operatorLabels select the operator chart's manager Deployment.
var operatorLabels = client.MatchingLabels{"control-plane": "controller-manager", "app.kubernetes.io/name": "authentik-bench-operator"}

// operatorVersion is the image tag of the operator's manager container.
func (s *Server) operatorVersion(ctx context.Context) (string, error) {
	var list appsv1.DeploymentList
	if err := s.client.List(ctx, &list, operatorLabels); err != nil {
		return "", err
	}
	for _, d := range list.Items {
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name != "manager" {
				continue
			}
			_, tag, _ := strings.Cut(c.Image[strings.LastIndex(c.Image, "/")+1:], ":")
			return tag, nil
		}
	}
	return "", fmt.Errorf("no operator Deployment found")
}

// applyUpdate pins the latest published build. Resolved here rather than
// taken from the request, so only a real published version can be pinned.
func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Appliance {
		writeError(w, r, http.StatusConflict, "outside the appliance, akbench is updated by upgrading its Helm release")
		return
	}
	latest, err := s.latestVersion(r.Context())
	if err == nil && !registry.TagPattern.MatchString(latest) {
		err = fmt.Errorf("%s:latest has no valid %s label (%q)", s.cfg.ApplianceImage, versionLabel, latest)
	}
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "could not look up the latest version: "+err.Error())
		return
	}
	cm := &corev1.ConfigMap{Namespace: appliance.SystemNamespace, Name: appliance.VersionConfigMap}
	if _, err := controllerutil.CreateOrUpdate(r.Context(), s.client, cm, func() error {
		cm.Data = map[string]string{appliance.VersionKeyTarget: latest, appliance.VersionKeyBase: s.cfg.ApplianceVersion}
		return nil
	}); err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, updateView{Managed: true, Current: Version, Target: latest, Latest: latest})
}
