// Command apiserver serves the benchmark web UI's backend against the
// management cluster's Kubernetes API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// so a kubeconfig using them works when run outside the cluster.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/api"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(benchv1alpha1.AddToScheme(scheme))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "apiserver:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr            string
		tlsCertFile     string
		tlsKeyFile      string
		namespace       string
		prometheusURL   string
		tokenSecretName string
		corsOrigin      string
		uiDir           string
		oidcCfg         api.OIDCConfig
		oidcScopes      string
		mcpAuth         api.MCPAuthConfig
		mcpScopes       string
		shellBinary     string
		shellKubeconfig string
		shellSecret     string
	)
	flag.StringVar(&addr, "bind-address", ":8443", "The address the web API serves HTTPS on.")
	flag.StringVar(&tlsCertFile, "tls-cert-file", os.Getenv("BENCH_TLS_CERT_FILE"),
		"PEM certificate the web API serves. Without this and --tls-key-file, a self-signed certificate "+
			"is generated at startup.")
	flag.StringVar(&tlsKeyFile, "tls-key-file", os.Getenv("BENCH_TLS_KEY_FILE"),
		"PEM private key for --tls-cert-file.")
	flag.StringVar(&namespace, "namespace", envOr("BENCH_NAMESPACE", "authentik-bench"),
		"The namespace bench CRs are read from and written to.")
	flag.StringVar(&prometheusURL, "prometheus-url", os.Getenv("BENCH_PROMETHEUS_URL"),
		"Base URL of the Prometheus the PromQL proxy forwards to.")
	flag.StringVar(&tokenSecretName, "token-secret", os.Getenv("BENCH_API_TOKEN_SECRET"),
		"Name of a Secret in --namespace holding the UI bearer token under key \"token\". "+
			"Overridden by BENCH_API_TOKEN when that is set.")
	flag.StringVar(&corsOrigin, "cors-origin", os.Getenv("BENCH_CORS_ORIGIN"),
		"Origin allowed to call the API cross-origin, for running the frontend dev server against a remote backend.")
	flag.StringVar(&uiDir, "ui-dir", os.Getenv("BENCH_UI_DIR"),
		"Directory holding the built web UI (web/ui's dist/), served at every path the API doesn't claim. "+
			"Empty serves no UI.")
	flag.StringVar(&oidcCfg.Issuer, "oidc-issuer", os.Getenv("BENCH_OIDC_ISSUER"),
		"OIDC issuer URL. Enables OIDC login alongside (or, without a token, instead of) the static token.")
	flag.StringVar(&oidcCfg.ClientID, "oidc-client-id", os.Getenv("BENCH_OIDC_CLIENT_ID"),
		"OIDC client ID of the web UI (a public client); also the audience accepted tokens must carry.")
	flag.StringVar(&oidcScopes, "oidc-scopes", os.Getenv("BENCH_OIDC_SCOPES"),
		"Comma-separated scopes the web UI requests. Defaults to openid,profile,email.")
	flag.StringVar(&oidcCfg.RequiredGroup, "oidc-required-group", os.Getenv("BENCH_OIDC_REQUIRED_GROUP"),
		"If set, OIDC tokens must list this group in their groups claim.")
	flag.StringVar(&mcpAuth.Resource, "mcp-resource-url", os.Getenv("BENCH_MCP_RESOURCE_URL"),
		"Public URL of the MCP endpoint (e.g. https://bench.example.com/api/v1/mcp), its OAuth resource "+
			"identifier. Required with --mcp-idp-issuer; otherwise optional, and derived from each request "+
			"when unset.")
	flag.StringVar(&mcpAuth.IdPIssuer, "mcp-idp-issuer", os.Getenv("BENCH_MCP_IDP_ISSUER"),
		"Issuer of the enterprise IdP whose ID-JAGs akbench trusts for MCP access tokens. Setting it makes "+
			"/api/v1/mcp accept only Enterprise-Managed Authorization tokens, granted by akbench itself as the "+
			"authorization server at --mcp-resource-url's origin. Unset, MCP takes the same tokens as the API, "+
			"and with --oidc-issuer advertises that issuer for OAuth.")
	flag.StringVar(&mcpAuth.ClientID, "mcp-client-id", os.Getenv("BENCH_MCP_CLIENT_ID"),
		"Client ID of the MCP client at akbench's token endpoint; its secret is read from BENCH_MCP_CLIENT_SECRET.")
	flag.StringVar(&mcpScopes, "mcp-scopes", os.Getenv("BENCH_MCP_SCOPES"),
		"Comma-separated scopes MCP access tokens must carry. Optional.")
	flag.StringVar(&shellBinary, "shell", envOr("BENCH_SHELL", "/bin/bash"),
		"Shell the in-browser kubectl shell runs.")
	flag.StringVar(&shellKubeconfig, "shell-kubeconfig", os.Getenv("BENCH_SHELL_KUBECONFIG"),
		"Kubeconfig file the in-browser shell uses. For local development; in-cluster, use --shell-kubeconfig-secret.")
	flag.StringVar(&shellSecret, "shell-kubeconfig-secret", os.Getenv("BENCH_SHELL_KUBECONFIG_SECRET"),
		"<namespace>/<name> of the merged-kubeconfig Secret the operator maintains (key \"config\"), "+
			"which the in-browser shell uses. Read fresh for every session. Without this or --shell-kubeconfig "+
			"the shell is disabled.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("apiserver")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	// NewWithWatch, not New: the live-events feed (api.Server.RunEventWatchers)
	// watches every bench resource kind through this same client.
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	ctx := ctrl.SetupSignalHandler()
	token, err := resolveToken(ctx, c, namespace, tokenSecretName, oidcCfg.Issuer != "")
	if err != nil {
		return err
	}

	var verifier api.TokenVerifier
	if oidcCfg.Issuer != "" {
		if oidcScopes != "" {
			oidcCfg.Scopes = strings.Split(oidcScopes, ",")
		}
		if verifier, err = api.NewOIDCVerifier(oidcCfg); err != nil {
			return err
		}
		log.Info("OIDC login enabled", "issuer", oidcCfg.Issuer, "clientID", oidcCfg.ClientID)
		if oidcCfg.RequiredGroup == "" {
			log.Info("WARNING: --oidc-required-group is not set - any account that can authenticate " +
				"against the OIDC issuer above is granted full access to this API, including a " +
				"cluster-admin kubectl shell if one is configured. Set --oidc-required-group (or " +
				"BENCH_OIDC_REQUIRED_GROUP) before exposing this backend beyond a trusted network.")
		}
	}

	var mcpAuthCfg *api.MCPAuthConfig
	if mcpAuth.IdPIssuer != "" {
		mcpAuth.ClientSecret = os.Getenv("BENCH_MCP_CLIENT_SECRET")
		if mcpScopes != "" {
			mcpAuth.Scopes = strings.Split(mcpScopes, ",")
		}
		if err := mcpAuth.Validate(); err != nil {
			return err
		}
		mcpAuthCfg = &mcpAuth
		log.Info("MCP Enterprise-Managed Authorization enabled", "idpIssuer", mcpAuth.IdPIssuer, "resource", mcpAuth.Resource)
	} else if oidcCfg.Issuer != "" {
		log.Info("MCP OAuth through the OIDC provider enabled", "issuer", oidcCfg.Issuer, "resource", mcpAuth.Resource)
	}

	wsOrigins := wsOriginPatterns(corsOrigin)
	shell, err := shellConfig(c, shellBinary, shellKubeconfig, shellSecret, wsOrigins)
	if err != nil {
		return err
	}
	if shell == nil {
		log.Info("No shell kubeconfig configured; the in-browser kubectl shell is disabled")
	}

	var ui fs.FS
	if uiDir != "" {
		if _, err := os.Stat(uiDir + "/index.html"); err != nil {
			return fmt.Errorf("--ui-dir: %w", err)
		}
		ui = os.DirFS(uiDir)
	}

	capiAvailable := hasCAPI(cfg)
	if !capiAvailable {
		log.Info("Cluster API is not installed; /clusters reports the management cluster only")
	}

	srv := api.New(c, api.Config{
		Namespace:        namespace,
		Token:            token,
		PrometheusURL:    prometheusURL,
		CAPIAvailable:    capiAvailable,
		Appliance:        os.Getenv("BENCH_APPLIANCE") == "true",
		ApplianceImage:   os.Getenv("BENCH_APPLIANCE_IMAGE"),
		ApplianceVersion: os.Getenv("BENCH_APPLIANCE_VERSION"),
		OIDC:             verifier,
		OIDCConfig:       oidcCfg,
		MCPAuth:          mcpAuthCfg,
		MCPResource:      mcpAuth.Resource,
		Shell:            shell,
		UI:               ui,
		WSOriginPatterns: wsOrigins,
	})
	go srv.RunEventWatchers(ctx)

	handler := srv.Router()
	if corsOrigin != "" {
		handler = withCORS(handler, corsOrigin)
	}

	tlsCfg, selfSigned, err := tlsConfig(tlsCertFile, tlsKeyFile)
	if err != nil {
		return err
	}
	if selfSigned {
		log.Info("No TLS certificate configured; serving a self-signed certificate")
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "Failed to shut down cleanly")
		}
	}()

	log.Info("Serving web API", "address", addr, "namespace", namespace)
	// The certificate comes from TLSConfig, hence the empty file arguments.
	if err := httpServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// resolveToken loads the shared bearer token, preferring the environment (for
