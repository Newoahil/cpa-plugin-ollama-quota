package quotaplugin

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// contextWithCancel keeps the collector tests readable.
func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// waitSweep waits for the next collector sweep, failing the test if the timer
// never fires.
func waitSweep(t *testing.T, sweeps <-chan int) int {
	t.Helper()
	select {
	case count := <-sweeps:
		return count
	case <-time.After(5 * time.Second):
		t.Fatal("the collector did not sweep")
		return 0
	}
}

// The number of goroutines must not grow as refreshes come and go. This is what
// the ticket asks for and what keeps the plugin from leaking goroutines inside
// the CPA process (SPEC 4.4).
//
// The measurement includes the collector goroutine that exists because of the
// register, so it is deliberately taken only after the collector has started and
// its first sweep has finished.
func TestCollectorGoroutineCountIsStable(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))

	handler := NewHandlerWithHost(NewStateFor(host.call), host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "interval_seconds: 3600\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { handler.state.Shutdown() })

	// Give the collector's immediate first sweep time to finish, then take the
	// baseline.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	for i := 0; i < 30; i++ {
		raw, _ := handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
		if envelope := decodeEnvelope(t, raw); !envelope.OK {
			// The fetch may legitimately come from the cache, but it must not
			// be an error at this point.
			t.Fatalf("fetch %d failed: %+v", i, envelope.Error)
		}
	}

	// Let anything asynchronous settle before counting.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// A small tolerance: the Go runtime and the testing package have their own
	// transient goroutines. The failure this guards against is growth, so one
	// or two is noise and thirty is not.
	if after > baseline+2 {
		t.Fatalf("goroutines grew from %d to %d across 30 refreshes", baseline, after)
	}
}

// Quiesce must actually stop the timer: no sweep may arrive after it returns.
func TestQuiesceStopsTheCollector(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))

	provider := NewProvider(ProviderOptions{HostCall: host.call})
	state := NewStateFor(host.call)
	state.provider = provider

	sweeps := make(chan int, 16)
	state.SetCollectorStarter(func(cfg Config) {
		worker := &collector{
			provider: provider,
			interval: 20 * time.Millisecond,
			onSweep: func(refreshed, failed int) {
				select {
				case sweeps <- refreshed:
				default:
				}
			},
		}
		ctx, cancel := contextWithCancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.run(ctx)
		}()
		state.SetCollector(func() {
			cancel()
			<-done
		})
	})

	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Prove the timer is live.
	waitSweep(t, sweeps)

	state.Quiesce()

	// Drain anything already queued, then confirm nothing new arrives.
	for {
		select {
		case <-sweeps:
			continue
		default:
		}
		break
	}
	select {
	case <-sweeps:
		t.Fatal("the collector swept after quiesce returned")
	case <-time.After(150 * time.Millisecond):
	}
}

