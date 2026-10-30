package quotaplugin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ErrQuotaResetUnsupported reports a reset request. Ollama has no endpoint that
// resets a window, so the plugin refuses instead of pretending it worked.
var ErrQuotaResetUnsupported = errors.New("resetting Ollama quota is not supported")

// reading is whatever is known about one credential: the last verified
// collection, or the reason there is none.
//
// A stale snapshot is kept here rather than inside ollama.Snapshot on purpose:
// the collector package refuses to hand old numbers back as if they were new,
// and the decision to serve a stale value belongs to the layer that owns the
// cache (SPEC 4.3). Every response built from a stale entry says so — see
// quotaResponse.
type reading struct {
	// snapshot is the last verified collection; zero when none has succeeded.
	snapshot ollama.Snapshot
	// observedAt is when snapshot was collected.
	observedAt time.Time
	// lastErr is the most recent failure, nil when the last attempt succeeded.
	lastErr error
	// retryAfter is the earliest time the next attempt may run, set from a
	// 429's Retry-After. An entry can exist purely to hold this window, which
	// is why it is recorded even before anything has succeeded.
	retryAfter time.Time
}

// hasData reports whether the entry carries a verified reading.
func (r reading) hasData() bool { return r.snapshot.HasData() }

// store holds one reading per credential. It is the only mutable state the
// provider keeps, and it is keyed by credential so two accounts can never share
// a figure.
type store struct {
	mu      sync.Mutex
	entries map[string]reading
}

func newStore() *store {
	return &store{entries: make(map[string]reading)}
}

// load returns the entry for a credential, if one exists.
func (s *store) load(key string) (reading, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	return entry, ok
}

// succeeded records a verified reading and clears the failure state and any
// backoff window.
func (s *store) succeeded(key string, snapshot ollama.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = reading{snapshot: snapshot, observedAt: snapshot.ObservedAt}
}

// failed records a failed attempt and the backoff it implies. Figures already
// held are kept and the failure is attached to them, which is what makes the
// next response stale instead of empty; a credential that has never succeeded
// keeps an entry with no data, so the backoff survives while nothing is served.
func (s *store) failed(key string, err error, retryAfter time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	entry.lastErr = err
	entry.retryAfter = retryAfter
	s.entries[key] = entry
}

// backoff reports the end of an active upstream-requested backoff window.
func (s *store) backoff(key string, now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || entry.retryAfter.IsZero() || !now.Before(entry.retryAfter) {
		return time.Time{}, false
	}
	return entry.retryAfter, true
}

// forget drops a credential's entry.
func (s *store) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// fetchQuota is the whole read path for one credential: read it from the store,
// decide whether an attempt is allowed now, collect if it is, and map whatever
// is known onto the host's quota model.
//
// The returned response never contains a figure that was not actually read. A
// failure after an earlier success returns those earlier figures marked stale;
// a failure with nothing behind it returns an error and no numbers at all
// (SPEC 4.3).
//
// The attempt as a whole — credential read included, not just the Ollama HTTP
// request — is bounded by one deadline (P2-1). Both host calls it makes
// (host.auth.get and host.http.do) enforce it themselves, because a deadline set
// here cannot reach a synchronous C callback (see hostBridge.callBounded).
func (p *Provider) fetchQuota(ctx context.Context, request pluginapi.QuotaFetchRequest, callbackID string) (pluginapi.QuotaFetchResponse, error) {
	providerKey := ProviderKey(request.Provider)
	if providerKey != "" && providerKey != QuotaIdentifier {
		return pluginapi.QuotaFetchResponse{}, UnsupportedProviderError{Provider: providerKey}
	}

	budget, cancelBudget := context.WithTimeout(ctx, p.attemptTimeout())
	defer cancelBudget()

	credential, errCredential := p.resolveCredential(budget, request)
	if errCredential != nil {
		return pluginapi.QuotaFetchResponse{}, errCredential
	}

	key := readingKey(request)
	now := p.now()

	if until, waiting := p.store.backoff(key, now); waiting {
		// Upstream asked this credential to slow down. Answer from what is
		// already known rather than spending another request inside the window
		// (SPEC 4.3).
		reason := backoffReason(until, now)
		if entry, ok := p.store.load(key); ok && entry.hasData() {
			return quotaResponse(entry, reason), nil
		}
		return pluginapi.QuotaFetchResponse{}, &backoffError{until: until, now: now}
	}

	snapshot, errFetch := p.collect(budget, credential, callbackID, now)
	if errFetch == nil {
		p.store.succeeded(key, snapshot)
		entry, _ := p.store.load(key)
		return quotaResponse(entry, ""), nil
	}

	retryAt := now
	// A 429 carries a backoff even without a Retry-After header: #2 fills in a
	// conservative default and reports HasRetryAfter=false to say so. The
	// duration is what matters here, so it is used whichever branch it came
	// from (see ollama.RetryAfterOf).
	if wait, _ := ollama.RetryAfterOf(errFetch); wait > 0 {
		retryAt = now.Add(wait)
	}
	p.store.failed(key, errFetch, retryAt)

	if entry, ok := p.store.load(key); ok && entry.hasData() {
		return quotaResponse(entry, errFetch.Error()), nil
	}
	// Never collected successfully: report the failure, not a number.
	return pluginapi.QuotaFetchResponse{}, errFetch
}

