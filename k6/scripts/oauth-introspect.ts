import http from "k6/http";
import { check } from "k6";
import { benchOptions, introspectURL, iteration, newTrace, params, requestToken, reusableToken } from "./bench.ts";
import type { Trace } from "./bench.ts";

// What a resource server does with an opaque token on every API call it
// receives: ask authentik's introspection endpoint (RFC 7662) whether it is
// still active. The token is a client_credentials access token from the
// oauth-token dependency, and the same client authenticates the
// introspection. Each VU reuses its token until shortly before it expires,
// so iterations measure the introspection alone.
const clientID = __ENV.OAUTH_CLIENT_ID || "";
const clientSecret = __ENV.OAUTH_CLIENT_SECRET || "";
if (!clientID || !clientSecret) {
    throw new Error("OAUTH_CLIENT_ID/SECRET are required for the oauth-introspection template - does the template list the oauth-token dependency?");
}
const kind = "oauth-introspection";

export const options = benchOptions();

function clientCredentials(trace: Trace) {
    return requestToken(trace, "client_credentials", { grant_type: "client_credentials", client_id: clientID, client_secret: clientSecret });
}

// The first grant creates the service account; one request here keeps the
// VUs from racing that create.
export function setup() {
    clientCredentials(newTrace(kind));
}

const accessToken = reusableToken(clientCredentials);

export default iteration(() => {
    const trace = newTrace(kind);
    const res = http.post(introspectURL, { token: accessToken(trace), client_id: clientID, client_secret: clientSecret }, params(trace));
    // authentik answers 200 {"active": false} for a token it does not accept
    // (and for a client it could not authenticate), so the status alone
    // says nothing.
    return check(res, { "token is active": (r) => r.status === 200 && r.json("active") === true });
});
