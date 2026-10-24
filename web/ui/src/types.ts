// These mirror bench.goauthentik.io/v1alpha1 (api/v1alpha1/*_types.go)
// deliberately partially: only the fields this UI renders or offers a guided
// form for. cmd/apiserver passes CRs through untouched and the CRD schema
// stays the one authority on what's valid - every form edits a copy of the
// object it fetched, so a field missing here is preserved on save rather
// than dropped, and every view page also renders the full object as YAML
// (components/ObjectYaml.tsx) so nothing the CRD has is ever hidden.

export interface Condition {
    type: string;
    status: "True" | "False" | "Unknown";
    reason?: string;
    message?: string;
    lastTransitionTime?: string;
}

// Alert is one firing/pending/silenced alert from a workload cluster's own
// Alertmanager (see internal/api/alerts.go - there is no central instance).
export interface Alert {
    cluster: string;
    labels: Record<string, string>;
    annotations: Record<string, string>;
    state: string;
    startsAt: string;
    endsAt?: string;
    generatorURL?: string;
}

// PodPlacement is one deployment pod and the node it was scheduled onto
// (internal/api/placement.go, read from each cluster's kube-state-metrics).
export interface PodPlacement {
    deployment: string;
    cluster: string;
    node: string;
    pod: string;
}

// ObjectEvent is one Kubernetes Event about a bench object
// (internal/api/objectevents.go).
export interface ObjectEvent {
    type: "Normal" | "Warning" | string;
    reason: string;
    message: string;
    count: number;
    firstSeen?: string;
    lastSeen?: string;
    source?: string;
}

export interface ObjectMeta {
    name: string;
    namespace?: string;
    generateName?: string;
    creationTimestamp?: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
}

export type PostgresMode = "Embedded" | "CNPG" | "External";
export type PlacementMode = "Default" | "Dedicated" | "Packed";
export type TestProfile = "Quick" | "Burn";

export interface ResourceRequirements {
    requests?: Record<string, string>;
    limits?: Record<string, string>;
}

export interface WorkloadSpec {
    replicas: number;
    resources?: ResourceRequirements;
}

export interface AuthentikDeploymentSpec {
    clusterRef?: { name: string };
    targetName?: string;
    namespace: string;
    chart?: { repo?: string; version?: string };
    server: WorkloadSpec;
    worker: WorkloadSpec;
    placement?: {
        mode?: PlacementMode;
        nodeSelector?: Record<string, string>;
        topologyKey?: string;
    };
    postgres: {
        mode: PostgresMode;
        embedded?: { storageSize?: string; storageClassName?: string; maxStorageSize?: string };
        cnpg?: {
            instances?: number;
            storageSize?: string;
            storageClassName?: string;
            maxStorageSize?: string;
        };
        external?: { secretRef: { name: string }; sslMode?: string };
    };
    authentik?: {
        image?: string;
        tag?: string;
        logLevel?: string;
        errorReporting?: { enabled?: boolean; tracingEndpoint?: string; sampleRate?: string };
        pyroscopeHost?: string;
        extraValuesRef?: { name: string };
    };
}

export interface AuthentikDeploymentStatus {
    conditions?: Condition[];
    phase?: string;
    observedGeneration?: number;
    serviceEndpoint?: string;
    helmReleaseRef?: { name: string };
    cnpgClusterRef?: { name: string };
    postgresStorage?: {
        size?: string;
        usedBytes?: number;
        capacityBytes?: number;
        lastChecked?: string;
    };
}

export interface AuthentikDeployment {
    metadata: ObjectMeta;
    spec: AuthentikDeploymentSpec;
    status?: AuthentikDeploymentStatus;
}

export interface TestCaseSpec {
    // deploymentRef is deprecated (kept for objects created before
    // deploymentRefs existed) - prefer deploymentRefs, which supports driving
    // the same test against multiple deployments at once. Both optional at
    // this layer; the CRD/controller require at least one to resolve.
    deploymentRef?: { name: string };
    deploymentRefs?: { name: string }[];
    // baselineRef is the deployment the others are compared against; unset or
    // stale means the first one (testCaseBaseline).
    baselineRef?: { name: string };
    runnerClusterRef?: { name: string };
    template?: string;
    inline?: string;
    // scriptConfigMapRef is a power-user escape hatch (bring-your-own
    // ConfigMap, overrides inline/template) - deliberately not exposed as a
    // TestCaseForm input, but still a real spec field a TestCase can carry.
    scriptConfigMapRef?: { name: string };
    profile: TestProfile;
    vus: number;
    iterations?: number;
    flow?: string;
    quickDuration?: string;
    burnDuration?: string;
    traceSampleRate?: string;
    seed?: number;
    extraTags?: Record<string, string>;
    // Stopped but kept: no burn workload, no new runs.
    archived?: boolean;
}

