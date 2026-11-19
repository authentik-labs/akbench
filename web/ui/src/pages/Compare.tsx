import { api } from "../api";
import { MetricsChart, ResourceCharts } from "../components/MetricsChart";
import { RestartsWarning } from "../components/Restarts";
import { Alert, Empty, Field, Page, TimeRangeSelect } from "../components/ui";
import {
    earliestBurnStart,
    formatMetricValue,
    metricsAnchorFromRuns,
    parseTimeRange,
    timeRangeSeconds,
} from "../format";
import { CompareResponse, isParent } from "../types";
import { useResource } from "../useResource";

// Side-by-side comparison of test cases: an instant-query summary table
// (/metrics/compare) and one chart per metric over time (/metrics/range).
// The metric list comes from the backend's response, never a local copy.
import { useMemo } from "react";
import { useSearchParams } from "react-router";

const SUMMARY_WINDOWS = ["1m", "5m", "15m", "1h"];
const DEFAULT_SUMMARY_WINDOW = "5m";
const DEFAULT_RANGE = "15m";

/**
 * resultKey/resultLabel identify and label one compare row - a (testid,
 * target) pair, since a multi-deployment TestCase now produces more than one
 * row per testid. Label drops the target suffix unless the testid actually
 * has more than one row in this response, same disambiguation the charts use.
 */
function resultKey(r: CompareResponse["results"][number]): string {
    return `${r.testid}::${r.target}`;
}
function resultLabel(
    r: CompareResponse["results"][number],
    results: CompareResponse["results"],
): string {
    return results.filter((other) => other.testid === r.testid).length > 1
        ? `${r.testid} / ${r.target}`
        : r.testid;
}

/** best picks the winning (testid, target) row per metric, honouring its direction. */
function best(response: CompareResponse, key: string, better: string): string | undefined {
    const scored = response.results
        .filter((r) => r.values[key] !== undefined)
        .map((r) => ({ rowKey: resultKey(r), value: r.values[key] }));
    if (scored.length < 2) {
        return undefined;
    }
    return scored.reduce((a, b) =>
        (better === "lower" ? b.value < a.value : b.value > a.value) ? b : a,
    ).rowKey;
}

