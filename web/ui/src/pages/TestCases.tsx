import { api } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { CodeEditor } from "../components/CodeEditor";
import { LogBreakdownSection } from "../components/LogBreakdown";
import { MetricsGrid, ResourceCharts } from "../components/MetricsChart";
import { ObjectYaml } from "../components/ObjectYaml";
import { ProfileBreakdownSection } from "../components/ProfileBreakdown";
import { Badge, ConditionList, StatusBadge } from "../components/Status";
import { buildTargetRows, burningFor, TargetTable } from "../components/Targets";
import { toast } from "../components/Toast";
import { TraceBreakdownSection } from "../components/TraceBreakdown";
import {
    Alert,
    confirmAndRun,
    Empty,
    errorMessage,
    Field,
    formatTimestamp,
    Loading,
    Page,
    Section,
    SectionNav,
    Table,
    TimeRangeSelect,
} from "../components/ui";
import {
    archivedBurnWindow,
    earliestBurnStart,
    formatDuration,
    metricsAnchorFromRuns,
    parseTimeRange,
    shortTag,
    SINCE_START,
    timeRangeSeconds,
} from "../format";
import { shellLinkForCluster } from "../shell/links";
import {
    TestCase,
    TestCaseSpec,
    TestProfile,
    testCaseBaseline,
    testCaseDeployments,
    testRunDeployment,
} from "../types";
import { useResource } from "../useResource";

