package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authHost is a stateful fake of the host credential callbacks: it keeps the
// physical JSON of every record, its runtime disabled flag, and the runtime
// auth index, so reconciliation can be exercised end to end (list -> get ->
// save -> list) instead of against a names-only stub.
type authHost struct {
	mu      sync.Mutex
	records []*authHostRecord
	saves   []savedAuth
	logs    []string

	// diskOnly emulates a host that has not attached its auth manager yet:
	// plain host.auth.list reads the directory and carries no id/auth_index.
	diskOnly bool
	listErr  bool
	getErr   bool
	saveErr  bool
}

type authHostRecord struct {
	name            string
	index           string
	raw             map[string]any
	runtimeDisabled bool
	runtimeOnly     bool
}

type savedAuth struct {
	name string
	json []byte
}

func newAuthHost() *authHost {
	return &authHost{}
}

func (h *authHost) call(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		if h.listErr {
			return hostErr("auth_unavailable", "auth directory unavailable"), nil
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		files := make([]pluginapi.HostAuthFileEntry, 0, len(h.records))
		for _, record := range h.records {
			entry := pluginapi.HostAuthFileEntry{
				Name:        record.name,
				Type:        stringField(record.raw, "type"),
				Provider:    stringField(record.raw, "type"),
				Label:       stringField(record.raw, "label"),
				Disabled:    record.runtimeDisabled,
				RuntimeOnly: record.runtimeOnly,
				Source:      "file",
				Path:        record.name,
			}
			if !h.diskOnly {
				entry.ID = strings.TrimSuffix(record.name, ".json")
				entry.AuthIndex = record.index
			}
			files = append(files, entry)
		}
		return hostOK(hostAuthListResponse{Files: files}), nil
	case pluginabi.MethodHostAuthGet:
		if h.getErr {
			return hostErr("auth_read_failed", "host exploded with sk-secret-in-error"), nil
		}
		var req pluginapi.HostAuthGetRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return hostErr("invalid_request", "bad get"), nil
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, record := range h.records {
			if record.index != req.AuthIndex {
				continue
			}
			raw, err := json.Marshal(record.raw)
			if err != nil {
				return hostErr("encode_failed", "bad record"), nil
			}
			return hostOK(pluginapi.HostAuthGetResponse{
				AuthIndex: record.index, Name: record.name, Path: record.name, JSON: raw,
			}), nil
		}
		return hostErr("auth_not_found", req.AuthIndex), nil
	case pluginabi.MethodHostAuthSave:
		if h.saveErr {
			return hostErr("auth_save_failed", "disk full"), nil
		}
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return hostErr("invalid_request", "bad save"), nil
		}
		var fields map[string]any
		if err := json.Unmarshal(req.JSON, &fields); err != nil {
			return hostErr("invalid_request", "bad record json"), nil
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.saves = append(h.saves, savedAuth{name: req.Name, json: append([]byte(nil), req.JSON...)})
		disabled, _ := fields["disabled"].(bool)
		for _, record := range h.records {
			if record.name != req.Name {
				continue
			}
			record.raw = fields
			record.runtimeDisabled = disabled
			return hostOK(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: req.Name}), nil
		}
		h.records = append(h.records, &authHostRecord{
			name: req.Name, index: "index-" + req.Name, raw: fields, runtimeDisabled: disabled,
		})
		return hostOK(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: req.Name}), nil
	case pluginabi.MethodHostLog:
		var req struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return hostOK(map[string]any{}), nil
		}
		h.mu.Lock()
		h.logs = append(h.logs, req.Message)
		h.mu.Unlock()
		return hostOK(map[string]any{}), nil
	case pluginabi.MethodHostHTTPDo:
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: 200,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(testCatalogJSON),
		}), nil
	}
	return hostOK(map[string]any{}), nil
}

// seedManaged stores a credential record shaped exactly like the plugin's
// materializer writes it, plus optional extra metadata.
func (h *authHost) seedManaged(t *testing.T, key string, disabled bool, extra map[string]any) *authHostRecord {
	t.Helper()
	id := authRecordID(key)
	raw := map[string]any{
		"type":    ProviderID,
		"id":      id,
		"label":   "OpenCode Go credential " + strings.TrimPrefix(id, managedAuthPrefix),
		"api_key": key,
	}
	if disabled {
		raw["disabled"] = true
	}
	for k, v := range extra {
		raw[k] = v
	}
	return h.seed(t, id+".json", raw, disabled)
}

