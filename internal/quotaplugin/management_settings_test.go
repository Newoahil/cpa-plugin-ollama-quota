package quotaplugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func settingsHandler(t *testing.T) (*Handler, *fixtureHost, string) {
	t.Helper()
	host := newFixtureHost()
	host.setRespond(balanceOK(balanceBody))
	h := NewHandlerWithHost(newStateWithoutCollector(host.call), host.call)
	dir := filepath.Join(t.TempDir(), "state")
	yaml := "state_dir: '" + dir + "'\n"
	raw, err := h.Handle(pluginabi.MethodPluginRegister, registerPayload(t, yaml))
	if err != nil || !decodeEnvelope(t, raw).OK {
		t.Fatal("register failed")
	}
	t.Cleanup(func() { h.Shutdown() })
	return h, host, dir
}

func callManagement(t *testing.T, h *Handler, method, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(pluginapi.ManagementRequest{Method: method, Path: path, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = h.Handle(pluginabi.MethodManagementHandle, raw)
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatal("management RPC failed")
	}
	var response pluginapi.ManagementResponse
	if json.Unmarshal(envelope.Result, &response) != nil {
		t.Fatal("decode response")
	}
	return response
}

func TestSettingsPageRegistrationAndPublicIsolation(t *testing.T) {
	h, _, _ := settingsHandler(t)
	raw, _ := h.Handle(pluginabi.MethodManagementRegister, nil)
	var registration pluginapi.ManagementRegistrationResponse
	if json.Unmarshal(decodeEnvelope(t, raw).Result, &registration) != nil {
		t.Fatal("decode registration")
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Menu != "Ollama 额度" || len(registration.Routes) != 3 {
		t.Fatal("missing routes/menu")
	}
	response := callManagement(t, h, "GET", pagePath, nil)
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "type=\"password\"") {
		t.Fatal("page unavailable")
	}
	if strings.Contains(string(response.Body), "https://") || strings.Contains(string(response.Body), "<script src") {
		t.Fatal("page depends on external resources")
	}
	if callManagement(t, h, "POST", pagePath, []byte("{}")).StatusCode != 404 {
		t.Fatal("public resource accepted write")
	}
	if callManagement(t, h, "GET", pagePath+"/settings", nil).StatusCode != 404 {
		t.Fatal("unknown resource exposed configuration")
	}
}

func TestSettingsSaveCollectReloadAndMask(t *testing.T) {
	h, host, dir := settingsHandler(t)
	const canary = "fixture-secret-page-7788"
	response := callManagement(t, h, "POST", settingsPath, []byte("{\"api_key\":\""+canary+"\"}"))
	if response.StatusCode != 200 {
		t.Fatalf("save status=%d", response.StatusCode)
	}
	var status settingsStatus
	if json.Unmarshal(response.Body, &status) != nil || !status.Configured || status.Balance == nil || status.Balance.Amount != 12.4 || status.CollectedAt == "" {
		t.Fatal("save did not collect balance")
	}
	if strings.Contains(string(response.Body), canary) || status.MaskedKey != "••••7788" {
		t.Fatal("key not masked")
	}
	file, err := os.Stat(filepath.Join(dir, "settings.json"))
	if err != nil || file.Mode().Perm() != 0600 {
		t.Fatal("settings permissions")
	}
	directory, err := os.Stat(dir)
	if err != nil || directory.Mode().Perm() != 0700 {
		t.Fatal("directory permissions")
	}
	fresh := NewHandlerWithHost(newStateWithoutCollector(host.call), host.call)
	defer fresh.Shutdown()
	fresh.Handle(pluginabi.MethodPluginRegister, registerPayload(t, "state_dir: '"+dir+"'\n"))
	response = callManagement(t, fresh, "GET", settingsPath, nil)
	if strings.Contains(string(response.Body), canary) || json.Unmarshal(response.Body, &status) != nil || !status.Configured {
		t.Fatal("reload lost settings or exposed key")
	}
	response = callManagement(t, fresh, "POST", refreshPath, []byte("{}"))
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &status) != nil || status.Balance == nil {
		t.Fatal("page-only refresh failed")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.files) != 0 || len(host.requests) != 2 {
		t.Fatal("page collection requires host auths")
	}
	for _, line := range host.logs {
		if strings.Contains(line, canary) {
			t.Fatal("key leaked in log")
		}
	}
}

