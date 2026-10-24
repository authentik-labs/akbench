import { api } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { ObjectYaml } from "../components/ObjectYaml";
import { Badge, StatusBadge } from "../components/Status";
import { Delta, TARGET_METRICS } from "../components/Targets";
import { toast } from "../components/Toast";
import { Alert, confirmAndRun, errorMessage, Page, Segmented, Table } from "../components/ui";
import { formatMetricValue, relativeDelta, shortTag } from "../format";
import { TestMatrix } from "../types";
import { useResource } from "../useResource";

import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router";

const WINDOWS = ["5m", "1h", "6h"].map((w) => ({ value: w, label: w }));

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

export function TestMatricesPage() {
    const matrices = useResource(() => api.testMatrices(), [], 5000, "TestMatrix");
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
            <Table
                rows={matrices.data}
                rowKey={(m) => m.metadata.name}
                empty="No test matrices yet."
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
                            <button
                                className="small danger"
                                onClick={() => void deleteMatrix(m.metadata.name, matrices.reload)}
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

/**
 * TestMatrixViewPage shows the matrix as a grid: a row per template (its
 * TestCase), a column per variant (that row's deployment running it), and
 * each cell's value of one metric against the baseline variant's.
 */
export function TestMatrixViewPage() {
    const { name = "" } = useParams();
    const navigate = useNavigate();
    const matrix = useResource(() => api.testMatrix(name), [name], 10000, "TestMatrix");
    const m = matrix.data;
    const [metricKey, setMetricKey] = useState("iteration_duration_p95");
    const [window, setWindow] = useState("5m");
    const [saving, setSaving] = useState(false);

    const testids = (m?.status?.rows ?? []).map((r) => r.testCase);
    const comparison = useResource(
        () => (testids.length ? api.compare(testids, window) : Promise.resolve(undefined)),
        [testids.join(","), window],
        15000,
    );
    const metrics = (comparison.data?.metrics ?? []).filter((x) => TARGET_METRICS.includes(x.key));
    const metric = metrics.find((x) => x.key === metricKey);
    const valueOf = (testid: string, target: string) =>
        comparison.data?.results.find((r) => r.testid === testid && r.target === target)?.values[
            metricKey
        ];

    const baseline = m ? baselineOf(m) : undefined;

    async function setArchived(archived: boolean) {
        if (!m) {
            return;
        }
        setSaving(true);
        try {
            await api.updateTestMatrix(name, {
                spec: { ...m.spec, testCase: { ...m.spec.testCase, archived } },
            });
            toast.success(archived ? `Archived ${name}` : `Unarchived ${name}`);
            matrix.reload();
        } catch (err) {
            toast.error(errorMessage(err));
        } finally {
            setSaving(false);
        }
    }

    return (
        <Page
            title={name}
            subtitle={m && `${m.spec.variants.map((v) => v.name).join(" vs ")}`}
            actions={
                m && (
                    <span className="row">
                        <button
                            disabled={saving}
                            onClick={() => void setArchived(!m.spec.testCase.archived)}
                        >
                            {m.spec.testCase.archived ? "Unarchive" : "Archive"}
                        </button>
                        <button
                            className="danger"
                            onClick={() => void deleteMatrix(name, () => navigate("/testmatrices"))}
                        >
                            Delete
                        </button>
                    </span>
                )
            }
        >
            <Alert>{matrix.error}</Alert>
            {m && (
                <div className="row" style={{ marginBottom: 8 }}>
                    <StatusBadge
                        kind="testmatrix"
                        name={name}
                        namespace={m.metadata.namespace}
                        phase={m.status?.phase}
                        conditions={m.status?.conditions}
                    />
                </div>
            )}
            {m && (
                <>
                    <div className="row" style={{ marginBottom: 8 }}>
                        <select value={metricKey} onChange={(e) => setMetricKey(e.target.value)}>
                            {metrics.map((x) => (
                                <option key={x.key} value={x.key}>
                                    {x.label}
                                </option>
                            ))}
                        </select>
                        <Segmented
                            label="Window"
                            options={WINDOWS}
                            value={window}
                            onChange={setWindow}
                        />
                    </div>
                    <Alert>{comparison.error}</Alert>
                    <table className="table">
                        <thead>
                            <tr>
                                <th>Template</th>
                                {m.spec.variants.map((v) => (
                                    <th key={v.name}>
                                        {v.name}
                                        {v.name === baseline && (
                                            <span className="muted"> (baseline)</span>
                                        )}
                                        <div className="muted mono" title={v.tag}>
                                            {shortTag(v.tag)}
                                        </div>
                                    </th>
                                ))}
                            </tr>
                        </thead>
                        <tbody>
                            {(m.status?.rows ?? []).map((row) => {
                                const baselineCell = row.cells?.find((c) => c.variant === baseline);
                                const baselineValue =
                                    baselineCell && valueOf(row.testCase, baselineCell.deployment);
                                return (
                                    <tr key={row.template}>
                                        <td>
                                            <Link to={`/testcases/${row.testCase}`}>
                                                {row.template}
                                            </Link>{" "}
                                            <Badge value={row.phase} />
                                        </td>
                                        {(row.cells ?? []).map((cell) => {
                                            const value = valueOf(row.testCase, cell.deployment);
                                            return (
                                                <td key={cell.variant}>
                                                    <div>
                                                        <strong>
                                                            {formatMetricValue(
                                                                value,
                                                                metric?.unit ?? "",
                                                            )}
                                                        </strong>{" "}
                                                        {metric && cell.variant !== baseline && (
                                                            <Delta
                                                                value={relativeDelta(
                                                                    value,
                                                                    baselineValue,
                                                                )}
                                                                better={metric.better}
                                                            />
                                                        )}
                                                    </div>
                                                    <Link
                                                        className="muted"
                                                        to={`/deployments/${cell.deployment}`}
                                                    >
                                                        {cell.deployment}
                                                    </Link>{" "}
                                                    <Badge value={cell.phase} />
                                                </td>
                                            );
                                        })}
                                    </tr>
                                );
                            })}
                        </tbody>
                    </table>
                    <div className="panel">
                        <ActivityLog path="testmatrices" name={name} />
                    </div>
                </>
            )}
            <ObjectYaml object={m} />
        </Page>
    );
}

/** baselineOf mirrors TestMatrix.BaselineVariant (api/v1alpha1/testmatrix_types.go). */
export function baselineOf(m: TestMatrix): string | undefined {
    return (
        m.spec.variants.find((v) => v.name === m.spec.baseline)?.name ?? m.spec.variants[0]?.name
    );
}
