import { api } from "../api";
import { toast } from "../components/Toast";
import { Alert, errorMessage, Field, Page, Wizard, WizardStep } from "../components/ui";
import {
    DEV_IMAGE,
    devImageTag,
    fetchBranchHead,
    fetchLatestRelease,
    fetchPullRequest,
    PULL_REQUEST_LABEL,
    k8sNameInput,
    k8sNameSegment,
    parsePullRequestNumber,
    PullRequest,
    RELEASE_IMAGE,
} from "../github";
import { AuthentikDeploymentSpec, TestCaseSpec } from "../types";
import { useResource } from "../useResource";
import { ClusterSelect, DeploymentSpecFields, emptySpec } from "./Deployments";

import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router";

// The branch comparison's equivalent, with the branch as a name segment.
const BRANCH_LABEL = "bench.goauthentik.io/branch";

// Helm's release name limit; a deployment's name is its release name.
const MAX_DEPLOYMENT_NAME_LENGTH = 53;

type Baseline = "base" | "release";

type ImageState = "checking" | "published" | "missing" | "unknown";

/**
 * useImageCheck asks the apiserver whether image:tag is published, debounced
 * while the tag is being typed. authentik's CI only publishes a commit's
 * image once its tests pass, so a PR's newest commit often has none yet.
 */
