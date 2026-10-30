package quotaplugin

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// SPEC 4.1 says quiesce must "wait for the collection in hand to abort", not
// just send the cancel and return. The host dlcloses the library once
// plugin.quiesce succeeds, so a fetch still executing past that point would run
// code from an unloaded object. These tests pin that Quiesce is a barrier.

// TestQuiesceWaitsForInFlightFetch is the direct assertion: a tracked fetch
// that needs time to wind down must be finished by the time Quiesce returns.
func TestQuiesceWaitsForInFlightFetch(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte(enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	provider, ok := state.Provider()
	if !ok {
		t.Fatal("provider unavailable after register")
	}
	ctx, release, tracked := provider.Track(nil)
	if !tracked {
		t.Fatal("tracking must be available on an active provider")
	}

	var mu sync.Mutex
	finished := false
	go func() {
		// A real fetch observes ctx.Done and returns through its own unwinding;
		// this models that window between cancellation and release.
		<-ctx.Done()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		finished = true
		mu.Unlock()
		release()
	}()

	state.Quiesce()

	mu.Lock()
	defer mu.Unlock()
	if !finished {
		t.Fatal("Quiesce returned while the tracked fetch was still winding down")
	}
}

// TestQuiesceReturnsWhenFetchIgnoresCancellation pins the bound: a fetch that
// never honours cancellation must not wedge a hot swap. The drain gives up after
// drainTimeout and Quiesce still returns.
func TestQuiesceReturnsWhenFetchIgnoresCancellation(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte(enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	provider, ok := state.Provider()
	if !ok {
		t.Fatal("provider unavailable")
	}
	releaseStuck := make(chan struct{})
	_, release, tracked := provider.Track(nil)
	if !tracked {
		t.Fatal("tracking must be available")
	}
	defer func() {
		close(releaseStuck)
		release()
	}()

	started := time.Now()
	state.Quiesce()
	elapsed := time.Since(started)

	if elapsed < drainTimeout {
		t.Fatalf("Quiesce returned after %v, want it to wait out the %v drain", elapsed, drainTimeout)
	}
	if elapsed > drainTimeout+3*time.Second {
		t.Fatalf("Quiesce took %v; the drain bound is not holding", elapsed)
	}
	// The instance is quiescing and refuses new work regardless.
	if _, ok := state.Provider(); ok {
		t.Fatal("provider must be refused after quiesce")
	}
}

// TestQuiesceWithoutTrackedWorkReturnsImmediately guards against the drain
// turning an ordinary quiesce into a multi-second stall.
func TestQuiesceWithoutTrackedWorkReturnsImmediately(t *testing.T) {
	state := NewState()
	if _, err := state.Register([]byte(enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}

	done := make(chan struct{})
	go func() {
		state.Quiesce()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("quiesce with nothing in flight must not wait for the drain timeout")
	}
}

// TestFetchAfterQuiesceIsRefusedWithAClearError covers the explicit "does a new
// FetchQuota get refused after quiesce, and is the error unambiguous" question.
func TestFetchAfterQuiesceIsRefusedWithAClearError(t *testing.T) {
	h := newRegisteredHandler(t)
	if _, err := h.Handle("plugin.quiesce", nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}

	for _, method := range []string{"quota.fetch", "quota.describe"} {
		raw, err := h.Handle(method, []byte(`{"provider":"ollama"}`))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		envelope := decodeEnvelope(t, raw)
		if envelope.OK {
			t.Fatalf("%s succeeded after quiesce", method)
		}
		if envelope.Error.Code != "plugin_quiesced" {
			t.Errorf("%s code = %q, want plugin_quiesced", method, envelope.Error.Code)
		}
		if envelope.Error.Message == "" {
			t.Errorf("%s error carries no message", method)
		}
	}
}

// The plugin only owns the quota path. This test pins the whole inference
// surface — every method a CPA inference or executor host call can reach — as
// refused with unknown_method both before and after quiesce, so an accidental
// capability can never appear without this test failing.
//
// It is the unit-level half of the "quiesce must not touch inference" claim.
// The end-to-end half (a live 40-second stream crossing a plugin swap on a real
// CPA) lives in scripts/verify-hotswap.sh, because only a real host can show
// that CPA's request path never enters the plugin library.
func TestPluginNeverServesInferenceSurface(t *testing.T) {
	// Real pluginabi method names, not invented ones: a typo here would make
	// the test pass for the wrong reason.
	methods := []string{
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodExecutorCountTokens,
		pluginabi.MethodExecutorHTTPRequest,
		pluginabi.MethodRequestTranslate,
		pluginabi.MethodRequestNormalize,
		pluginabi.MethodRequestInterceptBefore,
		pluginabi.MethodRequestInterceptAfter,
		pluginabi.MethodRequestComplete,
		pluginabi.MethodResponseTranslate,
		pluginabi.MethodResponseInterceptStreamChunk,
		pluginabi.MethodWebSocketResponseEvent,
		pluginabi.MethodThinkingApply,
		pluginabi.MethodUsageHandle,
		pluginabi.MethodModelRoute,
		pluginabi.MethodSchedulerPick,
	}
	for _, phase := range []struct {
		name  string
		enter func(*Handler)
	}{
		{"active", func(*Handler) {}},
		{"quiescing", func(h *Handler) { h.Handle("plugin.quiesce", nil) }},
		{"stopped", func(h *Handler) { h.Handle("plugin.shutdown", nil) }},
	} {
		t.Run(phase.name, func(t *testing.T) {
			h := newRegisteredHandler(t)
			phase.enter(h)
			for _, method := range methods {
				raw, err := h.Handle(method, nil)
				if err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				envelope := decodeEnvelope(t, raw)
				if envelope.OK {
					t.Fatalf("%s was served while %s", method, phase.name)
				}
				if envelope.Error.Code != "unknown_method" {
					t.Errorf("%s code = %q, want unknown_method", method, envelope.Error.Code)
				}
			}
		})
	}
}

// A failed hot swap rolls back by re-registering the old library. That path
// must leave the instance serving, and the provider must accept work again.
// The host-level behaviour is proven in docs/hotswap-verification.md; this pins
// the plugin half.
func TestRollbackRegisterAfterQuiesceServesAgain(t *testing.T) {
	h := newRegisteredHandler(t)
	// Give the first generation an in-flight fetch so the quiesce actually
	// drains, which is the window a rollback would race.
	provider, ok := h.state.Provider()
	if !ok {
		t.Fatal("provider unavailable")
	}
	_, release, tracked := provider.Track(context.Background())
	if !tracked {
		t.Fatal("tracking must be available")
	}
	release()

	if _, err := h.Handle("plugin.quiesce", nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	raw, err := h.Handle("plugin.register", registerPayload(t, enabledConfig))
	if err != nil {
		t.Fatalf("rollback register: %v", err)
	}
	if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("rollback register failed: %+v", envelope.Error)
	}
	fetched, err := h.Handle("quota.fetch", []byte(`{"provider":"ollama"}`))
	if err != nil {
		t.Fatalf("fetch after rollback: %v", err)
	}
	envelope := decodeEnvelope(t, fetched)
	if !envelope.OK {
		t.Fatalf("the rolled-back instance must serve quota again: %+v", envelope.Error)
	}
	var resp map[string]any
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatalf("decode fetch: %v", err)
	}
	if resp["summary"] == nil {
		t.Fatalf("fetch after rollback returned no summary: %v", resp)
	}
}
