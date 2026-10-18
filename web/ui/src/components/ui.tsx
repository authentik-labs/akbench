import { SINCE_START, TIME_RANGES } from "../format";
import { confirm, type ConfirmOptions } from "./Confirm";
import { toast } from "./Toast";

// The handful of layout/feedback primitives every page shares.
import { ReactNode } from "react";

export function Page({
    title,
    subtitle,
    actions,
    children,
}: {
    title: ReactNode;
    subtitle?: ReactNode;
    actions?: ReactNode;
    children: ReactNode;
}) {
    return (
        <>
            <div className="page-head">
                <div>
                    <h1>{title}</h1>
                    {subtitle && <div className="sub">{subtitle}</div>}
                </div>
                {actions && <div className="row">{actions}</div>}
            </div>
            {children}
        </>
    );
}

/**
 * Section is one titled card of a long page: a header bar with the
 * section's own controls, then its body. id is its SectionNav anchor.
 */
export function Section({
    id,
    title,
    description,
    actions,
    loading,
    children,
}: {
    id: string;
    title: ReactNode;
    description?: ReactNode;
    actions?: ReactNode;
    /**
     * loading marks the body as being (re)fetched: a spinner in the header,
     * and whatever the body still shows from before dimmed until it's back.
     */
    loading?: boolean;
    children: ReactNode;
}) {
    return (
        <section
            className="section"
            id={id}
            aria-labelledby={`${id}-title`}
            aria-busy={loading || undefined}
        >
            <header className="section-bar">
                <div>
                    <h2 id={`${id}-title`} className="row">
                        {title}
                        {loading && <Spinner />}
                    </h2>
                    {description && <div className="muted">{description}</div>}
                </div>
                {actions && <div className="row">{actions}</div>}
            </header>
            <div className={loading ? "section-body stale" : "section-body"}>{children}</div>
        </section>
    );
}

/**
 * SectionNav jumps between a page's Sections. It scrolls rather than
 * linking to #id, so the page's own search params and history stay put.
 */
export function SectionNav({ sections }: { sections: { id: string; label: string }[] }) {
    return (
        <nav className="section-nav" aria-label="Sections">
            {sections.map((s) => (
                <button
                    key={s.id}
                    type="button"
                    onClick={() =>
                        document
                            .getElementById(s.id)
                            ?.scrollIntoView({ behavior: "smooth", block: "start" })
                    }
                >
                    {s.label}
                </button>
            ))}
        </nav>
    );
}

export function Alert({
    kind = "error",
    children,
}: {
    kind?: "error" | "info" | "success";
    children: ReactNode;
}) {
    if (!children) {
        return null;
    }
    return <div className={`alert ${kind}`}>{children}</div>;
}

/** Spinner is a small inline busy indicator, sized to the surrounding text. */
export function Spinner({ label = "Loading" }: { label?: string }) {
    return <span className="spinner" role="status" aria-label={label} />;
}

/**
 * Loading is the placeholder for a block whose data hasn't arrived yet.
 * children says what it's waiting on, for the slow ones ("Querying Tempo…").
 */
export function Loading({ children = "Loading…" }: { children?: ReactNode }) {
    return (
        <div className="loading">
            <Spinner />
            <span>{children}</span>
        </div>
    );
}

export function Empty({ children }: { children: ReactNode }) {
    return <div className="empty">{children}</div>;
}

export interface Column<T> {
    label: string;
    render: (row: T) => ReactNode;
    className?: string;
}

export function Table<T>({
    columns,
    rows,
    rowKey,
    empty,
}: {
    columns: Column<T>[];
    rows: T[] | undefined;
    rowKey: (row: T) => string;
    empty: ReactNode;
}) {
    if (rows === undefined) {
        return <TableSkeleton columns={columns} />;
    }
    if (rows.length === 0) {
        return <Empty>{empty}</Empty>;
    }
    return (
        <table className="table">
            <thead>
                <tr>
                    {columns.map((c) => (
                        <th key={c.label}>{c.label}</th>
                    ))}
                </tr>
            </thead>
            <tbody>
                {rows.map((row) => (
                    <tr key={rowKey(row)}>
                        {columns.map((c) => (
                            <td key={c.label} className={c.className}>
                                {c.render(row)}
                            </td>
                        ))}
                    </tr>
                ))}
            </tbody>
        </table>
    );
}

