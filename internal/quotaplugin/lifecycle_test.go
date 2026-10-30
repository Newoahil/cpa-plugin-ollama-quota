package quotaplugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// registerPayload builds the lifecycle request body the host sends.
func registerPayload(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(LifecycleRequest{ConfigYAML: []byte(configYAML), SchemaVersion: 1})
	if errMarshal != nil {
		t.Fatalf("marshal register payload: %v", errMarshal)
	}
	return raw
}

func TestLifecycleRegisterThenQuiesceRefusesNewWork(t *testing.T) {
	h := newHandlerForTest(t)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "enabled: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok := h.state.Provider(); !ok {
		t.Fatal("provider must be available right after register")
	}

	raw, err := h.Handle(pluginabi.MethodPluginQuiesce, nil)
	if err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if !decodeEnvelope(t, raw).OK {
		t.Fatal("quiesce must succeed")
	}
	if _, ok := h.state.Provider(); ok {
		t.Fatal("provider must be refused after quiesce")
	}
	if phase := h.state.Phase(); phase != PhaseQuiescing {
		t.Errorf("phase = %s, want quiescing", phase)
	}

	// A fetch that races the quiesce gets a clean error, not sample data.
	raw, err = h.Handle(pluginabi.MethodQuotaFetch, []byte(`{"provider":"ollama"}`))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if envelope := decodeEnvelope(t, raw); envelope.OK {
		t.Fatal("quiesced plugin must not serve quota")
	}
}

func TestQuiesceIsIdempotent(t *testing.T) {
	h := newHandlerForTest(t)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "")); err != nil {
		t.Fatalf("register: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := h.Handle(pluginabi.MethodPluginQuiesce, nil); err != nil {
			t.Fatalf("quiesce %d: %v", i, err)
		}
	}
	if phase := h.state.Phase(); phase != PhaseQuiescing {
		t.Errorf("phase = %s, want quiescing", phase)
	}
}

func TestRegisterAfterQuiesceReArmsTheInstance(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))
	h := newHandlerWithHost(t, host)
	t.Cleanup(func() { h.state.Shutdown() })
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "")); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := h.Handle(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	// The host calls register again with the same library after a hot reload.
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "")); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if phase := h.state.Phase(); phase != PhaseActive {
		t.Fatalf("phase = %s, want active", phase)
	}
	raw, err := h.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("re-registered plugin must serve quota again: %+v", envelope.Error)
	}
}

func TestShutdownStopsTheInstance(t *testing.T) {
	h := newHandlerForTest(t)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := h.Handle(pluginabi.MethodPluginShutdown, nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if phase := h.state.Phase(); phase != PhaseStopped {
		t.Errorf("phase = %s, want stopped", phase)
	}
	if _, ok := h.state.Provider(); ok {
		t.Error("provider must not be available after shutdown")
	}
}

// A collector registered for #2 must be stopped by quiesce, otherwise the
// goroutine survives the library unload.
func TestQuiesceStopsRegisteredCollector(t *testing.T) {
	h := newHandlerForTest(t)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "")); err != nil {
		t.Fatalf("register: %v", err)
	}
	stopped := false
	h.state.SetCollector(func() { stopped = true })
	if _, err := h.Handle(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if !stopped {
		t.Fatal("quiesce must call the registered collector stop function")
	}
}

func TestQuiesceCancelsInFlightFetch(t *testing.T) {
	h := newHandlerForTest(t)
	if _, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "")); err != nil {
		t.Fatalf("register: %v", err)
	}
	provider, ok := h.state.Provider()
	if !ok {
		t.Fatal("provider unavailable")
	}
	ctx, release, tracked := provider.Track(nil)
	if !tracked {
		t.Fatal("tracking must be available on an active provider")
	}
	defer release()

	provider.CancelAll()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("CancelAll must cancel a tracked fetch")
	}
	if _, _, tracked = provider.Track(nil); tracked {
		t.Fatal("a cancelled provider must refuse new tracked work")
	}
}

func TestUnknownMethodIsReportedAsUnsupported(t *testing.T) {
	raw, err := newHandlerForTest(t).Handle("executor.execute", nil)
	if err != nil {
		t.Fatalf("unknown method: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("unknown method must not succeed")
	}
	if envelope.Error.Code != "unknown_method" {
		t.Errorf("code = %q, want unknown_method", envelope.Error.Code)
	}
}
