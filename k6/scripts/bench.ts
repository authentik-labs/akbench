// Shared library for akbench k6 scripts. The operator mounts it next to every
// script (TestCaseTemplate, inline or BYO), so a script imports it as
// "./bench.ts". It is reset to the operator's embedded copy on every run, so
// changes belong in a script, not here.
import http from "k6/http";
import type { Params, Response } from "k6/http";
import { check, fail } from "k6";
import { SharedArray } from "k6/data";
import { Rate } from "k6/metrics";
import type { Options, PerVUIterationsScenario, Scenario, Threshold } from "k6/options";

export interface User {
    username: string;
    email: string;
    password: string;
}

export const host = __ENV.BENCH_HOST || "localhost";
export const port = __ENV.BENCH_PORT || "9000";
export const authentikBase = `http://${host}:${port}`;
export const vus = Number(__ENV.VUS || 8);
export const flow = __ENV.FLOW || "default-authentication-flow";

// Identity of this test. testid is what the dashboards split on, target names
// the authentik host being driven, so several tests can share a Prometheus
// without their series merging. The runner also passes both as global --tag
// arguments, which covers the metrics k6 emits outside a scenario.
export const testid = __ENV.TEST_ID || "local";
export const target = __ENV.TARGET || host;
export const profile = __ENV.PROFILE || "quick";

// Share of iterations that get a sampled traceparent. authentik's sampler is
// parent-based, so this decides what ends up in Tempo; the header is cheap and
// simply ignored by an authentik that is not instrumented.
const traceSampleRate = Number(__ENV.TRACE_SAMPLE_RATE || 0);

// profileScenario is the load shape PROFILE selects: fixed work per VU rather
// than a fixed run length, so runs stay comparable. maxDuration only bites
// when the target is too slow to finish the iterations. burn keeps looping
// until its container is stopped - per-vu-iterations needs a finite count, so
// it gets one no run will reach.
export function profileScenario(): PerVUIterationsScenario {
    const iterations = Number(__ENV.ITERATIONS || 1000);
    const profiles: Record<string, PerVUIterationsScenario> = {
        quick: { executor: "per-vu-iterations", vus: vus, iterations: iterations, maxDuration: __ENV.QUICK_DURATION || "150s" },
        burn: { executor: "per-vu-iterations", vus: vus, iterations: 1000000000, maxDuration: __ENV.BURN_DURATION || "8760h" },
    };
    const scenario = profiles[profile];
    if (!scenario) {
        throw new Error(`Unknown PROFILE "${profile}", expected one of: ${Object.keys(profiles).join(", ")}`);
    }
    return scenario;
}

// benchOptions names the scenario after the test, so the scenario label
// identifies it as well, and adds the iteration_failed verdict (see
// iteration). thresholds are merged over it.
export function benchOptions(scenario: Scenario = profileScenario(), thresholds: Record<string, Threshold[]> = {}): Options {
    return {
        thresholds: { iteration_failed: ["rate<0.05"], ...thresholds },
        scenarios: {
            [testid]: { ...scenario, tags: { testid: testid, target: target, profile: profile } },
        },
    };
}

// loadUsers reads the credentials generated alongside the test-data
// blueprint. Call it from the script's init context; SharedArray keeps a
// single copy in memory regardless of VU count.
export function loadUsers(): User[] {
    const file = __ENV.USERS_FILE || "./users.json";
    const users = new SharedArray<User>("users", () => JSON.parse(open(file)) as User[]);
    if (users.length === 0) {
        throw new Error(`No credentials found in ${file}`);
    }
    return users as unknown as User[];
}

// userFor walks the user list so concurrent VUs never authenticate as the
// same user. stride is the highest VU count the scenario reaches.
export function userFor(users: User[], stride: number = vus): User {
    return users[(__VU - 1 + __ITER * stride) % users.length];
}

// Share of iterations that did not complete, whether a check failed or fail()
// aborted them midway (which the built-in checks rate never sees). A quick
// run over the threshold exits non-zero, which fails its TestRun.
const iterationFailed = new Rate("iteration_failed");

// iteration wraps a script's iteration, which returns whether it completed,
// into its default function: `export default iteration(() => ...)`.
export function iteration(run: () => boolean): () => void {
    return () => {
        let ok = false;
        try {
            ok = run();
        } finally {
            iterationFailed.add(!ok);
        }
    };
}

function hex(bytes: number): string {
    let out = "";
    for (let i = 0; i < bytes; i++) {
        out += ((Math.random() * 256) | 0).toString(16).padStart(2, "0");
    }
    return out;
}

