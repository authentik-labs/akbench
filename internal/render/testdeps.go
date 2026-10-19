package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// Images and ports for the TestCaseTemplateSpec.Dependencies services other
// than oidc-client (see oidctestclient.go for that one). Tags are pinned to
// versions confirmed against each image's own registry tag list - bump
// deliberately.
//
//   - saml-test-sp (github.com/BeryJu/saml-test-sp): crewjam/saml service
//     provider. Fetches its IdP's metadata (SP_METADATA_URL) once at startup
//     and panics if that fails, so authentik's SAML provider must exist first.
//   - saml-test-idp (github.com/BeryJu/saml-test-idp): crewjam/saml IdP with
//     two built-in users (user1/user1pass, user2/user2pass). Fetches its one
//     SP's metadata (IDP_METADATA_URL) at startup, so authentik's SAML source
//     must exist first too.
//   - dex: the OpenID provider authentik federates to via an OAuth source.
//     Unlike the other three, authentik talks to dex back-channel (token and
//     userinfo requests), so dex must be reachable from the deployment's own
//     cluster - true whenever the runner is that same cluster (the default).
//
// Images are pinned in deps/values.yaml.
var (
	SAMLTestSPImage  = pinnedImage("samlTestSP")
	SAMLTestIdPImage = pinnedImage("samlTestIdP")
	DexImage         = pinnedImage("dex")
)

const (
	SAMLTestSPPort = 9009

	SAMLTestIdPPort = 9009
	// SAMLTestIdPUsername/Password are saml-test-idp's hardcoded user1.
	SAMLTestIdPUsername = "user1"
	SAMLTestIdPPassword = "user1pass"

	DexPort = 5556
	// DexEmail/DexPassword log in to dex's one static password entry (dex's
	// local connector prompts for the email). DexUsername is what authentik
	// enrolls the user as - distinct from saml-test-idp's user1 so a template
	// combining both IdPs doesn't collide on one authentik username.
	DexUsername = "dex-user1"
	DexEmail    = "dex-user1@example.com"
	DexPassword = "dex-user1pass"
	// dexPasswordHash is bcrypt(DexPassword), checked by
	// TestDexPasswordHashMatches. Precomputed so the rendered config stays
	// deterministic across reconciles.
	dexPasswordHash = "$2a$10$rgidVs00BiB1YXnHwrig0ewZHnziEdBu8sfCroELByZigqVLFZAJq"

	dexConfigKey         = "config.yaml"
	dexConfigMountPath   = "/etc/dex/cfg"
	annotationConfigHash = "bench.goauthentik.io/config-hash"
)

// SAMLTestSPName, SAMLTestIdPName and DexName follow OIDCTestClientName: one
// instance per TestCase/target deployment pair, since each is configured
// against exactly one authentik.
func SAMLTestSPName(tc *benchv1alpha1.TestCase, deploymentName string) string {
	return "saml-test-sp-" + tc.Name + "-" + deploymentName
}

func SAMLTestIdPName(tc *benchv1alpha1.TestCase, deploymentName string) string {
	return "saml-test-idp-" + tc.Name + "-" + deploymentName
}

func DexName(tc *benchv1alpha1.TestCase, deploymentName string) string {
	return "dex-" + tc.Name + "-" + deploymentName
}

// ServiceURL is the cluster-local http://<name>.<namespace>.svc... root of a
// dependency Service.
func ServiceURL(name, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", name, namespace, port)
}

// DexIssuerURL is dex's issuer: what authentik's OAuth source endpoints are
// derived from, and what the browser (k6) is redirected to.
func DexIssuerURL(tc *benchv1alpha1.TestCase, deploymentName, namespace string) string {
	return ServiceURL(DexName(tc, deploymentName), namespace, DexPort) + "/dex"
}

// SAMLTestSPDeployment renders saml-test-sp for one TestCase/deployment pair.
// idpMetadataURL is authentik's SAML provider metadata; entityID must match
// the provider's audience.
func SAMLTestSPDeployment(tc *benchv1alpha1.TestCase, deploymentName, namespace, idpMetadataURL, entityID string) *appsv1.Deployment {
	name := SAMLTestSPName(tc, deploymentName)
	return dependencyDeployment(name, namespace, "saml-test-sp", SAMLTestSPImage, SAMLTestSPPort, "/health", []corev1.EnvVar{
		{Name: "SP_BIND", Value: fmt.Sprintf("0.0.0.0:%d", SAMLTestSPPort)},
		{Name: "SP_ROOT_URL", Value: ServiceURL(name, namespace, SAMLTestSPPort)},
		{Name: "SP_ENTITY_ID", Value: entityID},
		{Name: "SP_METADATA_URL", Value: idpMetadataURL},
	})
}

func SAMLTestSPService(tc *benchv1alpha1.TestCase, deploymentName, namespace string) *corev1.Service {
	return dependencyService(SAMLTestSPName(tc, deploymentName), namespace, SAMLTestSPPort)
}

