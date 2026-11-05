package copilot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const paidPayload = `{
  "login": "octo",
  "copilot_plan": "individual",
  "access_type_sku": "copilot_pro_plus",
  "quota_reset_date": "2026-12-01",
  "quota_reset_date_utc": "2026-12-01T00:00:00.000Z",
  "quota_snapshots": {
    "premium_interactions": {
      "entitlement": 7000,
      "remaining": 6300,
      "percent_remaining": 90,
      "unlimited": false,
      "overage_count": 0,
      "overage_permitted": false
    },
    "chat": {"unlimited": true, "entitlement": 0, "remaining": 0, "percent_remaining": 100},
    "completions": {"unlimited": true, "entitlement": 0, "remaining": 0, "percent_remaining": 100}
  }
}`

const freePayload = `{
  "login": "octo",
  "copilot_plan": "free",
  "access_type_sku": "copilot_free",
  "limited_user_reset_date": "2026-12-01",
  "limited_user_quotas": {"chat": 400, "completions": 1600},
  "monthly_quotas": {"chat": 500, "completions": 2000}
}`

func TestParsePaidPayload(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	snapshot, err := parseUserPayload([]byte(paidPayload), now)
	if err != nil {
		t.Fatalf("parseUserPayload: %v", err)
	}
	if snapshot.Plan != "Pro+" {
		t.Errorf("plan = %q, want Pro+", snapshot.Plan)
	}
	if snapshot.Status != StatusOK {
		t.Errorf("status = %q, want ok", snapshot.Status)
	}
	if !snapshot.ObservedAt.Equal(now) {
		t.Errorf("observedAt = %v, want %v", snapshot.ObservedAt, now)
	}
	if len(snapshot.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(snapshot.Windows))
	}
	primary, ok := snapshot.Window(WindowMonthly)
	if !ok {
		t.Fatal("premium_interactions bucket missing")
	}
	if primary.UsedPercent != 10 {
		t.Errorf("used = %v, want 10", primary.UsedPercent)
	}
	if !primary.HasCounts || primary.Entitlement != 7000 || primary.Remaining != 6300 {
		t.Errorf("counts = %+v, want 6300 of 7000", primary)
	}
	if primary.ResetAt == nil || !primary.ResetAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("resetAt = %v, want 2026-12-01T00:00:00Z", primary.ResetAt)
	}
	if snapshot.Windows[0].Name != WindowMonthly {
		t.Errorf("first bucket = %q, want premium_interactions", snapshot.Windows[0].Name)
	}
	chat, _ := snapshot.Window("chat")
	if !chat.Unlimited {
		t.Error("chat should be unlimited")
	}
	if chat.RemainingPercent() != 100 {
		t.Errorf("unlimited remaining = %v, want 100", chat.RemainingPercent())
	}
}

func TestParseFreePayload(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	snapshot, err := parseUserPayload([]byte(freePayload), now)
	if err != nil {
		t.Fatalf("parseUserPayload: %v", err)
	}
	if snapshot.Plan != "Free" {
		t.Errorf("plan = %q, want Free", snapshot.Plan)
	}
	if len(snapshot.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(snapshot.Windows))
	}
	for _, window := range snapshot.Windows {
		if window.UsedPercent != 20 {
			t.Errorf("%s used = %v, want 20", window.Name, window.UsedPercent)
		}
		if !window.HasCounts {
			t.Errorf("%s should carry counts", window.Name)
		}
		if window.ResetAt == nil {
			t.Errorf("%s missing reset date", window.Name)
		}
	}
}

func TestParseUnknownBucketKeyIsStillReadable(t *testing.T) {
	// The 2026 AI Credits shape is unverified. A new bucket key with the same
	// percent_remaining contract must still be shown, under its own label,
	// rather than dropped or converted into a zero.
	body := `{"copilot_plan":"individual","quota_snapshots":{"ai_credits":{"entitlement":2000,"remaining":1500,"percent_remaining":75,"unlimited":false}}}`
	now := time.Now()
	snapshot, err := parseUserPayload([]byte(body), now)
	if err != nil {
		t.Fatalf("parseUserPayload: %v", err)
	}
	window, ok := snapshot.Window("ai_credits")
	if !ok {
		t.Fatal("ai_credits bucket missing")
	}
	if window.UsedPercent != 25 {
		t.Errorf("used = %v, want 25", window.UsedPercent)
	}
	if window.Label != "Ai credits" {
		t.Errorf("label = %q, want %q", window.Label, "Ai credits")
	}
}

