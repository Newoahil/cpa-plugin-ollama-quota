package quotaplugin

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	settingsPath = "/v0/management/plugins/ollama-quota/settings"
	refreshPath  = "/v0/management/plugins/ollama-quota/refresh"
	pagePath     = "/v0/resource/plugins/ollama-quota/settings"
)

//go:embed settings.html
var settingsPage []byte

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: settingsPath},
			{Method: http.MethodPost, Path: settingsPath},
			{Method: http.MethodPost, Path: refreshPath},
		},
		Resources: []pluginapi.ResourceRoute{{
			Path: "/settings", Menu: "Ollama 额度", Description: "配置 Ollama API Key 并查看余额",
		}},
	}
}

func (h *Handler) handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := decodeOptional(raw, &req); err != nil {
		return ErrorEnvelope("invalid_request", "invalid management request"), nil
	}
	if !h.state.Active() {
		return OKEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusServiceUnavailable})
	}
	if req.Path == pagePath && req.Method == http.MethodGet {
		return OKEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}},
			Body:       settingsPage,
		})
	}
	if (req.Path == settingsPath && (req.Method == http.MethodGet || req.Method == http.MethodPost)) || (req.Path == refreshPath && req.Method == http.MethodPost) {
		return h.handleSettings(req)
	}
	return OKEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound})
}
