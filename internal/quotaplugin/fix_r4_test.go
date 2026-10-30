package quotaplugin

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// These pin the cross-entry defect the second final review found: a failed
// shutdown leaves the instance PhaseStopped, and Quiesce used to answer nil
// from the phase while the callback that failed the shutdown was still running.
//
// The rule under test is the same one the earlier rounds established, now
// applied across entries: a lifecycle call may only report success when IT
// observed no plugin host call running. Phase is never a substitute, in either
// direction.

// newHeldCallbackPlugin starts a real collector whose cancel callback parks
// inside the host, and returns the handler plus the release/exit signals.
func newHeldCallbackPlugin(t *testing.T, timeout time.Duration) (handler *Handler, release func(), exited <-chan struct{}) {
	t.Helper()
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setBlock()
	cancelEntered, releaseCancel, cancelExited := host.setCancelHold()
	t.Cleanup(releaseCancel)

	state := NewStateFor(host.call)
	state.provider.timeout = timeout
	handler = NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 1\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() {
		releaseCancel()
		_ = state.Shutdown()
	})

	select {
	case <-cancelEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback never entered the host")
	}
	return handler, releaseCancel, cancelExited
}

// shutdown fails -> quiesce must still fail -> after the callback leaves,
// quiesce succeeds.
func TestQuiesceAfterFailedShutdownStillFails(t *testing.T) {
	handler, release, exited := newHeldCallbackPlugin(t, 70*time.Millisecond)

	// --- failed shutdown, callback still held -------------------------------
	shutdownRaw, _ := handler.Handle(pluginabi.MethodPluginShutdown, nil)
	shutdownEnvelope := decodeEnvelope(t, shutdownRaw)
	if shutdownEnvelope.OK {
		t.Fatal("shutdown reported success while a host callback was live")
	}
	if shutdownEnvelope.Error == nil || shutdownEnvelope.Error.Code != "shutdown_incomplete" {
		t.Fatalf("shutdown error = %+v, want shutdown_incomplete", shutdownEnvelope.Error)
	}

	// --- quiesce while still held: must NOT answer from PhaseStopped --------
	quiesceRaw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	quiesceEnvelope := decodeEnvelope(t, quiesceRaw)
	if quiesceEnvelope.OK {
		t.Fatal("quiesce reported success after a failed shutdown while the callback was still running")
	}
	if quiesceEnvelope.Error == nil || quiesceEnvelope.Error.Code != "quiesce_incomplete" {
		t.Fatalf("quiesce error = %+v, want quiesce_incomplete", quiesceEnvelope.Error)
	}

	// --- release, observe the exit, then it may succeed ---------------------
	release()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback did not leave the host after being released")
	}
	afterRaw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	if envelope := decodeEnvelope(t, afterRaw); !envelope.OK {
		t.Fatalf("quiesce still failed after the callback exited: %+v", envelope.Error)
	}
}

// The reverse order: quiesce fails, shutdown must still fail, and only after
// the callback leaves does it converge.
func TestShutdownAfterFailedQuiesceStillFails(t *testing.T) {
	handler, release, exited := newHeldCallbackPlugin(t, 70*time.Millisecond)

	quiesceRaw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	quiesceEnvelope := decodeEnvelope(t, quiesceRaw)
	if quiesceEnvelope.OK {
		t.Fatal("quiesce reported success while a host callback was live")
	}
	if quiesceEnvelope.Error == nil || quiesceEnvelope.Error.Code != "quiesce_incomplete" {
		t.Fatalf("quiesce error = %+v, want quiesce_incomplete", quiesceEnvelope.Error)
	}

	shutdownRaw, _ := handler.Handle(pluginabi.MethodPluginShutdown, nil)
	if envelope := decodeEnvelope(t, shutdownRaw); envelope.OK {
		t.Fatal("shutdown reported success after a failed quiesce while the callback was still running")
	}

	release()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback did not leave the host after being released")
	}
	afterRaw, _ := handler.Handle(pluginabi.MethodPluginShutdown, nil)
	if envelope := decodeEnvelope(t, afterRaw); !envelope.OK {
		t.Fatalf("shutdown still failed after the callback exited: %+v", envelope.Error)
	}
}

// A stopped instance that never had work in flight still reports success, so
// the rule above is not achieved by making every post-shutdown call fail.
func TestStoppedInstanceWithoutWorkStillSucceeds(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))

	state := NewStateFor(host.call)
	state.provider.timeout = 2 * time.Second
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { _ = state.Shutdown() })

	// Let the first sweep finish, then shut down cleanly.
	time.Sleep(100 * time.Millisecond)
	if raw, _ := handler.Handle(pluginabi.MethodPluginShutdown, nil); !decodeEnvelope(t, raw).OK {
		t.Fatal("a clean shutdown must succeed")
	}
	// The instance is stopped with nothing in flight; a quiesce is still honest.
	if raw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil); !decodeEnvelope(t, raw).OK {
		t.Fatalf("quiesce on a drained, stopped instance must succeed: %s", raw)
	}
}

// A reconfigure that replaces a collector whose drain fails must not lose that
// failure: the replaced collector's callback is still running, and the fresh
// collector knows nothing about it.
//
// This is the Register-side version of the same defect. Without retention the
// new collector's empty in-flight set would answer "drained" for work that is
// still inside the host.
func TestReconfigureKeepsJudgingTheReplacedCollectorsWork(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setBlock()
	cancelEntered, releaseCancel, cancelExited := host.setCancelHold()
	t.Cleanup(releaseCancel)

	state := NewStateFor(host.call)
	state.provider.timeout = 70 * time.Millisecond
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 1\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() {
		releaseCancel()
		_ = state.Shutdown()
	})

	select {
	case <-cancelEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback never entered the host")
	}

	// Reconfigure while the callback is still parked. Register stops the old
	// collector; that stop cannot drain, and the fact must not be dropped.
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("reconfigure: %v", errRegister)
	}

	// The replacement instance is active with (as far as it knows) nothing in
	// flight, but the old callback is still inside the host.
	quiesceRaw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	if decodeEnvelope(t, quiesceRaw).OK {
		t.Fatal("quiesce reported success after a reconfigure left the replaced collector's callback running")
	}

	releaseCancel()
	select {
	case <-cancelExited:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback did not leave the host after being released")
	}
	// Once it is genuinely gone, the same instance may report success.
	deadline := time.Now().Add(20 * time.Second)
	for {
		raw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
		if decodeEnvelope(t, raw).OK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("quiesce never converged after the replaced collector's callback exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
