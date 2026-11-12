package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// Nothing reconciles a TenantPreset: the CRD's own schema is what keeps an
// unusable one out, so it is what these check, against a real API server.
var _ = Describe("TenantPreset schema", func() {
	ctx := context.Background()
	valid := benchv1alpha1.TenantPresetSpec{Users: 100, Groups: 10, GroupDepth: 2, GroupsPerUser: 3}
	preset := func(name string, spec benchv1alpha1.TenantPresetSpec) *benchv1alpha1.TenantPreset {
		return &benchv1alpha1.TenantPreset{Name: name, Namespace: "default", Spec: spec}
	}

	It("accepts every built-in preset", func() {
		Expect(SeedDefaultTenantPresets(ctx, k8sClient, "default")).To(Succeed())
		DeferCleanup(func() {
			for _, p := range defaultTenantPresets("default") {
				forceDelete(ctx, &p)
			}
		})
		var small benchv1alpha1.TenantPreset
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "small", Namespace: "default"}, &small)).To(Succeed())
		Expect(small.Spec.Users).To(BeEquivalentTo(5000))
		Expect(small.Labels).To(HaveKeyWithValue(benchv1alpha1.LabelBuiltIn, "true"))
	})

	It("defaults the seed", func() {
		p := preset("no-seed", valid)
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		DeferCleanup(func() { forceDelete(ctx, p) })
		Expect(p.Spec.Seed).To(BeEquivalentTo(1))
	})

	DescribeTable("accepts the boundaries of each number",
		func(change func(*benchv1alpha1.TenantPresetSpec)) {
			spec := valid
			change(&spec)
			p := preset("accepted", spec)
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			forceDelete(ctx, p)
		},
		Entry("no groups, a flat tree and no memberships", func(s *benchv1alpha1.TenantPresetSpec) {
			s.Groups, s.GroupDepth, s.GroupsPerUser = 0, 1, 0
		}),
		Entry("a single user", func(s *benchv1alpha1.TenantPresetSpec) { s.Users = 1 }),
		Entry("as many users as the names have digits for", func(s *benchv1alpha1.TenantPresetSpec) { s.Users = 1000000 }),
		Entry("as many groups as the names have digits for", func(s *benchv1alpha1.TenantPresetSpec) { s.Groups = 1000000 }),
		Entry("the deepest tree", func(s *benchv1alpha1.TenantPresetSpec) { s.Groups, s.GroupDepth = 10, 10 }),
		Entry("as many tree levels as groups", func(s *benchv1alpha1.TenantPresetSpec) { s.Groups, s.GroupsPerUser, s.GroupDepth = 3, 3, 3 }),
		Entry("every user in every group", func(s *benchv1alpha1.TenantPresetSpec) { s.GroupsPerUser = s.Groups }),
		Entry("the largest seed", func(s *benchv1alpha1.TenantPresetSpec) { s.Seed = 2147483647 }),
	)

	DescribeTable("rejects a preset the seeder could not use",
		func(change func(*benchv1alpha1.TenantPresetSpec)) {
			spec := valid
			change(&spec)
			p := preset("rejected", spec)
			err := k8sClient.Create(ctx, p)
			if err == nil {
				forceDelete(ctx, p)
			}
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an Invalid error, got %v", err)
		},
		Entry("no users", func(s *benchv1alpha1.TenantPresetSpec) { s.Users = 0 }),
		Entry("more users than the names have digits for", func(s *benchv1alpha1.TenantPresetSpec) { s.Users = 1000001 }),
		Entry("a negative group count", func(s *benchv1alpha1.TenantPresetSpec) { s.Groups = -1 }),
		Entry("a group tree with no levels", func(s *benchv1alpha1.TenantPresetSpec) { s.GroupDepth = 0 }),
		Entry("more groups per user than groups", func(s *benchv1alpha1.TenantPresetSpec) { s.GroupsPerUser = 11 }),
		Entry("more tree levels than groups", func(s *benchv1alpha1.TenantPresetSpec) { s.Groups, s.GroupsPerUser, s.GroupDepth = 3, 3, 4 }),
		Entry("a tree without groups", func(s *benchv1alpha1.TenantPresetSpec) { s.Groups, s.GroupsPerUser = 0, 0 }),
		Entry("a tree deeper than ten levels", func(s *benchv1alpha1.TenantPresetSpec) { s.Groups, s.GroupDepth = 11, 11 }),
		Entry("a negative seed", func(s *benchv1alpha1.TenantPresetSpec) { s.Seed = -1 }),
	)

	// The Go type cannot hold a seed past int32, but JSON from the web UI
	// can. The int32 format alone rejects it with a message about formats;
	// the maximum adds one that says what the limit is.
	It("rejects a seed past int32", func() {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": benchv1alpha1.GroupVersion.String(),
			"kind":       "TenantPreset",
			"metadata":   map[string]any{"name": "big-seed", "namespace": "default"},
			"spec": map[string]any{
				"seed": int64(2147483648), "users": int64(100), "groups": int64(10),
				"groupDepth": int64(2), "groupsPerUser": int64(3),
			},
		}}
		err := k8sClient.Create(ctx, u)
		if err == nil {
			forceDelete(ctx, u)
		}
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an Invalid error, got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.seed in body should be less than or equal to 2147483647"))
	})
})
