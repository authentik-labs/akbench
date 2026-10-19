package api

// The in-browser kubectl shell: a real interactive shell on a PTY, with
// KUBECONFIG pointed at a per-session copy of the merged kubeconfig
// ShellKubeconfigReconciler maintains (the management cluster plus every
// Ready WorkloadCluster), bridged to xterm.js over a WebSocket.
//
// Auth: a browser's WebSocket API cannot set an Authorization header, so the
// handshake carries a ticket instead of the bearer token:
//
//  1. POST /api/v1/shell/sessions {"context": "<name>", "namespace": "<ns>"}
//     - an ordinary bearer-authenticated request - validates the context
//     (and the optional namespace) and mints a random ticket bound to them. Tickets are single-use and expire after
//     shellTicketTTL.
//  2. GET /api/v1/shell/connect?ticket=<ticket> upgrades to a WebSocket.
//     The ticket is redeemed (and so invalidated) before anything else
//     happens; no process is spawned for a request without a valid one.
//
// This keeps the long-lived bearer token out of URLs, logs and browser
// history - only a seconds-lived, already-spent ticket ever appears there.
//
// Wire protocol, client -> server: text frames carrying JSON,
// {"type":"data","data":"<keystrokes>"} or
// {"type":"resize","cols":80,"rows":24}. Server -> client: binary frames of
// raw PTY output, which xterm.js decodes itself (so a multi-byte character
// split across two reads is never mangled by a premature UTF-8 decode).
//
// Lifecycle: one shell per WebSocket. Whichever side ends first (the shell
// exits, the socket closes, or a keepalive ping goes unanswered because the
// tab vanished without a close frame) tears down the whole session: the
// shell's entire process session is signalled (killShellSession), the
// process reaped and the per-session directory removed.

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/clientcmd"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// shellTicketTTL bounds how long a minted ticket can wait to be
	// redeemed - the SPA opens the socket immediately after minting.
	shellTicketTTL = 10 * time.Second
	// shellMaxSessions caps concurrent shells, so a misbehaving client
	// can't exhaust the pod's PIDs/memory.
	shellMaxSessions = 16
	// shellPingInterval detects a peer that vanished without a close frame
	// (laptop lid, network drop), which would otherwise leak its shell.
	shellPingInterval = 30 * time.Second
	// shellKillGrace is how long the shell gets to exit after SIGHUP before
	// everything left in its session is SIGKILLed.
	shellKillGrace = 2 * time.Second
	// shellReadLimit allows large pastes, which xterm.js delivers as one
	// data message.
	shellReadLimit = 1 << 20
)

// ShellConfig enables the kubectl shell. A nil Config.Shell disables it -
// the routes then answer 404.
type ShellConfig struct {
	// Kubeconfig returns the current merged kubeconfig. Called on every
	// contexts listing and session start, so a WorkloadCluster that became
	// Ready a moment ago is immediately usable.
	Kubeconfig func(ctx context.Context) ([]byte, error)
	// Shell is the shell binary to run, e.g. /bin/bash. Spawned directly -
	// never through "sh -c" - with no arguments derived from the request.
	Shell string
	// OriginPatterns are extra host patterns allowed to open the WebSocket
	// cross-origin (see websocket.AcceptOptions); same-origin is always
	// allowed.
	OriginPatterns []string
}

// shellContext is one kubeconfig context the shell can start in.
type shellContext struct {
	Name string `json:"name"`
	// Cluster is the context's cluster entry - for a WorkloadCluster that
	// is the WorkloadCluster's own name (CAPI's kubeconfig names it so),
	// which is how the UI maps a cluster page onto its context.
	Cluster string `json:"cluster"`
}

type shellTicket struct {
	context string
	// namespace, if set, becomes the session context's default namespace.
	namespace string
	// user is who minted the ticket (principal), for the session's log lines.
	user    string
	expires time.Time
}

// shellState is the mutable part of the shell feature, shared by every
// request.
type shellState struct {
	mu       sync.Mutex
	tickets  map[string]shellTicket
	sessions int
}

func newShellState() *shellState {
	return &shellState{tickets: map[string]shellTicket{}}
}