func (h *authHost) seed(t *testing.T, name string, raw map[string]any, disabled bool) *authHostRecord {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	record := &authHostRecord{
		name:            name,
		index:           fmt.Sprintf("index-%d", len(h.records)+1),
		raw:             raw,
		runtimeDisabled: disabled,
	}
	h.records = append(h.records, record)
	return record
}

func (h *authHost) savedAuths() []savedAuth {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]savedAuth(nil), h.saves...)
}

func (h *authHost) logMessages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

func stringField(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return value
}

func reconcileYAML(keys ...string) string {
	var b strings.Builder
	b.WriteString("api-keys:\n")
	for _, key := range keys {
		b.WriteString("  - value: " + key + "\n")
	}
	return b.String()
}

func registerReconciled(t *testing.T, h *authHost, yamlText string) *Manager {
	t.Helper()
	m := NewManager(NewHostBridge(h.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("register rejected: %+v", env.Error)
	}
	return m
}

func reconfigureReconciled(t *testing.T, m *Manager, h *authHost, yamlText string) {
	t.Helper()
	resp, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(yamlText))
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure rejected: %+v", env.Error)
	}
}

func decodeSaved(t *testing.T, saved savedAuth) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(saved.json, &fields); err != nil {
		t.Fatalf("decode saved record: %v (%s)", err, saved.json)
	}
	return fields
}

// TestReconcileDisablesCredentialRemovedFromConfig is the incident: a key that
// left cfg.APIKeys must stop being schedulable, without touching the records of
// keys that stayed.
func TestReconcileDisablesCredentialRemovedFromConfig(t *testing.T) {
	h := newAuthHost()
	removeKey, keepKey := "sk-reconcile-remove", "sk-reconcile-keep"
	h.seedManaged(t, removeKey, false, map[string]any{
		"note":     "operator metadata",
		"priority": 7,
		"nested":   map[string]any{"keep": []any{"a", true}},
	})
	h.seedManaged(t, keepKey, false, nil)
	m := registerReconciled(t, h, reconcileYAML(removeKey, keepKey))
	if got := len(h.savedAuths()); got != 0 {
		t.Fatalf("register saves = %d, want 0 (state already matches config)", got)
	}

	reconfigureReconciled(t, m, h, reconcileYAML(keepKey))
	saves := h.savedAuths()
	if len(saves) != 1 {
		t.Fatalf("removed-key saves = %d, want 1: %+v", len(saves), saves)
	}
	if want := authRecordID(removeKey) + ".json"; saves[0].name != want {
		t.Fatalf("disabled record = %q, want %q", saves[0].name, want)
	}
	fields := decodeSaved(t, saves[0])
	if disabled, _ := fields["disabled"].(bool); !disabled {
		t.Fatalf("removed key not disabled: %v", fields)
	}
	// The patch must preserve everything the plugin does not own.
	if fields["note"] != "operator metadata" || fields["priority"] != float64(7) {
		t.Fatalf("metadata lost: %v", fields)
	}
	nested, _ := fields["nested"].(map[string]any)
	if _, ok := nested["keep"]; !ok {
		t.Fatalf("nested metadata lost: %v", fields)
	}
	if fields["api_key"] != removeKey {
		t.Fatalf("api key lost or rewritten: %v", fields)
	}

	// Idempotent: the same config must not write again.
	reconfigureReconciled(t, m, h, reconcileYAML(keepKey))
	if got := len(h.savedAuths()); got != 1 {
		t.Fatalf("repeat reconfigure saves = %d, want 1", got)
	}
}

// TestReconcileReenablesCredentialReturnedToConfig pins the other direction:
// the config is authoritative for managed records, so putting the key back
// makes it schedulable again without operator action.
func TestReconcileReenablesCredentialReturnedToConfig(t *testing.T) {
	h := newAuthHost()
	key := "sk-reconcile-return"
	h.seedManaged(t, key, true, nil)
	m := registerReconciled(t, h, reconcileYAML(key))
	saves := h.savedAuths()
	if len(saves) != 1 {
		t.Fatalf("returning-key saves = %d, want 1", len(saves))
	}
	fields := decodeSaved(t, saves[0])
	if _, present := fields["disabled"]; present {
		t.Fatalf("enabled record still carries a disabled flag: %v", fields)
	}
	reconfigureReconciled(t, m, h, reconcileYAML(key))
	if got := len(h.savedAuths()); got != 1 {
		t.Fatalf("repeat reconfigure saves = %d, want 1", got)
	}
}

