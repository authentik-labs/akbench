package v1alpha1

import (
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Name limits, checked on create only (an object created before these
// checks must still be updatable and deletable). Not CRD CEL rules: a
// create-only rule needs optionalOldSelf, which helmify strips from the
// published chart's CRDs, leaving rules the API server won't compile.
const (
	// maxDeploymentNameLength is Helm's release name limit: an
	// AuthentikDeployment's name is its release name.
	maxDeploymentNameLength = 53
	// maxTestCaseNameLength is the label value limit: a TestCase's name
	// labels its k6 pods.
	maxTestCaseNameLength = validation.DNS1123LabelMaxLength
)

// nameErrors requires name to be a DNS label (no dots) of at most maxLen
// characters.
func nameErrors(name string, maxLen int) field.ErrorList {
	path := field.NewPath("metadata", "name")
	var errs field.ErrorList
	for _, msg := range validation.IsDNS1123Label(name) {
		errs = append(errs, field.Invalid(path, name, msg))
	}
	if len(name) > maxLen {
		errs = append(errs, field.TooLong(path, name, maxLen))
	}
	return errs
}
