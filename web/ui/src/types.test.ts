import {
    nestChildren,
    PARENT_LABEL,
    TestCase,
    TestCaseSpec,
    testCaseBaseline,
    testCaseBaselineTarget,
} from "./types";

import { describe, expect, it } from "vitest";

const spec = (patch: Partial<TestCaseSpec>): TestCaseSpec => ({
    profile: "Burn",
    vus: 1,
    ...patch,
});

describe("testCaseBaseline", () => {
    const deploymentRefs = [{ name: "a" }, { name: "b" }];

    it("uses baselineRef while it names one of the deployments", () => {
        expect(testCaseBaseline(spec({ deploymentRefs, baselineRef: { name: "b" } }))).toBe("b");
    });

    it("falls back to the first deployment when unset or stale", () => {
        expect(testCaseBaseline(spec({ deploymentRefs }))).toBe("a");
        expect(testCaseBaseline(spec({ deploymentRefs, baselineRef: { name: "gone" } }))).toBe("a");
    });

    it("reads the deprecated singular deploymentRef", () => {
        expect(testCaseBaseline(spec({ deploymentRef: { name: "old" } }))).toBe("old");
    });

    it("is undefined without deployments", () => {
        expect(testCaseBaseline(spec({}))).toBeUndefined();
    });
});

describe("testCaseBaselineTarget", () => {
    const testCase = (status?: TestCase["status"]): TestCase => ({
        metadata: { name: "t" },
        spec: spec({ deploymentRefs: [{ name: "a" }, { name: "b" }], baselineRef: { name: "b" } }),
        status,
    });

    it("is the target recorded for the baseline deployment", () => {
        expect(
            testCaseBaselineTarget(
                testCase({
                    deployments: [
                        { name: "a", ready: true, target: "head" },
                        { name: "b", ready: true, target: "main" },
                    ],
                }),
            ),
        ).toBe("main");
    });

    it("falls back to the deployment's name, the default target", () => {
        expect(testCaseBaselineTarget(testCase())).toBe("b");
        expect(
            testCaseBaselineTarget(testCase({ deployments: [{ name: "b", ready: true }] })),
        ).toBe("b");
    });
});

const tc = (name: string, parent?: string) =>
    ({
        metadata: { name, labels: parent ? { [PARENT_LABEL]: parent } : undefined },
        spec: { profile: "Burn", vus: 1 },
    }) as TestCase;

describe("nestChildren", () => {
    it("puts children right after their parent, indented", () => {
        const rows = [tc("pr-1-login", "pr-1"), tc("other"), tc("pr-1"), tc("pr-1-oauth", "pr-1")];
        expect(nestChildren(rows).map(({ tc, depth }) => [tc.metadata.name, depth])).toEqual([
            ["other", 0],
            ["pr-1", 0],
            ["pr-1-login", 1],
            ["pr-1-oauth", 1],
        ]);
    });

    it("keeps a child top level when its parent isn't listed", () => {
        expect(nestChildren([tc("pr-1-login", "pr-1")])).toEqual([
            { tc: expect.anything(), depth: 0 },
        ]);
    });
});