// SAMLTestIdPDeployment renders saml-test-idp for one TestCase/deployment
// pair. spMetadataURL is authentik's SAML source metadata.
func SAMLTestIdPDeployment(tc *benchv1alpha1.TestCase, deploymentName, namespace, spMetadataURL string) *appsv1.Deployment {
	name := SAMLTestIdPName(tc, deploymentName)
	return dependencyDeployment(name, namespace, "saml-test-idp", SAMLTestIdPImage, SAMLTestIdPPort, "/health", []corev1.EnvVar{
		{Name: "IDP_BIND", Value: fmt.Sprintf("0.0.0.0:%d", SAMLTestIdPPort)},
		{Name: "IDP_ROOT_URL", Value: ServiceURL(name, namespace, SAMLTestIdPPort)},
		{Name: "IDP_METADATA_URL", Value: spMetadataURL},
	})
}

func SAMLTestIdPService(tc *benchv1alpha1.TestCase, deploymentName, namespace string) *corev1.Service {
	return dependencyService(SAMLTestIdPName(tc, deploymentName), namespace, SAMLTestIdPPort)
}

// DexConfig renders dex's config: in-memory storage, its password DB with
// one static user (DexEmail/DexPassword), and authentik's OAuth source as
// the only client. skipApprovalScreen keeps the login a plain
// form-post-redirect walk for k6.
func DexConfig(issuerURL, clientID, clientSecret, redirectURI string) string {
	return fmt.Sprintf(`issuer: %[1]s
storage:
  type: memory
web:
  http: 0.0.0.0:%[2]d
oauth2:
  skipApprovalScreen: true
enablePasswordDB: true
staticClients:
  - id: %[3]s
    name: authentik
    secret: %[4]s
    redirectURIs:
      - %[5]s
staticPasswords:
  - email: %[6]s
    hash: "%[7]s"
    username: %[8]s
    preferredUsername: %[8]s
    emailVerified: true
    userID: %[8]s
`, issuerURL, DexPort, clientID, clientSecret, redirectURI, DexEmail, dexPasswordHash, DexUsername)
}

// DexConfigSecret holds DexConfig's output (it embeds the client secret).
func DexConfigSecret(tc *benchv1alpha1.TestCase, deploymentName, namespace, config string) *corev1.Secret {
	return &corev1.Secret{
		Kind: kindSecret, APIVersion: apiVersionV1,
		Name: DexName(tc, deploymentName) + "-config", Namespace: namespace,
		StringData: map[string]string{dexConfigKey: config},
	}
}

// DexDeployment renders dex for one TestCase/deployment pair, mounting
// DexConfigSecret. config is only hashed into a pod annotation, so a config
// change rolls the pod (dex doesn't reload its config file).
func DexDeployment(tc *benchv1alpha1.TestCase, deploymentName, namespace, config string) *appsv1.Deployment {
	name := DexName(tc, deploymentName)
	dep := dependencyDeployment(name, namespace, "dex", DexImage, DexPort, "/dex/.well-known/openid-configuration", nil)
	sum := sha256.Sum256([]byte(config))
	dep.Spec.Template.Annotations = map[string]string{annotationConfigHash: hex.EncodeToString(sum[:])}
	pod := &dep.Spec.Template.Spec
	pod.Containers[0].Args = []string{"dex", "serve", dexConfigMountPath + "/" + dexConfigKey}
	pod.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "config", MountPath: dexConfigMountPath, ReadOnly: true}}
	pod.Volumes = []corev1.Volume{{
		Name:   "config",
		Secret: &corev1.SecretVolumeSource{SecretName: name + "-config"},
	}}
	return dep
}

func DexService(tc *benchv1alpha1.TestCase, deploymentName, namespace string) *corev1.Service {
	return dependencyService(DexName(tc, deploymentName), namespace, DexPort)
}

// dependencyDeployment is the single-replica, single-container Deployment
// every test dependency runs as, selected by app=<name>.
func dependencyDeployment(name, namespace, container, image string, port int32, readinessPath string, env []corev1.EnvVar) *appsv1.Deployment {
	labels := map[string]string{labelKeyApp: name}
	return &appsv1.Deployment{
		Kind: kindDeployment, APIVersion: apiVersionAppsV1,
		Name: name, Namespace: namespace, Labels: labels,
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  container,
						Image: image,
						Env:   env,
						Ports: []corev1.ContainerPort{{Name: portNameHTTP, ContainerPort: port}},
						ReadinessProbe: &corev1.Probe{
							HTTPGet: &corev1.HTTPGetAction{Path: readinessPath, Port: intstr.FromInt32(port)},
						},
					}},
				},
			},
		},
	}
}

// dependencyService is the ClusterIP Service in front of dependencyDeployment.
func dependencyService(name, namespace string, port int32) *corev1.Service {
	return &corev1.Service{
		Kind: kindService, APIVersion: apiVersionV1,
		Name: name, Namespace: namespace,
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{labelKeyApp: name},
			Ports: []corev1.ServicePort{
				{Name: portNameHTTP, Port: port, TargetPort: intstr.FromInt32(port)},
			},
		},
	}
}
