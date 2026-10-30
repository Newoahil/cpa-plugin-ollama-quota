package quotaplugin

import (
	"context"
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
	// PhaseStopped is terminal for this library instance.
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

// State tracks one loaded plugin instance across the register / quiesce /
// shutdown cycle the host drives.
//
// register → active; quiesce → quiescing (new work refused, in-flight work
// cancelled); shutdown → stopped. The collector added in #2 registers its
// cancellation through collectStop so quiesce can stop it before the host
// unloads the library.
type State struct {
	mu          sync.RWMutex
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
// calls it on first load and again on hot reload, so a previous quiesce must
// not leak into the new incarnation.
func (s *State) Register(configYAML []byte) (Config, error) {
	cfg, errConfig := ParseConfig(configYAML)

	s.mu.Lock()
	s.phase = PhaseActive
	s.config = cfg
	s.mu.Unlock()

	s.provider.Reset()
	return cfg, errConfig
}

// Quiesce refuses new work and stops in-flight work. Returns the phase it was
// in before the call, which tests use to prove idempotence.
func (s *State) Quiesce() Phase {
	s.mu.Lock()
	previous := s.phase
	if previous == PhaseStopped || previous == PhaseQuiescing {
		s.mu.Unlock()
		return previous
	}
	s.phase = PhaseQuiescing
	stop := s.collectStop
	s.collectStop = nil
	s.mu.Unlock()

	if stop != nil {
		stop()
	}
	s.provider.CancelAll()
	return previous
}

// Shutdown is quiesce plus the terminal phase transition.
func (s *State) Shutdown() {
	s.Quiesce()
	s.mu.Lock()
	s.phase = PhaseStopped
	s.mu.Unlock()
}

// Provider returns the quota provider while the instance accepts new work.
func (s *State) Provider() (*Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.phase != PhaseActive {
		return nil, false
	}
	return s.provider, true
}

// Active reports whether the instance accepts new work.
func (s *State) Active() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phase == PhaseActive
}

// Phase reports the current lifecycle phase.
func (s *State) Phase() Phase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phase
}

// Config reports the config applied at the last register.
func (s *State) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// SetCollector registers the cancellation function for the background
// collector. It is never nil-safe by design: #2 replaces the placeholder
// collector, not this entry point.
func (s *State) SetCollector(stop context.CancelFunc) {
	s.mu.Lock()
	s.collectStop = stop
	s.mu.Unlock()
}
