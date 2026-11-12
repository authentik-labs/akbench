import {
    builtInLabel,
    isBuiltIn,
    nestChildren,
    PARENT_LABEL,
    TenantPreset,
    tenantPresetCopy,
    tenantPresetProblem,
    TenantPresetSpec,
    TestCase,
    TestCaseSpec,
    testCaseBaseline,
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

describe("isBuiltIn", () => {
    it("is true only for objects the operator labelled", () => {
        expect(isBuiltIn({ metadata: { name: "small", labels: { [builtInLabel]: "true" } } })).toBe(
            true,
        );
        expect(isBuiltIn({ metadata: { name: "ours", labels: { team: "perf" } } })).toBe(false);
        expect(isBuiltIn({ metadata: { name: "bare" } })).toBe(false);
    });
});

// The same cases as internal/controller/tenantpreset_schema_test.go, which
// checks them against the CRD schema this function stands in for.
describe("tenantPresetProblem", () => {
    const valid: TenantPresetSpec = { users: 100, groups: 10, groupDepth: 2, groupsPerUser: 3 };

    it.each<[string, Partial<TenantPresetSpec>]>([
        ["the valid preset", {}],
        [
            "no groups, a flat tree and no memberships",
            { groups: 0, groupDepth: 1, groupsPerUser: 0 },
        ],
        ["a single user", { users: 1 }],
        ["as many users as the names have digits for", { users: 1_000_000 }],
        ["as many groups as the names have digits for", { groups: 1_000_000 }],
        ["the deepest tree", { groups: 10, groupDepth: 10 }],
        ["as many tree levels as groups", { groups: 3, groupsPerUser: 3, groupDepth: 3 }],
        ["every user in every group", { groupsPerUser: 10 }],
        ["the largest seed", { seed: 2_147_483_647 }],
    ])("accepts %s", (_, change) => {
        expect(tenantPresetProblem({ ...valid, ...change })).toBeUndefined();
    });

    it.each<[string, Partial<TenantPresetSpec>]>([
        ["no users", { users: 0 }],
        ["more users than the names have digits for", { users: 1_000_001 }],
        ["a negative group count", { groups: -1 }],
        ["a group tree with no levels", { groupDepth: 0 }],
        ["more groups per user than groups", { groupsPerUser: 11 }],
        ["more tree levels than groups", { groups: 3, groupsPerUser: 3, groupDepth: 4 }],
        ["a tree without groups", { groups: 0, groupsPerUser: 0 }],
        ["a tree deeper than ten levels", { groups: 11, groupDepth: 11 }],
        ["a negative seed", { seed: -1 }],
        ["a seed past int32", { seed: 2_147_483_648 }],
        ["an empty number field", { users: NaN }],
        ["a fraction", { groups: 2.5 }],
    ])("rejects %s", (_, change) => {
        expect(tenantPresetProblem({ ...valid, ...change })).toEqual(expect.any(String));
    });
});

describe("tenantPresetCopy", () => {
    const source: TenantPreset = {
        metadata: { name: "small", labels: { "bench.goauthentik.io/built-in": "true" } },
        spec: {
            description: "5,000 users in 100 groups, three levels deep.",
            seed: 1,
            users: 5000,
            groups: 100,
            groupDepth: 3,
            groupsPerUser: 5,
        },
    };

    it("keeps the numbers but not the description, which describes the source", () => {
        expect(tenantPresetCopy(source)).toEqual({
            name: "small-copy",
            spec: {
                description: "",
                seed: 1,
                users: 5000,
                groups: 100,
                groupDepth: 3,
                groupsPerUser: 5,
            },
        });
        expect(source.spec.description).toBe("5,000 users in 100 groups, three levels deep.");
    });
});
