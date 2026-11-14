import { AuthentikDeployment, builtInLabel, Condition, TenantPreset, TenantStatus } from "../types";
import {
    tenantChangeNote,
    tenantPresetOptions,
    tenantRecordText,
    tenantSpec,
    tenantSummary,
} from "./Tenant";

import { describe, expect, it } from "vitest";

const preset = (name: string, users: number, builtIn = false): TenantPreset => ({
    metadata: {
        name,
        labels: builtIn ? { [builtInLabel]: "true" } : undefined,
    },
    spec: { users, groups: 10, groupDepth: 2, groupsPerUser: 3 },
});

const seeded = (state: TenantStatus["state"], extra: Partial<TenantStatus> = {}): TenantStatus => ({
    preset: "small",
    params: { seed: 1, users: 1000, groups: 50, groupDepth: 3, groupsPerUser: 4 },
    state,
    ...extra,
});

const deployment = (
    presetName: string | undefined,
    tenant?: TenantStatus,
    conditions?: Condition[],
): AuthentikDeployment => ({
    metadata: { name: "d" },
    spec: {
        namespace: "d",
        server: { replicas: 1 },
        worker: { replicas: 1 },
        postgres: { mode: "Embedded" },
        tenant: presetName ? { presetRef: { name: presetName } } : undefined,
    },
    status: { tenant, conditions },
});

describe("tenantPresetOptions", () => {
    it("lists built-in and own presets, smallest first, with their user count", () => {
        const presets = [
            preset("large", 100_000, true),
            preset("mine", 1),
            preset("small", 1000, true),
        ];
        expect(tenantPresetOptions(presets)).toEqual([
            { value: "mine", label: "mine - 1 user" },
            { value: "small", label: "small - 1,000 users" },
            { value: "large", label: "large - 100,000 users" },
        ]);
    });

    it("keeps a selected preset that no longer exists", () => {
        expect(tenantPresetOptions([preset("small", 1000)], "gone")).toEqual([
            { value: "small", label: "small - 1,000 users" },
            { value: "gone", label: "gone - not found" },
        ]);
    });

    it("keeps the selected preset when the list couldn't be loaded", () => {
        expect(tenantPresetOptions(undefined, "small")).toEqual([
            { value: "small", label: "small" },
        ]);
        expect(tenantPresetOptions(undefined)).toEqual([]);
    });
});

describe("tenantSpec", () => {
    it("names the picked preset", () => {
        expect(tenantSpec("small")).toEqual({ presetRef: { name: "small" } });
    });

    it("leaves the tenant out of the saved spec for None", () => {
        const spec = { ...deployment("small").spec, tenant: tenantSpec("") };
        expect(JSON.parse(JSON.stringify(spec))).not.toHaveProperty("tenant");
    });
});

describe("tenantChangeNote", () => {
    it("says nothing for a new deployment or the same preset", () => {
        expect(tenantChangeNote(undefined, "large")).toBeUndefined();
        expect(tenantChangeNote(seeded("Seeded"), "small")).toBeUndefined();
    });

    it("says another preset takes effect after a reset", () => {
        expect(tenantChangeNote(seeded("Seeded"), "large")).toMatch(
            /large takes effect after a reset/,
        );
        expect(tenantChangeNote(seeded("Seeding"), "large")).toMatch(/after a reset/);
    });

    it("says the seeded data stays when None is picked", () => {
        expect(tenantChangeNote(seeded("Seeded"), "")).toMatch(
            /stays .* until the deployment is reset/,
        );
    });

    it("says nothing after a failure, which is retried with the new preset", () => {
        expect(tenantChangeNote(seeded("Failed"), "large")).toBeUndefined();
    });
});

describe("tenantSummary", () => {
    it("is undefined without a tenant", () => {
        expect(tenantSummary(deployment(undefined))).toBeUndefined();
    });

    it("waits for seeding to start, with the operator's reason", () => {
        expect(
            tenantSummary(
                deployment("small", undefined, [
                    {
                        type: "TenantSeeded",
                        status: "False",
                        reason: "Reconciling",
                        message: "TenantPreset small not found",
                    },
                ]),
            ),
        ).toEqual({ text: "small", state: "Waiting", message: "TenantPreset small not found" });
    });

    it("shows the preset, users and state once seeding started", () => {
        expect(tenantSummary(deployment("small", seeded("Seeding")))).toEqual({
            text: "small - 1,000 users",
            state: "Seeding",
        });
        expect(tenantSummary(deployment("small", seeded("Seeded")))).toEqual({
            text: "small - 1,000 users",
            state: "Seeded",
        });
    });

    it("shows the seeder's message when it failed", () => {
        expect(
            tenantSummary(
                deployment("small", seeded("Failed", { message: "seeding failed: boom" })),
            ),
        ).toEqual({
            text: "small - 1,000 users",
            state: "Failed",
            message: "seeding failed: boom",
        });
    });

    it("waits without a message while nothing holds it up", () => {
        const waiting = { text: "small", state: "Waiting", message: undefined };
        expect(tenantSummary(deployment("small"))).toEqual(waiting);
        expect(
            tenantSummary(
                deployment("small", undefined, [
                    { type: "TenantSeeded", status: "True", reason: "Ready", message: "stale" },
                ]),
            ),
        ).toEqual(waiting);
    });

    it("falls back to the condition's message when the seeder left none", () => {
        const failed: Condition = {
            type: "TenantSeeded",
            status: "False",
            reason: "SeedingFailed",
            message: "the tenant seeding Job failed",
        };
        expect(
            tenantSummary(deployment("small", seeded("Failed", { message: "" }), [failed])),
        ).toEqual({
            text: "small - 1,000 users",
            state: "Failed",
            message: "the tenant seeding Job failed",
        });
        expect(tenantSummary(deployment("small", seeded("Failed")))).toEqual({
            text: "small - 1,000 users",
            state: "Failed",
            message: undefined,
        });
    });

    it("says a tenant no longer selected is still there", () => {
        const holds: Condition = {
            type: "TenantSeeded",
            status: "True",
            reason: "Ready",
            message: 'holds the tenant seeded from preset "small"; none is selected now',
        };
        expect(tenantSummary(deployment(undefined, seeded("Seeded"), [holds]))).toEqual({
            text: "small - 1,000 users",
            state: "Not selected",
            message: holds.message,
        });
        const stillSeeding: Condition = {
            type: "TenantSeeded",
            status: "False",
            reason: "Reconciling",
            message: 'seeding tenant from preset "small" (1000 users, 50 groups)',
        };
        expect(tenantSummary(deployment(undefined, seeded("Seeding"), [stillSeeding]))).toEqual({
            text: "small - 1,000 users",
            state: "Not selected",
            message: stillSeeding.message,
        });
    });

    it("reads reset required from the TenantSeeded condition", () => {
        const resetRequired: Condition = {
            type: "TenantSeeded",
            status: "False",
            reason: "TenantResetRequired",
            message: "preset changed to large; reset the deployment to seed it again",
        };
        expect(tenantSummary(deployment("large", seeded("Seeded"), [resetRequired]))).toEqual({
            text: "small - 1,000 users",
            state: "Reset required",
            message: resetRequired.message,
        });
    });
});

describe("tenantRecordText", () => {
    it("names the preset and its users", () => {
        expect(tenantRecordText(seeded("Seeded"))).toBe("small - 1,000 users");
        expect(
            tenantRecordText({ preset: "one", params: { ...seeded("Seeded").params, users: 1 } }),
        ).toBe("one - 1 user");
    });
});
