package quotaplugin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Fix round 5 pins the two defects the third final review found in the orphan
// bookkeeping this developer added, and nothing else.
//
//  1. P1: a collector detached from the live slot was only recorded as pending
//     work AFTER its stop reported a failure. While the stop was still running
//     (and its callback already inside the host) a concurrent Quiesce saw an
//     empty instance and answered success. The collector now becomes visible
//     under the same lock that detaches it, and answers "unsettled" while its
//     stop is in flight.
//
//  2. P2: a pending entry was never reclaimed, so 128 transient failures left
//     128 dead stops behind and every later drain re-ran all of them. Entries a
//     drain confirms are now removed by identity, keeping undrained and
//     concurrently added ones.

// heldCollector starts a real collector on its own provider and bridge, with a
// host.auth.list callback that parks until free() is called. Its own bridge is
// what makes the test honest: a replacement collector sharing one bridge could
// answer for the old collector's work and hide the defect.
//
// It returns the real stop function, the release function, a channel closed once
// the host callback has returned, and the bridge.
func heldCollector(t *testing.T, budget time.Duration) (stop func() drainOutcome, free func(), exited <-chan struct{}, bridge *hostBridge) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	exitedCh := make(chan struct{})
	var releaseOnce sync.Once
	var enterOnce sync.Once

	provider := NewProvider(ProviderOptions{
		Timeout: budget,
		HostCall: func(_ context.Context, method string, _ []byte) ([]byte, error) {
			if method == pluginabi.MethodHostAuthList {
				enterOnce.Do(func() { close(entered) })
				<-release
				close(exitedCh)
			}
			return okResult(map[string]any{"files": []any{}})
		},
	})

	stop = provider.startCollector(Config{IntervalSeconds: 3600})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector never reached host.auth.list")
	}
	free = func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		free()
		<-exitedCh
		_ = stop()
	})
	return stop, free, exitedCh, provider.bridge()
}

// emptyCollector installs a collector that is provably idle, so a drain that
// consults only the live collector would answer success.
func emptyCollector(state *State) {
	provider := NewProvider(ProviderOptions{Timeout: 20 * time.Millisecond})
	state.SetCollector(provider.startCollector(Config{IntervalSeconds: 3600}))
}

// TestDetachedCollectorVisibleWhileStopRuns is the P1 regression with the real
// collector stop the reviewer used, on both detach paths.
//
// The old stop is observed from inside its own bounded wait (the bridge's drain
// counter), which is the exact interleaving that produced six false successes:
// the callback is inside the host, the live slot is empty, and the pending list
// used to be empty too.
func TestDetachedCollectorVisibleWhileStopRuns(t *testing.T) {
	for _, path := range []string{"Register", "SetCollectorInactive"} {
		t.Run(path, func(t *testing.T) {
			state := newStateWithoutCollector(nil)
			stop, free, exited, oldBridge := heldCollector(t, 20*time.Millisecond)

			done := make(chan struct{})
			if path == "Register" {
				state.Register(nil)
				state.SetCollector(stop)
				state.SetCollectorStarter(func(Config) { emptyCollector(state) })
				go func() {
					defer close(done)
					state.Register(nil)
				}()
			} else {
				state.Quiesce()
				go func() {
					defer close(done)
					state.SetCollector(stop)
				}()
			}

			// Wait until the old stop is provably inside its own bounded in-flight
			// wait; the parked host callback is still running throughout.
			deadline := time.Now().Add(10 * time.Second)
			for oldBridge.DrainChecks() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the detached stop never examined its in-flight set")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case <-done:
				t.Fatal("the detach returned before its stop finished")
			default:
			}
			if pending := oldBridge.inflight.pending(); pending != 1 {
				t.Fatalf("old in-flight set = %d while its callback is parked, want 1", pending)
			}

			// The lifecycle call must see that work, not an empty instance.
			if _, err := state.Quiesce(); err == nil {
				t.Fatal("quiesce succeeded while the detached collector's callback was still inside the host")
			}

			// Release and observe the callback actually leave, then the same
			// instance converges -- release alone is not proof.
			free()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Fatal("the parked host callback never returned")
			}
			<-done

			converge := time.Now().Add(20 * time.Second)
			for {
				if _, err := state.Quiesce(); err == nil {
					return
				}
				if time.Now().After(converge) {
					t.Fatal("quiesce never converged after the detached collector drained")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// TestReconfigureLoopReclaimsFinishedStops is the P2 regression: completed
// pending stops must be reclaimed, so neither the list nor the cost of an empty
// drain grows with the number of reconfigurations.
//
// It also pins that a stop which keeps failing stays, and that its owner is not
// duplicated: the same failing collector must be judged once per drain, not
// copied once per attempt.
func TestReconfigureLoopReclaimsFinishedStops(t *testing.T) {
	const cycles = 128

	state := newStateWithoutCollector(nil)
	state.Register(nil)

	calls := 0
	for i := 0; i < cycles; i++ {
		finished := false
		state.SetCollector(func() drainOutcome {
			calls++
			if finished {
				return drainOutcome{drained: true}
			}
			return drainOutcome{pending: 1}
		})
		state.Register(nil)
		finished = true
		if _, err := state.Quiesce(); err != nil {
			t.Fatalf("cycle %d: quiesce after the collector finished: %v", i, err)
		}
		state.Register(nil)
	}

	if retained := state.pendingCount(); retained != 0 {
		t.Fatalf("pending entries after %d completed cycles = %d, want 0", cycles, retained)
	}

	before := calls
	if _, err := state.Quiesce(); err != nil {
		t.Fatalf("final quiesce: %v", err)
	}
	if extra := calls - before; extra != 0 {
		t.Fatalf("an empty drain re-ran %d completed stops; cost grows with history", extra)
	}
}

// A stop that keeps failing must stay pending, but repeated drains must judge
// it once each and must not append copies of it: re-judging is required, growing
// the list per attempt is not.
func TestFailingStopIsJudgedOncePerDrain(t *testing.T) {
	state := newStateWithoutCollector(nil)
	state.Register(nil)

	calls := 0
	state.SetCollector(func() drainOutcome {
		calls++
		return drainOutcome{pending: 1}
	})
	// A reconfigure detaches that collector while it is still undrained.
	state.Register(nil)

	if pending := state.pendingCount(); pending != 1 {
		t.Fatalf("pending entries = %d after one undrained detach, want 1", pending)
	}

	for i := 0; i < 8; i++ {
		before := calls
		if _, err := state.Quiesce(); err == nil {
			t.Fatalf("drain %d succeeded while the failing collector was undrained", i)
		}
		if judged := calls - before; judged != 1 {
			t.Fatalf("drain %d judged the failing owner %d times, want 1", i, judged)
		}
		if pending := state.pendingCount(); pending != 1 {
			t.Fatalf("pending entries after drain %d = %d, want the failing one retained exactly once", i, pending)
		}
	}
}
