package quotaplugin

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// collector refreshes every Ollama credential on a timer, so the quota page has
// figures to show before anyone opens it.
//
// Exactly one collector goroutine runs per plugin instance, and it is the only
// goroutine this package starts for itself. A sweep does not fan out: it talks
// to the credentials one at a time, inline. That costs a slower sweep across
// several accounts and buys the property the ticket asks for — the goroutine
// count does not move as refreshes come and go (SPEC 4.4).
//
// Reads that arrive between sweeps are served from the cached store, so opening
// the page does not start a goroutine either.
type collector struct {
	provider *Provider
	interval time.Duration
	// onSweep, when set, is called after every sweep with its counts. Tests set
	// it to observe a sweep without sleeping for the interval.
	onSweep func(refreshed, failed int)
}

// run is the collector goroutine body. It returns when ctx is cancelled, which
// is what quiesce and shutdown do (State.SetCollector holds the cancel).
//
// The recovery is the outermost frame so nothing — the host bridge, the HTTP
// layer, a parser reached through reflection — can escape into the CPA process
// and take the gateway down (SPEC 4.4).
func (c *collector) run(ctx context.Context) {
	// An outermost recovery so even a panic during teardown cannot escape into
	// the CPA process.
	defer func() {
		if recovered := recover(); recovered != nil {
			c.safeLog(ctx, "error", "the Ollama quota collector panicked and stopped")
		}
	}()

	// The first sweep runs immediately: after a register the page should not
	// wait a whole interval for its first figures.
	c.sweepOnce(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweepOnce(ctx)
		}
	}
}

// sweepOnce refreshes every Ollama credential once.
//
// A panic ends this sweep only, never the timer: listAuths and one credential's
// callbacks are both host code, and losing the background refresh for the rest
// of the process because one of them panicked is the failure SPEC 4.4 is about.
// The panic is recorded and the next tick sweeps again.
func (c *collector) sweepOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	refreshed, failed, recovered := c.sweepOnceSafely(ctx)
	if recovered {
		c.safeLog(ctx, "error", "the Ollama quota collector panicked during a sweep; the next sweep will retry")
	}
	c.report(refreshed, failed)
}

// sweepOnceSafely runs one sweep behind its own recovery and reports how far it
// got.
func (c *collector) sweepOnceSafely(ctx context.Context) (refreshed, failed int, recovered bool) {
	defer func() {
		if caught := recover(); caught != nil {
			recovered = true
		}
	}()

	bridge := c.provider.bridge()
	// The enumeration is bounded by the same per-attempt budget as a collection:
	// it is host code that can block, and it runs before any fetch, so an
	// unbounded host.auth.list would hold the sweep — and therefore quiesce —
	// forever (P2-1).
	listBudget, cancelList := context.WithTimeout(ctx, c.provider.attemptTimeout())
	entries, errList := bridge.listAuths(listBudget)
	cancelList()
	if errList != nil {
		// The bridge's own message names the callback, never a credential.
		c.safeLog(ctx, "warning", "the Ollama quota collector could not list credentials: "+errList.Error())
		return 0, 0, false
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return refreshed, failed, false
		}
		if !isOllamaCredential(entry) {
			continue
		}
		if authIndexOf(entry) == "" {
			// host.auth.list falls back to a bare directory listing before the
			// auth manager is wired, and that listing carries no id or index
			// (internal/pluginhost/auth_callbacks.go listAuthFilesFromDisk). Such
			// an entry is not addressable, so no host.auth.get can read it. It is
			// skipped rather than counted as a failure: the host will list the
			// same credentials with their indexes on the next sweep, and the
			// manual refresh path is unaffected because it arrives with an index.
			continue
		}
		if c.refreshOne(ctx, bridge, entry) {
			refreshed++
		} else {
			failed++
		}
	}
	return refreshed, failed, false
}

// refreshOne refreshes one credential, reporting whether it produced a reading.
func (c *collector) refreshOne(ctx context.Context, bridge *hostBridge, entry pluginapi.HostAuthFileEntry) (ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// The recovered value is never formatted into the message: it can be
			// anything the code that panicked chose.
			c.safeLog(ctx, "error", "the Ollama quota collector panicked while refreshing a credential")
			ok = false
		}
	}()

	index := authIndexOf(entry)
	auth, errGet := bridge.getAuth(ctx, index)
	if errGet != nil {
		// The bridge's message names the callback and, at most, the credential's
		// index — never its contents. It is the only way to tell "the host has
		// not loaded this credential yet", which is normal during start-up, from
		// "the host refuses this callback".
		c.safeLog(ctx, "warning", "the Ollama quota collector could not read a credential: "+errGet.Error())
		return false
	}

	request := pluginapi.QuotaFetchRequest{
		AuthIndex:   index,
		AuthID:      entry.ID,
		Provider:    QuotaIdentifier,
		StorageJSON: auth.JSON,
	}
	// Collection failures are already recorded in the store as that
	// credential's stale reason, so the sweep only needs the count.
	if _, errFetch := c.provider.fetchQuota(ctx, request, ""); errFetch != nil {
		return false
	}
	return true
}

// isOllamaCredential reports whether a host credential belongs to this plugin.
// A credential that declares no provider is treated as Ollama's, because the
// host only routes a plugin's own credentials to it.
func isOllamaCredential(entry pluginapi.HostAuthFileEntry) bool {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(entry.Type))
	}
	return provider == "" || provider == QuotaIdentifier
}

// authIndexOf prefers the runtime index, falling back to the credential id.
func authIndexOf(entry pluginapi.HostAuthFileEntry) string {
	if index := strings.TrimSpace(entry.AuthIndex); index != "" {
		return index
	}
	return strings.TrimSpace(entry.ID)
}

// safeLog records a line without letting the logger itself panic the collector.
func (c *collector) safeLog(ctx context.Context, level, message string) {
	bridge := c.provider.bridge()
	if bridge == nil || bridge.call == nil {
		return
	}
	defer func() { _ = recover() }()
	bridge.logNote(ctx, level, message)
}

// report publishes one sweep's counts to the test hook, if any.
func (c *collector) report(refreshed, failed int) {
	if c.onSweep == nil {
		return
	}
	c.onSweep(refreshed, failed)
}
