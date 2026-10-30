package quotaplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
)

// ErrIncompleteCredential reports a stored credential that carries neither an
// API key nor a session cookie. It is an auth-class failure for the caller: the
// operator has to fix the credential, and no request is sent upstream.
var ErrIncompleteCredential = errors.New("the Ollama credential is incomplete")

// Credential field names, as stored by CPA and by omc.
//
// CPA keeps a credential as one JSON file under auths/, whose keys land in
// coreauth.Auth.Metadata unchanged (internal/watcher/synthesizer/file.go:211).
// There is no Ollama-specific auth parser in CPA v8.0.47 — `type: "ollama"` is
// accepted as an opaque provider key — so the plugin defines the shape it
// reads. Two independent sources agree on the names:
//
//   - API key: "api_key" is CPA's canonical metadata key
//     (sdk/cliproxy/auth/classification.go:16 AttributeAPIKey), the one
//     CanonicalCredentialMetadataKey rewrites "api-key" into, and the one
//     AuthKind() uses to classify a credential (classification.go:40).
//   - Cookie: omc stores the Ollama session cookie under "cookie" in its write
//     path (internal/api/ollama_accounts.go:63 payload.Cookie `json:"cookie"`).
//
// Both are read from the credential file first and from QuotaFetchRequest's
// Metadata map as a fallback, because Metadata is populated for runtime-only
// credentials that have no file on disk.
const (
	credentialKeyAPIKey = "api_key"
	credentialKeyCookie = "cookie"
	// credentialKeyType is the provider discriminator every CPA auth file has.
	credentialKeyType = "type"
)

// credentialFromStorage reads the API key and cookie out of a credential.
//
// Precedence follows SPEC 3.2: an API key wins, because the balance endpoint is
// the primary path. Callers get both when both are present so the collector can
// still fall back to the cookie if the key turns out to be rejected.
func credentialFromStorage(storageJSON []byte, metadata map[string]any) (ollama.Credential, error) {
	values := make(map[string]any, len(metadata)+4)
	for key, value := range metadata {
		values[key] = value
	}
	if len(storageJSON) > 0 && !isJSONNull(storageJSON) {
		var stored map[string]any
		if errUnmarshal := json.Unmarshal(storageJSON, &stored); errUnmarshal != nil {
			// The payload can only be the operator's credential file. Reporting
			// the class rather than the decoder's byte offset keeps the shape of
			// a secret out of the message.
			return ollama.Credential{}, fmt.Errorf("%w: the stored credential is not a JSON object", ErrIncompleteCredential)
		}
		for key, value := range stored {
			// The file wins over metadata: it is what the operator edited.
			values[key] = value
		}
	}

	credential := ollama.Credential{
		APIKey: stringValue(values, credentialKeyAPIKey),
		Cookie: stringValue(values, credentialKeyCookie),
	}
	if credential.Empty() {
		return credential, fmt.Errorf("%w: no %q or %q field", ErrIncompleteCredential, credentialKeyAPIKey, credentialKeyCookie)
	}
	return credential, nil
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
