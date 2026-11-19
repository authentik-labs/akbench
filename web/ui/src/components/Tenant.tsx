// The tenant a deployment is seeded with: the picker the deployment forms
// share, and how a deployment's tenant is shown in its list and view.

import { api } from "../api";
import {
    AuthentikDeployment,
    AuthentikDeploymentSpec,
    TENANT_RESET_REQUIRED,
    TENANT_SEEDED_CONDITION,
    TenantPreset,
    TenantRecord,
    TenantStatus,
} from "../types";
import { useResource } from "../useResource";
import { Badge } from "./Status";
import { Alert, Field, formatTimestamp } from "./ui";

import { Link } from "react-router";

/** formatCount is a count of users or groups, with thousands separators. */
export const formatCount = (n: number) => n.toLocaleString("en-US");

const users = (n: number) => `${formatCount(n)} ${n === 1 ? "user" : "users"}`;

/** tenantRecordText is the tenant a test ran against, in a few words. */
export function tenantRecordText(t: TenantRecord): string {
    return `${t.preset} - ${users(t.params.users)}`;
}

/**
 * tenantPresetOptions are the picker's options after "None": every preset,
 * smallest first. A selected preset that isn't among them (deleted, or the
 * list couldn't be loaded) is kept, so the form doesn't drop it on save.
 */
export function tenantPresetOptions(
    presets: TenantPreset[] | undefined,
    selected?: string,
): { value: string; label: string }[] {
    const sorted = [...(presets ?? [])].sort(
        (a, b) => a.spec.users - b.spec.users || a.metadata.name.localeCompare(b.metadata.name),
    );
    const options = sorted.map((p) => ({
        value: p.metadata.name,
        label: `${p.metadata.name} - ${users(p.spec.users)}`,
    }));
    if (selected && !options.some((o) => o.value === selected)) {
        options.push({ value: selected, label: presets ? `${selected} - not found` : selected });
    }
    return options;
}

/** tenantSpec is spec.tenant for a picked preset; "" (None) leaves it out. */
export function tenantSpec(preset: string): AuthentikDeploymentSpec["tenant"] {
    return preset ? { presetRef: { name: preset } } : undefined;
}

/**
 * tenantChangeNote says what picking preset means for a deployment that
 * already has a tenant: the operator never re-seeds in place. A failed
 * attempt is retried with the new preset, so it needs no note.
 */
export function tenantChangeNote(
    seeded: TenantStatus | undefined,
    preset: string,
): string | undefined {
    if (!seeded || seeded.state === "Failed" || seeded.preset === preset) {
        return undefined;
    }
    if (!preset) {
        return `The tenant seeded from ${seeded.preset} stays in the database until the deployment is reset.`;
    }
    return `This deployment holds the tenant seeded from ${seeded.preset}. ${preset} takes effect after a reset.`;
}

export interface TenantSummary {
    /** The preset, and its users once seeding has started. */
    text: string;
    state: string;
    /** Why it failed or needs a reset, or what it is waiting for. */
    message?: string;
}

/**
 * tenantSummary is how a deployment's tenant reads in its list and view,
 * or undefined without one. "Reset required" isn't a state of its own: it
 * is the TenantSeeded condition's reason. "Not selected" is a tenant still
 * in the database (or still being written to it) after spec.tenant was
 * cleared: only a reset removes it.
 */
export function tenantSummary(d: AuthentikDeployment): TenantSummary | undefined {
    const status = d.status?.tenant;
    const condition = d.status?.conditions?.find((c) => c.type === TENANT_SEEDED_CONDITION);
    if (status) {
        const text = tenantRecordText(status);
        if (status.state === "Failed") {
            return { text, state: "Failed", message: status.message || condition?.message };
        }
        if (!d.spec.tenant) {
            return { text, state: "Not selected", message: condition?.message };
        }
        if (condition?.status === "False" && condition.reason === TENANT_RESET_REQUIRED) {
            return { text, state: "Reset required", message: condition.message };
        }
        return { text, state: status.state };
    }
    const preset = d.spec.tenant?.presetRef.name;
    if (!preset) {
        return undefined;
    }
    return {
        text: preset,
        state: "Waiting",
        message: condition?.status === "False" ? condition.message : undefined,
    };
}

/** TenantSelect picks the preset a deployment is seeded with, or none. */
export function TenantSelect({
    value,
    onChange,
    seeded,
    note: staticNote,
}: {
    value?: string;
    onChange: (preset: string) => void;
    /** The deployment's status.tenant, when editing one. */
    seeded?: TenantStatus;
    /** Shown when seeded has nothing to say: for a form whose deployments it can't see. */
    note?: string;
}) {
    const presets = useResource(() => api.tenantPresets(), [], 0, "TenantPreset");
    const note = tenantChangeNote(seeded, value ?? "") ?? staticNote;
    return (
        <>
            <div className="fields">
                <Field
                    label="Tenant"
                    help={
                        <>
                            Users and groups seeded once the deployment is up.{" "}
                            <Link to="/tenantpresets">Manage tenant presets</Link>
                        </>
                    }
                >
                    <select value={value ?? ""} onChange={(e) => onChange(e.target.value)}>
                        <option value="">None</option>
                        {tenantPresetOptions(presets.data, value).map((o) => (
                            <option key={o.value} value={o.value}>
                                {o.label}
                            </option>
                        ))}
                    </select>
                </Field>
            </div>
            {presets.error && <Alert>Couldn&apos;t load tenant presets: {presets.error}</Alert>}
            {note && <Alert kind="info">{note}</Alert>}
        </>
    );
}

/**
 * TenantCell is a deployment's tenant in one line, for the list. A long
 * failure message is cut short; the deployment's view shows all of it.
 */
export function TenantCell({ deployment }: { deployment: AuthentikDeployment }) {
    const summary = tenantSummary(deployment);
    if (!summary) {
        return <span className="muted">None</span>;
    }
    const message =
        summary.message && summary.message.length > 80
            ? `${summary.message.slice(0, 80)}…`
            : summary.message;
    return (
        <div title={summary.message}>
            <span style={{ whiteSpace: "nowrap" }}>
                {summary.text} <Badge value={summary.state} />
            </span>
            {message && <div className="muted">{message}</div>}
        </div>
    );
}

/** TenantDetails is a deployment's tenant for its view. */
export function TenantDetails({ deployment }: { deployment: AuthentikDeployment }) {
    const summary = tenantSummary(deployment);
    const status = deployment.status?.tenant;
    if (!summary) {
        return <div className="muted">Tenant: none</div>;
    }
    return (
        <div>
            <div className="row">
                <span>Tenant: {summary.text}</span>
                <Badge value={summary.state} />
                {status && (
                    <span className="muted">
                        {formatCount(status.params.groups)} groups, tree depth{" "}
                        {status.params.groupDepth}, {status.params.groupsPerUser} groups per user,
                        seed {status.params.seed}
                        {status.seededAt && `, seeded ${formatTimestamp(status.seededAt)}`}
                    </span>
                )}
            </div>
            {summary.message && (
                <Alert kind={summary.state === "Failed" ? "error" : "info"}>
                    {summary.message}
                </Alert>
            )}
        </div>
    );
}
