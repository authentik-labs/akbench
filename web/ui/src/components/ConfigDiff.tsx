import { api } from "../api";
import { ConfigDatabase, ConfigDiff } from "../types";
import { useResource } from "../useResource";
import { Alert } from "./ui";

/** formatDatabase writes an external database as user@host:port/name. */
export function formatDatabase(db: ConfigDatabase): string {
    const user = db.user ? `${db.user}@` : "";
    const port = db.port ? `:${db.port}` : "";
    const name = db.name ? `/${db.name}` : "";
    return `${user}${db.host ?? ""}${port}${name}`;
}

/**
 * configDiffNotices are the neutral notes about a test case's config diff:
 * what could not be compared, and the external databases, which differ on
 * every healthy pair. A failed fetch is one of them, in place of whatever
 * the last successful one said.
 */
export function configDiffNotices(
    testid: string,
    diff: ConfigDiff | undefined,
    error: string | undefined,
): string[] {
    if (error) {
        return [`Could not compare the deployments of ${testid}: ${error}`];
    }
    if (!diff) {
        return [];
    }
    const notices = (diff.pending ?? []).map(
        (pending) => `Could not compare ${pending.name}: ${pending.reason}`,
    );
    const databases = Object.entries(diff.databases ?? {});
    if (databases.length > 0 && diff.targets.length > 1) {
        const list = databases.map(([name, db]) => `${name} uses ${formatDatabase(db)}`);
        notices.push(
            `Each deployment has its own external database, which is expected: ${list.join("; ")}.`,
        );
    }
    return notices;
}

/**
 * ConfigDiffWarning warns when the deployments a test case compares differ in
 * anything but the build under test: a setting that reached only one side
 * makes the comparison measure that setting too. Renders nothing while they
 * match (internal/api/configdiff.go).
 */
export function ConfigDiffWarning({ testid }: { testid: string }) {
    // Configuration only changes when someone edits a deployment, so a slow
    // poll is enough, with a reload as soon as a deployment changes.
    const diff = useResource(() => api.configDiff(testid), [testid], 30000, "AuthentikDeployment");
    const notices = configDiffNotices(testid, diff.data, diff.error);
    // useResource keeps the last data when a fetch fails; that data may be
    // out of date, so it is not shown as a warning.
    const differences = diff.error ? [] : (diff.data?.differences ?? []);
    const targets = diff.data?.targets ?? [];
    return (
        <>
            {notices.map((notice) => (
                <Alert key={notice} kind="info">
                    {notice}
                </Alert>
            ))}
            {differences.length > 0 && (
                <Alert>
                    <strong>The deployments of {testid} are not configured the same.</strong>{" "}
                    Anything that differs besides the image is measured by the comparison too.
                    <table className="table">
                        <thead>
                            <tr>
                                <th>Setting</th>
                                {targets.map((target) => (
                                    <th key={target}>{target}</th>
                                ))}
                            </tr>
                        </thead>
                        <tbody>
                            {differences.map((difference) => (
                                <tr key={difference.path}>
                                    <td className="mono">
                                        {difference.path}
                                        {difference.note && (
                                            <div className="muted">{difference.note}</div>
                                        )}
                                    </td>
                                    {targets.map((target) => (
                                        <td key={target} className="mono">
                                            {difference.values[target] ?? (
                                                <span className="muted">not set</span>
                                            )}
                                        </td>
                                    ))}
                                </tr>
                            ))}
                        </tbody>
                    </table>
                </Alert>
            )}
        </>
    );
}
