// Package copilot collects one GitHub Copilot account's remaining quota.
//
// There is exactly one upstream endpoint: the Copilot client's internal
// GET https://api.github.com/copilot_internal/user, authenticated with the
// account's own GitHub token. It returns the plan, the remaining quota per
// bucket and the reset date. It is not part of GitHub's documented REST API,
// so the parser is strict: a response it does not recognise is a parse failure,
// never a zero and never an estimate.
//
// The endpoint is a constant. Nothing in this package can point a request at
// another host, a redirect is never followed, and an account's token never
// appears in a returned value, an error string or a log line.
//
// The payload shape handled here was established by the T25 research
// (2026-11-05) against the implementations in ericc-ch/copilot-api,
// onllm-dev/onWatch and looplj/axonhub. No source text was copied: the SKU
// table and the two payload shapes are re-implemented from the observed
// behaviour. See docs/copilot-quota.md.
//
// This package is deliberately independent of CPA: it imports no pluginapi
// type and exposes a plain Go seam (Transport) that the plugin maps onto the
// host's HTTP policy.
package copilot

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	// UserURL is the only upstream endpoint this package ever reaches. It is a
	// constant, not configuration.
	UserURL = "https://api.github.com/copilot_internal/user"

	// GitHubOrigin is the origin UserURL belongs to. A credential that names a
	// different GitHub host is refused before any request is built.
	GitHubOrigin = "https://api.github.com"

	// DefaultTimeout bounds one collection attempt end to end.
	DefaultTimeout = 20 * time.Second

	// DefaultMaxBodyBytes bounds how much of a response body is read, so a
	// runaway response cannot exhaust the CPA process's memory.
	DefaultMaxBodyBytes int64 = 4 << 20

	// DefaultWarningPercent is the share of a bucket that may be used before a
	// reading turns from ok into warning: three quarters used.
	DefaultWarningPercent = 75.0

	// WindowMonthly is the id of the bucket Copilot resets each billing month.
	WindowMonthly = "premium_interactions"
)

// Status is the per-credential state shown on the quota page. It is derived
// from the reading, never supplied by the upstream.
type Status string

const (
	// StatusOK means every bucket is comfortably below the warning threshold.
	StatusOK Status = "ok"
	// StatusWarning means at least one bucket crossed the warning threshold.
	StatusWarning Status = "warning"
	// StatusExhausted means a bucket is fully used.
	StatusExhausted Status = "exhausted"
	// StatusUnavailable means no verifiable reading was obtained. A snapshot
	// with this status carries no figures, so nothing can be rendered as a
	// number.
	StatusUnavailable Status = "unavailable"
)

// Source names the upstream path a snapshot came from.
type Source string

const (
	SourceNone Source = ""
	// SourceUser is GET /copilot_internal/user.
	SourceUser Source = "copilot_internal_user"
)

// Credential is what one collection attempt authenticates with.
//
// The token is never logged, formatted into an error, or written anywhere. The
// struct is deliberately plain so a caller can read it straight from CPA's
// StorageJSON without this package knowing that shape.
type Credential struct {
	// Token is sent as "Authorization: Bearer <Token>" to the user endpoint.
	Token string
}

// Empty reports whether the credential carries nothing usable.
func (c Credential) Empty() bool { return strings.TrimSpace(c.Token) == "" }

// Redacted returns a copy safe to log or serialize: the token is blanked.
func (c Credential) Redacted() Credential { return Credential{} }

// String redacts the token so a stray "%v" or log call cannot leak it.
func (c Credential) String() string { return fmt.Sprintf("Credential{token:%s}", presence(c.Token)) }

// GoString keeps a "%#v" from printing the struct fields verbatim.
func (c Credential) GoString() string { return c.String() }

func presence(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return "absent"
	}
	return "set"
}

// Window is one quota bucket recovered from the upstream response.
//
// A window exists only when a numeric usage share was actually read, or when
// upstream explicitly reported the bucket as unlimited. UsedPercent is
// therefore a value, not a pointer: there is no representable window whose
// usage is unknown, which is what keeps a redesigned payload from being
// reported as "0% used".
type Window struct {
	// Name is the upstream bucket key ("premium_interactions", "chat", ...).
	Name string `json:"name"`
	// Label is the user-facing text for the bucket.
	Label string `json:"label"`
	// UsedPercent is the share of the bucket consumed, in [0, 100]. For an
	// unlimited bucket it is 0.
	UsedPercent float64 `json:"used_percent"`
	// Unlimited reports a bucket upstream declared unlimited.
	Unlimited bool `json:"unlimited,omitempty"`
	// Entitlement is the bucket's included amount, when upstream reported it.
	Entitlement float64 `json:"entitlement,omitempty"`
	// Remaining is what is left in the bucket, when upstream reported it.
	Remaining float64 `json:"remaining,omitempty"`
	// HasCounts reports whether Entitlement and Remaining were both read.
	HasCounts bool `json:"has_counts,omitempty"`
	// ResetAt is when the bucket resets, when the upstream stated it.
	ResetAt *time.Time `json:"reset_at,omitempty"`
	// ResetText is the upstream wording when ResetAt could not be parsed. It
	// is displayed as-is rather than guessed at.
	ResetText string `json:"reset_text,omitempty"`
}

