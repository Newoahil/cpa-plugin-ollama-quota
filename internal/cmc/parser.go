package cmc

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// The credits payload:
//
//	{
//	  "credits": {"monthlyCredits": n, "purchasedCredits": n, "freeCredits": n, ...},
//	  "windowLimits": {
//	    "limited": bool,
//	    "fiveHour": {"used": n, "cap": n, "exceeded": bool, "resetAt": unixMillis},
//	    "weekly":   {"used": n, "cap": n, "exceeded": bool, "resetAt": unixMillis}
//	  }
//	}
//
// windowLimits.exceeded is deliberately NOT decoded. Command Code reports it as
// null while no window is over its cap and as the NAME of the over-cap window
// ("weekly") once one is, so a bool field makes exactly the exhausted accounts
// fail to decode and their whole snapshot get dropped. Each window's own
// exceeded bool already carries the information.
type creditsPayload struct {
	Credits *struct {
		MonthlyCredits   *float64 `json:"monthlyCredits"`
		PurchasedCredits *float64 `json:"purchasedCredits"`
		FreeCredits      *float64 `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits *struct {
		Limited  bool           `json:"limited"`
		FiveHour *accountWindow `json:"fiveHour"`
		Weekly   *accountWindow `json:"weekly"`
	} `json:"windowLimits"`
}

type accountWindow struct {
	Used     *float64 `json:"used"`
	Cap      *float64 `json:"cap"`
	Exceeded bool     `json:"exceeded"`
	// ResetAt is Unix milliseconds. It stays a number so a payload that spells
	// it as a string does not fail the whole decode; see resetTime.
	ResetAt json.RawMessage `json:"resetAt"`
}

// parsedCredits is the outcome of reading a credits payload.
type parsedCredits struct {
	Balance *Balance
	Windows []Window
}

// parseCredits maps the credits payload onto a snapshot's fields.
//
// A payload with neither a usable credit figure nor a usable window is a parse
// failure, not an empty success. A window the payload names but states
// unreadably is skipped rather than reported as "0% used" — the other window is
// still a real reading.
func parseCredits(body []byte) (parsedCredits, error) {
	var payload creditsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return parsedCredits{}, newError(KindParse, "credits", 0,
			"the Command Code credits endpoint returned a response that is not valid JSON")
	}

	result := parsedCredits{Balance: balanceFromPayload(payload.Credits)}
	if limits := payload.WindowLimits; limits != nil {
		for _, spec := range []struct {
			name  string
			label string
			// unlimited mirrors windowLimits.limited: when the account has no
			// cap, an exhausted window means nothing.
			window *accountWindow
		}{
			{WindowFiveHour, "5-hour session", limits.FiveHour},
			{WindowWeekly, "Weekly", limits.Weekly},
		} {
			window, ok := windowFromAccount(spec.name, spec.label, spec.window, limits.Limited)
			if ok {
				result.Windows = append(result.Windows, window)
			}
		}
	}

	if result.Balance == nil && len(result.Windows) == 0 {
		return parsedCredits{}, newError(KindParse, "credits", 0,
			"the credits endpoint returned no readable credit figure and no readable quota window")
	}
	return result, nil
}

// balanceFromPayload sums the credit buckets the payload actually reported.
// Either bucket alone is usable. A bucket that is present but not a usable
// figure is ignored rather than failing the whole read: the remaining credit is
// still the number the page needs.
func balanceFromPayload(credits *struct {
	MonthlyCredits   *float64 `json:"monthlyCredits"`
	PurchasedCredits *float64 `json:"purchasedCredits"`
	FreeCredits      *float64 `json:"freeCredits"`
}) *Balance {
	if credits == nil {
		return nil
	}
	var monthly, purchased, free *float64
	if usable(credits.MonthlyCredits) {
		monthly = credits.MonthlyCredits
	}
	if usable(credits.PurchasedCredits) {
		purchased = credits.PurchasedCredits
	}
	if usable(credits.FreeCredits) {
		free = credits.FreeCredits
	}
	if monthly == nil && purchased == nil && free == nil {
		return nil
	}
	total := 0.0
	for _, part := range []*float64{monthly, purchased, free} {
		if part != nil {
			total += *part
		}
	}
	balance := &Balance{Amount: roundCents(total), Currency: ""}
	if monthly != nil {
		value := roundCents(*monthly)
		balance.Included = &value
	}
	if purchased != nil {
		value := roundCents(*purchased)
		balance.Purchased = &value
	}
	return balance
}

// usable reports whether a credit figure is a finite, non-negative number.
func usable(value *float64) bool {
	if value == nil {
		return false
	}
	return !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0
}

// windowFromAccount converts one window. It reports false when the window
// cannot be stated: no window at all, no readable cap, or a window the account
// is not limited by.
func windowFromAccount(name, label string, window *accountWindow, limited bool) (Window, bool) {
	if window == nil || !limited {
		return Window{}, false
	}
	if window.Cap == nil || !usable(window.Cap) || *window.Cap <= 0 {
		return Window{}, false
	}
	cap := *window.Cap
	used := 0.0
	if window.Used != nil && usable(window.Used) {
		used = *window.Used
	}
	// An over-cap window saturates to fully used rather than reporting the raw
	// ratio, which can exceed 1.
	usedPercent := clampPercent(used / cap * 100)
	if window.Exceeded {
		usedPercent = 100
	}
	result := Window{Name: name, Label: label, UsedPercent: usedPercent}
	if reset, ok := resetTime(window.ResetAt); ok {
		result.ResetAt = &reset
	}
	return result, true
}

// resetTime reads windowLimits.*.resetAt, which is Unix MILLISECONDS. A number
// spelled as a JSON string is accepted; anything else, or a value outside a
// sane range, is dropped rather than rendered as a date the upstream did not
// state.
func resetTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return time.Time{}, false
	}
	millis, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(millis) || math.IsInf(millis, 0) || millis <= 0 {
		return time.Time{}, false
	}
	// Bound to a plausible instant so a value in seconds (or nonsense) cannot
	// show up as a year-56000 reset.
	if millis < 1e12 || millis > 4e12 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(millis)).UTC(), true
}

func clampPercent(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// roundCents keeps cents exact for the wire consumer.
func roundCents(amount float64) float64 { return math.Round(amount*100) / 100 }

// parseSubscription reads the plan identity. It reports false when the payload
// carries none, which the caller treats as "no plan name", not as a failure.
//
// The payload is {"success": bool, "data": {"planId": str, "status": str,
// "currentPeriodEnd": str}}. Only planId is used: the period end is the
// billing-renewal instant, and this plugin has no plan window to attach it to.
func parseSubscription(body []byte) (string, bool) {
	var payload struct {
		Success *bool `json:"success"`
		Data    *struct {
			PlanID string `json:"planId"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	if payload.Success != nil && !*payload.Success {
		return "", false
	}
	if payload.Data == nil {
		return "", false
	}
	plan := strings.TrimSpace(payload.Data.PlanID)
	if plan == "" {
		return "", false
	}
	return canonicalPlan(plan), true
}

// planNames maps a vendor plan id to its display name. The table is the
// vendor's own (CommandCode CLI); an unknown id is shown as-is rather than
// guessed at.
var planNames = map[string]string{
	"individual-goat":     "GOAT",
	"individual-pro":      "Pro",
	"individual-pro-v1":   "Pro",
	"individual-provider": "Provider",
	"individual-max":      "Max",
	"individual-ultra":    "Ultra",
	"teams-pro":           "Teams Pro",
}

// canonicalPlan resolves a plan id to its display name, preferring the longest
// matching prefix so "individual-goat" is not read as "individual-go".
func canonicalPlan(planID string) string {
	id := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(planID, "_", "-")))
	if id == "" {
		return ""
	}
	if name, ok := planNames[id]; ok {
		return name
	}
	best, bestLen := "", 0
	for key := range planNames {
		if len(key) > bestLen && strings.HasPrefix(id, key) {
			best, bestLen = key, len(key)
		}
	}
	if best == "" {
		return planID
	}
	return planNames[best]
}
