import { api } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { MatrixGrid } from "../components/Matrix";
import { MetricsGrid, ResourceCharts } from "../components/MetricsChart";
import { ObjectYaml } from "../components/ObjectYaml";
import { ConditionList, StatusBadge } from "../components/Status";
import { burningFor, TARGET_METRICS } from "../components/Targets";
import {
    Alert,
    confirmAndRun,
    Empty,
    Loading,
    Page,
    Section,
    SectionNav,
    Table,
    TimeRangeSelect,
} from "../components/ui";
import {
    archivedBurnWindow,
    createdBy,
    earliestBurnStart,
    formatDuration,
    newestFirst,
    parseTimeRange,
    SINCE_START,
    timeRangeSeconds,
} from "../format";
import { printReport } from "../theme";
import { TestMatrix } from "../types";
import { useResource } from "../useResource";

import { useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";

function deleteMatrix(name: string, then: () => void) {
    return confirmAndRun(
        {
            title: "Delete test matrix",
            message: `Delete test matrix "${name}" together with every test case and deployment it created?`,
            confirmLabel: "Delete",
            danger: true,
        },
        () => api.deleteTestMatrix(name),
        then,
        `Deleted ${name}`,
    );
}

/**
 * setArchived archives or unarchives m, offering on archive to also delete
 * the deployments it created to free their clusters - archiving leaves them
 * alone and never recreates them, so the test cases keep their results.
 */
function setArchived(m: TestMatrix, archived: boolean, reload: () => void) {
    const name = m.metadata.name;
    const own = archived
        ? [
              ...new Set(
                  (m.status?.rows ?? []).flatMap((r) => r.cells ?? []).map((c) => c.deployment),
              ),
          ]
        : [];
    return confirmAndRun(
        {
            title: archived ? "Archive test matrix" : "Unarchive test matrix",
            message: archived
                ? `Archive test matrix "${name}"? Its test cases stop running but stay listed with their past runs.`
                : `Unarchive test matrix "${name}"? Its deployments and test cases are recreated.`,
            confirmLabel: archived ? "Archive" : "Unarchive",
            checkbox:
                own.length > 0
                    ? {
                          label: `Also delete its ${own.length === 1 ? "deployment" : `${own.length} deployments`} (${own.join(", ")})`,
                      }
                    : undefined,
        },
        async (withDeployments) => {
            await api.updateTestMatrix(name, {
                spec: { ...m.spec, testCase: { ...m.spec.testCase, archived } },
            });
            if (withDeployments) {
                await Promise.all(own.map((d) => api.deleteDeployment(d)));
            }
        },
        reload,
        archived ? `Archived ${name}` : `Unarchived ${name}`,
    );
}

export function TestMatricesPage() {
    const matrices = useResource(() => api.testMatrices(), [], 5000, "TestMatrix");
    const table = (rows: TestMatrix[] | undefined, empty: string) => (
        <Table
            rows={rows}
            rowKey={(m) => m.metadata.name}
            empty={empty}
            columns={[
                {
                    label: "Name",
                    render: (m) => (
                        <Link to={`/testmatrices/${m.metadata.name}`}>{m.metadata.name}</Link>
                    ),
                },
                {
                    label: "Variants",
                    render: (m) => m.spec.variants.map((v) => v.name).join(" vs "),
                },
                { label: "Templates", render: (m) => m.spec.templates.join(", ") },
                { label: "Created by", render: (m) => createdBy(m.metadata) },
                {
                    label: "Status",
                    render: (m) => (
                        <StatusBadge
                            kind="testmatrix"
                            name={m.metadata.name}
                            namespace={m.metadata.namespace}
                            phase={m.status?.phase}
                            conditions={m.status?.conditions}
                        />
                    ),
                },
                {
                    label: "",
                    className: "actions",
                    render: (m) => (
                        <span className="row" style={{ justifyContent: "flex-end" }}>
                            <button
                                className="small"
                                onClick={() =>
                                    void setArchived(m, !m.spec.testCase.archived, matrices.reload)
                                }
                            >
                                {m.spec.testCase.archived ? "Unarchive" : "Archive"}
                            </button>
                            {m.spec.testCase.archived && (
                                <button
                                    className="small danger"
                                    onClick={() =>
                                        void deleteMatrix(m.metadata.name, matrices.reload)
                                    }
                                >
                                    Delete
                                </button>
                            )}
                        </span>
                    ),
                },
            ]}
        />
    );
    return (
        <Page
            title="Test matrices"
            subtitle="authentik builds crossed with test case templates, each template on its own deployments."
            actions={
                <span className="row">
                    <Link className="button" to="/branches/new">
                        Compare a branch
                    </Link>
                    <Link className="button primary" to="/pull-requests/new">
                        Compare a PR
                    </Link>
                </span>
            }
        >
            <Alert>{matrices.error}</Alert>
            <Section id="active" title="Active">
                {table(
                    matrices.data?.filter((m) => !m.spec.testCase.archived),
                    "No active test matrices.",
                )}
            </Section>
            <Section id="archived" title="Archived">
                {table(
                    matrices.data &&
                        newestFirst(matrices.data.filter((m) => m.spec.testCase.archived)),
                    "No archived test matrices.",
                )}
            </Section>
        </Page>
    );
}

const FALLBACK_TIMELINE_RANGE = "6h";

const SECTIONS = [
    { id: "matrix", label: "Matrix" },
    { id: "timeline", label: "Timeline" },
    { id: "resources", label: "Resources" },
    { id: "status", label: "Status" },
];

/**
 * TestMatrixViewPage lays a matrix out like a test case's page: a grid with
 * a row per template (its TestCase) and a column per variant (that row's
 * deployment running it), each cell's value of one metric against the
 * baseline variant's; then one chart per metric and resource across every
 * test case and deployment, over one shared time range.
 */
export function TestMatrixViewPage() {
    const { name = "" } = useParams();
    const navigate = useNavigate();
    const matrix = useResource(() => api.testMatrix(name), [name], 10000, "TestMatrix");
    const testCases = useResource(() => api.testCases(), [], 10000, "TestCase");
    const m = matrix.data;
    const [metricKey, setMetricKey] = useState("iteration_duration_p95");

    const rows = m?.status?.rows ?? [];
    const testids = rows.map((r) => r.testCase);
    const rowTestCases = (testCases.data ?? []).filter((tc) => testids.includes(tc.metadata.name));

    // Same time range handling as a Burn test case's page, across every
    // row's test case: since the earliest burn start, and for an archived
    // matrix ending when its last burn stopped.
    const [params, setParams] = useSearchParams();
    const archivedWindows = rowTestCases.flatMap((tc) => archivedBurnWindow(tc) ?? []);
    const archivedStarts = archivedWindows.flatMap((w) => (w.start !== undefined ? [w.start] : []));
    const burnStart = m?.spec.testCase.archived
        ? archivedStarts.length
            ? Math.min(...archivedStarts)
            : undefined
        : earliestBurnStart(rowTestCases);
    const burnEnd =
        m?.spec.testCase.archived && archivedWindows.length
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
    const since = rowTestCases.map((tc) => burningFor(tc)).find((d) => d !== undefined);
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
                        {m.spec.testCase.profile} · {m.spec.templates.length}{" "}
                        {m.spec.templates.length === 1 ? "template" : "templates"} ×{" "}
                        {m.spec.variants.map((v) => v.name).join(" vs ")} · {m.spec.testCase.vus}{" "}
                        VUs each
                        {since && ` · burning for ${since}`}
                        {burnedFor && ` · burned for ${burnedFor}`}
                    </>
                )
            }
            actions={
                m && (
                    <>
                        <StatusBadge
                            kind="testmatrix"
                            name={name}
                            namespace={m.metadata.namespace}
                            phase={m.status?.phase}
                            conditions={m.status?.conditions}
                        />
                        <button
                            onClick={() =>
                                void setArchived(m, !m.spec.testCase.archived, matrix.reload)
                            }
                        >
                            {m.spec.testCase.archived ? "Unarchive" : "Archive"}
                        </button>
                        <button onClick={printReport}>Report (PDF)</button>
                        <button
                            className="danger"
                            onClick={() => void deleteMatrix(name, () => navigate("/testmatrices"))}
                        >
                            Delete
                        </button>
                    </>
                )
            }
        >
            <Alert>{matrix.error ?? testCases.error ?? comparison.error}</Alert>
            <SectionNav sections={SECTIONS} />

            <Section
                id="matrix"
                title="Matrix"
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
                    !matrix.error && <Loading />
                ) : rows.length === 0 ? (
                    <Empty>No test cases yet - the matrix is still being set up.</Empty>
                ) : (
                    <MatrixGrid
                        matrix={m}
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
                        <ActivityLog path="testmatrices" name={name} />
                    </>
                )}
                <ObjectYaml object={m} />
            </Section>
        </Page>
    );
}
