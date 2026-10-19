package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"k8s.io/client-go/tools/clientcmd"
)

// testKubeconfig mirrors render.ShellKubeconfig's output shape: a
// synthesized management context plus one CAPI-style workload context.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: management
  cluster:
    server: https://kubernetes.default.svc
- name: wc1
  cluster:
    server: https://10.0.0.1:6443
users:
- name: management
  user:
    token: management-token
- name: wc1-admin
  user:
    token: wc1-token
contexts:
- name: management
  context:
    cluster: management
    user: management
- name: wc1-admin@wc1
  context:
    cluster: wc1
    user: wc1-admin
current-context: management
`

func shellServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newServer(t, Config{Shell: &ShellConfig{
		Kubeconfig: func(context.Context) ([]byte, error) { return []byte(testKubeconfig), nil },
		Shell:      "/bin/bash",
	}})
	return s
}

func mintTicket(t *testing.T, s *Server, contextName, namespace string) string {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/api/v1/shell/sessions",
		map[string]string{"context": contextName, "namespace": namespace})
	requireStatus(t, rec, http.StatusCreated)
	return decodeBody[struct {
		Ticket string `json:"ticket"`
	}](t, rec).Ticket
}

func TestShellContextsListsManagementFirst(t *testing.T) {
	s := shellServer(t)
	rec := do(t, s, http.MethodGet, "/api/v1/shell/contexts", nil)
	requireStatus(t, rec, http.StatusOK)
	got := decodeBody[struct {
		Items []shellContext `json:"items"`
	}](t, rec).Items
	want := []shellContext{{Name: "management", Cluster: "management"}, {Name: "wc1-admin@wc1", Cluster: "wc1"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("contexts = %+v, want %+v", got, want)
	}
}

func TestShellDisabledAnswers404(t *testing.T) {
	s, _ := newServer(t, Config{})
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/shell/contexts", nil), http.StatusNotFound)
}

func TestShellSessionRequiresBearerToken(t *testing.T) {
	s := shellServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shell/sessions", strings.NewReader(`{"context":"management"}`))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusUnauthorized)
}

func TestShellSessionRejectsUnknownContext(t *testing.T) {
	s := shellServer(t)
	for _, name := range []string{"wc1", "management; rm -rf /", "*", ""} {
		rec := do(t, s, http.MethodPost, "/api/v1/shell/sessions", map[string]string{"context": name})
		requireStatus(t, rec, http.StatusBadRequest)
	}
}

func TestShellSessionRejectsInvalidNamespace(t *testing.T) {
	s := shellServer(t)
	for _, ns := range []string{"Upper", "has space", "a/b", "-lead", strings.Repeat("a", 64)} {
		rec := do(t, s, http.MethodPost, "/api/v1/shell/sessions",
			map[string]string{"context": "management", "namespace": ns})
		requireStatus(t, rec, http.StatusBadRequest)
	}
}

func TestPrepareShellHomeSetsNamespace(t *testing.T) {
	for _, ns := range []string{"", "ak-1"} {
		dir, err := prepareShellHome([]byte(testKubeconfig), "wc1-admin@wc1", ns)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := clientcmd.LoadFromFile(filepath.Join(dir, "kubeconfig"))
		_ = os.RemoveAll(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.CurrentContext != "wc1-admin@wc1" {
			t.Errorf("current-context = %q, want wc1-admin@wc1", cfg.CurrentContext)
		}
		if got := cfg.Contexts["wc1-admin@wc1"].Namespace; got != ns {
			t.Errorf("namespace = %q, want %q", got, ns)
		}
		// Only the selected context is touched.
		if got := cfg.Contexts["management"].Namespace; got != "" {
			t.Errorf("management namespace = %q, want empty", got)
		}
	}
}

func TestShellConnectRejectsMissingOrBogusTicket(t *testing.T) {
	s := shellServer(t)
	for _, target := range []string{"/api/v1/shell/connect", "/api/v1/shell/connect?ticket=nope"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		// Even a valid bearer token is not a substitute for a ticket.
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		requireStatus(t, rec, http.StatusUnauthorized)
	}
}

func TestShellTicketIsSingleUseAndExpires(t *testing.T) {
	st := newShellState()
	now := time.Now()

	ticket, err := st.mint("management", "", "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := st.redeem(ticket, now); !ok || got.context != "management" || got.user != "alice" {
		t.Fatalf("first redeem = %+v, %v; want management/alice, true", got, ok)
	}
	if _, ok := st.redeem(ticket, now); ok {
		t.Fatal("ticket redeemed twice")
	}

	expired, _ := st.mint("management", "", "alice", now)
	if _, ok := st.redeem(expired, now.Add(shellTicketTTL+time.Second)); ok {
		t.Fatal("expired ticket accepted")
	}
	// An expired ticket is still consumed.
	if _, ok := st.redeem(expired, now); ok {
		t.Fatal("expired ticket survived a failed redeem")
	}
}

// TestShellSessionEndToEnd drives a real shell over a real WebSocket: the
// chosen context is selected, the backend's own environment does not leak
// in, and closing the socket kills the shell and its background jobs.
func TestShellSessionEndToEnd(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}
	t.Setenv("BENCH_API_TOKEN", "must-not-leak")
	s := shellServer(t)
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)

	ticket := mintTicket(t, s, "wc1-admin@wc1", "ak-1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/shell/connect?ticket=" + ticket
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// The ticket is spent now.
	if _, resp, err := websocket.Dial(ctx, url, nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second dial with the same ticket: err=%v resp=%v, want 401", err, resp)
	}

	send := func(msg shellMessage) {
		raw, _ := json.Marshal(msg)
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	send(shellMessage{Type: "resize", Cols: 120, Rows: 40})
	send(shellMessage{Type: "data", Data: "sleep 300 & echo BG=$!; echo PID=$$; " +
		"echo CTX=$(sed -n 's/^current-context: //p' \"$KUBECONFIG\"); echo PROMPT=$(__bench_ctx | tr ' ' _); " +
		"echo EDITOR=$EDITOR; echo LEAK=${BENCH_API_TOKEN:-none}; " +
		"echo SIZE=$(stty size); echo DONE\n"})

	var out bytes.Buffer
	done := regexp.MustCompile(`(?m)^DONE\r?$`)
	for !done.Match(out.Bytes()) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (output so far: %q)", err, out.String())
		}
		out.Write(data)
	}
	output := out.String()
	field := func(name string) string {
		m := regexp.MustCompile(`(?m)(?:^|\s)` + name + `=(\S*)\r?$`).FindStringSubmatch(output)
		if m == nil {
			t.Fatalf("no %s= line in %q", name, output)
		}
		return m[1]
	}
	if got := field("CTX"); got != "wc1-admin@wc1" {
		t.Errorf("current-context = %q, want wc1-admin@wc1", got)
	}
	if got := field("PROMPT"); got != "wc1-admin@wc1_(ak-1)" {
		t.Errorf("prompt context = %q, want wc1-admin@wc1_(ak-1)", got)
	}
	if got := field("EDITOR"); got != "nano" {
		t.Errorf("EDITOR = %q, want nano", got)
	}
	if got := field("LEAK"); got != "none" {
		t.Errorf("backend environment leaked into the shell: BENCH_API_TOKEN=%q", got)
	}
	if !strings.Contains(output, "SIZE=40 120") {
		t.Errorf("resize not applied, output %q", output)
	}
	shellPID, _ := strconv.Atoi(field("PID"))
	bgPID, _ := strconv.Atoi(field("BG"))

	_ = conn.Close(websocket.StatusNormalClosure, "")

	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range []int{shellPID, bgPID} {
		for processAlive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("pid %d still running after the socket closed", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// processAlive treats a zombie as dead: it has exited and only awaits
// reaping by whichever process it was reparented to.
func processAlive(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	stat := string(raw)
	fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])
	return len(fields) > 0 && fields[0] != "Z"
}
