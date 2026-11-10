package quotaplugin

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Credential field names, as the host stores them.
//
// A quota request can reach the plugin with a credential in one of two shapes,
// and api_key/base_url land in different places in each:
//
//   - a credential synthesized from config.yaml's openai-compatibility section
//     keeps api_key and base_url in Attributes
//     (internal/watcher/synthesizer/config.go:329-345). Such a credential has
//     no path, so the host's auth-file listing filters it out and it never
//     appears on the quota page;
//   - a credential loaded from auths/*.json keeps api_key and base_url in
//     Metadata, which is the auth file's own JSON
//     (internal/watcher/synthesizer/file.go:196-211). Its Attributes carry only
//     source/path/source_backend. StorageJSON holds the same JSON.
//
// A verified plugin-side capture from CPA v8.0.47 shows a disk credential
// arriving exactly that way: metadata {api_key, base_url, type}, attributes
// {auth_kind, path, source, source_backend}. Reading Attributes first therefore
// always misses the key on the shape that actually appears on the quota page,
// which is why Attributes is last and only ever a fallback.
//
// Only api_key and base_url are read: the Ollama-specific cookie path stays in
// credentialFromStorage, because neither Command Code nor OpenCode Go accepts a
// session cookie for a usage read.
const (
	credentialKeyBaseURL = "base_url"
)

// apiCompatibleCredential reads the API key and base URL of an
// OpenAI-compatible provider credential (Command Code, OpenCode Go).
//
// Precedence per field, in the order the PM statement mandates:
//
//	Metadata[key] → StorageJSON[key] → Attributes[key]
//
// Metadata and StorageJSON are two copies of the same auth file, so their
// relative order cannot change the result for any shape the host produces; the
// order that does matter is Attributes LAST, because that is the only place a
// config-synthesized credential carries these fields and the only place a disk
// credential does not.
//
// baseURL is returned as stored; callers fall back to their provider default
// when it is empty. The key never appears in the returned error.
func apiCompatibleCredential(attributes map[string]string, metadata map[string]any, storageJSON []byte) (apiKey, baseURL string, err error) {
	stored := map[string]any{}
	if len(storageJSON) > 0 && !isJSONNull(storageJSON) {
		if errUnmarshal := json.Unmarshal(storageJSON, &stored); errUnmarshal != nil {
			return "", "", fmt.Errorf("%w: the stored credential is not a JSON object", ErrIncompleteCredential)
		}
	}

	apiKey = firstValue(credentialKeyAPIKey, metadata, stored, attributes)
	baseURL = firstValue(credentialKeyBaseURL, metadata, stored, attributes)
	if apiKey == "" {
		return "", baseURL, fmt.Errorf("%w: no %q field", ErrIncompleteCredential, credentialKeyAPIKey)
	}
	return apiKey, baseURL, nil
}

// firstValue returns the first non-empty spelling of one field, reading the
// layers in the order given.
func firstValue(key string, metadata, stored map[string]any, attributes map[string]string) string {
	for _, layer := range []map[string]any{metadata, stored} {
		if value := stringValue(layer, key); value != "" {
			return value
		}
	}
	return attributeValue(attributes, key)
}

// attributeValue reads a trimmed non-empty string from the host's immutable
// attribute map.
func attributeValue(attributes map[string]string, key string) string {
	if attributes == nil {
		return ""
	}
	return strings.TrimSpace(attributes[key])
}
