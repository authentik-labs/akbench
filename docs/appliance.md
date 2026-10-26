# akbench appliance

A single container that is a complete akbench management cluster:

- **k3s**, the management cluster itself (PID 1 of the container)
- **Flux**, the root GitOps controllers: helm-controller and source-controller install
  every chart akbench renders, here and on workload clusters
- **Cluster API**, with the kubeadm bootstrap/control-plane providers and
  cert-manager, installed with `clusterctl`
- **akbench**: the operator (via a Flux `HelmRelease` of its published chart)
  and the apiserver (web UI + kubectl shell)

The appliance is not tied to any infrastructure. Workload clusters run on
infrastructure outside the container, through a Cluster API
**infrastructure provider** you enable in the web UI, along with that
provider's connection settings. Today that's **Incus** (cluster-api-provider-incus);
the image ships every provider it supports, so others such as AWS can be
added without changing how the appliance is run.

## Running it

```sh
docker run -d --name akbench --privileged --restart unless-stopped \
  -v akbench:/var/lib/rancher/k3s \
  -p 8443:8443 -p 6443:6443 \
  ghcr.io/authentik-labs/akbench-appliance:latest
```

Follow the first boot (2-5 minutes, mostly image pulls):

```sh
docker logs -f akbench 2>&1 | grep '\[akbench\]'
```

Then open `https://<host>:8443/` and log in with the API token. The web UI
serves a self-signed certificate (regenerated on every start), so expect a
browser warning:

```sh
docker exec akbench akbench-appliance token
```

### Setting up infrastructure

Everything infrastructure-specific happens on the web UI's **Infrastructure**
page. For Incus:

1. **Enable the provider:** click **Enable** next to *Incus / LXC*. The
   appliance installs cluster-api-provider-incus with `clusterctl` within a
   minute or two.
2. **Add a connection:** go to **New connection** and enter the Incus API URL
   (`https://incus.example.com:8443`) and, if you use one, the project. The
   appliance generates a client certificate for it, or you can bring your own.
3. **Test and trust it.** The connection page tests the connection straight away:
   - If the server's (usually self-signed) certificate isn't trusted, it
     shows the certificate's fingerprint. Compare it with `incus info` on the
     server, then click **Pin this certificate**.
   - If the server doesn't trust the client yet, run
     `incus config trust add akbench-<name>` on the server and paste the
     token. Alternatively, download the client certificate and run
     `incus config trust add-certificate` on it.
4. **Register a cluster:** register a workload cluster under **Clusters** and
   pick the connection.

You don't need to load test users: the operator seeds 50 synthetic users at
startup, which every deployment imports and every k6 run logs in as. Set
`BENCH_FIXTURE_USERS` on the operator, or run `make fixtures`, for a
different set.

Connections are plain Secrets in the akbench namespace, in the shape
`WorkloadCluster.spec.incus.lxcSecretRef` expects. A Secret created by hand
or migrated from an older setup shows up as soon as a cluster uses it.

Enabled providers are never uninstalled: removing one would strand every
cluster it provisioned.

### kubectl

`kubectl` is available inside the container (`docker exec -it akbench kubectl get workloadclusters -A`).
To use it from outside, copy the admin kubeconfig. Its server is
`https://127.0.0.1:6443`, which works through `-p 6443:6443`:

```sh
docker exec akbench cat /etc/rancher/k3s/k3s.yaml > akbench.kubeconfig
```

### Requirements

- Docker (or Podman) with **cgroup v2** and `--privileged`, the same as any
  k3s-in-Docker setup (k3d, Rancher's single-node install).
- About 2 vCPU, 4 GiB RAM and 20 GiB disk for the management plane itself.
  Workload clusters run on your infrastructure, not here.
- **Outbound** network access from the Docker host to:
  - your infrastructure's API (for Incus, usually `:8443`);
  - each workload cluster's API load balancer (`:6443`) and node IPs
    (Grafana/Prometheus NodePorts). With Incus, those are on the instance
    network. If that network is NATed (the default `incusbr0`), route it to the
    Docker host or bridge it onto your LAN.
  - container registries (ghcr.io, docker.io, registry.k8s.io, quay.io) and
    the chart repositories akbench installs from.

  Nothing ever connects *into* the appliance except your browser, so the
  Docker NAT in front of it is fine.
- No address overlap between k3s's own pod/service networks (`10.42.0.0/16` and
  `10.43.0.0/16`) and the network workload clusters run on. If they do overlap,
  pass `--cluster-cidr`/`--service-cidr` as extra arguments (see below). They
  take effect on the first boot only.

