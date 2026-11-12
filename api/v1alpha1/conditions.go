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
	// ConditionPostgresStorageExhausted is True while an AuthentikDeployment's
	// postgres volume is too full to keep putting load on it; its reason says
	// why (ReasonPostgresStorageAtMax, ReasonPostgresStorageGrowFailed or
	// ReasonPostgresStorageHardLimit). A Burn TestCase stops its k6 load
	// while any of its targets has it.
	ConditionPostgresStorageExhausted = "PostgresStorageExhausted"
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
	// ReasonPostgresStorageFull is a Burn TestCase whose k6 load was stopped
	// because a target's postgres volume is full and cannot grow any further.
	ReasonPostgresStorageFull = "PostgresStorageFull"
)

// Reasons of ConditionPostgresStorageExhausted.
const (
	// ReasonPostgresStorageAtMax is a volume that is full and has finished
	// growing to its maxStorageSize. Only a usage reading of the volume at
	// that size counts, never one taken while it was still growing to it.
	ReasonPostgresStorageAtMax = "AtMaxStorageSize"
	// ReasonPostgresStorageGrowFailed is a volume that is full and could not
	// be grown, e.g. because its StorageClass doesn't allow expansion.
	ReasonPostgresStorageGrowFailed = "GrowFailed"
	// ReasonPostgresStorageHardLimit is a volume nearly out of space whatever
	// its size or maximum: growing it hasn't kept up or never completes.
	ReasonPostgresStorageHardLimit = "HardLimit"
)
