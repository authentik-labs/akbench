import { api } from "../api";
import { colorFor, formatMetricValue, onlyTargets, stepFor } from "../format";
import { useDarkTheme } from "../theme";
import {
    CompareMetric,
    MonitoringMetric,
    PostgresMetric,
    RangeResponse,
    WorkerMetric,
} from "../types";
import { useResource } from "../useResource";

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
    const hasData = datasets.some((d) => d.data.length > 0);
    const options = useMemo(
        () => rangeChartOptions(unit, datasets.length > 1, theme, windowed?.window),
        [unit, datasets.length, theme, windowed?.window],
    );

    return (
        <div className="panel">
            <h3>{label}</h3>
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

/**
 * ResourceChart is one big chart for one resource (cpu/memory), one line per
 * *test* - each line is that test's own deployment summed across its pods
 * (internal/api/podmetrics.go), not broken out per pod: confirmed live that
 * reads as unreadable (a deployment mid-rollout leaves its old ReplicaSet
 * generation's pods in the same time window as the new one, turning a
 * single test into 6+ tangled lines). Matches MetricsChart's own "one line
 * per test" shape above - this is the Compare page's resource-usage
 * equivalent of the k6-metric charts, not a per-pod drill-down.
 */
export function ResourceChart({
    resource,
    component,
    testids,
    rangeSeconds = 900,
    step,
    refreshMs = 15000,
    height = 320,
    endSeconds,
}: {
    resource: "cpu" | "memory";
    /** Only this authentik process's pods (default: every pod of the deployment). */
    component?: "server" | "worker";
    testids: string[];
    rangeSeconds?: number;
    /** Defaults to stepFor(rangeSeconds). */
    step?: string;
    refreshMs?: number;
    height?: number;
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
                .resourceRange(
                    testids,
                    resource,
                    window.start,
                    window.end,
                    step ?? stepFor(rangeSeconds),
                    component,
                )
                .then((response) => ({ response, window }));
        },
        [testids.join(","), resource, component, rangeSeconds, step, endSeconds],
        endSeconds !== undefined ? 0 : refreshMs,
    );

    const data = windowed?.response;
    const unit = data?.metric.unit ?? "";
    const resourceLabel = data?.metric.label ?? resource;
    const label = component ? `${resourceLabel} · ${component}` : resourceLabel;
    const theme = useChartTheme();
    const datasets = useMemo(() => rangeDatasets(data, theme.dark), [data, theme.dark]);
    const hasData = datasets.some((d) => d.data.length > 0);
    const options = useMemo(
        () => rangeChartOptions(unit, datasets.length > 1, theme, windowed?.window),
        [unit, datasets.length, theme, windowed?.window],
    );

    return (
        <div className="panel">
            <h3>{label}</h3>
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

/**
 * ResourceCharts renders four big charts - CPU and memory, each for
 * authentik's server and worker pods - with one line per given test and
 * target.
 */
export function ResourceCharts({
    testids,
    endSeconds,
    rangeSeconds,
    wide,
}: {
    testids: string[];
    endSeconds?: number;
    rangeSeconds?: number;
    /** Fewer, wider charts per row. */
    wide?: boolean;
}) {
    const charts = [
        { resource: "cpu", component: "server" },
        { resource: "cpu", component: "worker" },
        { resource: "memory", component: "server" },
        { resource: "memory", component: "worker" },
    ] as const;
    return (
        <div className={wide ? "grid2 wide" : "grid2"}>
            {charts.map(({ resource, component }) => (
                <ResourceChart
                    key={`${resource}-${component}`}
                    resource={resource}
                    component={component}
                    testids={testids}
                    endSeconds={endSeconds}
                    rangeSeconds={rangeSeconds}
                />
            ))}
        </div>
    );
}

/**
 * DeploymentChart is one chart of a deployment's own components (postgres,
 * worker), one line per series the backend returns, labelled by the
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
    const unit = data?.metric.unit ?? "";
    const theme = useChartTheme();
    const datasets = useMemo(
        () =>
            (data?.series ?? []).map((s, i) => ({
                label: s.target,
                data: (s.points ?? []).map((p) => ({ x: p.time, y: p.value })),
                borderColor: colorFor(i, theme.dark),
                backgroundColor: colorFor(i, theme.dark),
                spanGaps: true,
            })),
        [data, theme.dark],
    );
    const hasData = datasets.some((d) => d.data.length > 0);
    const options = useMemo(
        () => rangeChartOptions(unit, datasets.length > 1, theme, windowed?.window),
        [unit, datasets.length, theme, windowed?.window],
    );

    return (
        <div className="panel">
            <h3>{data?.metric.label ?? metric}</h3>
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

/** PostgresCharts is a deployment's postgres CPU, memory, disk usage and disk IO. */
export function PostgresCharts({
    deployment,
    rangeSeconds,
}: {
    deployment: string;
    rangeSeconds?: number;
}) {
    const metrics: PostgresMetric[] = ["cpu", "memory", "disk", "io"];
    return (
        <div className="grid2">
            {metrics.map((metric) => (
                <DeploymentChart
                    key={metric}
                    deployment={deployment}
                    metric={metric}
                    fetch={(start, end, step) =>
                        api.postgresMetrics(deployment, metric, start, end, step)
                    }
                    rangeSeconds={rangeSeconds}
                />
            ))}
        </div>
    );
}

/**
 * WorkerCharts is a deployment's authentik worker: background tasks
 * processed and their p95 duration per actor, the queue, task failures, and
 * the database queries the worker runs.
 */
export function WorkerCharts({
    deployment,
    rangeSeconds,
    endSeconds,
}: {
    deployment: string;
    rangeSeconds?: number;
    endSeconds?: number;
}) {
    const metrics: WorkerMetric[] = [
        "tasks",
        "duration",
        "queue",
        "failures",
        "db_queries",
        "db_duration",
    ];
    return (
        <div className="grid2">
            {metrics.map((metric) => (
                <DeploymentChart
                    key={metric}
                    deployment={deployment}
                    metric={metric}
                    fetch={(start, end, step) =>
                        api.workerMetrics(deployment, metric, start, end, step)
                    }
                    rangeSeconds={rangeSeconds}
                    endSeconds={endSeconds}
                />
            ))}
        </div>
    );
}

/** MonitoringCharts is a workload cluster's observability stack: CPU, memory and disk. */
export function MonitoringCharts({ cluster }: { cluster: string }) {
    const metrics: MonitoringMetric[] = ["cpu", "memory", "disk"];
    return (
        <div className="grid2">
            {metrics.map((metric) => (
                <DeploymentChart
                    key={metric}
                    deployment={cluster}
                    metric={metric}
                    fetch={(start, end, step) =>
                        api.monitoringMetrics(cluster, metric, start, end, step)
                    }
                />
            ))}
        </div>
    );
}
