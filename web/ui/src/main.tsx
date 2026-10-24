import { AuthProvider, CALLBACK_PATH, SILENT_PATH } from "./auth";
import { ConfirmHost } from "./components/Confirm";
import { ToastHost } from "./components/Toast";
import { EventsProvider } from "./events";
import { Layout } from "./Layout";
import { AuthGate, OIDCCallback, OIDCSilent } from "./Login";
import type { RouteTitle } from "./title";

import { lazy, StrictMode, Suspense } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";

import { createBrowserRouter, Link, RouterProvider } from "react-router";

// @goauthentik/theme keys its dark-mode values off html[data-theme="dark"],
// not a prefers-color-scheme media query - bridge the two so this app still
// just follows the OS, same as before, including live switches (e.g. a
// laptop auto-dimming at night) without a page reload.
const darkMedia = window.matchMedia("(prefers-color-scheme: dark)");
const applyTheme = (isDark: boolean) => {
    document.documentElement.dataset.theme = isDark ? "dark" : "light";
};
applyTheme(darkMedia.matches);
darkMedia.addEventListener("change", (e) => applyTheme(e.matches));

// Every page is its own chunk, so the first load only pays for the page it
// lands on (charts, forms and the terminal each come in when first needed).
const page = <T extends Record<string, unknown>>(load: () => Promise<T>, name: keyof T) => {
    const Component = lazy(async () => ({ default: (await load())[name] as React.ComponentType }));
    return (
        <Suspense fallback={null}>
            <Component />
        </Suspense>
    );
};
const deployments = () => import("./pages/Deployments");
const testCases = () => import("./pages/TestCases");
const templates = () => import("./pages/TestCaseTemplates");
const clusters = () => import("./pages/Clusters");
const testRuns = () => import("./pages/TestRuns");
const grafana = () => import("./pages/Grafana");
const infrastructure = () => import("./pages/Infrastructure");

const t = (title: RouteTitle) => ({ title });

function NotFound() {
    return (
        <div>
            <h1>Not found</h1>
            <p>
                <Link to="/">Back to the dashboard</Link>
            </p>
        </div>
    );
}

const router = createBrowserRouter([
    { path: CALLBACK_PATH, element: <OIDCCallback /> },
    { path: SILENT_PATH, element: <OIDCSilent /> },
    {
        path: "/",
        element: (
            <AuthGate>
                <Layout />
            </AuthGate>
        ),
        children: [
            {
                index: true,
                handle: t("Dashboard"),
                element: page(() => import("./pages/Dashboard"), "DashboardPage"),
            },
            {
                path: "deployments",
                handle: t("Deployments"),
                element: page(deployments, "DeploymentsPage"),
            },
            {
                path: "deployments/new",
                handle: t("New deployment"),
                element: page(deployments, "DeploymentCreatePage"),
            },
            {
                path: "deployments/:name",
                handle: t((p) => `Deployment ${p.name}`),
                element: page(deployments, "DeploymentViewPage"),
            },
            {
                path: "deployments/:name/edit",
                handle: t((p) => `Edit deployment ${p.name}`),
                element: page(deployments, "DeploymentEditPage"),
            },
            {
                path: "testcases",
                handle: t("Test cases"),
                element: page(testCases, "TestCasesPage"),
            },
            {
                path: "testcases/new",
                handle: t("New test case"),
                element: page(testCases, "TestCaseCreatePage"),
            },
            {
                path: "testcases/:name",
                handle: t((p) => `Test case ${p.name}`),
                element: page(testCases, "TestCaseViewPage"),
            },
            {
                path: "testcases/:name/edit",
                handle: t((p) => `Edit test case ${p.name}`),
                element: page(testCases, "TestCaseEditPage"),
            },
            {
                path: "testcasetemplates",
                handle: t("Test case templates"),
                element: page(templates, "TestCaseTemplatesPage"),
            },
            {
                path: "testcasetemplates/new",
                handle: t("New test case template"),
                element: page(templates, "TestCaseTemplateCreatePage"),
            },
            {
                path: "testcasetemplates/:name",
                handle: t((p) => `Test case template ${p.name}`),
                element: page(templates, "TestCaseTemplateViewPage"),
            },
            {
                path: "testcasetemplates/:name/edit",
                handle: t((p) => `Edit test case template ${p.name}`),
                element: page(templates, "TestCaseTemplateEditPage"),
            },
            { path: "clusters", handle: t("Clusters"), element: page(clusters, "ClustersPage") },
            {
                path: "clusters/new",
                handle: t("New cluster"),
                element: page(clusters, "ClusterCreatePage"),
            },
            {
                path: "clusters/:name",
                handle: t((p) => `Cluster ${p.name}`),
                element: page(clusters, "ClusterViewPage"),
            },
            {
                path: "clusters/:name/edit",
                handle: t((p) => `Edit cluster ${p.name}`),
                element: page(clusters, "ClusterEditPage"),
            },
            {
                path: "infrastructure",
                handle: t("Providers"),
                element: page(infrastructure, "InfrastructurePage"),
            },
            {
                path: "infrastructure/incus/new",
                handle: t("New Incus credential"),
                element: page(infrastructure, "IncusCredentialCreatePage"),
            },
            {
                path: "infrastructure/incus/:name",
                handle: t((p) => `Incus credential ${p.name}`),
                element: page(infrastructure, "IncusCredentialViewPage"),
            },
            {
                path: "infrastructure/incus/:name/edit",
                handle: t((p) => `Edit Incus credential ${p.name}`),
                element: page(infrastructure, "IncusCredentialEditPage"),
            },
            { path: "testruns", handle: t("Test runs"), element: page(testRuns, "TestRunsPage") },
            {
                path: "testruns/:name",
                handle: t((p) => `Test run ${p.name}`),
                element: page(testRuns, "TestRunViewPage"),
            },
            {
                path: "testmatrices",
                handle: t("Test matrices"),
                element: page(() => import("./pages/TestMatrices"), "TestMatricesPage"),
            },
            {
                path: "testmatrices/:name",
                handle: t((p) => `Test matrix ${p.name}`),
                element: page(() => import("./pages/TestMatrices"), "TestMatrixViewPage"),
            },
            {
                path: "pull-requests/new",
                handle: t("Compare a PR"),
                element: page(
                    () => import("./pages/PullRequestComparison"),
                    "PullRequestComparisonPage",
                ),
            },
            {
                path: "branches/new",
                handle: t("Compare a branch"),
                element: page(
                    () => import("./pages/PullRequestComparison"),
                    "BranchComparisonPage",
                ),
            },
            {
                path: "compare",
                handle: t("Compare"),
                element: page(() => import("./pages/Compare"), "ComparePage"),
            },
            {
                path: "alerts",
                handle: t("Alerts"),
                element: page(() => import("./pages/Alerts"), "AlertsPage"),
            },
            { path: "grafana", handle: t("Grafana"), element: page(grafana, "GrafanaPage") },
            {
                path: "grafana/:cluster/*",
                handle: t((p) => `Grafana · ${p.cluster}`),
                element: page(grafana, "GrafanaPage"),
            },
            // Rendered by Layout's persistent ShellWorkspace, not an Outlet.
            { path: "shell", handle: t("Shell"), element: null },
            { path: "*", handle: t("Not found"), element: <NotFound /> },
        ],
    },
]);

createRoot(document.getElementById("root")!).render(
    <StrictMode>
        <AuthProvider>
            <EventsProvider>
                <RouterProvider router={router} />
            </EventsProvider>
        </AuthProvider>
        <ToastHost />
        <ConfirmHost />
    </StrictMode>,
);
