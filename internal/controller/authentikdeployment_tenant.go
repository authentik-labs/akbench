package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
	"github.com/authentik-labs/akbench/internal/render"
	"github.com/authentik-labs/akbench/seeder"
)

// tenantMessageLimit caps the failure message kept in status: the
// termination message it comes from can be a log tail of up to 4 KiB.
const tenantMessageLimit = 1024

// tenantJobPoll is how often a running seeding Job is checked on: it runs
// on the workload cluster, where nothing watches it.
const tenantJobPoll = 10 * time.Second

// tenantPresetRecheck is how often a deployment looks again for a missing
// TenantPreset. The preset watch normally notices one being created; this
// covers an install whose TenantPreset CRD arrived after the operator
// started, which runs without that watch.
const tenantPresetRecheck = 5 * time.Minute

// tenantOutcome is what reconcileTenant reports for the TenantSeeded
// condition, and when to check back on a timer (0: only on a change).
type tenantOutcome struct {
	status       metav1.ConditionStatus
	reason       string
	message      string
	requeueAfter time.Duration
}

// blocksReady reports whether the tenant holds the deployment's Ready
// condition back. A tenant that needs a reset doesn't: the data in the
// database is intact, it just isn't what spec.tenant asks for any more, and
// the test cases that need a tenant refuse to run on it instead.
func (o tenantOutcome) blocksReady() bool {
	return o.status != metav1.ConditionTrue && o.reason != benchv1alpha1.ReasonTenantResetRequired
}

func tenantReconciling(message string, requeueAfter time.Duration) tenantOutcome {
	return tenantOutcome{status: metav1.ConditionFalse, reason: benchv1alpha1.ReasonReconciling, message: message, requeueAfter: requeueAfter}
}

// reconcileTenant seeds spec.tenant into the deployment once its HelmRelease
// is Ready, and returns the TenantSeeded condition. What was seeded is
// recorded in status.tenant when seeding starts and never changed in place:
// a seeded tenant's Job is never looked at again (it is TTL-deleted, and
// re-creating it would only flip the deployment back to not Ready), and a
// later change of the preset, of spec.tenant or of the password is reported
// as needing a reset rather than re-seeded. A seeding run already started is
// followed to its end even if spec.tenant is removed meanwhile, since it
// still writes to the database.
func (r *AuthentikDeploymentReconciler) reconcileTenant(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment,
	wc *benchv1alpha1.WorkloadCluster, helmReleaseReady metav1.ConditionStatus,
) (tenantOutcome, error) {
	var preset *benchv1alpha1.TenantPreset
	if topo.Spec.Tenant != nil {
		var err error
		if preset, err = r.tenantPreset(ctx, topo); err != nil {
			return tenantOutcome{}, err
		}
	}
	dropStaleTenant(topo, preset)
	spec, seeded := topo.Spec.Tenant, topo.Status.Tenant
	switch {
	case seeded != nil && seeded.State == benchv1alpha1.TenantSeeded:
		return r.seededTenant(ctx, topo, preset)
	case seeded != nil && seeded.State == benchv1alpha1.TenantFailed:
		return tenantOutcome{status: metav1.ConditionFalse, reason: benchv1alpha1.ReasonSeedingFailed, message: seeded.Message}, nil
	case seeded == nil && spec == nil:
		return tenantOutcome{status: metav1.ConditionTrue, reason: benchv1alpha1.ReasonReady, message: "no tenant configured"}, nil
	case helmReleaseReady != metav1.ConditionTrue:
		// Only starting and following a run waits for the chart: a seeded
		// or failed tenant is reported as such through a chart upgrade.
		return tenantReconciling("waiting for the HelmRelease before seeding the tenant", 0), nil
	case seeded == nil && preset == nil:
		return tenantReconciling(fmt.Sprintf("TenantPreset %q not found", spec.PresetRef.Name), tenantPresetRecheck), nil
	}
	return r.seedTenant(ctx, rc, topo, wc, preset)
}

