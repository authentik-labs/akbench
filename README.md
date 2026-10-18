# authentik-bench

A Kubernetes operator and web UI for running k6 load tests against authentik.
It replaces the Ansible/docker-compose harness at
[BeryJu/authentik-benchmark](https://github.com/BeryJu/authentik-benchmark).

- Provisions workload clusters with Cluster API (Incus today).
- Deploys authentik with configurable replicas, node placement and Postgres
  (embedded, CloudNativePG or external).
- Runs k6 test cases from editable templates (login, OIDC, SAML, sources,
  mixed usage, breakpoint).
- Installs Prometheus, Grafana, Pyroscope, Tempo and Loki (fed by Alloy) on
  every workload cluster.
- Web UI with an in-browser `kubectl` shell, a compare view and a read-only
  MCP server.

## Quick start

Run the single-container appliance (k3s + Flux + Cluster API + akbench):

```sh
docker run -d --name akbench --privileged --restart unless-stopped \
  -v akbench:/var/lib/rancher/k3s -p 8443:8443 -p 6443:6443 \
  ghcr.io/authentik-labs/akbench-appliance:latest
docker exec akbench akbench-appliance token
```

Open `https://<host>:8443/` and log in with the token. See
[docs/appliance.md](docs/appliance.md).

Or install the operator chart into an existing management cluster:

```sh
helm install akbench oci://ghcr.io/<org>/<repo>-charts/authentik-bench-operator \
  --namespace authentik-bench-system --create-namespace
```

## Documentation

- [Architecture](docs/architecture.md): CRDs, controllers, web UI and auth
- [Appliance](docs/appliance.md) and [migrating onto it](docs/appliance-migration.md)
- [MCP server](docs/mcp.md)
- [Grafana dashboards](docs/grafana-dashboards.md)
- [Development](docs/development.md): local setup, tests, deploying, contributing

## License

Apache License 2.0. Copyright 2026.
