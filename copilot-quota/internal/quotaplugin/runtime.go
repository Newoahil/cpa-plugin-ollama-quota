package quotaplugin

import "sync"

// Runtime owns the handlers of every ABI generation the host has initialised in
// this process. A call is served by the newest generation; a shutdown retires
// the oldest live one.
//
// Generations exist because Go package state survives dlclose: the host dlopens
// the library, calls cliproxy_plugin_init, and on unload calls the plugin's
// shutdown before dlclosing, so a second load of the same file finds whatever
// the first load left behind — including a State that is already stopped. Every
// init therefore builds a fresh State instead of reusing that stopped one.
//
// Why a queue and not just the newest handler: cliproxy_plugin_call and
// cliproxy_plugin_shutdown carry no pointer to the generation they belong to.
// The ABI hands cliproxy_plugin_init a host pointer, but neither of the other
// entry points receives it, there is no epoch on the call path, and a C function
// pointer cannot close over per-init state — so the plugin cannot tag a call, or
// a shutdown, with the generation that issued it. There is no instance identity
// to recover here.
//
// What the host does provide is a count: one cliproxy_plugin_init per loaded
// client, and exactly one cliproxy_plugin_shutdown per successfully loaded
// client (loader_unix.go and loader_windows.go nil the shutdown pointer after
// invoking it, and guardedPluginClient dedupes the call). Retiring the oldest
// live generation on every shutdown keeps one live handler per unspent
// shutdown, which is what makes the queue work.
//
// FIFO is a strategy that holds under the host lifecycle measured against
// CLIProxyAPI v8.0.47 — not a binding of each shutdown call to the generation
// that issued it. The host does not shut generations down in init order: on
// unload it closes the active client first and the retired ones after it, and a
// failed replacement closes the candidate first. That mismatched order is fine
// because of what each path actually needs (reports/review-1d.md in the
// orchestration run):
//
//   - successful hot replacement shuts nothing down. The host quiesces the old
//     client, inits and registers the new one, and only moves the old client
//     record to the retired list (host.go:345-356). The plugin sees init(new)
//     with no shutdown in between, so the generation now serving can never be
//     retired by the swap itself.
//   - full unload tears every generation down anyway. UnloadPlugin
//     (host.go:648-708) and ShutdownAll (host.go:720-800) shut the active
//     client down first and the retired ones after it; order does not matter
//     once all of them must die, and FIFO drains the queue either way.
//   - a failed replacement cleans up the candidate before it rolls back
//     (host.go:557-600). That candidate's shutdown retires the oldest
//     generation, the newest handler is still alive, and the old table's
//     reconfigure reaches it through Current — so the rollback still serves.
//   - unload + reinstall: shutdown(old) → … → init(new). The old generation is
//     retired while it is the only one live; the queue is empty again before
//     init runs, so a shutdown that finds nothing to retire is a no-op. That
//     no-op is not a general licence for duplicate shutdowns across a new init:
//     it only means this queue has no unspent generation left.
//
// What FIFO guarantees, then, is these three properties: a successful
// replacement never stops the new instance, a complete unload releases every
// generation, and a failed rollback leaves the newest generation serving.
//
// This depends on the host's lifecycle contract: exactly one shutdown per
// successfully loaded client, and the close orders above. If a future host
// changes either — closing a client twice or not at all, closing an old client
// while it is still in use, or closing clients in an order that assumes each
// shutdown is paired with a specific generation — this scheme must be
// re-reviewed before it is relied on. The same applies if the #2 collector
// needs to be released strictly per real instance rather than per generation.
type Runtime struct {
	mu         sync.Mutex
	generation uint64
	current    *Handler
	// live holds the generations init created that no shutdown has retired yet,
	// oldest first. Every cliproxy_plugin_shutdown spends one entry; which
	// generation it belonged to in the host is not knowable here.
	live []*Handler
}

// NewRuntime returns a Runtime with no active generation.
func NewRuntime() *Runtime {
	return &Runtime{}
}

// Init starts a new generation and returns its handler.
//
// The new handler starts from a fresh State in PhaseNew, which is what a
// reinstalled plugin needs: the previous generation may have been stopped, and
// that must not carry over. The previous generation is deliberately not shut
// down here. The host quiesces it before replacing it, and its own shutdown
// call still has to arrive; stopping it now would retire a generation the host
// is still holding, and a failed replacement that rolls back to it would find a
// stopped instance.
//
// hostCall is this generation's host callback bridge (see Handler.hostCall). It
// is per generation because a generation belongs to one loaded host client, and
// a retired generation must not keep issuing callbacks through a live one.
func (r *Runtime) Init(hostCall HostCall) *Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	fresh := NewHandlerWithHost(NewStateFor(hostCall), hostCall)
	r.current = fresh
	r.live = append(r.live, fresh)
	return fresh
}

// Current returns the handler of the newest generation, or nil before Init.
//
// That handler is live for as long as any generation is: shutdowns retire from
// the oldest end, so the newest one is stopped last. Once every generation has
// been retired, Current still returns the last one — stopped — so a late call is
// refused rather than served by a generation the host has unloaded.
func (r *Runtime) Current() *Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// State exposes the newest generation's State for diagnostics and tests, or nil
// before Init. Callers must not depend on a particular generation surviving.
func (r *Runtime) State() *State {
	if handler := r.Current(); handler != nil {
		return handler.state
	}
	return nil
}

// Generation reports how many Init calls this Runtime has served. It is a
// diagnostic counter; callers do not need it to use the plugin.
func (r *Runtime) Generation() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

// Shutdown retires the oldest generation that is still live. The ABI cannot say
// which generation a shutdown belongs to, so the queue spends one live
// generation per shutdown it receives (see the package comment). Retiring the
// oldest rather than the newest is what stops a call arriving on a retired
// function table from stopping the generation that replaced it.
//
// It is a no-op once every generation has been retired, so repeated calls do
// not change an already-stopped instance.
func (r *Runtime) Shutdown() {
	r.mu.Lock()
	if len(r.live) == 0 {
		r.mu.Unlock()
		return
	}
	oldest := r.live[0]
	r.live = r.live[1:]
	r.mu.Unlock()

	// Outside the lock: shutting down a handler runs the collector stop
	// function, which is not ours and may take a while.
	oldest.Shutdown()
}

// LiveGenerations reports how many generations still await a shutdown call. It
// exists for tests that pin the init/shutdown bookkeeping.
func (r *Runtime) LiveGenerations() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}
