package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/controller"
	"github.com/authentik-labs/akbench/internal/multicluster"
	webhookv1alpha1 "github.com/authentik-labs/akbench/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(benchv1alpha1.AddToScheme(scheme))
	utilruntime.Must(helmv2.AddToScheme(scheme))
	utilruntime.Must(sourcev1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(controlplanev1.AddToScheme(scheme))
	utilruntime.Must(bootstrapv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the validating webhook server listens on.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	// Without --webhook-cert-path (e.g. `make dev`), controller-runtime looks
	// for certificates in its default temp directory; set ENABLE_WEBHOOKS=false
	// to run without them.
	webhookServerOptions := webhook.Options{
		Port:    webhookPort,
		TLSOpts: tlsOpts,
	}
	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)
		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhook.NewServer(webhookServerOptions),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "6f5d89a4.goauthentik.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	tracker, err := multicluster.SetupWithManager(ctx, mgr)
	if err != nil {
		setupLog.Error(err, "Failed to set up multicluster tracker")
		os.Exit(1)
	}

	if err := (&controller.AuthentikDeploymentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Tracker:  tracker,
		Recorder: mgr.GetEventRecorderFor("authentikdeployment-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "authentikdeployment")
		os.Exit(1)
	}
	if err := (&controller.TestCaseReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Tracker:  tracker,
		Recorder: mgr.GetEventRecorderFor("testcase-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "testcase")
		os.Exit(1)
	}
	if err := (&controller.TestCaseTemplateReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("testcasetemplate-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "testcasetemplate")
		os.Exit(1)
	}
	if err := (&controller.TestRunReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Tracker:  tracker,
		Recorder: mgr.GetEventRecorderFor("testrun-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "testrun")
		os.Exit(1)
	}
	if err := (&controller.WorkloadClusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("workloadcluster-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workloadcluster")
		os.Exit(1)
	}
	if err := (&controller.ShellKubeconfigReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Namespace:  envOr("BENCH_SHELL_KUBECONFIG_NAMESPACE", "authentik-bench"),
		SecretName: envOr("BENCH_SHELL_KUBECONFIG_SECRET_NAME", "shell-kubeconfig"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "shellkubeconfig")
		os.Exit(1)
	}
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha1.SetupAuthentikDeploymentWebhookWithManager(mgr, tracker); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "AuthentikDeployment")
			os.Exit(1)
		}
		if err := webhookv1alpha1.SetupTestCaseWebhookWithManager(mgr, tracker); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "TestCase")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	// Seeded with a plain, uncached client (the manager's own GetClient()
	// only works once its cache has started, i.e. after mgr.Start below) so
	// any failure here - e.g. the CRD not installed yet - surfaces
	// immediately as a clear startup error rather than racing the manager's
	// own lifecycle.
	seedClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		setupLog.Error(err, "Failed to build seed client")
		os.Exit(1)
	}
	if err := controller.SeedDefaultTestCaseTemplates(ctx, seedClient, envOr("BENCH_NAMESPACE", "default")); err != nil {
		setupLog.Error(err, "Failed to seed default TestCaseTemplates")
		os.Exit(1)
	}
	// Synthetic test users, unless already loaded (e.g. by `make fixtures`).
	// Not fatal: without them only test cases are blocked, and they say so.
	if users, seed, err := fixtureSettings(); err != nil {
		setupLog.Error(err, "Invalid test user settings; not seeding test users")
	} else if err := controller.SeedFixtures(
		ctx, seedClient, envOr("BENCH_NAMESPACE", "default"), users, seed,
	); err != nil {
		setupLog.Error(err, "Failed to seed test users")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// fixtureSettings reads how many test users SeedFixtures generates
// (BENCH_FIXTURE_USERS, default 50 like `make fixtures`) and from which seed
// (BENCH_FIXTURE_SEED, for a reproducible directory; random by default, so
// every install's passwords differ).
func fixtureSettings() (int, uint64, error) {
	users, err := strconv.Atoi(envOr("BENCH_FIXTURE_USERS", "50"))
	if err != nil || users < 1 {
		return 0, 0, fmt.Errorf("BENCH_FIXTURE_USERS must be a positive number, got %q", os.Getenv("BENCH_FIXTURE_USERS"))
	}
	if v := os.Getenv("BENCH_FIXTURE_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("BENCH_FIXTURE_SEED must be a number, got %q", v)
		}
		return users, seed, nil
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, 0, err
	}
	return users, binary.LittleEndian.Uint64(b[:]), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
