// An object's Kubernetes Events as a log, newest first - what `kubectl
// describe` shows at the bottom. Every controller emits one per condition it
// sets, so this reads as a history of what the operator did, where the
// conditions themselves only show the latest state.
import { api, ResourcePath } from "../api";
import { ObjectEvent } from "../types";
import { useResource } from "../useResource";

export function formatLogTime(value: string | undefined): string {
    if (!value) {
        return "-";
    }
    const date = new Date(value);
    const sameDay = date.toDateString() === new Date().toDateString();
    return sameDay
        ? date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
        : date.toLocaleString([], {
              month: "short",
              day: "numeric",
              hour: "2-digit",
              minute: "2-digit",
          });
}

export function ActivityLog({
    path,
    name,
    limit = 50,
}: {
    path: ResourcePath;
    name: string;
    limit?: number;
}) {
    const events = useResource(() => api.objectEvents(path, name), [path, name], 10000);
    const items = (events.data ?? []).slice(0, limit);

    return (
        <div className="activity">
            <h3>Activity</h3>
            {events.error && <div className="alert error">{events.error}</div>}
            {events.data && items.length === 0 ? (
                <div className="muted">
                    No events in the last hour. Kubernetes keeps events for about an hour by
                    default.
                </div>
            ) : (
                <ol className="log">
                    {items.map((ev: ObjectEvent, i) => (
                        <li
                            key={`${ev.lastSeen}-${ev.reason}-${i}`}
                            className={ev.type === "Warning" ? "warning" : undefined}
                        >
                            <time
                                dateTime={ev.lastSeen}
                                title={
                                    ev.firstSeen !== ev.lastSeen
                                        ? `First seen ${formatLogTime(ev.firstSeen)}`
                                        : undefined
                                }
                            >
                                {formatLogTime(ev.lastSeen)}
                            </time>
                            <span className="reason">{ev.reason}</span>
                            <span className="message">
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