// local development) over the Secret the deployment mounts. Starting with
// neither a token nor OIDC is refused rather than defaulted: an
// unauthenticated backend has full write access to every benchmark CRD and
// hands out cluster-admin shells.
func resolveToken(ctx context.Context, c client.Client, namespace, secretName string, oidcEnabled bool) (string, error) {
	if token := os.Getenv("BENCH_API_TOKEN"); token != "" {
		return checkTokenLength(token, "BENCH_API_TOKEN")
	}
	if secretName == "" {
		if oidcEnabled {
			return "", nil
		}
		return "", errors.New("no authentication configured: set BENCH_API_TOKEN, --token-secret or --oidc-issuer")
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, &secret); err != nil {
		return "", fmt.Errorf("read token secret %s/%s: %w", namespace, secretName, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", fmt.Errorf("secret %s/%s has no non-empty \"token\" key", namespace, secretName)
	}
	return checkTokenLength(token, fmt.Sprintf("secret %s/%s", namespace, secretName))
}

// checkTokenLength refuses a static token short enough to guess: nothing
// rate-limits failed logins, so length is the only protection it has.
func checkTokenLength(token, source string) (string, error) {
	if len(token) < api.MinStaticTokenLength {
		return "", fmt.Errorf("%s: the API token must be at least %d characters (e.g. `openssl rand -hex 32`)",
			source, api.MinStaticTokenLength)
	}
	return token, nil
}

