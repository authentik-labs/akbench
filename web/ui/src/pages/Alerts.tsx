import { api } from "../api";
import { Alert as AlertBanner, formatTimestamp, Page, Table } from "../components/ui";
import { shellLinkForCluster } from "../shell/links";
import { Alert } from "../types";
import { useResource } from "../useResource";

// There is no single central Alertmanager - one runs per workload cluster
// (see internal/api/alerts.go's own doc comment), so this page is always an
// aggregate across whichever clusters have reconciled an AlertmanagerURL.
// No notification channel is configured anywhere yet (a deliberate choice -
// see render.defaultAlertmanagerConfig's own doc comment), so this page is
// currently the only place firing alerts are visible at all.
//
// Grouped by cluster then namespace rather than one flat table: an alert's
// own cluster/namespace is exactly the scope a human investigating it is
// about to `kubectl` into, so that's the natural way to scan a page that can
// otherwise mix a dozen unrelated components' alerts together. A handful of
// alerts are cluster-scoped and carry no namespace at all (etcd, node-level
// alerts) - those get their own "(cluster-scoped)" group per
// cluster rather than being dropped or miscategorized.
import { Link } from "react-router";

const CLUSTER_SCOPED = "(cluster-scoped)";

function stateClass(state: string): string {
    switch (state) {
        case "active":
            return "err";
        case "suppressed":
            return "info";
        default:
            return "neutral";
    }
}

// groupByClusterThenNamespace returns clusters in the order they were first
// seen (cheap to compute, stable enough for a page that reloads every 15s),
// each with its namespaces sorted alphabetically except CLUSTER_SCOPED last -
// cluster-wide alerts are context for the namespaced ones above them, not the
// first thing worth scanning.
function groupByClusterThenNamespace(alerts: Alert[]): [string, [string, Alert[]][]][] {
    const byCluster = new Map<string, Map<string, Alert[]>>();
    for (const a of alerts) {
        const namespace = a.labels.namespace || CLUSTER_SCOPED;
        if (!byCluster.has(a.cluster)) {
            byCluster.set(a.cluster, new Map());
        }
        const byNamespace = byCluster.get(a.cluster)!;
        if (!byNamespace.has(namespace)) {
            byNamespace.set(namespace, []);
        }
        byNamespace.get(namespace)!.push(a);
    }
    return [...byCluster.entries()].map(([cluster, byNamespace]) => [
        cluster,
        [...byNamespace.entries()].sort(([a], [b]) =>
            a === CLUSTER_SCOPED ? 1 : b === CLUSTER_SCOPED ? -1 : a.localeCompare(b),
        ),
    ]);
}

function alertColumns(cluster: string) {
    return [
        {
            label: "Alert",
            render: (a: Alert) => (
                <>
                    <strong>{a.labels.alertname}</strong>
                    {a.annotations.summary && <div className="muted">{a.annotations.summary}</div>}
                    <details>
                        <summary className="muted" style={{ margin: "4px 0", fontWeight: 400 }}>
                            Labels
                        </summary>
                        <div className="mono" style={{ fontSize: 12 }}>
                            {Object.entries(a.labels)
                                .sort(([x], [y]) => x.localeCompare(y))
                                .map(([k, v]) => (
                                    <div key={k}>
                                        {k}=<strong>{v}</strong>
                                    </div>
                                ))}
                        </div>
                    </details>
                </>
            ),
        },
        { label: "Severity", render: (a: Alert) => a.labels.severity ?? "unknown" },
        {
            label: "State",
            render: (a: Alert) => <span className={`badge ${stateClass(a.state)}`}>{a.state}</span>,
        },
        { label: "Since", render: (a: Alert) => formatTimestamp(a.startsAt) },
        {
            label: "",
            className: "actions",
            render: () => (
                <Link className="button small" to={shellLinkForCluster(cluster)}>
                    Shell
                </Link>
            ),
        },
    ];
}

export function AlertsPage() {
    const alerts = useResource(() => api.alerts(), [], 15000);
    const grouped = alerts.data && groupByClusterThenNamespace(alerts.data.items);

    return (
        <Page
            title="Alerts"
            subtitle="Firing alerts across every workload cluster's own Alertmanager - there is no notification channel configured yet, so this is the only place to see them."
        >
            <AlertBanner>{alerts.error}</AlertBanner>
            {Object.entries(alerts.data?.unhealthy ?? {}).map(([cluster, reason]) => (
                <AlertBanner key={cluster}>
                    <Link to={`/clusters/${cluster}`}>{cluster}</Link>: {reason}
                </AlertBanner>
            ))}
            {grouped && grouped.length === 0 && (
                <p className="muted">No alerts firing right now.</p>
            )}
            {grouped?.map(([cluster, byNamespace]) => (
                <div key={cluster} className="panel">
                    <h2>
                        <Link to={`/clusters/${cluster}`}>{cluster}</Link>
                    </h2>
                    {byNamespace.map(([namespace, items]) => (
                        <div key={namespace} style={{ marginBottom: 16 }}>
                            <h3>
                                {namespace === CLUSTER_SCOPED ? (
                                    CLUSTER_SCOPED
                                ) : (
                                    <code>{namespace}</code>
                                )}
                            </h3>
                            <Table
                                rows={[...items].sort((a, b) =>
                                    b.startsAt.localeCompare(a.startsAt),
                                )}
                                rowKey={(a) => `${a.cluster}/${a.labels.alertname}/${a.startsAt}`}
                                empty=""
                                columns={alertColumns(cluster)}
                            />
                        </div>
                    ))}
                </div>
            ))}
        </Page>
    );
}
