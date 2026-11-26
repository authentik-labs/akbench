import { authorizationCode, benchOptions, iteration, loadUsers, newTrace, requestToken, userFor } from "./bench.ts";

// A whole OAuth2 authorization-code login straight against authentik, with
// no relying party in between: /authorize/, the login flow, the provider's
// consent flow, then the code exchanged for tokens at the token endpoint.
//
// With the oauth-policies dependency the application only admits users who
// pass a group binding and an expression policy, which authentik evaluates
// on every authorization request. With the oauth-token dependency instead
// it is the same login to an application without bindings: run both to see
// what the access check costs.
const policies = (__ENV.OAUTH_POLICIES_CLIENT_ID || "") !== "";
const clientID = __ENV.OAUTH_POLICIES_CLIENT_ID || __ENV.OAUTH_CLIENT_ID || "";
const clientSecret = (policies ? __ENV.OAUTH_POLICIES_CLIENT_SECRET : __ENV.OAUTH_CLIENT_SECRET) || "";
const redirectURI = __ENV.OAUTH_REDIRECT_URI || "";
if (!clientID || !clientSecret || !redirectURI) {
    throw new Error(
        "OAUTH_POLICIES_CLIENT_ID/SECRET or OAUTH_CLIENT_ID/SECRET, and OAUTH_REDIRECT_URI, are required - " +
            "does the template list the oauth-policies or the oauth-token dependency?"
    );
}
const kind = policies ? "oauth-code-policies" : "oauth-code";
// Only scopes both dependencies' providers map.
const client = { id: clientID, redirectURI: redirectURI, scope: "openid" };

const users = loadUsers();

export const options = benchOptions();

export default iteration(() => {
    const trace = newTrace(kind);
    const code = authorizationCode(trace, userFor(users), client);
    const res = requestToken(trace, "authorization_code", {
        grant_type: "authorization_code",
        code: code,
        redirect_uri: redirectURI,
        client_id: clientID,
        client_secret: clientSecret,
    });
    return res.access_token !== "";
});
