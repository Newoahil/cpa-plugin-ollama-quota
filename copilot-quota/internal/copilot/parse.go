package copilot

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// userPayload is the subset of GET /copilot_internal/user this package reads.
//
// Two quota shapes are supported, matching what the three independent
// implementations (copilot-api, onWatch, axonhub) observe:
//
//   - quota_snapshots: one entry per bucket with entitlement, remaining,
//     percent_remaining, unlimited. Paid plans report premium_interactions
//     (and sometimes chat/completions).
//   - limited_user_quotas + monthly_quotas: remaining/total per bucket, used by
//     free accounts.
//
// Everything is decoded into json.RawMessage first so an entry of an unexpected
// type is skipped rather than failing the whole reading.
type userPayload struct {
	CopilotPlan          *string                    `json:"copilot_plan"`
	AccessTypeSKU        *string                    `json:"access_type_sku"`
	QuotaResetDateUTC    *string                    `json:"quota_reset_date_utc"`
	QuotaResetDate       *string                    `json:"quota_reset_date"`
	LimitedUserResetDate *string                    `json:"limited_user_reset_date"`
	QuotaSnapshots       map[string]json.RawMessage `json:"quota_snapshots"`
	LimitedUserQuotas    map[string]json.RawMessage `json:"limited_user_quotas"`
	MonthlyQuotas        map[string]json.RawMessage `json:"monthly_quotas"`
}

// snapshotEntry is one quota_snapshots entry.
type snapshotEntry struct {
	Entitlement      *float64 `json:"entitlement"`
	Remaining        *float64 `json:"remaining"`
	PercentRemaining *float64 `json:"percent_remaining"`
	Unlimited        bool     `json:"unlimited"`
}

// parseUserPayload turns one user endpoint body into a Snapshot.
//
// It is strict on purpose: a body with no recognisable quota block is a parse
// failure. It never returns a zero-filled reading, because a "0% used" card
// built from a payload this package does not understand is exactly the wrong
// number the SPEC forbids.
func parseUserPayload(body []byte, now time.Time) (Snapshot, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return unavailable(now, SourceUser), parseError("the Copilot endpoint returned an empty body")
	}
	var payload userPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// The decoder message can quote the body; only the class is reported.
		return unavailable(now, SourceUser), parseError("the Copilot endpoint returned a body that is not a JSON object")
	}

	resetAt, resetText := parseResetDate(&payload)

	windows := windowsFromSnapshots(payload.QuotaSnapshots, resetAt, resetText)
	// Only fill from the free-account shape for buckets quota_snapshots did not
	// already describe: when both are present, the snapshot carries the more
	// precise percentages.
	windows = append(windows, windowsFromLimitedQuotas(payload.LimitedUserQuotas, payload.MonthlyQuotas, windows, resetAt, resetText)...)
	sortWindows(windows)

	if len(windows) == 0 {
		return unavailable(now, SourceUser), parseError("the Copilot quota response did not contain a usable quota block")
	}

	snapshot := Snapshot{
		Plan:       planFor(stringValue(payload.AccessTypeSKU), stringValue(payload.CopilotPlan)),
		Windows:    windows,
		ObservedAt: now,
		Source:     SourceUser,
	}
	snapshot.Status = snapshot.deriveStatus(DefaultWarningPercent)
	return snapshot, nil
}

// windowsFromSnapshots reads quota_snapshots.
//
// A bucket is skipped when it reports neither a usable percent_remaining nor
// unlimited. A block with no usable entry contributes nothing; whether that is
// a failure is decided by the caller after both shapes are considered.
func windowsFromSnapshots(snapshots map[string]json.RawMessage, resetAt *time.Time, resetText string) []Window {
	if len(snapshots) == 0 {
		return nil
	}
	windows := make([]Window, 0, len(snapshots))
	keys := make([]string, 0, len(snapshots))
	for key := range snapshots {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		var entry snapshotEntry
		if err := json.Unmarshal(snapshots[key], &entry); err != nil {
			continue
		}
		window := Window{
			Name:      key,
			Label:     windowLabel(key),
			ResetAt:   resetAt,
			ResetText: resetText,
		}
		if entry.Unlimited {
			window.Unlimited = true
			window.Entitlement = numberValue(entry.Entitlement)
			window.Remaining = numberValue(entry.Remaining)
			windows = append(windows, window)
			continue
		}
		if entry.PercentRemaining == nil || !validPercent(*entry.PercentRemaining) {
			continue
		}
		window.UsedPercent = 100 - *entry.PercentRemaining
		if entry.Entitlement != nil && entry.Remaining != nil &&
			validCount(*entry.Entitlement) && validCount(*entry.Remaining) {
			window.Entitlement = *entry.Entitlement
			window.Remaining = *entry.Remaining
			window.HasCounts = true
		}
		windows = append(windows, window)
	}
	return windows
}

// windowsFromLimitedQuotas reads the free-account shape: remaining in
// limited_user_quotas, total in monthly_quotas.
//
// A bucket whose total is missing is skipped rather than treated as
// fully-remaining: without the total there is no honest fraction to show.
func windowsFromLimitedQuotas(limited, monthly map[string]json.RawMessage, existing []Window, resetAt *time.Time, resetText string) []Window {
	if len(limited) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(existing))
	for _, window := range existing {
		seen[window.Name] = true
	}

	keys := make([]string, 0, len(limited))
	for key := range limited {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	windows := make([]Window, 0, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		remaining, okRemaining := numberFromRaw(limited[key])
		if !okRemaining || !validCount(remaining) {
			continue
		}
		total, okTotal := numberFromRaw(monthly[key])
		if !okTotal || !validCount(total) || total <= 0 {
			continue
		}
		used := (total - remaining) / total * 100
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		windows = append(windows, Window{
			Name:        key,
			Label:       windowLabel(key),
			UsedPercent: used,
			Entitlement: total,
			Remaining:   remaining,
			HasCounts:   true,
			ResetAt:     resetAt,
			ResetText:   resetText,
		})
	}
	return windows
}

// parseResetDate returns the reset instant and, when it could not be parsed, the
// upstream wording.
func parseResetDate(payload *userPayload) (*time.Time, string) {
	candidates := []struct {
		value  *string
		layout string
	}{
		{payload.QuotaResetDateUTC, time.RFC3339},
		{payload.QuotaResetDate, "2006-01-02"},
		{payload.LimitedUserResetDate, "2006-01-02"},
	}
	var raw string
	for _, candidate := range candidates {
		value := strings.TrimSpace(stringValue(candidate.value))
		if value == "" {
			continue
		}
		if raw == "" {
			raw = value
		}
		if parsed, err := time.Parse(candidate.layout, value); err == nil {
			parsed = parsed.UTC()
			return &parsed, ""
		}
	}
	// Nothing parsed. Keep the first upstream value as display text, but only
	// when it is short enough to be a date rather than arbitrary payload data.
	if len(raw) <= 64 {
		return nil, raw
	}
	return nil, ""
}

// stringValue trims a decoded string pointer.
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// numberFromRaw decodes a JSON number, rejecting anything else. A string is not
// coerced: upstream data that changed type is not a number we can trust. A
// literal null is rejected too: it decodes into a zero, which would turn "this
// bucket has no value" into a reading of 0 and a "100% used" card upstream
// never reported.
func numberFromRaw(raw json.RawMessage) (float64, bool) {
	var value *float64
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return 0, false
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) {
		return 0, false
	}
	return *value, true
}

// numberValue returns a pointer's value or zero.
func numberValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

// validCount reports whether a count read from upstream is usable.
func validCount(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}
