package cmc

import (
	"strings"
	"testing"
	"time"
)

// creditsFixture is the credits payload as the vendor returns it for an account
// that is inside both windows: windowLimits.exceeded is null and each window
// carries its own exceeded bool.
const creditsFixture = `{
  "credits": {
    "monthlyCredits": 12.5,
    "purchasedCredits": 3,
    "freeCredits": 1,
    "belowThreshold": false
  },
  "windowLimits": {
    "limited": true,
    "exceeded": null,
    "fiveHour": {"used": 30, "cap": 100, "exceeded": false, "resetAt": 1762000000000},
    "weekly":   {"used": 80, "cap": 100, "exceeded": false, "resetAt": 1762500000000}
  }
}`

// exceededFixture is the same account with the weekly window over its cap: the
// vendor then spells windowLimits.exceeded as the window's NAME. Decoding it as
// a bool made exactly these accounts fail the whole decode.
const exceededFixture = `{
  "credits": {"monthlyCredits": 0, "purchasedCredits": 0, "freeCredits": 0},
  "windowLimits": {
    "limited": true,
    "exceeded": "weekly",
    "fiveHour": {"used": 10, "cap": 100, "exceeded": false, "resetAt": 1762000000000},
    "weekly":   {"used": 140, "cap": 100, "exceeded": true, "resetAt": 1762500000000}
  }
}`

// TestParseCreditsReadsBalanceAndWindows is the mapping fixture: every credit
// bucket is summed, and each window's used share is converted to the percent the
// snapshot carries.
func TestParseCreditsReadsBalanceAndWindows(t *testing.T) {
	parsed, err := parseCredits([]byte(creditsFixture))
	if err != nil {
		t.Fatalf("parse credits: %v", err)
	}
	if parsed.Balance == nil {
		t.Fatal("no balance")
	}
	// 12.5 monthly + 3 purchased + 1 free.
	if parsed.Balance.Amount != 16.5 {
		t.Errorf("balance = %v, want 16.5", parsed.Balance.Amount)
	}
	if parsed.Balance.Included == nil || *parsed.Balance.Included != 12.5 {
		t.Errorf("included = %v, want 12.5", parsed.Balance.Included)
	}
	if parsed.Balance.Purchased == nil || *parsed.Balance.Purchased != 3 {
		t.Errorf("purchased = %v, want 3", parsed.Balance.Purchased)
	}
	if len(parsed.Windows) != 2 {
		t.Fatalf("windows = %+v, want two", parsed.Windows)
	}
	if parsed.Windows[0].Name != WindowFiveHour || parsed.Windows[0].UsedPercent != 30 {
		t.Errorf("five-hour window = %+v", parsed.Windows[0])
	}
	if parsed.Windows[1].Name != WindowWeekly || parsed.Windows[1].UsedPercent != 80 {
		t.Errorf("weekly window = %+v", parsed.Windows[1])
	}
}

// The resetAt values are Unix MILLISECONDS: 1762000000000 ms is 2025-11-01
// 12:26:40 UTC. Treating them as seconds would land in the year 57800.
func TestParseCreditsConvertsResetMilliseconds(t *testing.T) {
	parsed, err := parseCredits([]byte(creditsFixture))
	if err != nil {
		t.Fatalf("parse credits: %v", err)
	}
	fiveHour, ok := windowNamed(parsed.Windows, WindowFiveHour)
	if !ok || fiveHour.ResetAt == nil {
		t.Fatalf("five-hour window has no reset time: %+v", parsed.Windows)
	}
	want := time.UnixMilli(1762000000000).UTC()
	if !fiveHour.ResetAt.Equal(want) {
		t.Errorf("reset = %s, want %s", fiveHour.ResetAt, want)
	}
	if fiveHour.ResetAt.Year() != 2025 {
		t.Errorf("reset year = %d, want 2025 (milliseconds, not seconds)", fiveHour.ResetAt.Year())
	}
}

// The exhausted account is the case a bool-decode of windowLimits.exceeded
// broke: the whole payload must still parse, the balance stays readable, and the
// over-cap window saturates to fully used.
func TestParseCreditsSurvivesExceededAsWindowName(t *testing.T) {
	parsed, err := parseCredits([]byte(exceededFixture))
	if err != nil {
		t.Fatalf("an account with windowLimits.exceeded=%q must still parse: %v", "weekly", err)
	}
	if parsed.Balance == nil || parsed.Balance.Amount != 0 {
		t.Errorf("balance = %+v, want 0", parsed.Balance)
	}
	if len(parsed.Windows) != 2 {
		t.Fatalf("windows = %+v, want both preserved", parsed.Windows)
	}
	weekly, ok := windowNamed(parsed.Windows, WindowWeekly)
	if !ok {
		t.Fatalf("weekly window missing: %+v", parsed.Windows)
	}
	// used 140 of cap 100 is over the cap; the share saturates at 100.
	if weekly.UsedPercent != 100 {
		t.Errorf("exhausted weekly used = %v, want 100", weekly.UsedPercent)
	}
	fiveHour, ok := windowNamed(parsed.Windows, WindowFiveHour)
	if !ok || fiveHour.UsedPercent != 10 {
		t.Errorf("five-hour window = %+v, want 10 used", fiveHour)
	}
}

