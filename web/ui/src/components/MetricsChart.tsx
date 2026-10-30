import { api } from "../api";
import {
    colorFor,
    formatMetricValue,
    onlyTargets,
    relativeDelta,
    seriesAverage,
    stepFor,
    topActors,
} from "../format";
import { useDarkTheme } from "../theme";
import {
    CompareMetric,
    MonitoringMetric,
    PostgresMetric,
    RangeResponse,
    WorkerActor,
    WorkerMetric,
} from "../types";
import { useResource } from "../useResource";
import { Delta } from "./Targets";
import { Column, Table } from "./ui";

import {
    Chart as ChartJS,
    Legend as ChartJSLegend,
    LinearScale,
    LineElement,
    PointElement,
    Tooltip as ChartJSTooltip,
    type ChartOptions,
    type TooltipItem,
} from "chart.js";
// One metric over time, one line per test, from /metrics/range or
// /metrics/resources. Natively rendered (Chart.js via react-chartjs-2) - no
// Grafana iframe.
import { useMemo } from "react";
import { Line } from "react-chartjs-2";

ChartJS.register(LinearScale, PointElement, LineElement, ChartJSLegend, ChartJSTooltip);

function formatTime(seconds: number, withDate = false): string {
    const date = new Date(seconds * 1000);
    if (withDate) {
        return date.toLocaleString([], {
            month: "short",
            day: "numeric",
            hour: "2-digit",
            minute: "2-digit",
        });
    }
    return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

/**
 * ChartWindow is the [start, end] (unix seconds) a range query covered -
 * the x axis spans exactly this, not whatever the data happens to cover
 * rounded out to "nice" ticks.
 */
interface ChartWindow {
    start: number;
    end: number;
}

/** WindowedRange is a range response plus the window it was asked for. */
interface WindowedRange {
    response: RangeResponse;
    window: ChartWindow;
}

/**
 * useAvailableMetrics discovers the metric definitions cmd/apiserver knows
 * about (internal/api/metrics.go's compareMetrics) from a live
 * /metrics/compare call, rather than hardcoding a list here that would
 * silently go stale whenever the backend adds or renames one. Only the
 * definitions are used; the testid is just to get a real response. Fetched
 * once - they are static for a running backend.
 */
export function useAvailableMetrics(testids: string[]): CompareMetric[] {
    const primary = testids[0];
    const result = useResource(
        () => (primary ? api.compare([primary], "5m") : Promise.resolve(undefined)),
        [primary],
        0,
    );
    return result.data?.metrics ?? [];
}

/**
 * ChartTheme is the resolved theme the charts draw with. Chart.js paints
 * to a canvas, which can't resolve CSS custom properties - a literal
 * 'var(--muted)' silently falls back to black - so the values are read off
 * the document instead.
 */
interface ChartTheme {
    text: string;
    muted: string;
    border: string;
    panel: string;
    font: string;
    dark: boolean;
}

function readChartTheme(dark: boolean): ChartTheme {
    const style = getComputedStyle(document.documentElement);
    const value = (name: string, fallback: string) =>
        style.getPropertyValue(name).trim() || fallback;
    return {
        text: value("--text", "#151515"),
        muted: value("--muted", "#6a6e73"),
        border: value("--border", "#d2d2d2"),
        panel: value("--panel", "#ffffff"),
        font: value("--ak-global--font-family--body", "sans-serif"),
        dark,
    };
}

/**
 * useChartTheme re-reads the theme whenever main.tsx flips
 * html[data-theme] (an OS light/dark switch), so open charts follow along.
 */
function useChartTheme(): ChartTheme {
    const dark = useDarkTheme();
    return useMemo(() => readChartTheme(dark), [dark]);
}

/**
 * rangeChartOptions builds the Chart.js options shared by every line chart
 * on this page - a numeric (unix-seconds) x axis rather than Chart.js' own
 * 'time' scale, which would need the date-fns adapter as an extra
 * dependency for a feature (calendar-aware ticks) nothing here needs.
 */
function rangeChartOptions(
    unit: string,
    showLegend: boolean,
    theme: ChartTheme,
    window?: ChartWindow,
): ChartOptions<"line"> {
    const font = { family: theme.font, size: 11 };
    // Past a day, a bare time of day no longer says which day it is.
    const withDate = !!window && window.end - window.start > 86400;
    return {
        responsive: true,
        maintainAspectRatio: false,
        animation: false,
        normalized: true,
        interaction: { mode: "index", intersect: false },
        scales: {
            x: {
                type: "linear",
                min: window?.start,
                max: window?.end,
                ticks: {
                    callback: (v) => formatTime(v as number, withDate),
                    color: theme.muted,
                    font,
                    maxTicksLimit: 8,
                },
                grid: { color: theme.border },
                border: { color: theme.border },
            },
            y: {
                beginAtZero: true,
                // Ratios capped at 100%; everything else auto-scales.
                max: unit === "ratio" ? 1 : undefined,
                ticks: {
                    callback: (v) => formatMetricValue(v as number, unit),
                    color: theme.muted,
                    font,
                },
                grid: { color: theme.border },
                border: { color: theme.border },
            },
        },
        plugins: {
            legend: {
                display: showLegend,
                position: "bottom",
                labels: {
                    color: theme.text,
                    font: { ...font, size: 12 },
                    boxWidth: 12,
                    boxHeight: 3,
                    padding: 14,
                },
            },
            tooltip: {
                backgroundColor: theme.panel,
                titleColor: theme.text,
                bodyColor: theme.text,
                borderColor: theme.border,
                borderWidth: 1,
                titleFont: { ...font, size: 12 },
                bodyFont: { ...font, size: 12 },
                callbacks: {
                    title: (items) => formatTime(items[0]?.parsed.x ?? 0, withDate),
                    label: (item: TooltipItem<"line">) =>
                        `${item.dataset.label}: ${formatMetricValue(item.parsed.y ?? undefined, unit)}`,
                },
            },
        },
        elements: {
            point: { radius: 0, hoverRadius: 3 },
            line: { tension: 0.25, borderWidth: 2 },
        },
    };
}

/**
 * rangeDatasets turns a RangeResponse into Chart.js datasets, one per
 * (testid, target) series. A series is labeled by its testid alone unless
 * that testid has more than one target in this response (a multi-deployment
 * TestCase), in which case "testid / target" disambiguates - avoids an
 * always-on suffix for the common single-deployment case. spanGaps true
 * mirrors recharts' own connectNulls.
 */
function rangeDatasets(data: RangeResponse | undefined, dark: boolean) {
    const series = data?.series ?? [];
    const targetsByTestID = new Map<string, Set<string>>();
    for (const s of series) {
        const targets = targetsByTestID.get(s.testid) ?? new Set<string>();
        targets.add(s.target);
        targetsByTestID.set(s.testid, targets);
    }
    // A single test's own page needn't repeat its name on every line.
    const singleTest = targetsByTestID.size === 1;
    return series.map((s, i) => ({
        label:
            (targetsByTestID.get(s.testid)?.size ?? 0) > 1
                ? singleTest
                    ? s.target
                    : `${s.testid} / ${s.target}`
                : s.testid,
        data: (s.points ?? []).map((p) => ({ x: p.time, y: p.value })),
        borderColor: colorFor(i, dark),
        backgroundColor: colorFor(i, dark),
        spanGaps: true,
    }));
}

export function MetricsChart({
    metric,
    testids,
    targets,
    rangeSeconds = 900,
    step,
    refreshMs = 15000,
    height = 220,
    endSeconds,
}: {
    metric: string;
    testids: string[];
    /** Only chart these k6 targets (default: every target of the tests). */
    targets?: string[];
    rangeSeconds?: number;
    /** Defaults to stepFor(rangeSeconds). */
    step?: string;
    refreshMs?: number;
    height?: number;
    /**
     * Anchors the window to a unix time instead of "now" - for a Quick
     * TestCase whose k6 Job already exited (format.ts's metricsAnchorFromRuns).
     */
    endSeconds?: number;
}) {
    const { data: windowed, error } = useResource<WindowedRange | undefined>(
        () => {
            if (testids.length === 0) {
                return Promise.resolve(undefined);
            }
            const end = endSeconds ?? Math.floor(Date.now() / 1000);
            const window = { start: end - rangeSeconds, end };
            return api
                .metricsRange(
                    testids,
                    metric,
                    window.start,
                    window.end,
                    step ?? stepFor(rangeSeconds),
                )
                .then((response) => ({ response, window }));
        },
        [testids.join(","), metric, rangeSeconds, step, endSeconds],
        // A fixed-in-the-past window never gains new data - don't poll it.
        endSeconds !== undefined ? 0 : refreshMs,
    );

    const data = windowed?.response;
    const unit = data?.metric.unit ?? "";
    const label = data?.metric.label ?? metric;
    const theme = useChartTheme();
    const targetKey = targets?.join(",");
    const datasets = useMemo(
        () => rangeDatasets(onlyTargets(data, targets), theme.dark),
        // targetKey, not targets: a fresh array literal each render would rebuild every time.
        // oxlint-disable-next-line react-hooks/exhaustive-deps
        [data, targetKey, theme.dark],
    );
    return (
        <ChartPanel
            title={label}
            unit={unit}
            datasets={datasets}
            theme={theme}
            window={windowed?.window}
            error={error}
            height={height}
        />
    );
}

/** MetricsGrid charts every available metric for the given tests. */
export function MetricsGrid({
    testids,
    targets,
    endSeconds,
    rangeSeconds,
    height,
    wide,
}: {
    testids: string[];
    /** Only chart these k6 targets (default: every target of the tests). */
    targets?: string[];
    endSeconds?: number;
    rangeSeconds?: number;
    height?: number;
    /** Fewer, wider charts per row. */
    wide?: boolean;
}) {
    const metrics = useAvailableMetrics(testids);
    return (
        <div className={wide ? "grid2 wide" : "grid2"}>
            {metrics.map((m) => (
                <MetricsChart
                    key={m.key}
                    metric={m.key}
                    testids={testids}
                    targets={targets}
                    endSeconds={endSeconds}
                    rangeSeconds={rangeSeconds}
                    height={height}
                />
            ))}
        </div>
    );
}

/** ChartPanel is one titled line chart, or a placeholder without data. */
function ChartPanel({
    title,
    unit,
    datasets,
    theme,
    window,
    error,
    height,
}: {
    title: string;
    unit: string;
    datasets: ReturnType<typeof rangeDatasets>;
    theme: ChartTheme;
    window?: ChartWindow;
    error?: string;
    height: number;
}) {
    const hasData = datasets.some((d) => d.data.length > 0);
    const options = useMemo(
        () => rangeChartOptions(unit, datasets.length > 1, theme, window),
        [unit, datasets.length, theme, window],
    );
    return (
        <div className="panel">
            <h3>{title}</h3>
            {error && <div className="alert error">{error}</div>}
            {!hasData ? (
                <div className="muted" style={{ height, display: "grid", placeItems: "center" }}>
                    No data in this window yet.
                </div>
            ) : (
                <div style={{ height }}>
                    <Line data={{ datasets }} options={options} />
                </div>
            )}
        </div>
    );
}

interface AverageColumn {
    label: string;
    unit: string;
    better: "higher" | "lower";
}

interface AverageRow {
    key: string;
    label: string;
    /** One per column. */
    values: (number | undefined)[];
}

/**
 * AverageTable is a section's charts boiled down to one average per line
 * over the window, each with its change relative to the baseline row (the
 * first row unless baseline names another).
 */
function AverageTable({
    columns,
    rows,
    baseline,
}: {
    columns: AverageColumn[];
    rows: AverageRow[];
    baseline?: string;
}) {
    if (rows.length === 0) {
        return null;
    }
    const base = rows.find((r) => r.key === baseline) ?? rows[0];
    return (
        <div
            className="panel table-scroll"
            style={{ marginBottom: "var(--ak-global--spacer--md)" }}
        >
            <h3>Averages over the window</h3>
            <table className="table">
                <thead>
                    <tr>
                        <th />
                        {columns.map((c) => (
                            <th key={c.label} className="num">
                                {c.label}
                            </th>
                        ))}
                    </tr>
                </thead>
                <tbody>
                    {rows.map((row) => (
                        <tr key={row.key}>
                            <td>
                                {row.label}
                                {rows.length > 1 && row === base && (
                                    <span className="muted"> (baseline)</span>
                                )}
                            </td>
                            {columns.map((c, i) => (
                                <td key={c.label} className="mono num">
                                    {formatMetricValue(row.values[i], c.unit)}
                                    {rows.length > 1 && row !== base && (
                                        <>
                                            {" "}
                                            <Delta
                                                value={relativeDelta(row.values[i], base.values[i])}
                                                better={c.better}
                                            />
                                        </>
                                    )}
                                </td>
                            ))}
                        </tr>
                    ))}
                </tbody>
            </table>
        </div>
    );
}

const RESOURCE_CHARTS = [
    { resource: "cpu", component: "server" },
    { resource: "cpu", component: "worker" },
    { resource: "memory", component: "server" },
    { resource: "memory", component: "worker" },
] as const;

/**
 * ResourceCharts renders four big charts - CPU and memory, each for
 * authentik's server and worker pods - with one line per given test and
 * target, under a table of each line's average. Each line is that test's
 * own deployment summed across its pods (internal/api/podmetrics.go), not
 * broken out per pod: a deployment mid-rollout leaves its old ReplicaSet's
 * pods in the same window as the new one, turning one test into 6+ tangled
 * lines.
 */
export function ResourceCharts({
    testids,
    endSeconds,
    rangeSeconds = 900,
    wide,
    baseline,
    refreshMs = 15000,
}: {
    testids: string[];
    endSeconds?: number;
    rangeSeconds?: number;
    /** Fewer, wider charts per row. */
    wide?: boolean;
    /** The k6 target the averages' Δ is relative to (default: the first line). */
    baseline?: string;
    refreshMs?: number;
}) {
    const { data, error } = useResource(
        () => {
            if (testids.length === 0) {
                return Promise.resolve(undefined);
            }
            const end = endSeconds ?? Math.floor(Date.now() / 1000);
            const window = { start: end - rangeSeconds, end };
            return Promise.all(
                RESOURCE_CHARTS.map(({ resource, component }) =>
                    api.resourceRange(
                        testids,
                        resource,
                        window.start,
                        window.end,
                        stepFor(rangeSeconds),
                        component,
                    ),
                ),
            ).then((responses) => ({ responses, window }));
        },
        [testids.join(","), rangeSeconds, endSeconds],
        endSeconds !== undefined ? 0 : refreshMs,
    );
    const theme = useChartTheme();
    const charts = useMemo(
        () =>
            RESOURCE_CHARTS.map(({ resource, component }, i) => {
                const response = data?.responses[i];
                return {
                    title: `${response?.metric.label ?? resource} · ${component}`,
                    unit: response?.metric.unit ?? "",
                    series: response?.series ?? [],
                    datasets: rangeDatasets(response, theme.dark),
                };
            }),
        [data, theme.dark],
    );

    // One row per (testid, target) line, labeled as the charts' legend.
    const lines = charts[0].series;
    const labels = charts[0].datasets.map((d) => d.label);
    const rows = lines.map((line, i) => ({
        key: `${line.testid}::${line.target}`,
        label: labels[i],
        values: charts.map((c) =>
            seriesAverage(
                c.series.filter((s) => s.testid === line.testid && s.target === line.target),
            ),
        ),
    }));
    return (
        <>
            {error && <div className="alert error">{error}</div>}
            <AverageTable
                columns={charts.map((c) => ({ label: c.title, unit: c.unit, better: "lower" }))}
                rows={rows}
                baseline={baseline && rows[lines.findIndex((l) => l.target === baseline)]?.key}
            />
            <div className={wide ? "grid2 wide" : "grid2"}>
                {charts.map((c) => (
                    <ChartPanel
                        key={c.title}
                        title={c.title}
                        unit={c.unit}
                        datasets={c.datasets}
                        theme={theme}
                        window={data?.window}
                        height={320}
                    />
                ))}
            </div>
        </>
    );
}

/** deploymentDatasets is one line per series, labeled by its target. */
function deploymentDatasets(data: RangeResponse | undefined, dark: boolean) {
    return (data?.series ?? []).map((s, i) => ({
        label: s.target,
        data: (s.points ?? []).map((p) => ({ x: p.time, y: p.value })),
        borderColor: colorFor(i, dark),
        backgroundColor: colorFor(i, dark),
        spanGaps: true,
    }));
}

/**
 * DeploymentChart is one chart of a deployment's own components (postgres,
 * monitoring), one line per series the backend returns, labelled by the
 * series' target - not tied to any test. fetch runs the range query for
 * the window.
 */
function DeploymentChart({
    deployment,
    metric,
    fetch,
    rangeSeconds = 900,
    refreshMs = 15000,
    height = 260,
    endSeconds,
}: {
    deployment: string;
    metric: string;
    fetch: (start: number, end: number, step: string) => Promise<RangeResponse>;
    rangeSeconds?: number;
    refreshMs?: number;
    height?: number;
    endSeconds?: number;
}) {
    const { data: windowed, error } = useResource<WindowedRange>(
        () => {
            const end = endSeconds ?? Math.floor(Date.now() / 1000);
            const window = { start: end - rangeSeconds, end };
            return fetch(window.start, window.end, stepFor(rangeSeconds)).then((response) => ({
                response,
                window,
            }));
        },
        // Not fetch: a fresh closure every render. deployment and metric are what it closes over.
        [deployment, metric, rangeSeconds, endSeconds],
        endSeconds !== undefined ? 0 : refreshMs,
    );

    const data = windowed?.response;
    const theme = useChartTheme();
    const datasets = useMemo(() => deploymentDatasets(data, theme.dark), [data, theme.dark]);
    return (
        <ChartPanel
            title={data?.metric.label ?? metric}
            unit={data?.metric.unit ?? ""}
            datasets={datasets}
            theme={theme}
            window={windowed?.window}
            error={error}
            height={height}
        />
    );
}

/**
 * mergeDeployments folds one response per deployment into one response,
 * each line prefixed with its deployment when there's more than one.
 */
function mergeDeployments(
    responses: RangeResponse[],
    deployments: string[],
): RangeResponse | undefined {
    return (
        responses[0] && {
            ...responses[0],
            series: responses.flatMap((r, i) =>
                deployments.length > 1
                    ? r.series.map((s) => ({ ...s, target: `${deployments[i]} · ${s.target}` }))
                    : r.series,
            ),
        }
    );
}

const POSTGRES_METRICS: PostgresMetric[] = ["cpu", "memory", "disk", "io"];

/**
 * PostgresCharts is the postgres CPU, memory, disk usage and disk IO of one
 * or more deployments, all on the same charts. With more than one
 * deployment each line is prefixed with its deployment.
 */
export function PostgresCharts({
    deployments,
    rangeSeconds = 900,
    endSeconds,
    refreshMs = 15000,
}: {
    deployments: string[];
    rangeSeconds?: number;
    endSeconds?: number;
    refreshMs?: number;
}) {
    const { data, error } = useResource(
        () => {
            if (deployments.length === 0) {
                return Promise.resolve(undefined);
            }
            const end = endSeconds ?? Math.floor(Date.now() / 1000);
            const window = { start: end - rangeSeconds, end };
            // byMetric[m][d]: deployment d's response for POSTGRES_METRICS[m].
            return Promise.all(
                POSTGRES_METRICS.map((metric) =>
                    Promise.all(
                        deployments.map((d) =>
                            api.postgresMetrics(
                                d,
                                metric,
                                window.start,
                                window.end,
                                stepFor(rangeSeconds),
                            ),
                        ),
                    ),
                ),
            ).then((byMetric) => ({ byMetric, window }));
        },
        [deployments.join(","), rangeSeconds, endSeconds],
        endSeconds !== undefined ? 0 : refreshMs,
    );
    const theme = useChartTheme();
    const charts = useMemo(
        () =>
            POSTGRES_METRICS.map((metric, m) => {
                const merged = mergeDeployments(data?.byMetric[m] ?? [], deployments);
                return {
                    metric,
                    title: merged?.metric.label ?? metric,
                    unit: merged?.metric.unit ?? "",
                    datasets: deploymentDatasets(merged, theme.dark),
                };
            }),
        // deployments is what data was fetched for.
        // oxlint-disable-next-line react-hooks/exhaustive-deps
        [data, theme.dark],
    );
    if (deployments.length === 0) {
        return null;
    }
    return (
        <>
            {error && <div className="alert error">{error}</div>}
            <div className="grid2">
                {charts.map((c) => (
                    <ChartPanel
                        key={c.metric}
                        title={c.title}
                        unit={c.unit}
                        datasets={c.datasets}
                        theme={theme}
                        window={data?.window}
                        height={260}
                    />
                ))}
            </div>
        </>
    );
}

const WORKER_METRICS: WorkerMetric[] = [
    "tasks",
    "duration",
    "queue",
    "failures",
    "db_queries",
    "db_duration",
];

/**
 * WORKER_AVERAGES are the worker averages' columns: a metric's series by
 * name, or every series of it summed (db_queries, one per pod).
 */
const WORKER_AVERAGES: {
    metric: WorkerMetric;
    series?: string;
    label: string;
    better: "higher" | "lower";
}[] = [
    { metric: "tasks", series: "processed", label: "Tasks", better: "higher" },
    { metric: "duration", series: "p95", label: "Task duration p95", better: "lower" },
    { metric: "queue", series: "queued", label: "Queued", better: "lower" },
    { metric: "failures", series: "errors", label: "Task errors", better: "lower" },
    { metric: "db_queries", label: "Database queries", better: "lower" },
    { metric: "db_duration", series: "p95", label: "Database query p95", better: "lower" },
];

/**
 * WorkerCharts is the authentik worker of one or more deployments, all on
 * the same charts: background tasks processed and their duration, the
 * queue, task failures, and the database queries the worker runs - under a
 * table of each deployment's averages. With more than one deployment each
 * line is prefixed with its deployment.
 */
export function WorkerCharts({
    deployments,
    rangeSeconds = 900,
    endSeconds,
    baseline,
    refreshMs = 15000,
}: {
    deployments: string[];
    rangeSeconds?: number;
    endSeconds?: number;
    /** The deployment the averages' Δ is relative to (default: the first). */
    baseline?: string;
    refreshMs?: number;
}) {
    const { data, error } = useResource(
        () => {
            if (deployments.length === 0) {
                return Promise.resolve(undefined);
            }
            const end = endSeconds ?? Math.floor(Date.now() / 1000);
            const window = { start: end - rangeSeconds, end };
            // byDeployment[d][m]: deployment d's response for WORKER_METRICS[m].
            return Promise.all(
                deployments.map((d) =>
                    Promise.all(
                        WORKER_METRICS.map((metric) =>
                            api.workerMetrics(
                                d,
                                metric,
                                window.start,
                                window.end,
                                stepFor(rangeSeconds),
                            ),
                        ),
                    ),
                ),
            ).then((byDeployment) => ({ byDeployment, window }));
        },
        [deployments.join(","), rangeSeconds, endSeconds],
        endSeconds !== undefined ? 0 : refreshMs,
    );
    const theme = useChartTheme();
    const charts = useMemo(
        () =>
            WORKER_METRICS.map((metric, m) => {
                const responses = (data?.byDeployment ?? []).map((r) => r[m]);
                const merged = mergeDeployments(responses, deployments);
                return {
                    metric,
                    title: merged?.metric.label ?? metric,
                    unit: merged?.metric.unit ?? "",
                    datasets: deploymentDatasets(merged, theme.dark),
                };
            }),
        // deployments is what data was fetched for.
        // oxlint-disable-next-line react-hooks/exhaustive-deps
        [data, theme.dark],
    );
    if (deployments.length === 0) {
        return null;
    }

    const rows = (data?.byDeployment ?? []).map((responses, i) => ({
        key: deployments[i],
        label: deployments[i],
        values: WORKER_AVERAGES.map(({ metric, series }) =>
            seriesAverage(
                responses[WORKER_METRICS.indexOf(metric)].series.filter(
                    (s) => series === undefined || s.target === series,
                ),
            ),
        ),
    }));
    return (
        <>
            {error && <div className="alert error">{error}</div>}
            <AverageTable
                columns={WORKER_AVERAGES.map((a) => ({
                    label: a.label,
                    unit: charts[WORKER_METRICS.indexOf(a.metric)].unit,
                    better: a.better,
                }))}
                rows={rows}
                baseline={baseline}
            />
            <div className="grid2">
                {charts.map((c) => (
                    <ChartPanel
                        key={c.metric}
                        title={c.title}
                        unit={c.unit}
                        datasets={c.datasets}
                        theme={theme}
                        window={data?.window}
                        height={260}
                    />
                ))}
            </div>
            <WorkerActorTable
                deployments={deployments}
                rangeSeconds={rangeSeconds}
                endSeconds={endSeconds}
            />
        </>
    );
}

/** One actor's row: its WorkerActor per deployment, in deployment order. */
interface WorkerActorRow {
    actor: string;
    byDeployment: (WorkerActor | undefined)[];
}

/**
 * WorkerActorTable is the deployments' busiest worker actors over the
 * window, by tasks processed - the per-actor breakdown the charts sum away.
 * One row per actor, with each deployment's numbers side by side.
 */
function WorkerActorTable({
    deployments,
    rangeSeconds = 900,
    endSeconds,
    refreshMs = 15000,
}: {
    deployments: string[];
    rangeSeconds?: number;
    endSeconds?: number;
    refreshMs?: number;
}) {
    const { data, error } = useResource<WorkerActorRow[]>(
        () => {
            const end = endSeconds ?? Math.floor(Date.now() / 1000);
            return Promise.all(
                deployments.map((d) => api.workerActors(d, end - rangeSeconds, end)),
            ).then((lists) =>
                topActors(lists, WORKER_ACTOR_LIMIT).map((actor) => ({
                    actor,
                    byDeployment: lists.map((l) => l.find((a) => a.actor === actor)),
                })),
            );
        },
        [deployments.join(","), rangeSeconds, endSeconds],
        endSeconds !== undefined ? 0 : refreshMs,
    );
    const suffix = (d: string) => (deployments.length > 1 ? ` · ${d}` : "");
    const columns: Column<WorkerActorRow>[] = [
        { label: "Actor", render: (r) => <span className="mono">{r.actor}</span> },
        ...deployments.flatMap((d, i): Column<WorkerActorRow>[] => [
            // increase() extrapolates, so counts come back fractional.
            {
                label: `Tasks${suffix(d)}`,
                render: (r) => formatCount(r.byDeployment[i]?.tasks),
            },
            {
                label: `p95${suffix(d)}`,
                render: (r) => formatMetricValue(r.byDeployment[i]?.p95, "s"),
            },
            {
                label: `Errors${suffix(d)}`,
                render: (r) => formatCount(r.byDeployment[i]?.errors),
            },
        ]),
    ];
    return (
        <div className="panel" style={{ marginTop: "var(--ak-global--spacer--md)" }}>
            <h3>Busiest worker actors</h3>
            {error && <div className="alert error">{error}</div>}
            <div className="table-scroll">
                <Table
                    rows={data}
                    rowKey={(r) => r.actor}
                    empty="No tasks in this window."
                    columns={columns}
                />
            </div>
        </div>
    );
}

/** How many actors WorkerActorTable lists - internal/api's workerActorLimit. */
const WORKER_ACTOR_LIMIT = 10;

const formatCount = (n: number | undefined) =>
    n === undefined ? "-" : Math.round(n).toLocaleString();

/**
 * MonitoringCharts is a workload cluster's observability stack (CPU, memory,
 * disk) and its nodes' CPU, memory, disk IO and CPU steal.
 */
export function MonitoringCharts({ cluster }: { cluster: string }) {
    const metrics: MonitoringMetric[] = [
        "cpu",
        "memory",
        "disk",
        "node-cpu",
        "node-memory",
        "node-io",
        "node-steal",
    ];
    return (
        <div className="grid2 wide">
            {metrics.map((metric) => (
                <DeploymentChart
                    key={metric}
                    deployment={cluster}
                    metric={metric}
                    fetch={(start, end, step) =>
                        api.monitoringMetrics(cluster, metric, start, end, step)
                    }
                    height={360}
                />
            ))}
        </div>
    );
}