// resolveCredential reads the API key and cookie for one request.
//
// The host normally puts the credential file's JSON in StorageJSON before the
// RPC call (internal/pluginhost/quota_provider.go callQuotaFetch). It is not
// guaranteed to: StorageJSON is best-effort there, an embedded host can call a
// quota provider directly, and a runtime-only credential has no file. So when
// StorageJSON yields nothing, the credential is read back through host.auth.get
// with the same index — the same path the collector uses. Reading it here rather
// than trusting one field is what keeps a credential that is perfectly usable
// from being reported incomplete.
func (p *Provider) resolveCredential(ctx context.Context, request pluginapi.QuotaFetchRequest) (ollama.Credential, error) {
	credential, errStored := credentialFromStorage(request.StorageJSON, request.Metadata)
	if errStored == nil {
		return credential, nil
	}

	index := strings.TrimSpace(request.AuthIndex)
	if index == "" {
		return ollama.Credential{}, errStored
	}
	bridge := p.bridge()
	if bridge == nil || bridge.call == nil {
		return ollama.Credential{}, errStored
	}
	auth, errGet := bridge.getAuth(ctx, index)
	if errGet != nil {
		// The stored JSON was unusable and there is no better source. Report
		// the original problem: the missing field is what the operator has to
		// fix, and the bridge error may only say the file is unreadable.
		return ollama.Credential{}, errStored
	}
	return credentialFromStorage(auth.JSON, request.Metadata)
}

// collect performs one collection through the host transport.
func (p *Provider) collect(ctx context.Context, credential ollama.Credential, callbackID string, now time.Time) (ollama.Snapshot, error) {
	client := ollama.NewClient(
		ollama.WithTransport(&transport{bridge: p.bridge(), callbackID: callbackID}),
		ollama.WithTimeout(p.timeout),
		ollama.WithClock(func() time.Time { return now }),
	)
	return client.Fetch(ctx, credential)
}

// readingKey identifies a credential. AuthIndex is the host's stable runtime
// index; AuthID is the fallback for a call that carried only the latter.
func readingKey(request pluginapi.QuotaFetchRequest) string {
	if index := strings.TrimSpace(request.AuthIndex); index != "" {
		return index
	}
	return strings.TrimSpace(request.AuthID)
}

// quotaResponse maps a reading onto the host's normalized quota model.
//
// The mapping follows SPEC 3.1:
//
//   - plan    -> Subscription.Plan;
//   - balance -> one currency metric keyed "balance";
//   - windows -> one bucket each, RemainingFraction = remaining share.
//
// staleReason is empty for a fresh reading. For a stale one it is the scrubbed
// failure message, and it is written into the metric label and the bucket
// description: QuotaFetchResponse has no staleness field, so those strings are
// the only place the page can read "these numbers are old" from. The figures are
// the ones actually read, and the descriptions carry when they were read.
func quotaResponse(entry reading, staleReason string) pluginapi.QuotaFetchResponse {
	snapshot := entry.snapshot
	response := pluginapi.QuotaFetchResponse{}

	if plan := strings.TrimSpace(snapshot.Plan); plan != "" {
		response.Subscription = &pluginapi.QuotaSubscription{Plan: plan}
	}

	if snapshot.Balance != nil {
		label := "Balance"
		if staleReason != "" {
			label = "Balance · stale"
		}
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:      "balance",
			Label:    label,
			Value:    snapshot.Balance.Amount,
			Format:   "currency",
			Currency: snapshot.Balance.Currency,
		})
	}

	if len(snapshot.Windows) > 0 {
		group := pluginapi.QuotaGroup{DisplayName: "Ollama"}
		for _, window := range snapshot.Windows {
			group.Buckets = append(group.Buckets, quotaBucket(window, entry.observedAt, staleReason))
		}
		response.Groups = append(response.Groups, group)
	}

	// ServerTimeOffsetMs stays zero: nothing Ollama returns carries a server
	// clock to compare against, and an invented offset would shift every reset
	// time on the page.
	return response
}

// quotaBucket maps one ollama.Window onto a host bucket.
//
// RemainingFraction is the share of the window still available: 1 - used, not
// used. ollama.Window holds UsedPercent (used, 0..100) and RemainingPercent()
// already converts and clamps it, so the direction is decided in exactly one
// place.
func quotaBucket(window ollama.Window, observedAt time.Time, staleReason string) pluginapi.QuotaBucket {
	bucket := pluginapi.QuotaBucket{
		Window:            window.Name,
		RemainingFraction: window.RemainingPercent() / 100,
	}
	if window.ResetAt != nil {
		bucket.ResetTime = window.ResetAt.UTC().Format(time.RFC3339)
	} else if window.ResetText != "" {
		// The page's own wording, kept as-is rather than parsed into a guess.
		bucket.ResetTime = window.ResetText
	}

	parts := make([]string, 0, 3)
	if label := strings.TrimSpace(window.Label); label != "" {
		parts = append(parts, label)
	}
	observed := "read at " + observedAt.UTC().Format(time.RFC3339)
	if staleReason == "" {
		parts = append(parts, observed)
	} else {
		parts = append(parts, "stale · last "+observed, staleReason)
	}
	bucket.Description = strings.Join(parts, " · ")
	return bucket
}

// backoffReason describes an active backoff window in words a page can show.
func backoffReason(until, now time.Time) string {
	wait := until.Sub(now)
	if wait < 0 {
		wait = 0
	}
	return "upstream asked this credential to wait " + wait.Round(time.Second).String() + " before the next attempt"
}

// backoffError reports a credential that has never been read and is inside an
// upstream-requested backoff, so there is nothing to serve at all.
type backoffError struct {
	until time.Time
	now   time.Time
}

func (e *backoffError) Error() string { return backoffReason(e.until, e.now) }
