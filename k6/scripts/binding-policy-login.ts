import { benchOptions, expectRedirect, iteration, loadUsers, loginStages, newTrace, userFor, walkFlow } from "./bench.ts";

// prompt-policy-login, with the two policies bound to the prompt stage's
// FlowStageBinding instead: the policy engine runs on a saved binding with a
// stable pk. See prompt-policy-login.ts for what to compare.
const policyFlow = __ENV.POLICY_BINDING_FLOW || "";
if (!policyFlow) {
    throw new Error("POLICY_BINDING_FLOW is required - does the template list the policy-flows dependency?");
}
const users = loadUsers();

export const options = benchOptions();

export default iteration(() => {
    const ref = { slug: policyFlow };
    const stages = { ...loginStages(userFor(users)), "ak-stage-prompt": () => ({ newsletter: false }) };
    expectRedirect(ref, walkFlow(newTrace("binding-policy-login"), ref, stages));
    return true;
});
