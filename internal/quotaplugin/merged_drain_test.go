package quotaplugin

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Release the two independent work sets in either order. Neither release may
// let quiesce succeed while the other set still owns work in the library.
func TestQuiesceWaitsForBothWorkSets(t *testing.T) {
	for _, first := range []string{"fetch", "callback"} {
		t.Run(first, func(t *testing.T) {
			s := newStateWithoutCollector(nil)
			if _, err := s.Register(nil); err != nil {
				t.Fatal(err)
			}
			ctx, release, _ := s.provider.Track(context.Background())
			defer release()
			bridge := s.provider.bridge()
			bridge.inflight.begin()
			defer func() {
				if bridge.inflight.pending() > 0 {
					bridge.inflight.end()
				}
			}()
			done := make(chan error, 1)
			go func() { _, err := s.Quiesce(); done <- err }()
			<-ctx.Done()
			if first == "fetch" {
				release()
			} else {
				bridge.inflight.end()
			}
			select {
			case err := <-done:
				t.Fatalf("returned before both work sets released: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if first == "fetch" {
				bridge.inflight.end()
			} else {
				release()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("released work did not drain")
			}
			if s.LastDrain().TimedOut {
				t.Fatal("clean drain reported timeout")
			}
		})
	}
}

func TestQuiesceCombinesFailureReportAndRetries(t *testing.T) {
	s := newStateWithoutCollector(nil)
	_, _ = s.Register(nil)
	_, release, _ := s.provider.Track(nil)
	defer release()
	clean := false
	s.SetCollector(func() drainOutcome { return drainOutcome{drained: clean, pending: 2} })
	_, err := s.Quiesce()
	var report DrainReport
	if !errors.As(err, &report) || !errors.Is(err, ErrQuiesceIncomplete) {
		t.Fatalf("missing combined failure: %v", err)
	}
	if !report.TimedOut || report.InFlight != 1 || report.HostPending != 2 || report != s.LastDrain() {
		t.Fatalf("report = %+v, last = %+v", report, s.LastDrain())
	}
	release()
	clean = true
	if _, err := s.Quiesce(); err != nil {
		t.Fatal(err)
	}
	if s.LastDrain().TimedOut {
		t.Fatal("retry retained stale timeout")
	}
}
