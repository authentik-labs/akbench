package render

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Shared literals repeated across this package's own hand-authored objects
// (as opposed to vendored third-party manifests) - every metav1.TypeMeta a
// project-authored Deployment/Service/Namespace/ConfigMap sets must match
// its real Go type exactly, so typo'd Kind/APIVersion strings are a real
// correctness risk worth centralizing against, not just a style nit.
const (
	kindNamespace  = "Namespace"
	kindConfigMap  = "ConfigMap"
	kindDeployment = "Deployment"
	kindService    = "Service"
	kindSecret     = "Secret"

	apiVersionV1     = "v1"
	apiVersionAppsV1 = "apps/v1"

	// portNameHTTP is the conventional single-port name every plain-HTTP
	// Service/container port in this package uses.
	portNameHTTP = "http"

	// labelKeyApp is the selector label every single-Deployment addon in this
	// package (Grafana, Pyroscope, Tempo, oidc-test-client) uses to tie its
	// own Deployment to its own Service.
	labelKeyApp = "app"

	// labelAppInstance is the standard Kubernetes "recommended label"
	// (kubernetes.io/docs/concepts/overview/working-with-objects/common-labels)
	// kube-prometheus-stack's own charts key their component Services by -
	// used to build a selector that reaches them without guessing their
	// release name.
	labelAppInstance = "app.kubernetes.io/instance"

	// topologyKeyHostname pins an affinity/anti-affinity term to "one match
	// per node" - every required (anti-)affinity term in this package uses
	// node-level, not zone-level, spreading.
	topologyKeyHostname = "kubernetes.io/hostname"
)

// maxNameLength is the DNS label limit Service names and label values
// (e.g. a Job's job-name) share.
const maxNameLength = 63

// nonDNSLabel matches what a DNS label can't contain. Object names may
// (dots), and objects created before the CRDs' name rules still exist.
var nonDNSLabel = regexp.MustCompile(`[^a-z0-9-]+`)

// BoundedName turns name into a DNS label of at most maxLen characters:
// anything but [a-z0-9-] becomes "-", and a name that's still too long is
// truncated with a hash of the full name appended, so long TestCase/
// deployment names still give unique, valid names. A name that already
// fits is returned unchanged.
func BoundedName(name string, maxLen int) string {
	label := nonDNSLabel.ReplaceAllString(strings.ToLower(name), "-")
	if len(label) <= maxLen {
		return label
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(label[:maxLen-9], "-") + "-" + hex.EncodeToString(sum[:])[:8]
}