// Trace carries one iteration's request context: a cookie jar, and one trace
// per iteration so all of its requests end up in the same trace in Tempo.
// k6 emits no spans of its own, so the flags byte is the whole sampling
// decision: authentik's parent-based sampler keeps 01 and drops 00. The
// X-Benchmark-* headers are recorded as span attributes by authentik, so its
// spans say which test caused them.
export interface Trace {
    id: string;
    sampled: boolean;
    jar: http.CookieJar;
    headers: Record<string, string>;
    tags: Record<string, string>;
}

// newTrace starts an iteration. kind names it in the sampled log line, which
// Grafana's Loki datasource turns into a link to the trace. Trace IDs stay on
// Math.random(): they only need to be unique.
export function newTrace(kind: string, tags: Record<string, string> = {}): Trace {
    const trace: Trace = {
        id: hex(16),
        sampled: Math.random() < traceSampleRate,
        jar: new http.CookieJar(),
        headers: { Accept: "*/*", "X-Benchmark-Test": testid, "X-Benchmark-Target": target },
        tags: tags,
    };
    if (trace.sampled) {
        console.log(`sampled ${kind} testid=${testid} target=${target} traceID=${trace.id}`);
    }
    return trace;
}

// params gives the next request its own span ID within the iteration's trace
// and returns its request params. redirects: 0 is for hops whose Location
// header the script reads itself.
export function params(trace: Trace, extra: Params = {}): Params {
    trace.headers.traceparent = `00-${trace.id}-${hex(8)}-0${trace.sampled ? 1 : 0}`;
    return { jar: trace.jar, tags: trace.tags, ...extra, headers: { ...trace.headers, ...extra.headers } };
}

// resolveURL makes a Location header absolute against the URL that returned
// it. k6 has no WHATWG URL API, and authentik's own redirects are relative.
export function resolveURL(base: string, location: string): string {
    if (/^https?:\/\//.test(location)) {
        return location;
    }
    const origin = (base.match(/^https?:\/\/[^/]+/) as RegExpMatchArray)[0];
    if (location.startsWith("/")) {
        return origin + location;
    }
    const path = base.slice(origin.length).split("?")[0];
    return origin + path.slice(0, path.lastIndexOf("/") + 1) + location;
}

// FlowRef is an authentik flow to walk, and the querystring of the flow
// interface URL that sent the user there.
export interface FlowRef {
    slug: string;
    query?: string;
}