/** TableSkeleton stands in for a Table's rows until they've loaded. */
function TableSkeleton<T>({ columns, rows = 4 }: { columns: Column<T>[]; rows?: number }) {
    return (
        <table className="table skeleton" aria-busy="true">
            <thead>
                <tr>
                    {columns.map((c) => (
                        <th key={c.label}>{c.label}</th>
                    ))}
                </tr>
            </thead>
            <tbody>
                {Array.from({ length: rows }, (_, i) => (
                    <tr key={i}>
                        {columns.map((c) => (
                            <td key={c.label}>
                                <span className="skeleton-bar" />
                            </td>
                        ))}
                    </tr>
                ))}
            </tbody>
        </table>
    );
}

export function Field({
    label,
    help,
    children,
}: {
    label: string;
    help?: ReactNode;
    children: ReactNode;
}) {
    return (
        <label className="field">
            {label}
            {children}
            {help && <span className="help">{help}</span>}
        </label>
    );
}

export function Stat({
    value,
    label,
    hint,
}: {
    value: ReactNode;
    label: string;
    hint?: ReactNode;
}) {
    return (
        <div className="panel stat">
            <div className="value">{value}</div>
            <div className="label">{label}</div>
            {hint && <div className="hint">{hint}</div>}
        </div>
    );
}

/**
 * TimeRangeSelect picks a chart time range (format.ts's TIME_RANGES).
 * sinceStartLabel, when set, adds the SINCE_START option under that label
 * (e.g. "Since burn start") - only offer it where a start is known.
 */
export function TimeRangeSelect({
    value,
    onChange,
    sinceStartLabel,
}: {
    value: string;
    onChange: (value: string) => void;
    sinceStartLabel?: string;
}) {
    return (
        <select aria-label="Time range" value={value} onChange={(e) => onChange(e.target.value)}>
            {sinceStartLabel && <option value={SINCE_START}>{sinceStartLabel}</option>}
            {TIME_RANGES.map((r) => (
                <option key={r.value} value={r.value}>
                    {r.label}
                </option>
            ))}
        </select>
    );
}

/** Segmented is a small single-choice toggle (windows). */
export function Segmented<T extends string | number>({
    label,
    options,
    value,
    onChange,
}: {
    label: string;
    options: { value: T; label: string }[];
    value: T;
    onChange: (value: T) => void;
}) {
    return (
        <div className="seg" role="group" aria-label={label}>
            {options.map((o) => (
                <button
                    key={o.value}
                    type="button"
                    className={o.value === value ? "on" : undefined}
                    aria-pressed={o.value === value}
                    onClick={() => onChange(o.value)}
                >
                    {o.label}
                </button>
            ))}
        </div>
    );
}

export function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
}

/**
 * confirmAndRun asks first (in a modal, see Confirm.tsx), then runs action, toasting the outcome: a
 * success message if given (most actions here are a delete/kick with
 * nothing more interesting to say than "done", which the row vanishing or
 * refreshing already shows - pass one only when it adds information), and
 * always the error on failure.
 */
export async function confirmAndRun(
    question: ConfirmOptions | string,
    // checked is the answer to question.checkbox, if it has one.
    action: (checked: boolean) => Promise<unknown>,
    after?: () => void,
    successMessage?: string,
) {
    const answer = await confirm(question);
    if (!answer) {
        return;
    }
    try {
        await action(answer.checked);
        after?.();
        if (successMessage) {
            toast.success(successMessage);
        }
    } catch (err) {
        toast.error(errorMessage(err));
    }
}

export function formatTimestamp(value: string | undefined): string {
    return value ? new Date(value).toLocaleString() : "-";
}
