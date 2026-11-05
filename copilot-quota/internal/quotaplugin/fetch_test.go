package quotaplugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LambdaTheory/cpa-plugin-copilot-quota/internal/copilot"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testToken = "ghu_test_secret_value"

func copilotStorage() []byte {
	return []byte(`{"type":"copilot","github_access_token":"` + testToken + `","github_refresh_token":"refresh","github_login":"octo"}`)
}

// fakeHost answers the host callbacks a quota fetch needs.
type fakeHost struct {
	body        string
	status      int
	auths       []pluginapi.HostAuthFileEntry
	authJSON    json.RawMessage
	httpCalls   int
	getAuthCall int
}

func (f *fakeHost) call(_ context.Context, method string, _ []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostHTTPOperationOpen:
		return okHostResult(map[string]string{"operation_id": "op-1"})
	case pluginabi.MethodHostHTTPCancel:
		return okHostResult(map[string]any{})
	case pluginabi.MethodHostLog:
		return okHostResult(map[string]any{})
	case pluginabi.MethodHostHTTPDo:
		f.httpCalls++
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		return okHostResult(hostHTTPResponse{
			StatusCode: status,
			Headers:    httpHeaders{"Content-Type": {"application/json"}},
			Body:       []byte(f.body),
		})
	case pluginabi.MethodHostAuthList:
		return okHostResult(hostAuthListResponse{Files: f.auths})
	case pluginabi.MethodHostAuthGet:
		f.getAuthCall++
		return okHostResult(hostAuthGetResponse{AuthIndex: "1", JSON: f.authJSON})
	}
	return nil, errors.New("unexpected host method: " + method)
}

// okHostResult wraps a result the way CPA's host callbacks do.
func okHostResult(result any) ([]byte, error) {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(raw)})
}

const fetchPayload = `{"copilot_plan":"individual","access_type_sku":"copilot_pro",
 "quota_reset_date_utc":"2026-12-01T00:00:00Z",
 "quota_snapshots":{"premium_interactions":{"entitlement":1500,"remaining":1200,"percent_remaining":80}}}`

func newTestProvider(host *fakeHost, now time.Time) *Provider {
	return NewProvider(ProviderOptions{
		HostCall: host.call,
		Timeout:  5 * time.Second,
		Now:      func() time.Time { return now },
	})
}

func TestFetchQuotaMapsReading(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	host := &fakeHost{body: fetchPayload, authJSON: copilotStorage()}
	provider := newTestProvider(host, now)

	response, err := provider.fetchQuota(context.Background(), pluginapi.QuotaFetchRequest{
		AuthIndex:   "1",
		AuthID:      "auth-1",
		Provider:    CredentialProvider,
		StorageJSON: copilotStorage(),
	}, "")
	if err != nil {
		t.Fatalf("fetchQuota: %v", err)
	}
	if response.Subscription == nil || response.Subscription.Plan != "Pro" {
		t.Fatalf("subscription = %+v, want plan Pro", response.Subscription)
	}
	if len(response.Summary) != 3 {
		t.Fatalf("summary = %d metrics, want 3", len(response.Summary))
	}
	values := map[string]float64{}
	for _, metric := range response.Summary {
		values[metric.Key] = metric.Value
		if strings.Contains(metric.Label, "stale") {
			t.Errorf("fresh reading labelled stale: %q", metric.Label)
		}
	}
	if values["remaining"] != 1200 || values["used"] != 300 || values["included"] != 1500 {
		t.Errorf("summary values = %v", values)
	}
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", response.Groups)
	}
	bucket := response.Groups[0].Buckets[0]
	if bucket.Window != copilot.WindowMonthly {
		t.Errorf("window = %q", bucket.Window)
	}
	if bucket.RemainingFraction != 0.8 {
		t.Errorf("remaining fraction = %v, want 0.8", bucket.RemainingFraction)
	}
	if bucket.ResetTime != "2026-12-01T00:00:00Z" {
		t.Errorf("reset time = %q", bucket.ResetTime)
	}
	if !strings.Contains(bucket.Description, "1200 of 1500 left") {
		t.Errorf("description = %q", bucket.Description)
	}
}

func TestFetchQuotaMarksStaleAfterFailure(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	host := &fakeHost{body: fetchPayload, authJSON: copilotStorage()}
	provider := newTestProvider(host, now)

	if _, err := provider.fetchQuota(context.Background(), pluginapi.QuotaFetchRequest{
		AuthIndex: "1", Provider: CredentialProvider, StorageJSON: copilotStorage(),
	}, ""); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	host.status = http.StatusInternalServerError
	response, err := provider.fetchQuota(context.Background(), pluginapi.QuotaFetchRequest{
		AuthIndex: "1", Provider: CredentialProvider, StorageJSON: copilotStorage(),
	}, "")
	if err != nil {
		t.Fatalf("stale fetch must be served, got %v", err)
	}
	if len(response.Summary) == 0 {
		t.Fatal("stale response lost the summary")
	}
	for _, metric := range response.Summary {
		if !strings.Contains(metric.Label, "stale") {
			t.Errorf("stale metric label = %q", metric.Label)
		}
	}
	if !strings.Contains(response.Groups[0].Buckets[0].Description, "stale") {
		t.Errorf("stale bucket description = %q", response.Groups[0].Buckets[0].Description)
	}
}

