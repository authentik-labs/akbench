import type { Trace, User } from "./bench.ts";
import { authorizationCode, benchOptions, iteration, loadUsers, loginClient, newTrace, requestToken } from "./bench.ts";

// Single sign-on, the busiest path of a deployment with many applications:
// a user who is already signed in opens another application. Each VU logs
// in once and keeps its session; every iteration after that is only
// /authorize/ and the code exchanged for tokens at the token endpoint.
//
// authentik checks the user's access to the application on every
// authorization request, signed in or not, and never from a cache. With the
// oauth-policies dependency that check is a group binding and an expression
// policy; with the oauth-token dependency the application has no bindings.
// With no login around it the check is a much larger share of the
// iteration than in oauth-code.ts, so the two show what it costs.
//
// Every iteration leaves an "authorize application" event behind (kept a
// year by default) and an access token that lives an hour: a long Burn
// grows the events table faster than a login test does.
// Only a scope both dependencies' providers map.
const { client, secret, policies } = loginClient("openid");
const kind = policies ? "oauth-sso-policies" : "oauth-sso";

const users = loadUsers();

export const options = benchOptions();

// Per VU, for the whole run, set by its first iteration: its user, and the
// cookie jar holding its session. Kept through failed iterations too -
// authorizationCode signs in again by itself if the session is gone, and
// dropping it would turn every failure under load into a full login on top.
let user: User | undefined;
let jar: Trace["jar"] | undefined;

export default iteration(() => {
    const trace = newTrace(kind);
    user = user ?? users[(__VU - 1) % users.length];
    jar = jar ?? trace.jar;
    trace.jar = jar;
    const code = authorizationCode(trace, user, client);
    const res = requestToken(trace, "authorization_code", {
        grant_type: "authorization_code",
        code: code,
        redirect_uri: client.redirectURI,
        client_id: client.id,
        client_secret: secret,
    });
    return !!res.access_token;
});
