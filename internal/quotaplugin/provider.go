package quotaplugin

import (
	"context"
	"sync"
	"time"
)

// drainTimeout bounds how long quiesce waits for tracked fetches to finish.
// A collection is itself bounded by the client's 20s timeout (SPEC 4.4), and
// cancellation is cooperative, so this only has to cover the gap between a
// cancelled fetch and its release; five seconds is generous for that while
// staying well inside any host-side unload timeout.
const drainTimeout = 5 * time.Second

// Provider is the #1 placeholder implementation of pluginapi.QuotaProvider.
//
// It returns fixed sample data so the whole path — register, list, describe,
// fetch, reset — can be exercised before #2 adds real collection. It never
// reads credentials, so it cannot leak them.
type Provider struct {
	mu      sync.Mutex
	cancels map[uint64]context.CancelFunc
	nextID  uint64
	// inflight counts tracked work whose release has not run yet. Quiesce waits
	// on it so "quiesce returned" means "nothing of ours is still running", not
	// merely "the cancel signals were sent" (SPEC 4.1). Add only happens under
	// mu while cancels is non-nil, and CancelAll clears cancels under mu, so a
	// wait started after CancelAll cannot race a late Add.
	inflight sync.WaitGroup
}

// NewProvider returns a provider ready to serve; register re-arms it after a
// quiesce.
func NewProvider() *Provider {
	return &Provider{cancels: make(map[uint64]context.CancelFunc)}
}

// ID returns the quota provider identifier, matching the host contract's
// QuotaProvider.Identifier method without importing pluginapi here.
func (p *Provider) ID() string {
	return QuotaIdentifier
}

// Describe is the internal form of QuotaProvider.DescribeQuota.
func (p *Provider) Describe() (displayName string, supported []string, supportsReset bool) {
	return "Ollama", []string{QuotaIdentifier}, false
}

// Sample returns the fixed quota payload used until #2 replaces it with real
// collection. Values are intentionally obvious placeholders: a window half
// used and a $10.00 balance.
func (p *Provider) Sample(providerKey string) (Sample, error) {
	if providerKey != "" && providerKey != QuotaIdentifier {
		return Sample{}, UnsupportedProviderError{Provider: providerKey}
	}
	return Sample{
		Plan:              "Free",
		TierName:          "Free",
		TierID:            "free",
		BalanceUSD:        10.00,
		Window:            "5h",
		RemainingFraction: 0.5,
		WindowNote:        "sample data until #2 lands",
	}, nil
}

// Sample is the placeholder payload; the RPC layer maps it to pluginapi types
// so this package stays independent of the SDK's wire types.
type Sample struct {
	Plan              string
	TierName          string
	TierID            string
	BalanceUSD        float64
	Window            string
	RemainingFraction float64
	WindowNote        string
}

// Track registers a cancellable context so quiesce can abort in-flight
// fetches. It reports false once CancelAll has run.
func (p *Provider) Track(ctx context.Context) (context.Context, func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	if p.cancels == nil {
		p.mu.Unlock()
		return ctx, func() {}, false
	}
	trackedCtx, cancel := context.WithCancel(ctx)
	p.nextID++
	id := p.nextID
	p.cancels[id] = cancel
	p.inflight.Add(1)
	p.mu.Unlock()

	release := func() {
		p.mu.Lock()
		entry, ok := p.cancels[id]
		if ok {
			delete(p.cancels, id)
		}
		p.mu.Unlock()
		if ok {
			entry()
		}
		p.inflight.Done()
	}
	return trackedCtx, release, true
}

// CancelAll aborts every tracked fetch and refuses later ones until Reset.
func (p *Provider) CancelAll() {
	p.mu.Lock()
	cancels := p.cancels
	p.cancels = nil
	p.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// drain waits for every tracked fetch to call its release, bounded by timeout.
// It reports whether the drain finished; on false some work is still running
// and the caller must not let the host unload the library.
//
// It must only be called after CancelAll: Add is only reachable under mu while
// cancels is non-nil, and CancelAll clears cancels under mu, so no Add can
// start after a drain begins.
func (p *Provider) drain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Reset re-arms the provider after a hot reload registers it again.
func (p *Provider) Reset() {
	p.mu.Lock()
	if p.cancels == nil {
		p.cancels = make(map[uint64]context.CancelFunc)
	}
	p.mu.Unlock()
}
