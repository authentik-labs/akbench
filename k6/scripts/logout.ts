import { benchOptions, expectRedirect, flow, iteration, loadUsers, loginStages, newTrace, userFor, walkFlow } from "./bench.ts";

// Logs in, then logs out through default-invalidation-flow. Each logout
// deletes an AuthenticatedSession, and its pre_delete signal queues one
// outpost_session_end task, which broadcasts to every channel in the
// outposts' channel-layer groups (the embedded outpost is always one). A
// broadcast inserts one django_channels_postgres_message row per channel,
// expired and ghost channels included, so restarting the server pods during
// a burn run shows the fan-out: watch the AuthentikBenchChannelMessageBacklog
// and AuthentikBenchGroupChannelBacklog alerts.
const invalidationFlow = "default-invalidation-flow";
const users = loadUsers();

export const options = benchOptions();

export default iteration(() => {
    const trace = newTrace("logout");
    const login = { slug: flow };
    expectRedirect(login, walkFlow(trace, login, loginStages(userFor(users))));
    // The logout stage has no challenge, so the flow ends in a redirect at once.
    const logout = { slug: invalidationFlow };
    expectRedirect(logout, walkFlow(trace, logout, {}));
    return true;
});
