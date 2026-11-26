import { QueryErrors, queryErrorNotes } from "./QueryErrors";

import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

describe("queryErrorNotes", () => {
    it("is empty when every test answered", () => {
        expect(queryErrorNotes(undefined)).toEqual([]);
        expect(queryErrorNotes({})).toEqual([]);
    });

    it("names each failed test with its reason, sorted by test", () => {
        expect(queryErrorNotes({ b: "timeout", a: "no Prometheus configured" })).toEqual([
            "a: no Prometheus configured",
            "b: timeout",
        ]);
    });

    it("merges several responses and names a test once", () => {
        expect(
            queryErrorNotes({ a: "timeout" }, undefined, { a: "timeout", b: "refused" }),
        ).toEqual(["a: timeout", "b: refused"]);
    });
});

describe("QueryErrors", () => {
    it("renders nothing when every test answered", () => {
        expect(renderToStaticMarkup(<QueryErrors errors={undefined} />)).toBe("");
        expect(renderToStaticMarkup(<QueryErrors errors={{}} />)).toBe("");
    });

    it("warns and lists the tests that could not be loaded", () => {
        const html = renderToStaticMarkup(
            <QueryErrors errors={{ "pr-1-login": 'query Prometheus "http://x": timeout' }} />,
        );
        expect(html).toContain("alert error");
        expect(html).toContain("Could not load metrics for every test.");
        expect(html).toContain("<li>pr-1-login: query Prometheus");
    });
});
