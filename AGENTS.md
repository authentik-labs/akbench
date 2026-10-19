# authentik-bench - AI Agent Guide

A Kubernetes operator plus web UI for running k6 load tests against authentik
deployments on Cluster API-provisioned workload clusters. See `docs/` for
what each piece does; this file covers how to work in the repo.

## Project Structure

```
cmd/main.go                    Operator manager entry (registers controllers)
cmd/apiserver/                 Web UI backend: REST API, Grafana proxy, kubectl shell, serves web/ui
cmd/appliance/                 Single-container appliance bootstrapper (k3s + Flux + CAPI + akbench)
api/v1alpha1/*_types.go        CRD schemas (+kubebuilder markers)
api/v1alpha1/zz_generated.*    Auto-generated (DO NOT EDIT)
internal/controller/           Reconcilers; turn CRDs into objects via internal/render
internal/render/               Pure functions building every object the operator applies
                               (HelmReleases, CAPI clusters, k6 Jobs, observability stack, ...)
internal/render/manifests/     Embedded assets, incl. the Grafana dashboards
internal/multicluster/         clusterRef -> client for a workload cluster (wraps CAPI's ClusterCache)
internal/api/                  HTTP handlers behind cmd/apiserver
internal/appliance/            Appliance bootstrap logic and its embedded stack template
internal/incus/, grafana/      Incus provider helpers; TestRun Grafana annotations
k6/scripts/*.ts                Built-in k6 scripts, embedded and seeded as TestCaseTemplates
web/ui/                        Vite + React SPA (TypeScript)
test/contract/                 k6 tag/env contract between render and k6 scripts
test/e2e/                      Kind smoke test: deploys the manager, checks metrics
config/crd/bases/*             Generated CRDs (DO NOT EDIT)
config/crd/third-party/        Vendored CAPI/Flux CRDs, loaded by envtest
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/samples/*               Example CRs (edit these)
charts/authentik-bench-operator/  Generated from config/default by `make helm-chart` (helmify)
appliance/                     Appliance entrypoint, pinned upstream assets
docs/                          Architecture, appliance, MCP, dashboards, development
hack/                          Dev scripts behind the `make` targets
```

## Critical Rules

### Never Edit These (Auto-Generated)
- `config/crd/bases/*.yaml` - from `make manifests`
- `config/rbac/role.yaml` - from `make manifests`
- `**/zz_generated.*.go` - from `make generate`
- `charts/authentik-bench-operator/templates/*` - from `make helm-chart`; change
  `config/` instead (CI regenerates the chart before publishing)
- `PROJECT` - from `kubebuilder [OPTIONS]`
- `appliance/assets.lock`, `appliance/clusterctl.yaml` - from `make appliance-assets`

### Never Remove Scaffold Markers
Do NOT delete `// +kubebuilder:scaffold:*` comments. The kubebuilder CLI
injects code at these markers.

### New APIs
Use `kubebuilder create api --group bench --version v1alpha1 --kind <Kind>` so
`PROJECT`, the samples and RBAC helper roles stay in sync. The project has no
webhooks; defaulting lives in `+kubebuilder:default` markers and the
controllers.

### E2E Tests Require an Isolated Kind Cluster
`make test-e2e` creates and deletes its own Kind cluster. Never point it at a
real cluster.

## After Making Changes

**After editing `*_types.go` or markers:**
```
make manifests generate
```

**After editing `*.go` files:**
```
make lint-fix
make test        # envtest + unit + contract tests
```

Always run the linter and get it to `0 issues` before considering a change
done - CI's Lint job fails on any issue, including ones that predate your
change. On macOS also run `GOOS=linux bin/golangci-lint run`: CI lints on
Linux, and `*_linux.go` files (e.g. `internal/api/shell_session_linux.go`)
aren't checked by a local `make lint` otherwise.

**After editing `web/ui`:**
```
make ui-lint-fix # oxfmt + oxlint --fix (@goauthentik/oxfmt-config, @goauthentik/oxlint-config)
make ui-test     # typecheck + vitest
```

`envtest` runs a real API server but none of the controllers being integrated
with (Flux, CAPI, CNPG, CSI). For changes to `internal/render` or
`internal/controller`, smoke-test against a real cluster where practical.

## Development

```bash
make dev          # CRDs + operator + cmd/apiserver against the current kubeconfig context
make ui-dev       # Vite dev server on :5173, proxying to the API on :8443
make dev-metrics  # minimal Prometheus for local metrics
make fixtures     # synthetic users + k6 credentials Secret
make help         # every target
```

## Code Conventions

### Controllers
- **Idempotent reconciliation**: safe to run any number of times.
- **Build objects in `internal/render`**, apply them from the controller
  (`internal/controller/apply.go`). Keep render functions pure and unit-tested.
- **Cross-cluster objects get no ownerReferences** (a remote cluster's GC
  can't see the owner's UID). Clean them up from the owner's finalizer.
- **Status** uses `metav1.Condition` (see `api/v1alpha1/conditions.go`).
- **RBAC** comes from `+kubebuilder:rbac` markers on the reconcilers. Run
  `make manifests` after changing them.
- **Watch secondary resources** with `.Owns()`/`.Watches()`, not just
  `RequeueAfter`.

### Logging
Follow the [Kubernetes message style](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines)
(enforced by the `logcheck` linter):
- Start with a capital letter, no trailing period
- Past tense, name the object type: `"Created Deployment"`, `"Failed to create Pod"`
- Balanced key-value pairs

```go
log := log.FromContext(ctx)
log.Info("Created HelmRelease", "name", hr.Name)
log.Error(err, "Failed to create Job", "name", name)
```

### Tests
Go tests use Ginkgo + Gomega for envtest suites (`suite_test.go`) and plain
`testing` elsewhere. The web UI uses vitest.

## References
- Kubebuilder Book: https://book.kubebuilder.io
- controller-runtime: https://github.com/kubernetes-sigs/controller-runtime
- Cluster API: https://cluster-api.sigs.k8s.io
- Flux helm-controller: https://fluxcd.io/flux/components/helm/
- Kubernetes API conventions: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
