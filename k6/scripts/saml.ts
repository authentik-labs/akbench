import { benchOptions, iteration, loadUsers, loginStages, newTrace, samlLogin, samlTestSP, userFor } from "./bench.ts";

// An SP-initiated SAML login through the saml-client dependency's
// saml-test-sp per iteration - see bench.ts's samlLogin.
if (!samlTestSP) {
    throw new Error("SAML_TEST_SP_HOST is required for the saml template - does the template list the saml-client dependency?");
}

const users = loadUsers();

export const options = benchOptions();

export default iteration(() => samlLogin(newTrace("saml"), loginStages(userFor(users))));
