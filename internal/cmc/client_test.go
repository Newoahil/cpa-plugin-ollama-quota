package cmc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeTransport answers one request per call from a map keyed by path, and
// records what it was asked for. It is the same seam the plugin injects, so the
// client is exercised without a network.
type fakeTransport struct {
	responses map[string]http.Response
	requests  []http.Request
	err       error
}

func (f *fakeTransport) Do(request *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, *request)
	if f.err != nil {
		return nil, f.err
	}
	response, ok := f.responses[request.URL.Path]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: request}, nil
	}
	response.Request = request
	return &response, nil
}

func jsonResponse(status int, body string) http.Response {
	return http.Response{StatusCode: status, Header: http.Header{}, Body: bodyReader(body)}
}

// TestFetchReadsCreditsAndSubscription is the happy path: both account calls
// go out with the bearer key, and the plan name comes from the subscription.
func TestFetchReadsCreditsAndSubscription(t *testing.T) {
	transport := &fakeTransport{responses: map[string]http.Response{
		CreditsPath:      jsonResponse(200, creditsFixture),
		SubscriptionPath: jsonResponse(200, `{"success":true,"data":{"planId":"individual-max"}}`),
	}}
	client := NewClient(WithTransport(transport), WithClock(func() time.Time {
		return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	}))
	snapshot, err := client.Fetch(context.Background(), Credential{APIKey: "cmc-fixture-key"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snapshot.Plan != "Max" {
		t.Errorf("plan = %q, want Max", snapshot.Plan)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != 16.5 {
		t.Errorf("balance = %+v, want 16.5 credits", snapshot.Balance)
	}
	if len(snapshot.Windows) != 2 {
		t.Errorf("windows = %+v, want two", snapshot.Windows)
	}
	if len(transport.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(transport.requests))
	}
	for _, request := range transport.requests {
		if got := request.Header.Get("Authorization"); got != "Bearer cmc-fixture-key" {
			t.Errorf("%s Authorization = %q", request.URL.Path, got)
		}
	}
	if transport.requests[0].URL.Path != CreditsPath {
		t.Errorf("first request = %s, want %s", transport.requests[0].URL.Path, CreditsPath)
	}
}

// A failed subscription call is not a failed reading: the credits figure and the
// windows still come back, only without a plan name.
func TestFetchSurvivesSubscriptionFailure(t *testing.T) {
	transport := &fakeTransport{responses: map[string]http.Response{
		CreditsPath:      jsonResponse(200, creditsFixture),
		SubscriptionPath: jsonResponse(500, `{"error":"down"}`),
	}}
	snapshot, err := NewClient(WithTransport(transport)).Fetch(context.Background(), Credential{APIKey: "k"})
	if err != nil {
		t.Fatalf("a failing subscription call must not fail the reading: %v", err)
	}
	if snapshot.Plan != "" {
		t.Errorf("plan = %q, want none", snapshot.Plan)
	}
	if snapshot.Balance == nil || len(snapshot.Windows) != 2 {
		t.Errorf("snapshot = %+v, want the credits and both windows", snapshot)
	}
}

// A failed credits call is a failed reading, and the error is classified by
// status.
func TestFetchClassifiesCreditsFailure(t *testing.T) {
	cases := []struct {
		status int
		kind   Kind
	}{
		{401, KindAuth},
		{403, KindAuth},
		{429, KindRateLimit},
		{500, KindNetwork},
	}
	for _, tc := range cases {
		transport := &fakeTransport{responses: map[string]http.Response{CreditsPath: jsonResponse(tc.status, "")}}
		_, err := NewClient(WithTransport(transport)).Fetch(context.Background(), Credential{APIKey: "k"})
		if err == nil {
			t.Errorf("status %d must fail", tc.status)
			continue
		}
		if got := KindOf(err); got != tc.kind {
			t.Errorf("status %d classified %q, want %q", tc.status, got, tc.kind)
		}
	}
}

// A 429 with no Retry-After still reports the conservative default, flagged as
// not upstream-stated so the caller can tell the two apart.
func TestFetchRateLimitDefaultBackoff(t *testing.T) {
	transport := &fakeTransport{responses: map[string]http.Response{CreditsPath: jsonResponse(429, "")}}
	_, err := NewClient(WithTransport(transport)).Fetch(context.Background(), Credential{APIKey: "k"})
	wait, stated := RetryAfterOf(err)
	if wait != defaultRateLimitBackoff || stated {
		t.Errorf("backoff = %v stated=%t, want %v false", wait, stated, defaultRateLimitBackoff)
	}
}

// A 429 that states Retry-After reports it as upstream-stated.
func TestFetchRateLimitHonoursRetryAfter(t *testing.T) {
	response := jsonResponse(429, "")
	response.Header = http.Header{"Retry-After": []string{"90"}}
	transport := &fakeTransport{responses: map[string]http.Response{CreditsPath: response}}
	_, err := NewClient(WithTransport(transport)).Fetch(context.Background(), Credential{APIKey: "k"})
	wait, stated := RetryAfterOf(err)
	if wait != 90*time.Second || !stated {
		t.Errorf("backoff = %v stated=%t, want 90s true", wait, stated)
	}
}

// A credential with no key sends nothing at all.
func TestFetchWithoutCredentialSendsNothing(t *testing.T) {
	transport := &fakeTransport{}
	_, err := NewClient(WithTransport(transport)).Fetch(context.Background(), Credential{})
	if err == nil {
		t.Fatal("a missing key must fail")
	}
	if len(transport.requests) != 0 {
		t.Errorf("requests = %d, want none", len(transport.requests))
	}
	if KindOf(err) != KindAuth {
		t.Errorf("class = %q, want auth", KindOf(err))
	}
}

// The error and the snapshot never carry the key.
func TestNoValueLeaksTheKey(t *testing.T) {
	const canary = "cmc-canary-key-9931"
	transport := &fakeTransport{responses: map[string]http.Response{
		CreditsPath:      jsonResponse(200, `{"something":"else"}`),
		SubscriptionPath: jsonResponse(500, ""),
	}}
	_, err := NewClient(WithTransport(transport)).Fetch(context.Background(), Credential{APIKey: canary})
	if err == nil {
		t.Fatal("an unrecognised payload must fail")
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("error leaks the key: %s", err)
	}
}

// bodyReader adapts a string to an io.ReadCloser.
func bodyReader(body string) io.ReadCloser { return io.NopCloser(strings.NewReader(body)) }
