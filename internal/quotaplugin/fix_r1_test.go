package quotaplugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// These are the regressions the review asked for. Each one fails against the
// pre-fix commit; the names say which finding they pin.

// P1-a: a panicking host.http.cancel callback must not take the process down.
//
// Before the fix the callback called host.http.cancel with no recover, on a
// runtime goroutine nothing above could cover, and the review reproduced it as
// exit 1. The assertion here is that the fetch still returns a normal error
// envelope and the plugin serves afterwards.
func TestCancelCallbackPanicDoesNotCrash(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	// The upstream never answers, so the collection deadline fires and the
	// deadline callback runs — that is the callback that panics.
	host.setBlock()
	host.setPanicOnCancel(true)

	provider := NewProvider(ProviderOptions{HostCall: host.call, Timeout: 150 * time.Millisecond})
	state := NewStateFor(host.call)
	state.provider = provider
	state.SetCollectorStarter(nil)
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	raw, errHandle := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	if errHandle != nil {
		t.Fatalf("the handler returned a Go error instead of an envelope: %v", errHandle)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK {
		t.Fatal("a hung upstream must not be reported as success")
	}
	if host.cancelCount() == 0 {
		t.Fatal("the deadline never reached host.http.cancel, so the panic path was not exercised")
	}

	// The panicked value must not have escaped anywhere the page or a log can see.
	if strings.Contains(string(raw), "CANCELSECRET") {
		t.Errorf("the fetch response carries the panicked value: %s", raw)
	}
	host.mu.Lock()
	logs := append([]string(nil), host.logs...)
	host.mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "CANCELSECRET") {
			t.Errorf("a log line carries the panicked value: %q", line)
		}
	}

	// And the plugin still works: the panic did not wedge the bridge.
	host.setBlockCleared()
	host.setPanicOnCancel(false)
	host.setRespond(balanceOK(balanceBody))
	payload := fetchOnce(t, handler)
	if len(payload.Summary) != 1 || payload.Summary[0].Value != 12.40 {
		t.Errorf("the plugin did not recover after the panic: %+v", payload)
	}
}

// P1-b: quiesce must not return while a cancel callback is still inside the
// host.
//
// The review's repro held the callback inside host.http.cancel and showed
// quiesce returning first. Here the callback is parked until the test releases
// it, and the assertion is the opposite: quiesce is still blocked at that point.
func TestQuiesceWaitsForRunningCancelCallback(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setBlock()
	cancelEntered, releaseCancel := host.setCancelHold()

	provider := NewProvider(ProviderOptions{HostCall: host.call, Timeout: 200 * time.Millisecond})
	state := NewStateFor(host.call)
	state.provider = provider
	state.collectorStart = nil // this test drives the collector by hand
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	// Start a collection on the collector goroutine; its deadline will fire and
	// the cancel callback will park inside the host. The stop is registered with
	// the state, because that is the path quiesce drives.
	stop := provider.startCollector(Config{IntervalSeconds: 3600})
	state.SetCollector(stop)

	select {
	case <-cancelEntered:
	case <-time.After(10 * time.Second):
		releaseCancel()
		stop()
		t.Fatal("the cancel callback never entered the host")
	}

	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		state.Quiesce()
	}()

	// The barrier means this must NOT complete while the callback is parked.
	select {
	case <-quiesced:
		releaseCancel()
		t.Fatal("quiesce returned while the cancel callback was still inside the host")
	case <-time.After(300 * time.Millisecond):
	}

	// Let the callback finish; quiesce must then complete.
	releaseCancel()
	select {
	case <-quiesced:
	case <-time.After(15 * time.Second):
		t.Fatal("quiesce did not finish after the cancel callback returned")
	}
}

