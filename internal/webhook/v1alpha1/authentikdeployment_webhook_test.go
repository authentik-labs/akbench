package v1alpha1

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// fakeTags answers TagExists from a fixed set of "image:tag" strings, or
// fails every lookup with err.
type fakeTags struct {
	existing map[string]bool
	err      error
	lookups  int
}

func (f *fakeTags) TagExists(_ context.Context, image, tag string) (bool, error) {
	f.lookups++
	if f.err != nil {
		return false, f.err
	}
	return f.existing[image+":"+tag], nil
}

var _ = Describe("AuthentikDeployment Webhook", func() {
	var (
		obj       *benchv1alpha1.AuthentikDeployment
		tags      *fakeTags
		validator AuthentikDeploymentValidator
	)

	BeforeEach(func() {
		obj = &benchv1alpha1.AuthentikDeployment{
			Name: "ak", Namespace: "default",
			Spec: benchv1alpha1.AuthentikDeploymentSpec{
				Authentik: benchv1alpha1.AuthentikSpec{Image: "ghcr.io/goauthentik/server", Tag: "2026.8.1"},
			},
		}
		tags = &fakeTags{existing: map[string]bool{
			"ghcr.io/goauthentik/server:2026.8.1":   true,
			"ghcr.io/goauthentik/dev-server:gh-abc": true,
		}}
		validator = AuthentikDeploymentValidator{Images: tags}
	})

	Context("name", func() {
		It("rejects dots and names past Helm's release name limit on create only", func() {
			obj.Name = "compat-2026.8.5"
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(MatchError(ContainSubstring("metadata.name")))
			obj.Name = "2026-10-29-main-2026-8-5-oauth-token-exchange-and-then-some"
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(MatchError(ContainSubstring("53")))
			Expect(validator.ValidateUpdate(ctx, obj, obj)).Error().NotTo(HaveOccurred())
		})
	})

	Context("image tag", func() {
		It("admits a tag that exists", func() {
			Expect(validator.ValidateCreate(ctx, obj)).To(BeEmpty())
		})

		It("rejects a tag that doesn't exist", func() {
			obj.Spec.Authentik.Tag = "2026.8.99"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(MatchError(ContainSubstring("spec.authentik.tag")))
			Expect(err).To(MatchError(ContainSubstring("ghcr.io/goauthentik/server:2026.8.99")))
		})

		It("rejects a malformed tag without asking the registry", func() {
			obj.Spec.Authentik.Tag = "../latest"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(MatchError(ContainSubstring("must be a valid image tag")))
			Expect(tags.lookups).To(BeZero())
		})

		It("rejects an image that carries its own tag", func() {
			obj.Spec.Authentik.Image = "ghcr.io/goauthentik/server:2026.8.1"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(MatchError(ContainSubstring("spec.authentik.image")))
		})

		It("leaves an unset tag (the chart's default) alone", func() {
			obj.Spec.Authentik.Tag = ""
			Expect(validator.ValidateCreate(ctx, obj)).To(BeEmpty())
			Expect(tags.lookups).To(BeZero())
		})

		It("only warns when the registry can't be reached", func() {
			tags.err = errors.New("connection refused")
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("could not verify")))
		})

		It("checks the default image when none is set", func() {
			obj.Spec.Authentik.Image = ""
			Expect(validator.ValidateCreate(ctx, obj)).To(BeEmpty())
			Expect(tags.lookups).To(Equal(1))
		})

		It("checks a changed tag on update", func() {
			oldObj := obj.DeepCopy()
			obj.Spec.Authentik.Image = "ghcr.io/goauthentik/dev-server"
			obj.Spec.Authentik.Tag = "gh-missing"
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
		})

		It("doesn't re-check an unchanged tag on update", func() {
			obj.Spec.Authentik.Tag = "since-deleted"
			oldObj := obj.DeepCopy()
			obj.Spec.Server.Replicas = 3
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).To(BeEmpty())
			Expect(tags.lookups).To(BeZero())
		})
	})
})
