// Package opc collects one OpenCode Go account's remaining quota.
//
// One fixed endpoint is read, authenticated with "Authorization: Bearer <API
// key>":
//
//	GET https://opencode.ai/zen/go/v1/usage
//	→ {"usage":{"rolling"|"weekly"|"monthly":{"status","percent","resetsAt"}}}
//
// The endpoint is undocumented, so every field is read defensively and the
// package degrades instead of failing: the per-window status "rate-limited"
// means the window is fully used, an unrecognised status skips that window, and
// only a response with no readable window at all is an error.
//
// This package is deliberately independent of CPA: it imports no pluginapi type
// and exposes a plain Go seam (see Transport). A credential's key never appears
// in a returned value, an error string or a log line.
package opc

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the vendor's provider base when a credential carries no
	// base_url. The usage path is appended to it.
	DefaultBaseURL = "https://opencode.ai/zen/go"

	// UsagePath is the single usage endpoint, relative to the provider base.
	UsagePath = "/v1/usage"

	// DefaultTimeout bounds one collection attempt.
	DefaultTimeout = 20 * time.Second

	// DefaultMaxBodyBytes bounds how much of a response body is read.
	DefaultMaxBodyBytes int64 = 4 << 20
)

// Window names. They are the upstream keys, in display order.
const (
	WindowRolling = "rolling"
	WindowWeekly  = "weekly"
	WindowMonthly = "monthly"
)

// windowOrder is the order the page shows the windows in: shortest first.
var windowOrder = []string{WindowRolling, WindowWeekly, WindowMonthly}

// windowLabels is the display text for each window.
var windowLabels = map[string]string{
	WindowRolling: "Rolling",
	WindowWeekly:  "Weekly",
	WindowMonthly: "Monthly",
}

// Credential is what one collection attempt authenticates with.
type Credential struct {
	// APIKey is sent as "Authorization: Bearer <APIKey>".
	APIKey string
	// BaseURL is the provider base the credential was configured with. Empty
	// means DefaultBaseURL.
	BaseURL string
}

// Empty reports whether the credential carries nothing usable.
func (c Credential) Empty() bool { return strings.TrimSpace(c.APIKey) == "" }

// Redacted returns a copy safe to log or serialize: the key is blanked.
func (c Credential) Redacted() Credential { return Credential{BaseURL: c.BaseURL} }

// String redacts the key so a stray "%v" cannot leak it.
func (c Credential) String() string {
	presence := "absent"
	if strings.TrimSpace(c.APIKey) != "" {
		presence = "set"
	}
	return fmt.Sprintf("Credential{apiKey:%s, baseURL:%s}", presence, c.BaseURL)
}

// GoString keeps a "%#v" from printing the key.
func (c Credential) GoString() string { return c.String() }

// Window is one quota window recovered from the usage response.
type Window struct {
	// Name is WindowRolling, WindowWeekly or WindowMonthly.
	Name string
	// Label is the user-facing text for the window.
	Label string
	// UsedPercent is the share of the window consumed, in [0, 100].
	UsedPercent float64
	// ResetAt is when the window resets, when upstream stated it parseably.
	ResetAt *time.Time
	// Note carries a degradation the page should show, for example that
	// upstream reports the window rate limited.
	Note string
}

// Snapshot is one collection result.
type Snapshot struct {
	// Plan is left empty: the usage endpoint reports no plan identity, and the
	// host already keys every card by credential.
	Plan string
	// Windows holds the quota windows that were readable, shortest first.
	Windows []Window
	// ObservedAt is when this reading was taken.
	ObservedAt time.Time
}

// HasData reports whether the snapshot carries anything displayable.
func (s Snapshot) HasData() bool { return len(s.Windows) > 0 }

// Window returns the window with the given name.
func (s Snapshot) Window(name string) (Window, bool) {
	for _, window := range s.Windows {
		if window.Name == name {
			return window, true
		}
	}
	return Window{}, false
}

// UsageURL appends UsagePath to a provider base, without duplicating a path
// segment the base already carries (a base of ".../zen/go" and one of
// ".../zen/go/" both produce one "/v1/usage").
func UsageURL(rawBase string) (string, error) {
	trimmed := strings.TrimSpace(rawBase)
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("the OpenCode Go API base is not an absolute URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("the OpenCode Go API base uses an unsupported scheme")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(path, UsagePath) {
		path += UsagePath
	}
	return parsed.Scheme + "://" + parsed.Host + path, nil
}
