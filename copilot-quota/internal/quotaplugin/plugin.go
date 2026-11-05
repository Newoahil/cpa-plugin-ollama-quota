// Package quotaplugin holds the plugin behaviour behind the C ABI shim in
// cmd/plugin. Keeping it in a normal package lets the RPC surface, lifecycle
// and quota provider be tested with cgo disabled, which the shared-library
// build mode otherwise prevents.
package quotaplugin

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// Name is the stable plugin id CPA uses as the config key and the
	// discovery file stem (copilot-quota-v<version>.so).
	Name = "copilot-quota"
	// QuotaIdentifier is the plugin's own identifier. It is deliberately not
	// "copilot": the host's first dispatch pass matches a plugin whose
	// AuthProvider identifier equals the credential's provider, and the
	// syepes auth plugin owns that key. Claiming "copilot" here would make
	// dispatch depend on load order. The second pass, driven by
	// DescribeQuota.SupportedProviders, is the supported route (see Describe).
	QuotaIdentifier = "copilot-quota"
	// CredentialProvider is the provider key whose credentials this plugin
	// serves, as declared through SupportedProviders.
	CredentialProvider = "copilot"
	// Version is stamped into the registration metadata and names the
	// published artifact the host hot-swaps.
	Version = "0.1.0"
	// SchemaVersion is the RPC contract this plugin targets. Versions 2-8 add
	// request-body and interceptor features the quota path does not use, and
	// the host rejects a plugin reporting a schema newer than the host speaks.
	// Reporting 1 keeps the plugin loadable by every CPA release that has
	// quota providers.
	SchemaVersion uint32 = 1
)

// ErrQuotaUnavailable is returned when the plugin is quiescing or shutting
// down. The message never carries credential material.
var ErrQuotaUnavailable = errors.New("quota is temporarily unavailable")

// UnsupportedProviderError reports a credential whose provider this plugin
// does not handle. Returning an error keeps the management endpoint from
// showing a fabricated number for the wrong provider.
type UnsupportedProviderError struct {
	Provider string
}

func (e UnsupportedProviderError) Error() string {
	return fmt.Sprintf("provider %q is not handled by this plugin", e.Provider)
}

// IsUnsupportedProvider reports whether err came from a foreign provider key.
func IsUnsupportedProvider(err error) bool {
	var unsupported UnsupportedProviderError
	return errors.As(err, &unsupported)
}

// ProviderKey normalizes the provider value CPA passes on a quota request.
func ProviderKey(value string) string {
	return strings.TrimSpace(value)
}
