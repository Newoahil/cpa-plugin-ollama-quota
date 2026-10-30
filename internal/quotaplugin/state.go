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
// Shutdown is terminal by contract, so a re-register cannot resurrect the
// instance and start serving quota from a library the host already retired.
var ErrStopped = errors.New("plugin instance is stopped")

// State tracks one loaded plugin instance across the register / quiesce /
// shutdown cycle the host drives.
//
// Allowed transitions — nothing else is legal:
//
//	new       --register--> active
//	active    --register--> active      (reconfigure on hot reload)
//	quiescing --register--> active      (hot-reload retry / rollback)
//	active    --quiesce-->  quiescing   (idempotent: quiescing -> quiescing)
//	quiescing --quiesce-->  quiescing
//	*         --shutdown--> stopped     (terminal; quiesce runs first)
//
// Every phase change and its side effects (re-arming or cancelling the quota
// provider, stopping the collector) happen under the same lock. Splitting them
// lets a concurrent Register observe the new phase while the provider is still
// cancelled — active phase, provider refusing work — which is what the review
// reproduced. Holding the lock across the side effects costs a short critical
// section and buys a state that cannot lie about itself.
//
// Because the lock covers side effects, the collector stop function must not
// call back into State: it would deadlock. A context.CancelFunc satisfies this;
// a stop function that wants to inspect the plugin should signal its own
// goroutine instead of calling in.
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
// The phase change, the collector stop and the provider cancellation all run
// under one lock, so a concurrent Register sees either the whole pre-quiesce
// state or the whole post-quiesce state, never a mixture.
func (s *State) Quiesce() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.phase
	if previous == PhaseStopped || previous == PhaseQuiescing {
		return previous
	}
	s.phase = PhaseQuiescing
	if stop := s.collectStop; stop != nil {
		s.collectStop = nil
		stop()
	}
	s.provider.CancelAll()
	return previous
}

// Shutdown is quiesce plus the terminal phase transition.
func (s *State) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.phase == PhaseStopped {
		return
	}
	s.phase = PhaseStopped
	if stop := s.collectStop; stop != nil {
		s.collectStop = nil
		stop()
	}
	s.provider.CancelAll()
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
// A collector installed on an already-quiesced instance is stopped
// immediately: the instance is not accepting work, so a collector must not
// start collecting.
func (s *State) SetCollector(stop context.CancelFunc) {
	if stop == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == PhaseActive {
		s.collectStop = stop
		return
	}
	stop()
}
