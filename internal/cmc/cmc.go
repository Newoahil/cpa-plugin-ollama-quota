// Package cmc collects one Command Code (CMC) account's remaining quota.
//
// Two account endpoints are read, both fixed paths on the account API root and
// both authenticated with "Authorization: Bearer <API key>":
//
//	GET /alpha/billing/credits       remaining credits and the 5-hour/weekly windows
//	GET /alpha/billing/subscriptions plan identity and billing period end (best effort)
//
// The account API lives on the provider authority but outside the
// OpenAI-compatible surface a credential's base_url points at
// (<authority>/provider/v1), so the root is derived by trimming that suffix
// (see AccountAPIBase).
//
// /alpha/whoami?limits=1 is deliberately not called: it only names the mailbox
// and organisation the key belongs to, and nothing in the quota model this
// plugin renders has a field for that identity — the host already keys every
// card by credential. Reading it would spend a second request with the key that
// can only fail.
//
// A reading that cannot be verified is never reported: a response the parser
// does not recognise is a parse failure, not a zero. The subscriptions call is
// optional; when it fails the credits figure and the windows are still
// returned, only without a plan name or the monthly remaining figure.
//
// This package is deliberately independent of CPA: it imports no pluginapi type
// and exposes a plain Go seam (see Transport). A credential's key never appears
// in a returned value, an error string or a log line.
package cmc

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the vendor's provider base when a credential carries no
	// base_url. It is the OpenAI-compatible surface; AccountAPIBase trims the
	// suffix to reach the account API.
	DefaultBaseURL = "https://api.commandcode.ai"

	// CreditsPath and SubscriptionPath are the account endpoints, relative to
	// the account API root.
	CreditsPath      = "/alpha/billing/credits"
	SubscriptionPath = "/alpha/billing/subscriptions"

	// DefaultTimeout bounds one collection attempt.
	DefaultTimeout = 20 * time.Second

	// DefaultMaxBodyBytes bounds how much of a response body is read.
	DefaultMaxBodyBytes int64 = 4 << 20
)

// Window names. They are stable ids; Label is the display text.
const (
	WindowFiveHour = "5h"
	WindowWeekly   = "7d"
)

// Credential is what one collection attempt authenticates with.
type Credential struct {
	// APIKey is sent as "Authorization: Bearer <APIKey>".
	APIKey string
	// BaseURL is the provider base the credential was configured with. It may
	// point at <authority>/provider/v1; the account root is derived from it.
	// Empty means DefaultBaseURL.
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

// Balance is the remaining credit figure. Currency is empty when upstream
// reports credits rather than money, which is how Command Code reports them.
type Balance struct {
	Amount   float64
	Currency string
	// Included is the remaining plan credit, when upstream reported it.
	Included *float64
	// Purchased is the remaining bought credit, when upstream reported it.
	Purchased *float64
}

// Window is one quota window recovered from the credits response.
//
// A window exists only when both used and cap were readable: there is no
// representable window whose usage is unknown, so a drifted payload can never
// be rendered as "0% used".
type Window struct {
	// Name is WindowFiveHour or WindowWeekly.
	Name string
	// Label is the user-facing text for the window.
	Label string
	// UsedPercent is the share of the window consumed, in [0, 100].
	UsedPercent float64
	// ResetAt is when the window resets, when upstream stated it.
	ResetAt *time.Time
	// Note carries a degradation the page should show, for example that the
	// window is over its cap without a usable figure.
	Note string
}

// MonthlyCredit is the remaining monthly plan credit for the current billing
// period.
//
// It is a figure, not a window: upstream reports the remaining value without a
// used/cap pair, so there is no percentage to state. A consumer must show the
// number (with ResetAt, when known) and never derive a fraction from it.
type MonthlyCredit struct {
	// Remaining is the plan credit still available this period.
	Remaining float64
	// ResetAt is when the billing period ends and the allowance renews, when
	// the subscription stated it.
	ResetAt *time.Time
}

// Snapshot is one collection result.
type Snapshot struct {
	// Plan is the subscription's display name, when the subscription call
	// succeeded.
	Plan string
	// Balance is the remaining credit figure, nil when upstream reported none.
	Balance *Balance
	// Monthly is the remaining monthly plan credit. It is only present when the
	// subscription call succeeded: without it the account is not confirmed to
	// be on a plan with a monthly allowance.
	Monthly *MonthlyCredit
	// Windows holds the quota windows, five-hour first.
	Windows []Window
	// ObservedAt is when this reading was taken.
	ObservedAt time.Time
}

// HasData reports whether the snapshot carries anything displayable.
func (s Snapshot) HasData() bool {
	return s.Balance != nil || s.Monthly != nil || len(s.Windows) > 0
}

// Window returns the window with the given name.
func (s Snapshot) Window(name string) (Window, bool) {
	for _, window := range s.Windows {
		if window.Name == name {
			return window, true
		}
	}
	return Window{}, false
}

// AccountAPIBase derives the account API root (<scheme>://<authority>[/path])
// from a credential's provider base URL by trimming the OpenAI-compatible
// surface suffix. The host is preserved, so a self-hosted or staging base keeps
// working.
//
// An empty base resolves to DefaultBaseURL; a base that is not an absolute
// http(s) URL is refused rather than guessed at.
func AccountAPIBase(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("the Command Code account API base is not an absolute URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("the Command Code account API base uses an unsupported scheme")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	for _, suffix := range []string{"/provider/v1", "/provider"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	return parsed.Scheme + "://" + parsed.Host + path, nil
}
