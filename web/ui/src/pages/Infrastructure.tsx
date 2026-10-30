import { api } from "../api";
import { toast } from "../components/Toast";
import {
    Alert,
    confirmAndRun,
    Empty,
    errorMessage,
    Field,
    formatTimestamp,
    Page,
    Table,
} from "../components/ui";
import { k8sNameInput } from "../github";
import {
    IncusCredential,
    IncusCredentialInput,
    IncusProbe,
    InfrastructureProviderInfo,
} from "../types";
import { useResource } from "../useResource";

import { FormEvent, useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";

// Everything provider-specific about where workload clusters run: which
// Cluster API infrastructure providers are installed (enabled right here
// inside the appliance) and each provider's own connection settings - Incus
// today, so its section is the only one so far.

function providerBadge(p: InfrastructureProviderInfo) {
    if (p.installedVersion) {
        return <span className="badge ok">Installed {p.installedVersion}</span>;
    }
    if (p.enabled) {
        return <span className="badge info">Installing {p.availableVersion ?? ""}</span>;
    }
    if (p.availableVersion) {
        return <span className="badge neutral">Available {p.availableVersion}</span>;
    }
    return <span className="badge neutral">Not installed</span>;
}

// Inside the appliance, akbench itself (operator + apiserver) updates to the
// latest published build from here; the appliance's sync loop rolls it out.
function UpdateSection() {
    const update = useResource(() => api.update(), [], 30000);
    const info = update.data;
    if (!info?.managed) {
        return null;
    }
    const rollingOut = info.target && info.target !== info.current;
    const available = info.latest && info.latest !== info.target;
    // Flux upgrades the operator; a failed upgrade rolls it back and leaves
    // it behind the target while the web UI is already updated.
    const operatorBehind = info.operator && info.target && info.operator !== info.target;
    return (
        <>
            <h2>akbench</h2>
            <Alert>{update.error}</Alert>
            {info.latestError && <Alert>Checking for updates failed: {info.latestError}</Alert>}
            <p>
                Running <code>{info.current}</code>
                {rollingOut && (
                    <>
                        {" "}
                        - <span className="badge info">Updating to {info.target}</span>
                    </>
                )}
                {!rollingOut && !available && info.latest && (
                    <span className="muted"> - up to date</span>
                )}
            </p>
            <Table
                rows={[
                    { component: "Web UI / API server", version: info.current },
                    {
                        component: "Operator",
                        version: info.operator,
                        note: operatorBehind ? (
                            <span className="badge warn">Not on {info.target}</span>
                        ) : (
                            info.operatorError && (
                                <span className="muted">{info.operatorError}</span>
                            )
                        ),
                    },
                    { component: "Appliance", version: info.appliance },
                ]}
                rowKey={(r) => r.component}
                empty=""
                columns={[
                    { label: "Component", render: (r) => r.component },
                    {
                        label: "Version",
                        render: (r) =>
                            r.version ? (
                                <code>{r.version}</code>
                            ) : (
                                <span className="muted">unknown</span>
                            ),
                    },
                    { label: "", render: (r) => r.note },
                ]}
            />
            {operatorBehind && !rollingOut && (
                <Alert>
                    The operator is still on {info.operator}. Its Flux HelmRelease
                    (flux-system/authentik-bench-operator) has likely failed to upgrade; check its
                    status.
                </Alert>
            )}
            {available && (
                <button
                    className="primary"
                    onClick={() =>
                        void confirmAndRun(
                            `Update the operator and web UI to ${info.latest}? The web UI restarts while it rolls out.`,
                            () => api.applyUpdate(),
                            update.reload,
                            `Updating to ${info.latest}`,
                        )
                    }
                >
                    Update to {info.latest}
                </button>
            )}
        </>
    );
}

export function InfrastructurePage() {
    const infra = useResource(() => api.infrastructure(), [], 5000);
    const incus = useResource(() => api.incusCredentials(), [], 10000);
    const [enabling, setEnabling] = useState<string>();
    const incusProvider = infra.data?.providers.find((p) => p.id === "incus");

    async function enable(p: InfrastructureProviderInfo) {
        setEnabling(p.id);
        try {
            await api.enableInfrastructureProvider(p.id);
            toast.success(`Installing ${p.displayName} - this takes a minute or two`);
            infra.reload();
        } catch (err) {
            toast.error(errorMessage(err));
        } finally {
            setEnabling(undefined);
        }
    }

    return (
        <Page
            title="Infrastructure"
            subtitle="Where workload clusters are provisioned, and how to reach it."
        >
            <UpdateSection />
            <Alert>{infra.error}</Alert>
            <h2>Providers</h2>
            {infra.data && !infra.data.managed && (
                <Alert kind="info">
                    Cluster API infrastructure providers are installed on the management cluster
                    with <code>clusterctl init --infrastructure &lt;name&gt;</code>; this page only
                    reports them.
                </Alert>
            )}
            {infra.data?.syncError && (
                <Alert>Installing providers failed: {infra.data.syncError}</Alert>
            )}
            <Table
                rows={infra.data?.providers}
                rowKey={(p) => p.id}
                empty="No infrastructure providers known."
                columns={[
                    { label: "Provider", render: (p) => p.displayName },
                    { label: "Status", render: providerBadge },
                    {
                        label: "Workload clusters",
                        render: (p) =>
                            p.clusterProvider ? (
                                "Supported"
                            ) : (
                                <span className="muted">Not supported by akbench yet</span>
                            ),
                    },
                    {
                        label: "",
                        className: "actions",
                        render: (p) =>
                            infra.data?.managed &&
                            !p.installedVersion &&
                            !p.enabled &&
                            p.availableVersion ? (
                                <button
                                    className="small primary"
                                    disabled={enabling === p.id}
                                    onClick={() => void enable(p)}
                                >
                                    {enabling === p.id ? "Enabling…" : "Enable"}
                                </button>
                            ) : null,
                    },
                ]}
            />
            {infra.data?.syncedAt && (
                <p className="muted">Last synced {formatTimestamp(infra.data.syncedAt)}.</p>
            )}

            <div className="page-head" style={{ marginTop: 24 }}>
                <div>
                    <h2>Incus connections</h2>
                    <div className="sub">
                        The Incus daemons workload clusters are provisioned on - one per
                        server/project. A cluster picks one when it's registered.
                    </div>
                </div>
                <Link className="button primary" to="/infrastructure/incus/new">
                    New connection
                </Link>
            </div>
            {incusProvider && !incusProvider.installedVersion && (
                <Alert kind="info">
                    The Incus provider isn't installed yet
                    {infra.data?.managed && !incusProvider.enabled ? " - enable it above" : ""}.
                    Connections can be set up in the meantime.
                </Alert>
            )}
            <Alert>{incus.error}</Alert>
            <Table
                rows={incus.data}
                rowKey={(c) => c.name}
                empty="No Incus connections yet."
                columns={[
                    {
                        label: "Name",
                        render: (c) => <Link to={`/infrastructure/incus/${c.name}`}>{c.name}</Link>,
                    },
                    { label: "Server", render: (c) => <code>{c.server}</code> },
                    { label: "Project", render: (c) => c.project || "default" },
                    {
                        label: "Used by",
                        render: (c) =>
                            c.usedBy.length === 0 ? (
                                <span className="muted">-</span>
                            ) : (
                                c.usedBy.map((name) => (
                                    <Link
                                        key={name}
                                        className="button small"
                                        to={`/clusters/${name}`}
                                        style={{ marginRight: 4 }}
                                    >
                                        {name}
                                    </Link>
                                ))
                            ),
                    },
                    {
                        label: "",
                        className: "actions",
                        render: (c) => (
                            <span className="row" style={{ justifyContent: "flex-end" }}>
                                <Link
                                    className="button small"
                                    to={`/infrastructure/incus/${c.name}/edit`}
                                >
                                    Edit
                                </Link>
                                <button
                                    className="small danger"
                                    disabled={c.usedBy.length > 0}
                                    title={
                                        c.usedBy.length > 0 ? "Still used by a cluster" : undefined
                                    }
                                    onClick={() =>
                                        void confirmAndRun(
                                            {
                                                title: "Delete Incus connection",
                                                message: `Delete the Incus connection "${c.name}"?`,
                                                confirmLabel: "Delete",
                                                danger: true,
                                            },
                                            () => api.deleteIncusCredential(c.name),
                                            incus.reload,
                                            `Deleted ${c.name}`,
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

type ClientCertMode = "keep" | "generate" | "bring";

function IncusCredentialForm({ existing }: { existing?: IncusCredential }) {
    const navigate = useNavigate();
    const [name, setName] = useState(existing?.name ?? "");
    const [input, setInput] = useState<IncusCredentialInput>({
        server: existing?.server ?? "",
        project: existing?.project ?? "",
        serverCert: existing?.serverCert ?? "",
        insecureSkipVerify: existing?.insecureSkipVerify ?? false,
    });
    const [mode, setMode] = useState<ClientCertMode>(existing?.hasClientKey ? "keep" : "generate");
    const [clientCert, setClientCert] = useState("");
    const [clientKey, setClientKey] = useState("");
    const [error, setError] = useState<string>();
    const [saving, setSaving] = useState(false);
    const patch = (changes: Partial<IncusCredentialInput>) =>
        setInput((current) => ({ ...current, ...changes }));

    async function submit(event: FormEvent) {
        event.preventDefault();
        setSaving(true);
        setError(undefined);
        const body: IncusCredentialInput = {
            ...input,
            ...(mode === "bring" ? { clientCert, clientKey } : {}),
            regenerateClientCert: Boolean(existing) && mode === "generate",
        };
        try {
            if (existing) {
                await api.updateIncusCredential(existing.name, body);
                toast.success(`Saved ${existing.name}`);
            } else {
                await api.createIncusCredential({ ...body, name });
                toast.success(`Created ${name}`);
            }
            navigate(`/infrastructure/incus/${existing?.name ?? name}`);
        } catch (err) {
            setError(errorMessage(err));
            setSaving(false);
        }
    }

    return (
        <Page title={existing ? `Edit ${existing.name}` : "New Incus connection"}>
            <form onSubmit={submit}>
                <Alert>{error}</Alert>
                {existing && !existing.labeled && (
                    <Alert kind="info">
                        This connection was created outside the web UI; saving it here makes it
                        managed here.
                    </Alert>
                )}
                <fieldset>
                    <legend>Server</legend>
                    <div className="fields">
                        <Field
                            label="Name"
                            help={'What a cluster\'s connection setting refers to, e.g. "lab"'}
                        >
                            <input
                                required
                                value={name}
                                disabled={Boolean(existing)}
                                onChange={(e) => setName(k8sNameInput(e.target.value))}
                            />
                        </Field>
                        <Field label="Server URL" help="The Incus API, usually on port 8443">
                            <input
                                required
                                type="url"
                                value={input.server}
                                placeholder="https://incus.example.com:8443"
                                onChange={(e) => patch({ server: e.target.value })}
                            />
                        </Field>
                        <Field label="Project" help="Incus project instances are created in">
                            <input
                                value={input.project ?? ""}
                                placeholder="default"
                                onChange={(e) => patch({ project: e.target.value })}
                            />
                        </Field>
                        <Field
                            label="Server certificate"
                            help="Optional: pins the server's own (usually self-signed) certificate. Easiest to fill in from the connection test."
                        >
                            <textarea
                                rows={4}
                                value={input.serverCert ?? ""}
                                placeholder="-----BEGIN CERTIFICATE-----"
                                onChange={(e) => patch({ serverCert: e.target.value })}
                            />
                        </Field>
                    </div>
                    <label className="check">
                        <input
                            type="checkbox"
                            checked={input.insecureSkipVerify}
                            onChange={(e) => patch({ insecureSkipVerify: e.target.checked })}
                        />
                        Skip server certificate verification (not recommended - pin the certificate
                        instead)
                    </label>
                </fieldset>

                <fieldset>
                    <legend>Client certificate</legend>
                    <p className="muted">
                        What akbench authenticates to Incus with. The server has to trust it - the
                        next page walks through that.
                    </p>
                    <div className="row" style={{ marginBottom: 8 }}>
                        {existing?.hasClientKey && (
                            <label className="check">
                                <input
                                    type="radio"
                                    checked={mode === "keep"}
                                    onChange={() => setMode("keep")}
                                />
                                Keep the current certificate
                            </label>
                        )}
                        <label className="check">
                            <input
                                type="radio"
                                checked={mode === "generate"}
                                onChange={() => setMode("generate")}
                            />
                            {existing?.hasClientKey
                                ? "Generate a new one"
                                : "Generate one (recommended)"}
                        </label>
                        <label className="check">
                            <input
                                type="radio"
                                checked={mode === "bring"}
                                onChange={() => setMode("bring")}
                            />
                            Use an existing certificate
                        </label>
                    </div>
                    {mode === "generate" && existing?.hasClientKey && (
                        <Alert kind="info">
                            The new certificate has to be trusted on the server again before
                            clusters can be managed.
                        </Alert>
                    )}
                    {mode === "bring" && (
                        <div className="fields">
                            <Field label="Certificate">
                                <textarea
                                    rows={5}
                                    required
                                    value={clientCert}
                                    onChange={(e) => setClientCert(e.target.value)}
                                />
                            </Field>
                            <Field
                                label="Private key"
                                help="Write-only: never shown again once saved"
                            >
                                <textarea
                                    rows={5}
                                    required
                                    value={clientKey}
                                    onChange={(e) => setClientKey(e.target.value)}
                                />
                            </Field>
                        </div>
                    )}
                </fieldset>

                <div className="form-actions">
                    <button type="submit" className="primary" disabled={saving}>
                        {saving ? "Saving…" : existing ? "Save changes" : "Create connection"}
                    </button>
                    <button type="button" onClick={() => navigate(-1)}>
                        Cancel
                    </button>
                </div>
            </form>
        </Page>
    );
}

export function IncusCredentialCreatePage() {
    return <IncusCredentialForm />;
}

export function IncusCredentialEditPage() {
    const { name = "" } = useParams();
    const existing = useResource(() => api.incusCredential(name), [name], 0);
    if (existing.error) {
        return (
            <Page title="Edit Incus connection">
                <Alert>{existing.error}</Alert>
            </Page>
        );
    }
    return existing.data ? <IncusCredentialForm key={name} existing={existing.data} /> : null;
}

function download(filename: string, content: string) {
    const url = URL.createObjectURL(new Blob([content], { type: "application/x-pem-file" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    a.click();
    URL.revokeObjectURL(url);
}

function inputFrom(c: IncusCredential): IncusCredentialInput {
    return {
        server: c.server,
        project: c.project,
        serverCert: c.serverCert,
        insecureSkipVerify: c.insecureSkipVerify,
    };
}

export function IncusCredentialViewPage() {
    const { name = "" } = useParams();
    const credential = useResource(() => api.incusCredential(name), [name], 15000);
    const c = credential.data;
    const [probe, setProbe] = useState<IncusProbe>();
    const [probing, setProbing] = useState(false);
    const [token, setToken] = useState("");
    const [busy, setBusy] = useState(false);

    const test = useCallback(async () => {
        setProbing(true);
        try {
            setProbe(await api.testIncusCredential(name));
        } catch (err) {
            setProbe({ reachable: false, trusted: false, message: errorMessage(err) });
        } finally {
            setProbing(false);
        }
    }, [name]);

    useEffect(() => {
        // oxlint-disable-next-line react/set-state-in-effect -- kicks off the async probe on load
        void test();
    }, [test]);

    async function pin() {
        if (!c || !probe?.serverCert) {
            return;
        }
        setBusy(true);
        try {
            await api.updateIncusCredential(name, {
                ...inputFrom(c),
                serverCert: probe.serverCert,
                insecureSkipVerify: false,
            });
            toast.success("Pinned the server certificate");
            credential.reload();
            await test();
        } catch (err) {
            toast.error(errorMessage(err));
        } finally {
            setBusy(false);
        }
    }

    async function trust(event: FormEvent) {
        event.preventDefault();
        setBusy(true);
        try {
            setProbe(await api.trustIncusCredential(name, token));
            setToken("");
            toast.success("The server now trusts this connection");
        } catch (err) {
            toast.error(errorMessage(err));
        } finally {
            setBusy(false);
        }
    }

    const certFile = `akbench-${name}.crt`;
    return (
        <Page
            title={name}
            subtitle={c && <code>{c.server}</code>}
            actions={
                <>
                    <button onClick={() => void test()} disabled={probing}>
                        {probing ? "Testing…" : "Test connection"}
                    </button>
                    <Link className="button" to={`/infrastructure/incus/${name}/edit`}>
                        Edit
                    </Link>
                </>
            }
        >
            <Alert>{credential.error}</Alert>

            <h2>Connection</h2>
            {!probe ? (
                <p className="muted">Testing…</p>
            ) : (
                <div className="panel">
                    <div className="row" style={{ marginBottom: probe.message ? 8 : 0 }}>
                        <span className={`badge ${probe.reachable ? "ok" : "err"}`}>
                            {probe.reachable ? "Reachable" : "Unreachable"}
                        </span>
                        {probe.reachable && (
                            <span className={`badge ${probe.trusted ? "ok" : "warn"}`}>
                                {probe.trusted ? "Trusted" : "Not trusted"}
                            </span>
                        )}
                        {probe.serverVersion && (
                            <span className="muted">Incus {probe.serverVersion}</span>
                        )}
                    </div>
                    {probe.message && <div className="muted">{probe.message}</div>}
                    {!probe.reachable && probe.serverCert && (
                        <div style={{ marginTop: 12 }}>
                            <p>
                                The server presented a certificate with SHA-256 fingerprint{" "}
                                <code>{probe.serverCertFingerprint}</code>. Compare it with the
                                fingerprint <code>incus info</code> prints on the server before
                                pinning it.
                            </p>
                            <button className="primary" disabled={busy} onClick={() => void pin()}>
                                Pin this certificate
                            </button>
                        </div>
                    )}
                </div>
            )}

            {probe?.reachable && !probe.trusted && c?.clientCert && (
                <>
                    <h2>Trust this connection on the server</h2>
                    <div className="panel" style={{ marginBottom: 12 }}>
                        <p>
                            <strong>With a trust token</strong> - run this on the Incus server and
                            paste the token it prints:
                        </p>
                        <pre className="yaml">incus config trust add akbench-{name}</pre>
                        <form className="row" onSubmit={trust}>
                            <input
                                required
                                style={{ flex: 1, minWidth: 240 }}
                                value={token}
                                placeholder="Trust token"
                                onChange={(e) => setToken(e.target.value)}
                            />
                            <button type="submit" className="primary" disabled={busy}>
                                Trust
                            </button>
                        </form>
                    </div>
                    <div className="panel">
                        <p>
                            <strong>Or with the certificate</strong> - copy it onto the server and
                            add it:
                        </p>
                        <pre className="yaml">
                            incus config trust add-certificate {certFile} --name akbench-{name}
                        </pre>
                        <div className="row">
                            <button onClick={() => download(certFile, c.clientCert!)}>
                                Download {certFile}
                            </button>
                            <button
                                onClick={() =>
                                    void navigator.clipboard
                                        .writeText(c.clientCert!)
                                        .then(() => toast.success("Copied"))
                                }
                            >
                                Copy certificate
                            </button>
                        </div>
                    </div>
                </>
            )}

            {c && (
                <>
                    <h2>Details</h2>
                    <table className="table">
                        <tbody>
                            <tr>
                                <th>Project</th>
                                <td>{c.project || "default"}</td>
                            </tr>
                            <tr>
                                <th>Server certificate</th>
                                <td>
                                    {c.insecureSkipVerify
                                        ? "Not verified"
                                        : c.serverCert
                                          ? "Pinned"
                                          : "Verified against system CAs"}
                                </td>
                            </tr>
                            <tr>
                                <th>Client certificate</th>
                                <td>
                                    <code>{c.clientCertFingerprint || "-"}</code>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                    <h2>Used by</h2>
                    {c.usedBy.length === 0 ? (
                        <Empty>
                            No clusters use this connection yet.{" "}
                            <Link to="/clusters/new">Register one</Link>.
                        </Empty>
                    ) : (
                        <div className="row">
                            {c.usedBy.map((cluster) => (
                                <Link
                                    key={cluster}
                                    className="button small"
                                    to={`/clusters/${cluster}`}
                                >
                                    {cluster}
                                </Link>
                            ))}
                        </div>
                    )}
                </>
            )}
        </Page>
    );
}
