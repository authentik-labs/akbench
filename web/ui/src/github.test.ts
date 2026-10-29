import {
    devImageTag,
    k8sNameInput,
    k8sNameSegment,
    parsePullRequestNumber,
    releaseVersion,
} from "./github";

import { describe, expect, it } from "vitest";

describe("parsePullRequestNumber", () => {
    it("accepts a bare number, with or without a hash", () => {
        expect(parsePullRequestNumber("12345")).toBe(12345);
        expect(parsePullRequestNumber(" #12345 ")).toBe(12345);
    });

    it("accepts an authentik pull request URL", () => {
        expect(parsePullRequestNumber("https://github.com/goauthentik/authentik/pull/12345")).toBe(
            12345,
        );
        expect(parsePullRequestNumber("github.com/goauthentik/authentik/pull/12345/files")).toBe(
            12345,
        );
        expect(
            parsePullRequestNumber(
                "https://github.com/goauthentik/authentik/pull/12345#issuecomment-1",
            ),
        ).toBe(12345);
    });

    it("rejects another repository's pull request", () => {
        expect(
            parsePullRequestNumber("https://github.com/authentik-labs/akbench/pull/1"),
        ).toBeUndefined();
    });

    it("rejects anything else", () => {
        expect(parsePullRequestNumber("")).toBeUndefined();
        expect(parsePullRequestNumber("main")).toBeUndefined();
        expect(
            parsePullRequestNumber("https://github.com/goauthentik/authentik/issues/1"),
        ).toBeUndefined();
    });
});

describe("image helpers", () => {
    it("tags dev images by full commit sha", () => {
        expect(devImageTag("ccd5f45863edfb003c4dd876fabfcb0123456789")).toBe(
            "gh-ccd5f45863edfb003c4dd876fabfcb0123456789",
        );
    });

    it("strips authentik's release tag prefix", () => {
        expect(releaseVersion("version/2026.6.0")).toBe("2026.6.0");
        expect(releaseVersion("2026.6.0")).toBe("2026.6.0");
    });

    it("makes versions and branches name-safe", () => {
        expect(k8sNameSegment("2026.6.0")).toBe("2026-6-0");
        expect(k8sNameSegment("version-2026.6")).toBe("version-2026-6");
        expect(k8sNameSegment("Feature/Foo_bar")).toBe("feature-foo-bar");
        expect(k8sNameInput("My Deploy-")).toBe("my-deploy-");
        expect(k8sNameInput(" -a.b")).toBe("a-b");
    });
});
