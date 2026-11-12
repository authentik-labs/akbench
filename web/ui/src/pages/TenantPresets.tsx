import { api } from "../api";
import { toast } from "../components/Toast";
import { Alert, confirmAndRun, errorMessage, Field, Page, Table } from "../components/ui";
import { k8sNameInput } from "../github";
import {
    isBuiltIn,
    TenantPreset,
    tenantPresetCopy,
    tenantPresetLimits as limits,
    tenantPresetProblem,
    TenantPresetSpec,
} from "../types";
import { useResource } from "../useResource";

import { FormEvent, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";

const emptySpec: TenantPresetSpec = {
    description: "",
    seed: 1,
    users: 1000,
    groups: 50,
    groupDepth: 3,
    groupsPerUser: 4,
};

const count = (n: number) => n.toLocaleString("en-US");

export function TenantPresetsPage() {
    const presets = useResource(() => api.tenantPresets(), [], 5000, "TenantPreset");
    // Smallest first: the order someone picking a size reads them in.
    const rows = presets.data && [...presets.data].sort((a, b) => a.spec.users - b.spec.users);
    return (
        <Page
            title="Tenant presets"
            subtitle="Named sizes for the test data a deployment is to be seeded with: users, groups and memberships."
            actions={
                <Link className="button primary" to="/tenantpresets/new">
                    New preset
                </Link>
            }
        >
            <Alert>{presets.error}</Alert>
            <p className="muted">
                Built-in presets are reset to their own numbers whenever the operator restarts. Copy
                one to change it.
            </p>
            <Table
                rows={rows}
                rowKey={(p) => p.metadata.name}
                empty="No presets yet."
                columns={[
                    {
                        label: "Name",
                        render: (p) => (
                            <span style={{ whiteSpace: "nowrap" }}>
                                {p.metadata.name}
                                {isBuiltIn(p) && <span className="muted"> (built-in)</span>}
                            </span>
                        ),
                    },
                    { label: "Description", render: (p) => p.spec.description ?? "" },
                    { label: "Users", render: (p) => count(p.spec.users) },
                    { label: "Groups", render: (p) => count(p.spec.groups) },
                    { label: "Tree depth", render: (p) => p.spec.groupDepth },
                    { label: "Groups per user", render: (p) => p.spec.groupsPerUser },
                    {
                        label: "",
                        className: "actions",
                        render: (p) => (
                            <span
                                className="row"
                                style={{ justifyContent: "flex-end", flexWrap: "nowrap" }}
                            >
                                <Link
                                    className="button small"
                                    to={`/tenantpresets/new?from=${encodeURIComponent(p.metadata.name)}`}
                                >
                                    Copy
                                </Link>
                                {!isBuiltIn(p) && (
                                    <>
                                        <Link
                                            className="button small"
                                            to={`/tenantpresets/${p.metadata.name}/edit`}
                                        >
                                            Edit
                                        </Link>
                                        <button
                                            className="small danger"
                                            onClick={() =>
                                                void confirmAndRun(
                                                    {
                                                        title: "Delete preset",
                                                        message: `Delete preset "${p.metadata.name}"?`,
                                                        confirmLabel: "Delete",
                                                        danger: true,
                                                    },
                                                    () => api.deleteTenantPreset(p.metadata.name),
                                                    presets.reload,
                                                    `Deleted ${p.metadata.name}`,
                                                )
                                            }
                                        >
                                            Delete
                                        </button>
                                    </>
                                )}
                            </span>
                        ),
                    },
                ]}
            />
        </Page>
    );
}

function NumberField({
    label,
    help,
    value,
    min,
    max,
    onChange,
}: {
    label: string;
    help: string;
    value: number;
    min: number;
    max?: number;
    onChange: (value: number) => void;
}) {
    return (
        <Field label={label} help={help}>
            <input
                type="number"
                required
                step={1}
                min={min}
                max={max}
                value={Number.isNaN(value) ? "" : value}
                onChange={(e) => onChange(e.target.valueAsNumber)}
            />
        </Field>
    );
}

function TenantPresetForm({
    existing,
    copyOf,
}: {
    existing?: TenantPreset;
    copyOf?: TenantPreset;
}) {
    const navigate = useNavigate();
    const copy = copyOf && tenantPresetCopy(copyOf);
    const [name, setName] = useState(existing?.metadata.name ?? copy?.name ?? "");
    const [spec, setSpec] = useState<TenantPresetSpec>(existing?.spec ?? copy?.spec ?? emptySpec);
    const [error, setError] = useState<string>();
    const [saving, setSaving] = useState(false);
    // Reachable by URL only: the list offers Copy, not Edit, on a built-in.
    const builtIn = existing !== undefined && isBuiltIn(existing);
    const patch = (changes: Partial<TenantPresetSpec>) =>
        setSpec((current) => ({ ...current, ...changes }));

    async function submit(event: FormEvent) {
        event.preventDefault();
        const problem = tenantPresetProblem(spec);
        if (problem) {
            setError(problem);
            return;
        }
        setSaving(true);
        setError(undefined);
        try {
            const savedName = existing?.metadata.name ?? name;
            if (existing) {
                await api.updateTenantPreset(savedName, { spec });
                toast.success(`Saved ${savedName}`);
            } else {
                await api.createTenantPreset({ metadata: { name }, spec });
                toast.success(`Created ${savedName}`);
            }
            navigate("/tenantpresets");
        } catch (err) {
            setError(errorMessage(err));
            setSaving(false);
        }
    }

    return (
        <Page title={existing ? `Edit ${existing.metadata.name}` : "New tenant preset"}>
            <form onSubmit={submit}>
                <Alert>{error}</Alert>
                {builtIn && (
                    <Alert kind="info">
                        This is a built-in preset, which the operator resets whenever it restarts.{" "}
                        <Link
                            to={`/tenantpresets/new?from=${encodeURIComponent(name)}`}
                            style={{ textDecoration: "underline" }}
                        >
                            Copy it
                        </Link>{" "}
                        to change it.
                    </Alert>
                )}
                <div className="fields">
                    <Field label="Name" help="Cannot be changed once the preset is created">
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
                    <NumberField
                        {...limits.users}
                        help="Named u000000 onwards"
                        value={spec.users}
                        onChange={(users) => patch({ users })}
                    />
                    <NumberField
                        {...limits.groups}
                        help="Named g000000 onwards"
                        value={spec.groups}
                        onChange={(groups) => patch({ groups })}
                    />
                    <NumberField
                        {...limits.groupDepth}
                        help="1 is a flat list; 2 is one root group with the rest below it"
                        value={spec.groupDepth}
                        onChange={(groupDepth) => patch({ groupDepth })}
                    />
                    <NumberField
                        {...limits.groupsPerUser}
                        help="Direct memberships of each user; a few groups end up large"
                        max={spec.groups}
                        value={spec.groupsPerUser}
                        onChange={(groupsPerUser) => patch({ groupsPerUser })}
                    />
                    <NumberField
                        {...limits.seed}
                        help="The same seed and numbers always produce the same data"
                        value={spec.seed ?? 1}
                        onChange={(seed) => patch({ seed })}
                    />
                </div>
                <div className="form-actions">
                    <button type="submit" className="primary" disabled={saving || builtIn}>
                        {saving ? "Saving…" : existing ? "Save changes" : "Create preset"}
                    </button>
                    <button type="button" onClick={() => navigate(-1)}>
                        Cancel
                    </button>
                </div>
            </form>
        </Page>
    );
}

export function TenantPresetCreatePage() {
    const [params] = useSearchParams();
    const from = params.get("from");
    if (!from) {
        return <TenantPresetForm />;
    }
    return <TenantPresetCopy from={from} />;
}

function TenantPresetCopy({ from }: { from: string }) {
    const source = useResource(() => api.tenantPreset(from), [from], 0);
    if (source.error) {
        return (
            <Page title="New tenant preset">
                <Alert>{source.error}</Alert>
            </Page>
        );
    }
    return source.data ? <TenantPresetForm key={from} copyOf={source.data} /> : null;
}

export function TenantPresetEditPage() {
    const { name = "" } = useParams();
    const existing = useResource(() => api.tenantPreset(name), [name], 0);
    if (existing.error) {
        return (
            <Page title="Edit tenant preset">
                <Alert>{existing.error}</Alert>
            </Page>
        );
    }
    return existing.data ? <TenantPresetForm key={name} existing={existing.data} /> : null;
}
