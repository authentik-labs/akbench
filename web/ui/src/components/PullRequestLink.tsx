import { AUTHENTIK_REPO, cachedPullRequest, PULL_REQUEST_LABEL, PullRequest } from "../github";

// Links an object to the authentik pull request it was created for (its
// bench.goauthentik.io/pull-request label). With `full` (view pages) it adds
// the PR's live state and title from GitHub; lists stick to the number, so
// they don't spend the rate limit. Without GitHub it's still a link.
import { useEffect, useState } from "react";

const stateClass: Record<string, string> = { open: "ok", merged: "info", closed: "neutral" };

export function PullRequestLink({
    labels,
    full,
}: {
    labels?: Record<string, string>;
    full?: boolean;
}) {
    const number = Number(labels?.[PULL_REQUEST_LABEL]);
    const [pr, setPr] = useState<PullRequest>();

    useEffect(() => {
        if (!number || !full) {
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
    }, [number, full]);

    if (!number) {
        return full ? null : <>-</>;
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
