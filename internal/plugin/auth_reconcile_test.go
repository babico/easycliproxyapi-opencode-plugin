package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-go-cliproxyapi/internal/config"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authHost is a stateful fake of the host credential callbacks: it keeps the
// physical JSON of every record, its runtime disabled flag, and the runtime
// auth index, so reconciliation can be exercised end to end (list -> get ->
// save -> list) instead of against a names-only stub.
type authHost struct {
	mu      sync.Mutex
	dir     string // auth directory backing the records
	records []*authHostRecord
	saves   []savedAuth
	logs    []string
	// saveGate, when non-nil, parks every save callback until it is closed:
	// it reproduces a save that outlives its deadline.
	saveGate chan struct{}

	// diskOnly emulates a host that has not attached its auth manager yet:
	// plain host.auth.list reads the directory and carries no id/auth_index.
	diskOnly bool
	listErr  bool
	getErr   bool
	saveErr  bool
}

type authHostRecord struct {
	name            string
	path            string
	index           string
	raw             map[string]any
	runtimeDisabled bool
	runtimeOnly     bool
	// listedID, when non-empty, is what host.auth.list reports instead of the
	// canonical id (host input the plugin must not trust or echo).
	listedID string
}

type savedAuth struct {
	name string
	json []byte
}

func newAuthHost(t *testing.T) *authHost {
	t.Helper()
	return &authHost{dir: t.TempDir()}
}

// pathOf is the on-disk location of a seeded/created record.
func (h *authHost) pathOf(name string) string { return filepath.Join(h.dir, name) }

func (h *authHost) fileExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := os.Stat(h.pathOf(name))
	return err == nil
}

func writeAuthFile(t *testing.T, path string, raw map[string]any) {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode auth file: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
}

func (h *authHost) call(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		if h.listErr {
			return hostErr("auth_unavailable", "auth directory unavailable"), nil
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		kept := h.records[:0]
		for _, record := range h.records {
			if _, err := os.Stat(record.path); err == nil {
				kept = append(kept, record)
			}
		}
		h.records = kept
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
				Path:        record.path,
			}
			if !h.diskOnly {
				entry.ID = strings.TrimSuffix(record.name, ".json")
				if record.listedID != "" {
					entry.ID = record.listedID
				}
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
				AuthIndex: record.index, Name: record.name, Path: record.path, JSON: raw,
			}), nil
		}
		return hostErr("auth_not_found", req.AuthIndex), nil
	case pluginabi.MethodHostAuthSave:
		h.mu.Lock()
		gate := h.saveGate
		h.mu.Unlock()
		if gate != nil {
			<-gate
		}
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
		path := h.pathOf(req.Name)
		data, errEncode := json.Marshal(fields)
		if errEncode != nil {
			return hostErr("encode_failed", "bad record"), nil
		}
		if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
			return hostErr("write_failed", "cannot write auth file"), nil
		}
		for _, record := range h.records {
			if record.name != req.Name {
				continue
			}
			record.raw = fields
			record.path = path
			record.runtimeDisabled = disabled
			return hostOK(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: path}), nil
		}
		h.records = append(h.records, &authHostRecord{
			name: req.Name, path: path, index: "index-" + req.Name, raw: fields, runtimeDisabled: disabled,
		})
		return hostOK(pluginapi.HostAuthSaveResponse{Name: req.Name, Path: path}), nil
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
	path := h.pathOf(name)
	writeAuthFile(t, path, raw)
	record := &authHostRecord{
		name:            name,
		path:            path,
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

// TestReconcileRemovesCredentialDroppedFromConfig is the incident: a key that
// left cfg.APIKeys must stop being schedulable, and its record must be gone
// rather than left behind as a disabled credential.
func TestReconcileRemovesCredentialDroppedFromConfig(t *testing.T) {
	h := newAuthHost(t)
	removeKey, keepKey := "sk-reconcile-remove", "sk-reconcile-keep"
	removedName := authRecordID(removeKey) + ".json"
	keptName := authRecordID(keepKey) + ".json"
	h.seedManaged(t, removeKey, false, nil)
	h.seedManaged(t, keepKey, false, nil)
	m := registerReconciled(t, h, reconcileYAML(removeKey, keepKey))
	if got := len(h.savedAuths()); got != 0 {
		t.Fatalf("register saves = %d, want 0 (state already matches config)", got)
	}

	reconfigureReconciled(t, m, h, reconcileYAML(keepKey))
	if got := len(h.savedAuths()); got != 0 {
		t.Fatalf("removal wrote %d record(s), want a plain unlink: %+v", got, h.savedAuths())
	}
	if h.fileExists(t, removedName) {
		t.Fatalf("removed credential still on disk: %s", removedName)
	}
	if !h.fileExists(t, keptName) {
		t.Fatalf("configured credential was deleted: %s", keptName)
	}
	if !logsContain(h.logMessages(), "removed: no longer listed in configured api-keys") {
		t.Fatalf("removal diagnostic missing: %v", h.logMessages())
	}
	// Idempotent: the same config must not touch anything again.
	reconfigureReconciled(t, m, h, reconcileYAML(keepKey))
	if got := len(h.savedAuths()); got != 0 {
		t.Fatalf("repeat reconfigure wrote %d record(s)", got)
	}
	h.mu.Lock()
	remaining := len(h.records)
	h.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("records after removal = %d, want 1", remaining)
	}
}

