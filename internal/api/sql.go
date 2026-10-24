package api

// Read-only SQL against a deployment's own Postgres, for diagnosing a run
// from a script or an agent without driving the interactive shell:
//
//	POST /api/v1/deployments/{name}/sql {"query": "EXPLAIN ANALYZE ...", "format": "aligned"}
//
// The query is fed to psql's stdin inside the deployment's Postgres pod (the
// embedded chart's StatefulSet pod, or a CNPG cluster's primary), reached
// through the same merged kubeconfig the shell uses - so this endpoint is
// only available when the shell is, and grants nothing the shell doesn't
// already: anyone allowed here could kubectl exec into the same pod.
//
// The session runs with default_transaction_read_only and a statement
// timeout. That is a guardrail against an accidental write, not a security
// boundary: SQL can SET it back off, and psql meta-commands (\!) work too.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
)

const (
	// sqlStatementTimeout bounds each statement server-side; sqlExecTimeout
	// bounds the whole exec (pod lookup, connection, every statement).
	sqlStatementTimeout = "60s"
	sqlExecTimeout      = 90 * time.Second
	// sqlMaxOutputBytes caps each of stdout/stderr kept in memory.
	sqlMaxOutputBytes = 4 << 20

	sqlFormatAligned = "aligned"
	sqlFormatCSV     = "csv"
)

// sqlPsqlScript runs psql over the pod's local socket. Nothing from the
// request is ever interpolated into it: the query arrives on stdin, and the
// only argv extension ("$@") comes from the fixed sqlFormatArgs table.
//
// The local socket is not trust-authenticated in the embedded chart's pod,
// and which env var holds which role's password differs between chart
// versions (plain vs _FILE, the app role vs the postgres superuser that
// PostgresRoleFixJob creates), so each known user/password pairing is probed
// with a no-op connection (-w: never prompt) and the first that works is
// used. A CNPG instance runs as the postgres OS user with peer auth, which
// the final, empty-password candidate covers. If none work, the real psql
// below fails with the server's own authentication error.
const sqlPsqlScript = `DB="${POSTGRES_DB:-authentik}"
rd() { if [ -n "$1" ] && [ -r "$1" ]; then cat "$1"; fi; }
try() { PGPASSWORD="$2" psql -X -w -U "$1" -d "$DB" -c '\q' >/dev/null 2>&1; }
for c in \
  "${POSTGRES_USER:-postgres}|${POSTGRES_PASSWORD:-}" \
  "${POSTGRES_USER:-postgres}|$(rd "${POSTGRES_PASSWORD_FILE:-}")" \
  "postgres|${POSTGRES_POSTGRES_PASSWORD:-}" \
  "postgres|$(rd "${POSTGRES_POSTGRES_PASSWORD_FILE:-}")" \
  "postgres|${POSTGRES_PASSWORD:-}" \
  "postgres|"; do
  if try "${c%%|*}" "${c#*|}"; then export PGUSER="${c%%|*}" PGPASSWORD="${c#*|}"; break; fi
done
PGOPTIONS='-c default_transaction_read_only=on -c statement_timeout=` + sqlStatementTimeout + `' ` +
	`exec psql -X -w -v ON_ERROR_STOP=1 -P pager=off -d "$DB" -f - "$@"`

var sqlFormatArgs = map[string][]string{
	sqlFormatAligned: nil,
	sqlFormatCSV:     {"--csv"},
}

type sqlRequest struct {
	Query string `json:"query"`
	// Format is "aligned" (psql's default table output) or "csv".
	Format string `json:"format,omitempty"`
}

