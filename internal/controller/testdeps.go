package controller

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

const dependencyBlueprintKey = "blueprint.yaml"

//go:embed blueprints/dependencies.yaml.tmpl
var dependencyBlueprintTemplate string

// quote makes every templated value a double-quoted YAML scalar: Go's escape
// sequences (\n, \", \xNN, \uNNNN, \UNNNNNNNN) are all valid YAML ones too.
var dependencyBlueprintTmpl = template.Must(template.New("dependencies").Funcs(template.FuncMap{"quote": strconv.Quote}).Parse(dependencyBlueprintTemplate))

// dependencyBlueprint is the data blueprints/dependencies.yaml.tmpl renders:
// one section per dependency in use, nil ones are left out.
type dependencyBlueprint struct {
	OIDCClient *oidcClientBlueprint
	SAMLClient *samlClientBlueprint
	OIDCSource *oidcSourceBlueprint
	SAMLSource *samlSourceBlueprint
	OAuthToken *oauthTokenBlueprint
}

type (
	oidcClientBlueprint struct{ ID, Secret, RedirectURI string }
	samlClientBlueprint struct{ ID, ACSURL, Audience string }
	oidcSourceBlueprint struct{ Slug, Secret, IssuerURL string }
	samlSourceBlueprint struct{ Slug, SSOURL string }
	oauthTokenBlueprint struct{ ID, Secret, ExchangeID, ExchangeSecret string }
)

func (b dependencyBlueprint) render() (string, error) {
	var out strings.Builder
	if err := dependencyBlueprintTmpl.Execute(&out, b); err != nil {
		return "", fmt.Errorf("render dependency blueprint: %w", err)
	}
	return out.String(), nil
}

// dependencyIDs are the authentik-side names (provider/application name and
// slug, source slug, OAuth client ID) for one TestCase/deployment pair.
type dependencyIDs struct {
	oidcClient, samlClient, oidcSource, samlSource string
	oauthToken, oauthExchange                      string
}

func idsFor(tc *benchv1alpha1.TestCase, deploymentName string) dependencyIDs {
	suffix := tc.Name + "-" + deploymentName
	return dependencyIDs{
		oidcClient: "oauth-test-" + suffix,
		samlClient: "saml-test-" + suffix,
		oidcSource: "oidc-idp-" + suffix,
		samlSource: "saml-idp-" + suffix,

		oauthToken:    "oauth-cc-" + suffix,
		oauthExchange: "oauth-te-" + suffix,
	}
}

// dependencyEndpoints is where tc's k6 script finds each of spec's
// dependencies for one target - a pure function of names/namespace, so
// TestRunReconciler can call it once the TestCase itself reports Ready
// (which TestCaseReconciler only does once ensureDependencies has finished
// provisioning them) without re-running any of the ensure logic.
func dependencyEndpoints(tc *benchv1alpha1.TestCase, deploymentName string, spec *benchv1alpha1.TestCaseTemplateSpec) render.DependencyEndpoints {
	var out render.DependencyEndpoints
	ids := idsFor(tc, deploymentName)
	if spec.HasDependency(benchv1alpha1.DependencyOIDCClient) {
		out.OIDCClientHost = serviceHost(render.OIDCTestClientName(tc, deploymentName), tc.Namespace)
		out.OIDCClientPort = render.OIDCTestClientPort
	}
	if spec.HasDependency(benchv1alpha1.DependencySAMLClient) {
		out.SAMLClientHost = serviceHost(render.SAMLTestSPName(tc, deploymentName), tc.Namespace)
		out.SAMLClientPort = render.SAMLTestSPPort
	}
	if spec.HasDependency(benchv1alpha1.DependencyOIDCIdP) {
		out.OIDCSourceSlug = ids.oidcSource
	}
	if spec.HasDependency(benchv1alpha1.DependencySAMLIdP) {
		out.SAMLSourceSlug = ids.samlSource
	}
	if spec.HasDependency(benchv1alpha1.DependencyOAuthToken) {
		out.OAuthClientID = ids.oauthToken
		out.OAuthSecretName = ids.oauthToken
		out.OAuthExchangeClientID = ids.oauthExchange
		out.OAuthExchangeSecretName = ids.oauthExchange
	}
	return out
}