export function flowRef(url: string): FlowRef | undefined {
    const match = url.match(/\/if\/flow\/([^/?#]+)\/?(?:\?([^#]*))?/);
    if (!match) {
        return undefined;
    }
    return { slug: match[1], query: match[2] || "" };
}

// followToFlow chases plain redirects by hand (requests made with
// redirects: 0) until one points at an authentik flow interface, and returns
// that flow instead of loading its HTML shell.
export function followToFlow(trace: Trace, res: Response, step: string): FlowRef {
    for (let hop = 0; hop < 10; hop++) {
        if (res.status < 300 || res.status >= 400) {
            fail(`${step}: expected a redirect, got status ${res.status} from ${res.url}`);
        }
        const location = res.headers["Location"];
        if (!location) {
            fail(`${step}: redirect from ${res.url} had no Location header`);
        }
        const next = resolveURL(res.url, location);
        const flow = flowRef(next);
        if (flow) {
            return flow;
        }
        res = http.get(next, params(trace, { redirects: 0 }));
    }
    fail(`${step}: more than 10 redirects without reaching an authentik flow`);
}

// A stage answer: the payload fields to submit for a flow-executor challenge,
// keyed by its component. A handler can fail() to reject a stage outright.
export type Stages = Record<string, (res: Response) => Record<string, unknown>>;

// loginStages answers a username/password (+ optional static-token MFA)
// login, approves consent and declines remember-me.
export function loginStages(user: User, password: string = user.password): Stages {
    return {
        "ak-stage-identification": () => ({ uid_field: user.username }),
        "ak-stage-password": () => ({ password: password }),
        "ak-stage-authenticator-validate": () => ({ code: "staticToken" }),
        // Approving is re-submitting the component name.
        "ak-stage-consent": () => ({}),
        "ak-stage-user-login": () => ({ remember_me: false }),
    };
}

// FlowResult is how a flow walk ended: the flow's final redirect, an
// auto-submitting form (SAMLFlowFinalView for an HTTP-POST binding), or a
// stage that came back again after being submitted.
export type FlowResult =
    | { kind: "redirect"; to: string }
    | { kind: "autosubmit"; url: string; attrs: Record<string, string> }
    | { kind: "rejected"; component: string; errors: unknown };

// walkFlow drives one flow-executor sequence to its end, re-attaching
// ?query=<the flow URL's querystring> to every request so the flow resumes
// what sent the user there.
//
// A stage that comes back right after being submitted ends the walk as
// "rejected": a wrong password does that on purpose, but in a login that is
// supposed to succeed it is almost always a failed submission (e.g. a stale
// or wrong static token under concurrent load) - see expectRedirect. A normal
// login is ~3-4 requests, so more than 10 is a stuck flow.
//
// Redirects are followed here: the executor answers some requests with a bare
// same-URL redirect (authentik/flows/views/executor.py's to_stage_response(),
// whenever a flow's plan has to be (re)initiated for this session) that a
// client is expected to follow, not treat as a failure.
export function walkFlow(trace: Trace, flow: FlowRef, stages: Stages): FlowResult {
    const url = `${authentikBase}/api/v3/flows/executor/${flow.slug}/` + (flow.query ? `?query=${encodeURIComponent(flow.query)}` : "");
    let res = http.get(url, params(trace));
    let previous: string | undefined;
    for (let requests = 0; requests <= 10; requests++) {
        check(res, { "status is 200": (r) => r.status === 200 });
        if (res.status !== 200) {
            fail(`Flow executor for ${flow.slug} returned ${res.status} (last stage: ${previous ?? "none"}).`);
        }
        const component = res.json("component") as string;
        if (component === "xak-flow-redirect") {
            return { kind: "redirect", to: res.json("to") as string };
        }
        if (component === "ak-stage-autosubmit") {
            return { kind: "autosubmit", url: res.json("url") as string, attrs: res.json("attrs") as Record<string, string> };
        }
        if (component === previous) {
            return { kind: "rejected", component: component, errors: res.json("response_errors") };
        }
        previous = component;
        const answer = stages[component];
        if (!answer) {
            fail(`Unknown flow stage component in ${flow.slug}: ${component}`);
        }
        const payload = JSON.stringify({ component: component, ...answer(res) });
        res = http.post(url, payload, params(trace, { headers: { "Content-Type": "application/json" } }));
    }
    fail(`Flow ${flow.slug} made more than 10 queries without completing (last stage: ${previous ?? "none"}).`);
}

// expectRedirect returns where a flow that must succeed redirected to, and
// fails the iteration with the stage that would not advance otherwise.
export function expectRedirect(flow: FlowRef, result: FlowResult): string {
    check(result, { "flow completed": (r) => r.kind === "redirect" });
    if (result.kind === "rejected") {
        fail(
            `Flow stage "${result.component}" in ${flow.slug} was submitted but the flow did not advance (errors: ${JSON.stringify(result.errors ?? {})}) - ` +
                "likely a failed submission (e.g. a stale or wrong static token) the flow re-rendered instead of erroring."
        );
    }
    if (result.kind !== "redirect") {
        fail(`Flow ${flow.slug} ended in ${result.kind}, expected a redirect.`);
    }
    return result.to;
}

// tokenURL is authentik's OAuth2 token endpoint, shared by every provider.
export const tokenURL = `${authentikBase}/application/o/token/`;

export interface TokenResponse {
    access_token: string;
    expires_in: number;
    issued_token_type?: string;
}

// requestToken POSTs a grant to the token endpoint (form-encoded, client
// authentication in the body) and fails the iteration unless a token comes
// back.
export function requestToken(trace: Trace, grant: string, form: Record<string, string>): TokenResponse {
    const res = http.post(tokenURL, form, params(trace, { tags: { ...trace.tags, grant: grant } }));
    check(res, { [`${grant} issued a token`]: (r) => r.status === 200 });
    if (res.status !== 200) {
        fail(`${grant}: token endpoint returned ${res.status}: ${res.body}`);
    }
    return res.json() as unknown as TokenResponse;
}

// The oidc-client and saml-client dependencies' relying parties; empty when
// the template doesn't list them.
export const oidcTestClient = __ENV.OAUTH_TEST_CLIENT_HOST ? `http://${__ENV.OAUTH_TEST_CLIENT_HOST}:${__ENV.OAUTH_TEST_CLIENT_PORT || "9009"}` : "";
export const samlTestSP = __ENV.SAML_TEST_SP_HOST ? `http://${__ENV.SAML_TEST_SP_HOST}:${__ENV.SAML_TEST_SP_PORT || "9009"}` : "";

// oidcLogin drives an OAuth2/OIDC authorization-code flow through an oidc-test-client
// relying party (github.com/BeryJu/oidc-test-client) sitting in front of
// authentik, reusing the same authentik flow-executor stage walk loginStages
// answers (identification -> password -> optional MFA), plus
// the extra request-chasing an OAuth authorization needs on top of a plain
// login. The mechanical sequence below - which fields/headers to read at
// each step, which hops are plain 302s vs. JSON "xak-flow-redirect"
// challenges, and that the flow-executor API needs the *same*
// `query=<...>` querystring re-attached to every request in the walk - was
// verified directly against authentik's own source
// (authentik/providers/oauth2/views/authorize.py, authentik/policies/views.py,
// authentik/flows/planner.py, authentik/flows/views/executor.py), not
// assumed or copied from anywhere else, since no OAuth-driving script
// existed anywhere in this project's history to crib from.
//
// The whole redirect-chasing mechanic (steps 1-5 below) was exercised
// end-to-end against a throwaway mock authentik+oidc-test-client, and later
// against a real live cluster, before this was considered done, not just
// reasoned about from source - this caught several real bugs a source read
// alone wouldn't have:
//   - k6 (0.57.0) has no global URL/WHATWG URL API (splitPathAndQuery/
//     resolveLocation below exist because of this).
//   - Every Location/"to" value authentik itself emits here is a relative
//     path (Django's request.get_full_path() never includes scheme/host),
//     not absolute, so each one needs resolving against BENCH_HOST/PORT
//     before it can be requested directly.
//   - The flow-executor API answers some requests with a bare same-URL
//     redirect that must be followed - see bench.ts's walkFlow.
//   - default-provider-authorization-implicit-consent (the flow the OAuth2
//     provider's own `authorization_flow` names) is NOT stage-less despite
//     the name - confirmed live that it needs its own flow-executor walk
//     (an "ak-stage-consent" stage) exactly like the initial authentication
//     flow, not a direct 302 straight to redirect_uri.
//   - Under real concurrent load (multiple VUs authorizing at once), that
//     second (consent) flow-executor walk can occasionally 500 - root-caused
//     to a real, upstream authentik concurrency bug (not something this
//     script can work around): authentik/flows/planner.py's FlowPlan cache
//     key collapses to one shared key per flow for *all* anonymous sessions
//     (request.user.pk is None for every anonymous requester), with no
//     locking around the read-modify-write, so a plan built for one
//     anonymous session's request context can leak into a concurrent,
//     unrelated one. Left as a real, surfaced benchmark finding rather than
//     retried/hidden here - a transient 500 under concurrent authorization
//     load is exactly the kind of thing this benchmark exists to catch.
//
// One link in this chain is still taken on faith rather than verified live:
// oidc-test-client's own "/" handler is assumed to build the OAuth2
// authorize URL from its own OIDC_CLIENT_ID/OIDC_PROVIDER/OIDC_ROOT_URL and
// issue a redirect there (the standard shape for a minimal OIDC relying
// party) - confirm this against the real image first if the flow doesn't
// complete as expected.
//
// The full sequence, for reference:
//   0. GET oidc-test-client "/" -> 302 to authentik's own
//      /application/o/authorize/?client_id=...&redirect_uri=...&state=...
//      (built by oidc-test-client itself - never constructed here).
//   1. GET that authorize URL, unauthenticated -> 302 to
//      /if/flow/<authentication-flow-slug>/?<original params>&next=<...>
//      (authentik/policies/views.py's handle_no_permission()) - the slug
//      and exact next= value are config/session-dependent and must be read
//      from this Location header, not guessed.
//   2. Walk /api/v3/flows/executor/<slug>/ via walkFlow() exactly like
//      login.ts's own inline loop, but with ?query=<the querystring from
//      step 1's Location> re-attached to every GET/POST - this is what
//      makes the flow's own completion redirect back into the OAuth2
//      authorization instead of authentik's root page
//      (authentik/flows/views/executor.py's _flow_done()).
//   3. The flow's completing "xak-flow-redirect" JSON body's `to` field is
//      the same authorize URL from step 0/1, now with an authenticated
//      session.
//   4. GET that `to` URL -> either a direct 302 straight to
//      oidc-test-client's own redirect_uri with the real authorization
//      code, or (confirmed live: always, given oidc-test-client's own
//      prompt=consent) another /if/flow/.../ redirect for the provider's
//      own authorization_flow - walked via walkFlow() exactly like step 2,
//      looping in case that ever chains further.
//   5. GET that redirect_uri (oidc-test-client's own /auth/callback) with
//      redirects followed normally - it exchanges the code for tokens
//      server-side; a 200 here confirms the whole round trip completed.
export function oidcLogin(trace: Trace, stages: Stages): boolean {
    // Only for the OAuth-level navigation hops, where Location headers are
    // read by hand - never for a flow-executor walk itself.
    const nav = () => params(trace, { redirects: 0 });

    // Step 0/1.
    let res = http.get(`${oidcTestClient}/`, nav());
    let flow = followToFlow(trace, res, 'oidc-test-client "/"');

    // Step 2/3.
    let to = expectRedirect(flow, walkFlow(trace, flow, stages));

    // Step 4. Loop in case a policy chains further.
    let callbackURL: string | undefined;
    for (let hop = 0; hop < 3 && !callbackURL; hop++) {
        res = http.get(resolveURL(authentikBase, to), nav());
        if (res.status < 300 || res.status >= 400 || !res.headers["Location"]) {
            fail(`re-authorizing did not redirect (status ${res.status}).`);
        }
        const location = resolveURL(res.url, res.headers["Location"]);
        if (location.includes("code=")) {
            callbackURL = location;
            break;
        }
        flow = flowRef(location) ?? fail(`could not extract a flow slug from ${location}`);
        to = expectRedirect(flow, walkFlow(trace, flow, stages));
    }
    if (!callbackURL) {
        fail(`expected a redirect to redirect_uri with an authorization code, last redirect was ${to}`);
    }

    // Step 5.
    res = http.get(callbackURL, params(trace));
    return check(res, {
        "oauth flow completed": (r) => r.status === 200,
    });
}

// samlLogin drives an SP-initiated SAML login through a saml-test-sp service provider
// (github.com/BeryJu/saml-test-sp, crewjam/saml) sitting in front of
// authentik, reusing the flow-executor stage walk. The sequence was
// read off authentik's own source (authentik/providers/saml/views/sso.py,
// authentik/policies/views.py, authentik/flows/planner.py) and
// saml-test-sp/crewjam's samlsp middleware:
//   0. GET saml-test-sp "/" -> 302 to authentik's
//      /application/saml/<slug>/sso/binding/redirect/?SAMLRequest=...&RelayState=...
//      (the SP prefers the Redirect binding authentik's metadata advertises),
//      plus a request-tracking cookie on the SP.
//   1. GET that, unauthenticated -> 302 to /if/flow/<authentication-flow>/?...&next=<1>.
//   2. Walk the authentication flow (identification -> password -> optional
//      MFA) with ?query=<that querystring>, ending in a redirect back to <1>.
//   3. GET <1> again, now authenticated -> 302 to the provider's
//      authorization flow. Its plan ends in SAMLFlowFinalView, which for the
//      provider's sp_binding=post answers with an "ak-stage-autosubmit"
//      challenge: the SP's ACS URL plus SAMLResponse/RelayState.
//   4. POST those to the SP's /saml/acs (what the browser's auto-submitting
//      form does) -> 302 to "/" with the SP's session cookie -> the SP's JSON
//      session page.
export function samlLogin(trace: Trace, stages: Stages): boolean {
    // Step 0/1.
    const res = http.get(`${samlTestSP}/`, params(trace, { redirects: 0 }));
    const loginFlow = followToFlow(trace, res, 'saml-test-sp "/"');

    // Step 2.
    let result = walkFlow(trace, loginFlow, stages);

    // Step 3. Normally one more hop (the authorization flow); looped in case
    // a policy chains further.
    for (let hop = 0; hop < 3 && result.kind === "redirect"; hop++) {
        const sso = http.get(resolveURL(authentikBase, result.to), params(trace, { redirects: 0 }));
        result = walkFlow(trace, followToFlow(trace, sso, "SAML SSO endpoint"), stages);
    }
    if (result.kind !== "autosubmit") {
        fail(`expected the authorization flow to end in an ak-stage-autosubmit to the SP's ACS, got ${JSON.stringify(result)}`);
    }

    // Step 4.
    const acs = http.post(result.url, result.attrs, params(trace));
    return check(acs, {
        "saml flow completed": (r) => r.status === 200 && r.json("saml-session") === true,
    });
}