export interface TestCaseDeploymentStatus {
    name: string;
    ready: boolean;
    burnWorkloadRef?: { name: string };
    burningSince?: string;
    // Set once archiving stopped this target's burn; burningSince is then kept.
    burnStoppedAt?: string;
}

export interface TestCase {
    metadata: ObjectMeta;
    spec: TestCaseSpec;
    status?: {
        conditions?: Condition[];
        phase?: string;
        observedGeneration?: number;
        deployments?: TestCaseDeploymentStatus[];
    };
}

/**
 * testCaseDeployments mirrors TestCase.Deployments() (api/v1alpha1/testcase_types.go):
 * prefers spec.deploymentRefs, falls back to the deprecated singular
 * spec.deploymentRef - the one place the frontend should read "which
 * deployment(s) does this TestCase drive" rather than reading either field
 * directly.
 */
export function testCaseDeployments(spec: TestCaseSpec): { name: string }[] {
    if (spec.deploymentRefs && spec.deploymentRefs.length > 0) {
        return spec.deploymentRefs;
    }
    if (spec.deploymentRef?.name) {
        return [spec.deploymentRef];
    }
    return [];
}

/**
 * testCaseBaseline is the deployment a TestCase's other targets are
 * compared against: spec.baselineRef while it names one of its deployments,
 * otherwise the first.
 */
export function testCaseBaseline(spec: TestCaseSpec): string | undefined {
    const deployments = testCaseDeployments(spec);
    return deployments.find((d) => d.name === spec.baselineRef?.name)?.name ?? deployments[0]?.name;
}

export interface TestCaseTemplateSpec {
    description?: string;
    script: string;
    dependencies?: TestCaseDependency[];
}

/** Extra services a TestCaseTemplate needs provisioned next to its target. */
export const testCaseDependencies = {
    "oidc-client": "OIDC client",
    "saml-client": "SAML client",
    "oidc-idp": "OIDC IdP",
    "saml-idp": "SAML IdP",
    "oauth-token": "OAuth token clients",
} as const;

export type TestCaseDependency = keyof typeof testCaseDependencies;

export interface TestCaseTemplate {
    metadata: ObjectMeta;
    spec: TestCaseTemplateSpec;
    status?: {
        conditions?: Condition[];
        phase?: string;
        observedGeneration?: number;
    };
}

export type TestRunPhase = "Pending" | "Running" | "Succeeded" | "Failed";

export interface TestRun {
    metadata: ObjectMeta;
    spec: {
        testCaseRef: { name: string };
        // deploymentRef pins this run to one specific entry of its TestCase's
        // own deployments (see testCaseDeployments) - unset on every TestRun
        // created before multi-deployment TestCases existed, in which case it
        // ran (and still is read as having run) against that TestCase's own
        // first/only deployment.
        deploymentRef?: { name: string };
        overrides?: { vus?: number; iterations?: number; flow?: string };
    };
    status?: {
        phase?: TestRunPhase;
        startTime?: string;
        completionTime?: string;
        jobRef?: { name: string };
        conditions?: Condition[];
        // resolvedSpec is an audit snapshot of the exact TestCaseSpec +
        // overrides this run actually used (internal/controller/testrun_controller.go
        // sets it) - what a historical run's own "what ran" record is.
        resolvedSpec?: TestCaseSpec;
    };
}

/**
 * testRunDeployment is the deployment a TestRun ran against: its pinned
 * spec.deploymentRef, else (a run created before TestCases could drive more
 * than one deployment) the first deployment of its own resolvedSpec
 * snapshot - the same fallback TestRunReconciler uses.
 */
export function testRunDeployment(run: TestRun): string | undefined {
    const resolved = run.status?.resolvedSpec;
    return (
        run.spec.deploymentRef?.name ??
        (resolved ? testCaseDeployments(resolved)[0]?.name : undefined)
    );
}

