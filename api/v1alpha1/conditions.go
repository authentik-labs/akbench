package v1alpha1

// Condition types shared across bench.goauthentik.io kinds.
const (
	// ConditionReady is True once a resource has fully reconciled to its
	// desired state.
	ConditionReady = "Ready"
	// ConditionPostgresReady is True once an AuthentikDeployment's postgres
	// backend (embedded/CNPG/external) is reachable.
	ConditionPostgresReady = "PostgresReady"
	// ConditionHelmReleaseReady mirrors the status of the HelmRelease an
	// AuthentikDeployment owns.
	ConditionHelmReleaseReady = "HelmReleaseReady"
	// ConditionBlueprintApplied is True once the fixtures blueprint (if any
	// is configured) has been applied against this deployment's authentik
	// instance - True with no work done if none is configured.
	ConditionBlueprintApplied = "BlueprintApplied"
	// ConditionTenantSeeded is True once an AuthentikDeployment's tenant
	// (spec.tenant) is seeded - True with no work done if none is
	// configured. False with ReasonTenantResetRequired when what was seeded
	// no longer matches spec.tenant, its preset or the password; the
	// deployment stays Ready then, since its data is intact.
	ConditionTenantSeeded = "TenantSeeded"
	// ConditionDeploymentReady is True once a TestCase's AuthentikDeployment is
	// itself Ready.
	ConditionDeploymentReady = "DeploymentReady"
	// ConditionScriptReady is True once a TestCase's k6 script has been
	// resolved and mirrored to the runner cluster.
	ConditionScriptReady = "ScriptReady"
	// ConditionBurnStaged is True once every target of a Burn TestCase is
	// Ready and its k6 pod is up, waiting at the start gate. A parent
	// TestCase opens its children's shared start gate once all of them
	// report it.
	ConditionBurnStaged = "BurnStaged"
)

// Condition reasons shared across bench.goauthentik.io kinds.
const (
	ReasonReconciling        = "Reconciling"
	ReasonReconcileError     = "ReconcileError"
	ReasonReady              = "Ready"
	ReasonNotImplemented     = "NotImplemented"
	ReasonDependencyNotReady = "DependencyNotReady"
	ReasonResetting          = "Resetting"
	ReasonArchived           = "Archived"
	// ReasonSeedingFailed: the tenant seeding Job failed. A reset retries.
	ReasonSeedingFailed = "SeedingFailed"
	// ReasonTenantResetRequired: the seeded tenant no longer matches what
	// the deployment asks for; only a reset seeds it again.
	ReasonTenantResetRequired = "TenantResetRequired"
)
