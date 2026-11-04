package quotaplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type settingsStatus struct {
	Configured  bool            `json:"configured"`
	MaskedKey   string          `json:"masked_key,omitempty"`
	CollectedAt string          `json:"collected_at,omitempty"`
	Balance     *ollama.Balance `json:"balance,omitempty"`
	Error       string          `json:"error,omitempty"`
	Stale       bool            `json:"stale"`
}

func managementJSON(code int, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return OKEnvelope(pluginapi.ManagementResponse{
		StatusCode: code,
		Headers:    http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}},
		Body:       body,
	})
}

func (h *Handler) handleSettings(req pluginapi.ManagementRequest) ([]byte, error) {
	provider, ok := h.state.Provider()
	if !ok {
		return managementJSON(503, map[string]string{"error": "plugin is quiescing"})
	}
	ctx, release, ok := provider.Track(h.ctx)
	if !ok {
		return managementJSON(503, map[string]string{"error": "plugin is quiescing"})
	}
	defer release()
	budget, cancel := context.WithTimeout(ctx, provider.attemptTimeout())
	defer cancel()

	if req.Method == http.MethodPost && req.Path == settingsPath {
		var input savedSettings
		if len(req.Body) > 16384 || json.Unmarshal(req.Body, &input) != nil {
			return managementJSON(400, map[string]string{"error": "invalid settings request"})
		}
		input.APIKey = strings.TrimSpace(input.APIKey)
		if input.APIKey == "" || strings.ContainsAny(input.APIKey, "\r\n") {
			return managementJSON(400, map[string]string{"error": "API Key is required and must be a single line"})
		}
		if budget.Err() != nil {
			return managementJSON(503, map[string]string{"error": "plugin is quiescing"})
		}
		if provider.settings.save(input.APIKey) != nil {
			return managementJSON(500, map[string]string{"error": errSettings.Error()})
		}
	}
	key, err := provider.settings.load()
	if err != nil {
		return managementJSON(500, map[string]string{"error": errSettings.Error()})
	}
	result := settingsStatus{Configured: key != "", MaskedKey: maskedKey(key)}
	if req.Method == http.MethodPost {
		if key == "" {
			return managementJSON(400, map[string]string{"error": "configure an API Key first"})
		}
		if _, err := provider.fetchQuotaWithinBudget(budget, pluginapi.QuotaFetchRequest{Provider: QuotaIdentifier}, ""); err != nil {
			// Fixed text avoids exposing upstream-derived strings or key material.
			result.Error = "采集失败，请检查 API Key 或稍后重试"
		}
	}
	if key != "" {
		if entry, exists := provider.store.load(configuredReadingKey(key)); exists {
			if !entry.observedAt.IsZero() {
				result.CollectedAt = entry.observedAt.UTC().Format(time.RFC3339)
			}
			result.Balance = entry.snapshot.Balance
			if entry.lastErr != nil {
				result.Error = "采集失败，请检查 API Key 或稍后重试"
				if entry.hasData() { result.Error = "采集失败，已保留上次结果" }
				result.Stale = entry.hasData()
			}
		}
	}
	return managementJSON(http.StatusOK, result)
}
