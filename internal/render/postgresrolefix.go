package render

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// embeddedPostgresFullname is the bundled postgresql subchart's
// fullnameOverride, which its Service, Secret and StatefulSet are all named
// after: "<release>-postgresql", the subchart's own default, but pinned (it
// drops "-postgresql" for a release name containing "postgresql") and
// shortened so the StatefulSet's pods' controller-revision-hash label,
// "<fullname>-<10-char hash>", fits in 63.
func embeddedPostgresFullname(t *benchv1alpha1.AuthentikDeployment) string {
	return BoundedName(t.Name+"-postgresql", maxNameLength-11)
}

func embeddedPostgresServiceName(t *benchv1alpha1.AuthentikDeployment) string {
	return embeddedPostgresFullname(t)
}

func embeddedPostgresSecretName(t *benchv1alpha1.AuthentikDeployment) string {
	return embeddedPostgresFullname(t)
}

// EmbeddedPostgresStatefulSetName is the bundled subchart's StatefulSet -
// same fullname convention as the Service/Secret above (confirmed live,
// `kubectl get statefulset`), exported so AuthentikDeploymentReconciler can
// check the postgres StatefulSet's own readiness directly rather than
// waiting on the whole HelmRelease (server+worker+postgres together) to
// settle - see reconcilePostgresRoleFix's own doc comment for why those two
// are not the same thing.
func EmbeddedPostgresStatefulSetName(t *benchv1alpha1.AuthentikDeployment) string {
	return embeddedPostgresFullname(t)
}

// postgresRoleFixImage runs a one-shot psql command - pinned to match the
// authentik chart's own current default postgresql.image
// (docker.io/library/postgres:17.11-bookworm, confirmed against the chart's
// real values.yaml and a live pod's actual image) purely so this psql
// client's protocol version matches the server it's talking to; bump this
// if that default ever changes.
var postgresRoleFixImage = pinnedImage("postgresRoleFix")

// PostgresRoleFixJobName is deterministic and NOT content-hashed (unlike
// BlueprintJobName): this fixup has no changing "content" to version across
// reconciles the way a blueprint's script body does - it either still needs
// to run or it doesn't, tracked the same create-once way any other
// idempotent addon in this codebase is.
func PostgresRoleFixJobName(t *benchv1alpha1.AuthentikDeployment) string {
	return BoundedName(t.Name+"-postgres-role-fix", maxNameLength)
}

// PostgresRoleFixJob works around a real gap in the authentik chart's own
// bundled postgresql subchart: the chart overrides the Bitnami postgresql
// subchart's own default image with the plain, official
// docker.io/library/postgres image (confirmed live -
// `kubectl get pod <release>-postgresql-0 -o jsonpath='{.spec.containers[?(@.name=="postgresql")].image}'`
// on a real install), whose entrypoint script only understands the vanilla
// POSTGRES_USER/POSTGRES_PASSWORD env vars - it has no concept of Bitnami's
// own POSTGRES_POSTGRES_PASSWORD convention for a *separate* "postgres"
// superuser role, even though the subchart's own values
// (auth.enablePostgresUser, on by default) still generate a
// "postgres-password" Secret key and wire POSTGRES_POSTGRES_PASSWORD_FILE
// into the container as if it would be honored. The practical effect,
// confirmed live on a genuinely fresh install (not just an already-existing
// cluster predating some config change): only ONE role ever gets created at
// initdb time, named after postgresql.auth.username ("authentik", not
// "postgres") - so the bundled postgres_exporter sidecar (metrics.enabled,
// HelmValues), hardcoded to connect as "postgres" using that exact Secret
// key, can never authenticate ("password authentication failed for user
// \"postgres\""), and every postgres.json Grafana panel stays empty
// forever without this fix. This is a permanent gap in the chart's own
// image choice, not something fixable via Helm values alone.
//
// This Job creates (or, if it already exists with a stale password, resets)
// that missing "postgres" role - connecting as the app user (authentik,
// which the chart makes a real superuser) using its own known password, and
// setting the target role's password to whatever the "postgres-password"
// Secret key already holds (the exact value the exporter already expects),
// so nothing needs to change on the exporter's side at all. The new
// password is passed to psql via -v (its own variable-substitution
// mechanism, :'name') rather than interpolated into the SQL string
// directly, so it's never at risk of breaking the statement or being
// misquoted regardless of its exact character content. Idempotent (safe to
// leave applied indefinitely, unlike BlueprintJob's content-hashed
// one-shot-per-version approach - see PostgresRoleFixJobName's own doc
// comment) and self-waiting (`until pg_isready`) rather than relying
// purely on BackoffLimit retries, since this typically races the
// postgresql pod's own first startup.
func PostgresRoleFixJob(t *benchv1alpha1.AuthentikDeployment, wc *benchv1alpha1.WorkloadCluster) *batchv1.Job {
	host := embeddedPostgresServiceName(t)
	psql := fmt.Sprintf(`psql -h %s -U authentik -d authentik`, host)
	// The SQL is piped into psql's stdin (printf ... | psql ...), not passed
	// via -c - confirmed live, empirically, after two wrong guesses: psql's
	// own -v/:'name' variable substitution is a client-side rewrite that (1)
	// does not apply inside a dollar-quoted (DO $$ ... $$) string at all,
	// and (2), less obviously, silently does not apply to a plain top-level
	// statement passed via -c either ("SELECT :'newpass';" via -c fails
	// with a syntax error at ":", while the exact same statement piped
	// through stdin substitutes correctly) - only script/stdin-style input
	// gets full substitution. Two plain statements (not one DO block)
	// either way, so a NOT EXISTS check happens in the shell instead of SQL.
	script := fmt.Sprintf(`set -e
until pg_isready -h %s -p 5432 -U authentik >/dev/null 2>&1; do sleep 2; done
exists=$(printf '%%s' "SELECT 1 FROM pg_roles WHERE rolname='postgres'" | %s -tA)
if [ "$exists" = "1" ]; then
  printf '%%s' "ALTER ROLE postgres WITH PASSWORD :'newpass'" | %s -v ON_ERROR_STOP=1 -v newpass="$POSTGRES_PASSWORD"
else
  printf '%%s' "CREATE ROLE postgres WITH SUPERUSER LOGIN PASSWORD :'newpass'" | %s -v ON_ERROR_STOP=1 -v newpass="$POSTGRES_PASSWORD"
fi
`, host, psql, psql, psql)

	nodeSelector, tolerations := AuthentikPoolPlacement(wc)
	return &batchv1.Job{
		Kind: "Job", APIVersion: "batch/v1",
		Name: PostgresRoleFixJobName(t), Namespace: t.Spec.Namespace,
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(10)),
			TTLSecondsAfterFinished: new(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: DeploymentLabels(t.Name)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					NodeSelector:  nodeSelector,
					Tolerations:   tolerations,
					Containers: []corev1.Container{{
						Name:    "fix-postgres-role",
						Image:   postgresRoleFixImage,
						Command: []string{"sh", "-c", script},
						Env: []corev1.EnvVar{
							// PGPASSWORD (psql's own recognized env var) -
							// authenticates as "authentik", the app user
							// whose password we already know.
							{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									Name: embeddedPostgresSecretName(t),
									Key:  "password",
								},
							}},
							// POSTGRES_PASSWORD - the value the exporter
							// already expects for the "postgres" role,
							// substituted into the SQL via psql's -v/:'...'
							// mechanism rather than shell/string interpolation.
							{Name: "POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									Name: embeddedPostgresSecretName(t),
									Key:  "postgres-password",
								},
							}},
						},
					}},
				},
			},
		},
	}
}
