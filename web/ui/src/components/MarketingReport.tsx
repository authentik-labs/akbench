// MarketingReport turns a test case's (or a matrix's test cases') headline
// numbers into one stylized 1920x1080 image for sharing: a big number and a
// chart per picked metric, drawn straight onto a canvas so the preview is
// exactly the exported PNG.
import { api } from "../api";
import {
    colorFor,
    formatMetricValue,
    palettes,
    relativeDelta,
    seriesAverage,
    smoothPoints,
    stepFor,
} from "../format";
import { RangeResponse } from "../types";
import { useResource } from "../useResource";
import { toast } from "./Toast";
import { Alert, errorMessage, Segmented } from "./ui";

import { Chart as ChartJS, Filler, LinearScale, LineElement, PointElement } from "chart.js";
import { useEffect, useRef, useState } from "react";

ChartJS.register(LinearScale, PointElement, LineElement, Filler);

interface ReportMetric {
    /** A compare metric key, or for resource metrics just a unique id. */
    key: string;
    label: string;
    better: "higher" | "lower";
    resource?: "cpu" | "memory";
    component?: "server" | "worker";
}

const REPORT_METRICS: ReportMetric[] = [
    { key: "http_reqs", label: "Requests / s", better: "higher" },
    { key: "iterations", label: "Transactions / s", better: "higher" },
    { key: "http_req_duration_p95", label: "Latency p95", better: "lower" },
    { key: "http_req_duration_p99", label: "Latency p99", better: "lower" },
    { key: "iteration_duration_p95", label: "Transaction time p95", better: "lower" },
    { key: "http_req_failed", label: "Error rate", better: "lower" },
    {
        key: "cpu-server",
        label: "Server CPU",
        better: "lower",
        resource: "cpu",
        component: "server",
    },
    {
        key: "cpu-worker",
        label: "Worker CPU",
        better: "lower",
        resource: "cpu",
        component: "worker",
    },
    {
        key: "memory-server",
        label: "Server memory",
        better: "lower",
        resource: "memory",
        component: "server",
    },
    {
        key: "memory-worker",
        label: "Worker memory",
        better: "lower",
        resource: "memory",
        component: "worker",
    },
];

const DEFAULT_METRICS = ["http_reqs", "iterations", "http_req_duration_p95", "cpu-server"];
const MAX_METRICS = 6;

const W = 1920;
const H = 1080;
const PAD = 96;

const THEMES = {
    dark: {
        bg: ["#0b1020", "#1d1442"],
        panel: "rgba(255,255,255,0.06)",
        border: "rgba(255,255,255,0.12)",
        text: "#ffffff",
        muted: "rgba(255,255,255,0.6)",
        grid: "rgba(255,255,255,0.08)",
        accent: "#fd4b2d",
        good: "#5ba352",
        bad: "#fe5142",
    },
    light: {
        bg: ["#ffffff", "#eef1f8"],
        panel: "#ffffff",
        border: "rgba(0,0,0,0.08)",
        text: "#151515",
        muted: "#6a6e73",
        grid: "rgba(0,0,0,0.06)",
        accent: "#fd4b2d",
        good: "#3e8635",
        bad: "#c9190b",
    },
};
type ThemeName = keyof typeof THEMES;

/**
 * ReportRow is one series' k6 target and compare values within a source.
 * Rows line up by position across sources: position i is the same series
 * (a test case's deployment, a matrix's variant) in every one of them.
 */
export interface ReportRow {
    target: string;
    values: Record<string, number>;
}

/** ReportSource is a test case a metric can be pulled from. */
export interface ReportSource {
    testid: string;
    label: string;
    rows: ReportRow[];
}

interface Panel {
    metric: ReportMetric;
    response?: RangeResponse;
    rows: ReportRow[];
    /** The source's label, shown when there is more than one to pick from. */
    source?: string;
}

interface DrawInput {
    title: string;
    subtitle: string;
    theme: ThemeName;
    panels: Panel[];
    start: number;
    end: number;
    /** Display name per series position. */
    names: string[];
    /** Series position the deltas are relative to. */
    baseline: number;
    /** Moving-average radius, in points, applied to the chart lines only. */
    smoothing: number;
    /** Palette indexes keyed by series position; unset keeps the page's color. */
    colors: Record<number, number>;
}

/** fitFont sets the largest font up to size that fits text in maxWidth. */
function fitFont(
    ctx: CanvasRenderingContext2D,
    text: string,
    maxWidth: number,
    size: number,
    weight: number,
    family: string,
): number {
    for (; size > 12; size -= 2) {
        ctx.font = `${weight} ${size}px ${family}`;
        if (ctx.measureText(text).width <= maxWidth) {
            break;
        }
    }
    return size;
}

