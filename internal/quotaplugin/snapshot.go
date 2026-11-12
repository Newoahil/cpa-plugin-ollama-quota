package quotaplugin

import (
	"time"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/cmc"
	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/opc"
)

// quotaSnapshot is the provider-neutral reading this package caches and maps
// onto the host's quota model. Each provider package has its own snapshot type;
// this one is what the store, the staleness rules and the response builder work
// in, so adding a provider does not touch them.
//
// Every field means the same thing whichever provider produced it. In
// particular UsedPercent is always the share CONSUMED (0..100) and the response
// builder is the one place that turns it into a remaining fraction.
type quotaSnapshot struct {
	// provider is the provider key the reading came from; it decides the
	// QuotaGroup display name.
	provider string
	// Plan is the subscription name, when upstream reported one.
	Plan string
	// Balance is the credit figure, nil when the path reported none.
	Balance *quotaBalance
	// Windows holds the quota windows, shortest first.
	Windows []quotaWindow
	// ObservedAt is when this reading was taken.
	ObservedAt time.Time
}

// quotaBalance is a credit figure with its currency. Currency is empty when
// upstream reports credits rather than money; the metric is then rendered as a
// plain number instead of a currency.
type quotaBalance struct {
	Amount    float64
	Currency  string
	Included  *float64
	Purchased *float64
}

// quotaWindow is one quota window.
type quotaWindow struct {
	// Name is the stable window id, for example "5h", "7d" or "rolling".
	Name string
	// Label is the user-facing text.
	Label string
	// UsedPercent is the share consumed, already clamped to [0, 100].
	UsedPercent float64
	// ResetAt is when the window resets, when upstream stated it exactly.
	ResetAt *time.Time
	// ResetText is wording to display as-is when ResetAt could not be derived.
	ResetText string
	// Note carries a degradation worth showing, for example that upstream
	// reports the window rate limited. It is never an error.
	Note string
}

// hasData reports whether the snapshot carries anything displayable.
func (s quotaSnapshot) hasData() bool {
	return s.Balance != nil || len(s.Windows) > 0
}

// ollamaSnapshot converts an Ollama reading.
func ollamaSnapshot(snapshot ollama.Snapshot) quotaSnapshot {
	result := quotaSnapshot{
		provider:   QuotaIdentifier,
		Plan:       snapshot.Plan,
		ObservedAt: snapshot.ObservedAt,
	}
	if snapshot.Balance != nil {
		result.Balance = &quotaBalance{
			Amount:    snapshot.Balance.Amount,
			Currency:  snapshot.Balance.Currency,
			Included:  snapshot.Balance.Included,
			Purchased: snapshot.Balance.Purchased,
		}
	}
	for _, window := range snapshot.Windows {
		result.Windows = append(result.Windows, quotaWindow{
			Name:        window.Name,
			Label:       window.Label,
			UsedPercent: clampUsedPercent(window.UsedPercent),
			ResetAt:     window.ResetAt,
			ResetText:   window.ResetText,
		})
	}
	return result
}

// cmcSnapshot converts a Command Code reading.
func cmcSnapshot(snapshot cmc.Snapshot) quotaSnapshot {
	result := quotaSnapshot{
		provider:   CommandCodeProvider,
		Plan:       snapshot.Plan,
		ObservedAt: snapshot.ObservedAt,
	}
	if snapshot.Balance != nil {
		result.Balance = &quotaBalance{
			Amount:    snapshot.Balance.Amount,
			Currency:  snapshot.Balance.Currency,
			Included:  snapshot.Balance.Included,
			Purchased: snapshot.Balance.Purchased,
		}
	}
	for _, window := range snapshot.Windows {
		result.Windows = append(result.Windows, quotaWindow{
			Name:        window.Name,
			Label:       window.Label,
			UsedPercent: clampUsedPercent(window.UsedPercent),
			ResetAt:     window.ResetAt,
			Note:        window.Note,
		})
	}
	return result
}

// opcSnapshot converts an OpenCode Go reading.
func opcSnapshot(snapshot opc.Snapshot) quotaSnapshot {
	result := quotaSnapshot{provider: OpenCodeProvider, Plan: snapshot.Plan, ObservedAt: snapshot.ObservedAt}
	for _, window := range snapshot.Windows {
		result.Windows = append(result.Windows, quotaWindow{
			Name:        window.Name,
			Label:       window.Label,
			UsedPercent: clampUsedPercent(window.UsedPercent),
			ResetAt:     window.ResetAt,
			Note:        window.Note,
		})
	}
	return result
}

// clampUsedPercent keeps a used share inside [0, 100]. A provider that reports
// an over-cap window as a ratio above 1 therefore saturates instead of
// producing a negative remaining fraction.
func clampUsedPercent(value float64) float64 {
	if value != value || value < 0 { // NaN or negative
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// remainingFraction is the share of a window still available: 1 - used, not
// used. This is the only place the direction is decided.
func (w quotaWindow) remainingFraction() float64 {
	return (100 - clampUsedPercent(w.UsedPercent)) / 100
}