function useImageCheck(image: string, tag: string): ImageState | undefined {
    // Results are keyed by image:tag, so a stale one reads as "checking"
    // until the debounced check for the current tag lands.
    const key = `${image}:${tag}`;
    const [result, setResult] = useState<{ key: string; state: ImageState }>();
    useEffect(() => {
        if (!tag) {
            return;
        }
        let cancelled = false;
        const settle = (state: ImageState) => !cancelled && setResult({ key, state });
        const timer = setTimeout(() => {
            api.checkImage(image.replace(/^ghcr\.io\//, ""), tag)
                .then((res) => settle(res.exists ? "published" : "missing"))
                .catch(() => settle("unknown"));
        }, 400);
        return () => {
            cancelled = true;
            clearTimeout(timer);
        };
    }, [image, tag, key]);
    if (!tag) {
        return undefined;
    }
    return result?.key === key ? result.state : "checking";
}

function ImageStatus({ state }: { state?: ImageState }) {
    switch (state) {
        case "checking":
            return <span className="muted">Checking ghcr.io…</span>;
        case "published":
            return <span>Published on ghcr.io</span>;
        case "missing":
            return (
                <strong>
                    Not published (yet) - CI publishes a commit&apos;s image only once its tests
                    pass
                </strong>
            );
        case "unknown":
            return <span className="muted">Couldn&apos;t check ghcr.io</span>;
        default:
            return null;
    }
}

/**
 * ComparisonForm sets up a side-by-side Burn test of an authentik pull request's CI
 * build against its base branch or a release: two deployments sharing every setting but the image,
 * and one TestCase driving both with the same load. In branch mode the PR is replaced by a branch
 * and the baseline is always a release. It creates those objects directly, unless several templates
 * are picked: then a single parent TestCase owning one such pair of deployments plus a child
 * TestCase per template, so the templates' provisioning and load don't interfere with each other.
 */
export function ComparisonForm({
    mode,
    kindStep,
}: {
    mode: "pr" | "branch";
    kindStep: WizardStep;
}) {
    const isBranch = mode === "branch";
    const navigate = useNavigate();
    const templates = useResource(() => api.testCaseTemplates(), [], 0);

    const [prInput, setPrInput] = useState("");
    const [pr, setPr] = useState<PullRequest>();
    const [lookupError, setLookupError] = useState<string>();
    const [lookingUp, setLookingUp] = useState(false);
    const [branch, setBranch] = useState("main");
    const [branchSha, setBranchSha] = useState<string>();

    // The tags are prefilled from the lookup but stay editable - a fork PR,
    // a rate-limited lookup or an image built from another commit all need
    // them typed in by hand.
    const [prTag, setPrTag] = useState(isBranch ? "gh-main" : "");
    const [chosenBaseline, setBaseline] = useState<Baseline>("base");
    const baseline: Baseline = isBranch ? "release" : chosenBaseline;
    const [baseTag, setBaseTag] = useState("gh-main");
    const [version, setVersion] = useState("");

    const [prefix, setPrefix] = useState("");
    const [prefixEdited, setPrefixEdited] = useState(false);

    const [spec, setSpec] = useState<AuthentikDeploymentSpec>(emptySpec());
    const [extraValuesYAML, setExtraValuesYAML] = useState("");
    const patch = (changes: Partial<AuthentikDeploymentSpec>) =>
        setSpec((current) => ({ ...current, ...changes }));

    const [selectedTemplates, setSelectedTemplates] = useState(["login"]);
    const toggleTemplate = (name: string) =>
        setSelectedTemplates((current) =>
            current.includes(name) ? current.filter((t) => t !== name) : [...current, name],
        );
    const [vus, setVus] = useState(8);
    const [flow, setFlow] = useState("default-authentication-flow");
    const [burnDuration, setBurnDuration] = useState("8760h");
    const [traceSampleRate, setTraceSampleRate] = useState("0.01");

    const [error, setError] = useState<string>();
    const [saving, setSaving] = useState(false);

    const prImage = useImageCheck(DEV_IMAGE, prTag);
    const baselineImage = useImageCheck(
        baseline === "base" ? DEV_IMAGE : RELEASE_IMAGE,
        baseline === "base" ? baseTag : version,
    );

    // Prefill the release baseline with the newest release; not fatal if
    // GitHub can't be reached, the field just starts blank.
    useEffect(() => {
        let cancelled = false;
        fetchLatestRelease()
            .then((v) => !cancelled && setVersion((current) => current || v))
            .catch(() => undefined);
        return () => {
            cancelled = true;
        };
    }, []);

    const prNumber = parsePullRequestNumber(prInput);

    async function lookUpBranch() {
        setLookingUp(true);
        setLookupError(undefined);
        try {
            const sha = await fetchBranchHead(branch.trim());
            setBranchSha(sha);
            setPrTag(devImageTag(sha));
        } catch (err) {
            setBranchSha(undefined);
            setLookupError(errorMessage(err));
        } finally {
            setLookingUp(false);
        }
    }

    async function lookUp() {
        if (prNumber === undefined) {
            setLookupError(
                "Enter a pull request number or a github.com/goauthentik/authentik/pull/... URL",
            );
            return;
        }
        setLookingUp(true);
        setLookupError(undefined);
        try {
            const found = await fetchPullRequest(prNumber);
            setPr(found);
            setPrTag(devImageTag(found.headSha));
            setBaseTag(devImageTag(found.baseSha));
        } catch (err) {
            setPr(undefined);
            setLookupError(errorMessage(err));
        } finally {
            setLookingUp(false);
        }
    }

    // The prefix follows the PR number (or branch) until edited directly.
    const defaultPrefix = isBranch
        ? k8sNameSegment(branch)
        : prNumber !== undefined
          ? `pr-${prNumber}`
          : "";
    const effectivePrefix = prefixEdited ? prefix : defaultPrefix;
    const baselineSuffix =
        baseline === "base" ? k8sNameSegment(pr?.baseRef ?? "main") : k8sNameSegment(version);
    // The variant names make up the deployments' names,
    // <parent>-<variant>-<template> (see TestCase.ChildDeploymentName).
    const variants = [
        { name: "head", image: DEV_IMAGE, tag: prTag },
        baseline === "base"
            ? { name: baselineSuffix, image: DEV_IMAGE, tag: baseTag }
            : { name: baselineSuffix, image: RELEASE_IMAGE, tag: version },
    ];
    const rows = selectedTemplates.map((template) => ({
        template,
        testCaseName: effectivePrefix && `${effectivePrefix}-${template}`,
        deployments: variants.map((v) => ({
            ...v,
            deploymentName: effectivePrefix && v.name && `${effectivePrefix}-${v.name}-${template}`,
        })),
    }));
    // Each deployment's name is its Helm release name, which Helm caps at 53.
    const tooLong = rows
        .flatMap((r) => r.deployments.map((d) => d.deploymentName))
        .filter((n) => n.length > MAX_DEPLOYMENT_NAME_LENGTH);
    const asParent = rows.length > 1;

    async function submit() {
        if (!effectivePrefix || !baselineSuffix) {
            setError(`Enter a ${isBranch ? "branch" : "pull request"} and a baseline first`);
            return;
        }
        if (selectedTemplates.length === 0) {
            setError("Pick at least one template");
            return;
        }
        setSaving(true);
        setError(undefined);
        const labels: Record<string, string> | undefined = isBranch
            ? { [BRANCH_LABEL]: k8sNameSegment(branch) }
            : prNumber !== undefined
              ? { [PULL_REQUEST_LABEL]: String(prNumber) }
              : undefined;
        const testCase = { profile: "Burn" as const, vus, flow, burnDuration, traceSampleRate };
        if (!asParent) {
            await createDirectly(rows[0], labels, testCase);
            return;
        }
        try {
            await api.createTestCase({
                metadata: { name: effectivePrefix, labels },
                spec: {
                    ...testCase,
                    variants,
                    baselineVariant: baselineSuffix,
                    templates: selectedTemplates,
                    deployment: spec,
                    extraValues: extraValuesYAML.trim() ? extraValuesYAML : undefined,
                },
            });
            toast.success(`Created test case ${effectivePrefix}`);
            navigate(`/testcases/${effectivePrefix}`);
        } catch (err) {
            setError(errorMessage(err));
            setSaving(false);
        }
    }

    // createDirectly is a parent's work for a single template, without the
    // parent: the deployments and a TestCase driving them, named the same.
    async function createDirectly(
        row: (typeof rows)[number],
        labels: Record<string, string> | undefined,
        testCase: Omit<TestCaseSpec, "template">,
    ) {
        const created: string[] = [];
        try {
            for (const d of row.deployments) {
                await api.createDeployment({
                    metadata: { name: d.deploymentName, labels },
                    spec: {
                        ...spec,
                        namespace: d.deploymentName,
                        authentik: { ...spec.authentik, image: d.image, tag: d.tag },
                    },
                });
                created.push(`deployment ${d.deploymentName}`);
                if (extraValuesYAML.trim()) {
                    await api.setExtraValues(d.deploymentName, extraValuesYAML);
                }
            }
            await api.createTestCase({
                metadata: { name: row.testCaseName, labels },
                spec: {
                    ...testCase,
                    template: row.template,
                    deploymentRefs: row.deployments.map((d) => ({ name: d.deploymentName })),
                    baselineRef: {
                        name: row.deployments.find((d) => d.name === baselineSuffix)!
                            .deploymentName,
                    },
                },
            });
            toast.success(`Created test case ${row.testCaseName}`);
            navigate(`/testcases/${row.testCaseName}`);
        } catch (err) {
            setError(
                created.length > 0
                    ? `${errorMessage(err)} (already created: ${created.join(", ")} - delete or reuse them before retrying)`
                    : errorMessage(err),
            );
            setSaving(false);
        }
    }

    const steps: WizardStep[] = [
        kindStep,
        {
            label: isBranch ? "Branch" : "Pull request",
            validate: () =>
                !isBranch && prNumber === undefined
                    ? "Enter a pull request number or a github.com/goauthentik/authentik/pull/... URL"
                    : undefined,
            body: (
                <>
                    {isBranch ? (
                        <>
                            <div className="fields">
                                <Field label="Branch" help="On goauthentik/authentik">
                                    <input
                                        required
                                        value={branch}
                                        placeholder="main"
                                        onChange={(e) => {
                                            setBranch(e.target.value);
                                            setBranchSha(undefined);
                                        }}
                                    />
                                </Field>
                                <Field
                                    label="Branch image tag"
                                    help={
                                        <>
                                            On {DEV_IMAGE}; pin it to the branch&apos;s head commit
                                            so a restart doesn&apos;t pick up a newer build.{" "}
                                            <ImageStatus state={prImage} />
                                        </>
                                    }
                                >
                                    <input
                                        required
                                        value={prTag}
                                        placeholder="gh-<commit sha>"
                                        onChange={(e) => setPrTag(e.target.value)}
                                    />
                                </Field>
                            </div>
                            <div className="row">
                                <button
                                    type="button"
                                    disabled={lookingUp || !branch.trim()}
                                    onClick={() => void lookUpBranch()}
                                >
                                    {lookingUp ? "Looking up…" : "Pin to head commit"}
                                </button>
                                {branchSha && (
                                    <span className="muted">
                                        <code>{branch}</code> is at{" "}
                                        <code>{branchSha.slice(0, 12)}</code>
                                    </span>
                                )}
                            </div>
                            <Alert>{lookupError}</Alert>
                        </>
                    ) : (
                        <>
                            <div className="fields">
                                <Field
                                    label="Pull request"
                                    help="Number or URL on goauthentik/authentik"
                                >
                                    <input
                                        required
                                        value={prInput}
                                        placeholder="https://github.com/goauthentik/authentik/pull/12345"
                                        onChange={(e) => setPrInput(e.target.value)}
                                        onBlur={() => {
                                            if (prNumber !== undefined && prNumber !== pr?.number) {
                                                void lookUp();
                                            }
                                        }}
                                    />
                                </Field>
                                <Field
                                    label="PR image tag"
                                    help={
                                        <>
                                            On {DEV_IMAGE}; defaults to the PR&apos;s head commit.{" "}
                                            <ImageStatus state={prImage} />
                                        </>
                                    }
                                >
                                    <input
                                        required
                                        value={prTag}
                                        placeholder="gh-<commit sha>"
                                        onChange={(e) => setPrTag(e.target.value)}
                                    />
                                </Field>
                            </div>
                            <div className="row">
                                <button
                                    type="button"
                                    disabled={lookingUp || prNumber === undefined}
                                    onClick={() => void lookUp()}
                                >
                                    {lookingUp ? "Looking up…" : "Look up on GitHub"}
                                </button>
                                {pr && (
                                    <span className="muted">
                                        <a href={pr.url} target="_blank" rel="noreferrer">
                                            #{pr.number}
                                        </a>{" "}
                                        {pr.title} ({pr.state}, <code>{pr.headRef}</code> into{" "}
                                        <code>{pr.baseRef}</code>)
                                    </span>
                                )}
                            </div>
                            <Alert>{lookupError}</Alert>
                            {pr && !pr.sameRepo && (
                                <Alert kind="info">
                                    This PR comes from a fork - authentik&apos;s CI doesn&apos;t
                                    publish images for those, so the PR image tag likely
                                    doesn&apos;t exist. Enter the tag of an image you built
                                    yourself.
                                </Alert>
                            )}
                        </>
                    )}
                </>
            ),
        },
        {
            label: "Baseline",
            body: (
                <>
                    {!isBranch && (
                        <div className="choice-cards" role="radiogroup" aria-label="Baseline">
                            <label className="choice-card">
                                <input
                                    type="radio"
                                    name="baseline"
                                    checked={baseline === "base"}
                                    onChange={() => setBaseline("base")}
                                />
                                <span>
                                    <strong>Base branch{pr ? ` (${pr.baseRef})` : ""}</strong>
                                    <span className="muted">
                                        The dev build of the commit the PR is based on, so only the
                                        PR&apos;s own changes differ.
                                    </span>
                                </span>
                            </label>
                            <label className="choice-card">
                                <input
                                    type="radio"
                                    name="baseline"
                                    checked={baseline === "release"}
                                    onChange={() => setBaseline("release")}
                                />
                                <span>
                                    <strong>Tagged release</strong>
                                    <span className="muted">
                                        A released version, e.g. to check a PR against what users
                                        run today.
                                    </span>
                                </span>
                            </label>
                        </div>
                    )}
                    <div className="fields">
                        {baseline === "base" ? (
                            <Field
                                label="Baseline image tag"
                                help={
                                    <>
                                        On {DEV_IMAGE}; defaults to the PR&apos;s base commit.{" "}
                                        <ImageStatus state={baselineImage} />
                                        {baselineImage === "missing" && baseTag !== "gh-main" && (
                                            <>
                                                {" "}
                                                <button
                                                    type="button"
                                                    className="small"
                                                    onClick={() => setBaseTag("gh-main")}
                                                >
                                                    Use gh-main
                                                </button>
                                            </>
                                        )}
                                    </>
                                }
                            >
                                <input
                                    required
                                    value={baseTag}
                                    placeholder="gh-main"
                                    onChange={(e) => setBaseTag(e.target.value)}
                                />
                            </Field>
                        ) : (
                            <Field
                                label="Release version"
                                help={
                                    <>
                                        On {RELEASE_IMAGE}; defaults to the latest release.{" "}
                                        <ImageStatus state={baselineImage} />
                                    </>
                                }
                            >
                                <input
                                    required
                                    value={version}
                                    placeholder="2026.6.0"
                                    onChange={(e) => setVersion(e.target.value)}
                                />
                            </Field>
                        )}
                    </div>
                </>
            ),
        },
        {
            label: "Deployments",
            validate: () =>
                tooLong.length > 0
                    ? `${tooLong.join(", ")} ${tooLong.length === 1 ? "is" : "are"} longer than ${MAX_DEPLOYMENT_NAME_LENGTH} characters, Helm's release name limit - shorten the name.`
                    : undefined,
            body: (
                <>
                    <p className="muted">
                        Every deployment gets the settings below; only the image differs.
                    </p>
                    <div className="fields">
                        <Field
                            label="Name"
                            help="Test cases, deployments and namespaces are named after it"
                        >
                            <input
                                required
                                value={effectivePrefix}
                                placeholder={isBranch ? "main" : "pr-12345"}
                                onChange={(e) => {
                                    setPrefixEdited(true);
                                    setPrefix(k8sNameInput(e.target.value));
                                }}
                            />
                        </Field>
                        <Field label="Cluster">
                            <ClusterSelect
                                value={spec.clusterRef?.name}
                                onChange={(cluster) => patch({ clusterRef: { name: cluster } })}
                            />
                        </Field>
                    </div>
                    <DeploymentSpecFields
                        spec={spec}
                        patch={patch}
                        extraValuesYAML={extraValuesYAML}
                        onExtraValuesChange={setExtraValuesYAML}
                        showImage={false}
                    />
                </>
            ),
        },
        {
            label: "Script",
            validate: () =>
                selectedTemplates.length === 0 ? "Pick at least one template" : undefined,
            body: (
                <div className="fields">
                    <div className="field wide">
                        <span id="templates-label">Templates</span>
                        <div className="checklist" role="group" aria-labelledby="templates-label">
                            {(templates.data ?? []).map((t) => (
                                <label key={t.metadata.name}>
                                    <input
                                        type="checkbox"
                                        checked={selectedTemplates.includes(t.metadata.name)}
                                        onChange={() => toggleTemplate(t.metadata.name)}
                                    />
                                    <span className="mono">{t.metadata.name}</span>
                                    {t.spec.description && (
                                        <span className="meta">{t.spec.description}</span>
                                    )}
                                </label>
                            ))}
                        </div>
                        <Link to="/testcasetemplates">Manage test case templates</Link>
                    </div>
                    <Field label="Flow slug">
                        <input
                            value={flow}
                            placeholder="default-authentication-flow"
                            onChange={(e) => setFlow(e.target.value)}
                        />
                    </Field>
                </div>
            ),
        },
        {
            label: "Load",
            body: (
                <>
                    <p className="muted">
                        Keeps both deployments under the same steady load until stopped, each tagged
                        with its own k6 target. The load starts on both at the same moment, once
                        both deployments and their k6 runners are ready. Pick several templates to
                        run them side by side: each gets its own pair of deployments and test case.
                    </p>
                    <div className="fields">
                        <Field label="Virtual users" help="Per deployment">
                            <input
                                type="number"
                                min={1}
                                value={vus}
                                onChange={(e) => setVus(Number(e.target.value))}
                            />
                        </Field>
                        <Field label="Time limit">
                            <input
                                value={burnDuration}
                                placeholder="8760h"
                                onChange={(e) => setBurnDuration(e.target.value)}
                            />
                        </Field>
                    </div>
                </>
            ),
        },
        {
            label: "Review",
            body: (
                <>
                    <div className="fields">
                        <Field label="Trace sample rate">
                            <input
                                value={traceSampleRate}
                                placeholder="0.01"
                                onChange={(e) => setTraceSampleRate(e.target.value)}
                            />
                        </Field>
                    </div>
                    {asParent && (
                        <p className="muted">
                            One test case <code>{effectivePrefix || "…"}</code>, which creates and
                            owns:
                        </p>
                    )}
                    <ul className="muted">
                        {rows.map((row) => (
                            <li key={row.template}>
                                Burn test case <code>{row.testCaseName || "…"}</code> (
                                <code>{row.template}</code>) driving
                                <ul>
                                    {row.deployments.map((d) => (
                                        <li key={d.name}>
                                            Deployment <code>{d.deploymentName || "…"}</code>{" "}
                                            running{" "}
                                            <code>
                                                {d.image}:{d.tag || "…"}
                                            </code>
                                        </li>
                                    ))}
                                </ul>
                            </li>
                        ))}
                    </ul>
                </>
            ),
        },
    ];

    return (
        <Page
            title="New test case"
            subtitle={
                isBranch
                    ? "Burn-test an authentik branch build, e.g. main, against a stable release, side by side."
                    : "Burn-test an authentik PR build against its base branch or a release, side by side."
            }
        >
            <Wizard
                steps={steps}
                saving={saving}
                submitLabel="Create comparison"
                error={error}
                onSubmit={() => void submit()}
            />
        </Page>
    );
}