// dropStaleTenant forgets a status.tenant that left nothing in the database
// and no longer matches what the deployment asks for, so seeding starts
// afresh (or not at all, without spec.tenant):
//   - a failed attempt (the seeder writes everything in one transaction, so
//     a failure leaves nothing behind) once spec.tenant is removed or names
//     another preset, or its preset or the image would run a Job by another
//     name. The same Job would only fail the same way again; a reset retries
//     it.
//   - an attempt whose Job hasn't been created yet, once spec.tenant is
//     removed or no longer asks for what it recorded.
func dropStaleTenant(topo *benchv1alpha1.AuthentikDeployment, preset *benchv1alpha1.TenantPreset) {
	spec, seeded := topo.Spec.Tenant, topo.Status.Tenant
	switch {
	case seeded == nil:
		return
	case seeded.State == benchv1alpha1.TenantFailed:
		if spec == nil || spec.PresetRef.Name != seeded.Preset ||
			(preset != nil && render.TenantSeedJobName(topo, render.TenantParamsFromPreset(preset.Spec), seeder.Script) != seeded.JobName) {
			topo.Status.Tenant = nil
		}
	case seeded.State == benchv1alpha1.TenantSeeding && seeded.JobName == "":
		if spec == nil || !sameTenant(spec, preset, seeded) {
			topo.Status.Tenant = nil
		}
	}
}

// tenantPreset returns the TenantPreset spec.tenant names, or nil if there
// is none (yet): a preset may be created after the deployment naming it.
func (r *AuthentikDeploymentReconciler) tenantPreset(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment) (*benchv1alpha1.TenantPreset, error) {
	var preset benchv1alpha1.TenantPreset
	err := r.Get(ctx, types.NamespacedName{Name: topo.Spec.Tenant.PresetRef.Name, Namespace: topo.Namespace}, &preset)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("get TenantPreset: %w", err)
	}
	return &preset, nil
}

// sameTenant reports whether seeded is what spec and its preset ask for. A
// preset deleted since doesn't count as a change: status is the record of
// what was seeded, not the preset.
func sameTenant(spec *benchv1alpha1.TenantSpec, preset *benchv1alpha1.TenantPreset, seeded *benchv1alpha1.TenantStatus) bool {
	return spec.PresetRef.Name == seeded.Preset &&
		(preset == nil || render.TenantParamsFromPreset(preset.Spec) == seeded.Params)
}

// seededTenant reports a seeded tenant: True, or TenantResetRequired with
// what has changed since it was seeded.
func (r *AuthentikDeploymentReconciler) seededTenant(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment, preset *benchv1alpha1.TenantPreset) (tenantOutcome, error) {
	seeded := topo.Status.Tenant
	if topo.Spec.Tenant == nil {
		// The data is still in the database; only a reset removes it.
		return tenantOutcome{status: metav1.ConditionTrue, reason: benchv1alpha1.ReasonReady,
			message: fmt.Sprintf("holds the tenant seeded from preset %q; none is selected now", seeded.Preset)}, nil
	}
	var changes []string
	switch {
	case topo.Spec.Tenant.PresetRef.Name != seeded.Preset:
		changes = append(changes, fmt.Sprintf("spec.tenant names preset %q, but the tenant was seeded from %q", topo.Spec.Tenant.PresetRef.Name, seeded.Preset))
	case preset != nil && render.TenantParamsFromPreset(preset.Spec) != seeded.Params:
		changes = append(changes, fmt.Sprintf("TenantPreset %q has changed since the tenant was seeded", seeded.Preset))
	}
	fingerprint, err := tenantPasswordFingerprint(ctx, r.Client, topo.Namespace)
	if err != nil {
		return tenantOutcome{}, err
	}
	if fingerprint != seeded.PasswordFingerprint {
		changes = append(changes, fmt.Sprintf("Secret %s/%s no longer holds the password the users were given", topo.Namespace, render.TenantPasswordSecretName))
	}
	if len(changes) > 0 {
		return tenantOutcome{status: metav1.ConditionFalse, reason: benchv1alpha1.ReasonTenantResetRequired,
			message: strings.Join(changes, "; ") + " - reset the deployment to seed it again"}, nil
	}
	return tenantOutcome{status: metav1.ConditionTrue, reason: benchv1alpha1.ReasonReady,
		message: fmt.Sprintf("seeded %d users, %d groups from preset %q", seeded.Params.Users, seeded.Params.Groups, seeded.Preset)}, nil
}

