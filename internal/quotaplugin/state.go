package quotaplugin

import (
	"context"
	"errors"
	"sync"
)

// Phase is the plugin instance lifecycle state.
type Phase int

const (
	// PhaseNew is the state after the library is opened and before register.
	PhaseNew Phase = iota
	// PhaseActive accepts quota work.
	PhaseActive
	// PhaseQuiescing refuses new work while in-flight work drains.
	PhaseQuiescing
	// PhaseStopped is terminal for this library instance: nothing may move it
	// out of this phase, including a late register from the host.
	PhaseStopped
)

func (p Phase) String() string {
	switch p {
	case PhaseNew:
		return "new"
	case PhaseActive:
		return "active"
	case PhaseQuiescing:
		return "quiescing"
	case PhaseStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// ErrStopped is returned by Register on an instance that has been shut down.
// Stopped is terminal for one State: a late register arriving after shutdown
// cannot resurrect it. A genuinely new load gets a new State from Runtime.Init,
// not a second chance on this one.
var ErrStopped = errors.New("plugin instance is stopped")

// State tracks one loaded plugin instance across the register / quiesce /
// shutdown cycle the host drives.
//
// Allowed transitions — nothing else is legal:
//
//	new       --register--> active
//	active    --register--> active      (reconfigure on hot reload)
//	quiescing --register--> active      (hot-reload retry / rollback)
//	new       --quiesce-->  quiescing   (quiesced before ever registering)
//	active    --quiesce-->  quiescing
//	quiescing --quiesce-->  quiescing   (idempotent)
//	stopped   --quiesce-->  stopped     (idempotent: shutdown already cancelled)
//	new       --shutdown--> stopped
//	active    --shutdown--> stopped     (cancels directly; no quiescing stop)
//	quiescing --shutdown--> stopped
//
// PhaseStopped admits no register. A reinstall is a new ABI generation and goes
// through Runtime.Init, which builds a fresh State rather than reviving this one.
//
// The phase and the quota provider are one protected pair: every transition
// changes both under the same lock, so a concurrent Register can never observe
// "active" while the provider is still cancelled. That is the invariant the
// review broke. The collector stop function is the one exception — it is code
// we did not write, and calling it under the lock means any call back into
// State deadlocks. It is collected under the lock (so stop is invoked exactly
// once) and called after the lock is released.
type State struct {
	mu          sync.Mutex
	phase       Phase
	provider    *Provider
	config      Config
	collectStop context.CancelFunc
}

// NewState returns a state ready for its first register call.
func NewState() *State {
	return &State{
		phase:    PhaseNew,
		provider: NewProvider(),
		config:   Config{IntervalSeconds: defaultIntervalSeconds},
	}
}

// Register applies the config CPA passed and re-arms the instance. The host
// calls it on first load and again on hot reload; a register after quiesce is
// how a failed hot-swap rolls back, so that path stays open. A register after
// shutdown is refused because PhaseStopped is terminal.
func (s *State) Register(configYAML []byte) (Config, error) {
	cfg, errConfig := ParseConfig(configYAML)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == PhaseStopped {
		return s.config, ErrStopped
	}
	s.phase = PhaseActive
	s.config = cfg
	s.provider.Reset()

	return cfg, errConfig
}

// Quiesce refuses new work and stops in-flight work. It returns the phase it
// was in before the call, which tests use to prove idempotence.
//
// The phase change and the provider cancellation happen under one lock. The
// collector stop is taken under the lock and invoked after it is released.
//
// After the cancellation, Quiesce waits for tracked fetches to finish, so that
// by the time it returns nothing of ours is still executing — the host dlcloses
// the library right after a successful quiesce (SPEC 4.1). The wait is bounded
// so a fetch that ignores cancellation cannot wedge a hot swap forever.
func (s *State) Quiesce() Phase {
	s.mu.Lock()
	previous := s.phase
	if previous == PhaseStopped || previous == PhaseQuiescing {
		s.mu.Unlock()
		return previous
	}
	s.phase = PhaseQuiescing
	stop := s.takeCollectorLocked()
	provider := s.provider
	provider.CancelAll()
	s.mu.Unlock()

	if stop != nil {
		stop()
	}
	provider.drain(drainTimeout)
	return previous
}

// Shutdown is quiesce plus the terminal phase transition.
func (s *State) Shutdown() {
	s.mu.Lock()
	if s.phase == PhaseStopped {
		s.mu.Unlock()
		return
	}
	s.phase = PhaseStopped
	stop := s.takeCollectorLocked()
	provider := s.provider
	provider.CancelAll()
	s.mu.Unlock()

	if stop != nil {
		stop()
	}
	provider.drain(drainTimeout)
}

// takeCollectorLocked detaches the collector stop function so it is called
// exactly once. The caller must invoke it after releasing the lock.
func (s *State) takeCollectorLocked() context.CancelFunc {
	stop := s.collectStop
	s.collectStop = nil
	return stop
}

// Provider returns the quota provider while the instance accepts new work.
func (s *State) Provider() (*Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != PhaseActive {
		return nil, false
	}
	return s.provider, true
}

// Active reports whether the instance accepts new work.
func (s *State) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase == PhaseActive
}

// Phase reports the current lifecycle phase.
func (s *State) Phase() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// Config reports the config applied at the last successful register.
func (s *State) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// SetCollector registers the cancellation function for the background
// collector. #2 installs the real collector here; quiesce and shutdown call it
// before the host unloads the library.
//
// A collector installed on an instance that is not accepting work is stopped
// immediately, outside the lock, so the caller cannot deadlock by inspecting
// the plugin from inside stop.
func (s *State) SetCollector(stop context.CancelFunc) {
	if stop == nil {
		return
	}
	s.mu.Lock()
	if s.phase == PhaseActive {
		s.collectStop = stop
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	stop()
}
