import { AuthentikDeployment } from "../types";
import { duplicateSpec, emptySpec } from "./Deployments";

import { describe, expect, it } from "vitest";

describe("duplicateSpec", () => {
    const source: AuthentikDeployment = {
        metadata: { name: "a" },
        spec: {
            ...emptySpec(),
            namespace: "a",
            targetName: "a-target",
            tenant: { presetRef: { name: "small" } },
        },
    };

    it("clears the namespace and target name, so they follow the new name", () => {
        const spec = duplicateSpec(source);
        expect(spec.namespace).toBe("");
        expect(spec).not.toHaveProperty("targetName");
    });

    it("keeps the tenant", () => {
        expect(duplicateSpec(source).tenant).toEqual({ presetRef: { name: "small" } });
    });
});
