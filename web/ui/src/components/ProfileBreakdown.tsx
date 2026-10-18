import { api } from "../api";
import { formatMetricValue, relativeDelta } from "../format";
import { ProfileFunction, ProfileTarget } from "../types";
import { useResource } from "../useResource";
import { Delta, TargetKey, TargetRow } from "./Targets";
import { BREAKDOWN_WINDOWS } from "./TraceBreakdown";
import { Alert, Empty, Loading, Section, Segmented } from "./ui";

// "What is authentik's CPU busy with" per target, from the continuous
// profiles authentik pushes to its workload cluster's Pyroscope
// (internal/api/profiles.go). Traces say which requests and queries are
// slow; this says which code burns the CPU - authentik's own, Django's, the
// database driver's or other libraries'.
import { useState } from "react";

const COMPONENTS = [
    { value: "server", label: "Server" },
    { value: "worker", label: "Worker" },
];

// internal/api/profiles.go's categories, in its order.
const CATEGORIES: { name: string; label: string; color: string }[] = [
    { name: "authentik", label: "authentik", color: "var(--accent)" },
    { name: "Django", label: "Django", color: "var(--ok)" },
    { name: "Database driver", label: "DB driver", color: "var(--info)" },
    { name: "Other libraries", label: "Libraries", color: "#8a63d2" },
    { name: "Python runtime", label: "Python", color: "var(--muted)" },
];

/** How many functions the comparison table shows. */
const TOP_FUNCTIONS = 15;

const percent = (v: number | undefined) => formatMetricValue(v, "ratio");
const shareOf = (t: ProfileTarget, category: string) =>
    t.categories.find((c) => c.name === category)?.share ?? 0;
const functionKey = (f: ProfileFunction) => `${f.name}\u0000${f.file}`;