// TestReconcileReenablesCredentialReturnedToConfig pins the other direction:
// the config is authoritative for managed records, so putting the key back
// makes it schedulable again without operator action.
func TestReconcileReenablesCredentialReturnedToConfig(t *testing.T) {
	h := newAuthHost(t)
	key := "sk-reconcile-return"
	h.seedManaged(t, key, true, map[string]any{
		"note":     "operator metadata",
		"priority": 7,
		"nested":   map[string]any{"keep": []any{"a", true}},
	})
	m := registerReconciled(t, h, reconcileYAML(key))
	saves := h.savedAuths()
	if len(saves) != 1 {
		t.Fatalf("returning-key saves = %d, want 1", len(saves))
	}
	fields := decodeSaved(t, saves[0])
	if _, present := fields["disabled"]; present {
		t.Fatalf("enabled record still carries a disabled flag: %v", fields)
	}
	// The enable patch must preserve everything the plugin does not own.
	if fields["note"] != "operator metadata" || fields["priority"] != float64(7) {
		t.Fatalf("metadata lost: %v", fields)
	}
	nested, _ := fields["nested"].(map[string]any)
	if _, ok := nested["keep"]; !ok {
		t.Fatalf("nested metadata lost: %v", fields)
	}
	if fields["api_key"] != key {
		t.Fatalf("api key lost or rewritten: %v", fields)
	}
	reconfigureReconciled(t, m, h, reconcileYAML(key))
	if got := len(h.savedAuths()); got != 1 {
		t.Fatalf("repeat reconfigure saves = %d, want 1", got)
	}
}

