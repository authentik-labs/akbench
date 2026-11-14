import { ConfigDiff } from "../types";
import { configDiffNotices, formatDatabase } from "./ConfigDiff";

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
