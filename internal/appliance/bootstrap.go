package appliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const fieldManager = "akbench-appliance"

// Runner executes an external command (kubectl/clusterctl), feeding it stdin
// when non-nil, and returns its combined output. Swapped out in tests.
type Runner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// ExecRunner runs commands for real, with KUBECONFIG pointed at k3s's admin
// kubeconfig and clusterctl's GitHub version check disabled (every provider
// it installs comes from the image's local repository).
func ExecRunner(kubeconfig string) Runner {
	return func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig, "CLUSTERCTL_DISABLE_VERSIONCHECK=true")
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
}

// Bootstrapper installs everything on top of k3s. Every step is idempotent,
// so the whole sequence runs again on each container start - which is also
// how an image upgrade rolls out (newer providers, newer chart). After that
// it keeps syncing the infrastructure providers enabled in the web UI.
type Bootstrapper struct {
	Config  *Config
	State   *State
	Shipped *Shipped
	Run     Runner
	Log     io.Writer
	// SyncInterval is how often the provider loop re-reads the providers
	// the web UI enabled.
	SyncInterval time.Duration

	// syncMu serialises SyncProviders - the bootstrap and the provider loop
	// must never run clusterctl at the same time.
	syncMu sync.Mutex
	// coreReady is closed once Cluster API itself is installed, which is
	// when the provider loop starts - independently of the rest of the
	// bootstrap, so enabling a provider in the UI works even while e.g. the
	// operator chart can't be pulled.
	coreReady     chan struct{}
	coreReadyOnce sync.Once

	// stackMu guards appliedVersion, the akbench version the stack was last
	// applied at; empty until the bootstrap first applied it.
	stackMu        sync.Mutex
	appliedVersion string
}

// init prepares the zero-value fields. Called by Serve before it starts the
// provider loop, and by RunOnce for callers (tests) that use it directly -
// both from the same goroutine.
func (b *Bootstrapper) init() {
	if b.coreReady == nil {
		b.coreReady = make(chan struct{})
	}
}

func (b *Bootstrapper) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(b.Log, "[akbench] "+format+"\n", args...)
}

// Serve bootstraps (retrying until it succeeds) and, from the moment Cluster
// API itself is installed, syncs infrastructure providers every
// SyncInterval - both until ctx ends.
func (b *Bootstrapper) Serve(ctx context.Context) error {
	b.init()
	go b.providerLoop(ctx)
	for attempt := 1; ; attempt++ {
		err := b.RunOnce(ctx)
		if err == nil {
			break
		}
		b.logf("Bootstrap attempt %d failed, retrying in 15s: %v", attempt, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	b.PrintBanner()
	<-ctx.Done()
	return ctx.Err()
}

func (b *Bootstrapper) providerLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-b.coreReady:
	}
	lastErr := ""
	for {
		msg := ""
		if err := b.SyncProviders(ctx); err != nil {
			msg = err.Error()
		} else if err := b.syncStack(ctx, true); err != nil {
			msg = "update akbench: " + err.Error()
		}
		if msg != lastErr {
			if msg != "" {
				b.logf("Sync failed: %s", msg)
			} else if lastErr != "" {
				b.logf("Infrastructure providers and akbench are in sync again")
			}
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.SyncInterval):
		}
	}
}

// RunOnce performs one pass of the bootstrap sequence.
func (b *Bootstrapper) RunOnce(ctx context.Context) error {
	if err := b.waitForAPI(ctx); err != nil {
		return err
	}

	b.logf("Applying Flux")
	if _, err := b.Run(ctx, nil, "kubectl", "apply", "--server-side", "--force-conflicts",
		"--field-manager="+fieldManager, "-f", filepath.Join(b.Config.AssetsDir, "flux", "install.yaml")); err != nil {
		return err
	}
	if _, err := b.Run(ctx, nil, "kubectl", "wait", "--for=condition=Established", "--timeout=120s",
		"crd/helmreleases.helm.toolkit.fluxcd.io", "crd/ocirepositories.source.toolkit.fluxcd.io"); err != nil {
		return err
	}

	// The operator watches Cluster API types and won't start without their
	// CRDs, so the core providers go in before it does. Infrastructure
	// providers already enabled (e.g. on a restart) come along in the same
	// pass; newly enabled ones are the provider loop's job.
	if err := b.SyncProviders(ctx); err != nil {
		return err
	}
	b.init()
	b.coreReadyOnce.Do(func() { close(b.coreReady) })

	if err := b.syncStack(ctx, false); err != nil {
		return err
	}

	b.logf("Waiting for the operator and the web UI to roll out")
	if _, err := b.Run(ctx, nil, "kubectl", "-n", "flux-system", "wait", "helmrelease/authentik-bench-operator",
		"--for=condition=Ready", "--timeout=10m"); err != nil {
		return b.explainHelmRelease(ctx, err)
	}
	if _, err := b.Run(ctx, nil, "kubectl", "-n", SystemNamespace, "rollout", "status",
		"deployment/apiserver", "--timeout=10m"); err != nil {
		return err
	}
	return nil
}

