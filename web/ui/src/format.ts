/**
 * formatMetricValue renders a CompareMetric value the same way in the
 * compare table and every chart tooltip/axis, so a number means the same
 * thing wherever it appears.
 */
export function formatMetricValue(value: number | undefined, unit: string): string {
    if (value === undefined) {
        return "-";
    }
    if (unit === "ratio") {
        return `${(value * 100).toFixed(1)}%`;
    }
    if (unit === "s") {
        return value < 1 ? `${(value * 1000).toFixed(0)} ms` : `${value.toFixed(2)} s`;
    }
    if (unit === "cores") {
        return value < 1 ? `${Math.round(value * 1000)}m` : `${value.toFixed(2)} cores`;
    }
    if (unit === "bytes") {
        return formatBytes(value);
    }
    if (unit === "bytes/s") {
        return `${formatBytes(value)}/s`;
    }
    return value >= 100 ? value.toFixed(0) : value.toFixed(2);
}

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB"];

function formatBytes(value: number): string {
    let scaled = value;
    let unitIndex = 0;
    while (Math.abs(scaled) >= 1024 && unitIndex < BYTE_UNITS.length - 1) {
        scaled /= 1024;
        unitIndex++;
    }
    return `${scaled.toFixed(unitIndex === 0 ? 0 : 1)} ${BYTE_UNITS[unitIndex]}`;
}

/**
 * formatDuration renders an elapsed number of seconds as its two largest
 * units ("3d 4h", "19h 12m", "7m") - how long a Burn test has been running.
 */