// tenantPasswordFingerprint fingerprints the current tenant password in
// namespace on the management cluster, or returns "" if its Secret is gone.
func tenantPasswordFingerprint(ctx context.Context, c client.Client, namespace string) (string, error) {
	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: namespace}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("get tenant password secret: %w", err)
	}
	return passwordFingerprint(string(secret.Data[render.TenantPasswordKey])), nil
}

// passwordFingerprint is enough of a password's SHA-256 to tell two apart,
// without putting the password itself in status.
func passwordFingerprint(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])[:12]
}

// seedTenant starts seeding (recording what it seeds first) or follows the
// seeding Job it started. preset is only needed to start.
func (r *AuthentikDeploymentReconciler) seedTenant(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment,
	wc *benchv1alpha1.WorkloadCluster, preset *benchv1alpha1.TenantPreset,
) (tenantOutcome, error) {
	log := logf.FromContext(ctx)
	password, err := ensureRandomSecret(ctx, r.Client,
		types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: topo.Namespace}, render.TenantPasswordKey, 32)
	if err != nil {
		return tenantOutcome{}, fmt.Errorf("ensure tenant password: %w", err)
	}
	if topo.Status.Tenant == nil {
		// Saved before the Job exists, so the numbers it runs with are on
		// record even if this reconcile goes no further.
		topo.Status.Tenant = &benchv1alpha1.TenantStatus{
			Preset:              preset.Name,
			Params:              render.TenantParamsFromPreset(preset.Spec),
			PasswordFingerprint: passwordFingerprint(password),
			State:               benchv1alpha1.TenantSeeding,
		}
		if err := r.Status().Update(ctx, topo); err != nil {
			return tenantOutcome{}, err
		}
	}
	seeding := topo.Status.Tenant

	// The Job this attempt created, or else the one it is about to.
	name := seeding.JobName
	if name == "" {
		name = render.TenantSeedJobName(topo, seeding.Params, seeder.Script)
	}
	running := tenantReconciling(fmt.Sprintf("seeding tenant from preset %q (%d users, %d groups)",
		seeding.Preset, seeding.Params.Users, seeding.Params.Groups), tenantJobPoll)
	var job batchv1.Job
	err = rc.Get(ctx, types.NamespacedName{Name: name, Namespace: topo.Spec.Namespace}, &job)
	switch {
	case apierrors.IsNotFound(err):
		// Not created yet, or deleted (by its TTL, say) before this saw it
		// finish. Run again, the seeder finds a tenant it already seeded
		// and stops there.
		if err := r.createSeedJob(ctx, rc, topo, wc, name, password); err != nil {
			return tenantOutcome{}, err
		}
		seeding.JobName = name
		log.Info("Created tenant seeding Job", "name", name, "preset", seeding.Preset)
		return running, nil
	case err != nil:
		return tenantOutcome{}, fmt.Errorf("get tenant seeding job: %w", err)
	case seeding.JobName == "" && jobFinished(&job, batchv1.JobFailed):
		// An earlier, identical attempt's Job, still there until its TTL
		// runs out: this attempt starts over deliberately (spec.tenant was
		// removed and set again), so it mustn't read that one's failure as
		// its own. Once it's gone, the next pass creates a new one.
		if err := rc.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return tenantOutcome{}, fmt.Errorf("delete earlier tenant seeding job: %w", err)
		}
		log.Info("Deleted failed tenant seeding Job from an earlier attempt", "name", name)
		return tenantReconciling(fmt.Sprintf("removing the failed seeding Job %s of an earlier attempt before seeding again", name), tenantJobPoll), nil
	}
	// A Job by this name that is running or done is this attempt's own,
	// even if recording its name didn't make it into status.
	seeding.JobName = name

	switch {
	case jobFinished(&job, batchv1.JobComplete):
		now := metav1.Now()
		seeding.State, seeding.SeededAt, seeding.Message = benchv1alpha1.TenantSeeded, &now, ""
		log.Info("Seeded tenant", "job", name, "preset", seeding.Preset)
		// Reported like any seeded tenant, so a spec.tenant changed
		// while the Job ran shows up straight away.
		return r.seededTenant(ctx, topo, preset)
	case jobFinished(&job, batchv1.JobFailed):
		seeding.State = benchv1alpha1.TenantFailed
		seeding.Message = seederFailureMessage(r.seedJobTermination(ctx, topo, name), jobCondition(&job, batchv1.JobFailed))
		log.Info("Failed to seed tenant", "job", name, "preset", seeding.Preset, "message", seeding.Message)
		return tenantOutcome{status: metav1.ConditionFalse, reason: benchv1alpha1.ReasonSeedingFailed, message: seeding.Message}, nil
	}
	return running, nil
}

