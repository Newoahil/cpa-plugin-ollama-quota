package quotaplugin

import (
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// P1-b, fix round 3: a repeated quiesce must not report success while the
// callback that failed the first drain is still running.
//
// The final review held one cancel callback inside the host, quiesced (correctly
// failing with pending=1), quiesced again with the callback STILL held, and got
// ok=true. The cause was two-fold: Quiesce returned nil as soon as it saw
// phase==PhaseQuiescing, and the first call had already consumed the stop
// function, so nothing was left to re-judge with.
//
// The evidence this test has to produce, in order:
//
//  1. first quiesce  -> explicit failure carrying a pending count
//  2. callback still held, second quiesce -> still failure (not ok)
//  3. release, observe the callback actually leave, third quiesce -> success
//     AND that third call really ran a fresh drain judgement
func TestRepeatedQuiesceKeepsFailingUntilTheCallbackExits(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setBlock()
	cancelEntered, releaseCancel, cancelExited := host.setCancelHold()
	t.Cleanup(releaseCancel)

	state := NewStateFor(host.call)
	// A small attempt budget keeps the bounded waits short; the production
	// default is 20s and runs the same control flow.
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

	// --- 1. first quiesce must fail, with a pending count -------------------
	firstRaw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	first := decodeEnvelope(t, firstRaw)
	if first.OK {
		t.Fatal("first quiesce reported success while a host callback was live")
	}
	if first.Error == nil || first.Error.Code != DrainTimeoutCode {
		t.Fatalf("first quiesce error = %+v, want quiesce_incomplete", first.Error)
	}
	afterFirst := state.DrainAttempts()
	if afterFirst == 0 {
		t.Fatal("the first quiesce never ran a drain judgement")
	}

	// --- 2. second quiesce, callback STILL held: must still fail ------------
	secondRaw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	second := decodeEnvelope(t, secondRaw)
	if second.OK {
		t.Fatal("second quiesce reported success while the same callback was still running")
	}
	if second.Error == nil || second.Error.Code != DrainTimeoutCode {
		t.Fatalf("second quiesce error = %+v, want quiesce_incomplete", second.Error)
	}
	if got := state.DrainAttempts(); got <= afterFirst {
		t.Fatalf("drain attempts %d did not grow past %d: the repeat took an early-return path "+
			"instead of re-judging", got, afterFirst)
	}

	// --- 3. release, observe the exit, third quiesce must succeed -----------
	releaseCancel()
	select {
	case <-cancelExited:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback did not leave the host after being released")
	}
	afterSecond := state.DrainAttempts()
	checksAfterSecond := state.provider.bridge().DrainChecks()

	thirdRaw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	third := decodeEnvelope(t, thirdRaw)
	if !third.OK {
		t.Fatalf("third quiesce still failed after the callback exited: %+v", third.Error)
	}
	// The success has to come from a fresh judgement on THIS call, not from an
	// early return that trusts the previous attempts.
	if got := state.DrainAttempts(); got <= afterSecond {
		t.Fatalf("drain attempts %d did not grow past %d: the third quiesce returned success "+
			"without re-judging the drain", got, afterSecond)
	}
	// And that judgment must have actually examined the in-flight set. The
	// attempt counter above can grow on any path through State.drain; only
	// the bridge counter proves the real question was asked.
	if got := state.provider.bridge().DrainChecks(); got <= checksAfterSecond {
		t.Fatalf("in-flight checks %d did not grow past %d: the third quiesce reported success "+
			"without examining the in-flight set", got, checksAfterSecond)
	}
}

// Shutdown follows the same rule: a repeated shutdown must not report success
// while the callback that failed the first drain is still running.
func TestRepeatedShutdownKeepsFailingUntilTheCallbackExits(t *testing.T) {
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

	firstRaw, _ := handler.Handle(pluginabi.MethodPluginShutdown, nil)
	if first := decodeEnvelope(t, firstRaw); first.OK {
		t.Fatal("first shutdown reported success while a host callback was live")
	}

	// Still held: the repeat must fail too, not answer from "already stopped".
	secondRaw, _ := handler.Handle(pluginabi.MethodPluginShutdown, nil)
	second := decodeEnvelope(t, secondRaw)
	if second.OK {
		t.Fatal("second shutdown reported success while the same callback was still running")
	}
	if second.Error == nil || second.Error.Code != DrainTimeoutCode {
		t.Fatalf("second shutdown error = %+v, want shutdown_incomplete", second.Error)
	}

	releaseCancel()
	select {
	case <-cancelExited:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback did not leave the host after being released")
	}
	thirdRaw, _ := handler.Handle(pluginabi.MethodPluginShutdown, nil)
	if third := decodeEnvelope(t, thirdRaw); !third.OK {
		t.Fatalf("third shutdown still failed after the callback exited: %+v", third.Error)
	}
}

// A quiesce with nothing in flight still succeeds on the first call, so the
// retry behaviour above is not achieved by making quiesce always fail.
func TestQuiesceStillSucceedsWithNothingInFlight(t *testing.T) {
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

	time.Sleep(100 * time.Millisecond)
	raw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("a quiesce with nothing outstanding must succeed: %+v", envelope.Error)
	}
}

// Concurrent repeated quiesces must all fail while the callback is held, and
// must not corrupt each other: the stop function is now re-invoked, so two
// callers can be inside it at once.
func TestConcurrentQuiesceAllFailWhileCallbackHeld(t *testing.T) {
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

	const racers = 4
	results := make(chan bool, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
			results <- decodeEnvelope(t, raw).OK
		}()
	}
	wg.Wait()
	close(results)
	for ok := range results {
		if ok {
			t.Fatal("a concurrent quiesce reported success while the callback was still held")
		}
	}

	// Once it is genuinely gone, a further quiesce succeeds.
	releaseCancel()
	select {
	case <-cancelExited:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel callback did not exit")
	}
	raw, _ := handler.Handle(pluginabi.MethodPluginQuiesce, nil)
	if envelope := decodeEnvelope(t, raw); !envelope.OK {
		t.Fatalf("quiesce still failed after the callback exited: %+v", envelope.Error)
	}
}
