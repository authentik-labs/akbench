import { api } from "../api";
import { ConfigDatabase, ConfigDiff, ConfigDifference, PARENT_LABEL, TestCase } from "../types";
import { useResource } from "../useResource";
import { toast } from "./Toast";
import { Alert, errorMessage, Field } from "./ui";

import { Fragment, useEffect, useRef, useState } from "react";
import { Link } from "react-router";

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
 * every healthy pair. A deployment that is only still being installed is not
 * one: every new comparison goes through that. A failed fetch is one of
 * them, in place of whatever the last successful one said.
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
    const notices = (diff.pending ?? [])
        .filter((pending) => !pending.waiting)
        .map((pending) => `Could not compare ${pending.name}: ${pending.reason}`);
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

/**
 * withoutUnderTest is a configUnderTest list that no longer covers path. An
 * entry that covered more than path (values.postgresql, for
 * values.postgresql.auth.database) is replaced by the others of underTest,
 * the paths under test now, that it covered: they stay under test.
 */
export function withoutUnderTest(
    list: string[] | undefined,
    path: string,
    underTest: string[] = [],
): string[] {
    const next: string[] = [];
    for (const entry of list ?? []) {
        if (!configPathUnder(path, entry)) {
            next.push(entry);
            continue;
        }
        for (const other of underTest) {
            if (other !== path && !configPathUnder(other, path) && configPathUnder(other, entry)) {
                next.push(other);
            }
        }
    }
    return [...new Set(next)];
}

/** parseConfigUnderTest reads a list typed one path per line, or with commas. */
export function parseConfigUnderTest(text: string): string[] {
    return [
        ...new Set(
            text
                .split(/[\n,]/)
                .map((entry) => entry.trim())
                .filter(Boolean),
        ),
    ];
}

/** The most differences a group shows before it is folded away. */
const GROUP_FOLD = 3;

/**
 * configGroupPrefix is the part of the configuration path belongs to: the
 * chart's top-level key for a Helm value (values.postgresql), else its first
 * segment (cnpg, chart, spec).
 */
