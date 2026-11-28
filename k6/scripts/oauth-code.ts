import { authorizationCode, benchOptions, iteration, loadUsers, loginClient, newTrace, requestToken, userFor } from "./bench.ts";

// A whole OAuth2 authorization-code login straight against authentik, with
// no relying party in between: /authorize/, the login flow, the provider's
// authorization flow, then the code exchanged for tokens at the token
// endpoint. Every iteration starts a fresh session.
//
// With the oauth-policies dependency the application only admits users who
// pass a group binding and an expression policy, which authentik evaluates
// on every authorization request. With the oauth-token dependency instead
// it is the same login to an application without bindings. The access
// check is one step among a login's many here; oauth-sso.ts repeats only
// the authorization, where it is a much larger share.
// Only a scope both dependencies' providers map.
const { client, secret, policies } = loginClient("openid");
const kind = policies ? "oauth-code-policies" : "oauth-code";

const users = loadUsers();

export const options = benchOptions();

export default iteration(() => {
    const trace = newTrace(kind);
    const code = authorizationCode(trace, userFor(users), client);
    const res = requestToken(trace, "authorization_code", {
        grant_type: "authorization_code",
        code: code,
        redirect_uri: client.redirectURI,
        client_id: client.id,
        client_secret: secret,
    });
    return !!res.access_token;
});
