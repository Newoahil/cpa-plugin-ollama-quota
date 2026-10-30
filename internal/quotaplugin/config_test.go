package quotaplugin

import (
	"strings"
	"testing"
)

func TestParseConfigDefaults(t *testing.T) {
	for _, raw := range []string{"", "   \n", "enabled: true\npriority: 1\n"} {
		cfg, err := ParseConfig([]byte(raw))
		if err != nil {
			t.Fatalf("ParseConfig(%q): %v", raw, err)
		}
		if cfg.IntervalSeconds != defaultIntervalSeconds {
			t.Errorf("ParseConfig(%q).IntervalSeconds = %d, want %d", raw, cfg.IntervalSeconds, defaultIntervalSeconds)
		}
	}
}

func TestParseConfigReadsInterval(t *testing.T) {
	cfg, err := ParseConfig([]byte("enabled: true\npriority: 1\ninterval_seconds: 60\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.IntervalSeconds != 60 {
		t.Errorf("IntervalSeconds = %d, want 60", cfg.IntervalSeconds)
	}
}

func TestParseConfigRejectsNonInteger(t *testing.T) {
	if _, err := ParseConfig([]byte("interval_seconds: often\n")); err == nil {
		t.Fatal("a non-integer interval must be reported, not silently defaulted")
	}
}

// The parse error reaches the registration response and the management UI, so
// it must name the field and the expected type without echoing the input. A
// value that looks like a cookie or API key must not come back out.
func TestParseConfigErrorDoesNotEchoInput(t *testing.T) {
	const canary = "fixture-secret-canary"
	cases := []string{
		"interval_seconds: " + canary + "\n",
		"interval_seconds: \"" + canary + "\"\n",
		"interval_seconds: " + canary + "-123\n",
		"interval_seconds: " + canary + " # looks like a comment\n",
	}
	for _, raw := range cases {
		_, err := ParseConfig([]byte(raw))
		if err == nil {
			t.Fatalf("ParseConfig(%q) should fail", raw)
		}
		message := err.Error()
		if strings.Contains(message, canary) {
			t.Errorf("parse error leaks the input value: %q", message)
		}
		if strings.Contains(message, "strconv") || strings.Contains(message, "parsing") {
			t.Errorf("parse error wraps the strconv error: %q", message)
		}
		if !strings.Contains(message, "interval_seconds") {
			t.Errorf("parse error should name the field: %q", message)
		}
		if !strings.Contains(message, "positive integer") {
			t.Errorf("parse error should name the expected type: %q", message)
		}
	}
}

func TestParseConfigRejectsZeroAndNegative(t *testing.T) {
	for _, raw := range []string{"interval_seconds: 0\n", "interval_seconds: -60\n"} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("ParseConfig(%q) should reject a non-positive interval", raw)
		}
	}
}

func TestParseConfigIgnoresComments(t *testing.T) {
	cfg, err := ParseConfig([]byte("# interval_seconds: 5\ninterval_seconds: 120  # two minutes\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.IntervalSeconds != 120 {
		t.Errorf("IntervalSeconds = %d, want 120 (comments must not win)", cfg.IntervalSeconds)
	}
}
