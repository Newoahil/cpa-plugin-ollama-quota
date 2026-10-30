package quotaplugin

import (
	"context"
	"sync"
)

// Provider is the #1 placeholder implementation of pluginapi.QuotaProvider.
//
// It returns fixed sample data so the whole path — register, list, describe,
// fetch, reset — can be exercised before #2 adds real collection. It never
// reads credentials, so it cannot leak them.
type Provider struct {
	mu      sync.Mutex
	cancels map[uint64]context.CancelFunc
	nextID  uint64
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
	p.mu.Unlock()

	release := func() {
		p.mu.Lock()
		if entry, ok := p.cancels[id]; ok {
			delete(p.cancels, id)
			entry()
		}
		p.mu.Unlock()
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

// Reset re-arms the provider after a hot reload registers it again.
func (p *Provider) Reset() {
	p.mu.Lock()
	if p.cancels == nil {
		p.cancels = make(map[uint64]context.CancelFunc)
	}
	p.mu.Unlock()
}
