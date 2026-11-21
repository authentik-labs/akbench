// Library for akbench k6 scripts that run against a seeded tenant: a template
// with the seeded-tenant dependency, on a deployment with a tenant preset.
// The operator mounts it next to every script, like bench.ts, so a script
// imports it as "./tenant.ts". It is reset to the operator's embedded copy on
// every run, so changes belong in a script, not here.
//
// A tenant's users and groups are not listed anywhere. The seeder
// (seeder/seed.py) names each after its index, so the tenant's numbers,
// which the k6 Job gets as TENANT_* env vars, are enough to address every
// one of them, however many there are.
import exec from "k6/execution";
import type { User } from "./bench.ts";

// Tenant is what the target deployment was seeded with: the numbers of its
// TenantPreset at the time.
export interface Tenant {
    seed: number;
    users: number;
    groups: number;
    groupDepth: number;
    groupsPerUser: number;
}

export const tenant: Tenant = {
    seed: Number(__ENV.TENANT_SEED || 0),
    users: Number(__ENV.TENANT_USERS || 0),
    groups: Number(__ENV.TENANT_GROUPS || 0),
    groupDepth: Number(__ENV.TENANT_GROUP_DEPTH || 0),
    groupsPerUser: Number(__ENV.TENANT_GROUPS_PER_USER || 0),
};

// Every seeded user has the same password.
const password = __ENV.TENANT_PASSWORD || "";

// The seeder's naming, which test/contract checks against seed.py: a prefix
// and the index, zero-padded.
const userPrefix = "u";
const groupPrefix = "g";
const nameDigits = 6;
const emailDomain = "tenant.example";

// loadTenant returns the tenant, and fails the run at once when there is
// none, with what to do about it, rather than on every iteration. Call it
// from the script's init context, like loadUsers.
export function loadTenant(): Tenant {
    if (tenant.users < 1 || password === "") {
        throw new Error(
            "No seeded tenant: this script needs a template with the seeded-tenant dependency, run against a deployment that has a tenant preset."
        );
    }
    return tenant;
}

function seededName(prefix: string, index: number, count: number, kind: string): string {
    if (!Number.isInteger(index) || index < 0 || index >= count) {
        throw new Error(`No ${kind} ${index}: the tenant has ${count}, numbered from 0.`);
    }
    return prefix + String(index).padStart(nameDigits, "0");
}

// tenantUser is the index-th seeded user, counting from 0. One user in
// twenty is of the external type; all are active.
export function tenantUser(index: number): User {
    const username = seededName(userPrefix, index, tenant.users, "user");
    return { username: username, email: `${username}@${emailDomain}`, password: password };
}

// tenantGroup is the name of the index-th seeded group, counting from 0.
// The low-numbered groups are the large ones. With a group depth above 1,
// group 0 is the root of the group tree, which every user in a group belongs
// to through it.
export function tenantGroup(index: number): string {
    return seededName(groupPrefix, index, tenant.groups, "group");
}

// tenantUserFor walks the whole tenant, one user per iteration in the order
// iterations start, so no user comes round again until every other one has
// had a turn. On a tenant much larger than a pass's worth of iterations,
// that keeps what authentik caches per user cold, where the 50 fixture users
// all come round again within seconds. Concurrent VUs never use the same user
// unless there are more VUs than users. A restarted k6 pod starts over from
// user 0.
export function tenantUserFor(): User {
    return tenantUser(exec.scenario.iterationInTest % tenant.users);
}
