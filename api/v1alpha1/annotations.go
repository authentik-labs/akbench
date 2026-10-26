package v1alpha1

// Annotations shared between the web UI backend and the operator.
const (
	// AnnotationResetRequested asks the operator to reset an
	// AuthentikDeployment's state: uninstall its Helm release, delete its
	// database PVCs (and the CNPG Cluster, in CNPG mode) and every blueprint
	// import Job, so the next install starts from an empty database and
	// re-applies the fixtures/OAuth blueprints. The value is the RFC3339
	// request time, for humans only; the operator removes the annotation
	// once the reset is done and the normal reconcile takes over.
	AnnotationResetRequested = "bench.goauthentik.io/reset-requested-at"

	// AnnotationCreatedBy names who created an object through the web UI
	// backend: the authenticated principal, set by cmd/apiserver on create
	// and kept on update so a client can't forge or rewrite it.
	AnnotationCreatedBy = "bench.goauthentik.io/created-by"
)