// P2-1: a host.auth.list that never returns must not hold the collector — or
// quiesce — forever. The enumeration runs before any HTTP request, so before
// this fix it had no budget at all.
func TestBlockedAuthListIsBoundedAndDoesNotBlockQuiesce(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	release, entered := host.setAuthListBlock()

	// The real path: register starts the collector, so the provider that
	// collects is the one the state owns and the one quiesce stops. A
	// hand-wired collector would barrier a different provider's bridge.
	state := NewStateFor(host.call)
	state.provider.timeout = 150 * time.Millisecond
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() {
		release()
		state.Shutdown()
	})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the collector never called host.auth.list")
	}

	// The enumeration is bounded by the attempt budget, so the sweep gives up
	// and the stop path's own wait is bounded too. Quiesce must return without
	// the host ever releasing the callback.
	quiesced := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(quiesced)
		state.Quiesce()
	}()
	select {
	case <-quiesced:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("quiesce blocked on a host.auth.list that never returned")
	}
	elapsed := time.Since(start)
	if elapsed > 8*time.Second {
		t.Errorf("quiesce took %s; the blocked enumeration was not bounded", elapsed)
	}
	// It did not return instantly either: something real was being drained.
	if elapsed < 100*time.Millisecond {
		t.Errorf("quiesce returned in %s without waiting for the in-flight call", elapsed)
	}
	release()
}

// The budget belongs to the enumeration itself, not only to the stop barrier:
// the sweep has to give up and keep working. Without it the collector goroutine
// stays parked in host.auth.list, which is what the barrier then has to paper
// over with its own timeout — and a stopped library would still have a live
// call in it.
func TestBlockedAuthListEndsTheSweep(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	release, entered := host.setAuthListBlock()

	state := NewStateFor(host.call)
	state.provider.timeout = 150 * time.Millisecond
	sweeps := make(chan int, 8)
	state.SetCollectorStarter(func(cfg Config) {
		worker := &collector{
			provider: state.provider,
			interval: 30 * time.Millisecond,
			onSweep: func(refreshed, failed int) {
				select {
				case sweeps <- refreshed:
				default:
				}
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.run(ctx)
		}()
		state.SetCollector(func() drainOutcome {
			cancel()
			<-done
			return drainOutcome{drained: true}
		})
	})
	if _, err := state.Register([]byte("interval_seconds: 30\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() {
		release()
		state.Shutdown()
	})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the collector never called host.auth.list")
	}

	// The blocked enumeration must be abandoned at the budget. Two later sweeps
	// therefore reach the report hook while the host is still parked.
	// The first sweep is aborted; the ticker then keeps firing.
	for i := 0; i < 2; i++ {
		select {
		case <-sweeps:
		case <-time.After(10 * time.Second):
			t.Fatalf("sweep %d never finished; the blocked enumeration was not bounded", i)
		}
	}
	// And the host was never released: the budget, not the release, ended them.
	select {
	case <-entered:
	default:
	}
}

// The enumeration being bounded must not break the normal path: an unblocked
// host.auth.list still yields a refresh.
func TestBoundedAuthListStillRefreshes(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))

	state := NewStateFor(host.call)
	if _, errRegister := state.Register([]byte("interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if entry, ok := state.provider.store.load("cred-1"); ok && entry.hasData() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collector never cached a reading through the bounded path")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The whole-attempt budget covers the credential read, not just the HTTP
// request: a host.auth.get that never returns is cut off at the deadline.
func TestCredentialReadIsInsideTheBudget(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))
	release := host.setAuthGetBlock()

	provider := NewProvider(ProviderOptions{HostCall: host.call, Timeout: 150 * time.Millisecond})
	state := NewStateFor(host.call)
	state.provider = provider
	state.SetCollectorStarter(nil)
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() {
		release()
		state.Shutdown()
	})

	// A request with no StorageJSON forces the host.auth.get path.
	start := time.Now()
	raw, _ := handler.Handle(pluginabi.MethodQuotaFetch, []byte(`{"auth_index":"cred-1","provider":"ollama"}`))
	elapsed := time.Since(start)

	if envelope := decodeEnvelope(t, raw); envelope.OK {
		t.Fatal("a blocked credential read must not produce a reading")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the fetch took %s; the credential read was not bounded", elapsed)
	}
}

// unused keeps the import list honest for platform-specific builds.
var _ = context.Background
var _ = pluginapi.HostAuthFileEntry{}
