# Architecture

What each component does.

- **CRDs** (`bench.goauthentik.io/v1alpha1`): `AuthentikDeployment`, `TestCase`,
  `TestCaseTemplate`, `TestRun`, `WorkloadCluster`.
- **`WorkloadClusterReconciler`**: renders a full Cluster API cluster stack
  (`Cluster`/`LXCCluster`/`KubeadmControlPlane`/`MachineDeployment` plus their
  `LXCMachineTemplate`s and a worker `KubeadmConfigTemplate`) against the
  Incus infrastructure provider already installed on the management cluster,
  and mirrors the resulting `Cluster`'s readiness (and its
  `<name>-kubeconfig` Secret) back onto its own status. Control-plane size
  and Kubernetes version are create-once; a worker pool's own `replicas`/
  flavor are a supported day-2 edit, applied in place
  (`applyMachineDeploymentPool`). A pool can also opt into
  `spec.autoscaling{minReplicas,maxReplicas}`, which installs
  [cluster-autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/clusterapi)
  for it (one instance per `WorkloadCluster` that uses it, torn down again
  once no pool does) - `.spec.replicas` stops being reconciled for an
  autoscaled pool once set, so it never fights cluster-autoscaler's own
  scale-up/down.
  It also installs, once the control plane is reachable: CNI (Flannel),
  metrics-server, csi-incus (the cluster's only `StorageClass`, backed by a
  real Incus storage pool), and - in the `bench-system` namespace -
  kube-prometheus-stack (Prometheus with the remote-write receiver k6 pushes
  to, node-exporter, kube-state-metrics), Grafana (anonymous-admin, reachable
  through `cmd/apiserver`'s own reverse proxy - see below), Pyroscope,
  Tempo, and Loki with an Alloy DaemonSet shipping every pod's logs to it.
- **`clusterRef`/`runnerClusterRef`**: `AuthentikDeploymentReconciler`,
  `TestCaseReconciler`, and `TestRunReconciler` all resolve these to a live
  client via `internal/multicluster.Tracker` (wrapping Cluster API's
  `ClusterCache`). An AuthentikDeployment must set `clusterRef`: the
  management cluster is rejected at admission.
  Cross-cluster objects (namespace, CNPG `Cluster`, k6 workloads, generated
  Secrets) skip ownerReferences - a remote cluster's garbage collector can't
  recognize a UID from a different cluster's etcd - and clean up through each
  controller's finalizer instead.
- **`AuthentikDeploymentReconciler`**: renders a Flux `HelmRelease` +
  `HelmRepository` installing the official authentik chart
  (`charts.goauthentik.io`) with the deployment's replica/resource/placement
  values. All three Postgres modes work: `Embedded` (bundled subchart, with a
  generated password pinned on both sides so they agree), `CNPG` (renders a
  `postgresql.cnpg.io/v1 Cluster`, waits for it healthy, reads its generated
  app secret), `External` (reads a user-supplied secret directly).
  `Embedded` and `CNPG` take a `maxStorageSize` (default `12Gi`): the reconciler reads
  the postgres volumes' usage from the kubelet (`stats/summary` through the
  workload cluster's node proxy) every minute, and once a volume is 80% full
  grows it by half, capped at `maxStorageSize`. Embedded patches the
  StatefulSet's PVC (its `volumeClaimTemplates` are immutable); CNPG gets the
  new size through `spec.storage.size`, which `status.postgresStorage.size`
  keeps from ever shrinking back to spec's `storageSize`. The StorageClass
  needs `allowVolumeExpansion` (the Incus CSI one has it). Placement's
  `Dedicated` mode uses the chart's own `affinity.podAntiAffinity: hard`
  preset; `Default` explicitly sets `none` so pods can share a node (the
  chart's own root default is `soft`, which would already bias them apart).
  Deleting a deployment deletes its `HelmRelease` first and waits for
  helm-controller's own `helm uninstall` before sweeping the target
  namespace - required because the chart's `ClusterRole`/`ClusterRoleBinding`
  are cluster-scoped and a plain namespace delete can never reach them,
  orphaning them to block every future install under the same release name.
- **`TestCaseReconciler`**: `Burn` profile owns a self-healing k6
  `Deployment`; `Quick` profile just validates readiness (execution belongs
  to a `TestRun`). Mirrors the built-in k6 script into a `ConfigMap`.
- **`TestRunReconciler`**: executes one bounded `Quick` run as a k6 `Job`,
  tracks it to completion, and posts start/completion Grafana annotations to
  whichever Grafana `WorkloadClusterReconciler` installed for the target
  deployment's own cluster - reproducing `roles/runner/tasks/annotate.yml`'s
  behavior.
- **k6 tag/env contract**: `k6/scripts/login.ts` is ported byte-for-byte from
  the reference repo; `internal/render` reproduces its exact
  `testid`/`target`/`profile` tag scheme and env var contract
  (`TEST_ID`/`TARGET`/`BENCH_HOST`/... ), guarded by `test/contract`.
- **Fixtures**: the operator seeds synthetic test users at startup
  (`internal/fixtures`, a Go port of `fixtures/gen-blueprint.py`) unless
  they're already loaded - 50 by default (`BENCH_FIXTURE_USERS`), a random
  directory per install unless `BENCH_FIXTURE_SEED` is set.
  `make fixtures` (the original script) still replaces them on demand.
- **Test-case templates**: `TestCaseTemplate` is a real, UI-editable
  CRD (list/create/edit/delete, Monaco editor for the script) - the operator
  seeds nine built-in ones at startup (`login`; `oauth-oidc`, driving a
  real OAuth2/OIDC authorization-code round trip through
  [BeryJu/oidc-test-client](https://github.com/BeryJu/oidc-test-client);
  `saml`, an SP-initiated SAML login through
  [BeryJu/saml-test-sp](https://github.com/BeryJu/saml-test-sp);
  `oidc-source` and `saml-source`, federated logins through an authentik
  OAuth source (dex) and SAML source (saml-test-idp), where every VU is the
  IdP's one test user, enrolled once in k6's `setup()`;
  `oauth-client-credentials` and `oauth-token-exchange`, which only hit the
  OAuth2 token endpoint (a `client_credentials` grant, and an RFC 8693
  exchange of that token for another provider's);
  `mixed-usage`, a weighted mix of logins, logins + API browsing, failed
  logins and flow page loads, drawn from a PRNG seeded per TestCase via
  `spec.seed` - defaulting to a hash of the TestCase name - so every run
  replays the same sequence; `breakpoint`, a test-until-failure login run
  that adds `spec.vus` VUs every 2 minutes and aborts once authentik's error
  rate or p95 latency crosses its thresholds),
  sourced from the exact same embedded `k6/scripts/*.ts` files the operator
  binary carries (create-only, so editing one in the UI is never
  clobbered by a later restart). A template's `spec.dependencies` lists
  extra services the operator provisions per TestCase and target, together
  with the authentik objects they need (via a blueprint import):
  `oidc-client` ([BeryJu/oidc-test-client](https://github.com/BeryJu/oidc-test-client)
  + OAuth2 provider), `saml-client`
  ([BeryJu/saml-test-sp](https://github.com/BeryJu/saml-test-sp) + SAML
  provider), `oidc-idp` ([dex](https://dexidp.io) + OAuth source) and
  `saml-idp` ([BeryJu/saml-test-idp](https://github.com/BeryJu/saml-test-idp)
  + SAML source) and `oauth-token` (two OAuth2 providers, the second trusting
  the first's tokens for token exchange; no service, client secrets reach k6
  from a Secret). Their addresses, source slugs and IdP test credentials
  reach k6 as env vars (`render.DependencyEndpoints`).
- **Web UI**: `internal/api` is a REST/JSON backend (chi router) that CRUDs
  every CRD as unstructured objects (the CRD schema stays the single source
  of truth), proxies PromQL for the compare view (resolved per-testid
  against whichever cluster's own Prometheus actually has that data - see
  `prometheusURLsForTestID`), and reverse-proxies a workload cluster's own
  Grafana (`/grafana-proxy/<cluster>/...`). The same `cmd/apiserver`
  process serves the UI itself, `web/ui` - a Vite + React SPA (no SSR) with
  guided forms for every CRD, native metrics charts and a Compare view - so
  the whole UI is one Deployment (`make deploy-apiserver`,
  `hack/dev-apiserver.yaml`; `make ui-dev` for a live-reloading dev server).
  - **`kubectl` shell**: the UI's centerpiece - real shells on a PTY
    (`internal/api/shell.go`) bridged to xterm.js over a WebSocket, with
    `KUBECONFIG` pointed at a per-session copy of the merged kubeconfig
    `ShellKubeconfigReconciler` maintains, so every context (the
    management cluster plus each Ready `WorkloadCluster`) is one tab away.
    It replaces generic resource browsing: pages link into it with the
    relevant command typed at the prompt (never run until you press Enter).
    The WebSocket authenticates with a single-use, 10-second ticket minted by
    a normal authenticated request, and closing a tab kills that shell's
    whole process session.
  - **Auth**: OIDC (authorization code + PKCE, the SPA as a public client;
    `cmd/apiserver` verifies the JWT against the issuer's JWKS, optionally
    requiring a group - `BENCH_OIDC_ISSUER`/`BENCH_OIDC_CLIENT_ID`/
    `BENCH_OIDC_REQUIRED_GROUP`) and/or a static bearer token
    (`BENCH_API_TOKEN`/`--token-secret`). Register `<origin>/auth/callback`
    and `<origin>/auth/silent` as redirect URIs on the provider.

The original plan's central, mTLS-secured metrics receiver was superseded by
the per-workload-cluster observability stack described above once it became
clear a management-cluster pod can reach a workload cluster's node IPs
directly (the reverse can't), making a shared central pipeline unnecessary.
The Grafana dashboards live in
[`internal/render/manifests/grafana-dashboards/`](../internal/render/manifests/grafana-dashboards/) (see [grafana-dashboards.md](grafana-dashboards.md)).
