import { ConfigDiff, PARENT_LABEL, TestCase } from "../types";
import {
    configDiffFor,
    configDiffNotices,
    configPathUnder,
    configUnderTestOwner,
    formatDatabase,
    withoutUnderTest,
    withUnderTest,
} from "./ConfigDiff";

import { describe, expect, it } from "vitest";

function diff(extra: Partial<ConfigDiff> = {}): ConfigDiff {
    return { testid: "t", targets: ["main", "pr"], differences: [], ...extra };
}

describe("formatDatabase", () => {
    it("writes what is set", () => {
        expect(formatDatabase({ host: "db", port: "5432", name: "authentik", user: "ak" })).toBe(
            "ak@db:5432/authentik",
        );
        expect(formatDatabase({ host: "db" })).toBe("db");
    });
});

describe("configDiffFor", () => {
    it("keeps a diff only for the test it was fetched for", () => {
        expect(configDiffFor("t", diff())).toEqual(diff());
        expect(configDiffFor("other", diff())).toBeUndefined();
        expect(configDiffFor("t", undefined)).toBeUndefined();
    });
});

describe("configDiffNotices", () => {
    it("is empty for a clean diff, or before one arrives", () => {
        expect(configDiffNotices("t", diff(), undefined)).toEqual([]);
        expect(configDiffNotices("t", undefined, undefined)).toEqual([]);
    });

    it("says which deployments could not be compared", () => {
        const pending = [
            { name: "pr", reason: "it has no HelmRelease yet" },
            { name: "gone", reason: "the AuthentikDeployment does not exist" },
        ];
        expect(configDiffNotices("t", diff({ targets: ["main"], pending }), undefined)).toEqual([
            "Could not compare pr: it has no HelmRelease yet",
            "Could not compare gone: the AuthentikDeployment does not exist",
        ]);
    });

    it("notes the external databases of a compared pair", () => {
        const databases = { main: { host: "db-a" }, pr: { host: "db-b" } };
        expect(configDiffNotices("t", diff({ databases }), undefined)).toEqual([
            "Each deployment has its own external database, which is expected: main uses db-a; pr uses db-b.",
        ]);
        expect(
            configDiffNotices(
                "t",
                diff({ targets: ["main"], databases: { main: { host: "db-a" } } }),
                undefined,
            ),
        ).toEqual([]);
    });

    it("replaces the last result with a failed fetch", () => {
        const pending = [{ name: "pr", reason: "it has no HelmRelease yet" }];
        expect(configDiffNotices("t", diff({ pending }), "Forbidden")).toEqual([
            "Could not compare the deployments of t: Forbidden",
        ]);
    });
});

describe("configPathUnder", () => {
    it("matches the path itself and the settings below it", () => {
        expect(configPathUnder("values.a.b", "values.a.b")).toBe(true);
        expect(configPathUnder("values.a.b", "values.a")).toBe(true);
        expect(configPathUnder("values.env[X].value", "values.env")).toBe(true);
        expect(configPathUnder("values.ab", "values.a")).toBe(false);
        expect(configPathUnder("values.a", "values.a.b")).toBe(false);
    });
});

describe("withUnderTest and withoutUnderTest", () => {
    it("adds a path only when nothing listed covers it", () => {
        expect(withUnderTest(undefined, "values.a.b")).toEqual(["values.a.b"]);
        expect(withUnderTest(["values.a"], "values.a.b")).toEqual(["values.a"]);
        expect(withUnderTest(["values.c"], "values.a.b")).toEqual(["values.c", "values.a.b"]);
    });
    it("drops every entry that covers a path", () => {
        expect(withoutUnderTest(["values.a", "values.a.b", "values.c"], "values.a.b")).toEqual([
            "values.c",
        ]);
        expect(withoutUnderTest(undefined, "values.a")).toEqual([]);
    });
});

describe("configUnderTestOwner", () => {
    it("is a child's parent, else the test case itself", () => {
        const tc = (labels?: Record<string, string>): TestCase => ({
            metadata: { name: "child", labels },
            spec: { profile: "Quick", vus: 1 },
        });
        expect(configUnderTestOwner(tc())).toBe("child");
        expect(configUnderTestOwner(tc({ [PARENT_LABEL]: "parent" }))).toBe("parent");
    });
});
