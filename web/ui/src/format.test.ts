import {
    archivedBurnWindow,
    buildLabel,
    colorFor,
    earliestBurnStart,
    formatDuration,
    formatMetricValue,
    metricsAnchorFromRuns,
    onlyTargets,
    parseTimeRange,
    readableTextOn,
    relativeDelta,
    shortTag,
    SINCE_START,
    stepFor,
    targetNameOf,
    timeRangeSeconds,
} from "./format";

import { describe, expect, it } from "vitest";

describe("formatMetricValue", () => {
    it("renders undefined as a dash", () => {
        expect(formatMetricValue(undefined, "s")).toBe("-");
    });

    it("renders a byte rate with a per-second suffix", () => {
        expect(formatMetricValue(1536, "bytes/s")).toBe("1.5 KiB/s");
    });

    it("renders a ratio as a percentage with one decimal", () => {
        expect(formatMetricValue(0.4567, "ratio")).toBe("45.7%");
    });

    it("renders sub-second durations in milliseconds", () => {
        expect(formatMetricValue(0.234, "s")).toBe("234 ms");
    });

    it("renders one-second-or-longer durations in seconds", () => {
        expect(formatMetricValue(1.5, "s")).toBe("1.50 s");
    });

    it("renders large plain numbers with no decimals", () => {
        expect(formatMetricValue(1234, "req/s")).toBe("1234");
    });

    it("renders small plain numbers with two decimals", () => {
        expect(formatMetricValue(4.2, "req/s")).toBe("4.20");
    });
});

describe("colorFor", () => {
    it("returns a stable color for the same index", () => {
        expect(colorFor(0)).toBe(colorFor(0));
    });

    it("cycles once the palette is exhausted", () => {
        // The palette has 8 entries - index 8 must wrap back to index 0's color,
        // so two different tests eventually reuse a color rather than the
        // function throwing/returning undefined past the palette's length.
        expect(colorFor(8)).toBe(colorFor(0));
    });
});

describe("metricsAnchorFromRuns", () => {
    it("returns undefined with no run history", () => {
        expect(metricsAnchorFromRuns([])).toBeUndefined();
    });

    it("returns undefined when the latest run is still in flight", () => {
        const runs = [{ status: { startTime: "2026-09-15T12:00:00Z" } }];
        expect(metricsAnchorFromRuns(runs)).toBeUndefined();
    });

    it("anchors to the latest completed run, not just the last in the list", () => {
        const older = {
            status: { startTime: "2026-09-15T10:00:00Z", completionTime: "2026-09-15T10:05:00Z" },
        };
        const newer = {
            status: { startTime: "2026-09-15T12:00:00Z", completionTime: "2026-09-15T12:05:00Z" },
        };
        // Deliberately listed out of chronological order - anchoring must sort
        // by completionTime itself, not assume array order.
        const anchor = metricsAnchorFromRuns([older, newer]);
        const wantEnd = Math.floor(Date.parse(newer.status.completionTime) / 1000) + 60;
        expect(anchor?.endSeconds).toBe(wantEnd);
    });

    it("pads the range so the run's own start/end both fall inside the window", () => {
        const run = {
            status: { startTime: "2026-09-15T12:00:00Z", completionTime: "2026-09-15T12:05:00Z" },
        };
        const anchor = metricsAnchorFromRuns([run]);
        expect(anchor).toBeDefined();
        const startSeconds = anchor!.endSeconds - anchor!.rangeSeconds;
        expect(startSeconds).toBeLessThanOrEqual(
            Math.floor(Date.parse(run.status.startTime) / 1000),
        );
        // A floor: even a very short run gets at least a 900s window, matching
        // the function's own documented minimum.
        expect(anchor!.rangeSeconds).toBeGreaterThanOrEqual(900);
    });

    it("falls back to a default lookback when startTime is missing", () => {
        const run = { status: { completionTime: "2026-09-15T12:05:00Z" } };
        const anchor = metricsAnchorFromRuns([run]);
        // No startTime -> the function's own 900s default lookback, plus its
        // fixed 120s padding: 900 + 120 = 1020, not the 900s floor itself.
        expect(anchor!.rangeSeconds).toBe(1020);
    });
});

describe("formatDuration", () => {
    it("renders seconds and minutes as a single unit", () => {
        expect(formatDuration(42)).toBe("42s");
        expect(formatDuration(7 * 60 + 5)).toBe("7m");
    });

    it("renders hours and days as their two largest units", () => {
        expect(formatDuration(19 * 3600 + 12 * 60)).toBe("19h 12m");
        expect(formatDuration(3 * 86400 + 4 * 3600 + 59)).toBe("3d 4h");
    });

    it("clamps a negative duration (clock skew) to zero", () => {
        expect(formatDuration(-5)).toBe("0s");
    });
});

describe("relativeDelta", () => {
    it("is the change relative to the baseline", () => {
        expect(relativeDelta(0.362, 0.421)).toBeCloseTo(-0.14, 3);
        expect(relativeDelta(194, 181)).toBeCloseTo(0.0718, 3);
    });

    it("is undefined without both values or with a zero baseline", () => {
        expect(relativeDelta(undefined, 1)).toBeUndefined();
        expect(relativeDelta(1, undefined)).toBeUndefined();
        expect(relativeDelta(1, 0)).toBeUndefined();
    });
});

describe("stepFor", () => {
    it("never goes finer than 15s", () => {
        expect(stepFor(900)).toBe("15s");
    });

    it("keeps long ranges to about 240 points", () => {
        expect(stepFor(7 * 86400)).toBe("2520s");
    });
});

