import { AUTHENTIK_REPO, cachedPullRequest, PULL_REQUEST_LABEL, PullRequest } from "../github";

// Links an object to the authentik pull request it was created for (its
// bench.goauthentik.io/pull-request label), with the PR's live state and title
// from GitHub. Without GitHub (rate limited, offline) it's still a link.
import { useEffect, useState } from "react";

const stateClass: Record<string, string> = { open: "ok", merged: "info", closed: "neutral" };

export function PullRequestLink({ labels }: { labels?: Record<string, string> }) {
    const number = Number(labels?.[PULL_REQUEST_LABEL]);
    const [pr, setPr] = useState<PullRequest>();

    useEffect(() => {
        if (!number) {
            return;
        }
        let current = true;
        cachedPullRequest(number).then(
            (found) => current && setPr(found),
            () => undefined,
        );
        return () => {
            current = false;
        };
    }, [number]);

    if (!number) {
        return <>-</>;
    }
    return (
        <span title={pr?.title}>
            <a
                href={`https://github.com/${AUTHENTIK_REPO}/pull/${number}`}
                target="_blank"
                rel="noreferrer"
            >
                #{number}
            </a>
            {pr && (
                <>
                    {" "}
                    <span className={`badge ${stateClass[pr.state] ?? "neutral"}`}>
                        {pr.state}
                    </span>{" "}
                    {pr.title}
                </>
            )}
        </span>
    );
}
