package quotaplugin

import (
	"context"
	"errors"
	"sync"
	"time"
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
	collectStop *drainEntry
	// drainAttempts counts bounded drain judgements, so a caller (and a test)
	// can tell a real re-check apart from an early return.
	drainAttempts uint64
	lastDrain     DrainReport
	// orphanDrains holds every collector this instance detached from the live
	// slot, or refused one, whose stop has not been confirmed drained.
	//
	// An entry is published HERE, under the lock, at the moment its collector
	// stops being live -- before its stop runs. A concurrent drain therefore
	// finds the work from the instant it belongs to this instance, instead of
	// only after the stop happened to fail; while the stop is still executing
	// the entry answers "not known yet" and the drain fails rather than
	// reporting an empty in-flight set over a callback that is already inside
	// the host.
	//
	// Entries leave the list when a drain confirms them drained, so the list
	// tracks outstanding work, not the instance's history: a reconfigure loop
	// cannot make it grow without bound.
	orphanDrains []*drainEntry
	// collectorStart starts the background collector and installs its stop
	// function. Register calls it on every transition into active; a nil hook
	// means this instance runs without a collector (tests that only exercise
	// the lifecycle).
	collectorStart func(Config)
}

// drainEntry is one collector stop this instance owes a bounded judgement for.
//
// A stop function cannot be compared in Go, so the entry is the identity a
// drain reclaims by; it is also where "is this stop executing right now?" is
// recorded, which is what keeps a concurrent drain from reporting a collector
// drained while its stop is mid-flight and its result unknown.
type drainEntry struct {
	stop func() drainOutcome

	mu sync.Mutex
	// running is true while a caller is inside stop. At most one caller runs it
	// at a time: the stop itself already waits on the host for its own budget,
	// and a second waiter would burn the same budget learning nothing new.
	running bool
}

func newDrainEntry(stop func() drainOutcome) *drainEntry {
	return &drainEntry{stop: stop}
}

// judge performs one bounded drain judgement for this entry.
//
// Two answers are possible on top of the stop's own:
//
//   - drained / not drained: the stop ran and decided. A drained answer lets
//     the caller reclaim the entry; an undrained one keeps it, and the next
//     drain runs the stop again -- a retry has to re-judge, never answer from
//     the earlier attempt.
//   - unsettled: the stop is executing right now, so its result is not known.
//     This call reports that as a failure instead of waiting (the concurrent
//     caller is already waiting, inside its own budget) and instead of
//     inferring success from work it never saw finish.
//
// A drained answer is deliberately NOT remembered: every lifecycle call has to
// look at the in-flight set itself. What keeps that from costing anything over
// time is reclamation in State.reclaim, not a cache -- a cached answer would
// turn "re-judge the drain" back into "answer from earlier state".
func (e *drainEntry) judge() drainOutcome {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return drainOutcome{unsettled: true}
	}
	e.running = true
	e.mu.Unlock()

	// A stop that panics must not leave this entry marked as running forever:
	// every later judgement would answer "unsettled" and the instance would
	// never be able to report a drained collector again. The panic itself still
	// propagates -- the drain callers are the ones that decide what to do with
	// it, and swallowing it here would be a different, silent failure.
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()
	return e.stop()
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
	previous := s.detachLiveCollectorLocked()
	// The old collector is published as pending work in the same critical
	// section that removes it from the live slot, so there is no moment in which
	// it is neither: either the lock still shows it live, or it is already
	// visible to a drain that is about to report on this instance.
	s.phase = PhaseActive
	s.config = cfg
	s.provider.settings.setDirectory(cfg.StateDir)
	s.provider.Reset()
	start := s.collectorStart
	s.mu.Unlock()

	// The stop function is code this package owns only nominally: it cancels a
	// context and waits for a goroutine that may be mid-request. It must run
	// outside the lock, exactly like the one quiesce calls.
	//
	// A failed stop is not recorded here: the entry was already published as
	// pending work when it was detached, and every later drain re-judges it
	// until it confirms. Recording only on failure is what left the window in
	// which a concurrent drain saw an empty set while the stop was still
	// running. A stop that drains is reclaimed on the spot, so a reconfigure
	// loop does not accumulate entries.
	if previous != nil {
		_ = s.drainEntries([]*drainEntry{previous})
	}
	if start != nil {
		// start installs its own stop function through SetCollector, which stops
		// it immediately if the instance is no longer active (a quiesce that beat
		// the register).
		start(cfg)
	}

	return cfg, errConfig
}

