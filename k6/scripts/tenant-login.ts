import { benchOptions, expectRedirect, flow, iteration, loginStages, newTrace, walkFlow } from "./bench.ts";
import { loadTenant, tenantUserFor } from "./tenant.ts";

// The same login as login.ts, as a different seeded user every iteration,
// walking the whole tenant (see tenant.ts's tenantUserFor). login.ts cycles
// through 50 users, so everything authentik caches per user stays warm and
// every table is tiny; here a user's turn comes round once per pass over the
// tenant, and lookups run against tables of the preset's size.
loadTenant();

export const options = benchOptions();

export default iteration(() => {
    const ref = { slug: flow };
    expectRedirect(ref, walkFlow(newTrace("tenant-login"), ref, loginStages(tenantUserFor())));
    return true;
});
