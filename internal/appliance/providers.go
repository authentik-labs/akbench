package appliance

import "strings"

// The contract between the appliance and the web UI (internal/api's
// infrastructure endpoints) for choosing infrastructure providers: the UI
// writes the desired set into ProvidersConfigMap, the appliance's provider
// loop installs it with clusterctl and reports back in StatusConfigMap.
// Both live in SystemNamespace. Outside the appliance neither exists and the
// UI only reports what is installed (clusterctl's own inventory).
const (
	SystemNamespace = "authentik-bench"

	ProvidersConfigMap = "appliance-providers"
	// ProvidersKeyInfrastructure is a comma-separated list of clusterctl
	// infrastructure provider names, e.g. "incus".
	ProvidersKeyInfrastructure = "infrastructure"

	StatusConfigMap = "appliance-status"
	// StatusKeyShipped is a JSON object mapping each infrastructure provider
	// this image can install to its version.
	StatusKeyShipped = "shipped"
	// StatusKeyError is the last provider sync's error, empty on success.
	StatusKeyError = "error"
	// StatusKeySyncedAt is when the last provider sync finished (RFC 3339).
	StatusKeySyncedAt = "syncedAt"
)

// ParseProviderList splits a ProvidersKeyInfrastructure value.
func ParseProviderList(v string) []string {
	var out []string
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// The contract for updating akbench itself from the web UI: the UI writes
// the operator/apiserver version to run into VersionConfigMap, and the
// appliance's sync loop re-applies the stack at that version. The pin only
// holds while the appliance image's own version is still VersionKeyBase, so
// replacing the container with a newer image goes back to the version that
// image ships.
const (
	VersionConfigMap = "appliance-version"
	VersionKeyTarget = "version"
	VersionKeyBase   = "base"
)

// EffectiveVersion is the akbench version to run: the UI's pin while it was
// made against this image's version, otherwise the image's own.
func EffectiveVersion(imageVersion string, pin map[string]string) string {
	if v := pin[VersionKeyTarget]; v != "" && pin[VersionKeyBase] == imageVersion {
		return v
	}
	return imageVersion
}