export function formatDuration(seconds: number): string {
    const s = Math.max(0, Math.floor(seconds));
    if (s < 60) {
        return `${s}s`;
    }
    const minutes = Math.floor(s / 60);
    if (minutes < 60) {
        return `${minutes}m`;
    }
    const hours = Math.floor(minutes / 60);
    if (hours < 24) {
        return `${hours}h ${minutes % 60}m`;
    }
    return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

/**
 * relativeDelta is value's change relative to baseline (0.1 = 10% higher),
 * or undefined when either side is missing or the baseline is zero.
 */
export function relativeDelta(
    value: number | undefined,
    baseline: number | undefined,
): number | undefined {
    if (value === undefined || baseline === undefined || baseline === 0) {
        return undefined;
    }
    return (value - baseline) / baseline;
}

/**
 * stepFor picks a range query step that keeps a chart around 240 points,
 * never finer than 15s - a 7 day range at a fixed 15s step would be ~40k
 * points per series, past Prometheus' own 11k-point limit.
 */
export function stepFor(rangeSeconds: number): string {
    return `${Math.max(15, Math.ceil(rangeSeconds / 240))}s`;
}

/**
 * shortTag abbreviates an image tag that ends in a full git commit hash
 * ("gh-ccd5f45863edfb003c4dd876fabfcb...") to its first 7 characters, the
 * way git itself does; any other tag is returned unchanged.
 */
export function shortTag(tag: string): string {
    const match = /^(.*?)([0-9a-f]{40})$/.exec(tag);
    return match ? `${match[1]}${match[2].slice(0, 7)}` : tag;
}

/**
 * targetNameOf is the k6 `target` tag a deployment's own metrics carry -
 * mirrors internal/api's compareTargetsForTestID: spec.targetName, falling
 * back to the object name.
 */
export function targetNameOf(deployment: {
    metadata: { name: string };
    spec: { targetName?: string };
}): string {
    return deployment.spec.targetName || deployment.metadata.name;
}

/**
 * onlyTargets keeps just the series for the given k6 targets - a TestCase
 * driving several deployments (e.g. a PR comparison) returns one series per
 * target, and a single deployment's page must not show its siblings'.
 * Undefined keeps everything.
 */
export function onlyTargets<T extends { series: { target: string }[] }>(
    data: T | undefined,
    targets?: string[],
): T | undefined {
    if (!data || !targets) {
        return data;
    }
    return { ...data, series: data.series.filter((s) => targets.includes(s.target)) };
}

// A small qualitative palette, cycled by index so the same test keeps the
// same color across every chart on a page. One variant per theme: each
// color holds at least 3:1 against its own theme's surface, which a single
// palette can't do for both a white and a near-black background.
const palettes = {
    light: ["#0066cc", "#3e8635", "#a36a00", "#c9190b", "#6753ac", "#007a7b", "#c46100", "#4f7c00"],
    dark: ["#2b9af3", "#5ba352", "#f0ab00", "#fe5142", "#a18fff", "#2fb5b6", "#ec7a08", "#8bc34a"],
};

export function colorFor(index: number, dark = false): string {
    const palette = dark ? palettes.dark : palettes.light;
    return palette[index % palette.length];
}

function relativeLuminance(hex: string): number {
    const channels = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
    const [r, g, b] = channels.map((c) =>
        c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4,
    );
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/**
 * readableTextOn picks near-black or white text for a #rrggbb background,
 * whichever contrasts more - white letters on the palette's yellow or green
 * are unreadable.
 */
export function readableTextOn(hex: string): string {
    const l = relativeLuminance(hex);
    const onWhite = 1.05 / (l + 0.05);
    const onInk = (l + 0.05) / (relativeLuminance("#151515") + 0.05);
    return onWhite >= onInk ? "#ffffff" : "#151515";
}

/**
 * metricsAnchorFromRuns resolves a fixed-in-the-past metrics window from a
 * Quick TestCase's own run history, instead of the usual "trailing window
 * ending at now" - a Quick TestCase's k6 Job exits once its run completes,
 * so a window anchored to "now" only ever shows data for as long as the
 * run's own samples stay within that trailing window (Prometheus' own
 * ~5-minute staleness lookback for an instant query, or metricsRange's own
 * rangeSeconds), then silently goes empty even though the run itself
 * succeeded and its data is still in Prometheus.
 *
 * Returns undefined (meaning "use now", MetricsChart's own default) when the
 * latest run is still in flight (no completionTime yet) or there's no run
 * history at all - both cases where "now" is already the right anchor.
 */
export function metricsAnchorFromRuns(
    runs: { status?: { startTime?: string; completionTime?: string } }[],
): { endSeconds: number; rangeSeconds: number } | undefined {
    const completed = runs
        .filter((r) => r.status?.completionTime)
        .sort(
            (a, b) => Date.parse(b.status!.completionTime!) - Date.parse(a.status!.completionTime!),
        );
    const latest = completed[0];
    if (!latest?.status?.completionTime) {
        return undefined;
    }
    // A short buffer past completion, so the run's own last few samples
    // (k6's final Prometheus remote-write push can trail completion slightly)
    // are still inside the window.
    const endSeconds = Math.floor(Date.parse(latest.status.completionTime) / 1000) + 60;
    const startSeconds = latest.status.startTime
        ? Math.floor(Date.parse(latest.status.startTime) / 1000)
        : endSeconds - 900;
    const rangeSeconds = Math.max(900, endSeconds - startSeconds + 120);
    return { endSeconds, rangeSeconds };
}

/**
 * buildLabel shows the version, plus the short commit unless the version
 * already carries it (main builds are versioned 0.0.0-main.<sha>).
 */
export function buildLabel(info: { version: string; commit?: string; dirty?: boolean }): string {
    const short = info.commit?.slice(0, 7);
    let label = info.version;
    if (short && !label.includes(short)) {
        label = `${label} · ${short}`;
    }
    return info.dirty ? `${label}-dirty` : label;
}

/** TimeRange is one entry of the chart time range picker. */
export interface TimeRange {
    value: string;
    label: string;
    seconds: number;
}

/** TIME_RANGES are Grafana's relative "Last ..." quick ranges, up to 90 days. */
export const TIME_RANGES: TimeRange[] = [
    { value: "5m", label: "Last 5 minutes", seconds: 300 },
    { value: "15m", label: "Last 15 minutes", seconds: 900 },
    { value: "30m", label: "Last 30 minutes", seconds: 1800 },
    { value: "1h", label: "Last 1 hour", seconds: 3600 },
    { value: "3h", label: "Last 3 hours", seconds: 10800 },
    { value: "6h", label: "Last 6 hours", seconds: 21600 },
    { value: "12h", label: "Last 12 hours", seconds: 43200 },
    { value: "24h", label: "Last 24 hours", seconds: 86400 },
    { value: "2d", label: "Last 2 days", seconds: 172800 },
    { value: "7d", label: "Last 7 days", seconds: 604800 },
    { value: "30d", label: "Last 30 days", seconds: 2592000 },
    { value: "90d", label: "Last 90 days", seconds: 7776000 },
];

/** SINCE_START is the range from a test's (or burn's) start until now. */
export const SINCE_START = "start";

/**
 * parseTimeRange reads a ?range= value: a TIME_RANGES value, SINCE_START
 * (only while startSeconds is known), or the number of seconds older links
 * carry. Anything else is fallback.
 */
export function parseTimeRange(
    param: string | null,
    fallback: string,
    startSeconds?: number,
): string {
    if (!param) {
        return fallback;
    }
    if (param === SINCE_START) {
        return startSeconds === undefined ? fallback : param;
    }
    if (TIME_RANGES.some((r) => r.value === param)) {
        return param;
    }
    return TIME_RANGES.find((r) => String(r.seconds) === param)?.value ?? fallback;
}

/**
 * timeRangeSeconds resolves a range to the seconds the charts span, ending
 * at endSeconds. SINCE_START spans from startSeconds rounded down to the
 * minute, so the window - and with it every chart's query - doesn't change on
 * every render. Rounding down rather than up keeps the window from opening
 * before k6's first samples land: startSeconds is when its pod was created,
 * and scheduling, startup and the first remote-write push all come after.
 * Without a known start it falls back to the shortest range.
 */
export function timeRangeSeconds(
    value: string,
    startSeconds?: number,
    endSeconds = Date.now() / 1000,
): number {
    if (value === SINCE_START) {
        if (startSeconds === undefined) {
            return TIME_RANGES[0].seconds;
        }
        return Math.max(60, Math.floor((endSeconds - startSeconds) / 60) * 60);
    }
    return TIME_RANGES.find((r) => r.value === value)?.seconds ?? TIME_RANGES[0].seconds;
}

/**
 * earliestBurnStart is when the earliest of the given tests' Burn
 * Deployments started, optionally only those driving one deployment - in
 * unix seconds, or undefined if none is burning. A burn archiving already
 * stopped doesn't count.
 */
export function earliestBurnStart(
    testCases: {
        status?: {
            deployments?: { name: string; burningSince?: string; burnStoppedAt?: string }[];
        };
    }[],
    deployment?: string,
): number | undefined {
    const starts = testCases.flatMap((tc) =>
        (tc.status?.deployments ?? [])
            .filter(
                (d) =>
                    d.burningSince &&
                    !d.burnStoppedAt &&
                    (deployment === undefined || d.name === deployment),
            )
            .map((d) => Date.parse(d.burningSince!) / 1000),
    );
    return starts.length ? Math.min(...starts) : undefined;
}

/**
 * archivedBurnWindow is when an archived test burned, in unix seconds: from
 * its earliest target's burn start to its latest burn stop. Test cases
 * archived before the operator kept burningSince only know the end, taken
 * from when their Ready condition turned Archived. Undefined for a test
 * that isn't archived, or whose end isn't known.
 */
export function archivedBurnWindow(tc: {
    spec: { archived?: boolean };
    status?: {
        conditions?: { type: string; reason?: string; lastTransitionTime?: string }[];
        deployments?: { burningSince?: string; burnStoppedAt?: string }[];
    };
}): { start?: number; end: number } | undefined {
    if (!tc.spec.archived) {
        return undefined;
    }
    const deployments = tc.status?.deployments ?? [];
    const seconds = (t: string) => Date.parse(t) / 1000;
    const starts = deployments.flatMap((d) => (d.burningSince ? [seconds(d.burningSince)] : []));
    const stops = deployments.flatMap((d) => (d.burnStoppedAt ? [seconds(d.burnStoppedAt)] : []));
    if (stops.length === 0) {
        const archivedAt = tc.status?.conditions?.find(
            (c) => c.type === "Ready" && c.reason === "Archived",
        )?.lastTransitionTime;
        if (archivedAt) {
            stops.push(seconds(archivedAt));
        }
    }
    if (stops.length === 0) {
        return undefined;
    }
    return {
        start: starts.length ? Math.min(...starts) : undefined,
        end: Math.max(...stops),
    };
}

/** newestFirst sorts objects by creationTimestamp, most recent first, without mutating the input. */
export function newestFirst<T extends { metadata: { creationTimestamp?: string } }>(
    items: T[],
): T[] {
    return [...items].sort((a, b) =>
        (b.metadata.creationTimestamp ?? "").localeCompare(a.metadata.creationTimestamp ?? ""),
    );
}
