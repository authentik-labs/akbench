import { benchOptions, iteration, newTrace, requestToken } from "./bench.ts";

// Hits only authentik's token endpoint: a client_credentials grant
// authenticated with the oauth-token dependency's client secret, which
// authentik answers by resolving the provider's generated service account,
// checking its application access and issuing a signed access token.
const clientID = __ENV.OAUTH_CLIENT_ID || "";
const clientSecret = __ENV.OAUTH_CLIENT_SECRET || "";
if (!clientID || !clientSecret) {
    throw new Error("OAUTH_CLIENT_ID/SECRET are required for the oauth-client-credentials template - does the template list the oauth-token dependency?");
}
const form = { grant_type: "client_credentials", client_id: clientID, client_secret: clientSecret };

export const options = benchOptions();

// The first grant creates the service account; one request here keeps the
// VUs from racing that create.
export function setup() {
    requestToken(newTrace("oauth-token"), "client_credentials", form);
}

export default iteration(() => requestToken(newTrace("oauth-token"), "client_credentials", form).access_token !== "");
