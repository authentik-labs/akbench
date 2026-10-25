import { api } from "../api";
import { MatrixGrid } from "../components/Matrix";
import { NodePlacement } from "../components/Placement";
import { Badge } from "../components/Status";
import { buildTargetRows, burningFor, TargetTable } from "../components/Targets";
import { Alert, Empty, formatTimestamp, Loading, Page, Stat, Table } from "../components/ui";
import { formatMetricValue } from "../format";
import { RangePoint, TestMatrix, testRunDeployment } from "../types";
import { useResource } from "../useResource";

import { Link } from "react-router";

// Burn tests lead: they're what keeps deployments under load right now.
// The dashboard shows their live state per target; a test case's own page
// has the full charts.
const BURN_SUMMARY_METRICS = ["iterations", "iteration_duration_p95", "http_req_failed"];
const TREND_METRIC = "iteration_duration_p95";
const TREND_RANGE_SECONDS = 6 * 3600;
const DAY_MS = 86400 * 1000;
// Where each workload cluster's own observability stack runs
// (render.ObservabilityNamespace). An alert there means the numbers on this
// page may be wrong, so it's raised as a banner rather than just counted.
const OBSERVABILITY_NAMESPACE = "bench-system";
// TestCase label naming the TestMatrix that created it (LabelTestMatrix).
const MATRIX_LABEL = "bench.goauthentik.io/testmatrix";

