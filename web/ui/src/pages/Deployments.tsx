import { api } from "../api";
import { ActivityLog } from "../components/ActivityLog";
import { CodeEditor } from "../components/CodeEditor";
import { MetricsGrid, PostgresCharts, WorkerCharts } from "../components/MetricsChart";
import { ObjectYaml } from "../components/ObjectYaml";
import { NodePlacement } from "../components/Placement";
import { Rollout } from "../components/Rollout";
import { ConditionList, StatusBadge } from "../components/Status";
import { toast } from "../components/Toast";
import {
    Alert,
    confirmAndRun,
    Empty,
    errorMessage,
    Field,
    Page,
    Table,
    TimeRangeSelect,
} from "../components/ui";
import { earliestBurnStart, parseTimeRange, targetNameOf, timeRangeSeconds } from "../format";
import { psqlCommand, shellLinkForCluster } from "../shell/links";
import {
    AuthentikDeployment,
    AuthentikDeploymentSpec,
    PlacementMode,
    PostgresMode,
    testCaseDeployments,
} from "../types";
import { useResource } from "../useResource";

import { FormEvent, useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";

export function DeploymentsPage() {
    const deployments = useResource(() => api.deployments(), [], 5000, "AuthentikDeployment");

    return (
        <Page
            title="Deployments"
            subtitle="authentik installs to benchmark."
            actions={
                <Link className="button primary" to="/deployments/new">
                    New deployment
                </Link>
            }
        >
            <Alert>{deployments.error}</Alert>
            <Table
                rows={deployments.data}
                rowKey={(d) => d.metadata.name}
                empty="No deployments yet - create one to deploy an authentik install to benchmark."
                columns={[
                    {
                        label: "Name",
                        render: (d) => (
                            <Link to={`/deployments/${d.metadata.name}`}>{d.metadata.name}</Link>
                        ),
                    },
                    { label: "Target", render: (d) => d.spec.targetName || d.metadata.name },
                    { label: "Cluster", render: (d) => d.spec.clusterRef?.name ?? "management" },
                    { label: "Namespace", render: (d) => <code>{d.spec.namespace}</code> },
                    { label: "Postgres", render: (d) => d.spec.postgres.mode },
                    {
                        label: "Replicas",
                        render: (d) =>
                            `${d.spec.server.replicas} server / ${d.spec.worker.replicas} worker`,
                    },
                    {
                        label: "Status",
                        render: (d) => (
                            <StatusBadge
                                kind="authentikdeployment"
                                name={d.metadata.name}
                                namespace={d.metadata.namespace}
                                phase={d.status?.phase}
                                conditions={d.status?.conditions}
                            />
                        ),
                    },
                    {
                        label: "",
                        className: "actions",
                        render: (d) => (
                            <span className="row" style={{ justifyContent: "flex-end" }}>
                                <button
                                    className="small"
                                    title="Nudge the operator into reconciling this deployment right now, instead of waiting for its next naturally-triggered update."
                                    onClick={() =>
                                        void api
                                            .kickDeployment(d.metadata.name)
                                            .then(() => toast.success(`Kicked ${d.metadata.name}`))
                                            .catch((err) => toast.error(errorMessage(err)))
                                    }
                                >
                                    Kick
                                </button>
                                <Link
                                    className="button small"
                                    to={`/deployments/${d.metadata.name}/edit`}
                                >
                                    Edit
                                </Link>
                                <Link className="button small" to={duplicateLink(d.metadata.name)}>
                                    Duplicate
                                </Link>
                                <button
                                    className="small danger"
                                    onClick={() =>
                                        void confirmAndRun(
                                            {
                                                title: "Delete deployment",
                                                message: `Delete deployment "${d.metadata.name}"? This tears down its Helm release.`,
                                                confirmLabel: "Delete",
                                                danger: true,
                                            },
                                            () => api.deleteDeployment(d.metadata.name),
                                            deployments.reload,
                                            `Deleted ${d.metadata.name}`,
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

/**
 * emptySpec mirrors the CRD's own defaults, so the form shows what will
 * actually be created rather than blanks the API server fills in invisibly.
 */
export function emptySpec(): AuthentikDeploymentSpec {
    return {
        namespace: "",
        server: { replicas: 1 },
        worker: { replicas: 1 },
        placement: { mode: "Default", topologyKey: "kubernetes.io/hostname" },
        postgres: { mode: "Embedded", embedded: {} },
        chart: { repo: "https://charts.goauthentik.io" },
        authentik: { image: "ghcr.io/goauthentik/server" },
    };
}

/** slugify turns a deployment name into a namespace-safe default. */
export function slugify(value: string): string {
    return value
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "");
}

/** duplicateLink opens the new deployment form prefilled from `name`. */
function duplicateLink(name: string): string {
    return `/deployments/new?${new URLSearchParams({ from: name })}`;
}

/**
 * duplicateSpec copies a deployment's spec for a new one. The namespace and
 * target name are cleared so they follow the new name instead of colliding
 * with the source's.
 */
function duplicateSpec(source: AuthentikDeployment): AuthentikDeploymentSpec {
    const spec = structuredClone(source.spec);
    spec.namespace = "";
    delete spec.targetName;
    return spec;
}

function DeploymentForm({
    existing,
    source,
}: {
    existing?: AuthentikDeployment;
    source?: AuthentikDeployment;
}) {
    const navigate = useNavigate();
    const [name, setName] = useState(existing?.metadata.name ?? "");
    // Starts from the fetched spec, so fields this form has no input for are
    // carried through a save untouched.
    const [spec, setSpec] = useState<AuthentikDeploymentSpec>(
        existing?.spec ?? (source ? duplicateSpec(source) : emptySpec()),
    );
    const [extraValuesYAML, setExtraValuesYAML] = useState("");
    const [error, setError] = useState<string>();
    const [saving, setSaving] = useState(false);
    // Once the user edits the namespace directly, the name stops driving it.
    const [namespaceEdited, setNamespaceEdited] = useState(Boolean(existing));

    const patch = (changes: Partial<AuthentikDeploymentSpec>) =>
        setSpec((current) => ({ ...current, ...changes }));

    // Extra values live in a separate ConfigMap: fetched once for an edit or
    // a duplicate.
    const valuesFrom = existing ?? source;
    useEffect(() => {
        if (!valuesFrom) {
            return;
        }
        let cancelled = false;
        api.getExtraValues(valuesFrom.metadata.name)
            .then((res) => !cancelled && setExtraValuesYAML(res.valuesYAML))
            // Not fatal - the field starts blank and a real problem surfaces on save.
            .catch(() => undefined);
        return () => {
            cancelled = true;
        };
    }, [valuesFrom]);

    async function submit(event: FormEvent) {
        event.preventDefault();
        setSaving(true);
        setError(undefined);
        try {
            const savedName = existing?.metadata.name ?? name;
            if (existing) {
                await api.updateDeployment(savedName, { spec });
                toast.success(`Saved ${savedName}`);
            } else {
                await api.createDeployment({ metadata: { name }, spec });
                toast.success(`Created ${savedName}`);
            }
            await api.setExtraValues(savedName, extraValuesYAML);
            navigate(`/deployments/${savedName}`);
        } catch (err) {
            setError(errorMessage(err));
            setSaving(false);
        }
    }

    return (
        <Page
            title={existing ? `Edit ${existing.metadata.name}` : "New deployment"}
            subtitle={source && `Duplicating ${source.metadata.name}`}
        >
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
                                disabled={Boolean(existing)}
                                onChange={(e) => {
                                    setName(e.target.value);
                                    if (!namespaceEdited) {
                                        patch({ namespace: slugify(e.target.value) });
                                    }
                                }}
                            />
                        </Field>
                        <Field label="Target name" help="Defaults to the name">
                            <input
                                value={spec.targetName ?? ""}
                                placeholder={name}
                                onChange={(e) => patch({ targetName: e.target.value || undefined })}
                            />
                        </Field>
                        <Field label="Namespace on the target cluster">
                            <input
                                required
                                value={spec.namespace}
                                onChange={(e) => {
                                    setNamespaceEdited(true);
                                    patch({ namespace: e.target.value });
                                }}
                            />
                        </Field>
                        <Field label="Cluster">
                            <ClusterSelect
                                value={spec.clusterRef?.name}
                                onChange={(cluster) => patch({ clusterRef: { name: cluster } })}
                            />
                        </Field>
                    </div>
                </fieldset>

                <DeploymentSpecFields
                    spec={spec}
                    patch={patch}
                    extraValuesYAML={extraValuesYAML}
                    onExtraValuesChange={setExtraValuesYAML}
                />

                {existing && <GeneratedHelmValues name={existing.metadata.name} />}

                <div className="form-actions">
                    <button type="submit" className="primary" disabled={saving}>
                        {saving ? "Saving…" : existing ? "Save changes" : "Create deployment"}
                    </button>
                    <button type="button" onClick={() => navigate(-1)}>
                        Cancel
                    </button>
                </div>
            </form>
        </Page>
    );
}

/**
 * GeneratedHelmValues shows the chart values the operator last applied to the
 * HelmRelease - the form plus extra values, as authentik actually gets them.
 */
function GeneratedHelmValues({ name }: { name: string }) {
    const values = useResource(() => api.getHelmValues(name), [name], 0);
    return (
        <details>
            <summary>Generated Helm values (as last applied)</summary>
            <Alert>{values.error}</Alert>
            <pre className="yaml">{values.data?.valuesYAML || "Not generated yet."}</pre>
        </details>
    );
}

/**
 * ClusterSelect picks the workload cluster a deployment installs onto -
 * never the management cluster, which the CRD rejects.
 */
export function ClusterSelect({
    value,
    onChange,
}: {
    value?: string;
    onChange: (name: string) => void;
}) {
    const clusters = useResource(() => api.clusters(), [], 0);
    return (
        <select required value={value ?? ""} onChange={(e) => onChange(e.target.value)}>
            <option value="" disabled>
                Select a workload cluster
            </option>
            {(clusters.data ?? [])
                .filter((c) => !c.management)
                .map((c) => (
                    <option key={c.name} value={c.name}>
                        {c.name}
                    </option>
                ))}
        </select>
    );
}

/**
 * DeploymentSpecFields is every fieldset of the deployment form after its
 * identity: workloads, observability, placement, Postgres and extra Helm
 * values. Shared with the PR comparison flow, which picks the image itself
 * (showImage=false) and applies one spec to both of its deployments.
 */
export function DeploymentSpecFields({
    spec,
    patch,
    extraValuesYAML,
    onExtraValuesChange,
    showImage = true,
}: {
    spec: AuthentikDeploymentSpec;
    patch: (changes: Partial<AuthentikDeploymentSpec>) => void;
    extraValuesYAML: string;
    onExtraValuesChange: (value: string) => void;
    showImage?: boolean;
}) {
    const authentik = spec.authentik ?? {};
    const errorReporting = authentik.errorReporting ?? {
        enabled: true,
    };

    return (
        <>
            <fieldset>
                <legend>Workloads</legend>
                <div className="fields">
                    <Field label="Server replicas">
                        <input
                            type="number"
                            min={1}
                            value={spec.server.replicas}
                            onChange={(e) =>
                                patch({
                                    server: { ...spec.server, replicas: Number(e.target.value) },
                                })
                            }
                        />
                    </Field>
                    <Field label="Worker replicas">
                        <input
                            type="number"
                            min={1}
                            value={spec.worker.replicas}
                            onChange={(e) =>
                                patch({
                                    worker: { ...spec.worker, replicas: Number(e.target.value) },
                                })
                            }
                        />
                    </Field>
                    {showImage && (
                        <>
                            <Field label="authentik image">
                                <input
                                    value={authentik.image ?? ""}
                                    placeholder="ghcr.io/goauthentik/server"
                                    onChange={(e) =>
                                        patch({
                                            authentik: {
                                                ...authentik,
                                                image: e.target.value || undefined,
                                            },
                                        })
                                    }
                                />
                            </Field>
                            <Field label="authentik image tag" help="Defaults to the chart's own">
                                <input
                                    value={authentik.tag ?? ""}
                                    placeholder="2026.6.0"
                                    onChange={(e) =>
                                        patch({
                                            authentik: {
                                                ...authentik,
                                                tag: e.target.value || undefined,
                                            },
                                        })
                                    }
                                />
                            </Field>
                        </>
                    )}
                    <Field label="Chart version" help="Defaults to latest">
                        <input
                            value={spec.chart?.version ?? ""}
                            onChange={(e) =>
                                patch({
                                    chart: { ...spec.chart, version: e.target.value || undefined },
                                })
                            }
                        />
                    </Field>
                </div>
            </fieldset>

            <fieldset>
                <legend>Observability</legend>
                <div className="fields">
                    <Field label="Pyroscope host" help="Defaults to this cluster's own Pyroscope">
                        <input
                            value={authentik.pyroscopeHost ?? ""}
                            placeholder="http://pyroscope.bench-system.svc.cluster.local:4040"
                            onChange={(e) =>
                                patch({
                                    authentik: {
                                        ...authentik,
                                        pyroscopeHost: e.target.value || undefined,
                                    },
                                })
                            }
                        />
                    </Field>
                    <label className="check">
                        <input
                            type="checkbox"
                            checked={errorReporting.enabled ?? false}
                            onChange={(e) =>
                                patch({
                                    authentik: {
                                        ...authentik,
                                        errorReporting: {
                                            ...errorReporting,
                                            enabled: e.target.checked,
                                        },
                                    },
                                })
                            }
                        />
                        Send traces (error reporting / OTLP)
                    </label>
                    {errorReporting.enabled && (
                        <Field label="Tracing endpoint" help="Defaults to this cluster's own Tempo">
                            <input
                                value={errorReporting.tracingEndpoint ?? ""}
                                placeholder="http://tempo.bench-system.svc.cluster.local:4318"
                                onChange={(e) =>
                                    patch({
                                        authentik: {
                                            ...authentik,
                                            errorReporting: {
                                                ...errorReporting,
                                                tracingEndpoint: e.target.value || undefined,
                                            },
                                        },
                                    })
                                }
                            />
                        </Field>
                    )}
                </div>
            </fieldset>

            <fieldset>
                <legend>Placement</legend>
                <div className="fields">
                    <Field label="Mode">
                        <select
                            value={spec.placement?.mode ?? "Dedicated"}
                            onChange={(e) =>
                                patch({
                                    placement: {
                                        ...spec.placement,
                                        mode: e.target.value as PlacementMode,
                                    },
                                })
                            }
                        >
                            <option value="Default">Default - pods may share nodes</option>
                            <option value="Dedicated">Dedicated - one pod per node</option>
                            <option value="Packed">Packed - all pods on the same node</option>
                        </select>
                    </Field>
                    {spec.placement?.mode === "Dedicated" && (
                        <Field label="Anti-affinity topology key">
                            <input
                                value={spec.placement?.topologyKey ?? ""}
                                placeholder="kubernetes.io/hostname"
                                onChange={(e) =>
                                    patch({
                                        placement: {
                                            ...spec.placement,
                                            topologyKey: e.target.value,
                                        },
                                    })
                                }
                            />
                        </Field>
                    )}
                    <Field label="Node selector" help="Comma-separated key=value">
                        <input
                            defaultValue={Object.entries(spec.placement?.nodeSelector ?? {})
                                .map(([k, v]) => `${k}=${v}`)
                                .join(",")}
                            placeholder="disktype=ssd"
                            onChange={(e) => {
                                const nodeSelector: Record<string, string> = {};
                                for (const pair of e.target.value.split(",")) {
                                    const [k, v] = pair.split("=").map((s) => s.trim());
                                    if (k) {
                                        nodeSelector[k] = v ?? "";
                                    }
                                }
                                patch({ placement: { ...spec.placement, nodeSelector } });
                            }}
                        />
                    </Field>
                </div>
                <div className="muted">
                    Tolerations aren&apos;t a form field (a structured list) - set them with{" "}
                    <code>kubectl edit</code> in the shell.
                </div>
            </fieldset>

            <fieldset>
                <legend>Postgres</legend>
                <div className="fields">
                    <Field label="Mode">
                        <select
                            value={spec.postgres.mode}
                            onChange={(e) => {
                                const mode = e.target.value as PostgresMode;
                                patch({
                                    postgres: {
                                        mode,
                                        embedded:
                                            mode === "Embedded"
                                                ? (spec.postgres.embedded ?? {})
                                                : undefined,
                                        cnpg:
                                            mode === "CNPG"
                                                ? (spec.postgres.cnpg ?? { instances: 3 })
                                                : undefined,
                                        external:
                                            mode === "External"
                                                ? (spec.postgres.external ?? {
                                                      secretRef: { name: "" },
                                                      sslMode: "require",
                                                  })
                                                : undefined,
                                    },
                                });
                            }}
                        >
                            <option value="Embedded">
                                Embedded - the chart&apos;s bundled subchart
                            </option>
                            <option value="CNPG">CloudNativePG - HA cluster</option>
                            <option value="External">External - an instance you manage</option>
                        </select>
                    </Field>
                    {spec.postgres.mode === "Embedded" && (
                        <Field label="Storage size">
                            <input
                                value={spec.postgres.embedded?.storageSize ?? ""}
                                placeholder="8Gi"
                                onChange={(e) =>
                                    patch({
                                        postgres: {
                                            ...spec.postgres,
                                            embedded: {
                                                ...spec.postgres.embedded,
                                                storageSize: e.target.value || undefined,
                                            },
                                        },
                                    })
                                }
                            />
                        </Field>
                    )}
                    {spec.postgres.mode === "Embedded" && (
                        <Field
                            label="Max storage size"
                            help="Grows the volume by half once it is 80% full, up to this size."
                        >
                            <input
                                value={spec.postgres.embedded?.maxStorageSize ?? ""}
                                placeholder="12Gi"
                                onChange={(e) =>
                                    patch({
                                        postgres: {
                                            ...spec.postgres,
                                            embedded: {
                                                ...spec.postgres.embedded,
                                                maxStorageSize: e.target.value || undefined,
                                            },
                                        },
                                    })
                                }
                            />
                        </Field>
                    )}
                    {spec.postgres.mode === "CNPG" && (
                        <>
                            <Field label="Instances">
                                <input
                                    type="number"
                                    min={1}
                                    value={spec.postgres.cnpg?.instances ?? 3}
                                    onChange={(e) =>
                                        patch({
                                            postgres: {
                                                ...spec.postgres,
                                                cnpg: {
                                                    ...spec.postgres.cnpg,
                                                    instances: Number(e.target.value),
                                                },
                                            },
                                        })
                                    }
                                />
                            </Field>
                            <Field label="Storage size">
                                <input
                                    value={spec.postgres.cnpg?.storageSize ?? ""}
                                    placeholder="10Gi"
                                    onChange={(e) =>
                                        patch({
                                            postgres: {
                                                ...spec.postgres,
                                                cnpg: {
                                                    ...spec.postgres.cnpg,
                                                    storageSize: e.target.value || undefined,
                                                },
                                            },
                                        })
                                    }
                                />
                            </Field>
                            <Field
                                label="Max storage size"
                                help="Grows the volume by half once it is 80% full, up to this size."
                            >
                                <input
                                    value={spec.postgres.cnpg?.maxStorageSize ?? ""}
                                    placeholder="12Gi"
                                    onChange={(e) =>
                                        patch({
                                            postgres: {
                                                ...spec.postgres,
                                                cnpg: {
                                                    ...spec.postgres.cnpg,
                                                    maxStorageSize: e.target.value || undefined,
                                                },
                                            },
                                        })
                                    }
                                />
                            </Field>
                        </>
                    )}
                    {spec.postgres.mode === "External" && (
                        <>
                            <Field label="Connection secret name">
                                <input
                                    required
                                    value={spec.postgres.external?.secretRef.name ?? ""}
                                    onChange={(e) =>
                                        patch({
                                            postgres: {
                                                ...spec.postgres,
                                                external: {
                                                    ...spec.postgres.external,
                                                    secretRef: { name: e.target.value },
                                                },
                                            },
                                        })
                                    }
                                />
                            </Field>
                            <Field label="SSL mode">
                                <select
                                    value={spec.postgres.external?.sslMode ?? "require"}
                                    onChange={(e) =>
                                        patch({
                                            postgres: {
                                                ...spec.postgres,
                                                external: {
                                                    secretRef: spec.postgres.external
                                                        ?.secretRef ?? { name: "" },
                                                    sslMode: e.target.value,
                                                },
                                            },
                                        })
                                    }
                                >
                                    {["disable", "require", "verify-ca", "verify-full"].map((m) => (
                                        <option key={m} value={m}>
                                            {m}
                                        </option>
                                    ))}
                                </select>
                            </Field>
                        </>
                    )}
                </div>
            </fieldset>

            <fieldset>
                <legend>Advanced: extra Helm values</legend>
                <p className="muted">
                    Raw values.yaml for the authentik chart, deep-merged over everything above last
                    - the escape hatch for settings this form doesn&apos;t expose. For example:{" "}
                    <code>authentik.geoip.enabled: true</code>
                </p>
                <CodeEditor
                    language="yaml"
                    value={extraValuesYAML}
                    onChange={onExtraValuesChange}
                />
            </fieldset>
        </>
    );
}

export function DeploymentCreatePage() {
    const [params] = useSearchParams();
    const from = params.get("from");
    if (!from) {
        return <DeploymentForm />;
    }
    return <DeploymentDuplicate from={from} />;
}

function DeploymentDuplicate({ from }: { from: string }) {
    const source = useResource(() => api.deployment(from), [from], 0);
    if (source.error) {
        return (
            <Page title="New deployment">
                <Alert>{source.error}</Alert>
            </Page>
        );
    }
    return source.data ? <DeploymentForm key={from} source={source.data} /> : null;
}

export function DeploymentEditPage() {
    const { name = "" } = useParams();
    const existing = useResource(() => api.deployment(name), [name], 0);
    if (existing.error) {
        return (
            <Page title="Edit deployment">
                <Alert>{existing.error}</Alert>
            </Page>
        );
    }
    return existing.data ? <DeploymentForm key={name} existing={existing.data} /> : null;
}

const DEFAULT_METRICS_RANGE = "15m";

/**
 * The deployment's view: its status, shell hand-offs for its pods on
 * whichever cluster it runs on, and metrics for every TestCase pointed at it.
 */
export function DeploymentViewPage() {
    const { name = "" } = useParams();
    const deployment = useResource(
        () => api.deployment(name),
        [name],
        10000,
        "AuthentikDeployment",
    );
    const testCases = useResource(() => api.testCases(), [], 15000, "TestCase");
    const relevant = (testCases.data ?? []).filter((tc) =>
        testCaseDeployments(tc.spec).some((ref) => ref.name === name),
    );
    const testids = relevant.map((tc) => tc.metadata.name);
    const burnStart = earliestBurnStart(relevant, name);
    const [params, setParams] = useSearchParams();
    const range = parseTimeRange(params.get("range"), DEFAULT_METRICS_RANGE, burnStart);
    const setRange = (value: string) => {
        const next = new URLSearchParams(params);
        if (value === DEFAULT_METRICS_RANGE) {
            next.delete("range");
        } else {
            next.set("range", value);
        }
        setParams(next, { replace: true });
    };
    const [kicking, setKicking] = useState(false);

    const d = deployment.data;
    const resetting = !!d?.metadata.annotations?.["bench.goauthentik.io/reset-requested-at"];
    const cluster = d?.spec.clusterRef?.name;
    const ns = d?.spec.namespace;

    const kick = async () => {
        setKicking(true);
        try {
            await api.kickDeployment(name);
            deployment.reload();
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
                d &&
                `AuthentikDeployment on ${cluster ?? "the management cluster"}, namespace ${ns}`
            }
            actions={
                <>
                    <button
                        className="button"
                        disabled={kicking}
                        title="Nudge the operator into reconciling this deployment right now, instead of waiting for its next naturally-triggered update."
                        onClick={() => void kick()}
                    >
                        {kicking ? "Kicking…" : "Kick"}
                    </button>
                    {d && d.spec.postgres.mode !== "External" && (
                        <button
                            className="button danger"
                            disabled={resetting}
                            title="Uninstall authentik, delete its database volumes and reinstall from an empty database, re-applying the fixtures and test-client blueprints."
                            onClick={() =>
                                void confirmAndRun(
                                    {
                                        title: "Reset deployment",
                                        message: `Reset deployment "${name}"? This uninstalls authentik, permanently deletes its database and reinstalls it from scratch with the seed blueprints.`,
                                        confirmLabel: "Reset",
                                        danger: true,
                                    },
                                    () => api.resetDeployment(name),
                                    deployment.reload,
                                    `Resetting ${name}`,
                                )
                            }
                        >
                            {resetting ? "Resetting…" : "Reset"}
                        </button>
                    )}
                    <Link className="button" to={`/deployments/${name}/edit`}>
                        Edit
                    </Link>
                    <Link className="button" to={duplicateLink(name)}>
                        Duplicate
                    </Link>
                </>
            }
        >
            <Alert>{deployment.error}</Alert>
            {d && (
                <>
                    <div className="panel">
                        <div className="row" style={{ marginBottom: 8 }}>
                            <StatusBadge
                                kind="authentikdeployment"
                                name={name}
                                namespace={d.metadata.namespace}
                                phase={d.status?.phase}
                                conditions={d.status?.conditions}
                            />
                            {d.status?.serviceEndpoint && (
                                <span className="muted">
                                    Service: <code>{d.status.serviceEndpoint}</code>
                                </span>
                            )}
                        </div>
                        <ConditionList conditions={d.status?.conditions} />
                        <ActivityLog path="deployments" name={name} />
                        <Rollout deployments={[d]} />
                    </div>

                    <h2>On the cluster</h2>
                    <div className="row">
                        <Link
                            className="button"
                            to={shellLinkForCluster(
                                cluster,
                                `kubectl get pods -n ${ns} -o wide`,
                                ns,
                            )}
                        >
                            Pods
                        </Link>
                        <Link
                            className="button"
                            to={shellLinkForCluster(
                                cluster,
                                `kubectl logs -n ${ns} deploy/${name}-authentik-server -f --tail=200`,
                                ns,
                            )}
                        >
                            Server logs
                        </Link>
                        <Link
                            className="button"
                            to={shellLinkForCluster(
                                cluster,
                                `kubectl logs -n ${ns} deploy/${name}-authentik-worker -f --tail=200`,
                                ns,
                            )}
                        >
                            Worker logs
                        </Link>
                        <Link
                            className="button"
                            to={shellLinkForCluster(
                                cluster,
                                `kubectl get events -n ${ns} --sort-by=.lastTimestamp`,
                                ns,
                            )}
                        >
                            Events
                        </Link>
                        {d.spec.postgres.mode === "Embedded" && (
                            <Link
                                className="button"
                                to={shellLinkForCluster(
                                    cluster,
                                    `kubectl describe statefulset -n ${ns} ${name}-postgresql`,
                                    ns,
                                )}
                            >
                                Postgres
                            </Link>
                        )}
                        <Link
                            className="button"
                            title="Open the shell with a psql session on this deployment's database ready to run."
                            to={shellLinkForCluster(
                                cluster,
                                psqlCommand(name, ns ?? "", d.spec),
                                ns,
                            )}
                        >
                            psql
                        </Link>
                    </div>

                    <h2>Node placement</h2>
                    <NodePlacement deployment={name} />
                </>
            )}

            <div className="section-head">
                <h2>Metrics</h2>
                <TimeRangeSelect
                    value={range}
                    sinceStartLabel={burnStart !== undefined ? "Since burn start" : undefined}
                    onChange={setRange}
                />
            </div>
            {d && (
                <>
                    <h3>Worker</h3>
                    <WorkerCharts
                        deployment={name}
                        rangeSeconds={timeRangeSeconds(range, burnStart)}
                    />
                </>
            )}
            {d && d.spec.postgres.mode !== "External" && (
                <>
                    <h3>Postgres</h3>
                    <PostgresCharts
                        deployment={name}
                        rangeSeconds={timeRangeSeconds(range, burnStart)}
                    />
                </>
            )}
            {d && <h3>Tests</h3>}
            {testids.length === 0 ? (
                <Empty>No test cases reference this deployment yet.</Empty>
            ) : (
                // Test cases can drive other deployments too (e.g. a PR comparison):
                // chart only this deployment's own target.
                d && (
                    <MetricsGrid
                        testids={testids}
                        targets={[targetNameOf(d)]}
                        rangeSeconds={timeRangeSeconds(range, burnStart)}
                    />
                )
            )}

            <ObjectYaml object={d} />
        </Page>
    );
}