export function ComparePage() {
    // A parent has no metrics of its own - its children do.
    const testCases = useResource(
        () => api.testCases().then((all) => all.filter((tc) => !isParent(tc))),
        [],
        0,
    );
    const testRuns = useResource(() => api.testRuns(), [], 15000);
    // The whole comparison - which tests, the summary window, the chart range -
    // lives in the URL, so it can be shared/bookmarked exactly as configured
    // rather than just the test selection.
    const [params, setParams] = useSearchParams();
    const selected = useMemo(
        () => (params.get("tests") ?? "").split(",").filter(Boolean),
        [params],
    );
    const summaryWindowParam = params.get("window");
    const summaryWindow =
        summaryWindowParam && SUMMARY_WINDOWS.includes(summaryWindowParam)
            ? summaryWindowParam
            : DEFAULT_SUMMARY_WINDOW;

    /**
     * updateParams patches only the given keys, preserving the rest of the
     * query string - setSearchParams itself replaces the whole thing, so
     * toggling a test must not silently reset the window/range a user already
     * picked (and vice versa).
     */
    function updateParams(patch: Record<string, string | undefined>) {
        const next = new URLSearchParams(params);
        for (const [key, value] of Object.entries(patch)) {
            if (value === undefined) {
                next.delete(key);
            } else {
                next.set(key, value);
            }
        }
        setParams(next, { replace: true });
    }

    const allQuick =
        selected.length > 0 &&
        selected.every(
            (id) => testCases.data?.find((tc) => tc.metadata.name === id)?.spec.profile === "Quick",
        );
    // A Quick test's data ends when its run does; anchoring at "now" would
    // show nothing minutes later. A mixed selection keeps "now", which is
    // right for the Burn test(s) in it.
    const anchor = allQuick
        ? metricsAnchorFromRuns(
              (testRuns.data ?? []).filter((r) => selected.includes(r.spec.testCaseRef.name)),
          )
        : undefined;
    // "Since test start" begins at the earliest of the selected Burn tests'
    // start and each selected Quick test's latest run.
    const testStart = useMemo(() => {
        const chosen = (testCases.data ?? []).filter((tc) => selected.includes(tc.metadata.name));
        const starts = chosen
            .filter((tc) => tc.spec.profile === "Quick")
            .flatMap((tc) => {
                const runStarts = (testRuns.data ?? [])
                    .filter(
                        (r) => r.spec.testCaseRef.name === tc.metadata.name && r.status?.startTime,
                    )
                    .map((r) => Date.parse(r.status!.startTime!) / 1000);
                return runStarts.length ? [Math.max(...runStarts)] : [];
            });
        const burnStart = earliestBurnStart(chosen.filter((tc) => tc.spec.profile === "Burn"));
        if (burnStart !== undefined) {
            starts.push(burnStart);
        }
        return starts.length ? Math.min(...starts) : undefined;
    }, [testCases.data, testRuns.data, selected]);
    const range = parseTimeRange(params.get("range"), DEFAULT_RANGE, testStart);
    const rangeSeconds = timeRangeSeconds(range, testStart, anchor?.endSeconds);

    const comparison = useResource(
        () =>
            selected.length
                ? api.compare(selected, summaryWindow, anchor?.endSeconds)
                : Promise.resolve(undefined),
        [selected.join(","), summaryWindow, anchor?.endSeconds],
        10000,
    );
    const metrics = comparison.data?.metrics ?? [];
    const results = comparison.data?.results ?? [];

    function toggle(name: string) {
        const next = selected.includes(name)
            ? selected.filter((id) => id !== name)
            : [...selected, name];
        updateParams({ tests: next.length ? next.join(",") : undefined });
    }

    return (
        <Page
            title="Compare"
            subtitle={`Pick tests to put side by side. The table summarizes the last ${summaryWindow} of each; the charts plot them over time.`}
        >
            <Alert>{comparison.error ?? testCases.error}</Alert>
            {anchor && (
                <Alert kind="info">
                    All selected tests are Quick - showing data as of their latest completed run (
                    {new Date(anchor.endSeconds * 1000).toLocaleString()}) instead of now.
                </Alert>
            )}

            <div className="panel" style={{ marginBottom: 14 }}>
                <h3>Tests</h3>
                <div className="row">
                    {(testCases.data ?? []).map((tc) => (
                        <label className="check" key={tc.metadata.name} style={{ marginRight: 12 }}>
                            <input
                                type="checkbox"
                                checked={selected.includes(tc.metadata.name)}
                                onChange={() => toggle(tc.metadata.name)}
                            />
                            {tc.metadata.name} <span className="muted">({tc.spec.profile})</span>
                        </label>
                    ))}
                    {testCases.data?.length === 0 && (
                        <span className="muted">No test cases yet.</span>
                    )}
                </div>
                <div className="fields" style={{ marginTop: 12 }}>
                    <Field label="Summary window">
                        <select
                            value={summaryWindow}
                            onChange={(e) =>
                                updateParams({
                                    window:
                                        e.target.value === DEFAULT_SUMMARY_WINDOW
                                            ? undefined
                                            : e.target.value,
                                })
                            }
                        >
                            {SUMMARY_WINDOWS.map((w) => (
                                <option key={w}>{w}</option>
                            ))}
                        </select>
                    </Field>
                    <Field label="Chart range">
                        <TimeRangeSelect
                            value={range}
                            sinceStartLabel={
                                testStart !== undefined ? "Since test start" : undefined
                            }
                            onChange={(v) =>
                                updateParams({ range: v === DEFAULT_RANGE ? undefined : v })
                            }
                        />
                    </Field>
                </div>
            </div>

            {selected.length === 0 ? (
                <Empty>Select at least one test.</Empty>
            ) : (
                <>
                    <h2>Summary</h2>
                    <RestartsWarning
                        testids={selected}
                        period={summaryWindow}
                        endSeconds={anchor?.endSeconds}
                    />
                    <table className="table">
                        <thead>
                            <tr>
                                <th>Metric</th>
                                {results.map((r) => (
                                    <th key={resultKey(r)}>{resultLabel(r, results)}</th>
                                ))}
                            </tr>
                        </thead>
                        <tbody>
                            {metrics.map((m) => {
                                const winner = comparison.data
                                    ? best(comparison.data, m.key, m.better)
                                    : undefined;
                                return (
                                    <tr key={m.key}>
                                        <td>
                                            {m.label}{" "}
                                            <span className="muted">({m.better} is better)</span>
                                        </td>
                                        {results.map((r) => (
                                            <td key={resultKey(r)} className="mono">
                                                {formatMetricValue(r.values[m.key], m.unit)}
                                                {winner === resultKey(r) ? " ✓" : ""}
                                            </td>
                                        ))}
                                    </tr>
                                );
                            })}
                        </tbody>
                    </table>

                    <h2>Over time</h2>
                    <div className="grid2">
                        {metrics.map((m) => (
                            <MetricsChart
                                key={m.key}
                                metric={m.key}
                                testids={selected}
                                rangeSeconds={rangeSeconds}
                                endSeconds={anchor?.endSeconds}
                            />
                        ))}
                    </div>

                    <h2>Resource usage</h2>
                    <p className="muted">
                        CPU and memory of each test's authentik server and worker pods, summed per
                        target - one line per test, same as the charts above.
                    </p>
                    <ResourceCharts
                        testids={selected}
                        rangeSeconds={rangeSeconds}
                        endSeconds={anchor?.endSeconds}
                    />
                </>
            )}
        </Page>
    );
}