// RemainingPercent is the share of the window still available.
func (w Window) RemainingPercent() float64 {
	if w.Unlimited {
		return 100
	}
	remaining := 100 - w.UsedPercent
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Snapshot is one collection result: everything the quota page shows for a
// credential, plus the error class when the reading failed.
//
// A failed snapshot is still returned alongside its error: it carries
// StatusUnavailable and no figures. Values from an earlier successful read are
// kept by the caller, not here, so an old figure can never be mistaken for a
// fresh one.
type Snapshot struct {
	// Plan is the subscription name, for example Free, Pro or Business.
	Plan string `json:"plan,omitempty"`
	// Windows holds the quota buckets, the monthly premium bucket first.
	Windows []Window `json:"windows,omitempty"`
	// Status is the derived display state.
	Status Status `json:"status"`
	// ObservedAt is when this reading was taken.
	ObservedAt time.Time `json:"observed_at"`
	// Source names the upstream path that produced the reading.
	Source Source `json:"source,omitempty"`
}

// HasData reports whether the snapshot carries anything displayable. A true
// value means the reading was verified: at least one real bucket came back. A
// plan name alone is not a quota reading.
func (s Snapshot) HasData() bool { return len(s.Windows) > 0 }

// Window returns the bucket with the given name.
func (s Snapshot) Window(name string) (Window, bool) {
	for _, window := range s.Windows {
		if window.Name == name {
			return window, true
		}
	}
	return Window{}, false
}

// Primary returns the bucket a summary should describe: the monthly premium
// bucket when present, otherwise the first bucket with exact counts.
func (s Snapshot) Primary() (Window, bool) {
	if window, ok := s.Window(WindowMonthly); ok {
		return window, true
	}
	for _, window := range s.Windows {
		if window.HasCounts && !window.Unlimited {
			return window, true
		}
	}
	if len(s.Windows) > 0 {
		return s.Windows[0], true
	}
	return Window{}, false
}

// deriveStatus computes the display status for a snapshot that carries a
// reading. An empty snapshot is unavailable.
func (s Snapshot) deriveStatus(warningPercent float64) Status {
	if !s.HasData() {
		return StatusUnavailable
	}
	status := StatusOK
	for _, window := range s.Windows {
		if window.Unlimited {
			continue
		}
		switch {
		case window.UsedPercent >= 100:
			return StatusExhausted
		case window.UsedPercent >= warningPercent:
			status = StatusWarning
		}
	}
	return status
}

// unavailable returns a snapshot that carries no figures.
func unavailable(now time.Time, source Source) Snapshot {
	return Snapshot{Status: StatusUnavailable, ObservedAt: now, Source: source}
}

// canonicalPlan normalises a plan name taken from the copilot_plan field.
func canonicalPlan(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	switch strings.ToLower(value) {
	case "free":
		return "Free"
	case "individual", "pro":
		return "Individual"
	case "pro_plus", "proplus":
		return "Pro+"
	case "business", "team":
		return "Business"
	case "enterprise":
		return "Enterprise"
	case "edu":
		return "Edu"
	}
	return value
}

// planFor maps access_type_sku to a display plan, falling back to copilot_plan.
//
// The SKU table is the one looplj/axonhub ships (2026-09). An unknown SKU is
// not guessed at: the copilot_plan value is used when it is known, and the
// reading stays usable without a plan name otherwise.
func planFor(sku, copilotPlan string) string {
	if plan := planForSKU(sku); plan != "" {
		return plan
	}
	return canonicalPlan(copilotPlan)
}

func planForSKU(sku string) string {
	switch strings.ToLower(strings.TrimSpace(sku)) {
	case "copilot_free", "free_limited_copilot":
		return "Free"
	case "copilot_pro":
		return "Pro"
	case "copilot_pro_plus":
		return "Pro+"
	case "copilot_business":
		return "Business"
	case "copilot_enterprise":
		return "Enterprise"
	case "free_educational_quota":
		return "Edu"
	default:
		return ""
	}
}

// windowLabel renders a bucket key as page text.
func windowLabel(name string) string {
	switch name {
	case WindowMonthly:
		return "Premium requests"
	case "chat":
		return "Chat"
	case "completions":
		return "Completions"
	}
	words := strings.ReplaceAll(strings.TrimSpace(name), "_", " ")
	if words == "" {
		return ""
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

// windowRank orders buckets for display: premium requests, chat, completions,
// then anything else alphabetically.
func windowRank(name string) int {
	switch name {
	case WindowMonthly:
		return 0
	case "chat":
		return 1
	case "completions":
		return 2
	default:
		return 3
	}
}

// sortWindows orders buckets for a stable display.
func sortWindows(windows []Window) {
	sort.SliceStable(windows, func(i, j int) bool {
		ri, rj := windowRank(windows[i].Name), windowRank(windows[j].Name)
		if ri != rj {
			return ri < rj
		}
		return windows[i].Name < windows[j].Name
	})
}

// validPercent reports whether a percentage read from upstream is usable.
func validPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}
