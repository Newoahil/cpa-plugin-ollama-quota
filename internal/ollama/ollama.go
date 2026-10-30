// Package ollama collects one Ollama Cloud account's remaining quota.
//
// Two upstream paths are supported, tried in order (SPEC 3.2):
//
//  1. GET https://ollama.com/api/balance with the account's API key.
//  2. GET https://ollama.com/settings with the account's session cookie, with
//     the quota windows parsed out of the returned HTML.
//
// Both endpoints are fixed constants. Nothing in this package can point a
// request at another host, a redirect is never followed, and an account's key
// or cookie never appears in a returned value, an error string or a log line.
//
// A reading that cannot be verified is never reported. A page whose layout no
// longer matches yields a parse failure, not a zero and not an estimate
// (SPEC 4.3). Every failure falls in exactly one of four classes so the caller
// can decide whether to retry, back off, or ask the operator to fix a
// credential; see errors.go.
//
// This package is deliberately independent of CPA: it imports no pluginapi
// type and exposes a plain Go seam (see Transport) that #3 maps onto the host's
// HTTP policy.
//
// The parsing approach follows the Ollama collector in oh-my-cpa, which
// descends from the retired CPA-buddy (LGPL-3.0-only) and the projects it
// credits. This is an independent implementation; no upstream source text was
// copied. See docs/spec.md §7.
package ollama

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	// BalanceURL and SettingsURL are the only upstream endpoints this package
	// ever reaches. They are constants, not configuration.
	BalanceURL  = "https://ollama.com/api/balance"
	SettingsURL = "https://ollama.com/settings"

	// DefaultTimeout bounds one collection attempt end to end (SPEC 4.4).
	DefaultTimeout = 20 * time.Second

	// DefaultMaxBodyBytes bounds how much of a response body is read, so a
	// runaway or hostile response cannot exhaust the CPA process's memory.
	DefaultMaxBodyBytes int64 = 4 << 20

	// DefaultWarningPercent is the window usage at which a reading turns from
	// ok into warning: three quarters used, a quarter left.
	DefaultWarningPercent = 75.0
)

// Window names. They are stable ids: the display label is Window.Label, and
// anything persisted or matched on should use these.
const (
	WindowFiveHour = "5h"
	WindowWeekly   = "7d"
)

// Status is the per-credential state shown on the quota page (SPEC 3.1). It is
// derived from the reading, never supplied by the upstream.
type Status string

const (
	// StatusOK means every window is comfortably below the warning threshold.
	StatusOK Status = "ok"
	// StatusWarning means at least one window crossed the warning threshold.
	StatusWarning Status = "warning"
	// StatusExhausted means a window is fully used or the balance is zero.
	StatusExhausted Status = "exhausted"
	// StatusUnavailable means no verifiable reading was obtained. A snapshot
	// with this status carries no balance and no windows, so nothing can be
	// rendered as a number (SPEC 3.1).
	StatusUnavailable Status = "unavailable"
)

// Source names the upstream path a snapshot came from.
type Source string

const (
	SourceNone     Source = ""
	SourceBalance  Source = "balance"
	SourceSettings Source = "settings"
)

// Balance is a credit figure with its currency. The documented endpoints
// report US dollars; Currency is carried so the display layer never has to
// assume a symbol.
//
// Amount is the credit the account can actually spend: the included plan credit
// plus any purchased credit. Included and Purchased keep the split when upstream
// reported it, so a page can say "$0.00 included, $12.40 purchased" while the
// exhaustion check still sees the $12.40 (SPEC 3.1, AC-2).
type Balance struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	// Included is the remaining plan credit, when upstream reported it.
	Included *float64 `json:"included,omitempty"`
	// Purchased is the remaining bought credit, when upstream reported it.
	Purchased *float64 `json:"purchased,omitempty"`
}

// Valid reports whether the amount is a finite, non-negative figure. A value
// that fails this check is rejected at parse time rather than displayed.
func (b Balance) Valid() bool {
	return !math.IsNaN(b.Amount) && !math.IsInf(b.Amount, 0) && b.Amount >= 0
}