// TestReconcileRepairsPhysicalRuntimeDisagreement makes sure the decision reads
// the physical JSON as well as the runtime flag, in both directions.
func TestReconcileRepairsPhysicalRuntimeDisagreement(t *testing.T) {
	h := newAuthHost()
	enabledKey := "sk-reconcile-runtime-stale"
	disabledKey := "sk-reconcile-disk-stale"
	stoppedKey := "sk-reconcile-stop-runtime-stale"

	enabled := h.seedManaged(t, enabledKey, false, nil)
	enabled.runtimeDisabled = true // runtime says disabled, file says enabled
	disabled := h.seedManaged(t, disabledKey, true, nil)
	disabled.runtimeDisabled = false // runtime says enabled, file says disabled
	stopped := h.seedManaged(t, stoppedKey, true, nil)
	stopped.runtimeDisabled = false // not configured: desired disabled, runtime lags

	registerReconciled(t, h, reconcileYAML(enabledKey, disabledKey))

	saves := h.savedAuths()
	if len(saves) != 3 {
		t.Fatalf("repair saves = %d, want 3: %+v", len(saves), saves)
	}
	byName := map[string]map[string]any{}
	for _, saved := range saves {
		byName[saved.name] = decodeSaved(t, saved)
	}
	for _, key := range []string{enabledKey, disabledKey} {
		fields := byName[authRecordID(key)+".json"]
		if _, present := fields["disabled"]; present {
			t.Fatalf("configured credential %q not repaired to enabled: %v", key, fields)
		}
	}
	fields := byName[authRecordID(stoppedKey)+".json"]
	if disabledFlag, _ := fields["disabled"].(bool); !disabledFlag {
		t.Fatalf("unconfigured credential not repaired to disabled: %v", fields)
	}
}

// TestReconcileDefersWithoutRuntimeIndex pins the bootstrap contract: a host
// that lists from disk has no auth_index, so nothing may be written on a guess;
// the next reconfigure with indices converges.
func TestReconcileDefersWithoutRuntimeIndex(t *testing.T) {
	h := newAuthHost()
	removedKey, keepKey := "sk-reconcile-defer-removed", "sk-reconcile-defer-keep"
	h.seedManaged(t, removedKey, false, nil)
	h.seedManaged(t, keepKey, false, nil)
	h.diskOnly = true

	m := registerReconciled(t, h, reconcileYAML(keepKey))
	if got := len(h.savedAuths()); got != 0 {
		t.Fatalf("disk-only register saves = %d, want 0", got)
	}
	if !logsContain(h.logMessages(), "reconciliation deferred for 1 record") {
		t.Fatalf("deferred diagnostic missing: %v", h.logMessages())
	}

	// The host attaches its auth manager and applies the config again.
	h.diskOnly = false
	reconfigureReconciled(t, m, h, reconcileYAML(keepKey))
	saves := h.savedAuths()
	if len(saves) != 1 || saves[0].name != authRecordID(removedKey)+".json" {
		t.Fatalf("indexed reconfigure saves = %+v, want the removed key only", saves)
	}
}

// TestReconcileNeverRewritesUnownedRecord covers the ownership boundary: a
// canonical file name is not permission to overwrite somebody else's record.
func TestReconcileNeverRewritesUnownedRecord(t *testing.T) {
	t.Run("digest mismatch", func(t *testing.T) {
		h := newAuthHost()
		nameKey, foreignKey := "sk-reconcile-owner", "sk-reconcile-foreign"
		id := authRecordID(nameKey)
		h.seed(t, id+".json", map[string]any{
			"type": ProviderID, "id": id, "api_key": foreignKey,
		}, false)
		registerReconciled(t, h, reconcileYAML(nameKey))
		if got := len(h.savedAuths()); got != 0 {
			t.Fatalf("unowned record rewritten: %+v", h.savedAuths())
		}
		if !logsContain(h.logMessages(), "not plugin-managed") {
			t.Fatalf("ownership diagnostic missing: %v", h.logMessages())
		}
	})

	t.Run("id mismatch", func(t *testing.T) {
		h := newAuthHost()
		key := "sk-reconcile-id-mismatch"
		id := authRecordID(key)
		h.seed(t, id+".json", map[string]any{
			"type": ProviderID, "id": "someone-elses-id", "api_key": key,
		}, false)
		registerReconciled(t, h, reconcileYAML(key))
		if got := len(h.savedAuths()); got != 0 {
			t.Fatalf("record with foreign id rewritten: %+v", h.savedAuths())
		}
	})

	t.Run("runtime only", func(t *testing.T) {
		h := newAuthHost()
		removedKey, keepKey := "sk-reconcile-runtime-removed", "sk-reconcile-runtime-keep"
		h.seedManaged(t, removedKey, false, nil)
		h.seedManaged(t, keepKey, false, nil)
		h.mu.Lock()
		h.records[0].runtimeOnly = true
		h.mu.Unlock()
		registerReconciled(t, h, reconcileYAML(keepKey))
		if got := len(h.savedAuths()); got != 0 {
			t.Fatalf("runtime-only credential materialized into a file: %+v", h.savedAuths())
		}
	})
}

