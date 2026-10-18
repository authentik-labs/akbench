import { api } from "../api";
import { formatMetricValue, relativeDelta } from "../format";
import { LogEvent, LogTarget } from "../types";
import { useResource } from "../useResource";
import { Delta, TargetKey, TargetRow } from "./Targets";
import { BREAKDOWN_WINDOWS } from "./TraceBreakdown";
import { Alert, Empty, Loading, Section, Segmented } from "./ui";

// "What is authentik complaining about" per target, from the container logs
// Alloy ships to its workload cluster's Loki (internal/api/logs.go). Traces
// and profiles say where time goes; this says what went wrong along the way.
import { useState } from "react";

/** How many events the comparison table shows. */
const TOP_EVENTS = 15;

// internal/api/logs.go's logProblemLevels, split the way the table counts them.
const WARNING_LEVELS = ["warning"];
const ERROR_LEVELS = ["error", "critical", "fatal", "panic"];

const eventKey = (e: LogEvent) => `${e.level}\u0000${e.logger}\u0000${e.event}`;

function countLevels(t: LogTarget, levels: string[]): number {
    return t.components.reduce(
        (sum, c) => sum + levels.reduce((s, l) => s + (c.levels[l] ?? 0), 0),
        0,
    );
}

const componentLines = (t: LogTarget, component: string) =>
    t.components.find((c) => c.component === component)?.lines ?? 0;

export function LogBreakdownSection({
    testid,
    rows,
    baseline,
}: {
    testid: string;
    /** The test case's target rows, for consistent letters and colors. */
    rows: TargetRow[];
    baseline?: string;
}) {
    const [logWindow, setLogWindow] = useState("1h");
    // The backend caches for two minutes; polling faster would only re-read it.
    const breakdown = useResource(
        () => api.logBreakdown(testid, logWindow),
        [testid, logWindow],
        120000,
    );
    const targets = breakdown.data?.targets ?? [];
    const logged = targets.filter((t) => t.lines > 0);
    const indexOf = (target: string) => rows.find((r) => r.target === target)?.index ?? 0;
    const base = logged.find((t) => t.target === baseline) ?? logged[0];

    // The union of every target's most frequent events, most frequent first
    // by whichever target logs it the most.
    const frequent = new Map<string, { event: LogEvent; peak: number }>();
    for (const t of logged) {
        for (const e of t.topEvents) {
            const entry = frequent.get(eventKey(e));
            if (!entry || e.count > entry.peak) {
                frequent.set(eventKey(e), { event: e, peak: e.count });
            }
        }
    }
    const events = [...frequent.values()]
        .sort((a, b) => b.peak - a.peak)
        .slice(0, TOP_EVENTS)
        .map((e) => e.event);

    return (
        <Section
            id="logs"
            loading={breakdown.loading && breakdown.data !== undefined}
            title="Logs"
            description={
                <>
                    What authentik logs, from each target's server and worker pods in Loki. Covers
                    all of the target's pods, not only this test's requests.
                </>
            }
            actions={
                <Segmented
                    label="Log window"
                    options={BREAKDOWN_WINDOWS}
                    value={logWindow}
                    onChange={setLogWindow}
                />
            }
        >
            <Alert>{breakdown.error}</Alert>
            {targets
                .filter((t) => t.error)
                .map((t) => (
                    <Alert kind="info" key={t.target}>
                        No logs for {t.target}: {t.error}
                    </Alert>
                ))}
            {logged.length > 0 &&
                targets
                    .filter((t) => !t.error && t.lines === 0)
                    .map((t) => (
                        <Alert kind="info" key={t.target}>
                            No logs from {t.target}'s pods in the last {logWindow}.
                        </Alert>
                    ))}
            {!breakdown.data && !breakdown.error && <Loading>Querying Loki…</Loading>}
            {breakdown.data && logged.length === 0 ? (
                <Empty>
                    No logs from the targets' pods in the last {logWindow}. Alloy ships every pod's
                    logs to its cluster's Loki; check both are running on the workload cluster.
                </Empty>
            ) : (
                logged.length > 0 && (
                    <>
                        <div className="table-scroll">
                            <table className="table">
                                <thead>
                                    <tr>
                                        <th>Target</th>
                                        <th className="num">Lines/s</th>
                                        {logged.length > 1 && <th className="num">Δ</th>}
                                        <th className="num">Server</th>
                                        <th className="num">Worker</th>
                                        <th className="num">Warnings</th>
                                        <th
                                            className="num"
                                            title="Lines logged at error, critical, fatal or panic"
                                        >
                                            Errors
                                        </th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {logged.map((t) => {
                                        const errors = countLevels(t, ERROR_LEVELS);
                                        return (
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
                                                    title={`${t.lines} lines in the last ${logWindow}`}
                                                >
                                                    {formatMetricValue(t.linesPerSecond, "")}
                                                </td>
                                                {logged.length > 1 && (
                                                    <td className="mono num">
                                                        {t === base ? (
                                                            <span className="muted">baseline</span>
                                                        ) : (
                                                            <Delta
                                                                value={relativeDelta(
                                                                    t.linesPerSecond,
                                                                    base.linesPerSecond,
                                                                )}
                                                                better="lower"
                                                            />
                                                        )}
                                                    </td>
                                                )}
                                                <td className="mono num">
                                                    {componentLines(t, "server")}
                                                </td>
                                                <td className="mono num">
                                                    {componentLines(t, "worker")}
                                                </td>
                                                <td className="mono num">
                                                    {countLevels(t, WARNING_LEVELS)}
                                                </td>
                                                <td
                                                    className="mono num"
                                                    style={
                                                        errors > 0
                                                            ? { color: "var(--err)" }
                                                            : undefined
                                                    }
                                                >
                                                    {errors}
                                                </td>
                                            </tr>
                                        );
                                    })}
                                </tbody>
                            </table>
                        </div>

                        {events.length > 0 && (
                            <>
                                <h3>Most frequent warnings and errors · lines</h3>
                                <div className="table-scroll">
                                    <table className="table">
                                        <thead>
                                            <tr>
                                                <th>Event</th>
                                                {logged.map((t) => (
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
                                            {events.map((e) => (
                                                <tr key={eventKey(e)}>
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
                                                                    background:
                                                                        ERROR_LEVELS.includes(
                                                                            e.level,
                                                                        )
                                                                            ? "var(--err)"
                                                                            : "var(--warn)",
                                                                    marginTop: 4,
                                                                }}
                                                                title={e.level}
                                                            />
                                                            <span style={{ minWidth: 0 }}>
                                                                <span className="mono">
                                                                    {e.event}
                                                                </span>
                                                                <span
                                                                    className="fn-file mono"
                                                                    title={e.logger}
                                                                >
                                                                    {e.level}
                                                                    {e.logger && ` · ${e.logger}`}
                                                                </span>
                                                            </span>
                                                        </span>
                                                    </td>
                                                    {logged.map((t) => {
                                                        const own = t.topEvents.find(
                                                            (o) => eventKey(o) === eventKey(e),
                                                        );
                                                        return (
                                                            <td key={t.target} className="mono num">
                                                                {own ? (
                                                                    own.count
                                                                ) : (
                                                                    <span
                                                                        className="muted"
                                                                        title="Not among this target's most frequent events"
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