// TestReconcileRepairsPhysicalRuntimeDisagreement makes sure the decision reads
// the physical JSON as well as the runtime flag, in both directions.
func TestReconcileRepairsPhysicalRuntimeDisagreement(t *testing.T) {
	h := newAuthHost(t)
	enabledKey := "sk-reconcile-runtime-stale"
	disabledKey := "sk-reconcile-disk-stale"
	stoppedKey := "sk-reconcile-stop-runtime-stale"

	enabled := h.seedManaged(t, enabledKey, false, nil)
	enabled.runtimeDisabled = true // runtime says disabled, file says enabled
	disabled := h.seedManaged(t, disabledKey, true, nil)
	disabled.runtimeDisabled = false // runtime says enabled, file says disabled
	// Not configured at all: the record must go, regardless of its flags.
	stopped := h.seedManaged(t, stoppedKey, true, nil)
	stopped.runtimeDisabled = false

	registerReconciled(t, h, reconcileYAML(enabledKey, disabledKey))

	saves := h.savedAuths()
	if len(saves) != 2 {
		t.Fatalf("repair saves = %d, want 2 (the two configured keys): %+v", len(saves), saves)
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
	if h.fileExists(t, authRecordID(stoppedKey)+".json") {
		t.Fatalf("unconfigured credential was not removed")
	}
}

// TestReconcileDefersWithoutRuntimeIndex pins the bootstrap contract: a host
// that lists from disk has no auth_index, so nothing may be written on a guess;
// the next reconfigure with indices converges.
func TestReconcileDefersWithoutRuntimeIndex(t *testing.T) {
	h := newAuthHost(t)
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
	if got := len(h.savedAuths()); got != 0 {
		t.Fatalf("indexed reconfigure wrote %d record(s), want a plain unlink", got)
	}
	if h.fileExists(t, authRecordID(removedKey)+".json") {
		t.Fatalf("deferred record was never removed after the indexed pass")
	}
	if !h.fileExists(t, authRecordID(keepKey)+".json") {
		t.Fatalf("configured record was removed")
	}
}

// TestReconcileNeverRewritesUnownedRecord covers the ownership boundary: a
// canonical file name is not permission to overwrite somebody else's record.
func TestReconcileNeverRewritesUnownedRecord(t *testing.T) {
	t.Run("digest mismatch", func(t *testing.T) {
		h := newAuthHost(t)
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
		if !h.fileExists(t, id+".json") {
			t.Fatalf("unowned record was removed")
		}
	})

	t.Run("id mismatch", func(t *testing.T) {
		h := newAuthHost(t)
		key := "sk-reconcile-id-mismatch"
		id := authRecordID(key)
		record := h.seed(t, id+".json", map[string]any{
			"type": ProviderID, "id": id, "api_key": key,
		}, false)
		// The listed id is host input that may have been copied from credential
		// JSON; it must never reach the log.
		record.listedID = "sk-secret-in-listed-id"
		registerReconciled(t, h, reconcileYAML(key))
		if got := len(h.savedAuths()); got != 0 {
			t.Fatalf("record with foreign id rewritten: %+v", h.savedAuths())
		}
		logs := h.logMessages()
		if !logsContain(logs, "does not match the canonical file name") {
			t.Fatalf("identity diagnostic missing: %v", logs)
		}
		if logsContain(logs, "sk-secret-in-listed-id") {
			t.Fatalf("untrusted listed id leaked into logs: %v", logs)
		}
		if !h.fileExists(t, id+".json") {
			t.Fatalf("record with a mismatched listed id was removed")
		}
	})

	t.Run("runtime only", func(t *testing.T) {
		h := newAuthHost(t)
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
		if !h.fileExists(t, authRecordID(removedKey)+".json") {
			t.Fatalf("runtime-only record's backing file was removed")
		}
	})
}

// TestReconcileSurfacesFailures keeps a failed read or write visible instead of
// pretending the pool matches the config.
func TestReconcileSurfacesFailures(t *testing.T) {
	t.Run("get failure", func(t *testing.T) {
		h := newAuthHost(t)
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
		h := newAuthHost(t)
		key := "sk-reconcile-save-failure"
		// A configured record that must be re-enabled makes the save required.
		h.seedManaged(t, key, true, nil)
		h.saveErr = true
		m := NewManager(NewHostBridge(h.call))
		t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
		resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(reconcileYAML(key)))
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_materialization_failed" {
			t.Fatalf("save failure envelope = %+v", env.Error)
		}
		if !h.fileExists(t, authRecordID(key)+".json") {
			t.Fatalf("failed save deleted the credential")
		}
	})

	t.Run("delete failure", func(t *testing.T) {
		h := newAuthHost(t)
		key := "sk-reconcile-delete-failure"
		record := h.seedManaged(t, key, false, nil)
		// Replace the backing file with a directory: the unlink must refuse
		// rather than remove something that is not the credential file.
		dirPath := record.path
		if err := os.Remove(dirPath); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := os.Mkdir(dirPath, 0o700); err != nil {
			t.Fatalf("prepare dir: %v", err)
		}
		m := NewManager(NewHostBridge(h.call))
		t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
		resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(reconcileYAML("sk-reconcile-delete-other")))
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "auth_materialization_failed" ||
			!strings.Contains(env.Error.Message, "remove credential") {
			t.Fatalf("delete failure envelope = %+v", env.Error)
		}
		if info, errStat := os.Stat(dirPath); errStat != nil || !info.IsDir() {
			t.Fatalf("non-regular path was disturbed: %v", errStat)
		}
	})
}

