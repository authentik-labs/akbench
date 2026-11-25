import { api } from "../api";
import { ConfigDatabase, ConfigDiff, ConfigDifference, PARENT_LABEL, TestCase } from "../types";
import { useResource } from "../useResource";
import { toast } from "./Toast";
import { Alert, errorMessage } from "./ui";

import { useState } from "react";

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
 * configDiffFor is diff if it is testid's, else undefined. useResource keeps
 * the previous test's result until the new one lands, and that one must not
 * be shown under this test's name.
 */
export function configDiffFor(
    testid: string,
    diff: ConfigDiff | undefined,
): ConfigDiff | undefined {
    return diff?.testid === testid ? diff : undefined;
}

/** configPathUnder mirrors internal/api/configdiff.go's configPathUnder. */
export function configPathUnder(path: string, entry: string): boolean {
    if (!path.startsWith(entry)) {
        return false;
    }
    const rest = path.slice(entry.length);
    return rest === "" || rest[0] === "." || rest[0] === "[";
}

/** withUnderTest is a configUnderTest list that also covers path. */
export function withUnderTest(list: string[] | undefined, path: string): string[] {
    const current = list ?? [];
    return current.some((entry) => configPathUnder(path, entry)) ? current : [...current, path];
}

/** withoutUnderTest is a configUnderTest list that no longer covers path. */
export function withoutUnderTest(list: string[] | undefined, path: string): string[] {
    return (list ?? []).filter((entry) => !configPathUnder(path, entry));
}

/**
 * configUnderTestOwner is the test case whose configUnderTest applies to tc.
 * A child's spec is its parent's, re-rendered over any edit, so a child's
 * list is changed on its parent.
 */
export function configUnderTestOwner(tc: TestCase): string {
    return tc.metadata.labels?.[PARENT_LABEL] || tc.metadata.name;
}

/**
 * saveConfigUnderTest applies change to testid's configUnderTest, on its
 * parent if it has one, and returns the test case it changed.
 */
async function saveConfigUnderTest(
    testid: string,
    change: (list: string[] | undefined) => string[],
): Promise<string> {
    const tc = await api.testCase(testid);
    const owner = configUnderTestOwner(tc);
    const target = owner === testid ? tc : await api.testCase(owner);
    const list = change(target.spec.configUnderTest);
    await api.updateTestCase(owner, {
        spec: { ...target.spec, configUnderTest: list.length > 0 ? list : undefined },
    });
    return owner;
}

function DifferenceTable({
    targets,
    differences,
    action,
    busy,
    onAction,
}: {
    targets: string[];
    differences: ConfigDifference[];
    action: string;
    busy: boolean;
    onAction: (path: string) => void;
}) {
    return (
        <table className="table">
            <thead>
                <tr>
                    <th>Setting</th>
                    {targets.map((target) => (
                        <th key={target}>{target}</th>
                    ))}
                    <th />
                </tr>
            </thead>
            <tbody>
                {differences.map((difference) => (
                    <tr key={difference.path}>
                        <td className="mono">
                            {difference.path}
                            {difference.note && <div className="muted">{difference.note}</div>}
                        </td>
                        {targets.map((target) => (
                            <td key={target} className="mono">
                                {difference.values[target] ?? (
                                    <span className="muted">not set</span>
                                )}
                            </td>
                        ))}
                        <td>
                            <button
                                className="small"
                                disabled={busy}
                                onClick={() => onAction(difference.path)}
                            >
                                {action}
                            </button>
                        </td>
                    </tr>
                ))}
            </tbody>
        </table>
    );
}

/**
 * ConfigDiffWarning warns when the deployments a test case compares differ in
 * anything but the build under test: a setting that reached only one side
 * makes the comparison measure that setting too. A setting the test case
 * compares on purpose (spec.configUnderTest) is listed as under test instead,
 * and one it lists that doesn't differ is warned about. Renders nothing
 * while the deployments match (internal/api/configdiff.go).
 */
export function ConfigDiffWarning({ testid }: { testid: string }) {
    // Configuration only changes when someone edits a deployment, so a slow
    // poll is enough, with a reload as soon as a deployment changes.
    const diff = useResource(() => api.configDiff(testid), [testid], 30000, "AuthentikDeployment");
    const [busy, setBusy] = useState(false);
    const data = configDiffFor(testid, diff.data);
    const notices = configDiffNotices(testid, data, diff.error);
    // useResource keeps the last data when a fetch fails; that data may be
    // out of date, so it is not shown as a warning.
    const all = diff.error ? [] : (data?.differences ?? []);
    const differences = all.filter((difference) => !difference.underTest);
    const underTest = all.filter((difference) => difference.underTest);
    const unchanged = diff.error ? [] : (data?.unchangedUnderTest ?? []);
    const targets = data?.targets ?? [];

    async function save(change: (list: string[] | undefined) => string[], done: string) {
        setBusy(true);
        try {
            const owner = await saveConfigUnderTest(testid, change);
            toast.success(
                owner === testid ? done : `${done} on ${owner}, which ${testid} is part of`,
            );
            diff.reload();
            if (owner !== testid) {
                // The parent passes the change on to its children shortly.
                setTimeout(diff.reload, 2000);
            }
        } catch (err) {
            toast.error(errorMessage(err));
        } finally {
            setBusy(false);
        }
    }

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
                    Anything that differs besides the image is measured by the comparison too. Mark
                    a setting as under test if comparing it is the point of the test.
                    <DifferenceTable
                        targets={targets}
                        differences={differences}
                        action="Mark as under test"
                        busy={busy}
                        onAction={(path) =>
                            save(
                                (list) => withUnderTest(list, path),
                                `Marked ${path} as under test`,
                            )
                        }
                    />
                </Alert>
            )}
            {unchanged.length > 0 && (
                <Alert>
                    <strong>
                        {testid} is meant to compare settings its deployments don&apos;t differ on.
                    </strong>{" "}
                    The comparison doesn&apos;t test them:
                    <ul>
                        {unchanged.map((entry) => (
                            <li key={entry}>
                                <span className="mono">{entry}</span>{" "}
                                <button
                                    className="small"
                                    disabled={busy}
                                    onClick={() =>
                                        save(
                                            (list) => (list ?? []).filter((e) => e !== entry),
                                            `${entry} is no longer under test`,
                                        )
                                    }
                                >
                                    Not under test
                                </button>
                            </li>
                        ))}
                    </ul>
                </Alert>
            )}
            {underTest.length > 0 && (
                <Alert kind="info">
                    <strong>Under test:</strong> {testid} compares these settings on purpose.
                    <DifferenceTable
                        targets={targets}
                        differences={underTest}
                        action="Not under test"
                        busy={busy}
                        onAction={(path) =>
                            save(
                                (list) => withoutUnderTest(list, path),
                                `${path} is no longer under test`,
                            )
                        }
                    />
                </Alert>
            )}
        </>
    );
}