export interface Cluster {
    name: string;
    management: boolean;
    phase?: string;
    controlPlaneReady: boolean;
    infrastructureReady: boolean;
}

export type InfrastructureProvider = "Incus";

export interface MachineSpec {
    replicas: number;
}

export interface IncusMachineSpec {
    flavor?: string;
}

export interface IncusProviderSpec {
    lxcSecretRef: { name: string };
    image?: string;
    profiles?: string[];
    controlPlane?: IncusMachineSpec;
}

export interface AutoscalingSpec {
    minReplicas?: number;
    maxReplicas: number;
}

// WorkerPoolRole reserves a pool's nodes for one of this project's own
// workloads - WorkloadClusterReconciler taints/labels a non-Generic pool's
// nodes (bench.goauthentik.io/pool=<role>), and every workload rendered for
// that role gets a matching nodeSelector/toleration. Generic (the zero
// value / unset) means no taint, no label - schedules anywhere, same as a
// pool created before this field existed.
export type WorkerPoolRole = "Generic" | "Authentik" | "K6" | "Monitoring";

export interface WorkerPoolSpec {
    name: string;
    role?: WorkerPoolRole;
    replicas: number;
    incus?: IncusMachineSpec;
    // autoscaling hands this pool's replica count over to cluster-autoscaler
    // (WorkloadClusterReconciler's ensureClusterAutoscaler) - once set,
    // replicas above only sizes the pool at creation time.
    autoscaling?: AutoscalingSpec;
}

export interface WorkloadClusterSpec {
    kubernetesVersion: string;
    provider: InfrastructureProvider;
    controlPlane: MachineSpec;
    workerPools: WorkerPoolSpec[];
    incus?: IncusProviderSpec;
}

export type AddonState = "Ready" | "Progressing" | "Waiting" | "Failed";

// One add-on WorkloadClusterReconciler installs onto the cluster
// (api/v1alpha1.AddonStatus).
export interface AddonStatus {
    name: string;
    state: AddonState;
    message?: string;
    lastTransitionTime?: string;
}

export interface WorkloadClusterStatus {
    conditions?: Condition[];
    addons?: AddonStatus[];
    phase?: string;
    observedGeneration?: number;
    controlPlaneReady?: boolean;
    infrastructureReady?: boolean;
    kubeconfigSecretRef?: { name: string };
    // prometheusURL/grafanaURL are reachable *from the management cluster*
    // (a node's InternalIP + a fixed NodePort - see
    // render.PrometheusNodePort/GrafanaNodePort), populated once
    // ensurePrometheus/ensureGrafana succeed.
    prometheusURL?: string;
    grafanaURL?: string;
}

export interface WorkloadCluster {
    metadata: ObjectMeta;
    spec: WorkloadClusterSpec;
    status?: WorkloadClusterStatus;
}

export interface CompareMetric {
    key: string;
    label: string;
    unit: string;
    better: "higher" | "lower";
}

export interface CompareResponse {
    window: string;
    metrics: CompareMetric[];
    // One row per (testid, target) pair - a testid driving more than one
    // AuthentikDeployment produces more than one row, target always populated.
    results: { testid: string; target: string; values: Record<string, number> }[];
    // Keyed by testid: a testid whose Prometheus couldn't be queried, while
    // the others in the same request still could.
    errors?: Record<string, string>;
}

export interface RangePoint {
    time: number;
    value: number;
}

export interface RangeSeries {
    testid: string;
    target: string;
    points: RangePoint[];
}

export interface RangeResponse {
    metric: CompareMetric;
    series: RangeSeries[];
    // Keyed by testid, same as CompareResponse.errors.
    errors?: Record<string, string>;
}

// TraceBreakdown mirrors internal/api/traces.go: a sample of each target's
// most recent traces from its workload cluster's Tempo. Durations are seconds.
export interface TraceBreakdown {
    testid: string;
    window: string;
    generatedAt: string;
    targets: TraceTarget[];
}

export interface TraceTarget {
    target: string;
    traces: number;
    requests: number;
    requestP50: number;
    requestP95: number;
    queriesPerRequest: number;
    dbTimePerRequest: number;
    dbShare: number;
    stages: { name: string; count: number; p50: number; p95: number }[];
    topQueries: {
        statement: string;
        count: number;
        perRequest: number;
        total: number;
        mean: number;
    }[];
    error?: string;
}

