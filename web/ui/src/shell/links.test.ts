import type { AuthentikDeploymentSpec } from "../types";
import {
    psqlCommand,
    sanitizeNamespace,
    sanitizePrefill,
    shellLink,
    shellLinkForCluster,
} from "./links";

import { describe, expect, it } from "vitest";

describe("sanitizePrefill", () => {
    it("keeps an ordinary command intact", () => {
        expect(sanitizePrefill("kubectl get pods -n ak-1 | grep -v Running")).toBe(
            "kubectl get pods -n ak-1 | grep -v Running",
        );
    });

    it("never lets a link press Enter or send escape sequences", () => {
        const out = sanitizePrefill("true\rrm -rf /\nx\u001b[2J\u0003\u009b");
        // oxlint-disable-next-line no-control-regex -- control characters are what this asserts on
        expect(out).not.toMatch(/[\r\n\u001b\u0003\u009b]/);
    });

    it("handles a missing parameter", () => {
        expect(sanitizePrefill(null)).toBe("");
    });
});

describe("shell links", () => {
    it("encodes context and prefill", () => {
        expect(shellLink("wc1-admin@wc1", "kubectl get nodes")).toBe(
            "/shell?context=wc1-admin%40wc1&prefill=kubectl+get+nodes",
        );
    });

    it("falls back to the management context without a cluster", () => {
        expect(shellLinkForCluster(undefined)).toBe("/shell?context=management");
        expect(shellLinkForCluster("wc1")).toBe("/shell?cluster=wc1");
    });

    it("encodes a namespace", () => {
        expect(shellLinkForCluster("wc1", "kubectl get pods", "ak-1")).toBe(
            "/shell?cluster=wc1&namespace=ak-1&prefill=kubectl+get+pods",
        );
        expect(shellLink("management", undefined, "ak-1")).toBe(
            "/shell?context=management&namespace=ak-1",
        );
    });
});

describe("sanitizeNamespace", () => {
    it("accepts DNS-1123 labels only", () => {
        expect(sanitizeNamespace("ak-1")).toBe("ak-1");
        expect(sanitizeNamespace(" ak-1 ")).toBe("ak-1");
        for (const bad of [null, "", "Upper", "a b", "a/b", "-x", "a".repeat(64)]) {
            expect(sanitizeNamespace(bad)).toBeUndefined();
        }
    });
});

describe("psqlCommand", () => {
    const spec = (postgres: AuthentikDeploymentSpec["postgres"]) =>
        ({ postgres }) as AuthentikDeploymentSpec;

    it("execs into the embedded StatefulSet", () => {
        expect(psqlCommand("ak", "ak-1", spec({ mode: "Embedded" }))).toBe(
            'kubectl exec -it -n ak-1 statefulset/ak-postgresql -c postgresql -- sh -c \'PGPASSWORD="$(cat "$POSTGRES_PASSWORD_FILE")" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" "${POSTGRES_DATABASE:-authentik}"\'',
        );
    });

    it("execs into the CNPG primary", () => {
        expect(psqlCommand("ak", "ak-1", spec({ mode: "CNPG" }))).toBe(
            "kubectl exec -it -n ak-1 svc/ak-postgres-rw -c postgres -- psql authentik",
        );
    });

    it("wires an external database's Secret into a throwaway pod", () => {
        const cmd = psqlCommand(
            "ak",
            "ak-1",
            spec({ mode: "External", external: { secretRef: { name: "pg" } } }),
        );
        expect(cmd).toMatch(/^kubectl run psql-ak -n ak-1 --rm -it --restart=Never /);
        const overrides = JSON.parse(cmd.slice(cmd.indexOf("'") + 1, -1));
        const env = overrides.spec.containers[0].env;
        expect(env).toContainEqual({
            name: "PGPASSWORD",
            valueFrom: { secretKeyRef: { name: "pg", key: "password" } },
        });
        expect(env).toContainEqual({ name: "PGSSLMODE", value: "require" });
        expect(sanitizePrefill(cmd)).toBe(cmd);
    });
});