// TestReconcileSurfacesFailures keeps a failed read or write visible instead of
// pretending the pool matches the config.
func TestReconcileSurfacesFailures(t *testing.T) {
	t.Run("get failure", func(t *testing.T) {
		h := newAuthHost()
		key := "sk-reconcile-get-failure"
		h.seedManaged(t, key, false, nil)
		h.getErr = true
		m := NewManager(NewHostBridge(h.call))
		t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
		resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(reconcileYAML(key)))
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_materialization_failed" {
			t.Fatalf("get failure envelope = %+v", env.Error)
		}
		if strings.Contains(env.Error.Message, "sk-secret-in-error") {
			t.Fatalf("host error text leaked into envelope: %s", env.Error.Message)
		}
		if got := len(h.savedAuths()); got != 0 {
			t.Fatalf("get failure still wrote %d record(s)", got)
		}
	})

	t.Run("save failure", func(t *testing.T) {
		h := newAuthHost()
		removedKey, keepKey := "sk-reconcile-save-removed", "sk-reconcile-save-keep"
		h.seedManaged(t, removedKey, false, nil)
		h.seedManaged(t, keepKey, false, nil)
		// Start converged, then make the first required write fail.
		m := registerReconciled(t, h, reconcileYAML(removedKey, keepKey))
		h.saveErr = true
		resp, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(reconcileYAML(keepKey)))
		if err != nil {
			t.Fatalf("reconfigure: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_materialization_failed" {
			t.Fatalf("save failure envelope = %+v", env.Error)
		}
	})
}

// TestReconcileConvergesAfterManagerRestart pins idempotency across a fresh
// manager: identical state must not produce writes.
func TestReconcileConvergesAfterManagerRestart(t *testing.T) {
	h := newAuthHost()
	activeKey, retiredKey := "sk-reconcile-restart-active", "sk-reconcile-restart-retired"
	h.seedManaged(t, activeKey, false, nil)
	h.seedManaged(t, retiredKey, true, nil)
	for i := 0; i < 2; i++ {
		m := NewManager(NewHostBridge(h.call))
		resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(reconcileYAML(activeKey)))
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		if env := decodeEnv(t, resp); !env.OK {
			t.Fatalf("register %d rejected: %+v", i, env.Error)
		}
		if _, err := m.HandleCall("plugin.shutdown", nil); err != nil {
			t.Fatalf("shutdown %d: %v", i, err)
		}
	}
	if got := len(h.savedAuths()); got != 0 {
		t.Fatalf("converged state still wrote %d record(s)", got)
	}
}

// TestBridgeAuthGetWireAndSanitizesFailures pins the bridge contract.
func TestBridgeAuthGetWireAndSanitizesFailures(t *testing.T) {
	calls := 0
	bridge := NewHostBridge(func(method string, payload []byte) ([]byte, error) {
		calls++
		if method != pluginabi.MethodHostAuthGet {
			t.Fatalf("method = %q", method)
		}
		var req pluginapi.HostAuthGetRequest
		if err := json.Unmarshal(payload, &req); err != nil || req.AuthIndex != "idx-1" {
			t.Fatalf("payload = %s (%v)", payload, err)
		}
		return hostOK(pluginapi.HostAuthGetResponse{
			AuthIndex: "idx-1", Name: "x.json", Path: "x.json", JSON: json.RawMessage(`{"api_key":"sk-secret"}`),
		}), nil
	})
	resp, err := bridge.AuthGet(t.Context(), "idx-1")
	if err != nil {
		t.Fatalf("AuthGet: %v", err)
	}
	if resp.Name != "x.json" || !strings.Contains(string(resp.JSON), "sk-secret") {
		t.Fatalf("decoded response = %+v", resp)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}

	failing := NewHostBridge(func(string, []byte) ([]byte, error) {
		return hostErr("auth_read_failed", "upstream echoed sk-secret-in-error"), nil
	})
	if _, err := failing.AuthGet(t.Context(), "idx-1"); err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("failure text = %v", err)
	}
}

func logsContain(logs []string, want string) bool {
	for _, message := range logs {
		if strings.Contains(message, want) {
			return true
		}
	}
	return false
}