func TestSettingsPriorityAndCollectorWithoutAuths(t *testing.T) {
	h, host, _ := settingsHandler(t)
	host.setCredential("stored", map[string]any{"type": "ollama", "api_key": "file-key"})
	request := pluginapi.QuotaFetchRequest{AuthIndex: "stored", Metadata: map[string]any{"api_key": "metadata-key"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	credential, err := h.state.provider.resolveCredential(ctx, request)
	if err != nil || credential.APIKey != "file-key" {
		t.Fatal("host file must win over metadata")
	}
	if err = h.state.provider.settings.save("page-key"); err != nil {
		t.Fatal(err)
	}
	request.StorageJSON = []byte("not-json")
	credential, err = h.state.provider.resolveCredential(ctx, request)
	if err != nil || credential.APIKey != "page-key" {
		t.Fatal("page key must win over storage")
	}
	collector := collector{provider: h.state.provider}
	refreshed, failed, recovered := collector.sweepOnceSafely(ctx)
	if refreshed != 1 || failed != 0 || recovered {
		t.Fatal("page-only sweep failed")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 1 || host.requests[0].Headers.Get("Authorization") != "Bearer page-key" {
		t.Fatal("wrong collection credential")
	}
}

func TestSettingsFailureStaleReplacementAndQuiesce(t *testing.T) {
	h, host, _ := settingsHandler(t)
	callManagement(t, h, "POST", settingsPath, []byte("{\"api_key\":\"first-key\"}"))
	host.setRespond(func(pluginapi.HTTPRequest) pluginapi.HTTPResponse { return pluginapi.HTTPResponse{StatusCode: 401} })
	response := callManagement(t, h, "POST", refreshPath, []byte("{}"))
	var status settingsStatus
	json.Unmarshal(response.Body, &status)
	if !status.Stale || status.Balance == nil || status.Error == "" {
		t.Fatal("failed refresh must mark old balance stale")
	}
	response = callManagement(t, h, "POST", settingsPath, []byte("{\"api_key\":\"second-key\"}"))
	status = settingsStatus{}
	json.Unmarshal(response.Body, &status)
	if status.Balance != nil || status.Error == "" {
		t.Fatal("replacement inherited previous account balance")
	}
	h.state.Quiesce()
	if callManagement(t, h, "POST", refreshPath, []byte("{}")).StatusCode != 503 {
		t.Fatal("quiesced plugin accepted work")
	}
}

func TestSettingsValidationAndConcurrentPersistence(t *testing.T) {
	h, _, dir := settingsHandler(t)
	for _, body := range []string{"{", "{}", "{\"api_key\":123}", "{\"api_key\":\"bad\\nkey\"}"} {
		if callManagement(t, h, "POST", settingsPath, []byte(body)).StatusCode != 400 {
			t.Fatal("invalid key accepted")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.state.provider.settings.save("concurrent-key") != nil {
				t.Error("save failed")
			}
			if _, err := h.state.provider.settings.load(); err != nil {
				t.Error("load failed")
			}
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil || !json.Valid(raw) {
		t.Fatal("corrupt settings")
	}
	if maskedKey("abc") == "abc" {
		t.Fatal("short key exposed")
	}
	if err = os.WriteFile(filepath.Join(dir, "settings.json"), []byte("invalid fixture-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	response := callManagement(t, h, "GET", settingsPath, nil)
	if response.StatusCode != 500 || strings.Contains(string(response.Body), "fixture-secret") {
		t.Fatal("corrupt settings not safely reported")
	}
}

func TestStateDirectoryConfiguration(t *testing.T) {
	for _, value := range []string{"/tmp/ollama-state", "'/tmp/with # hash'", "\"/tmp/with space\""} {
		cfg, err := ParseConfig([]byte("state_dir: " + value + "\ninterval_seconds: 60\n"))
		if err != nil || cfg.StateDir == "" || cfg.IntervalSeconds != 60 {
			t.Fatal("state_dir parse failed")
		}
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := defaultStateDir()
	if err != nil || filepath.Base(path) != "cpa-plugin-ollama-quota" {
		t.Fatal("default directory")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if _, err = defaultStateDir(); err == nil {
		t.Fatal("missing user directories must fail")
	}
}
