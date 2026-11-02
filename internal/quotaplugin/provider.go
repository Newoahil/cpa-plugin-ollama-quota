package quotaplugin

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// drainTimeout bounds how long quiesce waits for tracked fetches to finish.
//
// A collection is itself bounded by the client's 20s timeout (SPEC 4.4), and
// cancellation is cooperative, so normally this only has to cover the gap
// between a cancelled fetch and its release. Five seconds is generous for that
// while staying well inside any host-side unload timeout.
//
// Be honest about what the bound buys: it is NOT a proof that nothing can still
// be running when quiesce returns. A fetch that ignores cancellation outlives
// the wait, and the caller is told so through DrainReport (see quiesce).
const drainTimeout = 5 * time.Second

// DrainReport is the outcome of one bounded drain. It exists so a timeout is an
// observable event rather than a swallowed boolean: the RPC layer turns TimedOut
// into a distinguishable error envelope and a warning, and tests assert on it.
type DrainReport struct {
	// TimedOut reports that work tracked before CancelAll had not released when
	// the bound expired. The host may still unload the library after this.
	TimedOut bool
	// InFlight is how many tracked fetches were still running when the bound
	// expired. Zero when the drain completed.
	InFlight int
	// Timeout is the bound that was applied.
	Timeout time.Duration
}

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
	// on it so a clean drain means nothing of ours is still running, not merely
	// that the cancel signals were sent (SPEC 4.1). Add only happens under mu
	// while cancels is non-nil, and CancelAll clears cancels under mu, so a wait
	// started after CancelAll cannot race a late Add.
	inflight sync.WaitGroup
	// running mirrors inflight as a number, because a WaitGroup cannot report
	// how much is left. It exists only so a timed-out drain can say how many
	// fetches outlived the bound instead of failing silently.
	running atomic.Int64
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
	p.running.Add(1)
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
		p.running.Add(-1)
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

// drain waits up to timeout for every tracked fetch to call its release, and
// reports the outcome. A completed drain means no tracked fetch is still
// running; a timed-out drain means some are, and the caller is expected to
// surface that instead of pretending the barrier held.
//
// It must only be called after CancelAll: Add is only reachable under mu while
// cancels is non-nil, and CancelAll clears cancels under mu, so no Add can
// start after a drain begins.
func (p *Provider) drain(timeout time.Duration) DrainReport {
	done := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return DrainReport{Timeout: timeout}
	case <-timer.C:
		// Read the counter rather than trusting the timer ordering: a fetch
		// that released at almost the same instant still finished.
		return DrainReport{TimedOut: true, InFlight: int(p.running.Load()), Timeout: timeout}
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
