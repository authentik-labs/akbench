import { api } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { MetricsGrid } from "../components/MetricsChart";
import { ObjectYaml } from "../components/ObjectYaml";
import { Badge, ConditionList } from "../components/Status";
import { Alert, confirmAndRun, Empty, formatTimestamp, Page, Table } from "../components/ui";
import { metricsAnchorFromRuns } from "../format";
import { shellLinkForCluster } from "../shell/links";
import { testCaseDeployments, testRunDeployment } from "../types";
import { useResource } from "../useResource";

// Test runs are created by "Run now" on a Quick test case, never by hand -
// so this is history plus a read-only view.
import { Link, useParams } from "react-router";

export function TestRunsPage() {
    const runs = useResource(() => api.testRuns(), [], 5000, "TestRun");
    const sorted =
        runs.data &&
        [...runs.data].sort((a, b) =>
            (b.metadata.creationTimestamp ?? "").localeCompare(a.metadata.creationTimestamp ?? ""),
        );
    return (
        <Page title="Test runs" subtitle="One-shot runs of Quick test cases.">
            <Alert>{runs.error}</Alert>
            <Table
                rows={sorted}
                rowKey={(r) => r.metadata.name}
                empty="No runs yet - start one with Run now on a Quick test case."
                columns={[
                    {
                        label: "Run",
                        render: (r) => (
                            <Link to={`/testruns/${r.metadata.name}`}>{r.metadata.name}</Link>
                        ),
                    },
                    {
                        label: "Test case",
                        render: (r) => (
                            <Link to={`/testcases/${r.spec.testCaseRef.name}`}>
                                {r.spec.testCaseRef.name}
                            </Link>
                        ),
                    },
                    {
                        label: "Target",
                        render: (r) => testRunDeployment(r) ?? <span className="muted">-</span>,
                    },
                    {
                        label: "Phase",
                        render: (r) => <Badge value={r.status?.phase ?? "Pending"} />,
                    },
                    {
                        label: "Started",
                        render: (r) =>
                            formatTimestamp(r.status?.startTime ?? r.metadata.creationTimestamp),
                    },
                    {
                        label: "Completed",
                        render: (r) => formatTimestamp(r.status?.completionTime),
                    },
                    {
                        label: "",
                        className: "actions",
                        render: (r) => (
                            <button
                                className="small danger"
                                onClick={() =>
                                    void confirmAndRun(
                                        {
                                            title: "Delete test run",
                                            message: `Delete test run "${r.metadata.name}"?`,
                                            confirmLabel: "Delete",
                                            danger: true,
                                        },
                                        () => api.deleteTestRun(r.metadata.name),
                                        runs.reload,
                                        `Deleted ${r.metadata.name}`,
                                    )
                                }
                            >
                                Delete
                            </button>
                        ),
                    },
                ]}
            />
        </Page>
    );
}

const SIBLING_WINDOW_MS = 10000;

export function TestRunViewPage() {
    const { name = "" } = useParams();
    const run = useResource(() => api.testRun(name), [name], 5000, "TestRun");
    const testCaseName = run.data?.spec.testCaseRef.name;
    const testCase = useResource(
        () => (testCaseName ? api.testCase(testCaseName) : Promise.resolve(undefined)),
        [testCaseName],
        0,
        "TestCase",
    );
    // The run's own record of its target (testRunDeployment), falling back to
    // the live TestCase's first deployment for a run that hasn't started yet.
    const deploymentName =
        (run.data && testRunDeployment(run.data)) ??
        (testCase.data ? testCaseDeployments(testCase.data.spec)[0]?.name : undefined);
    const deployment = useResource(
        () => (deploymentName ? api.deployment(deploymentName) : Promise.resolve(undefined)),
        [deploymentName],
        0,
        "AuthentikDeployment",
    );
    const cluster =
        testCase.data?.spec.runnerClusterRef?.name ?? deployment.data?.spec.clusterRef?.name;
    const namespace = testCase.data?.metadata.namespace;
    const jobName = run.data?.status?.jobRef?.name;
    // Metrics scoped to exactly this run's own window.
    const anchor = run.data ? metricsAnchorFromRuns([run.data]) : undefined;
    const status = run.data?.status;
    // "Run now" on a multi-deployment TestCase starts one TestRun per target
    // in the same request - siblings are that test case's runs created within
    // a few seconds of this one.
    const testCaseRuns = useResource(
        () => (testCaseName ? api.testRuns(testCaseName) : Promise.resolve([])),
        [testCaseName],
        10000,
        "TestRun",
    );
    const created = Date.parse(run.data?.metadata.creationTimestamp ?? "");
    const batch = (testCaseRuns.data ?? [])
        .filter(
            (r) =>
                Math.abs(Date.parse(r.metadata.creationTimestamp ?? "") - created) <=
                SIBLING_WINDOW_MS,
        )
        .sort((a, b) =>
            (a.spec.deploymentRef?.name ?? "").localeCompare(b.spec.deploymentRef?.name ?? ""),
        );

    return (
        <Page
            title={<span className="mono">{name}</span>}
            subtitle={
                testCaseName && (
                    <>
                        Run of <Link to={`/testcases/${testCaseName}`}>{testCaseName}</Link>
                        {deploymentName && (
                            <>
                                {" "}
                                against{" "}
                                <Link to={`/deployments/${deploymentName}`}>{deploymentName}</Link>
                            </>
                        )}
                    </>
                )
            }
        >
            <Alert>{run.error}</Alert>
            {batch.length > 1 && (
                <div className="panel chips">
                    <span className="muted">
                        Run now started {batch.length} runs, one per target:
                    </span>
                    {batch.map((r) =>
                        r.metadata.name === name ? (
                            <span
                                key={r.metadata.name}
                                className="chip current"
                                aria-current="page"
                            >
                                {r.spec.deploymentRef?.name ?? r.metadata.name}{" "}
                                <Badge value={r.status?.phase ?? "Pending"} />
                            </span>
                        ) : (
                            <Link
                                key={r.metadata.name}
                                className="chip"
                                to={`/testruns/${r.metadata.name}`}
                            >
                                {r.spec.deploymentRef?.name ?? r.metadata.name}{" "}
                                <Badge value={r.status?.phase ?? "Pending"} />
                            </Link>
                        ),
                    )}
                    <Link to={`/testcases/${testCaseName}`} style={{ marginLeft: "auto" }}>
                        Compare targets
                    </Link>
                </div>
            )}
            {run.data && (
                <div className="panel">
                    <div className="row" style={{ marginBottom: 8 }}>
                        <Badge value={status?.phase ?? "Pending"} />
                        <span className="muted">
                            {formatTimestamp(status?.startTime)} →{" "}
                            {formatTimestamp(status?.completionTime)}
                        </span>
                        {jobName && namespace && (
                            <Link
                                className="button small"
                                to={shellLinkForCluster(
                                    cluster,
                                    `kubectl logs -n ${namespace} job/${jobName} --tail=500`,
                                    namespace,
                                )}
                            >
                                k6 logs
                            </Link>
                        )}
                    </div>
                    <ConditionList conditions={status?.conditions} />
                    <ActivityLog path="testruns" name={name} />
                </div>
            )}
            <h2>Metrics</h2>
            {!anchor || !testCaseName ? (
                <Empty>Run still in progress - metrics will show once it completes.</Empty>
            ) : (
                <MetricsGrid
                    testids={[testCaseName]}
                    endSeconds={anchor.endSeconds}
                    rangeSeconds={anchor.rangeSeconds}
                />
            )}
            <ObjectYaml object={run.data} />
        </Page>
    );
}
