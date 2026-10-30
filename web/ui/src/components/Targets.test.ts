import type { TestCase } from "../types";
import { burnStatus } from "./Targets";

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

describe("burnStatus", () => {
    it("waits until the operator schedules the shared start", () => {
        expect(burnStatus(burn(), now)).toMatch(/^waiting/);
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