func serviceHost(name, namespace string) string {
	return name + "." + namespace + ".svc.cluster.local"
}

// ensureDependencies provisions spec's TestCaseTemplateSpec.Dependencies for
// one target: a single blueprint registering every authentik-side object
// they need (providers/applications for the clients, sources for the IdPs)
// in the target AuthentikDeployment, then - once that import Job has
// completed - each dependency's own Deployment/Service on rc.
//
// The order matters: saml-test-sp and saml-test-idp fetch authentik's
// metadata once at startup and exit if it 404s, and oidc-test-client does
// the same with its OIDC discovery document, so starting them before the
// blueprint lands only produces a CrashLoopBackOff.
//
// Dependencies run on rc (owned by tc, not topo - see
// TestCaseTemplateSpec.Dependencies' own doc comment for why), the cluster
// this reconcile already knows can reach BenchHost/BenchPort. The blueprint
// Job, in contrast, always runs on topo's own cluster (deploymentClient
// below), which can differ from rc when spec.runnerClusterRef is set. dex is
// the one dependency authentik itself calls (token/userinfo), so oidc-idp
// additionally needs rc reachable from topo's cluster - true by default,
// where both are the same cluster.
//
// RBAC markers for the extra resources this needs (services, secrets,
// batch/jobs) live on TestCaseReconciler.Reconcile in testcase_controller.go,
// not here - controller-gen's RBAC generator only picks up markers attached
// to a Reconcile method, confirmed by testing: the identical marker on this
// helper was silently dropped from the generated ClusterRole.
func (r *TestCaseReconciler) ensureDependencies(ctx context.Context, rc client.Client, remote bool, tc *benchv1alpha1.TestCase, deploymentName string, topo *benchv1alpha1.AuthentikDeployment, spec *benchv1alpha1.TestCaseTemplateSpec, host string, port int32) (render.DependencyEndpoints, error) {
	if len(spec.Dependencies) == 0 {
		return render.DependencyEndpoints{}, nil
	}
	ids := idsFor(tc, deploymentName)
	authentikURL := fmt.Sprintf("http://%s:%d", host, port)
	var blueprint dependencyBlueprint
	var objs []client.Object

	if spec.HasDependency(benchv1alpha1.DependencyOIDCClient) {
		secret, err := ensureRandomSecret(ctx, rc,
			types.NamespacedName{Name: render.OIDCTestClientName(tc, deploymentName) + "-secret", Namespace: tc.Namespace},
			"client-secret", 32)
		if err != nil {
			return render.DependencyEndpoints{}, fmt.Errorf("ensure oauth client secret: %w", err)
		}
		redirectURI := render.OIDCTestClientRootURL(tc, deploymentName, tc.Namespace) + "/auth/callback"
		blueprint.OIDCClient = &oidcClientBlueprint{ids.oidcClient, secret, redirectURI}
		issuerURL := fmt.Sprintf("%s/application/o/%s/", authentikURL, ids.oidcClient)
		objs = append(objs,
			render.OIDCTestClientDeployment(tc, deploymentName, tc.Namespace, issuerURL, ids.oidcClient, secret),
			render.OIDCTestClientService(tc, deploymentName, tc.Namespace))
	}

	if spec.HasDependency(benchv1alpha1.DependencySAMLClient) {
		spURL := render.ServiceURL(render.SAMLTestSPName(tc, deploymentName), tc.Namespace, render.SAMLTestSPPort)
		entityID := spURL + "/saml/metadata"
		blueprint.SAMLClient = &samlClientBlueprint{ids.samlClient, spURL + "/saml/acs", entityID}
		metadataURL := fmt.Sprintf("%s/application/saml/%s/metadata/", authentikURL, ids.samlClient)
		objs = append(objs,
			render.SAMLTestSPDeployment(tc, deploymentName, tc.Namespace, metadataURL, entityID),
			render.SAMLTestSPService(tc, deploymentName, tc.Namespace))
	}

	if spec.HasDependency(benchv1alpha1.DependencyOIDCIdP) {
		secret, err := ensureRandomSecret(ctx, rc,
			types.NamespacedName{Name: render.DexName(tc, deploymentName) + "-client-secret", Namespace: tc.Namespace},
			"client-secret", 32)
		if err != nil {
			return render.DependencyEndpoints{}, fmt.Errorf("ensure dex client secret: %w", err)
		}
		issuerURL := render.DexIssuerURL(tc, deploymentName, tc.Namespace)
		blueprint.OIDCSource = &oidcSourceBlueprint{ids.oidcSource, secret, issuerURL}
		redirectURI := fmt.Sprintf("%s/source/oauth/callback/%s/", authentikURL, ids.oidcSource)
		config := render.DexConfig(issuerURL, ids.oidcSource, secret, redirectURI)
		objs = append(objs,
			render.DexConfigSecret(tc, deploymentName, tc.Namespace, config),
			render.DexDeployment(tc, deploymentName, tc.Namespace, config),
			render.DexService(tc, deploymentName, tc.Namespace))
	}

	if spec.HasDependency(benchv1alpha1.DependencySAMLIdP) {
		idpURL := render.ServiceURL(render.SAMLTestIdPName(tc, deploymentName), tc.Namespace, render.SAMLTestIdPPort)
		blueprint.SAMLSource = &samlSourceBlueprint{ids.samlSource, idpURL + "/sso"}
		metadataURL := fmt.Sprintf("%s/source/saml/%s/metadata/", authentikURL, ids.samlSource)
		objs = append(objs,
			render.SAMLTestIdPDeployment(tc, deploymentName, tc.Namespace, metadataURL),
			render.SAMLTestIdPService(tc, deploymentName, tc.Namespace))
	}

	if spec.HasDependency(benchv1alpha1.DependencyOAuthToken) {
		// On rc, next to the k6 workload that reads them; named after the client.
		secret, err := ensureRandomSecret(ctx, rc,
			types.NamespacedName{Name: ids.oauthToken, Namespace: tc.Namespace}, render.OAuthClientSecretKey, 32)
		if err != nil {
			return render.DependencyEndpoints{}, fmt.Errorf("ensure oauth token client secret: %w", err)
		}
		exchangeSecret, err := ensureRandomSecret(ctx, rc,
			types.NamespacedName{Name: ids.oauthExchange, Namespace: tc.Namespace}, render.OAuthClientSecretKey, 32)
		if err != nil {
			return render.DependencyEndpoints{}, fmt.Errorf("ensure oauth exchange client secret: %w", err)
		}
		blueprint.OAuthToken = &oauthTokenBlueprint{ids.oauthToken, secret, ids.oauthExchange, exchangeSecret}
	}

	content, err := blueprint.render()
	if err != nil {
		return render.DependencyEndpoints{}, err
	}
	if err := r.ensureDependencyBlueprint(ctx, tc, deploymentName, topo, content); err != nil {
		return render.DependencyEndpoints{}, err
	}
	for _, obj := range objs {
		if !remote {
			if err := controllerutil.SetControllerReference(tc, obj, r.Scheme); err != nil {
				return render.DependencyEndpoints{}, err
			}
		}
		if err := applyDependencyObject(ctx, rc, obj); err != nil {
			return render.DependencyEndpoints{}, fmt.Errorf("apply %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return dependencyEndpoints(tc, deploymentName, spec), nil
}

// ensureDependencyBlueprint imports content into topo via a one-shot Job and
// returns nil once that Job has completed. A Job, not an exec into the
// server pod: see render.BlueprintJob's own doc comment - the same
// RWO-volume rolling-update deadlock that motivated moving the fixtures
// blueprint off the server Deployment entirely applies here too, since both
// share the exact same BlueprintPVC/BlueprintMountPath.
func (r *TestCaseReconciler) ensureDependencyBlueprint(ctx context.Context, tc *benchv1alpha1.TestCase, deploymentName string, topo *benchv1alpha1.AuthentikDeployment, content string) error {
	deploymentClient, _, err := resolveRunnerClient(ctx, r.Tracker, r.Client, tc.Namespace, nil, topo.Spec.ClusterRef)
	if err != nil {
		return fmt.Errorf("resolve deployment cluster client: %w", err)
	}

	cmName := "deps-blueprint-" + tc.Name + "-" + deploymentName
	var existingCM corev1.ConfigMap
	switch err := deploymentClient.Get(ctx, types.NamespacedName{Name: cmName, Namespace: topo.Spec.Namespace}, &existingCM); {
	case apierrors.IsNotFound(err):
		cm := &corev1.ConfigMap{
			Name: cmName, Namespace: topo.Spec.Namespace,
			Data: map[string]string{dependencyBlueprintKey: content},
		}
		if err := deploymentClient.Create(ctx, cm); err != nil {
			return fmt.Errorf("create dependency blueprint configmap: %w", err)
		}
	case err != nil:
		return fmt.Errorf("get dependency blueprint configmap: %w", err)
	default:
		existingCM.Data = map[string]string{dependencyBlueprintKey: content}
		if err := deploymentClient.Update(ctx, &existingCM); err != nil {
			return fmt.Errorf("update dependency blueprint configmap: %w", err)
		}
	}

	jobName := render.BlueprintJobName(topo, "deps-"+tc.Name+"-"+deploymentName, content)
	var job batchv1.Job
	switch err := deploymentClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: topo.Spec.Namespace}, &job); {
	case apierrors.IsNotFound(err):
		// topo's own clusterRef, not the runner's (tc.Spec.RunnerClusterRef
		// can differ) - this Job runs via deploymentClient, against the
		// deployment's own namespace/pool, not the runner's.
		wc := resolveWorkloadCluster(ctx, r.Client, topo.Namespace, topo.Spec.ClusterRef.Name)
		newJob := render.BlueprintJob(topo, wc, jobName, cmName, dependencyBlueprintKey)
		if err := deploymentClient.Create(ctx, newJob); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create dependency blueprint import job: %w", err)
		}
		return fmt.Errorf("waiting for dependency blueprint import job %q to start", jobName)
	case err != nil:
		return fmt.Errorf("get dependency blueprint import job: %w", err)
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return fmt.Errorf("dependency blueprint import job %q failed: %s", jobName, c.Message)
		}
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return nil
		}
	}
	return fmt.Errorf("waiting for dependency blueprint import job %q to complete", jobName)
}

// applyDependencyObject creates obj, or updates the fields render sets on an
// existing one, leaving everything else (e.g. a Service's clusterIP) alone.
func applyDependencyObject(ctx context.Context, c client.Client, obj client.Object) error {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a client.Object", obj)
	}
	switch err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, obj)
	case err != nil:
		return err
	}
	switch d := obj.(type) {
	case *appsv1.Deployment:
		e := existing.(*appsv1.Deployment)
		e.Spec = d.Spec
	case *corev1.Service:
		e := existing.(*corev1.Service)
		e.Spec.Selector = d.Spec.Selector
		e.Spec.Ports = d.Spec.Ports
	case *corev1.Secret:
		e := existing.(*corev1.Secret)
		e.Data = nil
		e.StringData = d.StringData
	default:
		return fmt.Errorf("applyDependencyObject: unsupported type %T", obj)
	}
	return c.Update(ctx, existing)
}
