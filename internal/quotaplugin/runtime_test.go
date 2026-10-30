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

// Shutdown is idempotent: the host must be able to call it more than once
// without the second call touching a generation that is already stopped.
func TestRuntimeShutdownIsIdempotent(t *testing.T) {
	runtime := NewRuntime()

	handler := runtime.Init()
	if _, err := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register: %v", err)
	}
	runtime.Shutdown()
	if phase := runtime.State().Phase(); phase != PhaseStopped {
		t.Fatalf("phase after shutdown = %s, want stopped", phase)
	}
	for i := 0; i < 3; i++ {
		runtime.Shutdown()
		if phase := runtime.State().Phase(); phase != PhaseStopped {
			t.Fatalf("phase after repeated shutdown #%d = %s, want stopped", i, phase)
		}
		if live := runtime.LiveGenerations(); live != 0 {
			t.Fatalf("live generations after repeated shutdown = %d, want 0", live)
		}
	}
	// Still refused after the extra calls: nothing revived the instance.
	if raw, err := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("late register: %v", err)
	} else if decodeEnvelope(t, raw).OK {
		t.Fatal("a stopped instance must not accept a register after repeated shutdown")
	}
}

// The host issues one shutdown per init, and FIFO spends one live generation
// per shutdown it receives. This test drives the synthetic "old client first"
// order: the retired client's shutdown arrives before the live client's own.
// The host does not actually order them this way — it closes the active client
// first — so TestRuntimeShutdownAfterReplacementKeepsNewestServing pins the
// property that has to hold under either order.
func TestRuntimeShutdownRetiresOldestGeneration(t *testing.T) {
	runtime := NewRuntime()

	first := runtime.Init()
	if _, err := first.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Hot replacement: the host inits the new client while the old one is still
	// held, and registers it before the old client is shut down.
	second := runtime.Init()
	if _, err := second.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if live := runtime.LiveGenerations(); live != 2 {
		t.Fatalf("live generations = %d, want 2", live)
	}

	// The retired client's shutdown arrives late, after the new generation was
	// registered. It must retire the first generation, not the live one.
	runtime.Shutdown()
	if phase := first.State().Phase(); phase != PhaseStopped {
		t.Fatalf("retired generation phase = %s, want stopped", phase)
	}
	if phase := second.State().Phase(); phase != PhaseActive {
		t.Fatalf("live generation phase after the retired client's shutdown = %s, want active", phase)
	}
	if _, ok := second.State().Provider(); !ok {
		t.Fatal("the live generation must keep serving after the retired client shuts down")
	}
	raw, err := second.Handle(pluginabi.MethodQuotaFetch, []byte(`{"provider":"ollama"}`))
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("the live generation must still fetch after the retired client shuts down: %+v", envelope.Error)
	}

	// The live client's own shutdown then retires it.
	runtime.Shutdown()
	if phase := second.State().Phase(); phase != PhaseStopped {
		t.Fatalf("live generation phase after its own shutdown = %s, want stopped", phase)
	}
	if live := runtime.LiveGenerations(); live != 0 {
		t.Fatalf("live generations = %d, want 0", live)
	}
}

// Under the host's real unload order the active client is shut down before the
// retired ones, so the first shutdown a replacement's queue receives can belong
// to the generation that is currently serving. The ABI cannot tell which
// generation a shutdown belongs to, so this test asserts only the property that
// must hold under either order: after one shutdown the newest generation is
// still serving, and the second shutdown leaves nothing live. It deliberately
// does not assert which generation the first shutdown retired.
func TestRuntimeShutdownAfterReplacementKeepsNewestServing(t *testing.T) {
	runtime := NewRuntime()

	first := runtime.Init()
	if _, err := first.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("first register: %v", err)
	}
	second := runtime.Init()
	if _, err := second.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if live := runtime.LiveGenerations(); live != 2 {
		t.Fatalf("live generations = %d, want 2", live)
	}

	// First shutdown of an unload sequence. Whichever generation it retires, the
	// newest one has to keep serving.
	runtime.Shutdown()

	if newest := runtime.Current(); newest != second {
		t.Fatal("Current must still be the newest generation while it is live")
	}
	if phase := second.State().Phase(); phase != PhaseActive {
		t.Fatalf("newest phase after first shutdown = %s, want active", phase)
	}
	if _, ok := second.State().Provider(); !ok {
		t.Fatal("the newest generation must keep its provider after the first shutdown")
	}
	if raw, err := second.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); err != nil {
		t.Fatalf("register on the newest generation: %v", err)
	} else if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("the newest generation must still accept a register: %+v", envelope.Error)
	}
	if raw, err := second.Handle(pluginabi.MethodQuotaFetch, []byte(`{"provider":"ollama"}`)); err != nil {
		t.Fatalf("fetch on the newest generation: %v", err)
	} else if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("the newest generation must still fetch after the first shutdown: %+v", envelope.Error)
	}

	// The second shutdown drains the queue; whatever the first one retired, all
	// generations end stopped.
	runtime.Shutdown()
	if live := runtime.LiveGenerations(); live != 0 {
		t.Fatalf("live generations = %d, want 0", live)
	}
	if phase := second.State().Phase(); phase != PhaseStopped {
		t.Fatalf("newest phase after second shutdown = %s, want stopped", phase)
	}
	if phase := first.State().Phase(); phase != PhaseStopped {
		t.Fatalf("oldest phase after second shutdown = %s, want stopped", phase)
	}
}

// The reinstall sequence the host actually uses is shut down, then initialised
// again; each generation is retired by its own shutdown and the new one serves.
func TestRuntimeReinstallCycleKeepsServing(t *testing.T) {
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
		if _, ok := runtime.State().Provider(); !ok {
			t.Fatalf("cycle %d must serve quota", cycle)
		}
		runtime.Shutdown()
		if phase := handler.State().Phase(); phase != PhaseStopped {
			t.Fatalf("cycle %d phase after shutdown = %s, want stopped", cycle, phase)
		}
		if live := runtime.LiveGenerations(); live != 0 {
			t.Fatalf("cycle %d live generations = %d, want 0", cycle, live)
		}
	}
}
