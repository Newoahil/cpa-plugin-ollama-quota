package quotaplugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// A reinstall is a new ABI generation: the host dlopen/init again in the same
// process. Go package state survives that, so the second init must build a
// fresh instance instead of inheriting the stopped one from the first.
func TestRuntimeReinitAfterShutdownWorks(t *testing.T) {
	runtime := NewRuntime()

	first := runtime.Init()
	if _, err := first.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("first register: %v", err)
	}
	runtime.Shutdown()
	if phase := runtime.State().Phase(); phase != PhaseStopped {
		t.Fatalf("first generation phase = %s, want stopped", phase)
	}

	// This is the load that used to fail with "plugin instance is stopped".
	second := runtime.Init()
	if second == first {
		t.Fatal("Init must build a new handler, not reuse the retired one")
	}
	raw, err := second.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig))
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("re-register after shutdown failed: %+v", envelope.Error)
	}
	var reg Registration
	if err := json.Unmarshal(envelope.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if !reg.Capabilities.QuotaProvider {
		t.Error("reinstalled generation must declare the quota capability again")
	}
	if phase := runtime.State().Phase(); phase != PhaseActive {
		t.Fatalf("phase after reinstall = %s, want active", phase)
	}

	// The reinstalled plugin must actually serve quota, not just register.
	fetchRaw, err := second.Handle(pluginabi.MethodQuotaFetch, []byte(`{"provider":"ollama"}`))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fetchEnvelope := decodeEnvelope(t, fetchRaw); !fetchEnvelope.OK {
		t.Fatalf("fetch after reinstall failed: %+v", fetchEnvelope.Error)
	}
	if _, ok := runtime.State().Provider(); !ok {
		t.Error("reinstalled generation must expose a working provider")
	}
}

// A register that was still in flight during an unload reaches the retired
// handler, and must be refused. That handler is no longer current, so it cannot
// turn a dead generation back on.
func TestRuntimeRetiredGenerationRejectsLateRegister(t *testing.T) {
	runtime := NewRuntime()

	retired := runtime.Init()
	if _, err := retired.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	runtime.Shutdown()

	// The host registers the new generation right after Init.
	live := runtime.Init()
	if _, err := live.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register on the new generation: %v", err)
	}

	// The old handler reference held by the in-flight call.
	raw, err := retired.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig))
	if err != nil {
		t.Fatalf("late register: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a retired generation must not accept a late register")
	}
	if envelope.Error.Code != "plugin_stopped" {
		t.Errorf("error code = %q, want plugin_stopped", envelope.Error.Code)
	}
	if phase := retired.State().Phase(); phase != PhaseStopped {
		t.Errorf("retired generation phase = %s, want stopped", phase)
	}
	if _, ok := retired.State().Provider(); ok {
		t.Error("a retired generation must not serve quota")
	}

	// The live generation is unaffected by the stale call.
	if _, ok := runtime.State().Provider(); !ok {
		t.Error("the current generation must keep serving after a stale call")
	}
}

// Reinstalling must not require a version bump: the host reuses the same file.
func TestRuntimeReinitSameFileSameGenerationContent(t *testing.T) {
	runtime := NewRuntime()
	for cycle := 1; cycle <= 3; cycle++ {
		handler := runtime.Init()
		raw, err := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig))
		if err != nil {
			t.Fatalf("cycle %d register: %v", cycle, err)
		}
		if envelope := decodeEnvelope(t, raw); !envelope.OK {
			t.Fatalf("cycle %d register failed: %+v", cycle, envelope.Error)
		}
		if got, want := runtime.Generation(), uint64(cycle); got != want {
			t.Errorf("generation = %d, want %d", got, want)
		}
		runtime.Shutdown()
	}
}

// A call before Init has no handler at all. The C shim guards this too; the
// Runtime just has to report it honestly.
func TestRuntimeBeforeInit(t *testing.T) {
	runtime := NewRuntime()
	if runtime.Current() != nil {
		t.Fatal("Current before Init must be nil")
	}
	if runtime.Generation() != 0 {
		t.Errorf("Generation before Init = %d, want 0", runtime.Generation())
	}
	// Shutdown before Init must not panic.
	runtime.Shutdown()
}
