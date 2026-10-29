// Resolves authentik pull requests and releases to the container images
// authentik's own CI publishes for them, so a PR can be benchmarked against
// its base branch or a release without looking up image tags by hand.
//
// Talks to the public GitHub API straight from the browser (it allows CORS);
// unauthenticated, so it's subject to GitHub's per-IP rate limit - every
// resolved value can also be typed in by hand.

export const AUTHENTIK_REPO = "goauthentik/authentik";

/**
 * Set by the pull-request comparison on its TestMatrix (and copied by the
 * operator onto the test cases and deployments it creates) to the PR number.
 */
export const PULL_REQUEST_LABEL = "bench.goauthentik.io/pull-request";

/**
 * Every commit CI builds - pushes to main and same-repo PR branches - is
 * published here as gh-<full commit sha>.
 */
export const DEV_IMAGE = "ghcr.io/goauthentik/dev-server";
/** Released versions, tagged with the bare version (e.g. 2026.6.0). */
export const RELEASE_IMAGE = "ghcr.io/goauthentik/server";

export interface PullRequest {
    number: number;
    title: string;
    url: string;
    state: string;
    headSha: string;
    headRef: string;
    baseSha: string;
    baseRef: string;
    /** False when the PR comes from a fork, whose CI runs never push images. */
    sameRepo: boolean;
}

/**
 * parsePullRequestNumber accepts a bare PR number ("12345", "#12345") or a
 * pull request URL on the authentik repo, returning the number - or
 * undefined for anything else (including another repo's PR URL).
 */
export function parsePullRequestNumber(input: string): number | undefined {
    const value = input.trim();
    const bare = /^#?(\d+)$/.exec(value);
    if (bare) {
        return Number(bare[1]);
    }
    const url =
        /^(?:https?:\/\/)?(?:www\.)?github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)(?:[/?#].*)?$/i.exec(
            value,
        );
    if (url && url[1].toLowerCase() === AUTHENTIK_REPO) {
        return Number(url[2]);
    }
    return undefined;
}

/** devImageTag is the dev-server tag CI publishes for one commit. */
export function devImageTag(sha: string): string {
    return `gh-${sha}`;
}

/**
 * releaseVersion strips authentik's "version/" release tag prefix
 * ("version/2026.6.0" -> "2026.6.0"), the form its image is tagged with.
 */
export function releaseVersion(tagName: string): string {
    return tagName.replace(/^version\//, "");
}

/**
 * k8sNameSegment turns arbitrary text (a version, a branch) into something
 * safe inside a Kubernetes object name.
 */
export function k8sNameSegment(value: string): string {
    return value
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "");
}

/**
 * k8sNameInput slugifies a Kubernetes object name as it is typed: unlike
 * k8sNameSegment it keeps a trailing dash, so "foo-" can become "foo-bar".
 */
export function k8sNameInput(value: string): string {
    return value
        .toLowerCase()
        .replace(/[^a-z0-9-]+/g, "-")
        .replace(/^-+/, "");
}

async function github<T>(path: string): Promise<T> {
    const res = await fetch(`https://api.github.com/repos/${AUTHENTIK_REPO}${path}`, {
        headers: { Accept: "application/vnd.github+json" },
    });
    if (res.status === 404) {
        throw new Error("Not found on GitHub");
    }
    if (res.status === 403 || res.status === 429) {
        throw new Error(
            "GitHub API rate limit reached - enter the image tags by hand or try again later",
        );
    }
    if (!res.ok) {
        throw new Error(`GitHub API: ${res.status} ${res.statusText}`);
    }
    return (await res.json()) as T;
}

interface GitHubPull {
    number: number;
    title: string;
    html_url: string;
    state: string;
    merged_at: string | null;
    head: { sha: string; ref: string; repo: { full_name: string } | null };
    base: { sha: string; ref: string };
}

export async function fetchPullRequest(number: number): Promise<PullRequest> {
    const pr = await github<GitHubPull>(`/pulls/${number}`);
    return {
        number: pr.number,
        title: pr.title,
        url: pr.html_url,
        state: pr.merged_at ? "merged" : pr.state,
        headSha: pr.head.sha,
        headRef: pr.head.ref,
        baseSha: pr.base.sha,
        baseRef: pr.base.ref,
        sameRepo: pr.head.repo?.full_name.toLowerCase() === AUTHENTIK_REPO,
    };
}

const pullRequests = new Map<number, Promise<PullRequest>>();

/**
 * cachedPullRequest is fetchPullRequest memoized for the page's lifetime, so a
 * list of objects from the same PR costs one request against the rate limit.
 */
export function cachedPullRequest(number: number): Promise<PullRequest> {
    let pr = pullRequests.get(number);
    if (!pr) {
        pr = fetchPullRequest(number);
        // Don't keep failures (e.g. rate limited): retry on the next render.
        pr.catch(() => pullRequests.delete(number));
        pullRequests.set(number, pr);
    }
    return pr;
}

/** fetchLatestRelease returns the newest release's version, e.g. "2026.6.0". */
export async function fetchLatestRelease(): Promise<string> {
    const release = await github<{ tag_name: string }>("/releases/latest");
    return releaseVersion(release.tag_name);
}

/** fetchBranchHead returns the sha of a branch's newest commit. */
export async function fetchBranchHead(branch: string): Promise<string> {
    const res = await github<{ commit: { sha: string } }>(
        `/branches/${encodeURIComponent(branch)}`,
    );
    return res.commit.sha;
}
