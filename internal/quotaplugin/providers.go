package quotaplugin

import "strings"

// The provider keys this plugin serves. They are copied verbatim from what the
// host derives for a credential, and the spelling matters:
//
//   - the host matches a quota provider with normalizeProviderID, which is
//     ToLower + TrimSpace and does NOT collapse internal whitespace
//     (internal/util/provider.go:18-27, internal/pluginhost/quota_provider.go);
//   - "openai-compatible-command code" is what a credential of type
//     "openai-compatible-command code" produces, spaces included. Rewriting it
//     (for example to "openai-compatible-command-code") makes the plugin
//     never match that credential.
const (
	// CommandCodeProvider is the key of a Command Code credential.
	CommandCodeProvider = "openai-compatible-command code"
	// OpenCodeProvider is the key of an OpenCode Go credential.
	OpenCodeProvider = "openai-compatible-opencode"
)

// SupportedProviders lists every provider key the quota path serves, in the
// order DescribeQuota reports them.
var SupportedProviders = []string{QuotaIdentifier, CommandCodeProvider, OpenCodeProvider}

// quotaProviderKey normalizes a provider key the way the host does. It is
// TrimSpace only: no case folding is needed because both call sites compare
// against lower-case constants, and no whitespace rewriting is allowed because
// it would stop the Command Code key from matching.
func quotaProviderKey(value string) string {
	return strings.TrimSpace(value)
}

// canonicalProviderKey maps a normalized provider key onto one of the keys this
// plugin serves, matching case-insensitively. It reports false for a key that
// belongs to another provider.
func canonicalProviderKey(value string) (string, bool) {
	normalized := strings.ToLower(quotaProviderKey(value))
	for _, supported := range SupportedProviders {
		if normalized == supported {
			return supported, true
		}
	}
	return "", false
}

// providerDisplayName is the QuotaGroup name for a provider key.
func providerDisplayName(provider string) string {
	switch provider {
	case CommandCodeProvider:
		return "Command Code"
	case OpenCodeProvider:
		return "OpenCode Go"
	default:
		return "Ollama"
	}
}
