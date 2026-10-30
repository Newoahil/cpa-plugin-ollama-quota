package quotaplugin

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// These pin the two findings the re-review left open (fix round 2).

// P2-1: the collector's first host.auth.get must run under the same attempt
// budget as the HTTP that follows it.
//
// Before this fix refreshOne passed the collector's bare context, which has no
// deadline, so callBounded took its synchronous path and auth.get was outside
// every budget. The re-review reproduced it as
// "collector auth.get deadline=false; quiesce returned with host call still
// blocked". The assertion is on the deadline the host actually observes, not on
// anything the plugin claims.
func TestCollectorAuthGetHasAttemptDeadline(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))
	release := host.setAuthGetBlock()

	state := NewStateFor(host.call)
	state.provider.timeout = 80 * time.Millisecond
	if _, errRegister := state.Register([]byte("interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() {
		release()
		state.Shutdown()
	})

	// The collector's first sweep runs immediately and parks in auth.get.
	deadline := time.Now().Add(5 * time.Second)
	for !host.authGetHadDeadline.Load() {
		if time.Now().After(deadline) {
			release()
			t.Fatal("the collector never called host.auth.get under a deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !host.authGetHadDeadline.Load() {
		release()
		t.Fatal("host.auth.get arrived without a deadline: it is outside the attempt budget")
	}

	// And the sweep must give up on it at that budget rather than waiting for
	// the host to answer. The budget is 80ms; allow a generous ceiling so the
	// test measures "bounded", not scheduler precision.
	time.Sleep(300 * time.Millisecond)

	// The quiesce must also settle without the host ever releasing the call.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = state.Quiesce()
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		release()
		t.Fatal("quiesce blocked on an auth.get the host never released")
	}
	release()
}

// P1-b: when the drain budget expires, quiesce must fail observably at the RPC
// boundary rather than reporting success while a host call is still live.
//
// The re-review's repro held the cancel callback inside the host past the stop
// budget and observed "quiesce returned while cancel still inside host" with an
// OK envelope. Success there is the dangerous answer: the host reads it as
// "safe to unload" and swaps a library that still has a callback running.
func TestQuiesceReportsIncompleteDrainAtRPC(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setBlock()
	cancelEntered, releaseCancel := host.setCancelHold()
	t.Cleanup(releaseCancel)

	state := NewStateFor(host.call)
	// A tiny attempt budget keeps the bounded wait short in the test; the
	// production default is 20s with the same control flow.
	state.provider.timeout = 80 * time.Millisecond
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 1\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() {
		releaseCancel()
		_ = state.Shutdown()
	})

	// Wait until the cancel callback is parked inside the host, then quiesce.
	select {
	case <-cancelEntered:
	case <-time.After(15 * time.Second):
		releaseCancel()
		t.Fatal("the cancel callback never entered the host")
	}

	raw, errHandle := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	if errHandle != nil {
		t.Fatalf("the handler returned a Go error instead of an envelope: %v", errHandle)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		releaseCancel()
		t.Fatal("quiesce reported success while the drain budget had expired with a host call still live")
	}
	if envelope.Error == nil || envelope.Error.Code != "quiesce_incomplete" {
		releaseCancel()
		t.Fatalf("error = %+v, want the quiesce_incomplete code", envelope.Error)
	}
	// The message must be safe to show: a count, never callback payload.
	if strings.Contains(envelope.Error.Message, "fixture") {
		t.Errorf("the quiesce error echoes host data: %q", envelope.Error.Message)
	}

	// Releasing the callback lets the next quiesce converge on success, which is
	// the "safe to complete afterwards" half of the requirement.
	releaseCancel()
	deadline := time.Now().Add(15 * time.Second)
	for {
		raw, _ = handler.Handle(pluginabi.MethodPluginQuiesce, nil)
		if decodeEnvelope(t, raw).OK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("quiesce never converged to success after the callback was released")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The short-block case must still pass cleanly: a callback that finishes inside
// the budget drains normally and quiesce reports success.
func TestQuiesceSucceedsWhenDrainFitsTheBudget(t *testing.T) {
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

	// Let the first sweep finish so the drain has nothing outstanding.
	time.Sleep(100 * time.Millisecond)
	raw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("a quiesce with nothing outstanding must succeed: %+v", envelope.Error)
	}
}
