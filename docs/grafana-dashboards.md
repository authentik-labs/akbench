# Dashboards

The Grafana dashboards in
[`internal/render/manifests/grafana-dashboards/`](../internal/render/manifests/grafana-dashboards/)
are embedded via `//go:embed` (every `*.json` there) and provisioned into every workload cluster's own Grafana by
`internal/render/grafana.go`, compacted to fit a single ConfigMap. The web UI's
Grafana page embeds them through `cmd/apiserver`'s `/grafana-proxy/{cluster}/...`
reverse proxy (`internal/api/grafana_proxy.go`).

All but `nodes.json` are copied from the reference harness,
[goauthentik/benchmark](https://github.com/goauthentik/benchmark/tree/main/roles/metrics/files/grafana/dashboards).
To update them, copy the upstream files over and re-apply the local change
below.

| File | UID | Source |
| --- | --- | --- |
| `summary.json` | `benchmark-summary` | upstream (v2 schema) |
| `compare-tests.json` | `benchmark-test-comparison` | upstream (v2 schema) |
| `k6.json` | `ccbb2351-2ae2-462f-ae0e-f2c893ad1028` | upstream |
| `authentik.json` | `authentik` | upstream |
| `postgres.json` | `pg_exporter_monitoring` | upstream |
| `node_exporter.json` | `rYdddlPWk` | upstream |
| `nodes.json` | `akbench-nodes` | this project |

## Matching the reference harness

The dashboards reference datasources by the same uids the harness provisions
(`prometheus`, `pyroscope`, `tempo`, `loki`; see `grafanaDatasources`). The v2-schema
ones need Grafana 13, which the chart ships.

They select on the labels the harness's `prometheus.yml.j2` attaches, which
this project reproduces with ServiceMonitor relabelings (`scrapeRelabelings`
in `internal/render/metrics.go`):

- authentik server/worker: `job="authentik"`, `target` (the deployment's
  `targetName`, the same value k6 tags its metrics with), `host` (node name).
- Embedded postgres' `postgres_exporter` sidecar: `job="postgres"`, `target`,
  `host`, plus the same extra collectors the harness enables.
- node-exporter: `host`. Its job stays `node-exporter`, since
  kube-prometheus-stack's own rules rely on it.

## Local changes

- `summary.json`'s `host` variable queries `up{job="node-exporter"}` instead
  of upstream's `up{job="node"}`.

## Known gaps

- CNPG-mode deployments expose CNPG's own `cnpg_*` metrics, not
  `postgres_exporter`'s `pg_*`, so the postgres panels stay empty for them.
- `postgres.json` is written for [nbari/pg_exporter](https://github.com/nbari/pg_exporter)
  (`pg_exporter_*`, `pg_system_*`, `pg_stat_io_*`, ...), not the
  `postgres_exporter` the harness and this project run: only 43 of the 180
  metrics it queries exist in `postgres_exporter`'s output.
- `summary.json`'s checkpoint panels query `pg_stat_bgwriter_checkpoints_*`
  and `buffers_{backend,checkpoint}`, which PostgreSQL 17 moved out of
  `pg_stat_bgwriter`, so no exporter emits them against the embedded
  postgres (17).
- `k6.json`'s latency panels query k6's trend-stat series
  (`k6_http_req_duration_p95`, ...), but k6 here (and in the harness) writes
  trends as native histograms (`k6_http_req_duration_seconds`). `summary.json`
  and `compare-tests.json` use the native histograms.
