import { api } from "../api";
import { RestartsResponse } from "../types";
import { useResource } from "../useResource";
import { Alert } from "./ui";

type RestartsTarget = RestartsResponse["targets"][number];

/**
 * restartSummary describes what happened to one target's containers, e.g.
 * "server restarted ×2, worker in 1 new pod", or "" if it stayed up.
 */
export function restartSummary(target: RestartsTarget): string {
    const restarted = Object.entries(target.restarts).map(
        ([container, count]) => `${container} restarted ×${count}`,
    );
    const started = Object.entries(target.started ?? {}).map(
        ([container, count]) => `${container} in ${count} new pod${count === 1 ? "" : "s"}`,
    );
    return [...restarted, ...started].join(", ");
}

/**
 * RestartsResult is one fetch of the restarts, tagged with the request it
 * answers (restartsKey) and holding either the response or why it failed.
 */
export interface RestartsResult {
    key: string;
    data?: RestartsResponse;
    error?: string;
}

/** restartsKey identifies a restarts request by everything it asks about. */
export function restartsKey(testids: string[], period: string, endSeconds?: number): string {
    return [testids.join(","), period, endSeconds ?? "now"].join("|");
}

/**
 * restartsNotes picks what to show from the latest fetch: nothing if it
 * answers a different request than key (useResource keeps the previous
 * request's result until the new one lands, and toggling a test must not
 * show the old selection's warning), and only the error if it failed, so a
 * failed poll doesn't sit next to the restarts an earlier one found.
 */
export function restartsNotes(
    result: RestartsResult | undefined,
    key: string,
): { restarted: RestartsTarget[]; unchecked: string[] } {
    if (!result || result.key !== key) {
        return { restarted: [], unchecked: [] };
    }
    if (result.error !== undefined) {
        return { restarted: [], unchecked: [result.error] };
    }
    return {
        restarted: (result.data?.targets ?? []).filter((target) => restartSummary(target)),
        // Said, not left out: a target that could not be checked must not
        // read the same as one that stayed up.
        unchecked: Object.entries(result.data?.errors ?? {}).map(
            ([testid, err]) => `${testid}: ${err}`,
        ),
    };
}

/**
 * RestartsNotes renders restartsNotes' result: a warning naming the targets
 * that restarted and a note naming what could not be checked. Renders
 * nothing while every target stayed up.
 */
export function RestartsNotes({
    restarted,
    unchecked,
}: {
    restarted: RestartsTarget[];
    unchecked: string[];
}) {
    if (restarted.length === 0 && unchecked.length === 0) {
        return null;
    }
    return (
        <>
            {restarted.length > 0 && (
                <Alert>
                    <strong>Containers restarted or were replaced during this window.</strong> The
                    numbers for these targets include downtime and cold starts, so they do not
                    compare with a target that stayed up.
                    <ul>
                        {restarted.map((target) => (
                            <li key={`${target.testid}::${target.target}`}>
                                <span className="mono">{target.target || target.testid}</span>:{" "}
                                {restartSummary(target)}
                            </li>
                        ))}
                    </ul>
                </Alert>
            )}
            {unchecked.length > 0 && (
                <Alert kind="info">
                    Could not check every target for container restarts, so one may have restarted
                    unnoticed: {unchecked.join("; ")}
                </Alert>
            )}
        </>
    );
}

/**
 * RestartsWarning warns when a container of one of the tests' deployments
 * restarted within the window the surrounding numbers cover. A target that
 * is being restarted reports throughput and latency that include its
 * downtime and cold starts, so they do not compare with a target that stayed
 * up (internal/api/restarts.go). Renders nothing while every target stayed up.
 */
export function RestartsWarning({
    testids,
    period,
    endSeconds,
}: {
    testids: string[];
    // The stretch of time the surrounding numbers cover, as a Prometheus duration.
    period: string;
    endSeconds?: number;
}) {
    const key = restartsKey(testids, period, endSeconds);
    // The error is kept in the result rather than left to useResource, so it
    // is tagged with the request it belongs to like the data is.
    const restarts = useResource<RestartsResult | undefined>(
        () =>
            testids.length
                ? api.restarts(testids, period, endSeconds).then(
                      (data) => ({ key, data }),
                      (err: unknown) => ({
                          key,
                          error: err instanceof Error ? err.message : String(err),
                      }),
                  )
                : Promise.resolve(undefined),
        [key],
        30000,
    );
    return <RestartsNotes {...restartsNotes(restarts.data, key)} />;
}
