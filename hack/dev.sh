#!/usr/bin/env bash
# Starts the operator manager and the web API backend against the current
# kubeconfig context, in one foreground command. Ctrl+C stops both. The web
# UI (web/ui) is served by the web API itself once built (`make ui`); for
# live-reloading UI work run `make ui-dev` alongside this instead.
#
# Usage: make dev
set -euo pipefail
# Job control: puts each `&` job in its own process group, so cleanup can
# signal "-$pid" (the group) rather than just $pid. Needed because `go run`
# forks further (a temp binary) - killing only the tracked PID would leave
# that actual process running as an orphan still bound to its port. See
# hack/webui-e2e.sh, which hit exactly this.
set -m

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BENCH_API_TOKEN="${BENCH_API_TOKEN:-dev-token-for-local-development-only}"
export BENCH_API_TOKEN

pids=()
cleanup() {
  echo "==> Stopping dev environment"
  for pid in "${pids[@]:-}"; do
    kill -TERM -- "-${pid}" 2>/dev/null || kill "${pid}" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

wait_for() {
  local url="$1" name="$2" attempt
  for attempt in $(seq 1 60); do
    if curl -kfsS -o /dev/null "${url}"; then
      return 0
    fi
    sleep 1
  done
  echo "${name} did not come up at ${url}" >&2
  return 1
}

echo "==> Installing CRDs into the current kubeconfig context ($(kubectl config current-context 2>/dev/null || echo unknown))"
(cd "${root}" && make install)

# Best-effort: the operator/API both work without it, but every metrics
# feature (compare, the dashboard's live charts, a TestCase's own metrics)
# just reports "no Prometheus configured" - see hack/dev-prometheus.yaml for
# how to stand one up locally if this doesn't find one.
if [[ -z "${BENCH_PROMETHEUS_URL:-}" ]] && kubectl get svc prometheus-server -n bench-system >/dev/null 2>&1; then
  echo "==> Port-forwarding bench-system/prometheus-server to :9091"
  kubectl port-forward -n bench-system svc/prometheus-server 9091:80 >/dev/null 2>&1 &
  pids+=($!)
  BENCH_PROMETHEUS_URL="http://127.0.0.1:9091"
fi
export BENCH_PROMETHEUS_URL="${BENCH_PROMETHEUS_URL:-}"

echo "==> Starting the operator manager"
# The validating webhooks need a cert-manager issued certificate and an
# in-cluster Service the API server can reach - neither exists for a
# locally running manager.
(cd "${root}" && ENABLE_WEBHOOKS=false go run ./cmd/main.go) &
pids+=($!)
wait_for "http://127.0.0.1:8081/healthz" "operator manager"

echo "==> Starting the web API backend"
if [[ -z "${BENCH_UI_DIR:-}" && -f "${root}/web/ui/dist/index.html" ]]; then
  export BENCH_UI_DIR="${root}/web/ui/dist"
fi
# The kubectl shell is opt-in here: set BENCH_SHELL_KUBECONFIG to a
# kubeconfig holding only the clusters you mean to expose. Defaulting to
# your own ~/.kube/config would hand every context in it to anyone with the
# (well-known) dev token who can reach :8443.
export BENCH_SHELL="${BENCH_SHELL:-$(command -v bash || echo /bin/sh)}"
(cd "${root}" && go run ./cmd/apiserver) &
pids+=($!)
wait_for "https://127.0.0.1:8443/healthz" "web API"

cat <<EOF

authentik-bench dev environment is up:
  API backend + UI  https://localhost:8443  (self-signed; token: ${BENCH_API_TOKEN:-dev-token-for-local-development-only})
  operator manager  health probe on :8081

The UI is served from web/ui/dist (run \`make ui\` to build it); for
live-reloading UI work run \`make ui-dev\` and open http://localhost:5173.

Press Ctrl+C to stop everything.
EOF

wait
