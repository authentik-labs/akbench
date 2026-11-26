import { Alert } from "./ui";

/**
 * queryErrorNotes lists the tests a metrics response could not answer for,
 * as "testid: why", sorted by testid. /metrics/compare and /metrics/range
 * answer 200 as long as one test's Prometheus could be queried and name the
 * rest in `errors` (internal/api/metrics.go's promErrors).
 */
export function queryErrorNotes(...errors: (Record<string, string> | undefined)[]): string[] {
    const merged = new Map<string, string>();
    for (const byTestID of errors) {
        for (const [testid, err] of Object.entries(byTestID ?? {})) {
            merged.set(testid, err);
        }
    }
    return [...merged.entries()]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([testid, err]) => `${testid}: ${err}`);
}

/**
 * QueryErrors says which tests' metrics could not be loaded. Without it such
 * a test only shows "-" or an empty chart, the same as one that reported
 * nothing. Renders nothing when every test answered.
 */
export function QueryErrors({ errors }: { errors: Record<string, string> | undefined }) {
    const notes = queryErrorNotes(errors);
    if (notes.length === 0) {
        return null;
    }
    return (
        <Alert>
            <strong>Could not load metrics for every test.</strong> A missing number below means the
            query failed, not that the test reported nothing.
            <ul>
                {notes.map((note) => (
                    <li key={note}>{note}</li>
                ))}
            </ul>
        </Alert>
    );
}
