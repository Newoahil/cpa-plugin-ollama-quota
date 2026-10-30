package quotaplugin

import (
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// The review reproduced this with a blocking collector hook: Quiesce set the
// phase to quiescing, then blocked in stop(), Register won the race and moved
// the state back to active, and only then did Quiesce continue and cancel the
// provider. The result was Phase()==active with Track()==false, so every
// fetch was refused while the state claimed to be serving.
//
// This test forces that interleaving with channels instead of sleeps. It must
// stay deterministic: the hook signals that Quiesce has entered, and Register
// is not allowed to start until that signal arrives.
func TestQuiesceAndRegisterCannotInterleave(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("initial register: %v", err)
	}

	enteredStop := make(chan struct{})
	releaseStop := make(chan struct{})
	state.SetCollector(func() {
		close(enteredStop)
		<-releaseStop
	})

	quiesceDone := make(chan Phase, 1)
	go func() {
		quiesceDone <- state.Quiesce()
	}()

	// Wait until Quiesce is provably inside its stop hook — the phase has been
	// committed but the provider has not been cancelled yet.
	<-enteredStop

	registerDone := make(chan struct{})
	go func() {
		defer close(registerDone)
		if _, err := state.Register([]byte("enabled: true\n")); err != nil {
			t.Errorf("register during quiesce: %v", err)
		}
	}()

	// Give Register every chance to race ahead of Quiesce. With the state and
	// the provider reset under one lock, it must block until Quiesce is done;
	// before the fix this returned in microseconds and the invariant below
	// failed. The wait only has to cover "did it sneak through", not a real
	// deadline, so it stays short.
	select {
	case <-registerDone:
	case <-time.After(250 * time.Millisecond):
	}

	close(releaseStop)
	<-quiesceDone
	<-registerDone

	// Whatever the order, the observable state must be self-consistent: an
	// active phase means the provider accepts work.
	if state.Phase() == PhaseActive {
		provider, ok := state.Provider()
		if !ok {
			t.Fatalf("phase is active but provider() says unavailable")
		}
		if _, _, tracked := provider.Track(nil); !tracked {
			t.Fatalf("phase is active but the provider refuses tracked work: state and provider disagree")
		}
	}
}

// Shutdown is documented as terminal. Register after it must not resurrect the
// instance, otherwise the phase constant's contract is a lie and the host could
// quietly start serving quota from an unloaded library.
func TestRegisterAfterShutdownStaysStopped(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	state.Shutdown()
	if phase := state.Phase(); phase != PhaseStopped {
		t.Fatalf("phase after shutdown = %s, want stopped", phase)
	}

	_, err := state.Register([]byte("enabled: true\n"))
	if err == nil {
		t.Error("register after shutdown should be rejected")
	}
	if phase := state.Phase(); phase != PhaseStopped {
		t.Errorf("phase after rejected register = %s, want stopped", phase)
	}
	if _, ok := state.Provider(); ok {
		t.Error("a stopped instance must not serve quota")
	}
}

// A hot-reload rollback re-registers the same library after quiesce; that has
// to keep working, which is the case the review explicitly called out.
func TestRegisterAfterQuiesceStillReArms(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	state.Quiesce()
	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("register after quiesce: %v", err)
	}
	if phase := state.Phase(); phase != PhaseActive {
		t.Fatalf("phase = %s, want active", phase)
	}
	provider, ok := state.Provider()
	if !ok {
		t.Fatal("provider unavailable after re-register")
	}
	if _, _, tracked := provider.Track(nil); !tracked {
		t.Fatal("provider refuses work after re-register")
	}
}

// Quiesce must stop accepting work before it cancels, and the cancellation must
// be visible by the time Quiesce returns. Otherwise the host can unload the
// library while a fetch is still running.
func TestQuiesceReturnsOnlyAfterCancellation(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	provider, ok := state.Provider()
	if !ok {
		t.Fatal("provider unavailable")
	}
	fetchCtx, release, tracked := provider.Track(nil)
	if !tracked {
		t.Fatal("tracking must be available on an active provider")
	}
	defer release()

	state.Quiesce()

	select {
	case <-fetchCtx.Done():
	default:
		t.Fatal("Quiesce returned before cancelling the in-flight fetch")
	}
	if _, _, tracked := provider.Track(nil); tracked {
		t.Fatal("provider accepts new work after Quiesce returned")
	}
}

// The RPC surface must refuse work during quiesce and after shutdown. This is
// the handler-level half of the invariant the state machine guarantees.
func TestRPCSurfaceRefusesWorkWhenNotActive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		enter func(*Handler)
	}{
		{"quiesced", func(h *Handler) { h.Handle(pluginabi.MethodPluginQuiesce, nil) }},
		{"stopped", func(h *Handler) { h.Handle(pluginabi.MethodPluginShutdown, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisteredHandler(t)
			tc.enter(h)
			for _, method := range []string{
				pluginabi.MethodQuotaDescribe,
				pluginabi.MethodQuotaFetch,
				pluginabi.MethodQuotaReset,
			} {
				raw, err := h.Handle(method, []byte(`{"provider":"ollama"}`))
				if err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				envelope := decodeEnvelope(t, raw)
				if envelope.OK {
					t.Errorf("%s served a request while %s", method, tc.name)
				}
				if envelope.Error.Code != "plugin_quiesced" {
					t.Errorf("%s error code = %q, want plugin_quiesced", method, envelope.Error.Code)
				}
			}
		})
	}
}

// concurrentQuiesceAndRegister hammers the two entry points from several
// goroutines; with the phases and the provider under one lock there must be no
// interleaving that leaves them disagreeing.
func TestConcurrentTransitionsStayConsistent(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}

	var wg sync.WaitGroup
	const iterations = 200
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			state.Quiesce()
		}()
		go func() {
			defer wg.Done()
			state.Register([]byte("enabled: true\n"))
		}()
	}
	wg.Wait()

	if state.Phase() != PhaseActive {
		return
	}
	provider, ok := state.Provider()
	if !ok {
		t.Fatal("phase is active but provider is unavailable")
	}
	if _, release, tracked := provider.Track(nil); !tracked {
		t.Fatal("phase is active but provider refuses work")
	} else {
		release()
	}
}