// TestReconcileConvergesAfterManagerRestart pins idempotency across a fresh
// manager: the retired record is removed once and identical state produces no
// writes afterwards.
func TestReconcileConvergesAfterManagerRestart(t *testing.T) {
	h := newAuthHost(t)
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
	if h.fileExists(t, authRecordID(retiredKey)+".json") {
		t.Fatalf("retired credential survived reconciliation")
	}
	if !h.fileExists(t, authRecordID(activeKey)+".json") {
		t.Fatalf("configured credential disappeared")
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

// TestReconcileWaitsForOutstandingSave pins the unknown-commit rule: a save
// that timed out keeps running, so the next pass must not read the old state
// and report success while that write can still land behind it.
func TestReconcileWaitsForOutstandingSave(t *testing.T) {
	h := newAuthHost(t)
	key, keep := "sk-reconcile-slow-save", "sk-reconcile-slow-keep"
	// The key is configured but its record is disabled: pass 1 must re-enable it.
	h.seedManaged(t, key, true, nil)
	h.seedManaged(t, keep, false, nil)
	withKey, err := config.Load([]byte(reconcileYAML(key, keep)))
	if err != nil {
		t.Fatalf("load config with key: %v", err)
	}
	withoutKey, err := config.Load([]byte(reconcileYAML(keep)))
	if err != nil {
		t.Fatalf("load config without key: %v", err)
	}
	bridge := NewHostBridge(h.call)
	m := NewManager(bridge)
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	gate := make(chan struct{})
	h.mu.Lock()
	h.saveGate = gate
	h.mu.Unlock()
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		h.mu.Lock()
		h.saveGate = nil
		h.mu.Unlock()
		close(gate)
	}
	t.Cleanup(release)

	// Pass 1 wants to re-enable the key, but the save outlives its deadline.
	shortCtx, cancelShort := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelShort()
	if err := m.reconcileAuthRecords(shortCtx, withKey); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("outlived save error = %v, want timeout", err)
	}

	// Pass 2 drops the key. It must not act while the orphaned enable can still
	// land: it blocks until the save settles, then removes the record.
	passCtx, cancelPass := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelPass()
	done := make(chan error, 1)
	go func() { done <- m.reconcileAuthRecords(passCtx, withoutKey) }()
	select {
	case err := <-done:
		t.Fatalf("pass returned before the outstanding save settled: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Let the orphaned save land, then let pass 2 run to completion.
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pass after settle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after the outstanding save settled")
	}

	if h.fileExists(t, authRecordID(key)+".json") {
		t.Fatalf("orphaned enable landed after the removal")
	}
	if !h.fileExists(t, authRecordID(keep)+".json") {
		t.Fatalf("configured record was removed")
	}
}

// TestReconcileRefusesWhileSaveUnsettled covers the other half of the rule: if
// the orphan does not settle inside the lifecycle deadline, the pass fails
// instead of claiming the pool matches the config.
func TestReconcileRefusesWhileSaveUnsettled(t *testing.T) {
	h := newAuthHost(t)
	key := "sk-reconcile-stuck-save"
	h.seedManaged(t, key, false, nil)
	withKey, err := config.Load([]byte(reconcileYAML(key)))
	if err != nil {
		t.Fatalf("load config with key: %v", err)
	}
	withoutKey, err := config.Load([]byte("api-keys:\n  - value: sk-reconcile-stuck-other\n"))
	if err != nil {
		t.Fatalf("load config without key: %v", err)
	}
	m := NewManager(NewHostBridge(h.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	gate := make(chan struct{})
	h.mu.Lock()
	h.saveGate = gate
	h.mu.Unlock()
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		h.mu.Lock()
		h.saveGate = nil
		h.mu.Unlock()
		close(gate)
	}
	t.Cleanup(release)

	shortCtx, cancelShort := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancelShort()
	if err := m.reconcileAuthRecords(shortCtx, withoutKey); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("outlived save error = %v, want timeout", err)
	}

	waitCtx, cancelWait := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancelWait()
	if err := m.reconcileAuthRecords(waitCtx, withKey); err == nil ||
		!strings.Contains(err.Error(), "still in flight") {
		t.Fatalf("unsettled save error = %v, want in-flight refusal", err)
	}
	release()
}

// TestBridgeAuthListSanitizesFailures keeps the list channel from echoing host
// error bodies, which can contain credential records.
func TestBridgeAuthListSanitizesFailures(t *testing.T) {
	bridge := NewHostBridge(func(string, []byte) ([]byte, error) {
		return hostErr("auth_unavailable", "auth directory echoed sk-secret-in-error"), nil
	})
	if _, err := bridge.AuthList(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "host auth list failed") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("list failure text = %v", err)
	}
}