// createSeedJob mirrors the password and the seeder script next to the
// seeding Job, then creates it.
func (r *AuthentikDeploymentReconciler) createSeedJob(ctx context.Context, rc client.Client, topo *benchv1alpha1.AuthentikDeployment,
	wc *benchv1alpha1.WorkloadCluster, name, password string,
) error {
	if err := ensureSecretData(ctx, rc, types.NamespacedName{Name: render.TenantPasswordSecretName, Namespace: topo.Spec.Namespace},
		map[string][]byte{render.TenantPasswordKey: []byte(password)}); err != nil {
		return fmt.Errorf("mirror tenant password secret: %w", err)
	}
	if err := ensureConfigMapData(ctx, rc, render.TenantSeederConfigMap(topo)); err != nil {
		return fmt.Errorf("apply tenant seeder configmap: %w", err)
	}
	if err := rc.Create(ctx, render.TenantSeedJob(topo, wc, name, topo.Status.Tenant.Params)); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create tenant seeding job: %w", err)
	}
	return nil
}

// jobCondition returns job's condition of type t if it is True, or a zero
// condition.
func jobCondition(job *batchv1.Job, t batchv1.JobConditionType) batchv1.JobCondition {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return batchv1.JobCondition{}
}

// jobFinished reports whether job has a True condition of type t
// (JobComplete or JobFailed).
func jobFinished(job *batchv1.Job, t batchv1.JobConditionType) bool {
	return jobCondition(job, t).Type == t
}

// seedJobTermination is how the seeding Job's latest pod that ran to an end
// ended: its exit code and reason, and its termination message, which is
// the seeder's log tail since the container uses FallbackToLogsOnError.
// Read through an uncached reader, so this doesn't start a Pod informer
// for a whole cluster - RBAC only grants get and list on pods. Best effort:
// nil on any failure, and the Job's own condition is reported instead.
func (r *AuthentikDeploymentReconciler) seedJobTermination(ctx context.Context, topo *benchv1alpha1.AuthentikDeployment, jobName string) *corev1.ContainerStateTerminated {
	reader := r.APIReader
	if topo.Spec.ClusterRef.Name != "" {
		if r.Tracker == nil {
			return nil
		}
		uncached, err := r.Tracker.GetUncachedClient(ctx, client.ObjectKey{Namespace: topo.Namespace, Name: topo.Spec.ClusterRef.Name})
		if err != nil {
			logf.FromContext(ctx).Error(err, "Failed to get workload cluster client for seeding Job pods", "job", jobName)
			return nil
		}
		reader = uncached
	}
	if reader == nil {
		return nil
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(topo.Spec.Namespace), client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list seeding Job pods", "job", jobName)
		return nil
	}
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int {
		return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
	})
	for _, pod := range pods.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Terminated != nil {
				return status.State.Terminated
			}
		}
	}
	return nil
}

// seederFailureMessage says why a seeding Job failed, from how its last pod
// ended (term, nil if unknown) and the Job's Failed condition, preferring
// whatever says most:
//   - the seeder's own "seeding failed: ..." line, which says why;
//   - the Job's deadline running out, which killed the seeder mid-run;
//   - how the container ended ("container OOMKilled (exit 137)"), with the
//     last line it logged: a crash before the seeder could catch it, or a
//     kill it never saw coming;
//   - the Job's condition, when no pod is left to ask.
func seederFailureMessage(term *corev1.ContainerStateTerminated, job batchv1.JobCondition) string {
	var lines []string
	if term != nil {
		lines = strings.Split(term.Message, "\n")
	}
	last := ""
	for _, line := range slices.Backward(lines) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "seeding failed:") {
			return truncateMessage(line)
		}
		if last == "" {
			last = line
		}
	}
	switch {
	case job.Reason == batchv1.JobReasonDeadlineExceeded && job.Message != "":
		return truncateMessage(job.Message)
	case term != nil:
		message := containerExit(term)
		if last != "" {
			message += ": " + last
		}
		return truncateMessage(message)
	case job.Message != "":
		return truncateMessage(job.Message)
	}
	return "the tenant seeding Job failed"
}