function drawReport(ctx: CanvasRenderingContext2D, input: DrawInput) {
    const { panels, names, start, end } = input;
    const t = THEMES[input.theme];
    const dark = input.theme === "dark";
    const family =
        getComputedStyle(document.documentElement)
            .getPropertyValue("--ak-global--font-family--heading")
            .trim() || "sans-serif";
    const colorOf = (i: number) =>
        input.colors[i] !== undefined
            ? colorFor(input.colors[i], dark)
            : names.length === 1
              ? t.accent
              : colorFor(i, dark);

    const bg = ctx.createLinearGradient(0, 0, W, H);
    bg.addColorStop(0, t.bg[0]);
    bg.addColorStop(1, t.bg[1]);
    ctx.fillStyle = bg;
    ctx.fillRect(0, 0, W, H);
    ctx.textBaseline = "alphabetic";
    ctx.textAlign = "left";

    // Legend, top right.
    if (names.length > 1) {
        let legendX = W - PAD;
        ctx.font = `600 26px ${family}`;
        for (let i = names.length - 1; i >= 0; i--) {
            legendX -= ctx.measureText(names[i]).width;
            ctx.fillStyle = t.text;
            ctx.fillText(names[i], legendX, PAD + 20);
            ctx.fillStyle = colorOf(i);
            ctx.beginPath();
            ctx.arc(legendX - 20, PAD + 11, 9, 0, Math.PI * 2);
            ctx.fill();
            legendX -= 72;
        }
    }

    ctx.fillStyle = t.accent;
    ctx.font = `700 26px ${family}`;
    ctx.fillText("AUTHENTIK BENCHMARK", PAD, PAD + 20);
    ctx.fillStyle = t.text;
    fitFont(ctx, input.title, W - 2 * PAD, 72, 700, family);
    ctx.fillText(input.title, PAD, PAD + 112);
    ctx.fillStyle = t.muted;
    fitFont(ctx, input.subtitle, W - 2 * PAD, 32, 400, family);
    ctx.fillText(input.subtitle, PAD, PAD + 164);

    ctx.font = `400 22px ${family}`;
    const fmt = (s: number) =>
        new Date(s * 1000).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
    ctx.fillText(`${fmt(start)} – ${fmt(end)}`, PAD, H - 56);
    ctx.textAlign = "right";
    ctx.fillText("goauthentik.io", W - PAD, H - 56);
    ctx.textAlign = "left";

    const n = panels.length;
    if (n === 0) {
        return;
    }
    const gap = 32;
    const top = 320;
    const bottom = H - 112;
    const cols = n <= 3 ? n : n === 4 ? 2 : 3;
    const lines = Math.ceil(n / cols);
    const pw = (W - 2 * PAD - (cols - 1) * gap) / cols;
    const ph = (bottom - top - (lines - 1) * gap) / lines;

    panels.forEach(({ metric, response, rows, source }, i) => {
        const unit = response?.metric.unit ?? "";
        const x = PAD + (i % cols) * (pw + gap);
        const y = top + Math.floor(i / cols) * (ph + gap);
        ctx.fillStyle = t.panel;
        ctx.strokeStyle = t.border;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.roundRect(x, y, pw, ph, 24);
        ctx.fill();
        ctx.stroke();

        const heading = (source ? `${metric.label} · ${source}` : metric.label).toUpperCase();
        ctx.fillStyle = t.muted;
        fitFont(ctx, heading, pw - 64, 24, 600, family);
        ctx.fillText(heading, x + 32, y + 52);

        // Compare metrics use the page's own window summary; resources
        // (which compare doesn't cover) average their range.
        const valueOf = (row: ReportRow | undefined) =>
            !row
                ? undefined
                : metric.resource
                  ? seriesAverage(response?.series.filter((s) => s.target === row.target) ?? [])
                  : row.values[metric.key];
        const baselineValue = valueOf(rows[input.baseline]);
        const colW = (pw - 64) / rows.length;
        const maxSize = Math.min(rows.length === 1 ? 96 : 60, ph / 4);
        let valuesBottom = y + 52;
        rows.forEach((row, j) => {
            const value = valueOf(row);
            const text = formatMetricValue(value, unit);
            const vx = x + 32 + j * colW;
            const size = fitFont(ctx, text, colW - 16, maxSize, 700, family);
            const vy = y + 64 + size;
            ctx.fillStyle = rows.length === 1 ? t.text : colorOf(j);
            ctx.fillText(text, vx, vy);
            valuesBottom = Math.max(valuesBottom, vy);
            const delta = j === input.baseline ? undefined : relativeDelta(value, baselineValue);
            if (delta !== undefined) {
                const label = `${delta >= 0 ? "+" : ""}${(delta * 100).toFixed(0)}% vs ${names[input.baseline]}`;
                ctx.fillStyle = delta >= 0 === (metric.better === "higher") ? t.good : t.bad;
                fitFont(ctx, label, colW - 16, 22, 600, family);
                ctx.fillText(label, vx, vy + 34);
                valuesBottom = Math.max(valuesBottom, vy + 34);
            }
        });

        const chartTop = valuesBottom + 24;
        const chartH = Math.floor(y + ph - 20 - chartTop);
        if (!response || chartH < 80) {
            return;
        }
        const off = document.createElement("canvas");
        off.width = Math.floor(pw - 40);
        off.height = chartH;
        const chart = new ChartJS(off, {
            type: "line",
            data: {
                datasets: rows.map((row, j) => {
                    const color = colorOf(j);
                    const points =
                        response.series.find((s) => s.target === row.target)?.points ?? [];
                    return {
                        data: smoothPoints(points, input.smoothing).map((p) => ({
                            x: p.time,
                            y: p.value,
                        })),
                        borderColor: color,
                        backgroundColor: `${color}33`,
                        fill: rows.length === 1 ? "origin" : false,
                        spanGaps: true,
                    };
                }),
            },
            options: {
                responsive: false,
                animation: false,
                devicePixelRatio: 1,
                events: [],
                scales: {
                    x: { type: "linear", min: start, max: end, display: false },
                    y: {
                        beginAtZero: true,
                        max: unit === "ratio" ? 1 : undefined,
                        ticks: {
                            color: t.muted,
                            font: { family, size: 18 },
                            maxTicksLimit: 4,
                            callback: (v) => formatMetricValue(v as number, unit),
                        },
                        grid: { color: t.grid },
                        border: { display: false },
                    },
                },
                plugins: { legend: { display: false }, tooltip: { enabled: false } },
                elements: { point: { radius: 0 }, line: { tension: 0.3, borderWidth: 4 } },
            },
        });
        ctx.drawImage(off, x + 16, chartTop);
        chart.destroy();
    });
}

