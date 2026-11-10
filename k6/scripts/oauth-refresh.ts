import http from "k6/http";
import { fail } from "k6";
import type { Trace, User } from "./bench.ts";
import {
    authentikBase,
    benchOptions,
    expectRedirect,
    flowRef,
    iteration,
    loadUsers,
    loginStages,
    newTrace,
    params,
    requestToken,
    resolveURL,
    userFor,
    walkFlow,
} from "./bench.ts";

// Refresh token rotation, the way mobile apps hold a session: each VU logs
// in once through an authorization-code grant with offline_access, then
// every iteration swaps its refresh token for a new one. authentik marks the
// old token revoked but keeps its original expiry, so every iteration leaves
// one revoked row in authentik_providers_oauth2_refreshtoken that
// clean_expired_models only removes once the token's validity runs out.
// Compare that table's live rows (Postgres dashboard) between builds.
const clientID = __ENV.OAUTH_CLIENT_ID || "";
const clientSecret = __ENV.OAUTH_CLIENT_SECRET || "";
const redirectURI = __ENV.OAUTH_REDIRECT_URI || "";
if (!clientID || !clientSecret || !redirectURI) {
    throw new Error("OAUTH_CLIENT_ID/SECRET/REDIRECT_URI are required for the oauth-refresh-rotation template - does the template list the oauth-token dependency?");
}

const users = loadUsers();

export const options = benchOptions();

// authorizationCode walks /authorize/ to the redirect URI: the login flow,
// then the provider's consent flow. The redirect URI is never requested,
// only its code is read.
function authorizationCode(trace: Trace, user: User): string {
    const query = [
        `client_id=${encodeURIComponent(clientID)}`,
        `redirect_uri=${encodeURIComponent(redirectURI)}`,
        "response_type=code",
        `scope=${encodeURIComponent("openid offline_access")}`,
        `state=${__VU}-${__ITER}`,
    ].join("&");
    let next = `/application/o/authorize/?${query}`;
    for (let hop = 0; hop < 8; hop++) {
        if (next.startsWith(redirectURI)) {
            const code = next.match(/[?&]code=([^&#]+)/);
            if (!code) {
                fail(`redirect to ${next} has no authorization code`);
            }
            return decodeURIComponent(code[1]);
        }
        const flow = flowRef(next);
        if (flow) {
            next = expectRedirect(flow, walkFlow(trace, flow, loginStages(user)));
            continue;
        }
        const res = http.get(resolveURL(authentikBase, next), params(trace, { redirects: 0 }));
        const location = res.headers["Location"];
        if (res.status < 300 || res.status >= 400 || !location) {
            fail(`authorize: expected a redirect from ${res.url}, got status ${res.status}`);
        }
        next = resolveURL(res.url, location);
    }
    fail(`authorize: no redirect to ${redirectURI} after 8 hops`);
}

const client = { client_id: clientID, client_secret: clientSecret };

// Per VU: the refresh token the next iteration rotates. Cleared before each
// use, so a failed refresh logs in again instead of reusing a revoked token.
let refreshToken = "";

export default iteration(() => {
    const trace = newTrace("oauth-refresh");
    let current = refreshToken;
    refreshToken = "";
    if (!current) {
        const code = authorizationCode(trace, userFor(users));
        current = requestToken(trace, "authorization_code", { grant_type: "authorization_code", code: code, redirect_uri: redirectURI, ...client }).refresh_token ?? "";
        if (!current) {
            fail("authorization_code grant returned no refresh token - is offline_access mapped on the provider?");
        }
    }
    // A provider with a refresh_token_threshold only rotates near expiry.
    const res = requestToken(trace, "refresh_token", { grant_type: "refresh_token", refresh_token: current, ...client });
    refreshToken = res.refresh_token ?? current;
    return res.access_token !== "";
});
