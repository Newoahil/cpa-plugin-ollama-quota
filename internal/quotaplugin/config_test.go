package quotaplugin

import "testing"

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

func TestParseConfigIgnoresComments(t *testing.T) {
	cfg, err := ParseConfig([]byte("# interval_seconds: 5\ninterval_seconds: 120  # two minutes\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.IntervalSeconds != 120 {
		t.Errorf("IntervalSeconds = %d, want 120 (comments must not win)", cfg.IntervalSeconds)
	}
}
