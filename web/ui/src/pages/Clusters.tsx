import { api, MANAGEMENT_CLUSTER } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { MonitoringCharts } from "../components/MetricsChart";
import { ObjectYaml } from "../components/ObjectYaml";
import { AddonList, Badge, ConditionList, StatusBadge } from "../components/Status";
import { toast } from "../components/Toast";
import { Alert, confirmAndRun, errorMessage, Field, Page, Table } from "../components/ui";
import { formatDuration } from "../format";
import { shellLinkForCluster } from "../shell/links";
import {
    ClusterPool,
    WorkerPoolRole,
    WorkerPoolSpec,
    WorkloadCluster,
    WorkloadClusterSpec,
} from "../types";
import { useResource } from "../useResource";

import { FormEvent, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";

export function ClustersPage() {
    const clusters = useResource(() => api.clusters(), [], 5000, "WorkloadCluster");
    return (
        <Page
            title="Clusters"
            subtitle="Where deployments and test runners run: the management cluster plus every provisioned workload cluster."
            actions={
                <Link className="button primary" to="/clusters/new">
                    Register cluster
                </Link>
            }
        >
            <Alert>{clusters.error}</Alert>
            <Table
                rows={clusters.data}
                rowKey={(c) => c.name}
                empty="No clusters."
                columns={[
                    {
                        label: "Name",
                        render: (c) => <Link to={`/clusters/${c.name}`}>{c.name}</Link>,
                    },
                    { label: "Role", render: (c) => (c.management ? "Management" : "Workload") },
                    { label: "Phase", render: (c) => <Badge value={c.phase} /> },
                    {
                        label: "Control plane",
                        render: (c) => (
                            <Badge value={c.controlPlaneReady ? "Ready" : "Not ready"} />
                        ),
                    },
                    {
                        label: "Infrastructure",
                        render: (c) => (
                            <Badge value={c.infrastructureReady ? "Ready" : "Not ready"} />
                        ),
                    },
                    {
                        label: "",
                        className: "actions",
                        render: (c) => (
                            <span className="row" style={{ justifyContent: "flex-end" }}>
                                <Link
                                    className="button small"
                                    to={shellLinkForCluster(c.management ? undefined : c.name)}
                                >
                                    Shell
                                </Link>
                                {!c.management && (
                                    <>
                                        <button
                                            className="small"
                                            title="Nudge the operator into reconciling this cluster right now, instead of waiting for its next naturally-triggered update."
                                            onClick={() =>
                                                void api
                                                    .kickWorkloadCluster(c.name)
                                                    .then(() => toast.success(`Kicked ${c.name}`))
                                                    .catch((err) => toast.error(errorMessage(err)))
                                            }
                                        >
                                            Kick
                                        </button>
                                        <Link
                                            className="button small"
                                            to={`/clusters/${c.name}/edit`}
                                        >
                                            Edit
                                        </Link>
                                        <button
                                            className="small danger"
                                            onClick={() =>
                                                void confirmAndRun(
                                                    {
                                                        title: "Deprovision cluster",
                                                        message: `Deprovision cluster "${c.name}"? This tears down every machine Cluster API created for it.`,
                                                        confirmLabel: "Deprovision",
                                                        danger: true,
                                                    },
                                                    () => api.deprovisionCluster(c.name),
                                                    clusters.reload,
                                                    `Deprovisioning ${c.name}`,
                                                )
                                            }
                                        >
                                            Deprovision
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

// The latest patch of each of the last 5 minor releases, newest first.
// Static rather than fetched live: a creation form that depends on reaching
// an external version index is a worse failure mode than a stale list.
// Sourced from dl.k8s.io/release/stable-<minor>.txt.
const KUBERNETES_VERSIONS = ["v1.36.4", "v1.35.8", "v1.34.11", "v1.33.13", "v1.32.13"];

// cluster-api-provider-incus's own "cX-mY" flavors (X vCPUs, Y GiB RAM).
const INSTANCE_FLAVORS = ["c1-m2", "c2-m4", "c4-m8", "c8-m16", "c16-m32"];

// Mirrors api/v1alpha1.WorkerPoolRole's own enum - Generic first, since it's
// the zero value and what every pool defaults to.
const WORKER_POOL_ROLES: WorkerPoolRole[] = ["Generic", "Authentik", "K6", "Monitoring"];

function roleLabel(role: WorkerPoolRole | undefined): string {
    return role && role !== "Generic" ? role : "Generic (no pool taint)";
}

// The monitoring pool runs the whole observability stack (Prometheus,
// Grafana, Tempo, Pyroscope, Loki, Alertmanager, ...) and scrapes every node plus
// k6's remote writes - on 4 GiB it ran out of memory under load and took the
// node down with it. Choosing the Monitoring role bumps a smaller flavor to
// this one.
const MIN_MONITORING_FLAVOR = "c4-m8";

function flavorMemoryGi(flavor: string | undefined): number {
    const match = /^c\d+-m(\d+)$/.exec(flavor ?? "");
    return match ? Number(match[1]) : 0;
}

function flavorLabel(flavor: string): string {
    const match = /^c(\d+)-m(\d+)$/.exec(flavor);
    return match ? `${flavor} - ${match[1]} vCPU, ${match[2]} GiB RAM` : flavor;
}

function emptyPool(name: string): WorkerPoolSpec {
    return { name, replicas: 2, incus: { flavor: "c2-m4" } };
}

function emptySpec(): WorkloadClusterSpec {
    return {
        kubernetesVersion: KUBERNETES_VERSIONS[0],
        provider: "Incus",
        controlPlane: { replicas: 1 },
        workerPools: [emptyPool("workers")],
        incus: {
            // Filled in with the first Incus connection once they've loaded.
            lxcSecretRef: { name: "" },
            image: "ubuntu:24.04",
            profiles: ["default"],
            controlPlane: { flavor: "c2-m4" },
        },
    };
}

/**
 * Registers a WorkloadCluster, or edits one. Editing is deliberately
 * narrow: WorkloadClusterReconciler only supports day-2 changes to worker
 * pools, so the control plane and the Incus-wide settings are read-only
 * once a cluster exists rather than accepting an edit that would never
 * take effect.
 */
function ClusterForm({ existing }: { existing?: WorkloadCluster }) {
    const navigate = useNavigate();
    const [name, setName] = useState(existing?.metadata.name ?? "");
    const [spec, setSpec] = useState<WorkloadClusterSpec>(existing?.spec ?? emptySpec());
    const [error, setError] = useState<string>();
    const [saving, setSaving] = useState(false);
    const locked = Boolean(existing);
    const infra = useResource(() => api.infrastructure(), [], 15000);
    const incusConnections = useResource(() => api.incusCredentials(), [], 15000);
    const providers = (infra.data?.providers ?? []).filter((p) => p.clusterProvider);
    const selectedProvider = providers.find((p) => p.clusterProvider === spec.provider);

    // A new cluster defaults to the first Incus connection there is.
    const firstConnection = incusConnections.data?.[0]?.name;
    if (!locked && firstConnection && spec.incus && !spec.incus.lxcSecretRef.name) {
        setSpec({ ...spec, incus: { ...spec.incus, lxcSecretRef: { name: firstConnection } } });
    }

    const patch = (changes: Partial<WorkloadClusterSpec>) =>
        setSpec((current) => ({ ...current, ...changes }));
    const patchIncus = (changes: Partial<NonNullable<WorkloadClusterSpec["incus"]>>) =>
        setSpec((current) => ({ ...current, incus: { ...current.incus!, ...changes } }));
    const patchPool = (index: number, changes: Partial<WorkerPoolSpec>) =>
        patch({
            workerPools: spec.workerPools.map((p, i) => (i === index ? { ...p, ...changes } : p)),
        });

    async function submit(event: FormEvent) {
        event.preventDefault();
        setSaving(true);
        setError(undefined);
        try {
            const savedName = existing?.metadata.name ?? name;
            if (existing) {
                await api.updateWorkloadCluster(savedName, { spec });
                toast.success(`Saved ${savedName}`);
            } else {
                await api.registerCluster({ metadata: { name }, spec });
                toast.success(`Registered ${savedName}`);
            }
            navigate(`/clusters/${savedName}`);
        } catch (err) {
            setError(errorMessage(err));
            setSaving(false);
        }
    }

    const versions =
        existing && !KUBERNETES_VERSIONS.includes(spec.kubernetesVersion)
            ? [spec.kubernetesVersion, ...KUBERNETES_VERSIONS]
            : KUBERNETES_VERSIONS;

    return (
        <Page title={existing ? `Edit ${existing.metadata.name}` : "Register cluster"}>
            <form onSubmit={submit}>
                <Alert>{error}</Alert>
                <fieldset>
                    <legend>Identity</legend>
                    <div className="fields">
                        <Field
                            label="Name"
                            help="A Kubernetes object name: lowercase letters, digits and dashes"
                        >
                            <input
                                required
                                value={name}
                                disabled={locked}
                                onChange={(e) => setName(e.target.value)}
                            />
                        </Field>
                        <Field label="Kubernetes version">
                            <select
                                value={spec.kubernetesVersion}
                                disabled={locked}
                                onChange={(e) => patch({ kubernetesVersion: e.target.value })}
                            >
                                {versions.map((v, i) => (
                                    <option key={v} value={v}>
                                        {v}
                                        {i === 0 && !existing ? " (latest)" : ""}
                                    </option>
                                ))}
                            </select>
                        </Field>
                        <Field
                            label="Infrastructure provider"
                            help={<Link to="/infrastructure">Manage infrastructure</Link>}
                        >
                            <select
                                disabled={locked || providers.length <= 1}
                                value={spec.provider}
                                onChange={(e) =>
                                    patch({
                                        provider: e.target.value as WorkloadClusterSpec["provider"],
                                    })
                                }
                            >
                                {providers.length === 0 && (
                                    <option value="Incus">Incus / LXC</option>
                                )}
                                {providers.map((p) => (
                                    <option key={p.id} value={p.clusterProvider}>
                                        {p.displayName}
                                    </option>
                                ))}
                            </select>
                        </Field>
                    </div>
                    {!locked && selectedProvider && !selectedProvider.installedVersion && (
                        <Alert kind="info">
                            The {selectedProvider.displayName} provider isn't installed on the
                            management cluster yet, so this cluster won't start provisioning until
                            it is - <Link to="/infrastructure">set it up under Infrastructure</Link>
                            .
                        </Alert>
                    )}
                </fieldset>

                <fieldset>
                    <legend>Control plane</legend>
                    {locked && (
                        <p className="muted">
                            Not reconfigurable after creation - register a replacement to change it.
                        </p>
                    )}
                    <div className="fields">
                        <Field label="Replicas">
                            <input
                                type="number"
                                min={1}
                                disabled={locked}
                                value={spec.controlPlane.replicas}
                                onChange={(e) =>
                                    patch({ controlPlane: { replicas: Number(e.target.value) } })
                                }
                            />
                        </Field>
                        <Field label="Flavor">
                            <select
                                disabled={locked}
                                value={spec.incus?.controlPlane?.flavor ?? INSTANCE_FLAVORS[1]}
                                onChange={(e) =>
                                    patchIncus({ controlPlane: { flavor: e.target.value } })
                                }
                            >
                                {INSTANCE_FLAVORS.map((f) => (
                                    <option key={f} value={f}>
                                        {flavorLabel(f)}
                                    </option>
                                ))}
                            </select>
                        </Field>
                    </div>
                </fieldset>

                <fieldset>
                    <legend>Worker pools</legend>
                    <p className="muted">
                        A role reserves a pool for one of this project's own workloads: its nodes
                        get a matching taint/label, and that workload gets a matching
                        nodeSelector/toleration. Generic schedules anywhere.
                    </p>
                    {spec.workerPools.map((pool, i) => (
                        <div className="pool" key={i}>
                            <div className="fields">
                                <Field label="Pool name">
                                    <input
                                        required
                                        value={pool.name}
                                        onChange={(e) => patchPool(i, { name: e.target.value })}
                                    />
                                </Field>
                                <Field label="Role">
                                    <select
                                        value={pool.role ?? "Generic"}
                                        onChange={(e) => {
                                            const role = e.target.value as WorkerPoolRole;
                                            const flavor =
                                                pool.incus?.flavor ?? INSTANCE_FLAVORS[1];
                                            patchPool(i, {
                                                role,
                                                ...(role === "Monitoring" &&
                                                flavorMemoryGi(flavor) <
                                                    flavorMemoryGi(MIN_MONITORING_FLAVOR)
                                                    ? {
                                                          incus: {
                                                              ...pool.incus,
                                                              flavor: MIN_MONITORING_FLAVOR,
                                                          },
                                                      }
                                                    : {}),
                                            });
                                        }}
                                    >
                                        {WORKER_POOL_ROLES.map((r) => (
                                            <option key={r} value={r}>
                                                {roleLabel(r)}
                                            </option>
                                        ))}
                                    </select>
                                </Field>
                                <Field
                                    label="Flavor"
                                    help={
                                        pool.role === "Monitoring" &&
                                        flavorMemoryGi(pool.incus?.flavor ?? INSTANCE_FLAVORS[1]) <
                                            flavorMemoryGi(MIN_MONITORING_FLAVOR)
                                            ? `Too small for the monitoring stack under load - use ${MIN_MONITORING_FLAVOR} or larger`
                                            : undefined
                                    }
                                >
                                    <select
                                        value={pool.incus?.flavor ?? INSTANCE_FLAVORS[1]}
                                        onChange={(e) =>
                                            patchPool(i, {
                                                incus: { ...pool.incus, flavor: e.target.value },
                                            })
                                        }
                                    >
                                        {INSTANCE_FLAVORS.map((f) => (
                                            <option key={f} value={f}>
                                                {flavorLabel(f)}
                                            </option>
                                        ))}
                                    </select>
                                </Field>
                            </div>
                            <div className="fields">
                                <Field
                                    label={pool.autoscaling ? "Initial replicas" : "Replicas"}
                                    help={
                                        pool.autoscaling &&
                                        "Only sizes the pool at creation; cluster-autoscaler owns it after that"
                                    }
                                >
                                    <input
                                        type="number"
                                        min={1}
                                        value={pool.replicas}
                                        onChange={(e) =>
                                            patchPool(i, { replicas: Number(e.target.value) })
                                        }
                                    />
                                </Field>
                                {pool.autoscaling && (
                                    <>
                                        <Field
                                            label="Min nodes"
                                            help="0 lets the pool scale to zero when idle"
                                        >
                                            <input
                                                type="number"
                                                required
                                                min={0}
                                                max={pool.autoscaling.maxReplicas}
                                                value={pool.autoscaling.minReplicas ?? 0}
                                                onChange={(e) =>
                                                    patchPool(i, {
                                                        autoscaling: {
                                                            ...pool.autoscaling!,
                                                            minReplicas: Number(e.target.value),
                                                        },
                                                    })
                                                }
                                            />
                                        </Field>
                                        <Field label="Max nodes">
                                            <input
                                                type="number"
                                                required
                                                min={Math.max(1, pool.autoscaling.minReplicas ?? 0)}
                                                value={pool.autoscaling.maxReplicas}
                                                onChange={(e) =>
                                                    patchPool(i, {
                                                        autoscaling: {
                                                            ...pool.autoscaling!,
                                                            maxReplicas: Number(e.target.value),
                                                        },
                                                    })
                                                }
                                            />
                                        </Field>
                                    </>
                                )}
                            </div>
                            <div className="pool-footer">
                                <label className="check">
                                    <input
                                        type="checkbox"
                                        checked={Boolean(pool.autoscaling)}
                                        onChange={(e) =>
                                            patchPool(i, {
                                                autoscaling: e.target.checked
                                                    ? {
                                                          minReplicas: pool.replicas,
                                                          maxReplicas: Math.max(pool.replicas, 3),
                                                      }
                                                    : undefined,
                                            })
                                        }
                                    />
                                    Autoscale
                                </label>
                                <button
                                    type="button"
                                    className="small danger"
                                    disabled={spec.workerPools.length <= 1}
                                    onClick={() =>
                                        patch({
                                            workerPools: spec.workerPools.filter((_, j) => j !== i),
                                        })
                                    }
                                >
                                    Remove pool
                                </button>
                            </div>
                        </div>
                    ))}
                    <button
                        type="button"
                        onClick={() =>
                            patch({
                                workerPools: [
                                    ...spec.workerPools,
                                    emptyPool(`pool-${spec.workerPools.length + 1}`),
                                ],
                            })
                        }
                    >
                        Add worker pool
                    </button>
                </fieldset>

                {spec.provider === "Incus" && (
                    <fieldset>
                        <legend>Incus / LXC</legend>
                        {locked && (
                            <p className="muted">
                                Image and profiles apply to new or edited worker pools only.
                            </p>
                        )}
                        <div className="fields">
                            <Field
                                label="Incus connection"
                                help={
                                    <Link
                                        to={
                                            incusConnections.data?.length === 0
                                                ? "/infrastructure/incus/new"
                                                : "/infrastructure"
                                        }
                                    >
                                        {incusConnections.data?.length === 0
                                            ? "Create a connection first"
                                            : "Manage connections"}
                                    </Link>
                                }
                            >
                                <select
                                    required
                                    disabled={locked}
                                    value={spec.incus?.lxcSecretRef.name ?? ""}
                                    onChange={(e) =>
                                        patchIncus({ lxcSecretRef: { name: e.target.value } })
                                    }
                                >
                                    <option value="" disabled>
                                        {incusConnections.data === undefined
                                            ? "Loading…"
                                            : "Select a connection"}
                                    </option>
                                    {/* An existing cluster's connection may not be listed (e.g. deleted since). */}
                                    {spec.incus?.lxcSecretRef.name &&
                                        !incusConnections.data?.some(
                                            (c) => c.name === spec.incus?.lxcSecretRef.name,
                                        ) && (
                                            <option value={spec.incus.lxcSecretRef.name}>
                                                {spec.incus.lxcSecretRef.name}
                                            </option>
                                        )}
                                    {incusConnections.data?.map((c) => (
                                        <option key={c.name} value={c.name}>
                                            {c.name} ({c.server})
                                        </option>
                                    ))}
                                </select>
                            </Field>
                            <Field label="Image">
                                <input
                                    value={spec.incus?.image ?? ""}
                                    placeholder="ubuntu:24.04"
                                    onChange={(e) => patchIncus({ image: e.target.value })}
                                />
                            </Field>
                            <Field label="Profiles" help="Comma-separated">
                                <input
                                    defaultValue={(spec.incus?.profiles ?? []).join(",")}
                                    placeholder="default"
                                    onChange={(e) =>
                                        patchIncus({
                                            profiles: e.target.value
                                                .split(",")
                                                .map((p) => p.trim())
                                                .filter(Boolean),
                                        })
                                    }
                                />
                            </Field>
                        </div>
                    </fieldset>
                )}

                <div className="form-actions">
                    <button type="submit" className="primary" disabled={saving}>
                        {saving ? "Saving…" : existing ? "Save changes" : "Register cluster"}
                    </button>
                    <button type="button" onClick={() => navigate(-1)}>
                        Cancel
                    </button>
                </div>
            </form>
        </Page>
    );
}

export function ClusterCreatePage() {
    return <ClusterForm />;
}

export function ClusterEditPage() {
    const { name = "" } = useParams();
    const existing = useResource(() => api.workloadCluster(name), [name], 0);
    if (existing.error) {
        return (
            <Page title="Edit cluster">
                <Alert>{existing.error}</Alert>
            </Page>
        );
    }
    return existing.data ? <ClusterForm key={name} existing={existing.data} /> : null;
}

/**
 * A cluster's view. "Is this cluster healthy" is a kubectl question, so the
 * main action is a shell on it; this page adds what the shell can't show at
 * a glance - the WorkloadCluster's own status and sizing.
 */
export function ClusterViewPage() {
    const { name = "" } = useParams();
    const isManagement = name === MANAGEMENT_CLUSTER;
    const wc = useResource(
        () => (isManagement ? Promise.resolve(undefined) : api.workloadCluster(name)),
        [name, isManagement],
        10000,
        "WorkloadCluster",
    );
    const shellTarget = isManagement ? undefined : name;
    const status = wc.data?.status;
    const pools = wc.data?.spec.workerPools ?? [];
    const nodes = useResource(
        () => (isManagement ? Promise.resolve(undefined) : api.clusterNodes(name)),
        [name, isManagement],
        10000,
    );
    const poolSize = (pool: string) => nodes.data?.pools.find((p) => p.name === pool);
    const [kicking, setKicking] = useState(false);

    const kick = async () => {
        setKicking(true);
        try {
            await api.kickWorkloadCluster(name);
            wc.reload();
            toast.success(`Kicked ${name}`);
        } catch (err) {
            toast.error(errorMessage(err));
        } finally {
            setKicking(false);
        }
    };

    return (
        <Page
            title={name}
            subtitle={
                isManagement
                    ? "The management cluster - where the operator and this UI run."
                    : "WorkloadCluster"
            }
            actions={
                <>
                    <Link className="button primary" to={shellLinkForCluster(shellTarget)}>
                        Open shell
                    </Link>
                    {!isManagement && (
                        <>
                            <button
                                className="button"
                                disabled={kicking}
                                title="Nudge the operator into reconciling this cluster right now, instead of waiting for its next naturally-triggered update."
                                onClick={() => void kick()}
                            >
                                {kicking ? "Kicking…" : "Kick"}
                            </button>
                            <Link className="button" to={`/clusters/${name}/edit`}>
                                Edit
                            </Link>
                        </>
                    )}
                </>
            }
        >
            <Alert>{wc.error}</Alert>
            <h2>Quick checks</h2>
            <div className="row">
                <Link
                    className="button"
                    to={shellLinkForCluster(shellTarget, "kubectl get nodes -o wide")}
                >
                    Nodes
                </Link>
                <Link
                    className="button"
                    to={shellLinkForCluster(
                        shellTarget,
                        "kubectl get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded",
                    )}
                >
                    Unhealthy pods
                </Link>
                <Link className="button" to={shellLinkForCluster(shellTarget, "kubectl top nodes")}>
                    Node usage
                </Link>
                <Link
                    className="button"
                    to={shellLinkForCluster(
                        shellTarget,
                        "kubectl get events -A --sort-by=.lastTimestamp | tail -50",
                    )}
                >
                    Recent events
                </Link>
            </div>

            {wc.data && (
                <>
                    <h2>Status</h2>
                    <div className="panel">
                        <div className="row" style={{ marginBottom: 8 }}>
                            <StatusBadge
                                kind="workloadcluster"
                                name={name}
                                namespace={wc.data.metadata.namespace}
                                phase={status?.phase}
                                conditions={status?.conditions}
                            />
                            <span>
                                Control plane{" "}
                                <Badge value={status?.controlPlaneReady ? "Ready" : "Not ready"} />
                            </span>
                            <span>
                                Infrastructure{" "}
                                <Badge
                                    value={status?.infrastructureReady ? "Ready" : "Not ready"}
                                />
                            </span>
                            <span className="muted">
                                Kubernetes {wc.data.spec.kubernetesVersion}
                            </span>
                        </div>
                        <ConditionList conditions={status?.conditions} />
                        <AddonList
                            addons={status?.addons}
                            controlPlaneReady={status?.controlPlaneReady}
                        />
                        <ActivityLog path="workloadclusters" name={name} />
                    </div>

                    <h2>Worker pools</h2>
                    <Table
                        rows={pools}
                        rowKey={(p) => p.name}
                        empty="No worker pools."
                        columns={[
                            { label: "Pool", render: (p) => p.name },
                            { label: "Role", render: (p) => <Badge value={roleLabel(p.role)} /> },
                            {
                                label: "Nodes",
                                render: (p) => (
                                    <PoolNodes
                                        size={poolSize(p.name)}
                                        loaded={nodes.data !== undefined}
                                    />
                                ),
                            },
                            {
                                label: "Size",
                                render: (p) =>
                                    p.autoscaling
                                        ? `autoscaling ${p.autoscaling.minReplicas ?? 0}-${p.autoscaling.maxReplicas}`
                                        : `${p.replicas} (fixed)`,
                            },
                            {
                                label: "Flavor",
                                render: (p) =>
                                    p.incus?.flavor ? flavorLabel(p.incus.flavor) : "-",
                            },
                        ]}
                    />

                    <h2>Nodes</h2>
                    <Alert>{nodes.error}</Alert>
                    <Table
                        rows={nodes.data?.machines}
                        rowKey={(m) => m.name}
                        empty="No machines yet - they appear once Cluster API starts provisioning."
                        columns={[
                            {
                                label: "Node",
                                render: (m) =>
                                    m.nodeName ?? (
                                        <span className="muted">{m.name} (not joined yet)</span>
                                    ),
                            },
                            { label: "Pool", render: (m) => m.pool },
                            {
                                label: "Status",
                                render: (m) => (
                                    <Badge value={m.ready ? "Ready" : (m.phase ?? "Pending")} />
                                ),
                            },
                            { label: "Internal IP", render: (m) => m.internalIP ?? "-" },
                            { label: "Version", render: (m) => m.version ?? "-" },
                            {
                                label: "Age",
                                render: (m) =>
                                    m.created
                                        ? formatDuration(
                                              (Date.now() - Date.parse(m.created)) / 1000,
                                          )
                                        : "-",
                            },
                        ]}
                    />

                    {(status?.prometheusURL || status?.grafanaURL) && (
                        <>
                            <h2>Observability</h2>
                            <div className="row">
                                {status?.prometheusURL && (
                                    <span>
                                        Prometheus: <code>{status.prometheusURL}</code>
                                    </span>
                                )}
                                {status?.grafanaURL && (
                                    <Link
                                        className="button"
                                        to={`/grafana/${encodeURIComponent(name)}`}
                                    >
                                        Open Grafana
                                    </Link>
                                )}
                            </div>
                            {status?.prometheusURL && <MonitoringCharts cluster={name} />}
                        </>
                    )}
                </>
            )}
            <ObjectYaml object={wc.data} />
        </Page>
    );
}

/**
 * PoolNodes shows how many nodes a worker pool has right now: Ready of
 * current, plus where it's heading while Cluster API (or
 * cluster-autoscaler, for an autoscaled pool) is still scaling it.
 */
function PoolNodes({ size, loaded }: { size: ClusterPool | undefined; loaded: boolean }) {
    if (!size) {
        return <span className="muted">{loaded ? "0" : "…"}</span>;
    }
    const scaling = size.desired !== size.current;
    return (
        <span>
            <Badge value={size.ready === size.current && !scaling ? "Ready" : "Pending"} />{" "}
            {size.ready}/{size.current} ready
            {scaling && <span className="muted"> - scaling to {size.desired}</span>}
        </span>
    );
}