func TestFetchQuotaNeverReturnsNumbersWithoutReading(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	host := &fakeHost{body: `{}`, authJSON: copilotStorage()}
	provider := newTestProvider(host, now)

	response, err := provider.fetchQuota(context.Background(), pluginapi.QuotaFetchRequest{
		AuthIndex: "1", Provider: CredentialProvider, StorageJSON: copilotStorage(),
	}, "")
	if !errors.Is(err, copilot.ErrParse) {
		t.Fatalf("err = %v, want a parse failure", err)
	}
	if len(response.Summary) != 0 || len(response.Groups) != 0 {
		t.Errorf("a failed first read returned figures: %+v", response)
	}
}

func TestFetchQuotaRejectsForeignProvider(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	host := &fakeHost{body: fetchPayload, authJSON: copilotStorage()}
	provider := newTestProvider(host, now)

	_, err := provider.fetchQuota(context.Background(), pluginapi.QuotaFetchRequest{
		AuthIndex: "1", Provider: "ollama", StorageJSON: copilotStorage(),
	}, "")
	if !IsUnsupportedProvider(err) {
		t.Fatalf("err = %v, want unsupported provider", err)
	}
	if host.httpCalls != 0 {
		t.Error("a foreign provider must not reach the network")
	}
}

func TestFetchQuotaReadsCredentialThroughHost(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	host := &fakeHost{body: fetchPayload, authJSON: copilotStorage()}
	provider := newTestProvider(host, now)

	// StorageJSON is missing; the host.auth.get fallback has to supply it.
	if _, err := provider.fetchQuota(context.Background(), pluginapi.QuotaFetchRequest{
		AuthIndex: "1", Provider: CredentialProvider,
	}, ""); err != nil {
		t.Fatalf("fetchQuota: %v", err)
	}
	if host.getAuthCall == 0 {
		t.Error("host.auth.get was not used as the fallback")
	}
}

func TestCollectorRefreshesOnlyCopilotCredentials(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	host := &fakeHost{
		body:     fetchPayload,
		authJSON: copilotStorage(),
		auths: []pluginapi.HostAuthFileEntry{
			{AuthIndex: "1", ID: "a", Provider: "copilot"},
			{AuthIndex: "2", ID: "b", Type: "openai"},
		},
	}
	provider := newTestProvider(host, now)
	worker := &collector{provider: provider, interval: time.Minute}

	worker.sweepOnce(context.Background())

	if _, ok := provider.store.load("1"); !ok {
		t.Error("copilot credential was not collected")
	}
	if _, ok := provider.store.load("2"); ok {
		t.Error("non-copilot credential was collected")
	}
	if host.httpCalls != 1 {
		t.Errorf("http calls = %d, want 1", host.httpCalls)
	}
}

func TestCredentialFromStorage(t *testing.T) {
	credential, err := credentialFromStorage(copilotStorage(), nil)
	if err != nil {
		t.Fatalf("credentialFromStorage: %v", err)
	}
	if credential.Token != testToken {
		t.Errorf("token not read from storage")
	}

	if _, err := credentialFromStorage([]byte(`{"type":"copilot"}`), nil); !errors.Is(err, ErrIncompleteCredential) {
		t.Errorf("missing token err = %v", err)
	}
	if _, err := credentialFromStorage([]byte(`{"type":"openai","github_access_token":"x"}`), nil); !errors.Is(err, ErrUnsupportedCredential) {
		t.Errorf("wrong type err = %v", err)
	}
	if _, err := credentialFromStorage([]byte(`{"type":"copilot","github_access_token":"x","github_base_url":"https://acme.ghe.com"}`), nil); !errors.Is(err, ErrUnsupportedCredential) {
		t.Errorf("GHE credential err = %v", err)
	}
	if _, err := credentialFromStorage(nil, map[string]any{"github_access_token": testToken}); err != nil {
		t.Errorf("metadata fallback err = %v", err)
	}
	if _, err := credentialFromStorage([]byte(`{"github_base_url":"https://github.com","github_access_token":"x"}`), nil); err != nil {
		t.Errorf("github.com base URL should be accepted: %v", err)
	}
	// The field is omitempty, and a writer may store a bare host or the API
	// host. Neither is an Enterprise tenant, so neither may be refused.
	for _, baseURL := range []string{"github.com", "api.github.com", "https://api.github.com", "https://github.com/"} {
		credential := []byte(`{"github_base_url":"` + baseURL + `","github_access_token":"x"}`)
		if _, err := credentialFromStorage(credential, nil); err != nil {
			t.Errorf("base URL %q should be accepted: %v", baseURL, err)
		}
	}
	for _, baseURL := range []string{"acme.ghe.com", "https://github.com.acme.example", "https://github.com@evil.example"} {
		credential := []byte(`{"github_base_url":"` + baseURL + `","github_access_token":"x"}`)
		if _, err := credentialFromStorage(credential, nil); !errors.Is(err, ErrUnsupportedCredential) {
			t.Errorf("base URL %q should be refused, got %v", baseURL, err)
		}
	}

	for _, message := range []string{ErrIncompleteCredential.Error(), ErrUnsupportedCredential.Error()} {
		if strings.Contains(message, testToken) {
			t.Error("credential error leaked the token")
		}
	}
}

func TestDescribeAdvertisesCopilotProvider(t *testing.T) {
	provider := NewProvider(ProviderOptions{})
	display, supported, reset := provider.Describe()
	if display == "" {
		t.Error("display name is empty")
	}
	if len(supported) != 1 || supported[0] != "copilot" {
		t.Errorf("supported = %v, want [copilot]", supported)
	}
	if reset {
		t.Error("reset must not be advertised")
	}
	if provider.ID() != "copilot-quota" {
		t.Errorf("identifier = %q, want copilot-quota", provider.ID())
	}
}