// shellConfig builds the in-browser shell's configuration from whichever
// kubeconfig source is set, or returns nil (shell disabled) for neither.
func shellConfig(c client.Client, shellBinary, file, secretRef string, wsOrigins []string) (*api.ShellConfig, error) {
	var load func(context.Context) ([]byte, error)
	switch {
	case file != "" && secretRef != "":
		return nil, errors.New("set only one of --shell-kubeconfig and --shell-kubeconfig-secret")
	case file != "":
		load = func(context.Context) ([]byte, error) { return os.ReadFile(file) }
	case secretRef != "":
		ns, name, ok := strings.Cut(secretRef, "/")
		if !ok || ns == "" || name == "" {
			return nil, fmt.Errorf("--shell-kubeconfig-secret %q: want <namespace>/<name>", secretRef)
		}
		load = func(ctx context.Context) ([]byte, error) {
			var secret corev1.Secret
			if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &secret); err != nil {
				return nil, err
			}
			raw := secret.Data["config"]
			if len(raw) == 0 {
				return nil, fmt.Errorf("secret %s has no \"config\" key", secretRef)
			}
			return raw, nil
		}
	default:
		return nil, nil
	}
	if _, err := os.Stat(shellBinary); err != nil {
		return nil, fmt.Errorf("--shell: %w", err)
	}
	return &api.ShellConfig{Kubeconfig: load, Shell: shellBinary, OriginPatterns: wsOrigins}, nil
}

// wsOriginPatterns derives the extra host patterns a WebSocket upgrade
// (shell or live events) accepts cross-origin from the same --cors-origin
// flag the plain-HTTP CORS middleware uses - empty unless that's set, which
// only happens for the dev-server-against-a-remote-backend case.
func wsOriginPatterns(corsOrigin string) []string {
	if corsOrigin == "" {
		return nil
	}
	// AcceptOptions.OriginPatterns match host[:port], not full origins.
	return []string{strings.TrimPrefix(strings.TrimPrefix(corsOrigin, "https://"), "http://")}
}

// hasCAPI reports whether cluster.x-k8s.io Clusters are servable, which is
// what decides whether /clusters can list anything beyond the management
// cluster itself.
func hasCAPI(cfg *rest.Config) bool {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return false
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return false
	}
	_, err = mapper.RESTMapping(schema.GroupKind{Group: "cluster.x-k8s.io", Kind: "Cluster"}, "v1beta2")
	return err == nil
}

// withCORS allows a frontend dev server to call a backend on another origin.
// Deliberately narrow: one configured origin, the methods this API uses, and
// the Authorization header.
func withCORS(next http.Handler, origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
