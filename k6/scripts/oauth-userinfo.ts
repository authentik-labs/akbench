import http from "k6/http";
import { check } from "k6";
import { benchOptions, iteration, newTrace, params, requestToken, reusableToken, userinfoURL } from "./bench.ts";
import type { Trace } from "./bench.ts";

// What a relying party or API gateway does with an access token on every
// request it serves: fetch the user's claims from authentik's OpenID Connect
// UserInfo endpoint, which looks the token up and evaluates the provider's
// scope mappings for it. The token is a client_credentials access token with
// the openid scope from the oauth-token dependency. Each VU reuses its token
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
        // UserInfo refuses a token without it.
        scope: "openid",
    });
}

// The first grant creates the service account; one request here keeps the
// VUs from racing that create.
export function setup() {
    clientCredentials(newTrace(kind));
}

const accessToken = reusableToken(clientCredentials);

export default iteration(() => {
    const trace = newTrace(kind);
    const res = http.get(userinfoURL, params(trace, { headers: { Authorization: `Bearer ${accessToken(trace)}` } }));
    return check(res, { "userinfo returned the subject": (r) => r.status === 200 && typeof r.json("sub") === "string" });
});
