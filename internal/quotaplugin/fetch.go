package quotaplugin

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/cmc"
	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/opc"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ErrQuotaResetUnsupported reports a reset request. None of the three upstreams
// has an endpoint that resets a window, so the plugin refuses instead of
// pretending it worked.
var ErrQuotaResetUnsupported = errors.New("resetting this provider's quota is not supported")

// reading is whatever is known about one credential: the last verified
// collection, or the reason there is none.
//
// A stale snapshot is kept here rather than inside a provider package on
// purpose: the collector packages refuse to hand old numbers back as if they
// were new, and the decision to serve a stale value belongs to the layer that
// owns the cache (SPEC 4.3). Every response built from a stale entry says so —
// see quotaResponse.
type reading struct {
	// snapshot is the last verified collection; zero when none has succeeded.
	// It is provider-neutral, so the store, the staleness rules and the
	// response builder work the same for all three providers.
	snapshot quotaSnapshot
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
func (r reading) hasData() bool { return r.snapshot.hasData() }

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
func (s *store) succeeded(key string, snapshot quotaSnapshot) {
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
// The attempt as a whole — credential read included, not just the upstream HTTP
// request — is bounded by one deadline (P2-1). Both host calls it makes
// (host.auth.get and host.http.do) enforce it themselves, because a deadline set
// here cannot reach a synchronous C callback (see hostBridge.callBounded).
func (p *Provider) fetchQuota(ctx context.Context, request pluginapi.QuotaFetchRequest, callbackID string) (pluginapi.QuotaFetchResponse, error) {
	budget, cancelBudget := context.WithTimeout(ctx, p.attemptTimeout())
	defer cancelBudget()
	return p.fetchQuotaWithinBudget(budget, request, callbackID)
}

// fetchQuotaWithinBudget is fetchQuota for a caller that already holds the
// attempt budget. The collector uses it so the credential read and the
// HTTP request it feeds share ONE deadline instead of each claiming the
// full timeout (P2-1). ctx must carry that deadline; if it does not, the
// call fails rather than silently running unbounded.
//
// The provider named by the request decides which upstream is read. Every key
// this plugin does not serve is refused with UnsupportedProviderError, so the
// page never shows a figure collected for the wrong provider.
func (p *Provider) fetchQuotaWithinBudget(budget context.Context, request pluginapi.QuotaFetchRequest, callbackID string) (pluginapi.QuotaFetchResponse, error) {
	if _, ok := budget.Deadline(); !ok {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("%w: the quota fetch needs an attempt deadline", ErrHostUnavailable)
	}
	provider, supported := canonicalProviderKey(request.Provider)
	if !supported {
		// An empty Provider is Ollama: the plugin started as Ollama's quota
		// provider and a request without a key can only have come from that
		// path.
		if quotaProviderKey(request.Provider) != "" {
			return pluginapi.QuotaFetchResponse{}, UnsupportedProviderError{Provider: quotaProviderKey(request.Provider)}
		}
		provider = QuotaIdentifier
	}

	collect, key, errCredential := p.credentialFor(budget, request, provider)
	if errCredential != nil {
		return pluginapi.QuotaFetchResponse{}, errCredential
	}
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

	snapshot, errFetch := collect(budget, callbackID, now)
	if errFetch == nil {
		p.store.succeeded(key, snapshot)
		entry, _ := p.store.load(key)
		return quotaResponse(entry, ""), nil
	}

	retryAt := now
	// A 429 carries a backoff even without a Retry-After header: each provider
	// package fills in a conservative default and reports HasRetryAfter=false to
	// say so. The duration is what matters here, so it is used whichever branch
	// it came from.
	wait, _ := retryAfterOf(errFetch)
	if wait > 0 {
		retryAt = now.Add(wait)
	}
	p.store.failed(key, errFetch, retryAt)

	if entry, ok := p.store.load(key); ok && entry.hasData() {
		// A rate-limited response is reported as a wait, not only as a failure:
		// the figures below are the ones read before upstream closed the door,
		// and the page has to be able to say why they are not being refreshed.
		reason := errFetch.Error()
		if wait > 0 {
			reason = backoffReason(retryAt, now) + "; " + reason
		}
		return quotaResponse(entry, reason), nil
	}
	// Never collected successfully: report the failure, not a number.
	return pluginapi.QuotaFetchResponse{}, errFetch
}

// collectorFunc performs one collection for a resolved credential.
type collectorFunc func(ctx context.Context, callbackID string, now time.Time) (quotaSnapshot, error)

// credentialFor resolves the credential for one provider and returns the
// collector that reads it, plus the store key its reading is cached under.
func (p *Provider) credentialFor(budget context.Context, request pluginapi.QuotaFetchRequest, provider string) (collect collectorFunc, key string, err error) {
	switch provider {
	case QuotaIdentifier:
		// The page-configured API key (settings.json) is Ollama-only: it is the
		// single key the settings page manages, and neither of the other two
		// providers has a page yet.
		saved, errSettings := p.settings.load()
		credential := ollama.Credential{APIKey: saved}
		if errSettings == nil && saved == "" {
			credential, errSettings = p.resolveHostCredential(budget, request)
		}
		if errSettings != nil {
			return nil, "", errSettings
		}
		key = readingKey(request)
		if saved != "" {
			key = configuredReadingKey(credential.APIKey)
		}
		return func(ctx context.Context, callbackID string, now time.Time) (quotaSnapshot, error) {
			snapshot, errFetch := p.collect(ctx, credential, callbackID, now)
			return ollamaSnapshot(snapshot), errFetch
		}, key, nil

	case CommandCodeProvider:
		credential, errCredential := p.resolveAPICompatibleCredential(budget, request)
		if errCredential != nil {
			return nil, "", errCredential
		}
		key = providerReadingKey(provider, request, credential.apiKey)
		return func(ctx context.Context, callbackID string, now time.Time) (quotaSnapshot, error) {
			client := cmc.NewClient(
				cmc.WithTransport(&transport{bridge: p.bridge(), callbackID: callbackID}),
				cmc.WithTimeout(p.timeout),
				cmc.WithClock(func() time.Time { return now }),
				cmc.WithBaseURL(credential.baseURL),
			)
			snapshot, errFetch := client.Fetch(ctx, cmc.Credential{APIKey: credential.apiKey, BaseURL: credential.baseURL})
			return cmcSnapshot(snapshot), errFetch
		}, key, nil

	case OpenCodeProvider:
		credential, errCredential := p.resolveAPICompatibleCredential(budget, request)
		if errCredential != nil {
			return nil, "", errCredential
		}
		key = providerReadingKey(provider, request, credential.apiKey)
		return func(ctx context.Context, callbackID string, now time.Time) (quotaSnapshot, error) {
			client := opc.NewClient(
				opc.WithTransport(&transport{bridge: p.bridge(), callbackID: callbackID}),
				opc.WithTimeout(p.timeout),
				opc.WithClock(func() time.Time { return now }),
				opc.WithBaseURL(credential.baseURL),
			)
			snapshot, errFetch := client.Fetch(ctx, opc.Credential{APIKey: credential.apiKey, BaseURL: credential.baseURL})
			return opcSnapshot(snapshot), errFetch
		}, key, nil
	}
	return nil, "", UnsupportedProviderError{Provider: provider}
}

// apiCredential is one OpenAI-compatible credential as the quota path needs it.
type apiCredential struct {
	apiKey  string
	baseURL string
}

// resolveAPICompatibleCredential reads the API key and base URL for a Command
// Code or OpenCode Go request.
//
// The host normally fills Attributes, Metadata and StorageJSON before the RPC
// call (internal/pluginhost/quota_provider.go callQuotaFetch), but that is
// best-effort: an embedded host can call a quota provider directly, and a
// runtime-only credential has no file. So when the request carries nothing
// usable, the credential is read back through host.auth.get by index — the same
// path the collector uses.
func (p *Provider) resolveAPICompatibleCredential(ctx context.Context, request pluginapi.QuotaFetchRequest) (apiCredential, error) {
	apiKey, baseURL, err := apiCompatibleCredential(request.Attributes, request.Metadata, request.StorageJSON)
	if err == nil {
		return apiCredential{apiKey: apiKey, baseURL: baseURL}, nil
	}

	index := strings.TrimSpace(request.AuthIndex)
	bridge := p.bridge()
	if index == "" || bridge == nil || bridge.call == nil {
		return apiCredential{}, err
	}
	auth, errGet := bridge.getAuth(ctx, index)
	if errGet != nil {
		// Report the original problem: the missing field is what the operator
		// has to fix, and the bridge error may only say the file is unreadable.
		return apiCredential{}, err
	}
	apiKey, baseURL, errStored := apiCompatibleCredential(request.Attributes, request.Metadata, auth.JSON)
	if errStored != nil {
		return apiCredential{}, errStored
	}
	return apiCredential{apiKey: apiKey, baseURL: baseURL}, nil
}

// resolveCredential reads the API key and cookie for one Ollama request.
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
	saved, err := p.settings.load()
	if err != nil {
		return ollama.Credential{}, err
	}
	if saved != "" {
		return ollama.Credential{APIKey: saved}, nil
	}
	return p.resolveHostCredential(ctx, request)
}

func (p *Provider) resolveHostCredential(ctx context.Context, request pluginapi.QuotaFetchRequest) (ollama.Credential, error) {
	credential, errStored := credentialFromStorage(request.StorageJSON, nil)
	if errStored == nil {
		return credential, nil
	}

	index := strings.TrimSpace(request.AuthIndex)
	if index == "" {
		return credentialFromStorage(request.StorageJSON, request.Metadata)
	}
	bridge := p.bridge()
	if bridge == nil || bridge.call == nil {
		return credentialFromStorage(request.StorageJSON, request.Metadata)
	}
	auth, errGet := bridge.getAuth(ctx, index)
	if errGet != nil {
		// The stored JSON was unusable and there is no better source. Report
		// the original problem: the missing field is what the operator has to
		// fix, and the bridge error may only say the file is unreadable.
		return credentialFromStorage(request.StorageJSON, request.Metadata)
	}
	return credentialFromStorage(auth.JSON, request.Metadata)
}

func configuredReadingKey(key string) string {
	return fmt.Sprintf("configured:%x", sha256.Sum256([]byte(key)))
}

// providerReadingKey identifies a non-Ollama credential. The provider is part
// of the key so the same credential index can never serve two providers, and a
// request that arrives with neither an index nor an id is still cacheable by a
// hash of the key it used — the hash is never rendered.
func providerReadingKey(provider string, request pluginapi.QuotaFetchRequest, apiKey string) string {
	if index := readingKey(request); index != "" {
		return provider + ":" + index
	}
	return fmt.Sprintf("%s:key:%x", provider, sha256.Sum256([]byte(apiKey)))
}

// collect performs one Ollama collection through the host transport.
func (p *Provider) collect(ctx context.Context, credential ollama.Credential, callbackID string, now time.Time) (ollama.Snapshot, error) {
	client := ollama.NewClient(
		ollama.WithTransport(&transport{bridge: p.bridge(), callbackID: callbackID}),
		ollama.WithTimeout(p.timeout),
		ollama.WithClock(func() time.Time { return now }),
	)
	return client.Fetch(ctx, credential)
}

// retryAfterOf reports the backoff attached to err by whichever provider
// package produced it. Exactly one of them can recognise the error, so the
// others answer zero.
//
// A 429 that carried no Retry-After header still gets a conservative default
// from its package and reports HasRetryAfter=false, so the second pass is what
// makes "upstream said nothing" behave like a real backoff instead of an
// immediate retry.
func retryAfterOf(err error) (time.Duration, bool) {
	retryAfterOfAll := []func(error) (time.Duration, bool){
		ollama.RetryAfterOf,
		cmc.RetryAfterOf,
		opc.RetryAfterOf,
	}
	for _, lookup := range retryAfterOfAll {
		if wait, ok := lookup(err); ok {
			return wait, true
		}
	}
	for _, lookup := range retryAfterOfAll {
		if wait, _ := lookup(err); wait > 0 {
			return wait, false
		}
	}
	return 0, false
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
//   - plan     -> Subscription.Plan;
//   - balance  -> one metric keyed "balance" (currency when upstream states
//     one, a plain number when it reports credits);
//   - monthly  -> one metric keyed "monthly_remaining", a plain credit figure
//     whose label carries the reset date when it is known;
//   - windows  -> one bucket each under a group named for the provider,
//     RemainingFraction = remaining share.
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
		metric := pluginapi.QuotaMetric{
			Key:   "balance",
			Label: label,
			Value: snapshot.Balance.Amount,
		}
		if currency := strings.TrimSpace(snapshot.Balance.Currency); currency != "" {
			metric.Format = "currency"
			metric.Currency = currency
		} else {
			// Credit figures that are not money: the page must not print a
			// currency symbol upstream never stated.
			metric.Format = "number"
			metric.Unit = "credits"
		}
		response.Summary = append(response.Summary, metric)
	}

	// The monthly plan credit is a figure, not a window. Command Code states
	// the remaining amount but no used/cap pair, and the host drops every
	// bucket without a remainingFraction — so a bucket here would have to
	// invent a percentage upstream never stated. The reset date rides in the
	// label instead: QuotaMetric has no reset field, and the management page
	// renders the label verbatim.
	if monthly := snapshot.Monthly; monthly != nil {
		label := "Monthly remaining"
		if monthly.ResetAt != nil {
			label += " · resets " + monthly.ResetAt.UTC().Format("2006-01-02")
		}
		if staleReason != "" {
			label += " · stale"
		}
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key:    "monthly_remaining",
			Label:  label,
			Value:  monthly.Remaining,
			Unit:   "credits",
			Format: "number",
		})
	}

	if len(snapshot.Windows) > 0 {
		group := pluginapi.QuotaGroup{DisplayName: providerDisplayName(snapshot.provider)}
		for _, window := range snapshot.Windows {
			group.Buckets = append(group.Buckets, quotaBucket(window, entry.observedAt, staleReason))
		}
		response.Groups = append(response.Groups, group)
	}

	// ServerTimeOffsetMs stays zero: none of the three upstreams returns a
	// server clock to compare against, and an invented offset would shift every
	// reset time on the page.
	return response
}

// quotaBucket maps one quotaWindow onto a host bucket.
//
// RemainingFraction is the share of the window still available: 1 - used, not
// used. quotaWindow holds the used share and its remainingFraction method is
// the only place the direction is decided.
func quotaBucket(window quotaWindow, observedAt time.Time, staleReason string) pluginapi.QuotaBucket {
	bucket := pluginapi.QuotaBucket{
		Window:            window.Name,
		RemainingFraction: window.remainingFraction(),
	}
	if window.ResetAt != nil {
		bucket.ResetTime = window.ResetAt.UTC().Format(time.RFC3339)
	} else if window.ResetText != "" {
		// The page's own wording, kept as-is rather than parsed into a guess.
		bucket.ResetTime = window.ResetText
	}

	parts := make([]string, 0, 4)
	if label := strings.TrimSpace(window.Label); label != "" {
		parts = append(parts, label)
	}
	observed := "read at " + observedAt.UTC().Format(time.RFC3339)
	if staleReason == "" {
		parts = append(parts, observed)
	} else {
		parts = append(parts, "stale · last "+observed, staleReason)
	}
	if note := strings.TrimSpace(window.Note); note != "" {
		parts = append(parts, note)
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