export function configGroupPrefix(path: string): string {
    const segments = path.split(/[.[]/);
    return segments.slice(0, segments[0] === "values" ? 2 : 1).join(".");
}

export interface ConfigDifferenceGroup {
    prefix: string;
    differences: ConfigDifference[];
}

/**
 * groupDifferences puts differences that belong to the same part of the
 * configuration together, in their own order: one change, like another
 * postgres mode, differs in dozens of settings at once.
 */
export function groupDifferences(differences: ConfigDifference[]): ConfigDifferenceGroup[] {
    const groups = new Map<string, ConfigDifference[]>();
    for (const difference of differences) {
        const prefix = configGroupPrefix(difference.path);
        groups.set(prefix, [...(groups.get(prefix) ?? []), difference]);
    }
    return [...groups].map(([prefix, grouped]) => ({ prefix, differences: grouped }));
}

/** The longest value shown in full; a longer one is cut, with the rest on hover. */
const VALUE_LENGTH = 80;

/**
 * configDiffSummary says in a few words what is wrong with a config diff, or
 * undefined when nothing is.
 */
export function configDiffSummary(diff: ConfigDiff): string | undefined {
    const differing = diff.differences.filter((difference) => !difference.underTest).length;
    const unchanged = diff.unchangedUnderTest?.length ?? 0;
    const unread = (diff.pending ?? []).filter((pending) => !pending.waiting).length;
    const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;
    const parts = [
        differing > 0 && `${plural(differing, "setting differs", "settings differ")}`,
        unchanged > 0 &&
            `${plural(unchanged, "setting under test doesn't differ", "settings under test don't differ")}`,
        unread > 0 && `${plural(unread, "deployment", "deployments")} could not be compared`,
    ].filter(Boolean);
    return parts.length > 0 ? parts.join(", ") : undefined;
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

function DifferenceRows({
    targets,
    differences,
    action,
    busy,
    onAction,
}: {
    targets: string[];
    differences: ConfigDifference[];
    action?: string;
    busy: boolean;
    onAction: (path: string) => void;
}) {
    return differences.map((difference) => (
        <tr key={difference.path}>
            <td className="mono">
                {difference.path}
                {difference.note && <div className="muted">{difference.note}</div>}
            </td>
            {targets.map((target) => {
                const value = difference.values[target];
                return (
                    <td key={target} className="mono" title={value}>
                        {value === undefined ? (
                            <span className="muted">not set</span>
                        ) : value.length > VALUE_LENGTH ? (
                            `${value.slice(0, VALUE_LENGTH)}…`
                        ) : (
                            value
                        )}
                    </td>
                );
            })}
            {action && (
                <td>
                    <button
                        className="small"
                        disabled={busy}
                        onClick={() => onAction(difference.path)}
                    >
                        {action}
                    </button>
                </td>
            )}
        </tr>
    ));
}

/**
 * DifferenceTable lists differences by the part of the configuration they
 * belong to. A part with many is folded to one row, which acts on all of
 * them at once; opening it lists them. Without an action it is read-only.
 */
function DifferenceTable({
    targets,
    differences,
    action,
    groupAction,
    busy,
    onAction,
}: {
    targets: string[];
    differences: ConfigDifference[];
    action?: string;
    groupAction?: string;
    busy: boolean;
    onAction: (path: string) => void;
}) {
    const [open, setOpen] = useState<string[]>([]);
    const rows = { targets, action, busy, onAction };
    return (
        <table className="table">
            <thead>
                <tr>
                    <th>Setting</th>
                    {targets.map((target) => (
                        <th key={target}>{target}</th>
                    ))}
                    {action && <th />}
                </tr>
            </thead>
            <tbody>
                {groupDifferences(differences).map((group) => {
                    if (group.differences.length <= GROUP_FOLD) {
                        return (
                            <DifferenceRows
                                key={group.prefix}
                                {...rows}
                                differences={group.differences}
                            />
                        );
                    }
                    const shown = open.includes(group.prefix);
                    return (
                        <Fragment key={group.prefix}>
                            <tr>
                                <td className="mono">{group.prefix}</td>
                                <td colSpan={targets.length}>
                                    {group.differences.length} settings differ{" "}
                                    <button
                                        className="small"
                                        aria-expanded={shown}
                                        aria-label={`${shown ? "Hide" : "Show"} the settings under ${group.prefix}`}
                                        onClick={() =>
                                            setOpen(
                                                shown
                                                    ? open.filter((p) => p !== group.prefix)
                                                    : [...open, group.prefix],
                                            )
                                        }
                                    >
                                        {shown ? "Hide" : "Show"}
                                    </button>
                                </td>
                                {action && (
                                    <td>
                                        <button
                                            className="small"
                                            disabled={busy}
                                            onClick={() => onAction(group.prefix)}
                                        >
                                            {groupAction ?? action}
                                        </button>
                                    </td>
                                )}
                            </tr>
                            {shown && <DifferenceRows {...rows} differences={group.differences} />}
                        </Fragment>
                    );
                })}
            </tbody>
        </table>
    );
}

/**
 * ConfigDiffChildren is a parent test case's warning: which of its children
 * compare deployments that are not configured the same, each linked to its
 * own page, where the differences are listed and can be marked.
 */
function ConfigDiffChildren({ testid, items }: { testid: string; items: ConfigDiff[] }) {
    const flagged = items.flatMap((child) => {
        const summary = configDiffSummary(child);
        return summary ? [{ testid: child.testid, summary }] : [];
    });
    if (flagged.length === 0) {
        return null;
    }
    return (
        <Alert>
            <strong>
                {flagged.length} of {testid}&apos;s {items.length}{" "}
                {items.length === 1 ? "test compares" : "tests compare"} deployments that are not
                set up as declared.
            </strong>{" "}
            Anything that differs besides the image is measured by the comparison too. Open a test
            to see the settings, or to mark them as under test.
            <ul>
                {flagged.map((child) => (
                    <li key={child.testid}>
                        <Link to={`/testcases/${child.testid}`}>{child.testid}</Link>:{" "}
                        {child.summary}
                    </li>
                ))}
            </ul>
        </Alert>
    );
}

/**
 * ConfigDiffWarning warns when the deployments a test case compares differ in
 * anything but the build under test: a setting that reached only one side
 * makes the comparison measure that setting too. A setting the test case
 * compares on purpose (spec.configUnderTest) is listed as under test instead,
 * and one it lists that doesn't differ is warned about. A parent test case
 * gets a summary of its children. Renders nothing while the deployments
 * match (internal/api/configdiff.go).
 */
export function ConfigDiffWarning({ testid }: { testid: string }) {
    // Configuration only changes when someone edits a deployment, so a slow
    // poll is enough, with a reload as soon as a deployment changes.
    const diff = useResource(() => api.configDiff(testid), [testid], 30000, "AuthentikDeployment");
    const [busy, setBusy] = useState(false);
    const later = useRef<ReturnType<typeof setTimeout>>(undefined);
    useEffect(() => () => clearTimeout(later.current), []);
    const data = configDiffFor(testid, diff.data);

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
                clearTimeout(later.current);
                later.current = setTimeout(diff.reload, 2000);
            }
        } catch (err) {
            toast.error(errorMessage(err));
        } finally {
            setBusy(false);
        }
    }

    return (
        <>
            {data?.children && !diff.error && (
                <ConfigDiffChildren testid={testid} items={data.children} />
            )}
            <ConfigDiffAlerts
                subject={`The deployments of ${testid}`}
                testid={testid}
                data={data}
                error={diff.error}
                busy={busy}
                save={save}
            />
        </>
    );
}