func (st *shellState) mint(contextName, namespace, user string, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	st.mu.Lock()
	defer st.mu.Unlock()
	for t, v := range st.tickets {
		if now.After(v.expires) {
			delete(st.tickets, t)
		}
	}
	st.tickets[ticket] = shellTicket{
		context:   contextName,
		namespace: namespace,
		user:      user,
		expires:   now.Add(shellTicketTTL),
	}
	return ticket, nil
}

// redeem consumes a ticket. It is deleted whether or not it was still
// valid, so a ticket can never be used twice.
func (st *shellState) redeem(ticket string, now time.Time) (shellTicket, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	v, ok := st.tickets[ticket]
	delete(st.tickets, ticket)
	if !ok || now.After(v.expires) {
		return shellTicket{}, false
	}
	return v, true
}

func (st *shellState) acquire() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessions >= shellMaxSessions {
		return false
	}
	st.sessions++
	return true
}

func (st *shellState) release() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions--
}

// shellEnabled guards every shell route.
func (s *Server) shellEnabled(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Shell == nil {
		writeError(w, r, http.StatusNotFound, "the kubectl shell is not configured on this backend")
		return false
	}
	return true
}

func (s *Server) loadShellContexts(ctx context.Context) ([]shellContext, []byte, error) {
	raw, err := s.cfg.Shell.Kubeconfig(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	contexts := make([]shellContext, 0, len(cfg.Contexts))
	for name, c := range cfg.Contexts {
		contexts = append(contexts, shellContext{Name: name, Cluster: c.Cluster})
	}
	slices.SortFunc(contexts, func(a, b shellContext) int {
		// The management cluster first, then alphabetical.
		switch {
		case a.Name == ManagementClusterName:
			return -1
		case b.Name == ManagementClusterName:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return contexts, raw, nil
}

func hasContext(contexts []shellContext, name string) bool {
	return slices.ContainsFunc(contexts, func(c shellContext) bool { return c.Name == name })
}

func (s *Server) listShellContexts(w http.ResponseWriter, r *http.Request) {
	if !s.shellEnabled(w, r) {
		return
	}
	contexts, _, err := s.loadShellContexts(r.Context())
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeItems(w, r, contexts)
}

func (s *Server) createShellSession(w http.ResponseWriter, r *http.Request) {
	if !s.shellEnabled(w, r) {
		return
	}
	var body struct {
		Context string `json:"context"`
		// Namespace optionally starts the session in that namespace, the
		// way `kubectl config set-context --current --namespace` would.
		Namespace string `json:"namespace"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	contexts, _, err := s.loadShellContexts(r.Context())
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	// Exact match against the kubeconfig's own context names - nothing
	// else ever reaches the session's kubeconfig.
	if !hasContext(contexts, body.Context) {
		writeError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown kubeconfig context %q", body.Context))
		return
	}
	// Only a well-formed name: it ends up in the session's kubeconfig. It
	// need not exist - kubectl reports that itself, on the first command.
	if body.Namespace != "" {
		if errs := validation.IsDNS1123Label(body.Namespace); len(errs) > 0 {
			writeError(w, r, http.StatusBadRequest,
				fmt.Sprintf("invalid namespace %q: %s", body.Namespace, strings.Join(errs, "; ")))
			return
		}
	}
	now := time.Now()
	ticket, err := s.shell.mint(body.Context, body.Namespace, principal(r.Context()), now)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{
		"ticket":     ticket,
		expiresAtKey: now.Add(shellTicketTTL).UTC().Format(time.RFC3339),
	})
}

// shellMessage is a client -> server control/data message.
type shellMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// connectShell is deliberately registered outside requireToken: the ticket
// is its credential, and it is checked before the upgrade and before any
// process exists.
func (s *Server) connectShell(w http.ResponseWriter, r *http.Request) {
	if !s.shellEnabled(w, r) {
		return
	}
	log := logf.FromContext(r.Context()).WithName("shell")

	redeemed, ok := s.shell.redeem(r.URL.Query().Get("ticket"), time.Now())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "missing, expired or already used shell ticket")
		return
	}
	contextName := redeemed.context
	contexts, raw, err := s.loadShellContexts(r.Context())
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	// Re-checked: the context may have vanished since the ticket was minted.
	if !hasContext(contexts, contextName) {
		writeError(w, r, http.StatusConflict, fmt.Sprintf("kubeconfig context %q no longer exists", contextName))
		return
	}
	if !s.shell.acquire() {
		writeError(w, r, http.StatusTooManyRequests, "too many concurrent shell sessions")
		return
	}
	defer s.shell.release()

	dir, err := prepareShellHome(raw, contextName, redeemed.namespace)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.cfg.Shell.OriginPatterns})
	if err != nil {
		// Accept has already written the HTTP error response.
		log.Info("Could not upgrade shell connection", "error", err.Error())
		return
	}
	conn.SetReadLimit(shellReadLimit)
	defer func() { _ = conn.CloseNow() }()

	cmd := exec.Command(s.cfg.Shell.Shell)
	cmd.Dir = dir
	cmd.Env = shellEnv(dir)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		log.Error(err, "Could not start shell")
		_ = conn.Close(websocket.StatusInternalError, "could not start shell")
		return
	}
	log.Info("Started shell session",
		"user", redeemed.user, "context", contextName, "namespace", redeemed.namespace, "pid", cmd.Process.Pid)

	// Not the request context: that one is cancelled the moment the
	// handler returns, and the teardown below has to outlive it.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); defer cancel(); pumpPTYToSocket(ctx, ptmx, conn) }()
	go func() { defer wg.Done(); defer cancel(); pumpSocketToPTY(ctx, conn, ptmx) }()
	go func() { defer wg.Done(); defer cancel(); keepAlive(ctx, conn) }()

	select {
	case <-ctx.Done():
	case <-exited:
		cancel()
	}

	killShellSession(cmd.Process.Pid, exited, shellKillGrace)
	_ = ptmx.Close()
	_ = conn.Close(websocket.StatusNormalClosure, "shell exited")
	wg.Wait()
	log.Info("Ended shell session", "user", redeemed.user, "context", contextName, "pid", cmd.Process.Pid)
}

func pumpPTYToSocket(ctx context.Context, ptmx io.Reader, conn *websocket.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func pumpSocketToPTY(ctx context.Context, conn *websocket.Conn, ptmx *os.File) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg shellMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "data":
			if _, err := io.WriteString(ptmx, msg.Data); err != nil {
				return
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 && msg.Cols <= 1000 && msg.Rows <= 1000 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: msg.Cols, Rows: msg.Rows})
			}
		}
	}
}

func keepAlive(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(shellPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, shellPingInterval/2)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// killShellSession ends everything the shell started. pty.Start runs the
// shell with Setsid, so its PID is also its process group and session ID:
// SIGHUP the group first (what a closing terminal sends), then SIGKILL
// whatever is left in the session - including background jobs the shell
// put in process groups of their own - once the shell has exited or the
// grace period is over.
func killShellSession(pid int, exited <-chan struct{}, grace time.Duration) {
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	select {
	case <-exited:
	case <-time.After(grace):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	killSessionMembers(pid)
	<-exited
}

// prepareShellHome creates the session's private HOME: its own copy of the
// kubeconfig with the chosen context selected (so `kubectl config
// use-context` only ever affects this one session) and, if one was asked
// for, namespace as that context's default, and a shell rc file.
func prepareShellHome(rawKubeconfig []byte, contextName, namespace string) (string, error) {
	dir, err := os.MkdirTemp("", "bench-shell-")
	if err != nil {
		return "", err
	}
	cfg, err := clientcmd.Load(rawKubeconfig)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	cfg.CurrentContext = contextName
	if c, ok := cfg.Contexts[contextName]; ok && namespace != "" {
		c.Namespace = namespace
	}
	if err := clientcmd.WriteToFile(*cfg, filepath.Join(dir, "kubeconfig")); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, ".bashrc"), []byte(shellRC), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// shellEnv is the shell's entire environment - deliberately built from
// scratch rather than inherited, so nothing of the backend's own (above all
// BENCH_API_TOKEN) leaks into a session.
func shellEnv(home string) []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
		"KUBECONFIG=" + filepath.Join(home, "kubeconfig"),
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		// nano over vi: `kubectl edit` shouldn't trap anyone who has never
		// quit vim. VISUAL too, since some tools prefer it.
		"EDITOR=nano",
		"VISUAL=nano",
	}
}

// shellRC is the session's .bashrc: prompt, kubectl aliases and completion.
//
//go:embed shell/bashrc
var shellRC string
