package ollama

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// The documented balance payload (https://docs.ollama.com/api/balance):
//
//	{
//	  "included": {
//	    "balance_usd": number,     // remaining included credit
//	    "allowance_usd": number,   // credit the period started with
//	    "period": {"from": date-time, "until": date-time}
//	  },
//	  "purchased": {"balance_usd": number}   // separately bought credit
//	}
//
// Legacy plans replace those three fields with "session" and "weekly", each a
// LegacyBalanceLimit {remaining_percent: 0..100, resets_at: date-time}.
//
// Both credit buckets are available money: a free plan that has spent its
// included credit but still holds purchased credit is not exhausted, and
// reporting only included.balance_usd would tell the operator $0.00 while
// ollama.com shows a positive balance (SPEC 3.1, AC-2).

// balancePayload is the subset of the response this package reads. Every
// numeric field is a *usdAmount so an absent field stays distinguishable from a
// genuine zero: a missing figure must never be rendered as $0.00.
type balancePayload struct {
	Included *balanceIncluded `json:"included"`
	// Plan is not part of the documented schema; Ollama may add it. When
	// absent, the plan is left blank rather than guessed.
	Plan      string            `json:"plan"`
	Purchased *balancePurchased `json:"purchased"`
}

type balanceIncluded struct {
	BalanceUSD   *usdAmount     `json:"balance_usd"`
	AllowanceUSD *usdAmount     `json:"allowance_usd"`
	Period       *balancePeriod `json:"period"`
	Session      *legacyLimit   `json:"session"`
	Weekly       *legacyLimit   `json:"weekly"`
}

type balancePurchased struct {
	BalanceUSD *usdAmount `json:"balance_usd"`
}

type balancePeriod struct {
	From  string `json:"from"`
	Until string `json:"until"`
}

// legacyLimit is one legacy-plan window.
type legacyLimit struct {
	RemainingPercent *float64 `json:"remaining_percent"`
	ResetsAt         string   `json:"resets_at"`
}

// usdAmount accepts a JSON number or a decimal string: the endpoint has been
// observed to vary between the two spellings. A null, an absent field and an
// empty string all stay absent.
type usdAmount struct {
	value   float64
	present bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (a *usdAmount) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "null" || raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, `"`) {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		raw = strings.TrimSpace(text)
	}
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return err
	}
	a.value = value
	a.present = true
	return nil
}

// parsedBalance is the outcome of reading a balance payload.
type parsedBalance struct {
	Plan    string
	Balance *Balance
	Windows []Window
}

// parseBalance maps the balance payload onto a Snapshot's fields.
//
// The current-plan shape yields a balance — the sum of the included and the
// purchased credit, which is the money the account can actually spend — and the
// legacy shape yields the session and weekly windows. A payload with neither is
// a parse failure, not an empty success.
func parseBalance(body []byte) (parsedBalance, error) {
	var payload balancePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// The decoder error names a byte offset, never a credential, but it is
		// not useful to an operator either; reduce it to the class.
		return parsedBalance{}, newError(KindParse, "balance", 0,
			"the Ollama balance endpoint returned a response that is not valid JSON")
	}

	result := parsedBalance{Plan: canonicalPlan(payload.Plan)}

	balance, err := balanceFromPayload(payload)
	if err != nil {
		return parsedBalance{}, err
	}
	result.Balance = balance

	if included := payload.Included; included != nil {
		windows, err := legacyWindows(included)
		if err != nil {
			return parsedBalance{}, err
		}
		result.Windows = windows
	}

	if !result.hasData() {
		return parsedBalance{}, newError(KindParse, "balance", 0,
			"the balance endpoint returned no balance and no quota window")
	}
	return result, nil
}

// balanceFromPayload sums the credit buckets the payload actually reported.
// Either bucket alone is a usable reading; a bucket that is present but cannot
// be read is a failure rather than being skipped, because a silently dropped
// purchased credit is exactly the $0.00-while-money-remains bug.
func balanceFromPayload(payload balancePayload) (*Balance, error) {
	var included, purchased *float64
	if payload.Included != nil && payload.Included.BalanceUSD != nil && payload.Included.BalanceUSD.present {
		included = &payload.Included.BalanceUSD.value
	}
	if payload.Purchased != nil && payload.Purchased.BalanceUSD != nil && payload.Purchased.BalanceUSD.present {
		purchased = &payload.Purchased.BalanceUSD.value
	}
	if included == nil && purchased == nil {
		return nil, nil
	}

	var total float64
	for _, part := range []*float64{included, purchased} {
		if part == nil {
			continue
		}
		amount, ok := validUSD(*part)
		if !ok {
			return nil, newError(KindParse, "balance", 0,
				"the balance endpoint reported an unusable credit figure")
		}
		total += amount
	}
	balance := &Balance{Amount: roundCents(total), Currency: "USD"}
	if included != nil {
		rounded := roundCents(*included)
		balance.Included = &rounded
	}
	if purchased != nil {
		rounded := roundCents(*purchased)
		balance.Purchased = &rounded
	}
	return balance, nil
}

// hasData reports whether anything displayable was read.
func (p parsedBalance) hasData() bool {
	return p.Balance != nil || len(p.Windows) > 0
}

// legacyWindows converts the legacy session/weekly limits into windows.
// Remaining is converted to used, because Window.UsedPercent is the used
// convention.
//
// A limit the payload names but states unreadably is a failure: dropping it
// would report the other window and present a partial page as a complete
// reading (SPEC 4.3).
func legacyWindows(included *balanceIncluded) ([]Window, error) {
	var windows []Window
	for _, spec := range []struct {
		name  string
		title string
		limit *legacyLimit
	}{
		{WindowFiveHour, "5-hour session", included.Session},
		{WindowWeekly, "Weekly", included.Weekly},
	} {
		if spec.limit == nil {
			continue
		}
		if spec.limit.RemainingPercent == nil {
			return nil, newError(KindParse, "balance", 0,
				"the balance endpoint named a quota window without a readable remaining_percent")
		}
		remaining := *spec.limit.RemainingPercent
		used, ok := normalizePercent(remaining, true)
		if !ok {
			return nil, newError(KindParse, "balance", 0,
				"the balance endpoint reported an impossible remaining_percent for a quota window")
		}
		window := Window{Name: spec.name, Label: spec.title, UsedPercent: used}
		if spec.limit.ResetsAt != "" {
			if at, err := time.Parse(time.RFC3339, spec.limit.ResetsAt); err == nil {
				window.ResetAt = &at
			} else {
				window.ResetText = spec.limit.ResetsAt
			}
		}
		windows = append(windows, window)
	}
	return windows, nil
}

// validUSD rejects a credit figure that cannot be shown: negative, non-finite,
// or beyond the range where cents stay an exact integer.
func validUSD(amount float64) (float64, bool) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return 0, false
	}
	if amount > maxExactCents/100 {
		return 0, false
	}
	return amount, true
}

// roundCents keeps cents exact for the wire consumer.
func roundCents(amount float64) float64 {
	return math.Round(amount*100) / 100
}

// maxExactCents bounds a figure so cents stay an exact integer
// (JavaScript's Number.MAX_SAFE_INTEGER).
const maxExactCents = 9_007_199_254_740_991
