package v1alpha1

// Labels shared across bench.goauthentik.io kinds and the workloads they
// render.
const (
	// LabelTestCase names the TestCase a TestRun or a k6 workload belongs to.
	// It is what makes `kubectl get testruns -l
	// bench.goauthentik.io/testcase=<name>` a test case's run history.
	LabelTestCase = "bench.goauthentik.io/testcase"
	// LabelProfile records which profile (quick/burn) a k6 workload runs.
	LabelProfile = "bench.goauthentik.io/profile"
	// LabelDeployment names the AuthentikDeployment a pod belongs to: set on
	// every one of a deployment's own pods (server, worker,
	// embedded/CNPG postgres) and on the k6 runner pod driving it, so
	// cross-deployment pod anti-affinity (internal/render/isolation.go) has a
	// single label to match on regardless of which component or postgres
	// mode is involved.
	LabelDeployment = "bench.goauthentik.io/deployment"
	// LabelParent names the parent TestCase an AuthentikDeployment, child
	// TestCase or extra-values ConfigMap was created for.
	LabelParent = "bench.goauthentik.io/parent"
	// LabelBuiltIn marks a TenantPreset the operator ships and resets to its
	// own numbers on every start, so the web UI can offer copying one
	// instead of editing it.
	LabelBuiltIn = "bench.goauthentik.io/built-in"
)