### Configuration

The container's own configuration is limited to the appliance itself.
Everything infrastructure-specific lives in the web UI.

| Variable | Default | |
|---|---|---|
| `AKBENCH_NAMESPACE` | `default` | Namespace for your akbench objects and infrastructure connections. |
| `AKBENCH_UI_PORT` | `8443` | Port the web UI serves HTTPS on (publish it with `-p`). |
| `AKBENCH_API_TOKEN` | generated | Pin the web UI's bearer token. |
| `AKBENCH_OIDC_ISSUER`, `AKBENCH_OIDC_CLIENT_ID`, `AKBENCH_OIDC_REQUIRED_GROUP`, `AKBENCH_OIDC_SCOPES` | | OIDC login for the web UI (see the main README). |
| `AKBENCH_VERSION` | the image's own | Operator chart/image and apiserver image version to install. |

Extra arguments to `docker run … <image> <args>` are passed to `k3s server`.

The appliance writes its generated state (API token, k3s node identity)
under `/var/lib/rancher/k3s/akbench` on the volume.

## Upgrading

### akbench only, from the web UI

The **Providers** page shows the running akbench version and, when a newer
build is published (the `:latest` apiserver image), an **Update** button. It
rolls out that version's operator chart/image and apiserver image without
touching the container: the web UI pins the version in the
`authentik-bench/appliance-version` ConfigMap and the appliance applies it
within a few seconds. The web UI restarts while it does.

The pin only holds for the appliance image it was made on: replacing the
container with a newer appliance image goes back to the version that image
ships.

### The whole appliance

Every boot re-runs the whole bootstrap, and every step is idempotent. To upgrade,
replace the container and keep the volume:

```sh
docker pull ghcr.io/authentik-labs/akbench-appliance:<new>
docker rm -f akbench
docker run -d --name akbench … ghcr.io/authentik-labs/akbench-appliance:<new>   # same flags as before
```

This rolls out:
- the new operator chart and apiserver image;
- `clusterctl upgrade apply` for Cluster API and every enabled infrastructure
  provider the new image ships a newer version of (providers are never downgraded);
- the new Flux manifests;
- the new k3s.

k3s only supports upgrading its datastore **one minor version at a time**,
so don't skip appliance releases that bump k3s by more than one minor.

## Backups

The appliance is a single node. If you lose the volume, your workload clusters
keep running on your infrastructure, but nothing manages them any more. Back
the volume up, for example with the container stopped:

```sh
docker run --rm -v akbench:/data -v "$PWD:/backup" debian:trixie-slim tar czf /backup/akbench.tgz -C /data .
```

Or move everything to another management cluster with `clusterctl move`, the
same mechanism the migration uses.

## Migrating from an existing management cluster

See [appliance-migration.md](appliance-migration.md).

## Development

```sh
make appliance-dev                    # build + run locally (apiserver from this checkout)
make docker-build-appliance           # just build the image
make appliance-assets                 # re-pin Flux/clusterctl/CAPI/cert-manager/k3s and the infrastructure providers
```

Every upstream file the image ships is pinned with its checksum in
`assets.lock`. To bump a version, or to ship another infrastructure provider
(`INFRASTRUCTURE_PROVIDERS`), edit `hack/update-appliance-assets.sh` and run
`make appliance-assets`. A provider the image ships can be enabled in the
UI. Provisioning `WorkloadCluster`s through it additionally needs operator
support (a new `spec.provider`) and an entry in
`internal/api/infrastructure.go`'s `knownInfraProviders`, plus its own
connection settings next to `internal/api/incus.go`.