// ProfileBreakdown mirrors internal/api/profiles.go: each target's merged CPU
// profile from its workload cluster's Pyroscope. Shares are 0..1 of the
// target's profiled CPU time.
export interface ProfileBreakdown {
    testid: string;
    window: string;
    component: string;
    generatedAt: string;
    targets: ProfileTarget[];
}

export interface ProfileTarget {
    target: string;
    cpuSeconds: number;
    cores: number;
    categories: { name: string; share: number }[];
    functions: ProfileFunction[];
    error?: string;
}

export interface ProfileFunction {
    name: string;
    file: string;
    category: string;
    self: number;
    total: number;
}

// LogBreakdown mirrors internal/api/logs.go: each target's log volume from
// its workload cluster's Loki, plus its most frequent warning-or-worse
// events.
export interface LogBreakdown {
    testid: string;
    window: string;
    generatedAt: string;
    targets: LogTarget[];
}

export interface LogTarget {
    target: string;
    lines: number;
    linesPerSecond: number;
    components: {
        component: string;
        lines: number;
        // Lines per level; "" counts lines that aren't JSON or have no level.
        levels: Record<string, number>;
    }[];
    topEvents: LogEvent[];
    error?: string;
}

export interface LogEvent {
    level: string;
    logger: string;
    event: string;
    count: number;
}

/** readyCondition is the one condition every list view renders. */
export function readyCondition(conditions?: Condition[]): Condition | undefined {
    return conditions?.find((c) => c.type === "Ready");
}

// -- Infrastructure (internal/api/infrastructure.go, incus.go)

export interface InfrastructureProviderInfo {
    // clusterctl provider name, e.g. "incus".
    id: string;
    displayName: string;
    // The WorkloadCluster spec.provider value this provider backs; absent when
    // akbench can install it but not provision clusters with it yet.
    clusterProvider?: InfrastructureProvider;
    installedVersion?: string;
    // What the appliance would install; absent outside the appliance.
    availableVersion?: string;
    enabled: boolean;
}

export interface Infrastructure {
    // True inside the appliance, where providers are enabled from the UI.
    managed: boolean;
    providers: InfrastructureProviderInfo[];
    syncError?: string;
    syncedAt?: string;
}

export interface IncusCredential {
    name: string;
    server: string;
    project?: string;
    serverCert?: string;
    insecureSkipVerify: boolean;
    clientCert?: string;
    clientCertFingerprint?: string;
    hasClientKey: boolean;
    usedBy: string[];
    // False for a Secret created outside the UI that a cluster references.
    labeled: boolean;
}

export interface IncusCredentialInput {
    name?: string;
    server: string;
    project?: string;
    serverCert?: string;
    insecureSkipVerify: boolean;
    clientCert?: string;
    clientKey?: string;
    regenerateClientCert?: boolean;
}

export interface IncusProbe {
    reachable: boolean;
    trusted: boolean;
    serverVersion?: string;
    serverCert?: string;
    serverCertFingerprint?: string;
    message?: string;
}

// -- A workload cluster's nodes (internal/api/machines.go): its Cluster API
// Machines and per-pool sizes, read from the management cluster.

export interface ClusterPool {
    // A WorkerPoolSpec name, or "control-plane".
    name: string;
    // Replicas currently asked for - moves within an autoscaled pool's range.
    desired: number;
    current: number;
    ready: number;
}

export interface ClusterMachine {
    name: string;
    pool: string;
    // Empty until the machine's node has joined the cluster.
    nodeName?: string;
    phase?: string;
    ready: boolean;
    internalIP?: string;
    version?: string;
    created?: string;
}

export interface ClusterNodes {
    pools: ClusterPool[];
    machines: ClusterMachine[];
}

/** The charts /deployments/{name}/postgres-metrics serves. */
export type PostgresMetric = "cpu" | "memory" | "disk" | "io";

/** The charts /clusters/{name}/monitoring-metrics serves. */
export type MonitoringMetric = "cpu" | "memory" | "disk";

/** The charts /deployments/{name}/worker-metrics serves. */
export type WorkerMetric =
    | "tasks"
    | "duration"
    | "queue"
    | "failures"
    | "db_queries"
    | "db_duration";
