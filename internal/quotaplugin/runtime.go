package quotaplugin

import "sync"

// Runtime owns the handlers of every ABI generation the host has initialised in
// this process and decides which generation a call belongs to.
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
// pointer cannot close over per-init state — so the plugin cannot tag a call
// with the generation that issued it. The one identity the host does provide is
// the *number and order* of its ABI calls: one cliproxy_plugin_init per loaded
// client, and exactly one cliproxy_plugin_shutdown per client (loader_unix.go
// and loader_windows.go nil the shutdown pointer after invoking it, and
// guardedPluginClient dedupes the call). Shutdowns are therefore matched to
// generations in init order: the k-th shutdown retires the k-th generation that
// init created, and a shutdown with nothing left to retire is a no-op.
//
// The host's real ordering (measured against CLIProxyAPI v8.0.47, see
// .orchestration/run_b5727398c1ca/reports/fix-3.md) is what makes that safe:
//
//   - hot replacement: plugin.quiesce(old) → init(new) → plugin.register(new).
//     The retired client is not shut down during the swap. It is shut down later
//     by UnloadPlugin/ShutdownAll, which shut down the active client first and
//     the retired ones after it, so every generation gets exactly one shutdown.
//   - unload + reinstall: shutdown(old) → … → init(new).
//
// Retiring in init order is what keeps the retired generation's late shutdown
// from stopping the live one: the call that arrives after a replacement retires
// the generation that was already replaced, not the one now serving.
type Runtime struct {
	mu         sync.Mutex
	generation uint64
	current    *Handler
	// live holds the generations init created that no shutdown has retired yet,
	// oldest first. Each one is retired by exactly one cliproxy_plugin_shutdown.
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
func (r *Runtime) Init() *Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	fresh := NewHandler(NewState())
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

// Shutdown retires the oldest generation that is still live — the one the
// host's shutdown call belongs to, since the host issues one shutdown per init.
// Retiring the oldest rather than the newest is what stops a call arriving on a
// retired function table from stopping the generation that replaced it.
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