type sqlResponse struct {
	// Target is the pod the query ran in, as <namespace>/<pod>.
	Target   string `json:"target"`
	Output   string `json:"output"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
}

// podExecRequest names a container to run a command in on some cluster of
// the merged kubeconfig. Exactly one of Pod and Selector is set; with a
// Selector the first Running pod matching it is used.
type podExecRequest struct {
	Kubeconfig []byte
	Context    string
	Namespace  string
	Pod        string
	Selector   string
	Container  string
	Command    []string
	Stdin      io.Reader
}

type podExecResult struct {
	Pod      string
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// podExecFunc is a Server field so tests can stand in for a real cluster.
type podExecFunc func(ctx context.Context, req podExecRequest) (podExecResult, error)

func (s *Server) runSQL(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Shell == nil {
		writeError(w, r, http.StatusNotFound, "SQL access needs the shell kubeconfig, which is not configured on this backend")
		return
	}
	var body sqlRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		writeError(w, r, http.StatusBadRequest, "query is required")
		return
	}
	if body.Format == "" {
		body.Format = sqlFormatAligned
	}
	formatArgs, ok := sqlFormatArgs[body.Format]
	if !ok {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown format %q, want %q or %q", body.Format, sqlFormatAligned, sqlFormatCSV))
		return
	}

	var topo benchv1alpha1.AuthentikDeployment
	key := types.NamespacedName{Name: chi.URLParam(r, "name"), Namespace: s.cfg.Namespace}
	if err := s.client.Get(r.Context(), key, &topo); err != nil {
		writeAPIError(w, r, err)
		return
	}
	req, err := sqlExecTarget(&topo)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	raw, kubeContext, ok := s.deploymentKubeContext(w, r, &topo)
	if !ok {
		return
	}
	req.Kubeconfig, req.Context = raw, kubeContext
	req.Command = append([]string{"sh", "-c", sqlPsqlScript, "psql"}, formatArgs...)
	req.Stdin = strings.NewReader(body.Query)

	ctx, cancel := context.WithTimeout(r.Context(), sqlExecTimeout)
	defer cancel()
	res, err := s.podExec(ctx, req)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, sqlResponse{
		Target:   req.Namespace + "/" + res.Pod,
		Output:   string(res.Stdout),
		Stderr:   string(res.Stderr),
		ExitCode: res.ExitCode,
	})
}

// deploymentKubeContext finds the merged-kubeconfig context for the cluster
// a deployment runs on, writing the error response itself when there is none.
func (s *Server) deploymentKubeContext(w http.ResponseWriter, r *http.Request, topo *benchv1alpha1.AuthentikDeployment) ([]byte, string, bool) {
	contexts, raw, err := s.loadShellContexts(r.Context())
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return nil, "", false
	}
	clusterName := topo.Spec.ClusterRef.Name
	if clusterName == "" {
		clusterName = ManagementClusterName
	}
	for _, c := range contexts {
		if c.Cluster == clusterName {
			return raw, c.Name, true
		}
	}
	writeError(w, r, http.StatusConflict, fmt.Sprintf("no kubeconfig context for cluster %q (is the WorkloadCluster Ready?)", clusterName))
	return nil, "", false
}

// clientsetFor builds a clientset for one context of a merged kubeconfig.
func clientsetFor(raw []byte, kubeContext string) (*kubernetes.Clientset, *rest.Config, error) {
	kubeconfig, err := clientcmd.Load(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	restCfg, err := clientcmd.NewNonInteractiveClientConfig(*kubeconfig, kubeContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("build client for context %q: %w", kubeContext, err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, err
	}
	return cs, restCfg, nil
}

// sqlExecTarget locates the Postgres container for each mode the operator
// itself provisions. External Postgres has no pod we know of.
func sqlExecTarget(t *benchv1alpha1.AuthentikDeployment) (podExecRequest, error) {
	req := podExecRequest{Namespace: t.Spec.Namespace}
	switch t.Spec.Postgres.Mode {
	case benchv1alpha1.PostgresEmbedded, "":
		req.Pod = render.EmbeddedPostgresStatefulSetName(t) + "-0"
		req.Container = "postgresql"
	case benchv1alpha1.PostgresCNPG:
		cluster := render.CNPGClusterName(t)
		if ref := t.Status.CNPGClusterRef; ref != nil && ref.Name != "" {
			cluster = ref.Name
		}
		req.Selector = "cnpg.io/cluster=" + cluster + ",cnpg.io/instanceRole=primary"
		req.Container = "postgres"
	default:
		return podExecRequest{}, fmt.Errorf("postgres mode %q has no in-cluster pod to query", t.Spec.Postgres.Mode)
	}
	return req, nil
}

// execInPod is the real podExecFunc: kubectl exec, websocket first with an
// SPDY fallback for API servers that predate it.
func execInPod(ctx context.Context, req podExecRequest) (podExecResult, error) {
	cs, restCfg, err := clientsetFor(req.Kubeconfig, req.Context)
	if err != nil {
		return podExecResult{}, err
	}

	pod := req.Pod
	if pod == "" {
		pods, err := cs.CoreV1().Pods(req.Namespace).List(ctx, metav1.ListOptions{LabelSelector: req.Selector})
		if err != nil {
			return podExecResult{}, fmt.Errorf("list pods %q: %w", req.Selector, err)
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning {
				pod = p.Name
				break
			}
		}
		if pod == "" {
			return podExecResult{}, fmt.Errorf("no running pod in %s matches %q", req.Namespace, req.Selector)
		}
	}

	execURL := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(req.Namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: req.Container,
			Command:   req.Command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec).URL()
	wsExec, err := remotecommand.NewWebSocketExecutor(restCfg, http.MethodGet, execURL.String())
	if err != nil {
		return podExecResult{}, err
	}
	spdyExec, err := remotecommand.NewSPDYExecutor(restCfg, http.MethodPost, execURL)
	if err != nil {
		return podExecResult{}, err
	}
	executor, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return podExecResult{}, err
	}

	stdout := &cappedBuffer{limit: sqlMaxOutputBytes}
	stderr := &cappedBuffer{limit: sqlMaxOutputBytes}
	res := podExecResult{Pod: pod}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: req.Stdin, Stdout: stdout, Stderr: stderr})
	res.Stdout, res.Stderr = stdout.Bytes(), stderr.Bytes()
	if exitErr, ok := errors.AsType[utilexec.ExitError](err); ok {
		// psql itself failed (bad SQL, ON_ERROR_STOP): its stderr says why,
		// which is the caller's answer rather than a gateway error.
		res.ExitCode = exitErr.ExitStatus()
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("exec in %s/%s: %w", req.Namespace, pod, err)
	}
	return res, nil
}

// cappedBuffer keeps the first limit bytes written and silently drops the
// rest, so a runaway SELECT can't exhaust the apiserver's memory.
type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func (b *cappedBuffer) Bytes() []byte {
	if b.truncated {
		return append(b.Buffer.Bytes(), []byte("\n[output truncated]\n")...)
	}
	return b.Buffer.Bytes()
}
