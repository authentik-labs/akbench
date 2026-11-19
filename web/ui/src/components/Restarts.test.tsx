import { RestartsResponse } from "../types";
import { restartsKey, RestartsNotes, restartsNotes, restartSummary } from "./Restarts";

import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

describe("restartSummary", () => {
    it("is empty for a target that stayed up", () => {
        expect(restartSummary({ testid: "t", target: "a", restarts: {}, started: {} })).toBe("");
    });

    it("names restarted containers and replaced pods", () => {
        expect(
            restartSummary({
                testid: "t",
                target: "a",
                restarts: { server: 2 },
                started: { worker: 1, postgresql: 2 },
            }),
        ).toBe("server restarted ×2, worker in 1 new pod, postgresql in 2 new pods");
    });

    it("accepts a response without started counts", () => {
        expect(restartSummary({ testid: "t", target: "a", restarts: { server: 1 } })).toBe(
            "server restarted ×1",
        );
    });
});

const response: RestartsResponse = {
    window: "5m",
    targets: [
        { testid: "t", target: "a", restarts: { server: 2 }, started: {} },
        { testid: "t", target: "b", restarts: {}, started: {} },
    ],
    errors: { u: "test case not found" },
};

describe("restartsNotes", () => {
    const key = restartsKey(["t", "u"], "5m", 1800000000);

    it("lists the targets that restarted and what could not be checked", () => {
        const notes = restartsNotes({ key, data: response }, key);
        expect(notes.restarted.map((target) => target.target)).toEqual(["a"]);
        expect(notes.unchecked).toEqual(["u: test case not found"]);
    });

    it("shows only the error of a failed fetch", () => {
        expect(restartsNotes({ key, error: "502 Bad Gateway" }, key)).toEqual({
            restarted: [],
            unchecked: ["502 Bad Gateway"],
        });
    });

    it("shows nothing fetched for another selection, period or anchor", () => {
        const empty = { restarted: [], unchecked: [] };
        for (const other of [
            restartsKey(["t"], "5m", 1800000000),
            restartsKey(["t", "u"], "15m", 1800000000),
            restartsKey(["t", "u"], "5m"),
        ]) {
            expect(restartsNotes({ key: other, data: response }, key)).toEqual(empty);
            expect(restartsNotes({ key: other, error: "failed" }, key)).toEqual(empty);
        }
        expect(restartsNotes(undefined, key)).toEqual(empty);
    });
});

describe("RestartsNotes", () => {
    it("renders nothing while every target stayed up", () => {
        expect(renderToStaticMarkup(<RestartsNotes restarted={[]} unchecked={[]} />)).toBe("");
    });

    it("warns about the targets that restarted and notes what could not be checked", () => {
        const html = renderToStaticMarkup(
            <RestartsNotes {...restartsNotes({ key: "k", data: response }, "k")} />,
        );
        expect(html).toContain('class="alert error"');
        expect(html).toContain('<span class="mono">a</span>: server restarted ×2');
        expect(html).not.toContain('<span class="mono">b</span>');
        expect(html).toContain('class="alert info"');
        expect(html).toContain("u: test case not found");
    });

    it("shows only the note when the fetch failed", () => {
        const html = renderToStaticMarkup(
            <RestartsNotes {...restartsNotes({ key: "k", error: "502 Bad Gateway" }, "k")} />,
        );
        expect(html).not.toContain('class="alert error"');
        expect(html).toContain("502 Bad Gateway");
    });
});
