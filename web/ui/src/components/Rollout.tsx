// What is happening on the cluster while deployments roll out: one grid of
// the objects (an optional test case, each deployment and its namespace's
// pods, and why each isn't ready yet) and one stream of their events - image
// pulls, scheduling, probe failures - where the Ready condition alone only
// says "still installing".
import { api } from "../api";
import { AuthentikDeployment, ObjectEvent, RolloutPod, TestCase } from "../types";
import { useResource } from "../useResource";
import { formatLogTime } from "./ActivityLog";
import { StatusBadge } from "./Status";
import { errorMessage } from "./ui";

import { Fragment } from "react";
import { Link } from "react-router";

const MAX_EVENTS = 50;

function podBadge(p: RolloutPod): { label: string; cls: string } {
    const [ready, total] = p.ready.split("/");
    if (!p.reason && (p.phase === "Succeeded" || (p.phase === "Running" && ready === total))) {
        return { label: p.phase === "Succeeded" ? "Completed" : "Ready", cls: "ok" };
    }
    const label = p.reason ?? p.phase;
    return { label, cls: /BackOff|Err|Error|Failed|OOM/.test(label) ? "err" : "info" };
}

export function Rollout({
    deployments,
    testCase,
}: {
    deployments: AuthentikDeployment[];
    /** Also show the test case driving these deployments, and its events. */
    testCase?: TestCase;
}) {
    const names = deployments.map((d) => d.metadata.name);
    const tcName = testCase?.metadata.name;
    const ready = deployments.every((d) => d.status?.phase === "Ready");
    // Poll faster while something is still rolling out.
    const rollouts = useResource(
        () => Promise.allSettled(names.map((n) => api.rollout(n))),
        [names.join(",")],
        ready ? 30000 : 5000,
    );
    const tcEvents = useResource(
        () => (tcName ? api.objectEvents("testcases", tcName) : Promise.resolve([])),
        [tcName],
        10000,
    );

    const events: (ObjectEvent & { object: string })[] = [
        ...(tcEvents.data ?? []).map((ev) => ({ ...ev, object: `testcase/${tcName}` })),
        ...(rollouts.data ?? []).flatMap((r) => (r.status === "fulfilled" ? r.value.events : [])),
    ]
        .sort((a, b) => (b.lastSeen ?? "").localeCompare(a.lastSeen ?? ""))
        .slice(0, MAX_EVENTS);

    return (
        <div className="activity">
            <h3>Rollout</h3>
            <table className="table">
                <thead>
                    <tr>
                        <th>Object</th>
                        <th>Status</th>
                        <th>Ready</th>
                        <th>Restarts</th>
                        <th>Node</th>
                        <th>Age</th>
                    </tr>
                </thead>
                <tbody>
                    {testCase && (
                        <tr>
                            <td className="mono">testcase/{tcName}</td>
                            <td>
                                <StatusBadge
                                    kind="testcase"
                                    name={testCase.metadata.name}
                                    namespace={testCase.metadata.namespace}
                                    phase={testCase.status?.phase}
                                    conditions={testCase.status?.conditions}
                                />
                            </td>
                            <td colSpan={3} />
                            <td className="mono">
                                {formatLogTime(testCase.metadata.creationTimestamp)}
                            </td>
                        </tr>
                    )}
                    {deployments.map((d, i) => {
                        const result = rollouts.data?.[i];
                        const r = result?.status === "fulfilled" ? result.value : undefined;
                        return (
                            <Fragment key={d.metadata.name}>
                                <tr>
                                    <td className="mono">
                                        <Link to={`/deployments/${d.metadata.name}`}>
                                            deployment/{d.metadata.name}
                                        </Link>
                                        {r && (
                                            <span className="muted">
                                                {" "}
                                                {r.cluster} / {r.namespace}
                                            </span>
                                        )}
                                    </td>
                                    <td>
                                        <StatusBadge
                                            kind="authentikdeployment"
                                            name={d.metadata.name}
                                            namespace={d.metadata.namespace}
                                            phase={d.status?.phase}
                                            conditions={d.status?.conditions}
                                        />
                                        {result?.status === "rejected" && (
                                            <span className="muted">
                                                {" "}
                                                {errorMessage(result.reason)}
                                            </span>
                                        )}
                                    </td>
                                    <td colSpan={3}>
                                        {r?.pods.length === 0 && (
                                            <span className="muted">No pods yet.</span>
                                        )}
                                    </td>
                                    <td className="mono">
                                        {formatLogTime(d.metadata.creationTimestamp)}
                                    </td>
                                </tr>
                                {r?.pods.map((p) => {
                                    const badge = podBadge(p);
                                    return (
                                        <tr key={p.name}>
                                            <td className="mono" style={{ paddingLeft: 28 }}>
                                                {p.name}
                                            </td>
                                            <td>
                                                <span className={`badge ${badge.cls}`}>
                                                    {badge.label}
                                                </span>
                                                {p.message && (
                                                    <span className="muted"> {p.message}</span>
                                                )}
                                            </td>
                                            <td className="mono">{p.ready}</td>
                                            <td className="mono">{p.restarts}</td>
                                            <td className="mono">{p.node ?? "-"}</td>
                                            <td className="mono">{formatLogTime(p.created)}</td>
                                        </tr>
                                    );
                                })}
                            </Fragment>
                        );
                    })}
                </tbody>
            </table>
            {tcEvents.error && <div className="alert error">{tcEvents.error}</div>}
            {events.length > 0 && (
                <ol className="log" style={{ marginTop: 8 }}>
                    {events.map((ev, i) => (
                        <li
                            key={`${ev.lastSeen}-${ev.object}-${ev.reason}-${i}`}
                            className={ev.type === "Warning" ? "warning" : undefined}
                        >
                            <time dateTime={ev.lastSeen}>{formatLogTime(ev.lastSeen)}</time>
                            <span className="reason">{ev.reason}</span>
                            <span className="message">
                                <span className="muted">{ev.object}: </span>
                                {ev.message}
                                {ev.count > 1 && <span className="count"> ×{ev.count}</span>}
                            </span>
                        </li>
                    ))}
                </ol>
            )}
        </div>
    );
}