/** fetchRanges runs each pick's range query against its source test case. */
function fetchRanges(
    picks: { metric: ReportMetric; source: number }[],
    sources: ReportSource[],
    rangeSeconds: number,
    endSeconds?: number,
) {
    const end = endSeconds ?? Math.floor(Date.now() / 1000);
    const start = end - rangeSeconds;
    const step = stepFor(rangeSeconds);
    return Promise.all(
        picks.map(({ metric: m, source }) => {
            const testid = sources[source]?.testid;
            if (!testid) {
                return Promise.resolve(undefined);
            }
            return m.resource
                ? api.resourceRange([testid], m.resource, start, end, step, m.component)
                : api.metricsRange([testid], m.key, start, end, step);
        }),
    ).then((responses) => ({ responses, start, end, picks }));
}

/**
 * MarketingReport is a modal that picks metrics and renders/exports the
 * image. The window and per-target values come from the page opening it, so
 * the image shows the same numbers the page does.
 */
export function MarketingReport({
    name,
    subtitle: defaultSubtitle,
    sources,
    names: defaultNames,
    baseline = 0,
    endSeconds,
    rangeSeconds = 900,
    onClose,
}: {
    /** Default title and file name. */
    name: string;
    subtitle: string;
    sources: ReportSource[];
    /** Default display name per series position. */
    names: string[];
    baseline?: number;
    endSeconds?: number;
    rangeSeconds?: number;
    onClose: () => void;
}) {
    const dialogRef = useRef<HTMLDialogElement>(null);
    const canvasRef = useRef<HTMLCanvasElement>(null);
    const [selected, setSelected] = useState(DEFAULT_METRICS);
    // Source index per metric key; unset is the first source.
    const [sourceOf, setSourceOf] = useState<Record<string, number>>({});
    const [title, setTitle] = useState(name);
    const [subtitle, setSubtitle] = useState(defaultSubtitle);
    const [theme, setTheme] = useState<ThemeName>("dark");
    const [labels, setLabels] = useState<Record<number, string>>({});
    const [smoothing, setSmoothing] = useState(0);
    const [colors, setColors] = useState<Record<number, number>>({});
    const metrics = REPORT_METRICS.filter((m) => selected.includes(m.key));
    const picks = metrics.map((m) => ({ metric: m, source: sourceOf[m.key] ?? 0 }));

    useEffect(() => dialogRef.current?.showModal(), []);

    const { data, error, loading } = useResource(
        () => fetchRanges(picks, sources, rangeSeconds, endSeconds),
        [
            picks.map((p) => `${p.metric.key}@${sources[p.source]?.testid}`).join(","),
            endSeconds,
            rangeSeconds,
        ],
        0,
    );

    useEffect(() => {
        const ctx = canvasRef.current?.getContext("2d");
        if (!ctx || !data) {
            return;
        }
        const panels = data.picks.map(({ metric, source }, i) => ({
            metric,
            response: data.responses[i],
            rows: sources[source]?.rows ?? [],
            source: sources.length > 1 ? sources[source]?.label : undefined,
        }));
        let cancelled = false;
        // Web fonts must be in before the canvas measures and draws text.
        void document.fonts.ready.then(() => {
            if (!cancelled) {
                drawReport(ctx, {
                    title,
                    subtitle,
                    theme,
                    panels,
                    start: data.start,
                    end: data.end,
                    names: defaultNames.map((n, i) => labels[i] || n),
                    baseline,
                    smoothing,
                    colors,
                });
            }
        });
        return () => {
            cancelled = true;
        };
    }, [data, title, subtitle, theme, sources, defaultNames, labels, baseline, smoothing, colors]);

    function toggle(key: string) {
        setSelected((s) => (s.includes(key) ? s.filter((k) => k !== key) : [...s, key]));
    }

    function toBlob(): Promise<Blob> {
        return new Promise((resolve, reject) =>
            canvasRef.current?.toBlob((b) => (b ? resolve(b) : reject(new Error("Empty image")))),
        );
    }

    async function download() {
        const url = URL.createObjectURL(await toBlob());
        const a = document.createElement("a");
        a.href = url;
        a.download = `${name}.png`;
        a.click();
        URL.revokeObjectURL(url);
    }

    async function copy() {
        try {
            await navigator.clipboard.write([new ClipboardItem({ "image/png": toBlob() })]);
            toast.success("Copied image");
        } catch (err) {
            toast.error(errorMessage(err));
        }
    }

    return (
        <dialog ref={dialogRef} className="report" onClose={onClose}>
            <div className="report-controls">
                <label className="field">
                    Title
                    <input value={title} onChange={(e) => setTitle(e.target.value)} />
                </label>
                <label className="field">
                    Subtitle
                    <input value={subtitle} onChange={(e) => setSubtitle(e.target.value)} />
                </label>
                <Segmented
                    label="Theme"
                    options={[
                        { value: "dark", label: "Dark" },
                        { value: "light", label: "Light" },
                    ]}
                    value={theme}
                    onChange={setTheme}
                />
                <label className="field">
                    Smoothing ({smoothing})
                    <input
                        type="range"
                        min={0}
                        max={20}
                        value={smoothing}
                        onChange={(e) => setSmoothing(Number(e.target.value))}
                    />
                </label>
            </div>
            <div className="report-labels">
                {defaultNames.map((n, i) => (
                    <div className="field" key={i}>
                        {n}
                        {defaultNames.length > 1 && (
                            <input
                                aria-label={`Label for ${n}`}
                                value={labels[i] ?? ""}
                                placeholder={n}
                                onChange={(e) => setLabels((l) => ({ ...l, [i]: e.target.value }))}
                            />
                        )}
                        <div className="report-swatches" role="group" aria-label="Color">
                            {palettes[theme].map((color, c) => (
                                <button
                                    key={color}
                                    type="button"
                                    aria-label={`Color ${c + 1}`}
                                    aria-pressed={colors[i] === c}
                                    style={{ background: color }}
                                    onClick={() => setColors((cs) => ({ ...cs, [i]: c }))}
                                />
                            ))}
                        </div>
                    </div>
                ))}
            </div>
            <div className="report-metrics">
                {REPORT_METRICS.map((m) => (
                    <span className="row" key={m.key}>
                        <label className="check">
                            <input
                                type="checkbox"
                                checked={selected.includes(m.key)}
                                disabled={
                                    !selected.includes(m.key) && selected.length >= MAX_METRICS
                                }
                                onChange={() => toggle(m.key)}
                            />
                            {m.label}
                        </label>
                        {sources.length > 1 && selected.includes(m.key) && (
                            <select
                                className="small"
                                aria-label={`Test case for ${m.label}`}
                                value={sourceOf[m.key] ?? 0}
                                onChange={(e) =>
                                    setSourceOf((s) => ({ ...s, [m.key]: Number(e.target.value) }))
                                }
                            >
                                {sources.map((src, i) => (
                                    <option key={src.testid} value={i}>
                                        {src.label}
                                    </option>
                                ))}
                            </select>
                        )}
                    </span>
                ))}
            </div>
            <Alert>{error}</Alert>
            <canvas
                ref={canvasRef}
                width={W}
                height={H}
                className={loading ? "loading" : undefined}
            />
            <div className="row confirm-actions">
                <button onClick={() => dialogRef.current?.close()}>Close</button>
                <button onClick={() => void copy()}>Copy</button>
                <button className="primary" onClick={() => void download()}>
                    Download PNG
                </button>
            </div>
        </dialog>
    );
}
