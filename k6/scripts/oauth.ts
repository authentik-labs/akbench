import { benchOptions, iteration, loadUsers, loginStages, newTrace, oidcLogin, oidcTestClient, userFor } from "./bench.ts";

// An OAuth2/OIDC authorization-code login through the oidc-client
// dependency's oidc-test-client per iteration - see bench.ts's oidcLogin.
if (!oidcTestClient) {
    throw new Error("OAUTH_TEST_CLIENT_HOST is required for the oauth-oidc template - does the template list the oidc-client dependency?");
}

const users = loadUsers();

export const options = benchOptions();

export default iteration(() => oidcLogin(newTrace("oauth"), loginStages(userFor(users))));
