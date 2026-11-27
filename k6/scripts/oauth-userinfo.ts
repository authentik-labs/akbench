import http from "k6/http";
import { check } from "k6";
import { benchOptions, iteration, newTrace, params, requestToken, requireReusable, reusableToken, userinfoURL } from "./bench.ts";
import type { Trace } from "./bench.ts";

// What a relying party or API gateway does with an access token on every
// request it serves: fetch the user's claims from authentik's OpenID Connect
// UserInfo endpoint, which looks the token up and evaluates the provider's
// scope mappings for it. The token is a client_credentials access token with
// the openid, email and profile scopes from the oauth-token dependency: the
// provider's service account, so lighter than a person with groups. Each VU reuses its token
// until shortly before it expires, so iterations measure UserInfo alone.
const clientID = __ENV.OAUTH_CLIENT_ID || "";
const clientSecret = __ENV.OAUTH_CLIENT_SECRET || "";
if (!clientID || !clientSecret) {
    throw new Error("OAUTH_CLIENT_ID/SECRET are required for the oauth-userinfo template - does the template list the oauth-token dependency?");
}
const kind = "oauth-userinfo";

export const options = benchOptions();

function clientCredentials(trace: Trace) {
    return requestToken(trace, "client_credentials", {
        grant_type: "client_credentials",
        client_id: clientID,
        client_secret: clientSecret,
        // UserInfo refuses a token without openid. email and profile are
        // what a relying party asks for, and make authentik evaluate
        // their scope mappings on every call as it would for one.
        scope: "openid email profile",
    });
}

// The first grant creates the service account; one request here keeps the
// VUs from racing that create.
export function setup() {
    requireReusable(clientCredentials(newTrace(kind)));
}

const accessToken = reusableToken(clientCredentials);

export default iteration(() => {
    const trace = newTrace(kind);
    const res = http.get(userinfoURL, params(trace, { headers: { Authorization: `Bearer ${accessToken.get(trace)}` } }));
    // preferred_username comes from the profile mapping: without it the
    // provider is not evaluating the scopes this test is about (authentik
    // quietly drops a scope the provider does not map).
    const ok = check(res, {
        "userinfo returned the profile claims": (r) => r.status === 200 && typeof r.json("sub") === "string" && typeof r.json("preferred_username") === "string",
    });
    // 401: authentik refused the token itself, so get another. An error or
    // a timeout says nothing about the token.
    if (res.status === 401) {
        accessToken.reset();
    }
    return ok;
});