func TestParseRejectsUnrecognisedShapes(t *testing.T) {
	cases := map[string]string{
		"empty object":          `{}`,
		"no quota block":        `{"login":"octo","copilot_plan":"individual"}`,
		"percent missing":       `{"quota_snapshots":{"premium_interactions":{"entitlement":1500,"remaining":1200}}}`,
		"percent out of range":  `{"quota_snapshots":{"premium_interactions":{"percent_remaining":150}}}`,
		"percent not a number":  `{"quota_snapshots":{"premium_interactions":{"percent_remaining":"90"}}}`,
		"limited without total": `{"limited_user_quotas":{"chat":400}}`,
		// A null value must not be read as 0, which would report "100% used".
		"limited value is null": `{"copilot_plan":"individual","limited_user_quotas":{"chat":null},"monthly_quotas":{"chat":500}}`,
		"not json":              `not json at all`,
		"json array":            `[{"percent_remaining":90}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot, err := parseUserPayload([]byte(body), time.Now())
			if err == nil {
				t.Fatalf("expected a parse failure, got %+v", snapshot)
			}
			if !errors.Is(err, ErrParse) {
				t.Errorf("err = %v, want a parse-class error", err)
			}
			if snapshot.HasData() {
				t.Errorf("a failed parse must carry no figures, got %+v", snapshot)
			}
			if snapshot.Status != StatusUnavailable {
				t.Errorf("status = %q, want unavailable", snapshot.Status)
			}
		})
	}
}

func TestParseExhaustedAndWarning(t *testing.T) {
	exhausted := `{"quota_snapshots":{"premium_interactions":{"entitlement":1500,"remaining":0,"percent_remaining":0}}}`
	snapshot, err := parseUserPayload([]byte(exhausted), time.Now())
	if err != nil {
		t.Fatalf("parseUserPayload: %v", err)
	}
	if snapshot.Status != StatusExhausted {
		t.Errorf("status = %q, want exhausted", snapshot.Status)
	}

	warning := `{"quota_snapshots":{"premium_interactions":{"entitlement":1500,"remaining":300,"percent_remaining":20}}}`
	snapshot, err = parseUserPayload([]byte(warning), time.Now())
	if err != nil {
		t.Fatalf("parseUserPayload: %v", err)
	}
	if snapshot.Status != StatusWarning {
		t.Errorf("status = %q, want warning", snapshot.Status)
	}
}

func TestParseDuplicateBucketPrefersSnapshot(t *testing.T) {
	body := `{
	  "quota_snapshots": {"chat": {"percent_remaining": 90}},
	  "limited_user_quotas": {"chat": 100},
	  "monthly_quotas": {"chat": 500}
	}`
	snapshot, err := parseUserPayload([]byte(body), time.Now())
	if err != nil {
		t.Fatalf("parseUserPayload: %v", err)
	}
	if len(snapshot.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(snapshot.Windows))
	}
	if snapshot.Windows[0].UsedPercent != 10 {
		t.Errorf("used = %v, want 10 from quota_snapshots", snapshot.Windows[0].UsedPercent)
	}
}

func TestPlanFor(t *testing.T) {
	cases := []struct {
		sku, plan, want string
	}{
		{"copilot_pro", "individual", "Pro"},
		{"copilot_pro_plus", "individual", "Pro+"},
		{"copilot_business", "business", "Business"},
		{"free_limited_copilot", "free", "Free"},
		{"free_educational_quota", "individual", "Edu"},
		{"unknown_sku", "business", "Business"},
		{"unknown_sku", "mystery", "mystery"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := planFor(c.sku, c.plan); got != c.want {
			t.Errorf("planFor(%q, %q) = %q, want %q", c.sku, c.plan, got, c.want)
		}
	}
}

// funcTransport serves canned responses and records the request it received.
type funcTransport struct {
	status int
	header http.Header
	body   string
	err    error
	block  bool

	lastRequest *http.Request
}

func (f *funcTransport) Do(request *http.Request) (*http.Response, error) {
	f.lastRequest = request
	if f.block {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	header := f.header
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: f.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

const token = "ghu_secret_token_value"

func TestFetchSendsFixedRequest(t *testing.T) {
	transport := &funcTransport{status: 200, body: paidPayload}
	client := NewClient(WithTransport(transport))
	snapshot, err := client.Fetch(context.Background(), Credential{Token: token})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.Plan != "Pro+" {
		t.Errorf("plan = %q", snapshot.Plan)
	}
	if transport.lastRequest.URL.String() != UserURL {
		t.Errorf("url = %q, want %q", transport.lastRequest.URL, UserURL)
	}
	if got := transport.lastRequest.Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("authorization = %q", got)
	}
	if got := transport.lastRequest.Header.Get("Accept"); got == "" {
		t.Error("Accept header missing")
	}
}

func TestFetchClassifiesFailures(t *testing.T) {
	cases := []struct {
		name      string
		transport *funcTransport
		want      error
	}{
		{"unauthorized", &funcTransport{status: 401, body: "{}"}, ErrAuth},
		{"forbidden", &funcTransport{status: 403, body: "{}"}, ErrAuth},
		{"no copilot access", &funcTransport{status: 404, body: "{}"}, ErrAuth},
		{"server error", &funcTransport{status: 500, body: "{}"}, ErrNetwork},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := NewClient(WithTransport(c.transport))
			snapshot, err := client.Fetch(context.Background(), Credential{Token: token})
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if snapshot.HasData() {
				t.Errorf("failed fetch carried data: %+v", snapshot)
			}
			if strings.Contains(err.Error(), token) {
				t.Error("error message leaked the token")
			}
		})
	}
}

func TestFetchRateLimitUsesRetryAfter(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "42")
	client := NewClient(WithTransport(&funcTransport{status: 429, header: header, body: "{}"}))
	_, err := client.Fetch(context.Background(), Credential{Token: token})
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want rate limit", err)
	}
	wait, communicated := RetryAfterOf(err)
	if !communicated || wait != 42*time.Second {
		t.Errorf("retry after = %v (communicated %v), want 42s", wait, communicated)
	}

	client = NewClient(WithTransport(&funcTransport{status: 429, body: "{}"}))
	_, err = client.Fetch(context.Background(), Credential{Token: token})
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want rate limit", err)
	}
	wait, communicated = RetryAfterOf(err)
	if communicated {
		t.Error("no Retry-After header must report communicated=false")
	}
	if wait != defaultRateLimitBackoff {
		t.Errorf("default backoff = %v, want %v", wait, defaultRateLimitBackoff)
	}
}

func TestFetchTimeout(t *testing.T) {
	client := NewClient(
		WithTransport(&funcTransport{block: true}),
		WithTimeout(20*time.Millisecond),
	)
	_, err := client.Fetch(context.Background(), Credential{Token: token})
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v, want a network error", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline sentinel", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Error("error message leaked the token")
	}
}

func TestFetchRefusesRedirect(t *testing.T) {
	header := http.Header{}
	header.Set("Location", "https://github.com/login")
	client := NewClient(WithTransport(&funcTransport{status: 302, header: header}))
	_, err := client.Fetch(context.Background(), Credential{Token: token})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want an auth error for a login redirect", err)
	}
}

func TestFetchRefusesEchoedToken(t *testing.T) {
	client := NewClient(WithTransport(&funcTransport{status: 200, body: `{"token":"` + token + `"}`}))
	snapshot, err := client.Fetch(context.Background(), Credential{Token: token})
	if !errors.Is(err, ErrParse) {
		t.Fatalf("err = %v, want a parse error", err)
	}
	if snapshot.HasData() {
		t.Error("an echoed credential must not produce a reading")
	}
}

func TestFetchMissingCredential(t *testing.T) {
	client := NewClient(WithTransport(&funcTransport{status: 200, body: paidPayload}))
	_, err := client.Fetch(context.Background(), Credential{})
	if !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("err = %v, want ErrMissingCredential", err)
	}
	if !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v, want an auth-class error", err)
	}
}

func TestCredentialStringRedacts(t *testing.T) {
	credential := Credential{Token: token}
	if strings.Contains(credential.String(), token) {
		t.Error("Credential.String leaked the token")
	}
	if strings.Contains(credential.Redacted().Token, token) {
		t.Error("Redacted leaked the token")
	}
	if credential.Empty() {
		t.Error("credential with a token should not be Empty")
	}
}
