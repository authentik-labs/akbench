import { api } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { MarketingReport } from "../components/MarketingReport";
import { baselineOf, MatrixGrid } from "../components/Matrix";
import { MetricsGrid, ResourceCharts } from "../components/MetricsChart";
import { ObjectYaml } from "../components/ObjectYaml";
import { PullRequestLink } from "../components/PullRequestLink";
import { ConditionList, StatusBadge } from "../components/Status";
import { burnStatus, TARGET_METRICS } from "../components/Targets";
import {
    Alert,
    confirmAndRun,
    Empty,
    Loading,
    Page,
    Section,
    SectionNav,
    TimeRangeSelect,
} from "../components/ui";
import {
    archivedBurnWindow,
    earliestBurnStart,
    formatDuration,
    parseTimeRange,
    SINCE_START,
    timeRangeSeconds,
} from "../format";
import { PULL_REQUEST_LABEL } from "../github";
import { TestCase } from "../types";
import { useResource } from "../useResource";

import { useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router";

function deleteParent(name: string, then: () => void) {
    return confirmAndRun(
        {
            title: "Delete test case",
            message: `Delete test case "${name}" together with every test case and deployment it created?`,
            confirmLabel: "Delete",
            danger: true,
        },
        () => api.deleteTestCase(name),
        then,
        `Deleted ${name}`,
    );
}

/** parentDeployments is every deployment a parent test case created. */
export function parentDeployments(m: TestCase): string[] {
    return [
        ...new Set(
            (m.status?.children ?? []).flatMap((r) => r.cells ?? []).map((c) => c.deployment),
        ),
    ];
}

/**
 * setArchived archives or unarchives a parent m, offering on archive to also
 * delete the deployments it created to free their clusters - archiving leaves
 * them alone and never recreates them, so the children keep their results.
 */
function setArchived(m: TestCase, archived: boolean, reload: () => void) {
    const name = m.metadata.name;
    const own = archived ? parentDeployments(m) : [];
    return confirmAndRun(
        {
            title: archived ? "Archive test case" : "Unarchive test case",
            message: archived
                ? `Archive test case "${name}"? Its child test cases stop running but stay listed with their past runs.`
                : `Unarchive test case "${name}"? Its deployments and child test cases are recreated.`,
            confirmLabel: archived ? "Archive" : "Unarchive",
            checkbox:
                own.length > 0
                    ? {
                          label: `Also delete its ${own.length === 1 ? "deployment" : `${own.length} deployments`} (${own.join(", ")})`,
                      }
                    : undefined,
        },
        async (withDeployments) => {
            await api.updateTestCase(name, { spec: { ...m.spec, archived } });
            if (withDeployments) {
                await Promise.all(own.map((d) => api.deleteDeployment(d)));
            }
        },
        reload,
        archived ? `Archived ${name}` : `Unarchived ${name}`,
    );
}

const FALLBACK_TIMELINE_RANGE = "6h";

const SECTIONS = [
    { id: "matrix", label: "Children" },
    { id: "timeline", label: "Timeline" },
    { id: "resources", label: "Resources" },
    { id: "status", label: "Status" },
];

/**
 * TestCaseParentView lays a parent test case out like a leaf's page: a grid
 * with a row per template (its child TestCase) and a column per variant
 * (that row's deployment running it), each cell's value of one metric
 * against the baseline variant's; then one chart per metric and resource
 * across every child and deployment, over one shared time range.
 */
export function TestCaseParentView({
    testCase,
}: {
    testCase: ReturnType<typeof useResource<TestCase>>;
}) {
    const { name = "" } = useParams();
    const navigate = useNavigate();
    const testCases = useResource(() => api.testCases(), [], 10000, "TestCase");
    const m = testCase.data;
    const variants = m?.spec.variants ?? [];
    const templates = m?.spec.templates ?? [];
    const [metricKey, setMetricKey] = useState("iteration_duration_p95");
    const [reportOpen, setReportOpen] = useState(false);

    const rows = m?.status?.children ?? [];
    const testids = rows.map((r) => r.testCase);
    const rowTestCases = (testCases.data ?? []).filter((tc) => testids.includes(tc.metadata.name));

    // Same time range handling as a Burn test case's page, across every
    // row's test case: since the earliest burn start, and for an archived
    // parent ending when its last burn stopped.
    const [params, setParams] = useSearchParams();
    const archivedWindows = rowTestCases.flatMap((tc) => archivedBurnWindow(tc) ?? []);
    const archivedStarts = archivedWindows.flatMap((w) => (w.start !== undefined ? [w.start] : []));
    const burnStart = m?.spec.archived
        ? archivedStarts.length
            ? Math.min(...archivedStarts)
            : undefined
        : earliestBurnStart(rowTestCases);
    const burnEnd =
        m?.spec.archived && archivedWindows.length
            ? Math.floor(Math.max(...archivedWindows.map((w) => w.end))) + 60
            : undefined;
    const defaultRange = burnStart !== undefined ? SINCE_START : FALLBACK_TIMELINE_RANGE;
    const rangeValue = parseTimeRange(params.get("range"), defaultRange, burnStart);
    let range = timeRangeSeconds(rangeValue, burnStart, burnEnd);
    if (burnStart !== undefined && burnEnd !== undefined) {
        range = Math.min(range, timeRangeSeconds(SINCE_START, burnStart, burnEnd));
    }
    function setRange(value: string) {
        const next = new URLSearchParams(params);
        if (value === defaultRange) {
            next.delete("range");
        } else {
            next.set("range", value);
        }
        setParams(next, { replace: true });
    }

    const comparison = useResource(
        () =>
            testids.length
                ? api.compare(testids, `${range}s`, burnEnd)
                : Promise.resolve(undefined),
        [testids.join(","), range, burnEnd],
        15000,
    );
    const metrics = (comparison.data?.metrics ?? []).filter((x) => TARGET_METRICS.includes(x.key));
    const metric = metrics.find((x) => x.key === metricKey);
    const burn = rowTestCases.map((tc) => burnStatus(tc)).find((d) => d !== undefined);
    const burnedFor =
        burnStart !== undefined && burnEnd !== undefined
            ? formatDuration(burnEnd - 60 - burnStart)
            : undefined;

    return (
        <Page
            title={name}
            subtitle={
                m && (
                    <>
                        {m.spec.profile} · {templates.length}{" "}
                        {templates.length === 1 ? "template" : "templates"} ×{" "}
                        {variants.map((v) => v.name).join(" vs ")} · {m.spec.vus} VUs each
                        {burn && ` · ${burn}`}
                        {burnedFor && ` · burned for ${burnedFor}`}
                        {m.metadata.labels?.[PULL_REQUEST_LABEL] && (
                            <>
                                {" · "}
                                <PullRequestLink labels={m.metadata.labels} full />
                            </>
                        )}
                    </>
                )
            }
            actions={
                m && (
                    <>
                        <StatusBadge
                            kind="testcase"
                            name={name}
                            namespace={m.metadata.namespace}
                            phase={m.status?.phase}
                            conditions={m.status?.conditions}
                        />
                        <button
                            onClick={() => void setArchived(m, !m.spec.archived, testCase.reload)}
                        >
                            {m.spec.archived ? "Unarchive" : "Archive"}
                        </button>
                        <button onClick={() => setReportOpen(true)}>Report</button>
                        <button
                            className="danger"
                            onClick={() => void deleteParent(name, () => navigate("/testcases"))}
                        >
                            Delete
                        </button>
                    </>
                )
            }
        >
            <Alert>{testCase.error ?? testCases.error ?? comparison.error}</Alert>
            <SectionNav sections={SECTIONS} />

            <Section
                id="matrix"
                title="Children"
                loading={comparison.loading && comparison.data !== undefined}
                description="Each template on every variant's deployment. Δ is relative to the baseline variant."
                actions={
                    <span className="row">
                        <select
                            aria-label="Metric"
                            value={metricKey}
                            onChange={(e) => setMetricKey(e.target.value)}
                        >
                            {metrics.map((x) => (
                                <option key={x.key} value={x.key}>
                                    {x.label}
                                </option>
                            ))}
                        </select>
                        <TimeRangeSelect
                            value={rangeValue}
                            sinceStartLabel={
                                burnStart !== undefined ? "Since burn start" : undefined
                            }
                            onChange={setRange}
                        />
                    </span>
                }
            >
                {!m ? (
                    !testCase.error && <Loading />
                ) : rows.length === 0 ? (
                    <Empty>No child test cases yet - they're still being set up.</Empty>
                ) : (
                    <MatrixGrid
                        parent={m}
                        results={comparison.data?.results ?? []}
                        metric={metric}
                        metricKey={metricKey}
                    />
                )}
            </Section>

            <Section
                id="timeline"
                title="Timeline"
                description="Every metric across all templates, one line per test case and deployment."
            >
                <MetricsGrid
                    testids={testids}
                    endSeconds={burnEnd}
                    rangeSeconds={range}
                    height={360}
                    wide
                />
            </Section>

            <Section
                id="resources"
                title="Resources"
                description="CPU and memory of every deployment's server and worker pods over the same range."
            >
                <ResourceCharts testids={testids} endSeconds={burnEnd} rangeSeconds={range} wide />
            </Section>

            <Section
                id="status"
                title="Status"
                description="Conditions, recent events and the full object."
            >
                {m && (
                    <>
                        <ConditionList conditions={m.status?.conditions} />
                        <ActivityLog path="testcases" name={name} />
                    </>
                )}
                <ObjectYaml object={m} />
            </Section>
            {reportOpen && m && (
                <MarketingReport
                    name={name}
                    subtitle={`${variants.map((v) => v.name).join(" vs ")} · ${m.spec.vus} VUs each`}
                    // One source per template; its rows in variant order, the
                    // cell's deployment being its k6 target (as in MatrixGrid).
                    sources={rows.map((row) => ({
                        testid: row.testCase,
                        label: row.template,
                        rows: variants.map((v) => {
                            const target =
                                row.cells?.find((c) => c.variant === v.name)?.deployment ?? "";
                            return {
                                target,
                                values:
                                    comparison.data?.results.find(
                                        (res) =>
                                            res.testid === row.testCase && res.target === target,
                                    )?.values ?? {},
                            };
                        }),
                    }))}
                    names={variants.map((v) => v.name)}
                    baseline={Math.max(
                        0,
                        variants.findIndex((v) => v.name === baselineOf(m)),
                    )}
                    endSeconds={burnEnd}
                    rangeSeconds={range}
                    onClose={() => setReportOpen(false)}
                />
            )}
        </Page>
    );
}
