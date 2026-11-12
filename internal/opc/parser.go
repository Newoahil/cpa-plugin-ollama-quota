package opc

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// The usage payload (observed live 2026-10-11):
//
//	{"usage":{
//	  "rolling": {"status":"rate-limited","percent":100,"resetsAt":"2026-10-11T…Z"},
//	  "weekly":  {"status":"ok","percent":42,"resetsAt":"…"},
//	  "monthly": {"status":"ok","percent":21,"resetsAt":"…"}
//	}}
//
// The endpoint is undocumented, so status is the only field that decides
// whether a window is readable:
//
//   - "ok"           → percent is the used share;
//   - "rate-limited" → the window is fully used, whatever percent says;
//   - anything else  → the window is skipped, not failed.
//
// A response with no readable window at all is a parse failure.
const (
	statusOK          = "ok"
	statusRateLimited = "rate-limited"
)

// usagePayload is the subset of the response this package reads. Percent is a
// pointer so an absent value stays distinguishable from a genuine zero.
type usagePayload struct {
	Usage map[string]*usageWindow `json:"usage"`
}

type usageWindow struct {
	Status   string   `json:"status"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resetsAt"`
}

// parseUsage maps the payload onto windows, in display order.
func parseUsage(body []byte) ([]Window, error) {
	var payload usagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, newError(KindParse, 0,
			"the OpenCode Go usage endpoint returned a response that is not valid JSON")
	}
	if payload.Usage == nil {
		return nil, newError(KindParse, 0,
			"the OpenCode Go usage endpoint returned no usage object")
	}

	windows := make([]Window, 0, len(windowOrder))
	for _, name := range windowOrder {
		window, ok := windowFromUsage(name, payload.Usage[name])
		if ok {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		return nil, newError(KindParse, 0,
			"the OpenCode Go usage endpoint returned no readable quota window")
	}
	return windows, nil
}

// windowFromUsage converts one window. It reports false when the window is
// absent, its status is unrecognised, or its percent cannot be read.
func windowFromUsage(name string, raw *usageWindow) (Window, bool) {
	if raw == nil {
		return Window{}, false
	}
	window := Window{Name: name, Label: windowLabels[name]}

	switch strings.ToLower(strings.TrimSpace(raw.Status)) {
	case statusRateLimited:
		// Upstream says the window is closed right now. Saturated to fully
		// used even when percent is missing or disagrees.
		window.UsedPercent = 100
		window.Note = "upstream reports this window rate limited"
	case statusOK:
		if raw.Percent == nil || math.IsNaN(*raw.Percent) || math.IsInf(*raw.Percent, 0) {
			return Window{}, false
		}
		window.UsedPercent = clampPercent(*raw.Percent)
	default:
		// An unrecognised status is not a reading. Skipping the window keeps
		// the others; the caller fails only when none is left.
		return Window{}, false
	}

	if reset, ok := resetTime(raw.ResetsAt); ok {
		window.ResetAt = &reset
	}
	return window, true
}

// resetTime parses the RFC3339 resetsAt. A value that does not parse is
// dropped rather than rendered as a date upstream did not state.
func resetTime(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if at, err := time.Parse(layout, trimmed); err == nil {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}

func clampPercent(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