// String renders the balance for display, for example "$12.40".
func (b Balance) String() string {
	symbol := "$"
	switch b.Currency {
	case "EUR":
		symbol = "€"
	case "GBP":
		symbol = "£"
	}
	return fmt.Sprintf("%s%.2f", symbol, b.Amount)
}

// Window is one quota window recovered from an upstream response.
//
// A window exists only when a numeric usage share was actually read. UsedPercent
// is therefore a value, not a pointer: there is no representable window whose
// usage is unknown, which is what keeps a redesigned page from being reported
// as "0% used" (SPEC 4.3).
type Window struct {
	// Name is WindowFiveHour or WindowWeekly.
	Name string `json:"name"`
	// Label is the user-facing text for the window.
	Label string `json:"label"`
	// UsedPercent is the share of the window consumed, in [0, 100].
	UsedPercent float64 `json:"used_percent"`
	// ResetAt is when the window resets, when the upstream stated it exactly
	// or a relative hint could be anchored to ObservedAt.
	ResetAt *time.Time `json:"reset_at,omitempty"`
	// ResetText is the wording the page used when ResetAt could not be
	// derived. It is displayed as-is rather than guessed at.
	ResetText string `json:"reset_text,omitempty"`
	// ResetDerived reports that ResetAt came from a relative hint ("in 3h")
	// rather than from an absolute instant, so a UI can say "about".
	ResetDerived bool `json:"reset_derived,omitempty"`
}

// RemainingPercent is the share of the window still available.
func (w Window) RemainingPercent() float64 {
	remaining := 100 - w.UsedPercent
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Snapshot is one collection result: everything SPEC 3.1 shows for a
// credential, plus the error class when the reading failed.
//
// A failed snapshot is still returned alongside its error, because status is
// part of the answer: it carries StatusUnavailable, the ObservedAt of the
// attempt, and no balance or windows. Values from an earlier successful read
// are kept by the caller, not here, so an old figure can never be mistaken for
// a fresh one.
type Snapshot struct {
	// Plan is the subscription name, for example Free or Pro.
	Plan string `json:"plan,omitempty"`
	// Balance is the credit figure, nil when the path did not report one.
	Balance *Balance `json:"balance,omitempty"`
	// Windows holds the quota windows, five-hour first.
	Windows []Window `json:"windows,omitempty"`
	// Status is the derived display state.
	Status Status `json:"status"`
	// ObservedAt is when this reading was taken.
	ObservedAt time.Time `json:"observed_at"`
	// Source names the upstream path that produced the reading.
	Source Source `json:"source,omitempty"`
}

// HasData reports whether the snapshot carries anything displayable. A true
// value means the reading was verified: at least one real figure came back.
func (s Snapshot) HasData() bool {
	return s.Balance != nil || len(s.Windows) > 0
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

// deriveStatus computes the display status for a snapshot that carries a
// reading. An empty snapshot is unavailable.
//
// A zero *available* balance is exhausted. Balance.Amount is the total the
// account can spend, so having spent the included credit while purchased credit
// remains is not exhausted (SPEC 3.1, AC-2).
func (s Snapshot) deriveStatus(warningPercent float64) Status {
	if !s.HasData() {
		return StatusUnavailable
	}
	if s.Balance != nil && s.Balance.Amount == 0 {
		return StatusExhausted
	}
	status := StatusOK
	for _, window := range s.Windows {
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

// canonicalPlan normalises a plan name taken from an upstream payload.
func canonicalPlan(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	switch strings.ToLower(value) {
	case "free":
		return "Free"
	case "pro":
		return "Pro"
	case "max":
		return "Max"
	case "team":
		return "Team"
	case "enterprise":
		return "Enterprise"
	case "trial":
		return "Trial"
	}
	return value
}

// currencyFor maps a currency symbol to its ISO code.
func currencyFor(symbol string) string {
	switch symbol {
	case "€":
		return "EUR"
	case "£":
		return "GBP"
	default:
		return "USD"
	}
}