/**
 * ConfigDiffAcross is the warning for tests put side by side: one test
 * case's own (ConfigDiffWarning), or one comparison across every deployment
 * the tests run on, which is read-only since no one test case owns it.
 */
export function ConfigDiffAcross({ testids }: { testids: string[] }) {
    if (testids.length === 1) {
        return <ConfigDiffWarning testid={testids[0]} />;
    }
    return testids.length > 1 ? <ConfigDiffAcrossTests testids={testids} /> : null;
}

function ConfigDiffAcrossTests({ testids }: { testids: string[] }) {
    const key = testids.join(",");
    const diff = useResource(
        () => api.configDiffAcross(testids),
        [key],
        30000,
        "AuthentikDeployment",
    );
    return (
        <ConfigDiffAlerts
            subject="The deployments of the selected tests"
            testid={key}
            data={configDiffFor(key, diff.data)}
            error={diff.error}
            busy={false}
        />
    );
}

/**
 * ConfigDiffAlerts renders a config diff: its notices, the differences that
 * are a problem, and the ones under test. Without save it is read-only.
 */
function ConfigDiffAlerts({
    subject,
    testid,
    data,
    error,
    busy,
    save,
}: {
    subject: string;
    testid: string;
    data: ConfigDiff | undefined;
    error: string | undefined;
    busy: boolean;
    save?: (change: (list: string[] | undefined) => string[], done: string) => void;
}) {
    const notices = configDiffNotices(testid, data, error);
    // useResource keeps the last data when a fetch fails; that data may be
    // out of date, so it is not shown as a warning.
    const all = error ? [] : (data?.differences ?? []);
    const differences = all.filter((difference) => !difference.underTest);
    const underTest = all.filter((difference) => difference.underTest);
    const unchanged = error ? [] : (data?.unchangedUnderTest ?? []);
    const targets = data?.targets ?? [];

    return (
        <>
            {notices.map((notice) => (
                <Alert key={notice} kind="info">
                    {notice}
                </Alert>
            ))}
            {differences.length > 0 && (
                <Alert>
                    <strong>{subject} are not configured the same.</strong> Anything that differs
                    besides the image is measured by the comparison too.{" "}
                    {save
                        ? "Mark a setting as under test if comparing it is the point of the test."
                        : "A setting is under test here only if every one of the tests says so; open a test to mark it."}
                    <DifferenceTable
                        targets={targets}
                        differences={differences}
                        action={save && "Mark as under test"}
                        groupAction="Mark all as under test"
                        busy={busy}
                        onAction={(path) =>
                            save?.(
                                (list) => withUnderTest(withoutCovered(list, path), path),
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
                                {save && (
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
                                )}
                            </li>
                        ))}
                    </ul>
                </Alert>
            )}
            {underTest.length > 0 && (
                <Alert kind="info">
                    <strong>Under test:</strong>{" "}
                    {save
                        ? `${testid} compares these settings on purpose.`
                        : "every selected test compares these settings on purpose."}
                    <DifferenceTable
                        targets={targets}
                        differences={underTest}
                        action={save && "Not under test"}
                        groupAction="None under test"
                        busy={busy}
                        onAction={(path) =>
                            save?.(
                                (list) =>
                                    withoutUnderTest(
                                        withoutCovered(list, path),
                                        path,
                                        underTest.map((difference) => difference.path),
                                    ),
                                `${path} is no longer under test`,
                            )
                        }
                    />
                </Alert>
            )}
        </>
    );
}

/**
 * withoutCovered is list without the entries below path, which an action on
 * path as a whole (a folded group's) makes redundant.
 */
export function withoutCovered(list: string[] | undefined, path: string): string[] {
    return (list ?? []).filter((entry) => entry === path || !configPathUnder(entry, path));
}

/**
 * ConfigUnderTestField edits a test case's configUnderTest in its form, so a
 * comparison of two setups can say what it compares before it first runs.
 */
export function ConfigUnderTestField({
    value,
    onChange,
}: {
    value: string[] | undefined;
    onChange: (next: string[] | undefined) => void;
}) {
    // Kept as typed, so a line being written isn't trimmed away under the cursor.
    const [text, setText] = useState((value ?? []).join("\n"));
    return (
        <Field
            label="Settings under test"
            help={
                <>
                    Only for a test that compares setups rather than builds: the settings its
                    deployments differ in on purpose, one per line, as the test&apos;s page names
                    them (<span className="mono">values.server.replicas</span>,{" "}
                    <span className="mono">values.authentik.postgresql</span> for everything below
                    it, <span className="mono">spec.postgres.mode</span>). Any other difference is
                    warned about. The image is always expected to differ.
                </>
            }
        >
            <textarea
                rows={3}
                className="mono"
                value={text}
                placeholder="values.server.replicas"
                onChange={(e) => {
                    setText(e.target.value);
                    const next = parseConfigUnderTest(e.target.value);
                    onChange(next.length > 0 ? next : undefined);
                }}
            />
        </Field>
    );
}
