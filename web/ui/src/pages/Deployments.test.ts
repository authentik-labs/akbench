import { AuthentikDeployment } from "../types";
import { duplicateSpec } from "./Deployments";

import { describe, expect, it } from "vitest";

const source: AuthentikDeployment = {
    metadata: { name: "src" },
    spec: {
        namespace: "src",
        targetName: "src-target",
        clusterRef: { name: "cluster-a" },
        server: { replicas: 2 },
        worker: { replicas: 1 },
        placement: { mode: "Default", topologyKey: "kubernetes.io/hostname" },
        postgres: { mode: "Embedded", embedded: {} },
        chart: { repo: "https://charts.goauthentik.io" },
        authentik: {
            image: "ghcr.io/goauthentik/server",
            tag: "2026.2",
            extraValuesRef: { name: "src-extra-values" },
        },
    },
};

describe("duplicateSpec", () => {
    it("keeps the source's settings", () => {
        const spec = duplicateSpec(source);
        expect(spec.clusterRef).toEqual({ name: "cluster-a" });
        expect(spec.server).toEqual({ replicas: 2 });
        expect(spec.authentik?.tag).toBe("2026.2");
    });

    it("clears what follows the new name", () => {
        const spec = duplicateSpec(source);
        expect(spec.namespace).toBe("");
        expect(spec.targetName).toBeUndefined();
    });

    // The source owns its extra-values ConfigMap, which is deleted with it.
    it("drops the source's extraValuesRef", () => {
        expect(duplicateSpec(source).authentik?.extraValuesRef).toBeUndefined();
    });

    it("leaves the source untouched", () => {
        duplicateSpec(source);
        expect(source.spec.namespace).toBe("src");
        expect(source.spec.authentik?.extraValuesRef).toEqual({ name: "src-extra-values" });
    });
});
