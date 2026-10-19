// Deep links into the shell page. Everything else in the UI hands off to the
// shell this way rather than re-implementing kubectl's views.

import type { AuthentikDeploymentSpec } from "../types";

function withOptions(params: URLSearchParams, prefill?: string, namespace?: string): string {
    if (namespace) {
        params.set("namespace", namespace);
    }
    if (prefill) {
        params.set("prefill", prefill);
    }
    return `/shell?${params.toString()}`;
}

/**
 * shellLink opens the shell on a kubeconfig context, optionally with a
 * command typed at the prompt - typed only, never run: the user still
 * presses Enter (see sanitizePrefill) - and optionally in a namespace, which
 * becomes the session context's default so follow-up commands land there too.
 */
export function shellLink(context: string, prefill?: string, namespace?: string): string {
    return withOptions(new URLSearchParams({ context }), prefill, namespace);
}

/**
 * shellLinkForCluster is shellLink keyed by a WorkloadCluster name; the
 * shell page resolves it to that cluster's context (CAPI's kubeconfig names
 * it <name>-admin@<name>, but the page reads the real mapping from the
 * backend rather than assuming that).
 */
export function shellLinkForCluster(
    cluster: string | undefined,
    prefill?: string,
    namespace?: string,
): string {
    if (!cluster) {
        return shellLink("management", prefill, namespace);
    }
    return withOptions(new URLSearchParams({ cluster }), prefill, namespace);
}

// A DNS-1123 label, as the backend requires of a session namespace.
const NAMESPACE_RE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

/**
 * sanitizeNamespace returns value if it is a valid namespace name, else
 * undefined - a malformed ?namespace= opens the shell in the context's own
 * default rather than failing.
 */
export function sanitizeNamespace(value: string | null): string | undefined {
    const ns = value?.trim();
    return ns && NAMESPACE_RE.test(ns) ? ns : undefined;
}

/**
 * sanitizePrefill strips every control character - above all CR/LF, which
 * would turn "typed at the prompt" into "executed", and ESC, which could
 * drive the terminal. A crafted link must never be able to run anything
 * without the user pressing Enter themselves.
 */
export function sanitizePrefill(value: string | null): string {
    // eslint-disable-next-line no-control-regex
    return (value ?? "").replace(/[\u0000-\u001f\u007f-\u009f]/g, " ").slice(0, 2000);
}

// Matches render.postgresRoleFixImage, so the client speaks the same protocol
// version as the embedded server.
const PSQL_IMAGE = "docker.io/library/postgres:17.11-bookworm";

/**
 * psqlCommand is the shell command that drops into psql on an
 * AuthentikDeployment's database, per Postgres mode:
 *
 * - Embedded: exec into the bundled subchart's StatefulSet. The subchart defaults to
 *   auth.usePasswordFiles, so the container has POSTGRES_USER and POSTGRES_PASSWORD_FILE (a mounted
 *   Secret key) rather than the password itself; the single-quoted sh -c reads both inside the
 *   pod.
 * - CNPG: exec into the primary via the cluster's -rw Service (kubectl picks a pod from its
 *   selector), as the postgres user CNPG runs as.
 * - External: a throwaway pod with psql, its PG* env wired from the deployment's connection Secret so
 *   the password never hits the terminal.
 */
export function psqlCommand(name: string, ns: string, spec: AuthentikDeploymentSpec): string {
    switch (spec.postgres.mode) {
        case "CNPG":
            return `kubectl exec -it -n ${ns} svc/${name}-postgres-rw -c postgres -- psql authentik`;
        case "External": {
            const secret = spec.postgres.external?.secretRef.name ?? "";
            const pod = `psql-${name}`;
            const fromSecret = (env: string, key: string) => ({
                name: env,
                valueFrom: { secretKeyRef: { name: secret, key } },
            });
            const overrides = {
                spec: {
                    containers: [
                        {
                            name: pod,
                            image: PSQL_IMAGE,
                            command: ["psql"],
                            stdin: true,
                            tty: true,
                            env: [
                                fromSecret("PGHOST", "host"),
                                fromSecret("PGPORT", "port"),
                                fromSecret("PGDATABASE", "database"),
                                fromSecret("PGUSER", "username"),
                                fromSecret("PGPASSWORD", "password"),
                                {
                                    name: "PGSSLMODE",
                                    value: spec.postgres.external?.sslMode || "require",
                                },
                            ],
                        },
                    ],
                },
            };
            return `kubectl run ${pod} -n ${ns} --rm -it --restart=Never --image=${PSQL_IMAGE} --overrides='${JSON.stringify(overrides)}'`;
        }
        default:
            return `kubectl exec -it -n ${ns} statefulset/${name}-postgresql -c postgresql -- sh -c 'PGPASSWORD="$(cat "$POSTGRES_PASSWORD_FILE")" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" "\${POSTGRES_DATABASE:-authentik}"'`;
    }
}
