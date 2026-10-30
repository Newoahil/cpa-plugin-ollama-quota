package quotaplugin

import (
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

// ErrQuiesceIncomplete reports a quiesce whose in-flight host calls could not be
// drained inside the bounded wait.
//
// It is a real failure and the host has to see it: reporting success here would
// tell the host it may swap or unload a library that still has a collection
// callback running inside it. The bound exists because this plugin cannot
// abandon a synchronous host call, and waiting forever would wedge hot
// replacement on exactly the broken host the bound is meant to survive.
var ErrQuiesceIncomplete = errors.New("quiesce could not drain in-flight collection calls")

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
// State deadlocks. It is read under the lock and called after it is released.
//
// Stop is RETAINED rather than consumed, so a repeated quiesce can re-run the
// bounded drain against the same in-flight calls. Only Register takes it, and
// only because a reconfigure genuinely replaces the collector.
type State struct {
	mu       sync.Mutex
	phase    Phase
	provider *Provider
	config   Config
	// collectStop is the installed collector stop. It is RETAINED, not consumed:
	// a drain that ended with work still running leaves it in place so the next
	// Quiesce/Shutdown can re-judge the same in-flight calls instead of
	// concluding "no stop function, therefore nothing running".
	collectStop func() drainOutcome
	// drainAttempts counts bounded drain judgements, so a caller (and a test)
	// can tell a real re-check apart from an early return.
	drainAttempts uint64
	// orphanDrains holds stops whose drain did not finish and which no longer
	// own the live collector: a reconfigure replaced them, or they were
	// installed after a quiesce had already begun. A callback they left
	// running is still running, so every later drain re-judges them instead
	// of reporting an empty in-flight set from the new collector alone.
	orphanDrains []func() drainOutcome
	// collectorStart starts the background collector and installs its stop
	// function. Register calls it on every transition into active; a nil hook
	// means this instance runs without a collector (tests that only exercise
	// the lifecycle).
	collectorStart func(Config)
}

// NewState returns a state ready for its first register call.
//
// It runs without a host bridge, so a quota call on it fails with
// host_unavailable. Tests that drive the lifecycle never reach the network;
// tests that do use NewStateFor.
func NewState() *State {
	return NewStateFor(nil)
}

// NewStateFor returns a state whose provider collects through hostCall.
//
// The collector is always installed, so register and quiesce drive a real
// timer. A nil hostCall leaves the timer running but every sweep fails its
// first callback, which is the honest behaviour for a plugin loaded without a
// host table.
func NewStateFor(hostCall HostCall) *State {
	provider := NewProvider(ProviderOptions{HostCall: hostCall})
	state := &State{
		phase:    PhaseNew,
		provider: provider,
		config:   Config{IntervalSeconds: defaultIntervalSeconds},
	}
	state.collectorStart = func(cfg Config) {
		state.SetCollector(provider.startCollector(cfg))
	}
	return state
}

// newStateWithoutCollector returns a state whose provider collects on demand
// but which starts no background timer.
//
// Tests that assert exact upstream request counts use it: the collector's
// immediate first sweep would race them for that count. The timer itself is
// covered by the collector tests, which are the only place that needs it.
func newStateWithoutCollector(hostCall HostCall) *State {
	return &State{
		phase:    PhaseNew,
		provider: NewProvider(ProviderOptions{HostCall: hostCall}),
		config:   Config{IntervalSeconds: defaultIntervalSeconds},
	}
}

// SetCollectorStarter installs the hook Register uses to start the background
// collector. The hook must install the stop function through SetCollector; a
// nil hook disables the collector entirely, which is what tests that assert
// exact upstream request counts need.
func (s *State) SetCollectorStarter(start func(Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collectorStart = start
}

// Register applies the config CPA passed and re-arms the instance. The host
// calls it on first load and again on hot reload; a register after quiesce is
// how a failed hot-swap rolls back, so that path stays open. A register after
// shutdown is refused because PhaseStopped is terminal.
//
// Any collector from the previous configuration is stopped and replaced, so a
// reconfigure with a new interval does not leave the old timer running.
func (s *State) Register(configYAML []byte) (Config, error) {
	cfg, errConfig := ParseConfig(configYAML)

	s.mu.Lock()
	if s.phase == PhaseStopped {
		s.mu.Unlock()
		return cfg, ErrStopped
	}
	stopPrevious := s.takeCollectorLocked()
	s.phase = PhaseActive
	s.config = cfg
	s.provider.Reset()
	start := s.collectorStart
	s.mu.Unlock()

	// The stop function is code this package owns only nominally: it cancels a
	// context and waits for a goroutine that may be mid-request. It must run
	// outside the lock, exactly like the one quiesce calls.
	//
	// If it could not finish, the callback it left behind is still running and
	// the new collector knows nothing about it. Retaining the stop keeps that
	// work visible to later drains instead of letting a fresh, empty in-flight
	// set report success.
	if stopPrevious != nil {
		if outcome := stopPrevious(); !outcome.drained {
			s.mu.Lock()
			s.retainUndrainedLocked(stopPrevious, false)
			s.mu.Unlock()
		}
	}
	if start != nil {
		// start installs its own stop function through SetCollector, which stops
		// it immediately if the instance is no longer active (a quiesce that beat
		// the register).
		start(cfg)
	}

	return cfg, errConfig
}

// Quiesce refuses new work, cancels in-flight fetches, and drains the host
// calls the collector started.
//
// A successful quiesce means every host call this plugin started has
// returned, so the library is safe to unload. That is a BOUNDED guarantee:
// the drain waits at most the attempt budget plus a small margin, because a
// host that ignores the cancel cannot be abandoned from inside the plugin.
// When the bound expires Quiesce reports ErrQuiesceIncomplete and the RPC
// layer refuses the call, which is what tells the host there is no safe
// unload condition right now. Reporting success while a callback still runs
// would be worse than a longer wait: the host would swap a live library.
//
// It returns the phase it was in before the call, plus the drain error.
//
// A repeated quiesce RE-JUDGES the drain; it does not answer from the phase.
// PhaseQuiescing only records that a quiesce has started, so an earlier
// attempt may have given up while a host call was still running. Answering
// success on a retry because "the first call already said so" is the false
// success the final review reproduced: the callback never exited, yet the
// retry reported a library that was safe to unload. The stop function is
// therefore retained rather than consumed, and every call -- first or
// repeated -- runs the bounded drain judgement again.
//
// The phase change and the provider cancellation happen under one lock. The
// drain runs outside the lock, because stop waits on host calls that may
// themselves be waiting for this lock.
func (s *State) Quiesce() (Phase, error) {
	s.mu.Lock()
	previous := s.phase
	if previous != PhaseStopped {
		s.phase = PhaseQuiescing
	}
	s.provider.CancelAll()
	// The stop function is deliberately NOT taken here: it is retained so a
	// retry can re-judge.
	stop := s.collectStop
	s.mu.Unlock()

	// Phase is NEVER the answer: neither PhaseQuiescing nor PhaseStopped says
	// anything about whether a host call is still running. A failed Shutdown
	// leaves the phase Stopped with work still in flight, so short-circuiting
	// on it here would report a library that is safe to unload while a callback
	// is still inside the host -- the cross-entry version of the false success
	// the reviews kept finding. Every call runs the bounded drain judgement.
	return previous, s.drain(stop)
}

// Shutdown is quiesce plus the terminal phase transition. It reports the same
// bounded-drain failure Quiesce does; the phase is stopped regardless, so a
// caller cannot accidentally keep using this instance.
//
// Like Quiesce, a repeated shutdown re-judges the drain rather than answering
// from the already-stopped phase. Being stopped says nothing about whether a
// host call is still running, and that is the property the host needs before
// it unloads.
func (s *State) Shutdown() error {
	s.mu.Lock()
	s.phase = PhaseStopped
	s.provider.CancelAll()
	stop := s.collectStop
	s.mu.Unlock()

	// Shutdown is terminal either way: the phase stays stopped so nothing can
	// revive this instance. The error only reports that the drain did not
	// finish, which the host needs before it unloads.
	//
	// There is no "already stopped, therefore drained" shortcut. Being stopped
	// only says new work is refused; a previous shutdown can have given up with
	// a host call still running, and that call does not disappear because the
	// phase changed.
	return s.drain(stop)
}

// takeCollectorLocked detaches the collector stop function. ONLY Register uses
// it, because a reconfigure genuinely replaces the collector and the old one
// must not be drained twice.
//
// Quiesce and Shutdown must NOT use it: consuming the stop function there is
// what made a failed drain unrecoverable. Once it was nil, a retry saw "no
// stop function" and reported success while the callback was still running.
func (s *State) takeCollectorLocked() func() drainOutcome {
	stop := s.collectStop
	s.collectStop = nil
	return stop
}

// drain performs one bounded drain judgement and counts it.
//
// It is the single place both Quiesce and Shutdown go through, so a repeat
// call cannot answer from an inference (phase, or the absence of a stop
// function) instead of from the actual in-flight set.
//
// "No stop function" is NOT proof of an empty set. A stop can have been
// replaced by a reconfigure, or installed after a quiesce had already
// begun, and either way a callback it left running outlives it. Those stops
// are kept in orphanDrains and re-judged on every call, so a drain can only
// report success when EVERY stop this instance ever installed agrees nothing
// is running.
//
// The wait stays bounded by each stop's own budget, so a host that never
// returns still results in a clear failure rather than a wedged hot
// replace.
func (s *State) drain(stop func() drainOutcome) error {
	s.mu.Lock()
	s.drainAttempts++
	orphans := append([]func() drainOutcome(nil), s.orphanDrains...)
	s.mu.Unlock()

	var failures []error
	if stop != nil {
		if outcome := stop(); !outcome.drained {
			failures = append(failures, outcome.errDrainTimeout())
		}
	}
	for _, orphan := range orphans {
		if outcome := orphan(); !outcome.drained {
			failures = append(failures, outcome.errDrainTimeout())
		}
	}
	return errors.Join(failures...)
}

// retainUndrainedLocked parks a stop whose drain did not finish, so a later
// drain re-judges it even after the stop itself has been replaced.
//
// Only a FAILED drain is retained: a stop that reported success has no
// in-flight call of its own left, and keeping it would make every future
// drain re-wait on a dead collector.
func (s *State) retainUndrainedLocked(stop func() drainOutcome, drained bool) {
	if stop == nil || drained {
		return
	}
	// Duplicate retention is harmless -- the stop is idempotent and would
	// simply be judged twice -- but comparing func values is not possible in
	// Go beyond a nil check, so callers rely on inserting each stop at most
	// once by construction (Register and SetCollector each own one).
	s.orphanDrains = append(s.orphanDrains, stop)
}

// DrainAttempts reports how many bounded drain judgements this instance has
// run. It exists so a caller can tell a real re-check apart from an early
// return; nothing in the plugin depends on its value.
func (s *State) DrainAttempts() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drainAttempts
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
// collector. Register installs the real collector through it; quiesce and
// shutdown call it before the host unloads the library.
//
// A collector installed on an instance that is not accepting work is stopped
// immediately, outside the lock, so the caller cannot deadlock by inspecting
// the plugin from inside stop.
//
// A collector already installed is replaced without being stopped: the caller
// that replaces one owns stopping the old one, which is what Register does
// before calling this (it must, because stopping under this lock would wait on
// a goroutine that may be inside the plugin).
func (s *State) SetCollector(stop func() drainOutcome) {
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
	// Installed onto an instance that is not accepting work: stop it at once.
	// If that stop cannot finish, the work it leaves behind belongs to this
	// instance and must stay visible to later drains; dropping the result here
	// is the same false success as answering from the phase.
	if outcome := stop(); !outcome.drained {
		s.mu.Lock()
		s.retainUndrainedLocked(stop, false)
		s.mu.Unlock()
	}
}