// Quiesce refuses new work, cancels fetches, and judges tracked fetches plus
// live/detached collector and callback work in one result. Every stop keeps
// its bounded attempt budget plus grace; tracked work gets a 5s release wait.
// A failure is reported through ErrQuiesceIncomplete, LastDrain and the RPC
// envelope. CPA v8.0.47 ignores the quiesce bool, so reporting failure cannot
// itself stop that host from unloading a library with unconfirmed work.
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
	live, pending := s.drainPlanLocked()
	s.mu.Unlock()

	// Phase is NEVER the answer: neither PhaseQuiescing nor PhaseStopped says
	// anything about whether a host call is still running. A failed Shutdown
	// leaves the phase Stopped with work still in flight, so short-circuiting
	// on it here would report a library that is safe to unload while a callback
	// is still inside the host -- the cross-entry version of the false success
	// the reviews kept finding. Every call runs the bounded drain judgement.
	return previous, s.drain(live, pending)
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
	live, pending := s.drainPlanLocked()
	s.mu.Unlock()

	// Shutdown is terminal either way: the phase stays stopped so nothing can
	// revive this instance. The error only reports that the drain did not
	// finish, which the host needs before it unloads.
	//
	// There is no "already stopped, therefore drained" shortcut. Being stopped
	// only says new work is refused; a previous shutdown can have given up with
	// a host call still running, and that call does not disappear because the
	// phase changed.
	return s.drain(live, pending)
}

// drainPlanLocked returns the work one lifecycle call has to judge: the live
// collector plus every detached one still pending, each exactly once.
//
// The live collector is included here rather than read separately by the
// caller, so a collector that a concurrent Register detaches between this call
// and the drain cannot be judged twice or -- worse -- missed.
func (s *State) drainPlanLocked() (live *drainEntry, pending []*drainEntry) {
	live = s.collectStop
	for _, entry := range s.orphanDrains {
		if entry != live {
			pending = append(pending, entry)
		}
	}
	return live, pending
}

// detachLiveCollectorLocked removes the live collector from the slot AND
// publishes it as pending work, in one critical section.
//
// ONLY Register uses it, because a reconfigure genuinely replaces the collector
// and the old one must not be drained twice. Quiesce and Shutdown must NOT use
// it: consuming the stop function there is what made a failed drain
// unrecoverable -- with the live slot empty, a retry saw "no stop function" and
// reported success while the callback was still running.
//
// Publication happens here rather than after the stop runs. A stop is code this
// package does not own and it takes as long as its budget; during that time its
// collector is neither in the live slot nor, under the old code, in any list a
// drain looked at. A concurrent quiesce then judged an empty instance while the
// host callback was already running. Publishing under the same lock that
// detaches closes that window: from this point the entry is visible, and while
// its stop is executing it answers "not known yet" instead of "drained".
func (s *State) detachLiveCollectorLocked() *drainEntry {
	entry := s.collectStop
	s.collectStop = nil
	if entry != nil {
		s.publishPendingLocked(entry)
	}
	return entry
}

// publishPendingLocked records an entry as work this instance still owes a
// judgement for. Re-publishing the same entry is a no-op, so a caller cannot
// make the list grow by retrying.
func (s *State) publishPendingLocked(entry *drainEntry) {
	if entry == nil {
		return
	}
	for _, existing := range s.orphanDrains {
		if existing == entry {
			return
		}
	}
	s.orphanDrains = append(s.orphanDrains, entry)
}

// drain performs one bounded drain judgement and counts it. It is the single
// place both Quiesce and Shutdown go through, so a repeat call cannot answer
// from an inference (phase, or the absence of a stop function) instead of from
// the actual in-flight set.
//
// "No stop function" is NOT proof of an empty set. A stop can have been
// replaced by a reconfigure, or refused by an instance that was no longer
// active, and either way a callback it left running outlives it. Those entries
// stay in orphanDrains and are re-judged on every call, so a drain can only
// report success when EVERY stop this instance ever installed agrees nothing
// is running.
//
// The serial upper bound is the sum of each stop's budget, plus 5s for tracked
// fetches, plus an independent 5s for the current bridge. With one default
// collector this is 22s + 5s + 5s; detached collectors add their own budgets.
// A host that never returns therefore reports failure within a finite wait.
func (s *State) drain(live *drainEntry, pending []*drainEntry) error {
	s.mu.Lock()
	s.drainAttempts++
	s.mu.Unlock()

	// Keep one judgement for every live/detached stop, including unsettled
	// stops. Cancellation alone is never evidence that either work set exited.
	errEntries := s.drainEntries(append([]*drainEntry{live}, pending...))
	report := s.provider.drain(drainTimeout)
	var entryReport DrainReport
	if errors.As(errEntries, &entryReport) {
		report.HostPending = entryReport.HostPending
		report.Unsettled = entryReport.Unsettled
	}
	// On-demand fetches can leave host callbacks running after release even
	// without a collector. Old bridges remain owned by their drain entries.
	bridgeDeadline := time.Now().Add(drainTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), bridgeDeadline)
	outcome := s.provider.bridge().waitInflight(ctx)
	cancel()
	var errBridge error
	if !outcome.drained {
		errBridge = outcome.errDrainTimeout()
		// HostPending is max(sum of stop obligations, current bridge pending),
		// not a distinct-callback total: the live stop may count this bridge too.
		if outcome.pending > report.HostPending {
			report.HostPending = outcome.pending
		}
	}
	report.TimedOut = report.TimedOut || errEntries != nil || errBridge != nil
	s.mu.Lock()
	s.lastDrain = report
	s.mu.Unlock()
	if !report.TimedOut {
		return nil
	}
	return errors.Join(report, errEntries, errBridge)
}

