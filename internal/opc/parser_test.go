package opc

import (
	"strings"
	"testing"
	"time"
)

// usageFixture is the response observed live on 2026-10-11: the rolling window
// is rate limited, the weekly and monthly windows are inside their limits.
const usageFixture = `{
  "usage": {
    "rolling": {"status": "rate-limited", "percent": 100, "resetsAt": "2026-10-11T18:00:00Z"},
    "weekly":  {"status": "ok", "percent": 42, "resetsAt": "2026-10-14T00:00:00Z"},
    "monthly": {"status": "ok", "percent": 21, "resetsAt": "2026-11-01T00:00:00Z"}
  }
}`

// TestParseUsageMapsWindows is the mapping fixture: every readable window
// becomes a bucket with its used share, in shortest-first order.
func TestParseUsageMapsWindows(t *testing.T) {
	windows, err := parseUsage([]byte(usageFixture))
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}
	if len(windows) != 3 {
		t.Fatalf("windows = %+v, want three", windows)
	}
	want := []struct {
		name string
		used float64
	}{
		{WindowRolling, 100},
		{WindowWeekly, 42},
		{WindowMonthly, 21},
	}
	for i, expected := range want {
		if windows[i].Name != expected.name {
			t.Errorf("windows[%d].Name = %q, want %q", i, windows[i].Name, expected.name)
		}
		if windows[i].UsedPercent != expected.used {
			t.Errorf("windows[%d] (%s) used = %v, want %v", i, expected.name, windows[i].UsedPercent, expected.used)
		}
	}
}

// A rate-limited window is fully used whatever its percent says: remaining 0.
func TestParseUsageRateLimitedIsFullyUsed(t *testing.T) {
	const body = `{"usage":{"rolling":{"status":"rate-limited","percent":3},"weekly":{"status":"ok","percent":10}}}`
	windows, err := parseUsage([]byte(body))
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}
	rolling, ok := windowNamed(windows, WindowRolling)
	if !ok {
		t.Fatalf("rolling window missing: %+v", windows)
	}
	if rolling.UsedPercent != 100 {
		t.Errorf("rate-limited used = %v, want 100 even though percent said 3", rolling.UsedPercent)
	}
	if rolling.Note == "" {
		t.Error("a rate-limited window must say so in its note")
	}
}

// An unknown status skips that window only: the rest of the payload still
// counts.
func TestParseUsageSkipsUnknownStatus(t *testing.T) {
	const body = `{"usage":{"rolling":{"status":"paused","percent":90},"weekly":{"status":"ok","percent":10}}}`
	windows, err := parseUsage([]byte(body))
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}
	if len(windows) != 1 || windows[0].Name != WindowWeekly {
		t.Fatalf("windows = %+v, want only the weekly one", windows)
	}
}

// No readable window at all is an error: upstream did not return a structure
// this package recognises.
func TestParseUsageRejectsWhenNothingReadable(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{}`,
		`{"usage":null}`,
		`{"usage":{"rolling":{"status":"paused"},"weekly":{"status":"weird"},"monthly":{}}}`,
		`{"usage":{"rolling":{"status":"ok"}}}`,
	} {
		_, err := parseUsage([]byte(body))
		if err == nil {
			t.Errorf("payload %q must fail", body)
			continue
		}
		if KindOf(err) != KindParse {
			t.Errorf("payload %q failed with %q, want parse", body, KindOf(err))
		}
	}
}

// An unparseable resetsAt is dropped rather than written into the reset time.
func TestParseUsageDropsUnparseableReset(t *testing.T) {
	const body = `{"usage":{"weekly":{"status":"ok","percent":5,"resetsAt":"in 3 hours"}}}`
	windows, err := parseUsage([]byte(body))
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].ResetAt != nil {
		t.Errorf("reset = %s, want none for an unparseable value", windows[0].ResetAt)
	}
}

// A parseable resetsAt is carried as an instant, and an out-of-range percent is
// clamped rather than producing a negative remaining share.
func TestParseUsageClampsAndParsesReset(t *testing.T) {
	const body = `{"usage":{"weekly":{"status":"ok","percent":140,"resetsAt":"2026-10-14T00:00:00Z"}}}`
	windows, err := parseUsage([]byte(body))
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}
	if windows[0].UsedPercent != 100 {
		t.Errorf("used = %v, want clamped to 100", windows[0].UsedPercent)
	}
	want := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	if windows[0].ResetAt == nil || !windows[0].ResetAt.Equal(want) {
		t.Errorf("reset = %v, want %s", windows[0].ResetAt, want)
	}
}

// UsageURL appends the usage path once, whatever the base spelling.
func TestUsageURLDoesNotDuplicatePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://opencode.ai/zen/go", "https://opencode.ai/zen/go/v1/usage"},
		{"https://opencode.ai/zen/go/", "https://opencode.ai/zen/go/v1/usage"},
		{"https://opencode.ai/zen/go/v1/usage", "https://opencode.ai/zen/go/v1/usage"},
		{"", DefaultBaseURL + UsagePath},
	}
	for _, tc := range cases {
		got, err := UsageURL(tc.in)
		if err != nil {
			t.Errorf("UsageURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("UsageURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUsageURLRefusesUnusableBase(t *testing.T) {
	for _, in := range []string{"not a url", "ftp://opencode.ai", "https://"} {
		if _, err := UsageURL(in); err == nil {
			t.Errorf("UsageURL(%q) should fail", in)
		}
	}
}

// A credential that formats into a log line must not print the key.
func TestCredentialStringRedacts(t *testing.T) {
	credential := Credential{APIKey: "opc-secret-canary", BaseURL: DefaultBaseURL}
	if strings.Contains(credential.String(), "canary") {
		t.Errorf("String() leaks the key: %s", credential.String())
	}
	if credential.Redacted().APIKey != "" {
		t.Error("Redacted() kept the key")
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
