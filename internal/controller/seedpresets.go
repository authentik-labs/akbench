package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// defaultTenantPresets are the operator's built-in tenant sizes. The numbers
// are assumptions about what small to very large authentik installs look
// like, not measurements of real ones; replace them when real figures are
// available. tiny matches the 50 users of the fixtures blueprint. The larger
// two need more Postgres storage than a deployment gets by default.
func defaultTenantPresets(namespace string) []benchv1alpha1.TenantPreset {
	preset := func(name, description string, users, groups, depth, groupsPerUser int32) benchv1alpha1.TenantPreset {
		return benchv1alpha1.TenantPreset{
			Name: name, Namespace: namespace,
			Spec: benchv1alpha1.TenantPresetSpec{
				Description:   description,
				Seed:          1,
				Users:         users,
				Groups:        groups,
				GroupDepth:    depth,
				GroupsPerUser: groupsPerUser,
			},
		}
	}
	return []benchv1alpha1.TenantPreset{
		preset("tiny", "50 users in 20 groups. As many users as the fixtures blueprint creates.", 50, 20, 2, 3),
		preset("small", "5,000 users in 100 groups, three levels deep.", 5000, 100, 3, 5),
		preset("medium", "20,000 users in 400 groups, three levels deep.", 20000, 400, 3, 6),
		preset("large", "100,000 users in 2,000 groups, four levels deep.", 100000, 2000, 4, 8),
		preset("huge", "500,000 users in 5,000 groups, five levels deep.", 500000, 5000, 5, 10),
	}
}

// +kubebuilder:rbac:groups=bench.goauthentik.io,resources=tenantpresets,verbs=get;list;watch;create;update;patch;delete

// SeedDefaultTenantPresets creates or updates the operator's built-in tenant
// presets, resetting their spec on every start, the same way
// SeedDefaultTestCaseTemplates treats the built-in templates: an operator
// upgrade always ships its own numbers, and local edits to a built-in preset
// are discarded (copy it under another name to keep changes). Built-in presets
// carry LabelBuiltIn, and that label is what makes a preset the operator's:
//   - a preset without it is never changed, even one that has the name of a
//     built-in added in a later release - someone created it, and resetting it
//     would silently change their numbers;
//   - a labelled preset whose name is no longer a built-in (dropped or renamed
//     in a later release) loses the label, so it becomes an ordinary preset
//     that can be edited or deleted. It is not deleted: something may still
//     refer to it by name.
func SeedDefaultTenantPresets(ctx context.Context, c client.Client, namespace string) error {
	log := logf.FromContext(ctx)
	builtIn := map[string]bool{}
	for _, want := range defaultTenantPresets(namespace) {
		builtIn[want.Name] = true
		var preset benchv1alpha1.TenantPreset
		err := c.Get(ctx, client.ObjectKeyFromObject(&want), &preset)
		switch {
		case apierrors.IsNotFound(err):
			want.Labels = map[string]string{benchv1alpha1.LabelBuiltIn: "true"}
			if err := c.Create(ctx, &want); err != nil {
				return fmt.Errorf("create TenantPreset %q: %w", want.Name, err)
			}
		case err != nil:
			return fmt.Errorf("get TenantPreset %q: %w", want.Name, err)
		case preset.Labels[benchv1alpha1.LabelBuiltIn] != "true":
			log.Info("Skipped built-in TenantPreset: a preset without the built-in label already has its name",
				"namespace", namespace, "name", want.Name)
		default:
			if preset.Spec == want.Spec {
				continue
			}
			preset.Spec = want.Spec
			if err := c.Update(ctx, &preset); err != nil {
				return fmt.Errorf("update TenantPreset %q: %w", want.Name, err)
			}
		}
	}

	var labelled benchv1alpha1.TenantPresetList
	if err := c.List(ctx, &labelled, client.InNamespace(namespace),
		client.MatchingLabels{benchv1alpha1.LabelBuiltIn: "true"}); err != nil {
		return fmt.Errorf("list built-in TenantPresets: %w", err)
	}
	for i := range labelled.Items {
		preset := &labelled.Items[i]
		if builtIn[preset.Name] {
			continue
		}
		base := preset.DeepCopy()
		delete(preset.Labels, benchv1alpha1.LabelBuiltIn)
		if err := c.Patch(ctx, preset, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("unlabel TenantPreset %q: %w", preset.Name, err)
		}
		log.Info("Removed built-in label from TenantPreset that is no longer built-in",
			"namespace", namespace, "name", preset.Name)
	}
	return nil
}
