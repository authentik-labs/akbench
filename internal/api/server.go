// Package api implements the benchmark web UI's backend: a thin REST/JSON
// layer over the management cluster's bench.goauthentik.io CRDs, plus a
// read-only PromQL proxy, a per-workload-cluster Grafana reverse proxy
// (grafana_proxy.go), an in-browser kubectl shell (shell.go) and the web UI's
// own static assets (static.go) and a read-only MCP server exposing the
// performance analytics to LLMs (mcp.go).
//
// The REST API deliberately talks to nothing but the management cluster -
// all multi-cluster logic lives in the operator. The shell is the one
// exception: it hands the user the operator's merged kubeconfig, which is
// the point of it.
package api

import (
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Config is everything the backend needs to serve the UI.
type Config struct {
	// Namespace is the single namespace bench CRs are read from and written
	// to. The UI is not namespace-aware by design.
	Namespace string

	// Token is a shared static bearer token requests may present. With both
	// Token and OIDC unset authentication is disabled, which is only ever
	// valid in tests - New's caller is responsible for refusing to start
	// that way.
	Token string

	// OIDC verifies OIDC-issued bearer tokens (oidc.go); nil disables OIDC
	// login. OIDCConfig is what the SPA is told to log in with.
	OIDC       TokenVerifier
	OIDCConfig OIDCConfig

	// MCPAuth puts the MCP endpoint behind Enterprise-Managed Authorization
	// (mcp_auth.go) instead of Token/OIDC; nil keeps it on those.
	MCPAuth *MCPAuthConfig

	// MCPResource is the MCP endpoint's public URL, advertised for OAuth
	// through the OIDC provider when OIDC is on and MCPAuth isn't
	// (mcp_oauth.go). Empty derives it from each request.
	MCPResource string

	// Shell enables the kubectl shell (shell.go); nil disables it.
	Shell *ShellConfig

	// UI is the built web UI (web/ui's dist/) served at every path the API
	// doesn't claim; nil serves no UI.
	UI fs.FS

	// PrometheusURL is the base URL of the central Prometheus the PromQL
	// proxy forwards to (e.g. http://prometheus.monitoring:9090).
	PrometheusURL string

	// CAPIAvailable reports whether cluster.x-k8s.io Clusters can be listed.
	// False (M3's single-cluster case) makes /clusters report just the
	// management cluster itself.
	CAPIAvailable bool

	// Appliance is true when running inside the single-container appliance
	// (Dockerfile.appliance), where infrastructure providers are enabled
	// from the UI (infrastructure.go). Elsewhere they're installed by hand.
	Appliance bool
	// ApplianceImage and ApplianceVersion are, inside the appliance, the
	// apiserver image repository and the appliance image's own akbench
	// version - what updating akbench from the UI (update.go) needs.
	ApplianceImage   string
	ApplianceVersion string

	// HTTPClient is used for Prometheus calls. Defaults to a 30s client.
	HTTPClient *http.Client

	// WSOriginPatterns are extra host patterns allowed to open a WebSocket
	// (shell or live-events) cross-origin - same-origin is always allowed.
	// Only non-empty for the dev-server CORS case (see cmd/apiserver's
	// --cors-origin).
	WSOriginPatterns []string
}

// Server holds the wiring shared by all handlers.
type Server struct {
	client client.WithWatch
	cfg    Config
	http   *http.Client
	shell  *shellState
	// grafanaKey signs the Grafana proxy's session cookies (grafana_session.go).
	grafanaKey []byte
	// grafanaAdminMu/grafanaAdminCache cache each workload cluster's
	// generated Grafana admin password (grafana_proxy.go), keyed by cluster
	// name - read-only and create-once at the source, so a cached value
	// never goes stale short of a manual rotation, which a process restart
	// would pick up anyway.
	grafanaAdminMu    sync.Mutex
	grafanaAdminCache map[string]string
	// grafanaTools* track re-exporting mcp-grafana's tools (grafana_mcp.go).
	grafanaToolsMu    sync.Mutex
	grafanaToolsAdded bool
	grafanaToolsTried time.Time
	traces            *breakdownCache[TraceBreakdown]
	profiles          *breakdownCache[ProfileBreakdown]
	logs              *breakdownCache[LogBreakdown]
	events            *eventHub
	eventsTickets     *eventsState
	// podExec runs a command in a workload cluster pod (sql.go); a field so
	// tests can fake it.
	podExec podExecFunc
	// clusterClient builds a clientset for a context of the merged
	// kubeconfig (rollout.go); a field so tests can fake it.
	clusterClient func(kubeconfig []byte, kubeContext string) (kubernetes.Interface, error)
	// imageRegistry overrides the ghcr.io endpoint checkImage (images.go) queries, in tests.
	imageRegistry string
	// mcpAuth is the MCP authorization server (mcp_auth.go), set with cfg.MCPAuth.
	mcpAuth *mcpAuth
}

// New builds a Server. The client must be backed by the management cluster
// and its scheme must know the bench.goauthentik.io types. It must support
// Watch (client.NewWithWatch, not client.New) - the live-events feed watches
// every bench resource kind through this same client.
func New(c client.WithWatch, cfg Config) *Server {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	s := &Server{
		client: c, cfg: cfg, http: httpClient, shell: newShellState(), grafanaKey: newSessionKey(),
		grafanaAdminCache: map[string]string{},
		traces:            newBreakdownCache[TraceBreakdown](),
		profiles:          newBreakdownCache[ProfileBreakdown](),
		logs:              newBreakdownCache[LogBreakdown](),
		events:            newEventHub(),
		eventsTickets:     newEventsState(),
		podExec:           execInPod,
		clusterClient: func(raw []byte, kubeContext string) (kubernetes.Interface, error) {
			cs, _, err := clientsetFor(raw, kubeContext)
			return cs, err
		},
	}
	if cfg.MCPAuth != nil {
		s.mcpAuth = newMCPAuth(cfg.MCPAuth)
	}
	return s
}

// probeStatusField is the JSON key /healthz and /readyz answer under -
// unrelated to (and coincidentally the same string as) every k8s object's
// own status field elsewhere in this package, kept named just for goconst's
// sake.
const probeStatusField = "status"

// Router builds the full HTTP surface. Everything under /api/v1 requires a
// bearer token except the auth discovery endpoint the login screen needs and
// the two WebSocket endpoints (shell, live events), which authenticate with
// a single-use ticket minted by an authenticated request (see shell.go,
// events.go). /healthz, /readyz and the UI's static assets are public.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, securityHeaders)

	probeOK := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusOK, map[string]string{probeStatusField: "ok"})
	}
	r.Get("/healthz", probeOK)
	r.Get("/readyz", probeOK)

	// Outside the /api/v1 group because an <iframe src=> can't carry a
	// bearer token - it authenticates with a session cookie instead, see
	// grafana_session.go.
	r.HandleFunc("/grafana-proxy/{cluster}/*", s.grafanaProxy)

	if s.mcpAuth != nil {
		if _, path, err := s.cfg.MCPAuth.metadataURL(); err == nil {
			r.Get(path, s.mcpAuth.resourceMetadata().ServeHTTP)
			r.Options(path, s.mcpAuth.resourceMetadata().ServeHTTP)
		}
		r.Get(wellKnownAuthServer, s.mcpAuth.serverMetadata)
		r.Post(mcpTokenPath, s.mcpAuth.token)
	}
	if s.mcpOAuthEnabled() {
		r.Get(s.mcpOAuthMetadataPath(), s.mcpOAuthMetadata)
		r.Options(s.mcpOAuthMetadataPath(), s.mcpOAuthMetadata)
	}

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/auth/config", s.authConfig)
		r.Get("/shell/connect", s.connectShell)
		r.Get("/events/connect", s.connectEvents)

		// Read-only MCP server for LLM clients (mcp.go), authenticated by
		// its own access tokens when MCPAuth is set (mcp_auth.go), else like
		// the rest of the API, challenging with OAuth discovery for the OIDC
		// provider when OIDC is on (mcp_oauth.go).
		switch {
		case s.mcpAuth != nil:
			r.With(s.mcpAuth.middleware()).Handle("/mcp", s.mcpHandler())
		case s.mcpOAuthEnabled():
			r.With(s.requireTokenWithChallenge(s.mcpOAuthChallenge)).Handle("/mcp", s.mcpHandler())
		}

		r.Group(func(r chi.Router) {
			r.Use(s.requireToken)

			r.Post("/grafana/session", s.createGrafanaSession)
			r.Get("/traces/breakdown", s.traceBreakdown)
			r.Get("/profiles/breakdown", s.profileBreakdown)
			r.Get("/logs/breakdown", s.logBreakdown)
			r.Get("/version", s.getVersion)
			r.Get("/clusters", s.listClusters)
			r.Post("/clusters", s.registerCluster)
			r.Get("/clusters/{name}", s.getCluster)
			r.Delete("/clusters/{name}", s.deleteCluster)
			r.Get("/clusters/{name}/nodes", s.listClusterNodes)
			r.Get("/clusters/{name}/monitoring-metrics", s.listMonitoringMetrics)

			for path, kind := range resourceKinds {
				r.Get("/"+path, s.listResource(kind))
				r.Post("/"+path, s.createResource(kind))
				r.Get("/"+path+"/{name}", s.getResource(kind))
				r.Put("/"+path+"/{name}", s.replaceResource(kind))
				r.Delete("/"+path+"/{name}", s.deleteResource(kind))
				r.Get("/"+path+"/{name}/status", s.getResourceStatus(kind))
				r.Get("/"+path+"/{name}/events", s.listObjectEvents(kind))
				r.Post("/"+path+"/{name}/reconcile", s.kickResource(kind))
			}

			r.Get("/deployments/{name}/extra-values", s.getExtraValues)
			r.Put("/deployments/{name}/extra-values", s.putExtraValues)
			r.Get("/deployments/{name}/helm-values", s.getHelmValues)
			r.Post("/deployments/{name}/sql", s.runSQL)
			r.Get("/deployments/{name}/rollout", s.getRollout)
			r.Post("/deployments/{name}/reset", s.resetDeployment)
			r.Get("/deployments/{name}/postgres-metrics", s.listPostgresMetrics)
			r.Get("/deployments/{name}/worker-metrics", s.listWorkerMetrics)

			r.Post("/testcases/{name}/runs", s.createRun)

			r.Get("/metrics/query", s.proxyQuery)
			r.Get("/metrics/compare", s.compare)
			r.Get("/metrics/range", s.metricsRange)
			r.Get("/metrics/resources", s.listResourceMetrics)

			r.Get("/infrastructure", s.getInfrastructure)
			r.Get("/infrastructure/update", s.getUpdate)
			r.Post("/infrastructure/update", s.applyUpdate)
			r.Post("/infrastructure/providers/{id}/enable", s.enableInfrastructureProvider)
			r.Get("/infrastructure/incus/credentials", s.listIncusCredentials)
			r.Post("/infrastructure/incus/credentials", s.createIncusCredential)
			r.Get("/infrastructure/incus/credentials/{name}", s.getIncusCredential)
			r.Put("/infrastructure/incus/credentials/{name}", s.updateIncusCredential)
			r.Delete("/infrastructure/incus/credentials/{name}", s.deleteIncusCredential)
			r.Post("/infrastructure/incus/credentials/{name}/test", s.testIncusCredential)
			r.Post("/infrastructure/incus/credentials/{name}/trust", s.trustIncusCredential)

			r.Get("/images/check", s.checkImage)

			r.Get("/alerts", s.listAlerts)
			r.Get("/placement", s.listPlacement)

			r.Get("/shell/contexts", s.listShellContexts)
			r.Post("/shell/sessions", s.createShellSession)

			r.Post("/events/sessions", s.createEventsSession)

			if s.mcpAuth == nil && !s.mcpOAuthEnabled() {
				r.Handle("/mcp", s.mcpHandler())
			}
		})

		// Without this, an unknown /api/v1 path would fall through to the
		// SPA's index.html and answer 200 with HTML.
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, http.StatusNotFound, "no such API route")
		})
	})

	if s.cfg.UI != nil {
		spa := spaHandler(s.cfg.UI, spaCSP(s.cfg.OIDCConfig.Issuer))
		r.Get("/*", spa.ServeHTTP)
		r.Head("/*", spa.ServeHTTP)
	}

	return r
}
