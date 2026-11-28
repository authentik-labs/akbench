package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authentik-labs/akbench/internal/fixtures"
)

// fixturesUsersKey is the users.json key in the fixtures Secret, the file
// every k6 workload reads (render.UsersFilePath).
const fixturesUsersKey = "users.json"

// SeedFixtures generates the synthetic test users (internal/fixtures) into
// namespace when they aren't there yet: the fixtures Secret every k6
// workload mounts and the blueprint ConfigMap AuthentikDeploymentReconciler
// imports into each deployment - so a fresh install can run a test case
// without `make fixtures` first. Create-only, unlike
// SeedDefaultTestCaseTemplates: fixtures loaded any other way (e.g. `make
// fixtures` with more users) are never replaced. The two must describe the
// same users, so nothing is generated unless both are missing.
func SeedFixtures(ctx context.Context, c client.Client, namespace string, users int, seed uint64) error {
	log := logf.FromContext(ctx)
	secretKey := types.NamespacedName{Namespace: namespace, Name: fixturesSecretName}
	cmKey := types.NamespacedName{Namespace: namespace, Name: fixturesBlueprintConfigMapName}

	secretExists, err := exists(ctx, c, secretKey, &corev1.Secret{})
	if err != nil {
		return err
	}
	cmExists, err := exists(ctx, c, cmKey, &corev1.ConfigMap{})
	if err != nil {
		return err
	}
	switch {
	case secretExists && cmExists:
		return nil
	case secretExists || cmExists:
		log.Info("Not seeding test users: only one of the fixtures Secret and blueprint ConfigMap exists, and they must describe the same users",
			"secret", secretKey, "secretExists", secretExists, "configMap", cmKey, "configMapExists", cmExists)
		return nil
	}

	f, err := fixtures.Generate(users, seed)
	if err != nil {
		return fmt.Errorf("generate test users: %w", err)
	}
	// Blueprint first: a k6 run only starts once the Secret exists, and by
	// then the users it logs in as are already being imported.
	if err := c.Create(ctx, &corev1.ConfigMap{
		Name: cmKey.Name, Namespace: namespace,
		Data: map[string]string{fixturesBlueprintKey: string(f.Blueprint)},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s: %w", cmKey, err)
	}
	if err := c.Create(ctx, &corev1.Secret{
		Name: secretKey.Name, Namespace: namespace,
		Data: map[string][]byte{fixturesUsersKey: f.Credentials},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s: %w", secretKey, err)
	}
	log.Info("Seeded synthetic test users", "users", users, "namespace", namespace)
	return nil
}

func exists(ctx context.Context, c client.Client, key types.NamespacedName, obj client.Object) (bool, error) {
	err := c.Get(ctx, key, obj)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, errors.Join(fmt.Errorf("get %s", key), err)
	}
}
