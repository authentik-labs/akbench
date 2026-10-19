import { api, MANAGEMENT_CLUSTER, ShellContext } from "../api";
import { Alert } from "../components/ui";
import { useResource } from "../useResource";
import { sanitizeNamespace, sanitizePrefill } from "./links";
import { SessionStatus, TerminalSession } from "./TerminalSession";

// The kubectl shell: tabs of real shells, one backend process each, against
// any context in the operator's merged kubeconfig (the management cluster
// plus every Ready WorkloadCluster). Mounted by Layout for the app's whole
// lifetime once first opened, and merely hidden on other pages, so running
// shells (a `kubectl logs -f`, say) survive navigating around the app.
// Closing a tab - or the browser tab - ends its shell on the backend.
import { useCallback, useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router";

interface Session {
    id: number;
    context: string;
    namespace?: string;
    prefill?: string;
    status: SessionStatus;
}

let nextID = 1;

function shortName(context: string): string {
    // CAPI names workload contexts <cluster>-admin@<cluster>.
    const at = context.indexOf("-admin@");
    return at === -1 ? context : context.slice(at + "-admin@".length);
}

/**
 * resolveContext maps the page's ?context= / ?cluster= params onto a real
 * context name, or undefined if none matches.
 */
function resolveContext(contexts: ShellContext[], params: URLSearchParams): string | undefined {
    const byName = params.get("context");
    if (byName) {
        return contexts.find((c) => c.name === byName)?.name;
    }
    const cluster = params.get("cluster");
    if (cluster) {
        return contexts.find((c) => c.cluster === cluster)?.name;
    }
    return undefined;
}

export function ShellWorkspace({ visible }: { visible: boolean }) {
    const location = useLocation();
    const navigate = useNavigate();
    const contexts = useResource(() => api.shellContexts(), [], 30000);
    const [sessions, setSessions] = useState<Session[]>([]);
    const [activeID, setActiveID] = useState<number>();
    const [picked, setPicked] = useState(MANAGEMENT_CLUSTER);
    const [pickedNamespace, setPickedNamespace] = useState("");
    const [linkError, setLinkError] = useState<string>();

    const open = useCallback((context: string, namespace?: string, prefill?: string) => {
        const id = nextID++;
        setSessions((s) => [...s, { id, context, namespace, prefill, status: "connecting" }]);
        setActiveID(id);
    }, []);

    // Deep links (shell/links.ts) open a new tab, then the URL is reduced to
    // plain /shell so a reload or revisit doesn't open it again.
    useEffect(() => {
        if (location.pathname !== "/shell" || !contexts.data) {
            return;
        }
        const params = new URLSearchParams(location.search);
        if (!params.has("context") && !params.has("cluster")) {
            if (sessions.length === 0 && contexts.data.length > 0) {
                open(contexts.data[0].name);
            }
            return;
        }
        const context = resolveContext(contexts.data, params);
        if (context) {
            setLinkError(undefined);
            open(
                context,
                sanitizeNamespace(params.get("namespace")),
                sanitizePrefill(params.get("prefill")) || undefined,
            );
        } else {
            setLinkError(
                `No kubeconfig context for ${params.get("context") ?? `cluster "${params.get("cluster")}"`} - ` +
                    "a WorkloadCluster only appears here once it is Ready.",
            );
        }
        navigate("/shell", { replace: true });
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [location.pathname, location.search, contexts.data]);

    const close = (id: number) => {
        setSessions((s) => {
            const rest = s.filter((x) => x.id !== id);
            if (id === activeID) {
                setActiveID(rest.at(-1)?.id);
            }
            return rest;
        });
    };

    const setStatus = useCallback((id: number, status: SessionStatus) => {
        setSessions((s) =>
            s.map((x) => (x.id === id && x.status !== status ? { ...x, status } : x)),
        );
    }, []);

    return (
        <div className="shell-panes" style={{ display: visible ? "flex" : "none" }}>
            <div className="shell-bar">
                <div className="shell-tabs">
                    {sessions.map((s) => (
                        <span key={s.id} className="row" style={{ gap: 0 }}>
                            <button
                                className={`small ${s.id === activeID ? "active" : ""}`}
                                onClick={() => setActiveID(s.id)}
                                title={s.namespace ? `${s.context} (${s.namespace})` : s.context}
                            >
                                {s.status === "connected"
                                    ? "●"
                                    : s.status === "connecting"
                                      ? "○"
                                      : "✕"}{" "}
                                {shortName(s.context)}
                                {s.namespace && <span className="muted"> {s.namespace}</span>}
                            </button>
                            <button
                                className="small linkish"
                                title="Close (ends the shell)"
                                onClick={() => close(s.id)}
                            >
                                ×
                            </button>
                        </span>
                    ))}
                </div>
                <span className="row" style={{ marginLeft: "auto" }}>
                    <select
                        value={picked}
                        onChange={(e) => setPicked(e.target.value)}
                        aria-label="Context"
                    >
                        {(contexts.data ?? []).map((c) => (
                            <option key={c.name} value={c.name}>
                                {c.name}
                            </option>
                        ))}
                    </select>
                    <input
                        value={pickedNamespace}
                        onChange={(e) => setPickedNamespace(e.target.value)}
                        placeholder="namespace (optional)"
                        aria-label="Namespace"
                        aria-invalid={
                            pickedNamespace.trim() !== "" && !sanitizeNamespace(pickedNamespace)
                        }
                        title="Start the shell with this as the context's default namespace"
                        spellCheck={false}
                        autoCapitalize="off"
                        size={20}
                    />
                    <button
                        className="small primary"
                        onClick={() => open(picked, sanitizeNamespace(pickedNamespace))}
                        disabled={
                            !contexts.data?.length ||
                            (pickedNamespace.trim() !== "" && !sanitizeNamespace(pickedNamespace))
                        }
                    >
                        New shell
                    </button>
                </span>
            </div>
            {(contexts.error || linkError) && (
                <div style={{ padding: "8px 12px 0" }}>
                    <Alert>{linkError ?? contexts.error}</Alert>
                </div>
            )}
            {sessions.length === 0 && !contexts.error && (
                <div className="muted" style={{ padding: 16 }}>
                    No shells open. Pick a context and start one.
                </div>
            )}
            {sessions.map((s) => (
                <div key={s.id} className={`shell-pane ${s.id === activeID ? "active" : ""}`}>
                    <TerminalSession
                        context={s.context}
                        namespace={s.namespace}
                        prefill={s.prefill}
                        active={visible && s.id === activeID}
                        onStatus={(status) => setStatus(s.id, status)}
                    />
                </div>
            ))}
        </div>
    );
}
