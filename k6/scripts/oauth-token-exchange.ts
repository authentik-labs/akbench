import { benchOptions, iteration, newTrace, requestToken } from "./bench.ts";
import type { Trace } from "./bench.ts";

// RFC 8693 token exchange at authentik's token endpoint: a client_credentials
// access token from the oauth-token dependency's first client is the
// subject_token, exchanged for an access token of the second client, which
// trusts the first through jwt_federation_providers. Each VU reuses its
// subject token until shortly before it expires, so iterations measure the
// exchange alone.
const clientID = __ENV.OAUTH_CLIENT_ID || "";
const clientSecret = __ENV.OAUTH_CLIENT_SECRET || "";
const exchangeClientID = __ENV.OAUTH_EXCHANGE_CLIENT_ID || "";
const exchangeClientSecret = __ENV.OAUTH_EXCHANGE_CLIENT_SECRET || "";
if (!clientID || !clientSecret || !exchangeClientID || !exchangeClientSecret) {
    throw new Error("OAUTH_(EXCHANGE_)CLIENT_ID/SECRET are required for the oauth-token-exchange template - does the template list the oauth-token dependency?");
}
const accessTokenType = "urn:ietf:params:oauth:token-type:access_token";

export const options = benchOptions();

function clientCredentials(trace: Trace) {
    return requestToken(trace, "client_credentials", { grant_type: "client_credentials", client_id: clientID, client_secret: clientSecret });
}

// The first grant creates the service account; one request here keeps the
// VUs from racing that create.
export function setup() {
    clientCredentials(newTrace("oauth-token-exchange"));
}

let subject = "";
let subjectExpires = 0;

export default iteration(() => {
    const trace = newTrace("oauth-token-exchange");
    if (Date.now() >= subjectExpires) {
        const res = clientCredentials(trace);
        subject = res.access_token;
        subjectExpires = Date.now() + (res.expires_in - 30) * 1000;
    }
    const res = requestToken(trace, "token_exchange", {
        grant_type: "urn:ietf:params:oauth:grant-type:token-exchange",
        client_id: exchangeClientID,
        client_secret: exchangeClientSecret,
        subject_token: subject,
        subject_token_type: accessTokenType,
    });
    return res.access_token !== "" && res.issued_token_type === accessTokenType;
});
