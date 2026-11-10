import { benchOptions, expectRedirect, iteration, loadUsers, loginStages, newTrace, userFor, walkFlow } from "./bench.ts";

// Logs in through the policy-flows dependency's prompt flow: identification,
// password, a prompt stage with one newsletter checkbox, then login. Here
// the prompt stage's validation_policies run an orchestrator policy, which
// calls two policies with ak_call_policy. authentik runs validation_policies on
// an unsaved PolicyBindingModel with a random pk, so each login adds a new
// obj_pk label set to authentik_policies_engine_time_total_seconds, and each
// ak_call_policy result is cached under a key that is never read again.
// Run with the other *-policy-login template on a separate deployment of the
// same build and compare policy engine series, policy cache keys
// (authentik_policies_cached) and /metrics scrape time.
const policyFlow = __ENV.POLICY_PROMPT_FLOW || "";
if (!policyFlow) {
    throw new Error("POLICY_PROMPT_FLOW is required - does the template list the policy-flows dependency?");
}
const users = loadUsers();

export const options = benchOptions();

export default iteration(() => {
    const ref = { slug: policyFlow };
    const stages = { ...loginStages(userFor(users)), "ak-stage-prompt": () => ({ newsletter: false }) };
    expectRedirect(ref, walkFlow(newTrace("prompt-policy-login"), ref, stages));
    return true;
});