// An unrecognised status on one window must not cost the others: a window whose
// cap is unreadable is skipped, and the rest of the payload still counts.
func TestParseCreditsSkipsUnreadableWindow(t *testing.T) {
	const body = `{
	  "credits": {"monthlyCredits": 5},
	  "windowLimits": {
	    "limited": true,
	    "fiveHour": {"used": 10, "cap": null, "exceeded": false, "resetAt": 0},
	    "weekly":   {"used": 50, "cap": 100, "exceeded": false, "resetAt": 0}
	  }
	}`
	parsed, err := parseCredits([]byte(body))
	if err != nil {
		t.Fatalf("parse credits: %v", err)
	}
	if len(parsed.Windows) != 1 || parsed.Windows[0].Name != WindowWeekly {
		t.Fatalf("windows = %+v, want only the weekly one", parsed.Windows)
	}
	if parsed.Windows[0].ResetAt != nil {
		t.Error("a resetAt of 0 must not become a reset time")
	}
}

// A payload with nothing readable is a parse failure, not an empty success.
func TestParseCreditsRejectsUnrecognisedPayload(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"something":"else"}`,
		`{"credits":{},"windowLimits":{"limited":true}}`,
	} {
		if _, err := parseCredits([]byte(body)); err == nil {
			t.Errorf("payload %q must fail", body)
		} else if KindOf(err) != KindParse {
			t.Errorf("payload %q failed with %q, want parse", body, KindOf(err))
		}
	}
}

// An account the vendor does not limit has no meaningful windows: they are
// skipped rather than reported as fully free.
func TestParseCreditsSkipsWindowsWhenUnlimited(t *testing.T) {
	const body = `{
	  "credits": {"monthlyCredits": 7},
	  "windowLimits": {
	    "limited": false,
	    "fiveHour": {"used": 0, "cap": 100, "exceeded": false},
	    "weekly":   {"used": 0, "cap": 100, "exceeded": false}
	  }
	}`
	parsed, err := parseCredits([]byte(body))
	if err != nil {
		t.Fatalf("parse credits: %v", err)
	}
	if len(parsed.Windows) != 0 {
		t.Errorf("windows = %+v, want none for an unlimited account", parsed.Windows)
	}
	if parsed.Balance == nil || parsed.Balance.Amount != 7 {
		t.Errorf("balance = %+v, want 7", parsed.Balance)
	}
}

func TestParseSubscriptionReadsPlan(t *testing.T) {
	body := `{"success":true,"data":{"planId":"individual-pro","status":"active","currentPeriodEnd":"2026-10-16T04:03:37.000Z"}}`
	plan, ok := parseSubscription([]byte(body))
	if !ok || plan != "Pro" {
		t.Errorf("plan = %q (%t), want Pro", plan, ok)
	}
}

func TestParseSubscriptionRejectsFailure(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"success":false,"data":{"planId":"individual-pro"}}`,
		`{"success":true,"data":{}}`,
		`{"success":true}`,
	} {
		if plan, ok := parseSubscription([]byte(body)); ok {
			t.Errorf("payload %q yielded plan %q, want none", body, plan)
		}
	}
}

// An unknown plan id is shown as-is rather than guessed at.
func TestCanonicalPlanFallsBackToTheID(t *testing.T) {
	if got := canonicalPlan("individual-mystery"); got != "individual-mystery" {
		t.Errorf("canonicalPlan = %q, want the raw id", got)
	}
	if got := canonicalPlan("individual_pro"); got != "Pro" {
		t.Errorf("canonicalPlan with underscores = %q, want Pro", got)
	}
}

func TestAccountAPIBaseTrimsProviderSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.commandcode.ai/provider/v1", "https://api.commandcode.ai"},
		{"https://api.commandcode.ai/provider/v1/", "https://api.commandcode.ai"},
		{"https://api.commandcode.ai/provider", "https://api.commandcode.ai"},
		{"https://api.commandcode.ai", "https://api.commandcode.ai"},
		{"", DefaultBaseURL},
	}
	for _, tc := range cases {
		got, err := AccountAPIBase(tc.in)
		if err != nil {
			t.Errorf("AccountAPIBase(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("AccountAPIBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAccountAPIBaseRefusesUnusableURL(t *testing.T) {
	for _, in := range []string{"not a url", "ftp://api.commandcode.ai", "https://"} {
		if _, err := AccountAPIBase(in); err == nil {
			t.Errorf("AccountAPIBase(%q) should fail", in)
		}
	}
}

// A credential that formats into a log line must not print the key.
func TestCredentialStringRedacts(t *testing.T) {
	credential := Credential{APIKey: "cmc-secret-canary", BaseURL: DefaultBaseURL}
	for _, rendered := range []string{credential.String(), credential.Redacted().APIKey} {
		if strings.Contains(rendered, "canary") {
			t.Errorf("rendered %q leaks the key", rendered)
		}
	}
}

func windowNamed(windows []Window, name string) (Window, bool) {
	for _, window := range windows {
		if window.Name == name {
			return window, true
		}
	}
	return Window{}, false
}
