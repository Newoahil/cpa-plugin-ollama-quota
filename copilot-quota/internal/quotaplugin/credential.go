package quotaplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/LambdaTheory/cpa-plugin-copilot-quota/internal/copilot"
)

// ErrIncompleteCredential reports a stored credential that carries no GitHub
// access token. It is an auth-class failure for the caller: the operator has to
// fix the credential, and no request is sent upstream.
var ErrIncompleteCredential = errors.New("the Copilot credential is incomplete")

// ErrUnsupportedCredential reports a credential this plugin must not use: a
// GitHub Enterprise host or a non-Copilot credential type. Sending such a token
// to api.github.com would hand a tenant secret to another host, so it is
// refused before any request is built.
var ErrUnsupportedCredential = errors.New("the Copilot credential is not supported")

// Credential field names, as stored by the syepes GitHub Copilot auth plugin.
//
// The plugin declares no AuthProvider of its own; it reads the auth files the
// syepes plugin wrote, which the host passes verbatim as QuotaFetchRequest's
// StorageJSON. That file is one flat JSON object:
//
//	{"type":"copilot","github_access_token":"...","github_refresh_token":"...",
//	 "github_login":"...","github_base_url":"..."}
//
// Nothing else in the file is read: the refresh token is never touched, because
// refreshing is the auth plugin's job and the host re-reads the file for every
// fetch (internal/pluginhost/quota_provider.go).
const (
	credentialKeyType        = "type"
	credentialKeyAccessToken = "github_access_token"
	credentialKeyBaseURL     = "github_base_url"

	// credentialTypeCopilot is the provider discriminator the auth plugin
	// writes. A credential of another type is left alone even if the host
	// routes it here.
	credentialTypeCopilot = "copilot"
)

// credentialFromStorage reads the GitHub token out of a Copilot credential.
//
// The file wins over Metadata, because the file is what the operator and the
// auth plugin write; Metadata is a fallback for runtime-only credentials that
// have no file on disk.
func credentialFromStorage(storageJSON []byte, metadata map[string]any) (copilot.Credential, error) {
	values := make(map[string]any, len(metadata)+4)
	for key, value := range metadata {
		values[key] = value
	}
	if len(storageJSON) > 0 && !isJSONNull(storageJSON) {
		var stored map[string]any
		if errUnmarshal := json.Unmarshal(storageJSON, &stored); errUnmarshal != nil {
			// The payload can only be the operator's credential file. Reporting
			// the class rather than the decoder's byte offset keeps the shape
			// of a secret out of the message.
			return copilot.Credential{}, fmt.Errorf("%w: the stored credential is not a JSON object", ErrIncompleteCredential)
		}
		for key, value := range stored {
			values[key] = value
		}
	}

	if credentialType := strings.ToLower(stringValue(values, credentialKeyType)); credentialType != "" && credentialType != credentialTypeCopilot {
		return copilot.Credential{}, fmt.Errorf("%w: the credential type is not %q", ErrUnsupportedCredential, credentialTypeCopilot)
	}
	if baseURL := stringValue(values, credentialKeyBaseURL); baseURL != "" && !isGitHubDotCom(baseURL) {
		return copilot.Credential{}, fmt.Errorf("%w: only github.com accounts are supported, not a GitHub Enterprise host", ErrUnsupportedCredential)
	}

	credential := copilot.Credential{Token: stringValue(values, credentialKeyAccessToken)}
	if credential.Empty() {
		return credential, fmt.Errorf("%w: no %q field", ErrIncompleteCredential, credentialKeyAccessToken)
	}
	return credential, nil
}

// isGitHubDotCom reports whether a stored base URL points at github.com. A bare
// host (no scheme) is accepted too: the auth plugin's field is omitempty and
// some writers store "github.com".
func isGitHubDotCom(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "" {
		host = strings.TrimSuffix(strings.TrimPrefix(trimmed, "//"), "/")
	}
	host = strings.ToLower(host)
	return host == "github.com" || host == "api.github.com"
}

// stringValue reads a trimmed non-empty string from a decoded JSON object. Any
// other type is treated as absent rather than stringified: a nested object
// rendered as text could contain a secret and must never become a header.
func stringValue(values map[string]any, key string) string {
	raw, ok := values[key]
	if !ok {
		return ""
	}
	text, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// isJSONNull reports an all-whitespace or literal-null credential payload, which
// the host sends when a runtime credential has no backing file.
func isJSONNull(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}