export function ProfileBreakdownSection({
    testid,
    rows,
    baseline,
}: {
    testid: string;
    /** The test case's target rows, for consistent letters and colors. */
    rows: TargetRow[];
    baseline?: string;
}) {
    const [profileWindow, setProfileWindow] = useState("1h");
    const [component, setComponent] = useState("server");
    // The backend caches for two minutes; polling faster would only re-read it.
    const breakdown = useResource(
        () => api.profileBreakdown(testid, profileWindow, component),
        [testid, profileWindow, component],
        120000,
    );
    const targets = breakdown.data?.targets ?? [];
    const profiled = targets.filter((t) => t.cpuSeconds > 0);
    const indexOf = (target: string) => rows.find((r) => r.target === target)?.index ?? 0;
    const base = profiled.find((t) => t.target === baseline) ?? profiled[0];

    // The union of every target's hottest functions, hottest first by
    // whichever target spends the most on it.
    const hottest = new Map<string, { fn: ProfileFunction; peak: number }>();
    for (const t of profiled) {
        for (const fn of t.functions) {
            const entry = hottest.get(functionKey(fn));
            if (!entry || fn.self > entry.peak) {
                hottest.set(functionKey(fn), { fn, peak: fn.self });
            }
        }
    }
    const functions = [...hottest.values()]
        .sort((a, b) => b.peak - a.peak)
        .slice(0, TOP_FUNCTIONS)
        .map((e) => e.fn);

    return (
        <Section
            id="profiling"
            loading={breakdown.loading && breakdown.data !== undefined}
            title="Profiling"
            description={
                <>
                    What authentik's CPU is busy with, from each target's continuous profiles in
                    Pyroscope. Covers all of the target's {component} pods, not only this test's
                    requests.
                </>
            }
            actions={
                <>
                    <Segmented
                        label="Process"
                        options={COMPONENTS}
                        value={component}
                        onChange={setComponent}
                    />
                    <Segmented
                        label="Profile window"
                        options={BREAKDOWN_WINDOWS}
                        value={profileWindow}
                        onChange={setProfileWindow}
                    />
                </>
            }
        >
            <Alert>{breakdown.error}</Alert>
            {targets
                .filter((t) => t.error)
                .map((t) => (
                    <Alert kind="info" key={t.target}>
                        No profiles for {t.target}: {t.error}
                    </Alert>
                ))}
            {profiled.length > 0 &&
                targets
                    .filter((t) => !t.error && t.cpuSeconds === 0)
                    .map((t) => (
                        <Alert kind="info" key={t.target}>
                            No profiles from {t.target}'s {component} pods in the last{" "}
                            {profileWindow}.
                        </Alert>
                    ))}
            {!breakdown.data && !breakdown.error && <Loading>Querying Pyroscope…</Loading>}
            {breakdown.data && profiled.length === 0 ? (
                <Empty>
                    No CPU profiles from the targets' {component} pods in the last {profileWindow}.
                    authentik pushes them to its cluster's Pyroscope on its own; check the workload
                    cluster's Pyroscope is running.
                </Empty>
            ) : (
                profiled.length > 0 && (
                    <>
                        <div className="table-scroll">
                            <table className="table">
                                <thead>
                                    <tr>
                                        <th>Target</th>
                                        <th
                                            className="num"
                                            title="Average Python CPU the profiler sampled. It leaves out authentik's Go router and native code, so it reads lower than the pods' CPU under Resources."
                                        >
                                            Profiled CPU
                                        </th>
                                        {profiled.length > 1 && <th className="num">Δ</th>}
                                        <th>Where CPU goes</th>
                                        {CATEGORIES.map((c) => (
                                            <th key={c.name} className="num">
                                                {c.label}
                                            </th>
                                        ))}
                                    </tr>
                                </thead>
                                <tbody>
                                    {profiled.map((t) => (
                                        <tr key={t.target}>
                                            <td>
                                                <span
                                                    className="row"
                                                    style={{ flexWrap: "nowrap" }}
                                                >
                                                    <TargetKey index={indexOf(t.target)} />
                                                    {t.target}
                                                </span>
                                            </td>
                                            <td
                                                className="mono num"
                                                title={`${Math.round(t.cpuSeconds)} CPU seconds profiled`}
                                            >
                                                {formatMetricValue(t.cores, "cores")}
                                            </td>
                                            {profiled.length > 1 && (
                                                <td className="mono num">
                                                    {t === base ? (
                                                        <span className="muted">baseline</span>
                                                    ) : (
                                                        <Delta
                                                            value={relativeDelta(
                                                                t.cores,
                                                                base.cores,
                                                            )}
                                                            better="lower"
                                                        />
                                                    )}
                                                </td>
                                            )}
                                            <td>
                                                <CPUSplit target={t} />
                                            </td>
                                            {CATEGORIES.map((c) => (
                                                <td key={c.name} className="mono num">
                                                    {percent(shareOf(t, c.name))}
                                                </td>
                                            ))}
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                        <div className="legend" aria-hidden="true">
                            {CATEGORIES.map((c) => (
                                <span key={c.name}>
                                    <span className="swatch" style={{ background: c.color }} />
                                    {c.name}
                                </span>
                            ))}
                        </div>

                        {functions.length > 0 && (
                            <>
                                <h3>Hottest functions · share of CPU, self (total)</h3>
                                <div className="table-scroll">
                                    <table className="table">
                                        <thead>
                                            <tr>
                                                <th>Function</th>
                                                {profiled.map((t) => (
                                                    <th key={t.target} className="num">
                                                        <span
                                                            className="row"
                                                            style={{
                                                                justifyContent: "flex-end",
                                                                flexWrap: "nowrap",
                                                            }}
                                                        >
                                                            <TargetKey index={indexOf(t.target)} />
                                                            {t.target}
                                                        </span>
                                                    </th>
                                                ))}
                                            </tr>
                                        </thead>
                                        <tbody>
                                            {functions.map((fn) => (
                                                <tr key={functionKey(fn)}>
                                                    <td>
                                                        <span
                                                            className="row"
                                                            style={{
                                                                flexWrap: "nowrap",
                                                                alignItems: "flex-start",
                                                            }}
                                                        >
                                                            <span
                                                                className="swatch"
                                                                style={{
                                                                    background: categoryColor(
                                                                        fn.category,
                                                                    ),
                                                                    marginTop: 4,
                                                                }}
                                                                title={fn.category}
                                                            />
                                                            <span style={{ minWidth: 0 }}>
                                                                <span className="mono">
                                                                    {fn.name}
                                                                </span>
                                                                <span
                                                                    className="fn-file mono"
                                                                    title={fn.file}
                                                                >
                                                                    {fn.file}
                                                                </span>
                                                            </span>
                                                        </span>
                                                    </td>
                                                    {profiled.map((t) => {
                                                        const own = t.functions.find(
                                                            (f) =>
                                                                functionKey(f) === functionKey(fn),
                                                        );
                                                        return (
                                                            <td key={t.target} className="mono num">
                                                                {own ? (
                                                                    <>
                                                                        {percent(own.self)}{" "}
                                                                        <span className="muted">
                                                                            ({percent(own.total)})
                                                                        </span>
                                                                    </>
                                                                ) : (
                                                                    <span
                                                                        className="muted"
                                                                        title="Not among this target's hottest functions"
                                                                    >
                                                                        -
                                                                    </span>
                                                                )}
                                                            </td>
                                                        );
                                                    })}
                                                </tr>
                                            ))}
                                        </tbody>
                                    </table>
                                </div>
                            </>
                        )}
                    </>
                )
            )}
        </Section>
    );
}

function categoryColor(name: string): string {
    return CATEGORIES.find((c) => c.name === name)?.color ?? "var(--muted)";
}

/** CPUSplit is one target's CPU as a stacked bar of code categories. */
function CPUSplit({ target }: { target: ProfileTarget }) {
    return (
        <span
            className="cpu-split"
            role="img"
            aria-label={CATEGORIES.map((c) => `${c.name} ${percent(shareOf(target, c.name))}`).join(
                ", ",
            )}
        >
            {CATEGORIES.map((c) => {
                const share = shareOf(target, c.name);
                return share > 0 ? (
                    <span
                        key={c.name}
                        style={{ width: `${share * 100}%`, background: c.color }}
                        title={`${c.name}: ${percent(share)}`}
                    />
                ) : null;
            })}
        </span>
    );
}