import { FormEvent, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";

function setArchived(tc: TestCase, archived: boolean, reload: () => void) {
    const name = tc.metadata.name;
    return confirmAndRun(
        {
            title: archived ? "Archive test case" : "Unarchive test case",
            message: archived
                ? `Archive test case "${name}"? It stops running but stays listed with its past runs.`
                : `Unarchive test case "${name}"?${tc.spec.profile === "Burn" ? " Its burn starts again." : ""}`,
            confirmLabel: archived ? "Archive" : "Unarchive",
        },
        () => api.updateTestCase(name, { spec: { ...tc.spec, archived } }),
        reload,
        archived ? `Archived ${name}` : `Unarchived ${name}`,
    );
}

export function TestCasesPage() {
    const testCases = useResource(() => api.testCases(), [], 5000, "TestCase");

    function deleteTestCase(tc: TestCase) {
        const name = tc.metadata.name;
        // Only offer the deployments no other test case drives - deleting a
        // shared one would pull it out from under that test case too.
        const usedElsewhere = new Set(
            (testCases.data ?? [])
                .filter((other) => other.metadata.name !== name)
                .flatMap((other) => testCaseDeployments(other.spec).map((d) => d.name)),
        );
        const own = testCaseDeployments(tc.spec)
            .map((d) => d.name)
            .filter((d) => !usedElsewhere.has(d));
        const shared = testCaseDeployments(tc.spec).length - own.length;
        return confirmAndRun(
            {
                title: "Delete test case",
                message: `Delete test case "${name}"?`,
                confirmLabel: "Delete",
                danger: true,
                checkbox:
                    own.length > 0
                        ? {
                              label:
                                  `Also delete its ${own.length === 1 ? "deployment" : `${own.length} deployments`} (${own.join(", ")})` +
                                  (shared > 0
                                      ? ` - ${shared} shared with other test cases stay`
                                      : ""),
                          }
                        : undefined,
            },
            async (withDeployments) => {
                await api.deleteTestCase(name);
                if (withDeployments) {
                    await Promise.all(own.map((d) => api.deleteDeployment(d)));
                }
            },
            testCases.reload,
            `Deleted ${name}`,
        );
    }

    async function runNow(name: string) {
        try {
            const runs = await api.runTestCase(name);
            toast.success(
                runs.length > 1
                    ? `Started ${runs.length} test runs (one per deployment)`
                    : `Started test run ${runs[0]?.metadata.name ?? ""}`,
            );
        } catch (err) {
            toast.error(errorMessage(err));
        }
    }

    function table(rows: TestCase[] | undefined, empty: string) {
        return (
            <Table
                rows={rows}
                rowKey={(tc) => tc.metadata.name}
                empty={empty}
                columns={[
                    {
                        label: "Name",
                        render: (tc) => (
                            <Link to={`/testcases/${tc.metadata.name}`}>{tc.metadata.name}</Link>
                        ),
                    },
                    {
                        label: "Deployment",
                        render: (tc) => (
                            <span className="row" style={{ gap: 4 }}>
                                {testCaseDeployments(tc.spec).map((ref, i) => (
                                    <span key={ref.name}>
                                        {i > 0 && ", "}
                                        <Link to={`/deployments/${ref.name}`}>{ref.name}</Link>
                                    </span>
                                ))}
                            </span>
                        ),
                    },
                    {
                        label: "Template",
                        render: (tc) =>
                            tc.spec.inline !== undefined ? (
                                <em>inline</em>
                            ) : (
                                (tc.spec.template ?? "-")
                            ),
                    },
                    { label: "Profile", render: (tc) => <Badge value={tc.spec.profile} /> },
                    { label: "VUs", render: (tc) => tc.spec.vus },
                    {
                        label: "Status",
                        render: (tc) => (
                            <StatusBadge
                                kind="testcase"
                                name={tc.metadata.name}
                                namespace={tc.metadata.namespace}
                                phase={tc.status?.phase}
                                conditions={tc.status?.conditions}
                            />
                        ),
                    },
                    {
                        label: "",
                        className: "actions",
                        render: (tc) => (
                            <span className="row" style={{ justifyContent: "flex-end" }}>
                                {tc.spec.profile === "Quick" && !tc.spec.archived && (
                                    <button
                                        className="small"
                                        onClick={() => void runNow(tc.metadata.name)}
                                    >
                                        Run now
                                    </button>
                                )}
                                <Link
                                    className="button small"
                                    to={`/testcases/${tc.metadata.name}/edit`}
                                >
                                    Edit
                                </Link>
                                <button
                                    className="small"
                                    onClick={() =>
                                        void setArchived(tc, !tc.spec.archived, testCases.reload)
                                    }
                                >
                                    {tc.spec.archived ? "Unarchive" : "Archive"}
                                </button>
                                {tc.spec.archived && (
                                    <button
                                        className="small danger"
                                        onClick={() => void deleteTestCase(tc)}
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
    }

    return (
        <Page
            title="Test cases"
            subtitle="k6 load tests against one or more deployments. Burn runs until stopped; Quick runs once per test run."
            actions={
                <>
                    <Link className="button" to="/pull-requests/new">
                        Compare a PR
                    </Link>
                    <Link className="button primary" to="/testcases/new">
                        New test case
                    </Link>
                </>
            }
        >
            <Alert>{testCases.error}</Alert>
            <Section id="active" title="Active">
                {table(
                    testCases.data?.filter((tc) => !tc.spec.archived),
                    "No active test cases.",
                )}
            </Section>
            <Section id="archived" title="Archived">
                {table(
                    testCases.data?.filter((tc) => tc.spec.archived),
                    "No archived test cases.",
                )}
            </Section>
        </Page>
    );
}

const INLINE_TEMPLATE = "__inline__";

const PROFILES: { value: TestProfile; title: string; description: string }[] = [
    {
        value: "Burn",
        title: "Burn",
        description:
            "Keeps every target under steady load until you stop it. Change a deployment while it runs and watch the difference live.",
    },
    {
        value: "Quick",
        title: "Quick",
        description:
            "A bounded number of logins per virtual user, started with Run now - one test run per target.",
    },
];

function emptySpec(): TestCaseSpec {
    return {
        deploymentRefs: [],
        template: "login",
        profile: "Burn",
        vus: 8,
        iterations: 1000,
        flow: "default-authentication-flow",
        quickDuration: "150s",
        burnDuration: "8760h",
        traceSampleRate: "0.01",
    };
}

function TestCaseForm({ existing }: { existing?: TestCase }) {
    const navigate = useNavigate();
    const deployments = useResource(() => api.deployments(), [], 0);
    const templates = useResource(() => api.testCaseTemplates(), [], 0);
    const [name, setName] = useState(existing?.metadata.name ?? "");
    const [spec, setSpec] = useState<TestCaseSpec>(existing?.spec ?? emptySpec());
    const [error, setError] = useState<string>();
    const [saving, setSaving] = useState(false);

    const patch = (changes: Partial<TestCaseSpec>) =>
        setSpec((current) => ({ ...current, ...changes }));

    const selectedDeployments = testCaseDeployments(spec).map((r) => r.name);
    function toggleDeployment(deploymentName: string) {
        const next = selectedDeployments.includes(deploymentName)
            ? selectedDeployments.filter((n) => n !== deploymentName)
            : [...selectedDeployments, deploymentName];
        // Always write deploymentRefs going forward and drop the deprecated
        // singular field, even if it's what this TestCase was loaded with -
        // any edit through this form migrates it cleanly.
        patch({
            deploymentRefs: next.map((n) => ({ name: n })),
            deploymentRef: undefined,
            baselineRef:
                spec.baselineRef && next.includes(spec.baselineRef.name)
                    ? spec.baselineRef
                    : undefined,
        });
    }
    const baselineChoice = testCaseBaseline(spec);

    // scriptConfigMapRef (bring-your-own ConfigMap) is a power-user escape
    // hatch deliberately not offered here - it survives a save untouched.
    const scriptChoice = spec.inline !== undefined ? INLINE_TEMPLATE : (spec.template ?? "login");

    async function submit(event: FormEvent) {
        event.preventDefault();
        if (selectedDeployments.length === 0) {
            setError("Select at least one deployment to drive");
            return;
        }
        setSaving(true);
        setError(undefined);
        try {
            const savedName = existing?.metadata.name ?? name;
            if (existing) {
                await api.updateTestCase(savedName, { spec });
                toast.success(`Saved ${savedName}`);
            } else {
                await api.createTestCase({ metadata: { name }, spec });
                toast.success(`Created ${savedName}`);
            }
            navigate(`/testcases/${savedName}`);
        } catch (err) {
            setError(errorMessage(err));
            setSaving(false);
        }
    }

    return (
        <Page title={existing ? `Edit ${existing.metadata.name}` : "New test case"}>
            <form onSubmit={submit}>
                <Alert>{error}</Alert>
                <fieldset>
                    <legend>Identity</legend>
                    <div className="fields">
                        <Field label="Name" help="Also the k6 testid every metric is split on">
                            <input
                                required
                                value={name}
                                disabled={Boolean(existing)}
                                onChange={(e) => setName(e.target.value)}
                            />
                        </Field>
                        <div className="field wide">
                            <span id="targets-label">Target deployments</span>
                            <div className="checklist" role="group" aria-labelledby="targets-label">
                                {(deployments.data ?? []).map((d) => (
                                    <label key={d.metadata.name}>
                                        <input
                                            type="checkbox"
                                            checked={selectedDeployments.includes(d.metadata.name)}
                                            onChange={() => toggleDeployment(d.metadata.name)}
                                        />
                                        <span className="mono" title={d.metadata.name}>
                                            {d.metadata.name}
                                        </span>
                                        <span className="meta" title={d.spec.authentik?.tag}>
                                            {d.spec.authentik?.tag
                                                ? shortTag(d.spec.authentik.tag)
                                                : "chart default"}
                                        </span>
                                    </label>
                                ))}
                                {deployments.data?.length === 0 && (
                                    <span className="muted">No deployments yet.</span>
                                )}
                            </div>
                            <span className="help">
                                Same script and load against each, tagged with its own k6 target.
                                Pick two or more to compare versions or setups side by side; Run now
                                starts one test run per target.
                            </span>
                        </div>
                        {selectedDeployments.length > 1 && (
                            <Field
                                label="Baseline"
                                help="The target the others are compared against"
                            >
                                <select
                                    value={baselineChoice}
                                    onChange={(e) =>
                                        patch({ baselineRef: { name: e.target.value } })
                                    }
                                >
                                    {selectedDeployments.map((n) => (
                                        <option key={n} value={n}>
                                            {n}
                                        </option>
                                    ))}
                                </select>
                            </Field>
                        )}
                    </div>
                </fieldset>

                <fieldset>
                    <legend>Test template</legend>
                    <div className="fields">
                        <Field
                            label="Template"
                            help={<Link to="/testcasetemplates">Manage test case templates</Link>}
                        >
                            <select
                                value={scriptChoice}
                                onChange={(e) => {
                                    const value = e.target.value;
                                    if (value === INLINE_TEMPLATE) {
                                        patch({ inline: spec.inline ?? "", template: undefined });
                                    } else {
                                        patch({ template: value, inline: undefined });
                                    }
                                }}
                            >
                                {(templates.data ?? []).map((t) => (
                                    <option key={t.metadata.name} value={t.metadata.name}>
                                        {t.metadata.name}
                                        {t.spec.description ? ` - ${t.spec.description}` : ""}
                                    </option>
                                ))}
                                <option value={INLINE_TEMPLATE}>Custom - inline k6 script</option>
                            </select>
                        </Field>
                    </div>
                    {scriptChoice === INLINE_TEMPLATE && (
                        <>
                            <p className="muted">
                                Inline k6 script (TypeScript), stored on this TestCase.
                            </p>
                            <CodeEditor
                                language="typescript"
                                value={spec.inline ?? ""}
                                onChange={(v) => patch({ inline: v })}
                            />
                        </>
                    )}
                </fieldset>

                <fieldset>
                    <legend>Load</legend>
                    <div className="choice-cards" role="radiogroup" aria-label="Profile">
                        {PROFILES.map((p) => (
                            <label className="choice-card" key={p.value}>
                                <input
                                    type="radio"
                                    name="profile"
                                    value={p.value}
                                    checked={spec.profile === p.value}
                                    onChange={() => patch({ profile: p.value })}
                                />
                                <span>
                                    <strong>{p.title}</strong>
                                    <span className="muted">{p.description}</span>
                                </span>
                            </label>
                        ))}
                    </div>
                    <div className="fields">
                        <Field label="Virtual users">
                            <input
                                type="number"
                                min={1}
                                value={spec.vus}
                                onChange={(e) => patch({ vus: Number(e.target.value) })}
                            />
                        </Field>
                        {spec.profile === "Quick" ? (
                            <>
                                <Field label="Logins per virtual user">
                                    <input
                                        type="number"
                                        min={1}
                                        value={spec.iterations ?? 1000}
                                        onChange={(e) =>
                                            patch({ iterations: Number(e.target.value) })
                                        }
                                    />
                                </Field>
                                <Field
                                    label="Time limit"
                                    help="Caps a run that cannot finish its logins"
                                >
                                    <input
                                        value={spec.quickDuration ?? ""}
                                        placeholder="150s"
                                        onChange={(e) => patch({ quickDuration: e.target.value })}
                                    />
                                </Field>
                            </>
                        ) : (
                            <Field label="Time limit">
                                <input
                                    value={spec.burnDuration ?? ""}
                                    placeholder="8760h"
                                    onChange={(e) => patch({ burnDuration: e.target.value })}
                                />
                            </Field>
                        )}
                        <Field label="Flow slug">
                            <input
                                value={spec.flow ?? ""}
                                placeholder="default-authentication-flow"
                                onChange={(e) => patch({ flow: e.target.value })}
                            />
                        </Field>
                        <Field label="Trace sample rate">
                            <input
                                value={spec.traceSampleRate ?? ""}
                                placeholder="0.01"
                                onChange={(e) => patch({ traceSampleRate: e.target.value })}
                            />
                        </Field>
                        <Field
                            label="Seed"
                            help="Fixes randomized scripts' sequence; defaults to a hash of the name"
                        >
                            <input
                                type="number"
                                min={0}
                                max={4294967295}
                                value={spec.seed ?? ""}
                                placeholder="auto"
                                onChange={(e) =>
                                    patch({
                                        seed:
                                            e.target.value === ""
                                                ? undefined
                                                : Number(e.target.value),
                                    })
                                }
                            />
                        </Field>
                    </div>
                </fieldset>

                <div className="form-actions">
                    <button type="submit" className="primary" disabled={saving}>
                        {saving ? "Saving…" : existing ? "Save changes" : "Create test case"}
                    </button>
                    <button type="button" onClick={() => navigate(-1)}>
                        Cancel
                    </button>
                </div>
            </form>
        </Page>
    );
}

export function TestCaseCreatePage() {
    return <TestCaseForm />;
}

export function TestCaseEditPage() {
    const { name = "" } = useParams();
    const existing = useResource(() => api.testCase(name), [name], 0);
    if (existing.error) {
        return (
            <Page title="Edit test case">
                <Alert>{existing.error}</Alert>
            </Page>
        );
    }
    return existing.data ? <TestCaseForm key={name} existing={existing.data} /> : null;
}

// Until a burn has started there's no start to range from.
const FALLBACK_TIMELINE_RANGE = "6h";

const SECTIONS = [
    { id: "targets", label: "Targets & timeline" },
    { id: "tracing", label: "Tracing" },
    { id: "profiling", label: "Profiling" },
    { id: "logs", label: "Logs" },
    { id: "resources", label: "Resources" },
    { id: "status", label: "Status" },
];

export function TestCaseViewPage() {
    const { name = "" } = useParams();
    const testCase = useResource(() => api.testCase(name), [name], 10000, "TestCase");
    const deployments = useResource(() => api.deployments(), [], 30000, "AuthentikDeployment");
    const runs = useResource(() => api.testRuns(name), [name], 10000, "TestRun");
    const tc = testCase.data;
    const profile = tc?.spec.profile;

    // The time range lives in the URL, so a view can be shared exactly as
    // configured; the baseline is stored on the TestCase itself.
    const [params, setParams] = useSearchParams();
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
    // An archived burn's charts end when it stopped (plus a minute for k6's
    // last remote-write push) and never reach back before it started.
    const archivedWindow = tc && profile === "Burn" ? archivedBurnWindow(tc) : undefined;
    const burnStart = archivedWindow
        ? archivedWindow.start
        : tc
          ? earliestBurnStart([tc])
          : undefined;
    const burnEnd = archivedWindow ? Math.floor(archivedWindow.end) + 60 : undefined;
    const defaultTimelineRange = burnStart !== undefined ? SINCE_START : FALLBACK_TIMELINE_RANGE;
    const timelineRangeValue = parseTimeRange(params.get("range"), defaultTimelineRange, burnStart);
    let timelineRange = timeRangeSeconds(timelineRangeValue, burnStart, burnEnd);
    if (burnStart !== undefined && burnEnd !== undefined) {
        timelineRange = Math.min(timelineRange, timeRangeSeconds(SINCE_START, burnStart, burnEnd));
    }

    const sortedRuns = [...(runs.data ?? [])].sort((a, b) =>
        (b.metadata.creationTimestamp ?? "").localeCompare(a.metadata.creationTimestamp ?? ""),
    );
    // Burn runs continuously, so "now" is right for it; a Quick test's data
    // ends with its latest completed run.
    const anchor = profile === "Quick" ? metricsAnchorFromRuns(runs.data ?? []) : undefined;
    const metricsEnd = profile === "Quick" ? anchor?.endSeconds : burnEnd;
    const summaryWindow = profile === "Quick" ? "5m" : `${timelineRange}s`;
    const summary = useResource(
        () =>
            profile ? api.compare([name], summaryWindow, metricsEnd) : Promise.resolve(undefined),
        [name, profile, summaryWindow, metricsEnd],
        15000,
    );
    const rows = tc ? buildTargetRows(tc, summary.data?.results ?? [], deployments.data ?? []) : [];
    // pendingBaseline shows a picked baseline (a deployment name) right away,
    // until the reloaded TestCase carries it.
    const [pendingBaseline, setPendingBaseline] = useState<string>();
    const storedBaseline = tc ? testCaseBaseline(tc.spec) : undefined;
    if (pendingBaseline !== undefined && tc?.spec.baselineRef?.name === pendingBaseline) {
        setPendingBaseline(undefined);
    }
    const baselineDeployment = pendingBaseline ?? storedBaseline;
    const baseline = (rows.find((r) => r.deploymentName === baselineDeployment) ?? rows[0])?.target;

    async function saveBaseline(target: string) {
        const row = rows.find((r) => r.target === target);
        if (!tc || !row || row.deploymentName === baselineDeployment) {
            return;
        }
        setPendingBaseline(row.deploymentName);
        try {
            await api.updateTestCase(name, {
                spec: { ...tc.spec, baselineRef: { name: row.deploymentName } },
            });
            testCase.reload();
        } catch (err) {
            setPendingBaseline(undefined);
            toast.error(errorMessage(err));
        }
    }
    const since = tc && profile === "Burn" ? burningFor(tc) : undefined;
    const burnedFor =
        archivedWindow?.start !== undefined
            ? formatDuration(archivedWindow.end - archivedWindow.start)
            : undefined;

    const [busy, setBusy] = useState(false);
    const [actionError, setActionError] = useState<string>();

    async function runNow() {
        setBusy(true);
        setActionError(undefined);
        try {
            const started = await api.runTestCase(name);
            toast.success(
                started.length > 1
                    ? `Started ${started.length} test runs, one per target`
                    : "Started test run",
            );
            runs.reload();
        } catch (err) {
            setActionError(errorMessage(err));
        } finally {
            setBusy(false);
        }
    }

    function switchToQuick() {
        if (!tc) {
            return;
        }
        void confirmAndRun(
            {
                title: "Switch to Quick",
                message: `Stop the burn on all ${rows.length} target(s) by switching "${name}" to Quick?`,
                confirmLabel: "Switch",
            },
            () => api.updateTestCase(name, { spec: { ...tc.spec, profile: "Quick" } }),
            testCase.reload,
            `Switched ${name} to Quick`,
        );
    }

    return (
        <Page
            title={name}
            subtitle={
                tc && (
                    <>
                        {tc.spec.profile} · {rows.length} {rows.length === 1 ? "target" : "targets"}{" "}
                        · {tc.spec.vus} VUs
                        {rows.length > 1 ? " each" : ""} ·{" "}
                        {tc.spec.inline !== undefined
                            ? "inline script"
                            : `template ${tc.spec.template ?? "login"}`}
                        {since && ` · burning for ${since}`}
                        {burnedFor && ` · burned for ${burnedFor}`}
                    </>
                )
            }
            actions={
                <>
                    {tc && (
                        <StatusBadge
                            kind="testcase"
                            name={name}
                            namespace={tc.metadata.namespace}
                            phase={tc.status?.phase}
                            conditions={tc.status?.conditions}
                        />
                    )}
                    {profile === "Quick" && !tc?.spec.archived && (
                        <button className="primary" disabled={busy} onClick={() => void runNow()}>
                            {busy ? "Starting…" : "Run now"}
                        </button>
                    )}
                    <Link className="button" to={`/testcases/${name}/edit`}>
                        {profile === "Burn" ? "Change load or targets" : "Edit"}
                    </Link>
                    {profile === "Burn" && !tc?.spec.archived && (
                        <button className="danger" onClick={switchToQuick}>
                            Stop burn
                        </button>
                    )}
                    {tc && (
                        <button
                            onClick={() => void setArchived(tc, !tc.spec.archived, testCase.reload)}
                        >
                            {tc.spec.archived ? "Unarchive" : "Archive"}
                        </button>
                    )}
                </>
            }
        >
            <Alert>{testCase.error ?? actionError ?? summary.error}</Alert>
            <SectionNav sections={SECTIONS} />

            <Section
                id="targets"
                title="Targets & timeline"
                loading={summary.loading && summary.data !== undefined}
                description={
                    profile === "Quick"
                        ? anchor
                            ? `The last 5 minutes of the latest completed run (${new Date(anchor.endSeconds * 1000).toLocaleString()}).`
                            : "Shows once a run has completed."
                        : "Same script and load on each target. Δ is relative to the baseline target."
                }
                actions={
                    profile === "Burn" && (
                        <TimeRangeSelect
                            value={timelineRangeValue}
                            sinceStartLabel={
                                burnStart !== undefined ? "Since burn start" : undefined
                            }
                            onChange={(v) =>
                                updateParams({ range: v === defaultTimelineRange ? undefined : v })
                            }
                        />
                    )
                }
            >
                {!tc ? (
                    !testCase.error && <Loading />
                ) : rows.length === 0 ? (
                    <Empty>
                        This test case has no target deployments - edit it to pick at least one.
                    </Empty>
                ) : (
                    <TargetTable
                        rows={rows}
                        metrics={summary.data?.metrics ?? []}
                        loading={summary.loading && !summary.data}
                        baseline={baseline}
                        onBaseline={(target) => void saveBaseline(target)}
                        actions={(row) => {
                            const cluster =
                                tc?.spec.runnerClusterRef?.name ??
                                row.deployment?.spec.clusterRef?.name;
                            return (
                                row.burnWorkload &&
                                tc?.metadata.namespace && (
                                    <Link
                                        className="button small"
                                        to={shellLinkForCluster(
                                            cluster,
                                            `kubectl logs -n ${tc.metadata.namespace} deploy/${row.burnWorkload} -f --tail=200`,
                                            tc.metadata.namespace,
                                        )}
                                    >
                                        k6 logs
                                    </Link>
                                )
                            );
                        }}
                    />
                )}

                {profile === "Quick" && (
                    <>
                        <h3>Runs</h3>
                        <Table
                            rows={runs.data && sortedRuns.slice(0, 10)}
                            rowKey={(r) => r.metadata.name}
                            empty="No runs yet."
                            columns={[
                                {
                                    label: "Run",
                                    render: (r) => (
                                        <Link to={`/testruns/${r.metadata.name}`}>
                                            {r.metadata.name}
                                        </Link>
                                    ),
                                },
                                {
                                    label: "Target",
                                    render: (r) =>
                                        testRunDeployment(r) ?? <span className="muted">-</span>,
                                },
                                {
                                    label: "Phase",
                                    render: (r) => <Badge value={r.status?.phase ?? "Pending"} />,
                                },
                                {
                                    label: "Started",
                                    render: (r) =>
                                        formatTimestamp(
                                            r.status?.startTime ?? r.metadata.creationTimestamp,
                                        ),
                                },
                                {
                                    label: "Completed",
                                    render: (r) => formatTimestamp(r.status?.completionTime),
                                },
                            ]}
                        />
                    </>
                )}

                <div className="sub-head">
                    <div>
                        <h3>Timeline</h3>
                        <div className="muted">
                            One line per target, colored as in the table above.
                        </div>
                    </div>
                </div>
                {profile === "Quick" && !anchor && (runs.data?.length ?? 0) > 0 && (
                    <Alert kind="info">
                        Latest run is still in progress - metrics will show once it completes.
                    </Alert>
                )}
                <MetricsGrid
                    testids={[name]}
                    endSeconds={metricsEnd}
                    rangeSeconds={profile === "Quick" ? anchor?.rangeSeconds : timelineRange}
                />
            </Section>

            {tc && <TraceBreakdownSection testid={name} rows={rows} baseline={baseline} />}
            {tc && <ProfileBreakdownSection testid={name} rows={rows} baseline={baseline} />}
            {tc && <LogBreakdownSection testid={name} rows={rows} baseline={baseline} />}

            <Section
                id="resources"
                title="Resources"
                description="CPU and memory of each target's server and worker pods over the timeline's range."
            >
                <ResourceCharts
                    testids={[name]}
                    endSeconds={metricsEnd}
                    rangeSeconds={profile === "Quick" ? anchor?.rangeSeconds : timelineRange}
                />
            </Section>

            <Section
                id="status"
                title="Status"
                description="Conditions, recent events and the full object."
            >
                {tc && (
                    <>
                        <ConditionList conditions={tc.status?.conditions} />
                        <ActivityLog path="testcases" name={name} />
                    </>
                )}
                <ObjectYaml object={tc} />
            </Section>
        </Page>
    );
}
