package quotaplugin

import "sync"

// Runtime owns the handler for the current ABI generation.
//
// The host dlopens the library, calls cliproxy_plugin_init, and on unload calls
// the plugin's shutdown before dlclosing. Go package state is not torn down
// with the library, so a second load of the same file finds whatever the first
// load left behind — including a State that is already stopped. A Runtime gives
// every init a fresh State instead of reusing that stopped one.
//
// The retired handler is shut down rather than dropped: a call that was already
// in flight when the host unloaded the previous generation still holds its
// reference, and must be refused rather than resurrect service.
type Runtime struct {
	mu         sync.Mutex
	generation uint64
	current    *Handler
}

// NewRuntime returns a Runtime with no active generation.
func NewRuntime() *Runtime {
	return &Runtime{}
}

// Init starts a new generation and returns its handler.
//
// The new handler starts from a fresh State in PhaseNew, which is what a
// reinstalled plugin needs: the previous generation may have been stopped, and
// that must not carry over. The previous handler, if any, is retired so its
// own late calls are rejected.
func (r *Runtime) Init() *Handler {
	r.mu.Lock()
	r.generation++
	fresh := NewHandler(NewState())
	previous := r.current
	r.current = fresh
	r.mu.Unlock()

	if previous != nil {
		// Outside the lock: shutting down a handler runs the collector stop
		// function, which is not ours and may take a while.
		previous.Shutdown()
	}
	return fresh
}

// Current returns the handler of the active generation, or nil before Init.
func (r *Runtime) Current() *Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// State exposes the active generation's State for diagnostics and tests, or nil
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

// Shutdown retires the active generation. Later calls on the same handler are
// refused until Init starts a new one.
func (r *Runtime) Shutdown() {
	r.mu.Lock()
	handler := r.current
	r.mu.Unlock()
	if handler != nil {
		handler.Shutdown()
	}
}
