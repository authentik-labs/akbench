import { Condition, PARENT_LABEL, TestCase } from "../types";
import { buildTargetRows, burnStatus } from "./Targets";

import { describe, expect, it } from "vitest";

const now = Date.parse("2026-10-30T12:00:00Z");

function burn(burningSince?: string, extra: Partial<TestCase["spec"]> = {}): TestCase {
    return {
        metadata: { name: "t" },
        spec: { profile: "Burn", ...extra },
        status: {
            deployments: [
                { name: "a", burningSince },
                { name: "b", burningSince },
            ],
        },
    } as TestCase;
}

describe("buildTargetRows", () => {
    it("does not call the targets of a finished burn not ready", () => {
        const tc = burn("2026-10-30T10:00:00Z");
        tc.spec.deploymentRefs = [{ name: "a" }, { name: "b" }];
        tc.status!.deployments = tc.status!.deployments!.map((d) => ({ ...d, ready: false }));

        expect(buildTargetRows(tc, [], []).map((r) => r.ready)).toEqual([false, false]);
        tc.status!.phase = "Completed";
        expect(buildTargetRows(tc, [], []).map((r) => r.ready)).toEqual([undefined, undefined]);
    });
});

describe("burnStatus", () => {
    it("says a burn finished at its time limit, not that it is waiting", () => {
        const tc = burn("2026-10-30T10:00:00Z", { burnDuration: "1h" });
        tc.status!.deployments = tc.status!.deployments!.map((d) => ({
            ...d,
            burnStoppedAt: "2026-10-30T11:00:00Z",
        }));
        tc.status!.conditions = [
            { type: "Ready", status: "False", reason: "BurnFinished" } as Condition,
        ];
        expect(burnStatus(tc, now)).toBe("finished at its time limit of 1h");
    });

    it("waits until the operator schedules the shared start", () => {
        expect(burnStatus(burn(), now)).toMatch(/^waiting/);
    });

    it("waits for the rest of its parent once staged", () => {
        const tc = burn();
        tc.metadata.labels = { [PARENT_LABEL]: "m" };
        expect(burnStatus(tc, now)).toBe(
            "waiting for every target to be ready, to start them together",
        );
        tc.status!.conditions = [{ type: "BurnStaged", status: "True" } as Condition];
        expect(burnStatus(tc, now)).toMatch(/^waiting for every test case of m /);
    });

    it("counts down to a scheduled start", () => {
        expect(burnStatus(burn("2026-10-30T12:01:30Z"), now)).toBe("starting on all targets in 1m");
    });

    it("reports how long it has been burning", () => {
        expect(burnStatus(burn("2026-10-30T09:00:00Z"), now)).toBe("burning for 3h 0m");
    });

    it("says nothing for an archived or Quick test", () => {
        expect(burnStatus(burn(undefined, { archived: true }), now)).toBeUndefined();
        expect(burnStatus(burn(undefined, { profile: "Quick" }), now)).toBeUndefined();
    });
});
