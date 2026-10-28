// MarketingReport turns a test case's headline numbers into one stylized
// 1920x1080 image for sharing: a big number and a chart per picked metric,
// drawn straight onto a canvas so the preview is exactly the exported PNG.
import { api } from "../api";
import {
    colorFor,
    formatMetricValue,
    relativeDelta,
    seriesAverage,
    smoothPoints,
    stepFor,
} from "../format";
import { RangeResponse } from "../types";
import { useResource } from "../useResource";
import { TargetRow } from "./Targets";
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

interface DrawInput {
    title: string;
    subtitle: string;
    theme: ThemeName;
    metrics: ReportMetric[];
    responses: RangeResponse[];
    start: number;
    end: number;
    rows: TargetRow[];
    baseline?: string;
    /** Display names keyed by k6 target; unset or empty keeps the target. */
    labels: Record<string, string>;
    /** Moving-average radius, in points, applied to the chart lines only. */
    smoothing: number;
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
    const { metrics, responses, rows, start, end } = input;
    const t = THEMES[input.theme];
    const family =
        getComputedStyle(document.documentElement)
            .getPropertyValue("--ak-global--font-family--heading")
            .trim() || "sans-serif";
    const nameOf = (row: TargetRow) => input.labels[row.target] || row.target;
    const colorOf = (row: TargetRow) =>
        rows.length === 1 ? t.accent : colorFor(row.index, input.theme === "dark");
    const baselineRow = rows.find((r) => r.target === input.baseline) ?? rows[0];

    const bg = ctx.createLinearGradient(0, 0, W, H);
    bg.addColorStop(0, t.bg[0]);
    bg.addColorStop(1, t.bg[1]);
    ctx.fillStyle = bg;
    ctx.fillRect(0, 0, W, H);
    ctx.textBaseline = "alphabetic";
    ctx.textAlign = "left";

    // Legend, top right; its width is what the title has to stay clear of.
    let legendX = W - PAD;
    if (rows.length > 1) {
        ctx.font = `600 26px ${family}`;
        for (const row of [...rows].reverse()) {
            const w = ctx.measureText(nameOf(row)).width;
            legendX -= w;
            ctx.fillStyle = t.text;
            ctx.fillText(nameOf(row), legendX, PAD + 20);
            ctx.fillStyle = colorOf(row);
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

    const n = metrics.length;
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

    metrics.forEach((metric, i) => {
        const response = responses[i];
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

        ctx.fillStyle = t.muted;
        ctx.font = `600 24px ${family}`;
        ctx.fillText(metric.label.toUpperCase(), x + 32, y + 52);

        // Compare metrics use the page's own window summary; resources
        // (which compare doesn't cover) average their range.
        const valueOf = (row: TargetRow) =>
            metric.resource
                ? seriesAverage(response?.series.filter((s) => s.target === row.target) ?? [])
                : row.values[metric.key];
        const colW = (pw - 64) / rows.length;
        const maxSize = Math.min(rows.length === 1 ? 96 : 60, ph / 4);
        let valuesBottom = y + 52;
        rows.forEach((row, j) => {
            const value = valueOf(row);
            const text = formatMetricValue(value, unit);
            const vx = x + 32 + j * colW;
            const size = fitFont(ctx, text, colW - 16, maxSize, 700, family);
            const vy = y + 64 + size;
            ctx.fillStyle = rows.length === 1 ? t.text : colorOf(row);
            ctx.fillText(text, vx, vy);
            valuesBottom = Math.max(valuesBottom, vy);
            const delta =
                row === baselineRow ? undefined : relativeDelta(value, valueOf(baselineRow));
            if (delta !== undefined) {
                const label = `${delta >= 0 ? "+" : ""}${(delta * 100).toFixed(0)}% vs ${nameOf(baselineRow)}`;
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
        const chartW = Math.floor(pw - 40);
        const off = document.createElement("canvas");
        off.width = chartW;
        off.height = chartH;
        const chart = new ChartJS(off, {
            type: "line",
            data: {
                datasets: rows.map((row) => {
                    const color = colorOf(row);
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

/**
 * MarketingReport is a modal that picks metrics and renders/exports the
 * image. The window and per-target values come from the test case page, so
 * the image shows the same numbers the page does.
 */
export function MarketingReport({
    testid,
    subtitle: defaultSubtitle,
    rows,
    baseline,
    endSeconds,
    rangeSeconds = 900,
    onClose,
}: {
    testid: string;
    subtitle: string;
    rows: TargetRow[];
    baseline?: string;
    endSeconds?: number;
    rangeSeconds?: number;
    onClose: () => void;
}) {
    const dialogRef = useRef<HTMLDialogElement>(null);
    const canvasRef = useRef<HTMLCanvasElement>(null);
    const [selected, setSelected] = useState(DEFAULT_METRICS);
    const [title, setTitle] = useState(testid);
    const [subtitle, setSubtitle] = useState(defaultSubtitle);
    const [theme, setTheme] = useState<ThemeName>("dark");
    const [labels, setLabels] = useState<Record<string, string>>({});
    const [smoothing, setSmoothing] = useState(0);
    const metrics = REPORT_METRICS.filter((m) => selected.includes(m.key));

    useEffect(() => dialogRef.current?.showModal(), []);

    const { data, error, loading } = useResource(
        () => {
            const end = endSeconds ?? Math.floor(Date.now() / 1000);
            const start = end - rangeSeconds;
            const step = stepFor(rangeSeconds);
            return Promise.all(
                metrics.map((m) =>
                    m.resource
                        ? api.resourceRange([testid], m.resource, start, end, step, m.component)
                        : api.metricsRange([testid], m.key, start, end, step),
                ),
            ).then((responses) => ({ responses, start, end, metrics }));
        },
        [testid, selected.join(","), endSeconds, rangeSeconds],
        0,
    );

    useEffect(() => {
        const ctx = canvasRef.current?.getContext("2d");
        if (!ctx || !data) {
            return;
        }
        let cancelled = false;
        // Web fonts must be in before the canvas measures and draws text.
        void document.fonts.ready.then(() => {
            if (!cancelled) {
                drawReport(ctx, {
                    ...data,
                    title,
                    subtitle,
                    theme,
                    rows,
                    baseline,
                    labels,
                    smoothing,
                });
            }
        });
        return () => {
            cancelled = true;
        };
    }, [data, title, subtitle, theme, rows, baseline, labels, smoothing]);

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
        a.download = `${testid}.png`;
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
            {rows.length > 1 && (
                <div className="report-labels">
                    {rows.map((row) => (
                        <label className="field" key={row.target}>
                            {row.deploymentName}
                            <input
                                value={labels[row.target] ?? ""}
                                placeholder={row.target}
                                onChange={(e) =>
                                    setLabels((l) => ({ ...l, [row.target]: e.target.value }))
                                }
                            />
                        </label>
                    ))}
                </div>
            )}
            <div className="report-metrics">
                {REPORT_METRICS.map((m) => (
                    <label className="check" key={m.key}>
                        <input
                            type="checkbox"
                            checked={selected.includes(m.key)}
                            disabled={!selected.includes(m.key) && selected.length >= MAX_METRICS}
                            onChange={() => toggle(m.key)}
                        />
                        {m.label}
                    </label>
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