// drainEntries judges each entry once, reclaims the ones it confirmed drained,
// and reports the rest as a bounded-drain failure.
//
// Reclamation is by identity against the live list rather than by overwriting
// it with the snapshot: an entry a concurrent Register appended while this
// drain was running is not in the snapshot and must survive, and an entry that
// reported "not drained" must stay so the next call re-judges it.
func (s *State) drainEntries(entries []*drainEntry) error {
	var failures []error
	var report DrainReport
	var confirmed []*drainEntry
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		outcome := entry.judge()
		if outcome.drained {
			confirmed = append(confirmed, entry)
			continue
		}
		failures = append(failures, outcome.errDrainTimeout())
		report.HostPending += outcome.pending
		report.Unsettled = report.Unsettled || outcome.unsettled
	}
	if len(confirmed) > 0 {
		s.reclaim(confirmed...)
	}
	if len(failures) == 0 {
		return nil
	}
	report.TimedOut = true
	return errors.Join(append([]error{report}, failures...)...)
}

// LastDrain exposes the same combined judgement returned by Quiesce/Shutdown.
func (s *State) LastDrain() DrainReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDrain
}

// reclaim removes entries a drain confirmed drained from the pending list.
//
// Only the confirmed entries leave; anything else in the list -- work that
// failed this drain, or an entry another goroutine published meanwhile -- is
// left untouched. This is what keeps the list proportional to outstanding work
// instead of to how many times the instance was reconfigured.
func (s *State) reclaim(entries ...*drainEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.orphanDrains) == 0 {
		return
	}
	keep := s.orphanDrains[:0]
	for _, entry := range s.orphanDrains {
		if !drainEntryIn(entries, entry) {
			keep = append(keep, entry)
		}
	}
	// Drop the tail's references: the slice is reused by append, and a dead
	// stop's closure (its bridge, done/cancel channels, timer) would otherwise
	// stay reachable from the backing array after it left the list.
	for i := len(keep); i < len(s.orphanDrains); i++ {
		s.orphanDrains[i] = nil
	}
	s.orphanDrains = keep
}

// drainEntryIn reports whether entry is one of the given set.
func drainEntryIn(set []*drainEntry, entry *drainEntry) bool {
	for _, candidate := range set {
		if candidate == entry {
			return true
		}
	}
	return false
}

// DrainAttempts reports how many bounded drain judgements this instance has
// run. It exists so a caller can tell a real re-check apart from an early
// return; nothing in the plugin depends on its value.
func (s *State) DrainAttempts() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drainAttempts
}

// pendingCount reports how many detached collectors this instance still owes a
// drain judgement for. A test uses it to prove reclamation happens; nothing in
// the plugin depends on its value.
func (s *State) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.orphanDrains)
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
// a goroutine that may be inside the plugin). The replaced collector is still
// published as pending work, so a drain sees the collector that is no longer
// live instead of losing it.
func (s *State) SetCollector(stop func() drainOutcome) {
	if stop == nil {
		return
	}
	entry := newDrainEntry(stop)
	s.mu.Lock()
	if s.phase == PhaseActive {
		if replaced := s.collectStop; replaced != nil && replaced != entry {
			s.publishPendingLocked(replaced)
		}
		s.collectStop = entry
		s.mu.Unlock()
		return
	}
	// Refused: the instance is not accepting work, so this collector must be
	// stopped at once. Publishing it here, before the stop runs, is the second
	// half of the same fix as Register's: from the moment it is refused it is
	// this instance's outstanding work, and a concurrent drain must not read an
	// empty set while its stop is still waiting inside the host.
	s.publishPendingLocked(entry)
	s.mu.Unlock()

	// The stop runs outside the lock: it is host code that may inspect this
	// very instance.
	s.drainEntries([]*drainEntry{entry})
}
