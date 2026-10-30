package quotaplugin

import (
	"context"
	"sync"
	"time"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
)

// Provider is the pluginapi.QuotaProvider implementation for Ollama, in the
// internal form this package uses so it can be tested without the SDK's wire
// types. The RPC layer (rpc.go) is the only adapter between the two.
//
// A Provider never holds a credential. It is handed one per call, uses it for
// the duration of one collection, and keeps only the resulting figures. That is
// what makes it safe to keep a Provider alive across a quiesce while the host
// swaps credentials underneath it.
type Provider struct {
	// timeout bounds one collection (SPEC 4.4).
	timeout time.Duration
	// now is the clock, injected by tests.
	now func() time.Time
	// store keeps the last verified reading per credential.
	store *store

	mu sync.Mutex
	// host is the callback bridge of the generation currently serving. It is
	// replaced by every register, so a re-initialised generation never keeps
	// using an older one.
	host    *hostBridge
	cancels map[uint64]context.CancelFunc
	nextID  uint64
}

// ProviderOptions configures a Provider.
type ProviderOptions struct {
	// HostCall is the host callback bridge supplied by the cgo shim. It may be
	// nil, in which case every collection fails with host_unavailable.
	HostCall HostCall
	// Timeout overrides the per-collection deadline. Zero keeps
	// ollama.DefaultTimeout (20s).
	Timeout time.Duration
	// Now overrides the clock; only tests set it.
	Now func() time.Time
}

// NewProvider returns a provider ready to serve; register re-arms it after a
// quiesce.
func NewProvider(options ProviderOptions) *Provider {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = ollama.DefaultTimeout
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Provider{
		timeout: timeout,
		now:     now,
		store:   newStore(),
		host:    newHostBridge(options.HostCall),
		cancels: make(map[uint64]context.CancelFunc),
	}
}

// ID returns the quota provider identifier, matching the host contract's
// QuotaProvider.Identifier method without importing pluginapi here.
func (p *Provider) ID() string {
	return QuotaIdentifier
}

// Describe is the internal form of QuotaProvider.DescribeQuota.
//
// SupportsReset is false because Ollama has no endpoint that resets a window;
// ResetQuota refuses rather than reporting a reset that did not happen.
func (p *Provider) Describe() (displayName string, supported []string, supportsReset bool) {
	return "Ollama", []string{QuotaIdentifier}, false
}

// Track registers a cancellable context so quiesce can abort an in-flight fetch
// and the collection it drives. It reports false once CancelAll has run.
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
//
// The cached readings survive a quiesce on purpose: they are the figures a
// downgrade has to be able to show as stale, and dropping them would make the
// page go blank for a reason unrelated to the credential.
func (p *Provider) Reset() {
	p.mu.Lock()
	if p.cancels == nil {
		p.cancels = make(map[uint64]context.CancelFunc)
	}
	p.mu.Unlock()
}

// attemptTimeout is the budget for one whole collection attempt: the credential
// read plus the Ollama HTTP request.
//
// SPEC 4.4's 20 seconds is the budget for 采集, which starts when the attempt
// starts — at the credential — not when the first byte of HTTP goes out. A host
// that blocks either half therefore cannot hold the collector (or quiesce) past
// this.
func (p *Provider) attemptTimeout() time.Duration {
	if p.timeout <= 0 {
		return ollama.DefaultTimeout
	}
	return p.timeout
}

// bridge returns the host callback bridge of the generation currently serving.
func (p *Provider) bridge() *hostBridge {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host
}

// setHostBridge installs the bridge for this generation. A nil or bridgeless
// value still replaces the previous one, so a generation loaded without a host
// cannot keep collecting through an earlier generation's bridge.
func (p *Provider) setHostBridge(call HostCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = newHostBridge(call)
}

// startCollector starts this provider's background collector and returns the
// function that stops it.
//
// The stop function does two things, and both are needed:
//
//  1. it cancels the collector's context and waits for the collector goroutine
//     to exit, so no sweep keeps running after quiesce;
//  2. it then waits on the bridge's in-flight set, which is what closes the P1-b
//     gap: a context.AfterFunc cancel callback runs on a runtime goroutine that
//     the collector goroutine does not wait for, and it can still be inside
//     host.http.cancel when the collector returns. Waiting on the in-flight set
//     drains every such call, so by the time stop returns no host call this
//     plugin started is still running and the library can be unloaded.
//
// BOTH waits are bounded, and for the same reason (P2-1): a synchronous C
// callback cannot be abandoned by this plugin. If the collector goroutine is
// parked inside host.auth.list, it will not notice the cancel until that call
// returns, and waiting for it unbounded would make quiesce hang on exactly the
// broken host this is meant to survive. So the collector wait is bounded by the
// attempt budget plus a small margin, and when it expires the collector is left
// to unwind on its own; the in-flight bound still applies underneath it.
//
// It must be called outside the state lock.
func (p *Provider) startCollector(cfg Config) context.CancelFunc {
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultIntervalSeconds * time.Second
	}
	worker := &collector{
		provider: p,
		interval: interval,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.run(ctx)
	}()

	// The bridge is read once here rather than inside stop: a register that
	// replaced it would otherwise leave the old generation's calls unbarriered.
	bridge := p.bridge()

	// One dead-man timer for the whole stop, so the two waits share a single
	// budget rather than each getting its own.
	return func() {
		cancel()

		waitBudget := p.attemptTimeout() + collectorBarrierGrace
		stopped := make(chan struct{})
		go func() {
			<-done
			if bridge != nil {
				barrier, cancelBarrier := context.WithTimeout(context.Background(), waitBudget)
				bridge.waitInflight(barrier)
				cancelBarrier()
			}
			close(stopped)
		}()

		select {
		case <-stopped:
		case <-time.After(waitBudget):
			// The collector (or a host call it made) did not return in time. The
			// goroutine above is left waiting so it can finish the drain if the
			// host ever answers; it is not detached work, it is the same drain
			// that would have run a moment later.
		}
	}
}

// collectorBarrierGrace is added to the attempt budget when waiting for
// in-flight host calls to drain.
//
// A callback that is genuinely progressing can take a little longer than the
// budget that bounded the work it is cleaning up — opening or closing an
// operation is a separate round trip — so the barrier allows that. It stays
// finite so a wedged host cannot make quiesce wait forever.
const collectorBarrierGrace = 2 * time.Second
