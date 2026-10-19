import { MANAGEMENT_CLUSTER } from "../api";
import { shellLink } from "../shell/links";
import { AddonStatus, Condition, readyCondition } from "../types";

// A phase/Ready badge that opens the object's conditions on click. Events
// aren't served by cmd/apiserver - `kubectl describe` in the shell is one
// click away for those (the popover links there).
import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";

function phaseClass(phase?: string): string {
    switch (phase) {
        case "Ready":
        case "Succeeded":
        case "Provisioned":
        case "True":
            return "ok";
        case "Failed":
        case "False":
            return "err";
        case "Not ready":
            return "warn";
        case "Waiting":
            return "warn";
        case "Reconciling":
        case "Progressing":
        case "Running":
        case "Provisioning":
        case "Pending":
            return "info";
        default:
            return "neutral";
    }
}

export function Badge({ value }: { value: string | undefined }) {
    return <span className={`badge ${phaseClass(value)}`}>{value ?? "Unknown"}</span>;
}

export function StatusBadge({
    kind,
    name,
    namespace,
    phase,
    conditions,
}: {
    /** The CRD resource name for kubectl, e.g. "authentikdeployments". */
    kind: string;
    name: string;
    /** Where the object itself lives on the management cluster. */
    namespace?: string;
    phase?: string;
    conditions?: Condition[];
}) {
    const [open, setOpen] = useState(false);
    const ref = useRef<HTMLSpanElement>(null);
    const label = phase ?? readyCondition(conditions)?.status ?? "Unknown";

    useEffect(() => {
        if (!open) {
            return;
        }
        const close = (e: MouseEvent) => {
            if (ref.current && !ref.current.contains(e.target as Node)) {
                setOpen(false);
            }
        };
        document.addEventListener("mousedown", close);
        return () => document.removeEventListener("mousedown", close);
    }, [open]);

    return (
        <span className="popover-anchor" ref={ref}>
            <button
                type="button"
                className="linkish"
                onClick={() => setOpen((o) => !o)}
                title="Show conditions"
            >
                <Badge value={label} />
            </button>
            {open && (
                <div className="popover">
                    <h3>Conditions</h3>
                    <ConditionList conditions={conditions} />
                    <div style={{ marginTop: 10 }}>
                        <Link
                            to={shellLink(
                                MANAGEMENT_CLUSTER,
                                `kubectl describe ${kind} ${name}${namespace ? ` -n ${namespace}` : ""}`,
                                namespace,
                            )}
                        >
                            Describe in shell (events) →
                        </Link>
                    </div>
                </div>
            )}
        </span>
    );
}

export function ConditionList({ conditions }: { conditions?: Condition[] }) {
    if (!conditions?.length) {
        return <div className="muted">No conditions reported yet.</div>;
    }
    return (
        <div className="conditions">
            {conditions.map((c) => (
                <div className="cond" key={c.type}>
                    <Badge
                        value={
                            c.status === "True"
                                ? "True"
                                : c.status === "False"
                                  ? "False"
                                  : "Unknown"
                        }
                    />
                    <span className="cond-type">{c.type}</span>
                    <span>
                        {c.reason}
                        {c.message ? `: ${c.message}` : ""}
                    </span>
                </div>
            ))}
        </div>
    );
}

// A workload cluster's add-ons (status.addons) - CNI, storage, monitoring,
// ... - each with its own state, since one stuck add-on (say a Helm install
// failing) otherwise only shows up as a line in the activity log.
export function AddonList({
    addons,
    controlPlaneReady,
}: {
    addons?: AddonStatus[];
    controlPlaneReady?: boolean;
}) {
    const ready = (addons ?? []).filter((a) => a.state === "Ready").length;
    return (
        <div className="activity">
            <h3>
                Add-ons{" "}
                {addons?.length ? (
                    <span className="muted">
                        {ready}/{addons.length} ready
                    </span>
                ) : null}
            </h3>
            {!addons?.length ? (
                <div className="muted">
                    {controlPlaneReady
                        ? "Not reported yet."
                        : "Installed once the control plane is ready."}
                </div>
            ) : (
                <div className="conditions">
                    {addons.map((a) => (
                        <div className="cond" key={a.name}>
                            <Badge value={a.state} />
                            <span
                                className="cond-type"
                                title={
                                    a.lastTransitionTime
                                        ? `Since ${new Date(a.lastTransitionTime).toLocaleString()}`
                                        : undefined
                                }
                            >
                                {a.name}
                            </span>
                            <span className={a.state === "Ready" ? "muted" : undefined}>
                                {a.message ?? ""}
                            </span>
                        </div>
                    ))}
                </div>
            )}
        </div>
    );
}