// containerExit is how a container ended, e.g. "container OOMKilled (exit
// 137)". "Error", the reason for any non-zero exit, says nothing the exit
// code doesn't.
func containerExit(term *corev1.ContainerStateTerminated) string {
	if term.Reason == "" || term.Reason == "Error" {
		return fmt.Sprintf("container exited with code %d", term.ExitCode)
	}
	return fmt.Sprintf("container %s (exit %d)", term.Reason, term.ExitCode)
}

func truncateMessage(s string) string {
	if len(s) <= tenantMessageLimit {
		return s
	}
	cut := tenantMessageLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// ensureConfigMapData creates want on c, or updates the data of the existing
// ConfigMap if it differs.
func ensureConfigMapData(ctx context.Context, c client.Client, want *corev1.ConfigMap) error {
	var existing corev1.ConfigMap
	err := c.Get(ctx, client.ObjectKeyFromObject(want), &existing)
	switch {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, want)
	case err != nil:
		return err
	case maps.Equal(existing.Data, want.Data):
		return nil
	}
	existing.Data = want.Data
	return c.Update(ctx, &existing)
}

// ensureSecretData creates the Secret key on c holding data, or updates the
// data of the existing one if it differs.
func ensureSecretData(ctx context.Context, c client.Client, key types.NamespacedName, data map[string][]byte) error {
	var existing corev1.Secret
	err := c.Get(ctx, key, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, &corev1.Secret{Name: key.Name, Namespace: key.Namespace, Data: data})
	case err != nil:
		return err
	case maps.EqualFunc(existing.Data, data, bytes.Equal):
		return nil
	}
	existing.Data = data
	return c.Update(ctx, &existing)
}

// deploymentsForTenantPreset maps a TenantPreset change to the deployments in
// its namespace whose spec.tenant names it, so a preset created after the
// deployment (or edited after seeding) is picked up straight away.
func (r *AuthentikDeploymentReconciler) deploymentsForTenantPreset(ctx context.Context, preset client.Object) []reconcile.Request {
	var list benchv1alpha1.AuthentikDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(preset.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, topo := range list.Items {
		if topo.Spec.Tenant != nil && topo.Spec.Tenant.PresetRef.Name == preset.GetName() {
			reqs = append(reqs, reconcile.Request{Name: topo.Name, Namespace: topo.Namespace})
		}
	}
	return reqs
}

// deploymentsForTenantPassword maps a change to a namespace's tenant
// password Secret to the deployments there that hold a tenant, so a
// replaced or deleted password is reported as needing a reset straight
// away, not on the next unrelated reconcile.
func (r *AuthentikDeploymentReconciler) deploymentsForTenantPassword(ctx context.Context, secret client.Object) []reconcile.Request {
	var list benchv1alpha1.AuthentikDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(secret.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, topo := range list.Items {
		if topo.Status.Tenant != nil {
			reqs = append(reqs, reconcile.Request{Name: topo.Name, Namespace: topo.Namespace})
		}
	}
	return reqs
}

// tenantPresetsInstalled reports whether the TenantPreset kind is served,
// so the deployment controller can watch presets. The chart pulls :latest,
// so an install whose CRDs predate TenantPreset can restart into this
// binary, and a watch on a missing kind would stop the whole manager; such
// an install's AuthentikDeployment CRD predates spec.tenant too, so nothing
// there names a preset. Any other failure is returned: starting without the
// watch over a passing API server hiccup would leave presets unwatched
// until the next restart.
func tenantPresetsInstalled(mapper meta.RESTMapper) (bool, error) {
	kind := benchv1alpha1.GroupVersion.WithKind("TenantPreset")
	_, err := mapper.RESTMapping(kind.GroupKind(), kind.Version)
	switch {
	case meta.IsNoMatchError(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("look up the TenantPreset kind: %w", err)
	}
	return true, nil
}