// syncStack applies the akbench stack at EffectiveVersion. With onlyChanged
// (the sync loop) it does nothing until the bootstrap applied the stack once,
// and afterwards only when the web UI pinned a different version.
func (b *Bootstrapper) syncStack(ctx context.Context, onlyChanged bool) error {
	b.stackMu.Lock()
	defer b.stackMu.Unlock()
	if onlyChanged && b.appliedVersion == "" {
		return nil
	}
	raw, err := b.Run(ctx, nil, "kubectl", "-n", SystemNamespace, "get", "configmap", VersionConfigMap,
		"--ignore-not-found", "-o", "jsonpath={.data}")
	if err != nil {
		return err
	}
	pin := map[string]string{}
	if s := strings.TrimSpace(string(raw)); s != "" {
		if err := json.Unmarshal([]byte(s), &pin); err != nil {
			return fmt.Errorf("parse %s: %w", VersionConfigMap, err)
		}
	}
	version := EffectiveVersion(b.Config.Version, pin)
	if onlyChanged && version == b.appliedVersion {
		return nil
	}
	b.logf("Applying akbench %s", version)
	stack, err := RenderStack(b.Config, b.State, version)
	if err != nil {
		return fmt.Errorf("render stack: %w", err)
	}
	if _, err := b.Run(ctx, stack, "kubectl", "apply", "--server-side", "--force-conflicts",
		"--field-manager="+fieldManager, "-f", "-"); err != nil {
		return err
	}
	b.appliedVersion = version
	return nil
}

// explainHelmRelease replaces a bare `kubectl wait` timeout with the
// HelmRelease's own Ready message (e.g. the chart registry refusing an
// anonymous pull), which is what the user actually needs to see.
func (b *Bootstrapper) explainHelmRelease(ctx context.Context, waitErr error) error {
	msg, err := b.Run(ctx, nil, "kubectl", "-n", "flux-system", "get", "helmrelease/authentik-bench-operator",
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].message}`)
	if err != nil || strings.TrimSpace(string(msg)) == "" {
		return waitErr
	}
	return fmt.Errorf("operator HelmRelease is not ready: %s", strings.TrimSpace(string(msg)))
}

func (b *Bootstrapper) waitForAPI(ctx context.Context) error {
	b.logf("Waiting for the k3s API server")
	for {
		if _, err := os.Stat(b.Config.Kubeconfig); err == nil {
			if _, err := b.Run(ctx, nil, "kubectl", "get", "--raw=/readyz"); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// SyncProviders installs/upgrades the core Cluster API providers plus every
// infrastructure provider enabled in the web UI, and reports the outcome in
// StatusConfigMap. Providers are never removed: disabling one in the UI
// would otherwise orphan every cluster it provisioned.
func (b *Bootstrapper) SyncProviders(ctx context.Context) error {
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	enabled, err := b.enabledProviders(ctx)
	if err == nil {
		err = b.ensureProviders(ctx, enabled)
	}
	b.writeStatus(ctx, err)
	return err
}

func (b *Bootstrapper) enabledProviders(ctx context.Context) ([]string, error) {
	out, err := b.Run(ctx, nil, "kubectl", "-n", SystemNamespace, "get", "configmap", ProvidersConfigMap,
		"--ignore-not-found", "-o", "jsonpath={.data."+ProvidersKeyInfrastructure+"}")
	if err != nil {
		return nil, err
	}
	return ParseProviderList(string(out)), nil
}

func (b *Bootstrapper) ensureProviders(ctx context.Context, enabled []string) error {
	desired, unknown := b.Shipped.Desired(enabled)
	var installed []Provider
	crd, err := b.Run(ctx, nil, "kubectl", "get", "crd", "providers.clusterctl.cluster.x-k8s.io", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(crd)) != "" {
		raw, err := b.Run(ctx, nil, "kubectl", "get", "providers.clusterctl.cluster.x-k8s.io", "-A", "-o", "json")
		if err != nil {
			return err
		}
		if installed, err = ParseInventory(raw); err != nil {
			return fmt.Errorf("parse clusterctl inventory: %w", err)
		}
	}
	cmds, skipped, err := PlanClusterctl(installed, desired)
	if err != nil {
		return err
	}
	for _, s := range skipped {
		b.logf("Not downgrading Cluster API provider: %s", s)
	}
	global := []string{"--config", filepath.Join(b.Config.AssetsDir, "clusterctl", "clusterctl.yaml"), "--kubeconfig", b.Config.Kubeconfig}
	for _, args := range cmds {
		b.logf("Running clusterctl %s", strings.Join(args, " "))
		if _, err := b.Run(ctx, nil, "clusterctl", append(args, global...)...); err != nil {
			return err
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("this appliance image doesn't ship the infrastructure provider(s) %s (available: %s)",
			strings.Join(unknown, ", "), strings.Join(slices.Sorted(maps.Keys(b.Shipped.Infrastructure)), ", "))
	}
	return nil
}

// writeStatus reports a provider sync's outcome for the web UI. Best-effort:
// before the akbench stack has created SystemNamespace (the very first
// sync), there is nowhere to write it yet, and nothing reads it either.
func (b *Bootstrapper) writeStatus(ctx context.Context, syncErr error) {
	shipped, _ := json.Marshal(b.Shipped.Infrastructure)
	msg := ""
	if syncErr != nil {
		msg = syncErr.Error()
	}
	cm, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": StatusConfigMap, "namespace": SystemNamespace},
		"data": map[string]string{
			StatusKeyShipped:  string(shipped),
			StatusKeyError:    msg,
			StatusKeySyncedAt: time.Now().UTC().Format(time.RFC3339),
		},
	})
	if _, err := b.Run(ctx, cm, "kubectl", "apply", "--server-side", "--force-conflicts",
		"--field-manager="+fieldManager, "-f", "-"); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if !strings.Contains(err.Error(), "NotFound") {
			b.logf("Could not write provider status: %v", err)
		}
	}
}

// PrintBanner reports where the UI is and how to log in.
func (b *Bootstrapper) PrintBanner() {
	b.logf("akbench %s is ready", b.Config.Version)
	b.logf("  Web UI:    https://<host>:%s/ (self-signed certificate)", b.Config.UIPort)
	b.logf("  API token: docker exec <container> akbench-appliance token")
	b.logf("  Next:      enable an infrastructure provider and add its credentials under Infrastructure in the web UI")
}
