import type { Response } from "k6/http";
import { benchOptions, expectRedirect, flow, iteration, loadUsers, loginStages, newTrace, userFor, walkFlow } from "./bench.ts";

// Logs in, then saves the user settings form unchanged through
// default-user-settings-flow. Its prompt stage runs a validation policy
// through PromptValidationPolicyEngine, which times itself under a new
// obj_pk label on every submission (an unsaved PolicyBindingModel gets a
// fresh UUID). Each save so adds histogram series to the server's
// prometheus multiprocess files in /dev/shm: watch the
// AuthentikBenchPolicyEngineLabelCardinality and
// AuthentikBenchMetricsScrapeSlow alerts on a burn run.
const settingsFlow = "default-user-settings-flow";
const users = loadUsers();

export const options = benchOptions();

interface PromptField {
    field_key: string;
    initial_value: unknown;
}

// Submit every field with the value the form came with, so the
// default-user-settings-authorization policy passes and the user is unchanged.
function unchanged(res: Response): Record<string, unknown> {
    const answer: Record<string, unknown> = {};
    for (const field of res.json("fields") as unknown as PromptField[]) {
        answer[field.field_key] = field.initial_value ?? "";
    }
    return answer;
}

export default iteration(() => {
    const trace = newTrace("user-settings");
    const user = userFor(users);
    const login = { slug: flow };
    expectRedirect(login, walkFlow(trace, login, loginStages(user)));
    const settings = { slug: settingsFlow };
    expectRedirect(settings, walkFlow(trace, settings, { "ak-stage-prompt": unchanged }));
    return true;
});
