import { api } from "./api";
import { useAuth } from "./auth";
import { buildLabel } from "./format";
import { APP_TITLE, useDocumentTitle } from "./title";
import { useResource } from "./useResource";

import { lazy, Suspense, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router";

// The terminal (ghostty-web and its WebAssembly) only loads once the shell is first opened.
const ShellWorkspace = lazy(async () => ({
    default: (await import("./shell/ShellWorkspace")).ShellWorkspace,
}));

// Grouped by what you're doing with it: running load tests, then reading
// their results, then the things they run against - deployments, templates
// and the infrastructure under them, set up once and rarely touched. One-off
// actions (comparing a PR or branch) are buttons on the pages they belong to,
// not destinations of their own.
type NavSection = { label?: string; items: { to: string; label: string; end?: boolean }[] };

const navSections: NavSection[] = [
    { items: [{ to: "/", label: "Dashboard", end: true }] },
    {
        label: "Testing",
        items: [
            { to: "/testcases", label: "Test cases" },
            { to: "/testmatrices", label: "Test matrices" },
            { to: "/testruns", label: "Test runs" },
        ],
    },
    {
        label: "Analysis",
        items: [
            { to: "/compare", label: "Compare" },
            { to: "/alerts", label: "Alerts" },
            { to: "/grafana", label: "Grafana" },
        ],
    },
    {
        label: "Setup",
        items: [
            { to: "/deployments", label: "Deployments" },
            { to: "/testcasetemplates", label: "Templates" },
            { to: "/clusters", label: "Clusters" },
            { to: "/infrastructure", label: "Providers" },
            { to: "/shell", label: "Shell" },
        ],
    },
];

const REPO_URL = "https://github.com/authentik-labs/akbench";

/**
 * BuildBadge identifies the running build, so a screenshot or bug report
 * always says which one it came from. Links to the commit when known.
 */
function BuildBadge() {
    const build = useResource(() => api.version(), [], 0);
    const info = build.data;
    if (!info) {
        return null;
    }
    const title = [
        info.commit && `Commit ${info.commit}`,
        info.goVersion && `Built with ${info.goVersion}`,
    ]
        .filter(Boolean)
        .join("\n");
    return info.commit ? (
        <a
            className="build-badge"
            href={`${REPO_URL}/commit/${info.commit}`}
            target="_blank"
            rel="noreferrer"
            title={title}
        >
            {buildLabel(info)}
        </a>
    ) : (
        <span className="build-badge" title={title || undefined}>
            {buildLabel(info)}
        </span>
    );
}

function NavSectionLinks({ section, onNavigate }: { section: NavSection; onNavigate: () => void }) {
    return (
        <div>
            {section.label && <div className="nav-section-label">{section.label}</div>}
            {section.items.map((item) => (
                <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    className="nav-link"
                    onClick={onNavigate}
                >
                    {item.label}
                </NavLink>
            ))}
        </div>
    );
}

export function Layout() {
    const { identity, logout } = useAuth();
    const location = useLocation();
    useDocumentTitle();
    const onShell = location.pathname === "/shell";
    // The shell workspace mounts on first visit and then stays mounted (just
    // hidden), so its sessions survive navigating elsewhere in the app.
    const [shellMounted, setShellMounted] = useState(onShell);
    // Only matters on narrow screens, where the sidebar is an off-canvas drawer.
    const [navOpen, setNavOpen] = useState(false);
    const closeNav = () => setNavOpen(false);
    if (onShell && !shellMounted) {
        setShellMounted(true);
    }

    return (
        <div className="app">
            <header className="mobile-bar">
                <button
                    className="small"
                    aria-label="Toggle navigation"
                    aria-expanded={navOpen}
                    onClick={() => setNavOpen(!navOpen)}
                >
                    ☰
                </button>
                <img src="/favicon.svg" alt="" />
                {APP_TITLE}
            </header>
            {navOpen && <div className="nav-backdrop" onClick={closeNav} />}
            <nav className={navOpen ? "sidebar open" : "sidebar"}>
                <div className="brand">
                    <img src="/favicon.svg" alt="" />
                    {APP_TITLE}
                </div>
                {navSections.map((section, i) => (
                    <NavSectionLinks
                        key={section.label ?? i}
                        section={section}
                        onNavigate={closeNav}
                    />
                ))}
                <div className="sidebar-foot">
                    <BuildBadge />
                    {identity && <span>Signed in as {identity}</span>}
                    <button className="small" onClick={() => void logout()}>
                        Sign out
                    </button>
                </div>
            </nav>
            <main className={onShell ? "main full" : "main"}>
                {!onShell && <Outlet />}
                {shellMounted && (
                    <Suspense fallback={null}>
                        <ShellWorkspace visible={onShell} />
                    </Suspense>
                )}
            </main>
        </div>
    );
}