describe("targetNameOf", () => {
    it("prefers spec.targetName and falls back to the object name", () => {
        expect(targetNameOf({ metadata: { name: "ak-a" }, spec: { targetName: "a" } })).toBe("a");
        expect(targetNameOf({ metadata: { name: "ak-a" }, spec: {} })).toBe("ak-a");
    });
});

describe("shortTag", () => {
    it("abbreviates a trailing full commit hash", () => {
        expect(shortTag("gh-ccd5f45863edfb003c4dd876fabfcb0123456789")).toBe("gh-ccd5f45");
    });

    it("leaves other tags alone", () => {
        expect(shortTag("2026.8.2")).toBe("2026.8.2");
        expect(shortTag("gh-main")).toBe("gh-main");
    });
});

describe("readableTextOn", () => {
    it("uses white on dark colors and ink on light ones", () => {
        expect(readableTextOn("#0066cc")).toBe("#ffffff");
        expect(readableTextOn("#f0ab00")).toBe("#151515");
    });
});

describe("buildLabel", () => {
    it("appends the short commit to a release version", () => {
        expect(buildLabel({ version: "2026.10.1", commit: "0123456789abcdef" })).toBe(
            "2026.10.1 · 0123456",
        );
    });

    it("does not repeat a commit the version already carries", () => {
        expect(buildLabel({ version: "0.0.0-main.0123456", commit: "0123456789abcdef" })).toBe(
            "0.0.0-main.0123456",
        );
    });

    it("marks a build from a tree with local changes", () => {
        expect(buildLabel({ version: "dev", commit: "0123456789abcdef", dirty: true })).toBe(
            "dev · 0123456-dirty",
        );
    });

    it("shows just the version without a commit", () => {
        expect(buildLabel({ version: "dev" })).toBe("dev");
    });
});

describe("onlyTargets", () => {
    const data = {
        series: [
            { testid: "pr-compare", target: "ak", points: [] },
            { testid: "pr-compare", target: "ak-pr-123", points: [] },
        ],
    };

    it("drops other deployments' series", () => {
        expect(onlyTargets(data, ["ak"])?.series.map((s) => s.target)).toEqual(["ak"]);
    });

    it("keeps everything without a target filter", () => {
        expect(onlyTargets(data, undefined)).toBe(data);
        expect(onlyTargets(undefined, ["ak"])).toBeUndefined();
    });
});

describe("parseTimeRange", () => {
    it("accepts known ranges and falls back otherwise", () => {
        expect(parseTimeRange("7d", "15m")).toBe("7d");
        expect(parseTimeRange(null, "15m")).toBe("15m");
        expect(parseTimeRange("13m", "15m")).toBe("15m");
    });

    it("maps the seconds older links carry", () => {
        expect(parseTimeRange("21600", "15m")).toBe("6h");
    });

    it('only keeps "since start" while a start is known', () => {
        expect(parseTimeRange(SINCE_START, "15m", 1000)).toBe(SINCE_START);
        expect(parseTimeRange(SINCE_START, "15m")).toBe("15m");
    });
});

describe("timeRangeSeconds", () => {
    it("resolves a relative range", () => {
        expect(timeRangeSeconds("90d")).toBe(90 * 86400);
    });

    it('spans "since start" with a minute of lead-in, rounded up to 5 minutes', () => {
        expect(timeRangeSeconds(SINCE_START, 10_000, 10_000 + 3600)).toBe(3900);
        expect(timeRangeSeconds(SINCE_START, 10_000, 10_010)).toBe(300);
    });
});

describe("earliestBurnStart", () => {
    const testCases = [
        {
            status: {
                deployments: [{ name: "a", burningSince: "2026-10-14T10:00:00Z" }, { name: "b" }],
            },
        },
        { status: { deployments: [{ name: "b", burningSince: "2026-10-14T12:00:00Z" }] } },
    ];

    it("picks the earliest burn, optionally for one deployment", () => {
        expect(earliestBurnStart(testCases)).toBe(Date.parse("2026-10-14T10:00:00Z") / 1000);
        expect(earliestBurnStart(testCases, "b")).toBe(Date.parse("2026-10-14T12:00:00Z") / 1000);
        expect(earliestBurnStart(testCases, "c")).toBeUndefined();
    });
});

describe("archivedBurnWindow", () => {
    const s = (t: string) => Date.parse(t) / 1000;

    it("spans from the earliest burn start to the latest burn stop", () => {
        const tc = {
            spec: { archived: true },
            status: {
                deployments: [
                    { burningSince: "2026-10-14T10:00:00Z", burnStoppedAt: "2026-10-16T00:00:00Z" },
                    { burningSince: "2026-10-14T12:00:00Z", burnStoppedAt: "2026-10-16T00:01:00Z" },
                ],
            },
        };
        expect(archivedBurnWindow(tc)).toEqual({
            start: s("2026-10-14T10:00:00Z"),
            end: s("2026-10-16T00:01:00Z"),
        });
        expect(earliestBurnStart([tc as never])).toBeUndefined();
    });

    it("falls back to when the test was archived for the end", () => {
        const tc = {
            spec: { archived: true },
            status: {
                conditions: [
                    {
                        type: "Ready",
                        reason: "Archived",
                        lastTransitionTime: "2026-10-16T00:37:20Z",
                    },
                ],
                deployments: [{}],
            },
        };
        expect(archivedBurnWindow(tc)).toEqual({
            start: undefined,
            end: s("2026-10-16T00:37:20Z"),
        });
    });

    it("is undefined for an active test", () => {
        expect(
            archivedBurnWindow({
                spec: {},
                status: { deployments: [{ burningSince: "2026-10-14T10:00:00Z" }] },
            }),
        ).toBeUndefined();
    });
});
