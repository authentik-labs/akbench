// What is happening on the cluster while a deployment rolls out: its
// conditions, its namespace's pods (and why each isn't ready yet) and the
// namespace's events - image pulls, scheduling, probe failures - all in one
// place, where the Ready condition alone only says "still installing".
import { api } from "../api";
import { AuthentikDeployment, RolloutPod } from "../types";
import { useResource } from "../useResource";
import { formatLogTime } from "./ActivityLog";
import { ConditionList, StatusBadge } from "./Status";

import { Link } from "react-router";

function podBadge(p: RolloutPod): { label: string; cls: string } {
    const [ready, total] = p.ready.split("/");
    if (!p.reason && (p.phase === "Succeeded" || (p.phase === "Running" && ready === total))) {
        return { label: p.phase === "Succeeded" ? "Completed" : "Ready", cls: "ok" };
    }
    const label = p.reason ?? p.phase;
    return { label, cls: /BackOff|Err|Error|Failed|OOM/.test(label) ? "err" : "info" };
}

export function Rollout({
    deployment,
    showConditions,
}: {
    deployment: AuthentikDeployment;
    /** Also show the deployment's own conditions, for pages that don't already. */
    showConditions?: boolean;
}) {
    const name = deployment.metadata.name;
    const ready = deployment.status?.phase === "Ready";
    // Poll faster while it's still rolling out.
    const rollout = useResource(() => api.rollout(name), [name], ready ? 30000 : 5000);
    const r = rollout.data;

    return (
        <div className="activity">
            <h3>
                {showConditions ? (
                    <>
                        <Link to={`/deployments/${name}`}>{name}</Link>{" "}
                        <StatusBadge
                            kind="authentikdeployment"
                            name={name}
                            namespace={deployment.metadata.namespace}
                            phase={deployment.status?.phase}
                            conditions={deployment.status?.conditions}
                        />
                    </>
                ) : (
                    "Rollout"
                )}{" "}
                {r && (
                    <span className="muted">
                        {r.cluster} / {r.namespace}
                    </span>
                )}
            </h3>
            {showConditions && <ConditionList conditions={deployment.status?.conditions} />}
            {rollout.error && <div className="alert error">{rollout.error}</div>}
            {r &&
                (r.pods.length === 0 ? (
                    <div className="muted">No pods yet.</div>
                ) : (
                    <table className="table" style={{ marginTop: 8 }}>
                        <thead>
                            <tr>
                                <th>Pod</th>
                                <th>Status</th>
                                <th>Ready</th>
                                <th>Restarts</th>
                                <th>Node</th>
                                <th>Age</th>
                            </tr>
                        </thead>
                        <tbody>
                            {r.pods.map((p) => {
                                const badge = podBadge(p);
                                return (
                                    <tr key={p.name}>
                                        <td className="mono">{p.name}</td>
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
                        </tbody>
                    </table>
                ))}
            {r && r.events.length > 0 && (
                <ol className="log" style={{ marginTop: 8 }}>
                    {r.events.map((ev, i) => (
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
