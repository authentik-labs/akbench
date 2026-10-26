import http from "k6/http";
import { check } from "k6";
import type { Trace, User } from "./bench.ts";
import {
    authentikBase,
    benchOptions,
    flow,
    iteration,
    loadUsers,
    loginStages,
    newTrace,
    oidcLogin,
    oidcTestClient,
    params,
    samlLogin,
    samlTestSP,
    userFor,
    walkFlow,
} from "./bench.ts";

// Mixed-usage test plan: each iteration picks one of several realistic
// authentik interactions (see `actions` below) instead of running the same
// login over and over. The pick is drawn from a seeded PRNG, not
// Math.random(), so a TestCase replays the exact same action sequence on
// every run and against every target it drives - SEED is fixed per TestCase
// by the operator (TestCase.Seed: spec.seed, or a hash of the TestCase's own
// name), which keeps runs of this template comparable with each other the
// same way login.ts's fixed per-VU work already does.
const seed = Number(__ENV.SEED || 0) >>> 0;

const users = loadUsers();

export const options = benchOptions();

// mulberry32: small, fast, good-enough 32-bit PRNG - deterministic for a
// given seed, which is the whole point here.
function mulberry32(a: number): () => number {
    return function (): number {
        a = (a + 0x6d2b79f5) | 0;
        let t = Math.imul(a ^ (a >>> 15), 1 | a);
        t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
        return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
}

// One independent stream per VU, derived from the TestCase seed, so a VU's
// sequence doesn't depend on how fast the other VUs happen to run. Created
// lazily: __VU is only meaningful inside the VU's own default function.
// Trace IDs stay off this stream, so toggling the trace sample rate never
// shifts the action sequence.
let rng: (() => number) | undefined;
function random(): number {
    if (!rng) {
        rng = mulberry32((seed ^ Math.imul(__VU, 0x9e3779b1)) >>> 0);
    }
    return rng();
}

// login walks the authentication flow, optionally submitting a wrong
// password - in which case it must end at a password challenge that came
// back with errors. Returns whether the flow ended where expected.
function login(trace: Trace, user: User, wrongPassword: boolean): boolean {
    const stages = loginStages(user, wrongPassword ? `${user.password}-wrong` : user.password);
    const result = walkFlow(trace, { slug: flow }, stages);
    if (wrongPassword) {
        return result.kind === "rejected" && result.component === "ak-stage-password" && !!result.errors;
    }
    return result.kind === "redirect";
}

// The pages an already-logged-in user's browser hits right after landing on
// the user interface - authenticated by the session cookie login() left in
// the jar.
const browsePaths = ["/api/v3/core/users/me/", "/api/v3/core/applications/"];

function browse(trace: Trace): boolean {
    let ok = true;
    for (const path of browsePaths) {
        const res = http.get(`${authentikBase}${path}`, params(trace));
        ok = check(res, { "api status is 200": (r) => r.status === 200 }) && ok;
    }
    return ok;
}

interface Action {
    name: string;
    weight: number;
    run: (trace: Trace, user: User) => boolean;
}

// The usage mix. Weights are relative, not percentages - edit them (or add
// actions) freely in the TestCaseTemplate; the seeded sequence stays stable
// for a given seed and mix.
const actions: Action[] = [
    { name: "login", weight: 45, run: (trace, user) => login(trace, user, false) },
    { name: "login-browse", weight: 30, run: (trace, user) => login(trace, user, false) && browse(trace) },
    { name: "failed-login", weight: 15, run: (trace, user) => login(trace, user, true) },
    {
        name: "flow-page",
        weight: 10,
        run: (trace) => {
            const res = http.get(`${authentikBase}/if/flow/${flow}/`, params(trace));
            return check(res, { "flow page status is 200": (r) => r.status === 200 });
        },
    },
];
// SSO logins through the oidc-client/saml-client dependencies, when the
// template lists them (mixed-usage-sso) - so the plain mix, and its seeded
// sequence, are unchanged without them.
if (oidcTestClient) {
    actions.push({ name: "oidc", weight: 20, run: (trace, user) => oidcLogin(trace, loginStages(user)) });
}
if (samlTestSP) {
    actions.push({ name: "saml", weight: 20, run: (trace, user) => samlLogin(trace, loginStages(user)) });
}
const totalWeight = actions.reduce((sum, a) => sum + a.weight, 0);

function pickAction(): Action {
    let roll = random() * totalWeight;
    for (const action of actions) {
        roll -= action.weight;
        if (roll < 0) {
            return action;
        }
    }
    return actions[actions.length - 1];
}

export default iteration(() => {
    const action = pickAction();
    // Tagged per action, so dashboards can split latency/errors by it.
    const ok = action.run(newTrace(action.name, { action: action.name }), userFor(users));
    return check(ok, { [`${action.name} completed`]: (c) => c });
});
