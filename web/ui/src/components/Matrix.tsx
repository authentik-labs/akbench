import { formatMetricValue, relativeDelta, shortTag } from "../format";
import { CompareMetric, CompareResponse, TestCase } from "../types";
import { Badge } from "./Status";
import { Delta } from "./Targets";

import { Link } from "react-router";

/** baselineOf mirrors TestCase.BaselineVariantName (api/v1alpha1/testcase_types.go). */
export function baselineOf(tc: TestCase): string | undefined {
    const variants = tc.spec.variants ?? [];
    return variants.find((v) => v.name === tc.spec.baselineVariant)?.name ?? variants[0]?.name;
}

/**
 * MatrixGrid is a parent test case's row per template (its child TestCase)
 * and column per variant (that row's deployment running it), each cell's
 * value of one metric against the baseline variant's.
 */
export function MatrixGrid({
    parent: m,
    results,
    metric,
    metricKey,
}: {
    parent: TestCase;
    results: CompareResponse["results"];
    metric?: CompareMetric;
    metricKey: string;
}) {
    const baseline = baselineOf(m);
    const valueOf = (testid: string, target: string) =>
        results.find((r) => r.testid === testid && r.target === target)?.values[metricKey];
    return (
        <div className="table-scroll">
            <table className="table">
                <thead>
                    <tr>
                        <th>Template</th>
                        {(m.spec.variants ?? []).map((v) => (
                            <th key={v.name}>
                                {v.name}
                                {v.name === baseline && <span className="muted"> (baseline)</span>}
                                <div className="muted mono" title={v.tag}>
                                    {shortTag(v.tag)}
                                </div>
                            </th>
                        ))}
                    </tr>
                </thead>
                <tbody>
                    {(m.status?.children ?? []).map((row) => {
                        const baselineCell = row.cells?.find((c) => c.variant === baseline);
                        const baselineValue =
                            baselineCell && valueOf(row.testCase, baselineCell.deployment);
                        return (
                            <tr key={row.template}>
                                <td>
                                    <Link to={`/testcases/${row.testCase}`}>{row.template}</Link>{" "}
                                    <Badge value={row.phase} />
                                </td>
                                {(row.cells ?? []).map((cell) => {
                                    const value = valueOf(row.testCase, cell.deployment);
                                    return (
                                        <td key={cell.variant}>
                                            <div>
                                                <strong>
                                                    {formatMetricValue(value, metric?.unit ?? "")}
                                                </strong>{" "}
                                                {metric && cell.variant !== baseline && (
                                                    <Delta
                                                        value={relativeDelta(value, baselineValue)}
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
        </div>
    );
}