export function DashboardPage() {
    const deployments = useResource(() => api.deployments(), [], 15000);
    const testCases = useResource(() => api.testCases(), [], 15000);
    const runs = useResource(() => api.testRuns(), [], 10000);
    const clusters = useResource(() => api.clusters(), [], 30000);
    const alerts = useResource(() => api.alerts(), [], 30000);
    const matrices = useResource(() => api.testMatrices(), [], 15000);

    const burnTests = (testCases.data ?? [])
        .filter((tc) => tc.spec.profile === "Burn" && !tc.spec.archived)
        .sort((a, b) => a.metadata.name.localeCompare(b.metadata.name));
    const burnIDs = burnTests.map((tc) => tc.metadata.name);
    const live = useResource(
        () => (burnIDs.length ? api.compare(burnIDs, "1m") : Promise.resolve(undefined)),
        [burnIDs.join(",")],
        15000,
    );
    const trend = useResource(
        () => {
            if (!burnIDs.length) {
                return Promise.resolve(undefined);
            }
            const end = Math.floor(Date.now() / 1000);
            return api.metricsRange(burnIDs, TREND_METRIC, end - TREND_RANGE_SECONDS, end, "5m");
        },
        [burnIDs.join(",")],
        60000,
    );

    // A matrix's test cases are shown as its grid instead of one group each;
    // a one-template matrix's single test case keeps its own, fuller group.
    const burnMatrices = (matrices.data ?? [])
        .filter((m) => m.spec.templates.length > 1)
        .filter((m) => (m.status?.rows ?? []).some((r) => burnIDs.includes(r.testCase)))
        .sort((a, b) => a.metadata.name.localeCompare(b.metadata.name));
    const matrixBurnTests = (m: TestMatrix) =>
        burnTests.filter((tc) => tc.metadata.labels?.[MATRIX_LABEL] === m.metadata.name);
    const trendMetric = live.data?.metrics.find((x) => x.key === TREND_METRIC);

    const groups = burnTests.map((tc) => {
        const sparklines = new Map<string, RangePoint[]>();
        for (const series of trend.data?.series ?? []) {
            if (series.testid === tc.metadata.name) {
                sparklines.set(series.target, series.points ?? []);
            }
        }
        return {
            tc,
            sparklines,
            rows: buildTargetRows(tc, live.data?.results ?? [], deployments.data ?? []),
        };
    });
    const targets = groups.flatMap((g) => g.rows);
    const notReadyTargets = targets.filter((r) => r.ready === false).length;
    const total = (key: string) => targets.reduce((sum, r) => sum + (r.values[key] ?? 0), 0);
    const worst = (key: string) =>
        targets
            .filter((r) => r.values[key] !== undefined)
            .reduce<(typeof targets)[number] | undefined>(
                (w, r) => (!w || r.values[key] > w.values[key] ? r : w),
                undefined,
            );
    const slowest = worst("http_req_duration_p99");
    const mostFailing = worst("http_req_failed");
    const firing = (alerts.data?.items ?? []).filter((a) => a.state === "active");
    const monitoringAlerts = firing.filter((a) => a.labels.namespace === OBSERVABILITY_NAMESPACE);

    const quickRuns = [...(runs.data ?? [])].sort((a, b) =>
        (b.metadata.creationTimestamp ?? "").localeCompare(a.metadata.creationTimestamp ?? ""),
    );
    const lastDay = quickRuns.filter(
        // Re-evaluated on each poll's re-render, which is as fresh as it needs to be.
        // oxlint-disable-next-line react/purity
        (r) => Date.now() - Date.parse(r.metadata.creationTimestamp ?? "") < DAY_MS,
    );
    const failedLastDay = lastDay.filter((r) => r.status?.phase === "Failed").length;
    const notReady = (deployments.data ?? []).filter(
        (d) => d.status?.phase && d.status.phase !== "Ready",
    );

    return (
        <Page
            title="Dashboard"
            subtitle="Sustained load on your authentik deployments, and recent quick runs."
            actions={
                <>
                    <Link className="button" to="/compare">
                        Compare
                    </Link>
                    <Link className="button primary" to="/testcases/new">
                        New test case
                    </Link>
                </>
            }
        >
            <Alert>{deployments.error ?? testCases.error ?? runs.error ?? live.error}</Alert>
            {monitoringAlerts.map((a) => (
                <Alert key={`${a.cluster}/${a.labels.alertname}/${a.startsAt}`}>
                    <Link to="/alerts">{a.labels.alertname}</Link> on{" "}
                    <Link to={`/clusters/${a.cluster}`}>{a.cluster}</Link>
                    {a.annotations.summary && `: ${a.annotations.summary}`}
                </Alert>
            ))}
            <div className="stats">
                <Stat
                    value={live.data ? formatMetricValue(total("http_reqs"), "req/s") : "-"}
                    label="Requests/s under burn"
                    hint={`${burnTests.length} burn tests · ${targets.length} targets`}
                />
                <Stat
                    value={live.data ? formatMetricValue(total("iterations"), "tx/s") : "-"}
                    label="Transactions/s under burn"
                />
                <Stat
                    value={formatMetricValue(slowest?.values.http_req_duration_p99, "s")}
                    label="Slowest request p99"
                    hint={slowest?.deploymentName}
                />
                <Stat
                    value={formatMetricValue(mostFailing?.values.http_req_failed, "ratio")}
                    label="Highest failure rate"
                    hint={mostFailing?.deploymentName}
                />
                <Stat
                    value={alerts.data ? <Link to="/alerts">{firing.length}</Link> : "-"}
                    label="Alerts firing"
                    hint={
                        firing.length
                            ? [...new Set(firing.map((a) => a.cluster))].join(", ")
                            : undefined
                    }
                />
                <Stat
                    value={deployments.data?.length ?? "-"}
                    label="Deployments"
                    hint={notReady.length ? `${notReady.length} not ready` : "All ready"}
                />
                <Stat
                    value={runs.data ? lastDay.length : "-"}
                    label="Quick runs · 24 h"
                    hint={failedLastDay ? `${failedLastDay} failed` : undefined}
                />
                <Stat value={clusters.data?.length ?? "-"} label="Clusters" />
            </div>

            <div className="section-head">
                <div>
                    <h2>Burn tests</h2>
                    <div className="muted">
                        Each test drives one or more targets with the same load. Live values cover
                        the last minute; the trend shows p95 over 6 hours. A test matrix shows p95
                        per template and variant, against its baseline.
                    </div>
                </div>
                {notReadyTargets > 0 && <Badge value="Not ready" />}
            </div>
            {!testCases.data && !testCases.error ? (
                <Loading />
            ) : testCases.data && burnTests.length === 0 ? (
                <Empty>
                    No burn tests running. A Burn test keeps deployments under steady load until you
                    stop it - <Link to="/testcases/new">create one</Link>.
                </Empty>
            ) : (
                groups
                    .filter(({ tc }) => !burnMatrices.some((m) => matrixBurnTests(m).includes(tc)))
                    .map(({ tc, rows, sparklines }) => {
                        const since = burningFor(tc);
                        const degraded = rows.filter((r) => r.ready === false).length;
                        return (
                            <div className="group" key={tc.metadata.name}>
                                <div className="group-head">
                                    <span>
                                        <Link
                                            to={`/testcases/${tc.metadata.name}`}
                                            className="mono"
                                        >
                                            <strong>{tc.metadata.name}</strong>
                                        </Link>{" "}
                                        <span className="muted">
                                            · {rows.length}{" "}
                                            {rows.length === 1 ? "target" : "targets"} ·{" "}
                                            {tc.spec.vus} VUs each
                                            {since && ` · burning for ${since}`}
                                        </span>
                                    </span>
                                    {degraded > 0 ? (
                                        <Badge value="Not ready" />
                                    ) : (
                                        <Badge value={tc.status?.phase ?? "Pending"} />
                                    )}
                                </div>
                                <TargetTable
                                    rows={rows}
                                    metrics={live.data?.metrics ?? []}
                                    metricKeys={BURN_SUMMARY_METRICS}
                                    loading={live.loading && !live.data}
                                    sparklines={sparklines}
                                />
                            </div>
                        );
                    })
            )}

            {burnMatrices.map((m) => {
                const since = matrixBurnTests(m)
                    .map((tc) => burningFor(tc))
                    .find((d) => d !== undefined);
                return (
                    <div className="group" key={m.metadata.name}>
                        <div className="group-head">
                            <span>
                                <Link to={`/testmatrices/${m.metadata.name}`} className="mono">
                                    <strong>{m.metadata.name}</strong>
                                </Link>{" "}
                                <span className="muted">
                                    · {m.spec.variants.map((v) => v.name).join(" vs ")} ·{" "}
                                    {m.spec.testCase.vus} VUs each
                                    {since && ` · burning for ${since}`}
                                </span>
                            </span>
                            <Badge value={m.status?.phase ?? "Pending"} />
                        </div>
                        <MatrixGrid
                            matrix={m}
                            results={live.data?.results ?? []}
                            metric={trendMetric}
                            metricKey={TREND_METRIC}
                        />
                    </div>
                );
            })}

            {notReady.length > 0 && (
                <>
                    <h2>Needs attention</h2>
                    <div className="row">
                        {notReady.map((d) => (
                            <Link
                                key={d.metadata.name}
                                className="button small"
                                to={`/deployments/${d.metadata.name}`}
                            >
                                {d.metadata.name} <Badge value={d.status?.phase} />
                            </Link>
                        ))}
                    </div>
                </>
            )}

            <div className="section-head">
                <div>
                    <h2>Node placement</h2>
                    <div className="muted">
                        Which node each deployment's pods run on. An outlined node holds more than
                        one deployment.
                    </div>
                </div>
            </div>
            <NodePlacement />

            <div className="section-head">
                <h2>Recent quick runs</h2>
                <Link to="/testruns">All runs</Link>
            </div>
            <Table
                rows={runs.data && quickRuns.slice(0, 8)}
                rowKey={(r) => r.metadata.name}
                empty="No quick runs yet."
                columns={[
                    {
                        label: "Run",
                        render: (r) => (
                            <Link to={`/testruns/${r.metadata.name}`}>{r.metadata.name}</Link>
                        ),
                    },
                    {
                        label: "Test case",
                        render: (r) => (
                            <Link to={`/testcases/${r.spec.testCaseRef.name}`}>
                                {r.spec.testCaseRef.name}
                            </Link>
                        ),
                    },
                    {
                        label: "Target",
                        render: (r) => testRunDeployment(r) ?? <span className="muted">-</span>,
                    },
                    {
                        label: "Phase",
                        render: (r) => <Badge value={r.status?.phase ?? "Pending"} />,
                    },
                    {
                        label: "Started",
                        render: (r) =>
                            formatTimestamp(r.status?.startTime ?? r.metadata.creationTimestamp),
                    },
                ]}
            />
        </Page>
    );
}
