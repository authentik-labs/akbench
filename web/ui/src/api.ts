// Thin fetch wrapper over cmd/apiserver's REST API (internal/api/server.go's
// route table). The app is served by cmd/apiserver itself, so every path is
// same-origin and the credential is a plain Authorization: Bearer header -
// either the static API token or an OIDC access token, see auth.tsx.
import {
    Alert,
    AuthentikDeployment,
    AuthentikDeploymentSpec,
    Cluster,
    ClusterNodes,
    CompareResponse,
    IncusCredential,
    IncusCredentialInput,
    IncusProbe,
    Infrastructure,
    PodPlacement,
    RangeResponse,
    TestCase,
    TestCaseSpec,
    TestCaseTemplate,
    ObjectEvent,
    TestCaseTemplateSpec,
    TestRun,
    LogBreakdown,
    ProfileBreakdown,
    TraceBreakdown,
    WorkloadCluster,
    WorkloadClusterSpec,
} from "./types";

// Must match internal/api/clusters.go's ManagementClusterName - also the
// merged kubeconfig's context name for the management cluster.
export const MANAGEMENT_CLUSTER = "management";

export class ApiError extends Error {
    readonly status: number;

    constructor(status: number, message: string) {
        super(message);
        this.status = status;
        this.name = "ApiError";
    }
}

type TokenSource = () => Promise<string | undefined>;

let tokenSource: TokenSource = async () => undefined;
let onUnauthorized: () => void = () => {};

/** configureAuth is called once by AuthProvider. */
export function configureAuth(source: TokenSource, unauthorized: () => void) {
    tokenSource = source;
    onUnauthorized = unauthorized;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
    const token = await tokenSource();
    const res = await fetch(path, {
        ...init,
        headers: {
            "Content-Type": "application/json",
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
            ...(init?.headers as Record<string, string> | undefined),
        },
    });
    if (res.status === 401) {
        onUnauthorized();
    }
    if (!res.ok) {
        let message = `${res.status} ${res.statusText}`;
        try {
            const body = (await res.json()) as { error?: string };
            if (body.error) {
                message = body.error;
            }
        } catch {
            // Not every failure has the API's own JSON error body (a proxy in
            // between, say) - the status line is the best there is then.
        }
        throw new ApiError(res.status, message);
    }
    if (res.status === 204) {
        return undefined as T;
    }
    return (await res.json()) as T;
}

interface List<T> {
    items: T[] | null;
}

interface ObjectInput<Spec> {
    metadata: { name?: string; generateName?: string; labels?: Record<string, string> };
    spec: Spec;
}

const json = (method: string, body: unknown): RequestInit => ({
    method,
    body: JSON.stringify(body),
});
const seg = encodeURIComponent;

/**
 * ResourcePath is a bench kind's /api/v1/<path> segment
 * (internal/api/resources.go's resourceKinds).
 */
export type ResourcePath =
    | "deployments"
    | "testcases"
    | "testcasetemplates"
    | "testruns"
    | "workloadclusters";

export interface AuthConfig {
    oidc?: { issuer: string; clientId: string; scopes: string[] };
}

export interface BuildInfo {
    version: string;
    commit?: string;
    dirty?: boolean;
    goVersion?: string;
}

export interface ShellContext {
    name: string;
    cluster: string;
}

