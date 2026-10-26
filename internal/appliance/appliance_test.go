package appliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func testEnv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(testEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "default" || cfg.UIPort != "8443" || cfg.Version != Version {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigRejectsBadPort(t *testing.T) {
	if _, err := LoadConfig(testEnv(map[string]string{"AKBENCH_UI_PORT": "http"})); err == nil {
		t.Error("expected an error")
	}
}

func TestEnsureStateIsStable(t *testing.T) {
	cfg, err := LoadConfig(testEnv(map[string]string{"AKBENCH_STATE_DIR": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	first, err := EnsureState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.APIToken) != 64 {
		t.Fatalf("unexpected token %q", first.APIToken)
	}
	second, err := EnsureState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if *first != *second {
		t.Error("token changed between boots")
	}

	cfg.APIToken = "fixed"
	if st, err := EnsureState(cfg); err != nil || st.APIToken != "fixed" {
		t.Errorf("pinned token not used: %+v %v", st, err)
	}
}

// writeRepo lays out a clusterctl local repository the way
// hack/update-appliance-assets.sh does.
func writeRepo(t *testing.T, dirs ...string) string {
	t.Helper()
	repo := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestDiscoverShipped(t *testing.T) {
	repo := writeRepo(t,
		"cluster-api/v1.14.3", "bootstrap-kubeadm/v1.14.3", "control-plane-kubeadm/v1.14.3",
		"infrastructure-incus/v0.9.1", "infrastructure-aws/v2.9.0", "cert-manager/v1.21.2")
	shipped, err := DiscoverShipped(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := &Shipped{CAPIVersion: "v1.14.3", Infrastructure: map[string]string{"incus": "v0.9.1", "aws": "v2.9.0"}}
	if !reflect.DeepEqual(shipped, want) {
		t.Errorf("got %+v, want %+v", shipped, want)
	}

	for name, dirs := range map[string][]string{
		"two versions":     {"cluster-api/v1.14.3", "cluster-api/v1.14.2", "bootstrap-kubeadm/v1.14.3", "control-plane-kubeadm/v1.14.3"},
		"version mismatch": {"cluster-api/v1.14.3", "bootstrap-kubeadm/v1.14.2", "control-plane-kubeadm/v1.14.3"},
		"no core":          {"infrastructure-incus/v0.9.1"},
	} {
		if _, err := DiscoverShipped(writeRepo(t, dirs...)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestShippedDesired(t *testing.T) {
	shipped := &Shipped{CAPIVersion: "v1.14.3", Infrastructure: map[string]string{"incus": "v0.9.1"}}
	got, unknown := shipped.Desired([]string{"incus", "aws", "incus"})
	want := []Provider{
		{Type: "CoreProvider", Name: "cluster-api", Version: "v1.14.3"},
		{Type: "BootstrapProvider", Name: "kubeadm", Version: "v1.14.3"},
		{Type: "ControlPlaneProvider", Name: "kubeadm", Version: "v1.14.3"},
		{Type: "InfrastructureProvider", Name: "incus", Version: "v0.9.1"},
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(unknown, []string{"aws"}) {
		t.Errorf("got %v unknown %v", got, unknown)
	}
	if got, _ := shipped.Desired(nil); len(got) != 3 {
		t.Errorf("no infrastructure enabled: got %v, want only the core providers", got)
	}
}

func TestParseProviderList(t *testing.T) {
	if got := ParseProviderList(" incus, ,aws "); !reflect.DeepEqual(got, []string{"incus", "aws"}) {
		t.Errorf("got %v", got)
	}
	if got := ParseProviderList(""); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func renderDocs(t *testing.T, cfg *Config, st *State) map[string]*unstructured.Unstructured {
	t.Helper()
	out, err := RenderStack(cfg, st, cfg.Version)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]*unstructured.Unstructured{}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode rendered stack: %v\n%s", err, out)
		}
		if obj == nil {
			continue
		}
		u := &unstructured.Unstructured{Object: obj}
		docs[u.GetKind()+"/"+u.GetName()] = u
	}
	return docs
}

func TestRenderStack(t *testing.T) {
	cfg, err := LoadConfig(testEnv(map[string]string{
		"AKBENCH_VERSION":     "1.2.3",
		"AKBENCH_NAMESPACE":   "bench",
		"AKBENCH_UI_PORT":     "9000",
		"AKBENCH_OIDC_ISSUER": "https://id.example.com/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	docs := renderDocs(t, cfg, &State{APIToken: `to"k`})

	if v, _, _ := unstructured.NestedString(docs["Secret/apiserver-token"].Object, "stringData", "token"); v != `to"k` {
		t.Errorf("token = %q", v)
	}
	for key := range docs {
		if strings.HasPrefix(key, "Secret/") && key != "Secret/apiserver-token" {
			t.Errorf("unexpected %s - infrastructure credentials belong to the web UI", key)
		}
	}

	if tag, _, _ := unstructured.NestedString(docs["OCIRepository/authentik-bench-operator"].Object, "spec", "ref", "tag"); tag != "1.2.3" {
		t.Errorf("chart tag = %q, want 1.2.3", tag)
	}
	if tag, _, _ := unstructured.NestedString(docs["HelmRelease/authentik-bench-operator"].Object, "spec", "values", "controllerManager", "manager", "image", "tag"); tag != "1.2.3" {
		t.Errorf("operator image tag = %q, want 1.2.3", tag)
	}
	if ns, _, _ := unstructured.NestedString(docs["HelmRelease/authentik-bench-operator"].Object, "spec", "values", "controllerManager", "manager", "env", "benchNamespace"); ns != "bench" {
		t.Errorf("operator BENCH_NAMESPACE = %q, want bench", ns)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(docs["HelmRelease/authentik-bench-operator"].Object, "spec", "postRenderers"); found {
		t.Error("postRenderers set - kustomize rejects the chart's duplicate label keys")
	}

	containers, _, _ := unstructured.NestedSlice(docs["Deployment/apiserver"].Object, "spec", "template", "spec", "containers")
	c := containers[0].(map[string]any)
	if c["image"] != "ghcr.io/authentik-labs/akbench-apiserver:1.2.3" {
		t.Errorf("apiserver image = %v", c["image"])
	}
	env := map[string]any{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		env[m["name"].(string)] = m["value"]
	}
	if env["BENCH_NAMESPACE"] != "bench" || env["BENCH_OIDC_ISSUER"] != "https://id.example.com/" || env["BENCH_APPLIANCE"] != "true" {
		t.Errorf("apiserver env = %v", env)
	}
	if _, ok := env["BENCH_OIDC_CLIENT_ID"]; ok {
		t.Error("unset OIDC client ID rendered")
	}

	ports, _, _ := unstructured.NestedSlice(docs["Service/apiserver"].Object, "spec", "ports")
	if fmt.Sprint(ports[0].(map[string]any)["port"]) != "9000" {
		t.Errorf("service port = %v, want 9000", ports[0])
	}
}

func TestPlanClusterctl(t *testing.T) {
	shipped := &Shipped{CAPIVersion: "v1.14.3", Infrastructure: map[string]string{"incus": "v0.9.1"}}
	desired, _ := shipped.Desired([]string{"incus"})

	cmds, skipped, err := PlanClusterctl(nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{
		"init", "--wait-providers",
		"--core", "cluster-api:v1.14.3", "--bootstrap", "kubeadm:v1.14.3",
		"--control-plane", "kubeadm:v1.14.3", "--infrastructure", "incus:v0.9.1",
	}}
	if !reflect.DeepEqual(cmds, want) || len(skipped) != 0 {
		t.Errorf("fresh install: cmds %v skipped %v", cmds, skipped)
	}

	installed := []Provider{
		{Type: "CoreProvider", Name: "cluster-api", Version: "v1.13.0"},
		{Type: "BootstrapProvider", Name: "kubeadm", Version: "v1.14.3"},
		{Type: "ControlPlaneProvider", Name: "kubeadm", Version: "v1.15.0"},
	}
	cmds, skipped, err = PlanClusterctl(installed, desired)
	if err != nil {
		t.Fatal(err)
	}
	want = [][]string{
		{"init", "--wait-providers", "--infrastructure", "incus:v0.9.1"},
		{"upgrade", "apply", "--wait-providers", "--core", "cluster-api:v1.14.3"},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Errorf("mixed: cmds %v", cmds)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "v1.15.0") {
		t.Errorf("mixed: skipped %v", skipped)
	}
}

func TestParseInventory(t *testing.T) {
	got, err := ParseInventory([]byte(`{"items":[{"metadata":{"namespace":"capn-system"},"type":"InfrastructureProvider","providerName":"incus","version":"v0.9.1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Provider{{Type: "InfrastructureProvider", Name: "incus", Version: "v0.9.1"}}) {
		t.Errorf("got %v", got)
	}
}

// fakeCluster answers the kubectl/clusterctl calls the bootstrapper makes,
// recording them.
type fakeCluster struct {
	calls    []string
	enabled  string
	pin      string
	statuses []map[string]string
	stack    []byte
}

func (f *fakeCluster) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case strings.Contains(call, "get configmap "+ProvidersConfigMap):
		return []byte(f.enabled), nil
	case strings.Contains(call, "get configmap "+VersionConfigMap):
		return []byte(f.pin), nil
	case stdin != nil && bytes.Contains(stdin, []byte(StatusConfigMap)):
		var cm struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(stdin, &cm); err != nil {
			return nil, err
		}
		f.statuses = append(f.statuses, cm.Data)
	case stdin != nil:
		f.stack = stdin
	}
	return nil, nil
}

func newTestBootstrapper(t *testing.T, f *fakeCluster) *Bootstrapper {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "k3s.yaml")
	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(testEnv(map[string]string{"AKBENCH_KUBECONFIG": kubeconfig, "AKBENCH_ASSETS_DIR": "/assets"}))
	if err != nil {
		t.Fatal(err)
	}
	return &Bootstrapper{
		Config: cfg, State: &State{APIToken: "tok"}, Run: f.run, Log: io.Discard,
		Shipped: &Shipped{CAPIVersion: "v1.14.3", Infrastructure: map[string]string{"incus": "v0.9.1"}},
	}
}

func TestBootstrapRunOnceOrder(t *testing.T) {
	f := &fakeCluster{}
	if err := newTestBootstrapper(t, f).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range f.calls {
		switch {
		case strings.Contains(c, "/assets/flux/install.yaml"):
			order = append(order, "flux")
		case strings.HasPrefix(c, "clusterctl init"):
			order = append(order, "clusterctl")
			if strings.Contains(c, "--infrastructure") {
				t.Errorf("infrastructure provider installed without being enabled: %s", c)
			}
		case strings.HasSuffix(c, "-f -") && !strings.Contains(c, "status"):
			if len(order) == 0 || order[len(order)-1] != "stack" {
				order = append(order, "stack")
			}
		case strings.Contains(c, "wait helmrelease/authentik-bench-operator"):
			order = append(order, "wait-operator")
		}
	}
	if !reflect.DeepEqual(order, []string{"flux", "clusterctl", "stack", "wait-operator"}) {
		t.Errorf("order = %v (calls %v)", order, f.calls)
	}
	if !bytes.Contains(f.stack, []byte("kind: HelmRelease")) {
		t.Error("stack not passed on stdin")
	}
}

func TestSyncProvidersInstallsEnabledAndReportsUnknown(t *testing.T) {
	f := &fakeCluster{enabled: "incus,aws"}
	err := newTestBootstrapper(t, f).SyncProviders(context.Background())
	if err == nil || !strings.Contains(err.Error(), "aws") {
		t.Fatalf("expected an error about aws, got %v", err)
	}
	var initCall string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "clusterctl init") {
			initCall = c
		}
	}
	if !strings.Contains(initCall, "--infrastructure incus:v0.9.1") {
		t.Errorf("incus not installed: %q", initCall)
	}
	if len(f.statuses) != 1 {
		t.Fatalf("statuses = %v", f.statuses)
	}
	st := f.statuses[0]
	if !strings.Contains(st[StatusKeyError], "aws") || st[StatusKeyShipped] != `{"incus":"v0.9.1"}` || st[StatusKeySyncedAt] == "" {
		t.Errorf("status = %v", st)
	}
}

func TestServeStartsProviderLoopBeforeBootstrapFinishes(t *testing.T) {
	// The operator HelmRelease never becomes Ready (e.g. its chart can't be
	// pulled), yet a provider enabled in the UI afterwards still gets
	// installed - by the provider loop, well before the bootstrap's own 15s
	// retry could.
	f := &fakeCluster{}
	var mu sync.Mutex
	installed := make(chan struct{})
	b := newTestBootstrapper(t, f)
	b.SyncInterval = 10 * time.Millisecond
	b.Run = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		call := name + " " + strings.Join(args, " ")
		if strings.Contains(call, "wait helmrelease/authentik-bench-operator") {
			f.enabled = "incus"
			return nil, errors.New("timed out")
		}
		if strings.Contains(call, "--infrastructure incus:v0.9.1") {
			select {
			case <-installed:
			default:
				close(installed)
			}
		}
		return f.run(ctx, stdin, name, args...)
	}
	ctx := t.Context()
	go func() { _ = b.Serve(ctx) }()
	select {
	case <-installed:
	case <-time.After(5 * time.Second):
		t.Fatal("enabled provider not installed while the bootstrap was still failing")
	}
}

func TestEffectiveVersion(t *testing.T) {
	for _, tc := range []struct {
		pin  map[string]string
		want string
	}{
		{nil, "1.0.0"},
		{map[string]string{VersionKeyTarget: "1.1.0", VersionKeyBase: "1.0.0"}, "1.1.0"},
		// The appliance image was replaced since: its own version wins.
		{map[string]string{VersionKeyTarget: "1.1.0", VersionKeyBase: "0.9.0"}, "1.0.0"},
		{map[string]string{VersionKeyBase: "1.0.0"}, "1.0.0"},
	} {
		if got := EffectiveVersion("1.0.0", tc.pin); got != tc.want {
			t.Errorf("EffectiveVersion(%v) = %q, want %q", tc.pin, got, tc.want)
		}
	}
}

func TestSyncStackAppliesPinnedVersion(t *testing.T) {
	f := &fakeCluster{}
	b := newTestBootstrapper(t, f)
	ctx := context.Background()
	if err := b.syncStack(ctx, true); err != nil || f.stack != nil {
		t.Fatalf("applied before the bootstrap did: err=%v", err)
	}
	if err := b.syncStack(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.stack = nil
	if err := b.syncStack(ctx, true); err != nil || f.stack != nil {
		t.Fatalf("re-applied an unchanged version: err=%v", err)
	}
	f.pin = `{"version":"1.1.0","base":"dev"}`
	if err := b.syncStack(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(f.stack, []byte(`akbench-apiserver:1.1.0"`)) {
		t.Errorf("stack not applied at the pinned version:\n%s", f.stack)
	}
}
