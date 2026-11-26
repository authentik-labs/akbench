import { fail } from "k6";
import { authorizationCode, benchOptions, iteration, loadUsers, newTrace, requestToken, userFor } from "./bench.ts";

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

const oauthClient = { id: clientID, redirectURI: redirectURI, scope: "openid offline_access" };

const client = { client_id: clientID, client_secret: clientSecret };

// Per VU: the refresh token the next iteration rotates. Cleared before each
// use, so a failed refresh logs in again instead of reusing a revoked token.
let refreshToken = "";

export default iteration(() => {
    const trace = newTrace("oauth-refresh");
    let current = refreshToken;
    refreshToken = "";
    if (!current) {
        const code = authorizationCode(trace, userFor(users), oauthClient);
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