export const api = {
    authConfig: () => request<AuthConfig>("/api/v1/auth/config"),
    // The running apiserver build (internal/api/version.go).
    version: () => request<BuildInfo>("/api/v1/version"),

    // -- Clusters: internal/api/clusters.go's convenience endpoints - the
    // management cluster plus every CAPI Cluster (for pickers);
    // register/deprovision create/delete the WorkloadCluster CRD.
    clusters: () => request<List<Cluster>>("/api/v1/clusters").then((r) => r.items ?? []),
    registerCluster: (obj: ObjectInput<WorkloadClusterSpec>) =>
        request<WorkloadCluster>("/api/v1/clusters", json("POST", obj)),
    deprovisionCluster: (name: string) =>
        request<void>(`/api/v1/clusters/${seg(name)}`, { method: "DELETE" }),
    clusterNodes: (name: string) => request<ClusterNodes>(`/api/v1/clusters/${seg(name)}/nodes`),
    workloadClusters: () =>
        request<List<WorkloadCluster>>("/api/v1/workloadclusters").then((r) => r.items ?? []),
    workloadCluster: (name: string) =>
        request<WorkloadCluster>(`/api/v1/workloadclusters/${seg(name)}`),
    updateWorkloadCluster: (name: string, obj: { spec: WorkloadClusterSpec }) =>
        request<WorkloadCluster>(`/api/v1/workloadclusters/${seg(name)}`, json("PUT", obj)),
    kickWorkloadCluster: (name: string) =>
        request<WorkloadCluster>(
            `/api/v1/workloadclusters/${seg(name)}/reconcile`,
            json("POST", {}),
        ),

    // -- Infrastructure: internal/api/infrastructure.go (providers) and
    // incus.go (Incus connections, i.e. the Secrets spec.incus.lxcSecretRef
    // names). The client key is write-only - never returned.
    infrastructure: () => request<Infrastructure>("/api/v1/infrastructure"),
    enableInfrastructureProvider: (id: string) =>
        request<Infrastructure>(
            `/api/v1/infrastructure/providers/${seg(id)}/enable`,
            json("POST", {}),
        ),
    incusCredentials: () =>
        request<List<IncusCredential>>("/api/v1/infrastructure/incus/credentials").then(
            (r) => r.items ?? [],
        ),
    incusCredential: (name: string) =>
        request<IncusCredential>(`/api/v1/infrastructure/incus/credentials/${seg(name)}`),
    createIncusCredential: (input: IncusCredentialInput) =>
        request<IncusCredential>("/api/v1/infrastructure/incus/credentials", json("POST", input)),
    updateIncusCredential: (name: string, input: IncusCredentialInput) =>
        request<IncusCredential>(
            `/api/v1/infrastructure/incus/credentials/${seg(name)}`,
            json("PUT", input),
        ),
    deleteIncusCredential: (name: string) =>
        request<void>(`/api/v1/infrastructure/incus/credentials/${seg(name)}`, {
            method: "DELETE",
        }),
    testIncusCredential: (name: string) =>
        request<IncusProbe>(
            `/api/v1/infrastructure/incus/credentials/${seg(name)}/test`,
            json("POST", {}),
        ),
    trustIncusCredential: (name: string, token: string) =>
        request<IncusProbe>(
            `/api/v1/infrastructure/incus/credentials/${seg(name)}/trust`,
            json("POST", { token }),
        ),

    // -- Deployments
    deployments: () =>
        request<List<AuthentikDeployment>>("/api/v1/deployments").then((r) => r.items ?? []),
    deployment: (name: string) => request<AuthentikDeployment>(`/api/v1/deployments/${seg(name)}`),
    createDeployment: (obj: ObjectInput<AuthentikDeploymentSpec>) =>
        request<AuthentikDeployment>("/api/v1/deployments", json("POST", obj)),
    updateDeployment: (name: string, obj: { spec: AuthentikDeploymentSpec }) =>
        request<AuthentikDeployment>(`/api/v1/deployments/${seg(name)}`, json("PUT", obj)),
    deleteDeployment: (name: string) =>
        request<void>(`/api/v1/deployments/${seg(name)}`, { method: "DELETE" }),
    kickDeployment: (name: string) =>
        request<AuthentikDeployment>(
            `/api/v1/deployments/${seg(name)}/reconcile`,
            json("POST", {}),
        ),
    resetDeployment: (name: string) =>
        request<AuthentikDeployment>(`/api/v1/deployments/${seg(name)}/reset`, json("POST", {})),
    getExtraValues: (name: string) =>
        request<{ valuesYAML: string }>(`/api/v1/deployments/${seg(name)}/extra-values`),
    setExtraValues: (name: string, valuesYAML: string) =>
        request<{ valuesYAML: string }>(
            `/api/v1/deployments/${seg(name)}/extra-values`,
            json("PUT", { valuesYAML }),
        ),

    // -- Test cases
    testCases: () => request<List<TestCase>>("/api/v1/testcases").then((r) => r.items ?? []),
    testCase: (name: string) => request<TestCase>(`/api/v1/testcases/${seg(name)}`),
    createTestCase: (obj: ObjectInput<TestCaseSpec>) =>
        request<TestCase>("/api/v1/testcases", json("POST", obj)),
    updateTestCase: (name: string, obj: { spec: TestCaseSpec }) =>
        request<TestCase>(`/api/v1/testcases/${seg(name)}`, json("PUT", obj)),
    deleteTestCase: (name: string) =>
        request<void>(`/api/v1/testcases/${seg(name)}`, { method: "DELETE" }),
    // One TestRun per deployment the TestCase drives (see testCaseDeployments) -
    // always an array, even for today's common single-deployment case.
    runTestCase: (name: string) =>
        request<TestRun[]>(`/api/v1/testcases/${seg(name)}/runs`, json("POST", {})),

    // -- Test case templates
    testCaseTemplates: () =>
        request<List<TestCaseTemplate>>("/api/v1/testcasetemplates").then((r) => r.items ?? []),
    testCaseTemplate: (name: string) =>
        request<TestCaseTemplate>(`/api/v1/testcasetemplates/${seg(name)}`),
    createTestCaseTemplate: (obj: ObjectInput<TestCaseTemplateSpec>) =>
        request<TestCaseTemplate>("/api/v1/testcasetemplates", json("POST", obj)),
    updateTestCaseTemplate: (name: string, obj: { spec: TestCaseTemplateSpec }) =>
        request<TestCaseTemplate>(`/api/v1/testcasetemplates/${seg(name)}`, json("PUT", obj)),
    deleteTestCaseTemplate: (name: string) =>
        request<void>(`/api/v1/testcasetemplates/${seg(name)}`, { method: "DELETE" }),

    // -- Test runs (read-only: a TestRun is only ever created via runTestCase)
    testRuns: (testCase?: string) =>
        request<List<TestRun>>(
            testCase
                ? `/api/v1/testruns?labelSelector=${seg(`bench.goauthentik.io/testcase=${testCase}`)}`
                : "/api/v1/testruns",
        ).then((r) => r.items ?? []),
    testRun: (name: string) => request<TestRun>(`/api/v1/testruns/${seg(name)}`),
    deleteTestRun: (name: string) =>
        request<void>(`/api/v1/testruns/${seg(name)}`, { method: "DELETE" }),

    // -- Metrics. atSeconds anchors the instant query to a unix time instead
    // of "now" - see format.ts's metricsAnchorFromRuns for why a Quick
    // TestCase needs that.
    compare: (testids: string[], window: string, atSeconds?: number) =>
        request<CompareResponse>(
            `/api/v1/metrics/compare?testids=${seg(testids.join(","))}&window=${seg(window)}` +
                (atSeconds !== undefined ? `&time=${atSeconds}` : ""),
        ),
    metricsRange: (
        testids: string[],
        metric: string,
        startSeconds: number,
        endSeconds: number,
        step: string,
    ) =>
        request<RangeResponse>(
            `/api/v1/metrics/range?testids=${seg(testids.join(","))}` +
                `&metric=${seg(metric)}&start=${startSeconds}&end=${endSeconds}&step=${seg(step)}`,
        ),
    // CPU/memory for each testid's own deployment, summed across its pods -
    // scoped by namespace, not by a testid label the way metricsRange's
    // queries are (cAdvisor never tags container metrics with a k6 testid).
    // Same RangeResponse shape as metricsRange (see internal/api/podmetrics.go).
    resourceRange: (
        testids: string[],
        resource: "cpu" | "memory",
        startSeconds: number,
        endSeconds: number,
        step: string,
        // Only that authentik process's pods; omitted, every non-Job pod in
        // the namespace (postgres included).
        component?: "server" | "worker",
    ) =>
        request<RangeResponse>(
            `/api/v1/metrics/resources?testids=${seg(testids.join(","))}` +
                `&resource=${seg(resource)}&start=${startSeconds}&end=${endSeconds}&step=${seg(step)}` +
                (component ? `&component=${seg(component)}` : ""),
        ),

    // -- Grafana proxy (internal/api/grafana_session.go): sets the cookie the
    // proxy itself authenticates with, since its <iframe> can't send a bearer
    // token.
    grafanaSession: () =>
        request<{ expiresAt: string }>("/api/v1/grafana/session", json("POST", {})),

    // -- Traces (internal/api/traces.go): a cached sample of recent traces per
    // target, from each target's own workload cluster Tempo.
    traceBreakdown: (testid: string, window: string) =>
        request<TraceBreakdown>(
            `/api/v1/traces/breakdown?testid=${seg(testid)}&window=${seg(window)}`,
        ),

    // -- Profiles (internal/api/profiles.go): each target's merged CPU profile
    // from Pyroscope, cached server-side for two minutes.
    profileBreakdown: (testid: string, window: string, component: string) =>
        request<ProfileBreakdown>(
            `/api/v1/profiles/breakdown?testid=${seg(testid)}&window=${seg(window)}&component=${seg(component)}`,
        ),

    // -- Logs (internal/api/logs.go): each target's log lines by level and
    // its most frequent warnings/errors, from Loki, cached server-side for
    // two minutes.
    logBreakdown: (testid: string, window: string) =>
        request<LogBreakdown>(`/api/v1/logs/breakdown?testid=${seg(testid)}&window=${seg(window)}`),

    // -- Kubernetes Events about one bench object, newest first
    // (internal/api/objectevents.go).
    objectEvents: (path: ResourcePath, name: string) =>
        request<List<ObjectEvent>>(`/api/v1/${path}/${seg(name)}/events`).then(
            (r) => r.items ?? [],
        ),

    // -- Images (internal/api/images.go): whether a goauthentik image tag has
    // been published on ghcr.io - repository without the registry host.
    checkImage: (repository: string, tag: string) =>
        request<{ exists: boolean }>(
            `/api/v1/images/check?repository=${seg(repository)}&tag=${seg(tag)}`,
        ),

    // -- Alerts (internal/api/alerts.go) - aggregated across every workload
    // cluster's own Alertmanager, there is no central instance.
    // unhealthy maps cluster -> why its alerts can't be trusted (Alertmanager
    // unreachable or Watchdog not firing).
    alerts: () =>
        request<List<Alert> & { unhealthy?: Record<string, string> }>("/api/v1/alerts").then(
            (r) => ({
                items: r.items ?? [],
                unhealthy: r.unhealthy ?? {},
            }),
        ),
    placement: () => request<List<PodPlacement>>("/api/v1/placement").then((r) => r.items ?? []),

    // -- Shell (internal/api/shell.go)
    shellContexts: () =>
        request<List<ShellContext>>("/api/v1/shell/contexts").then((r) => r.items ?? []),
    createShellSession: (context: string) =>
        request<{ ticket: string; expiresAt: string }>(
            "/api/v1/shell/sessions",
            json("POST", { context }),
        ),

    // -- Live updates (internal/api/events.go): mint a ticket, then open
    // /api/v1/events/connect?ticket=... the same way the shell does - see
    // events.ts.
    createEventsSession: () =>
        request<{ ticket: string; expiresAt: string }>("/api/v1/events/sessions", json("POST", {})),
};
