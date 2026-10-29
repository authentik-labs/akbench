import { api } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { CodeEditor } from "../components/CodeEditor";
import { ObjectYaml } from "../components/ObjectYaml";
import { StatusBadge } from "../components/Status";
import { toast } from "../components/Toast";
import { Alert, confirmAndRun, Empty, errorMessage, Field, Page, Table } from "../components/ui";
import { k8sNameInput } from "../github";
import {
    TestCaseDependency,
    testCaseDependencies,
    TestCaseTemplate,
    TestCaseTemplateSpec,
} from "../types";
import { useResource } from "../useResource";

import { FormEvent, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";

function dependencyLabels(deps?: TestCaseDependency[]): string {
    return (deps ?? []).map((d) => testCaseDependencies[d] ?? d).join(", ");
}

export function TestCaseTemplatesPage() {
    const templates = useResource(() => api.testCaseTemplates(), [], 5000, "TestCaseTemplate");
    return (
        <Page
            title="Test case templates"
            subtitle="Reusable k6 scripts a test case selects by name."
            actions={
                <Link className="button primary" to="/testcasetemplates/new">
                    New template
                </Link>
            }
        >
            <Alert>{templates.error}</Alert>
            <Table
                rows={templates.data}
                rowKey={(t) => t.metadata.name}
                empty="No templates yet."
                columns={[
                    {
                        label: "Name",
                        render: (t) => (
                            <Link to={`/testcasetemplates/${t.metadata.name}`}>
                                {t.metadata.name}
                            </Link>
                        ),
                    },
                    { label: "Description", render: (t) => t.spec.description ?? "" },
                    {
                        label: "Dependencies",
                        render: (t) => dependencyLabels(t.spec.dependencies) || "-",
                    },
                    {
                        label: "Status",
                        render: (t) => (
                            <StatusBadge
                                kind="testcasetemplate"
                                name={t.metadata.name}
                                namespace={t.metadata.namespace}
                                phase={t.status?.phase}
                                conditions={t.status?.conditions}
                            />
                        ),
                    },
                    {
                        label: "",
                        className: "actions",
                        render: (t) => (
                            <span className="row" style={{ justifyContent: "flex-end" }}>
                                <Link
                                    className="button small"
                                    to={`/testcasetemplates/${t.metadata.name}/edit`}
                                >
                                    Edit
                                </Link>
                                <button
                                    className="small danger"
                                    onClick={() =>
                                        void confirmAndRun(
                                            {
                                                title: "Delete template",
                                                message: `Delete template "${t.metadata.name}"? Any test case still using it stops reconciling.`,
                                                confirmLabel: "Delete",
                                                danger: true,
                                            },
                                            () => api.deleteTestCaseTemplate(t.metadata.name),
                                            templates.reload,
                                            `Deleted ${t.metadata.name}`,
                                        )
                                    }
                                >
                                    Delete
                                </button>
                            </span>
                        ),
                    },
                ]}
            />
        </Page>
    );
}

function TestCaseTemplateForm({ existing }: { existing?: TestCaseTemplate }) {
    const navigate = useNavigate();
    const [name, setName] = useState(existing?.metadata.name ?? "");
    const [spec, setSpec] = useState<TestCaseTemplateSpec>(
        existing?.spec ?? { description: "", script: "", dependencies: [] },
    );
    const [error, setError] = useState<string>();
    const [saving, setSaving] = useState(false);
    const patch = (changes: Partial<TestCaseTemplateSpec>) =>
        setSpec((current) => ({ ...current, ...changes }));

    async function submit(event: FormEvent) {
        event.preventDefault();
        setSaving(true);
        setError(undefined);
        try {
            const savedName = existing?.metadata.name ?? name;
            if (existing) {
                await api.updateTestCaseTemplate(savedName, { spec });
                toast.success(`Saved ${savedName}`);
            } else {
                await api.createTestCaseTemplate({ metadata: { name }, spec });
                toast.success(`Created ${savedName}`);
            }
            navigate(`/testcasetemplates/${savedName}`);
        } catch (err) {
            setError(errorMessage(err));
            setSaving(false);
        }
    }

    return (
        <Page title={existing ? `Edit ${existing.metadata.name}` : "New test case template"}>
            <form onSubmit={submit}>
                <Alert>{error}</Alert>
                <div className="fields">
                    <Field
                        label="Name"
                        help={'What a test case\'s spec.template refers to, e.g. "login"'}
                    >
                        <input
                            required
                            value={name}
                            disabled={Boolean(existing)}
                            onChange={(e) => setName(k8sNameInput(e.target.value))}
                        />
                    </Field>
                    <Field label="Description">
                        <input
                            value={spec.description ?? ""}
                            onChange={(e) => patch({ description: e.target.value })}
                        />
                    </Field>
                </div>
                <p className="muted" style={{ marginTop: 8 }}>
                    Dependencies, provisioned for every test case using this template:
                </p>
                <div className="row" style={{ marginBottom: 16 }}>
                    {(Object.keys(testCaseDependencies) as TestCaseDependency[]).map((dep) => (
                        <label key={dep} className="check">
                            <input
                                type="checkbox"
                                checked={spec.dependencies?.includes(dep) ?? false}
                                onChange={(e) =>
                                    patch({
                                        dependencies: e.target.checked
                                            ? [...(spec.dependencies ?? []), dep]
                                            : spec.dependencies?.filter((d) => d !== dep),
                                    })
                                }
                            />
                            {testCaseDependencies[dep]}
                        </label>
                    ))}
                </div>
                <p className="muted">
                    k6 script (TypeScript), mirrored verbatim into every test case selecting this
                    template.
                </p>
                <CodeEditor
                    language="typescript"
                    height="500px"
                    value={spec.script}
                    onChange={(v) => patch({ script: v })}
                />
                <div className="form-actions">
                    <button type="submit" className="primary" disabled={saving}>
                        {saving ? "Saving…" : existing ? "Save changes" : "Create template"}
                    </button>
                    <button type="button" onClick={() => navigate(-1)}>
                        Cancel
                    </button>
                </div>
            </form>
        </Page>
    );
}

export function TestCaseTemplateCreatePage() {
    return <TestCaseTemplateForm />;
}

export function TestCaseTemplateEditPage() {
    const { name = "" } = useParams();
    const existing = useResource(() => api.testCaseTemplate(name), [name], 0);
    if (existing.error) {
        return (
            <Page title="Edit test case template">
                <Alert>{existing.error}</Alert>
            </Page>
        );
    }
    return existing.data ? <TestCaseTemplateForm key={name} existing={existing.data} /> : null;
}

export function TestCaseTemplateViewPage() {
    const { name = "" } = useParams();
    const template = useResource(
        () => api.testCaseTemplate(name),
        [name],
        15000,
        "TestCaseTemplate",
    );
    const testCases = useResource(() => api.testCases(), [], 15000, "TestCase");
    // The one cross-reference the object itself can't show: a TestCase names
    // its template, not the other way round.
    const users = (testCases.data ?? []).filter((tc) => tc.spec.template === name);
    const t = template.data;

    return (
        <Page
            title={name}
            subtitle={t?.spec.description}
            actions={
                <Link className="button" to={`/testcasetemplates/${name}/edit`}>
                    Edit
                </Link>
            }
        >
            <Alert>{template.error}</Alert>
            {t && (
                <div className="row" style={{ marginBottom: 8 }}>
                    <StatusBadge
                        kind="testcasetemplate"
                        name={name}
                        namespace={t.metadata.namespace}
                        phase={t.status?.phase}
                        conditions={t.status?.conditions}
                    />
                    {t.spec.dependencies?.length ? (
                        <span className="muted">
                            Provisions {dependencyLabels(t.spec.dependencies)}
                        </span>
                    ) : null}
                </div>
            )}
            {t && (
                <div className="panel">
                    <ActivityLog path="testcasetemplates" name={name} />
                </div>
            )}
            <h2>Used by</h2>
            {users.length === 0 ? (
                <Empty>No test cases use this template yet.</Empty>
            ) : (
                <div className="row">
                    {users.map((tc) => (
                        <Link
                            key={tc.metadata.name}
                            className="button small"
                            to={`/testcases/${tc.metadata.name}`}
                        >
                            {tc.metadata.name}
                        </Link>
                    ))}
                </div>
            )}
            {t && (
                <>
                    <h2>Script</h2>
                    <pre className="yaml">{t.spec.script}</pre>
                </>
            )}
            <ObjectYaml object={t} />
        </Page>
    );
}