// The collector refreshes every Ollama credential it finds and caches the
// figures for later reads, so the page has data before anyone asks for it.
func TestCollectorRefreshesCredentialsAndCachesReadings(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))

	state := NewStateFor(host.call)
	if _, err := state.Register([]byte("enabled: true\ninterval_seconds: 3600\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { state.Shutdown() })

	// The collector's first sweep runs immediately; wait for the cached reading.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := state.provider.store.load("cred-1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the collector never cached a reading")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Read what the collector cached. The check is on the stored entry and its
	// mapping: going through fetchQuota would start a fresh collection by
	// design (the cache is served only inside a backoff window or after a
	// failure), so a request count there would prove nothing about the cache.
	entry, ok := state.provider.store.load("cred-1")
	if !ok {
		t.Fatal("the collector cached nothing")
	}
	response := quotaResponse(entry, "")
	if len(response.Summary) != 1 || response.Summary[0].Value != 12.40 {
		t.Errorf("cached balance = %+v, want 12.4", response.Summary)
	}
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 2 {
		t.Errorf("cached buckets = %+v, want the two windows the collector read", response.Groups)
	}
	if host.requestCount() == 0 {
		t.Error("the reading was not collected from upstream")
	}
}

// Quiesce must abort a fetch that is in flight, not just stop taking new ones.
// The fetch is parked on a host HTTP call; the quiesce cancels the tracked
// context, and the test asserts the call returns promptly instead of hanging
// until the collection deadline.
func TestQuiesceAbortsInFlightHostCall(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setBlock()

	// A long deadline proves the return is caused by the quiesce, not by the
	// timeout.
	state := newFetchTestState(host)
	state.provider = NewProvider(ProviderOptions{HostCall: host.call, Timeout: 30 * time.Second})
	handler := NewHandlerWithHost(state, host.call)
	if _, errRegister := handler.Handle(pluginabi.MethodPluginRegister, registerPayload(t, enabledConfig)); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	t.Cleanup(func() { state.Shutdown() })

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_, _ = handler.Handle(pluginabi.MethodQuotaFetch, fetchRequest(t, "cred-1", "ollama"))
	}()

	// Wait until the fetch is parked inside the host HTTP call.
	deadline := time.Now().Add(5 * time.Second)
	for host.requestCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the fetch never reached the host")
		}
		time.Sleep(5 * time.Millisecond)
	}

	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		if _, errQuiesce := handler.Handle(pluginabi.MethodPluginQuiesce, nil); errQuiesce != nil {
			t.Errorf("quiesce: %v", errQuiesce)
		}
	}()

	select {
	case <-quiesced:
	case <-time.After(5 * time.Second):
		t.Fatal("quiesce waited for the in-flight fetch instead of cancelling it")
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight fetch did not return after quiesce")
	}
}

// A panic in the collector's own sweep is recovered and does not stop the
// timer: later sweeps still arrive.
func TestCollectorSurvivesPanicInSweep(t *testing.T) {
	host := newFixtureHost()
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	host.setRespond(balanceOK(balanceBody))

	provider := NewProvider(ProviderOptions{HostCall: host.call})
	state := NewStateFor(host.call)
	state.provider = provider

	sweeps := make(chan int, 32)
	state.SetCollectorStarter(func(cfg Config) {
		worker := &collector{
			provider: provider,
			interval: 20 * time.Millisecond,
			onSweep: func(refreshed, failed int) {
				select {
				case sweeps <- refreshed:
				default:
				}
			},
		}
		ctx, cancel := contextWithCancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.run(ctx)
		}()
		state.SetCollector(func() {
			cancel()
			<-done
		})
	})

	if _, err := state.Register([]byte("enabled: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { state.Shutdown() })

	// The first sweep succeeds.
	waitSweep(t, sweeps)

	// Break the next sweep: host.auth.list panics. The collector's per-sweep
	// recovery turns that into a logged warning and keeps the timer going.
	host.setPanicOnListAuths(true)
	time.Sleep(80 * time.Millisecond)
	host.setPanicOnListAuths(false)

	// Drain the queued panic-time sweeps, then prove the timer is still alive.
	for len(sweeps) > 0 {
		<-sweeps
	}
	if host.logCount() == 0 {
		t.Error("the recovered panic was not recorded")
	}
	waitSweep(t, sweeps)
}

// host.auth.list falls back to a bare directory listing before the auth manager
// is wired, and that listing carries no id and no auth_index. Such an entry
// cannot be read back, so it is skipped: it must not be counted as a failure and
// must not stop the sweep from servicing the entries that do have an index.
func TestCollectorSkipsUnaddressableEntries(t *testing.T) {
	host := newFixtureHost()
	// An addressable credential.
	host.setCredential("cred-1", map[string]any{"type": "ollama", "api_key": "fixture-key"})
	// The shape listAuthFilesFromDisk produces: provider and name only.
	host.addListedEntry(pluginapi.HostAuthFileEntry{
		Name:     "ollama-disk.json",
		Provider: QuotaIdentifier,
		Type:     QuotaIdentifier,
		Source:   "file",
	})
	host.setRespond(balanceOK(balanceBody))

	state := NewStateFor(host.call)
	if _, err := state.Register([]byte("enabled: true\ninterval_seconds: 3600\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { state.Shutdown() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := state.provider.store.load("cred-1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the addressable credential was never refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The unaddressable entry produced no warning about being unreadable.
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, line := range host.logs {
		if strings.Contains(line, "could not read a credential") {
			t.Errorf("an entry with no index was reported as unreadable: %q", line)
		}
	}
}
