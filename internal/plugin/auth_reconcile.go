package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// managedAuthPrefix marks the auth records this plugin materializes from
// cfg.APIKeys: the canonical file name is managedAuthPrefix + sha256(api_key)
// + ".json". The digest is non-secret and independent of config ordering.
const managedAuthPrefix = "opencode-go-key-"

// authRecordID is the canonical record id for one configured key.
func authRecordID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return managedAuthPrefix + hex.EncodeToString(digest[:])
}

// reconcileAuthRecords makes CPA-visible auth files track cfg.APIKeys, the
// single source of truth for this provider's credentials:
//
//   - a configured key without a record is created (enabled);
//   - a managed record whose key left the config is DISABLED, and re-enabled
//     when the key returns, so CPA stops scheduling a key the operator already
//     removed (a stale record used to stay active and 401 every cooldown);
//   - nothing is deleted: the host ABI has no delete callback, and a disabled
//     record stays inspectable/enable-able in Management Center;
//   - a record the plugin cannot prove it created (canonical file name, but the
//     raw api_key does not hash to that name) is never touched.
//
// Ownership is proven from the record's raw JSON, which is only readable by
// runtime auth index. A host that has not attached its auth manager yet lists
// auth files from disk without indices: creation still happens, and existing
// records whose state disagrees with the config are reported as deferred for
// the next reconfigure instead of being written blindly.
func (m *Manager) reconcileAuthRecords(ctx context.Context, cfg config.Config) error {
	if m.bridge == nil {
		return nil
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("list existing auth records: %w", err)
	}
	expected := make(map[string]struct{}, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		expected[authRecordID(key.Value)] = struct{}{}
	}
	known := make(map[string]struct{}, len(entries)*2)
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.Name); name != "" {
			known[name] = struct{}{}
		}
		if id := strings.TrimSpace(entry.ID); id != "" {
			known[id] = struct{}{}
		}
	}
	for _, key := range cfg.APIKeys {
		id := authRecordID(key.Value)
		if _, ok := known[id]; ok {
			continue
		}
		if _, ok := known[id+".json"]; ok {
			continue
		}
		record, err := json.Marshal(struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Label  string `json:"label"`
			APIKey string `json:"api_key"`
		}{
			Type: ProviderID, ID: id,
			Label:  "OpenCode Go credential " + strings.TrimPrefix(id, managedAuthPrefix),
			APIKey: key.Value,
		})
		if err != nil {
			return fmt.Errorf("build auth record")
		}
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: id + ".json", JSON: record}); err != nil {
			return err
		}
		known[id] = struct{}{}
		known[id+".json"] = struct{}{}
		debugTrace("auth materialized id=%s file=%s", id, id+".json")
	}
	return m.reconcileManagedAuthState(ctx, entries, expected)
}

// reconcileManagedAuthState brings every indexed managed record to the disabled
// state its config membership implies. Reads are compared against the PHYSICAL
// JSON and the runtime flag, so a disk/runtime disagreement is repaired; writes
// happen only when a state differs, which keeps repeated reconfigures and
// manager restarts save-free.
func (m *Manager) reconcileManagedAuthState(ctx context.Context, entries []pluginapi.HostAuthFileEntry, expected map[string]struct{}) error {
	deferred := 0
	for _, entry := range entries {
		id, ok := managedAuthID(entry)
		if !ok {
			continue
		}
		if listed := strings.TrimSpace(entry.ID); listed != "" && listed != id {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: the host reports id %q",
				shortAuthID(id), listed), nil)
			continue
		}
		_, configured := expected[id]
		desiredDisabled := !configured
		if entry.RuntimeOnly {
			// No backing file: there is no record to patch, and writing one
			// would materialize a credential nobody configured.
			debugTrace("auth reconcile skipped runtime-only record id=%s", id)
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) == "" {
			// Disk-only listing: the file state is visible, the raw JSON (and
			// with it the ownership proof) is not.
			if entry.Disabled != desiredDisabled {
				deferred++
			}
			continue
		}
		fetched, err := m.bridge.AuthGet(ctx, entry.AuthIndex)
		if err != nil {
			return err
		}
		if fetched.AuthIndex != entry.AuthIndex {
			return fmt.Errorf("read credential %s: host returned a different index", shortAuthID(id))
		}
		if name := strings.TrimSpace(fetched.Name); name != "" && name != strings.TrimSpace(entry.Name) {
			return fmt.Errorf("read credential %s: host returned a different file name", shortAuthID(id))
		}
		physicalDisabled, managed, err := inspectManagedAuthRecord(id, fetched.JSON)
		if err != nil {
			return fmt.Errorf("read credential %s: undecodable credential JSON", shortAuthID(id))
		}
		if !managed {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: the record is not plugin-managed",
				shortAuthID(id)), nil)
			continue
		}
		if physicalDisabled == desiredDisabled && entry.Disabled == desiredDisabled {
			continue
		}
		patched, err := setAuthRecordDisabled(fetched.JSON, desiredDisabled)
		if err != nil {
			return fmt.Errorf("read credential %s: undecodable credential JSON", shortAuthID(id))
		}
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: entry.Name, JSON: patched}); err != nil {
			return err
		}
		action := "enabled: listed in configured api-keys"
		if desiredDisabled {
			action = "disabled: no longer listed in configured api-keys"
		}
		_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s %s", shortAuthID(id), action), nil)
	}
	if deferred > 0 {
		_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential reconciliation deferred for %d record(s): the host has no runtime credential index yet, the next reconfigure finishes it",
			deferred), nil)
	}
	return nil
}

// managedAuthID recognizes the plugin's own canonical record name. Provider and
// type must not contradict it; an empty one on a disk-only listing is fine.
func managedAuthID(entry pluginapi.HostAuthFileEntry) (string, bool) {
	name := strings.TrimSpace(entry.Name)
	if !strings.HasSuffix(name, ".json") {
		return "", false
	}
	id := strings.TrimSuffix(name, ".json")
	suffix, ok := strings.CutPrefix(id, managedAuthPrefix)
	if !ok || len(suffix) != 64 || !isLowerHex(suffix) {
		return "", false
	}
	if provider := strings.TrimSpace(entry.Provider); provider != "" && provider != ProviderID {
		return "", false
	}
	if kind := strings.TrimSpace(entry.Type); kind != "" && kind != ProviderID {
		return "", false
	}
	return id, true
}

// inspectManagedAuthRecord reports the physical disabled state and whether the
// raw record proves plugin ownership: our type, our canonical id, and an
// api_key that hashes to that id's digest. Any mismatch means the record is not
// ours to rewrite.
func inspectManagedAuthRecord(id string, raw []byte) (disabled, managed bool, err error) {
	var record struct {
		Type     string `json:"type"`
		ID       string `json:"id"`
		APIKey   string `json:"api_key"`
		Disabled *bool  `json:"disabled"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return false, false, err
	}
	digest := sha256.Sum256([]byte(record.APIKey))
	owned := record.Type == ProviderID && record.ID == id && record.APIKey != "" &&
		hex.EncodeToString(digest[:]) == strings.TrimPrefix(id, managedAuthPrefix)
	if !owned {
		return false, false, nil
	}
	return record.Disabled != nil && *record.Disabled, true, nil
}

// setAuthRecordDisabled patches only the "disabled" flag. Every other field is
// carried over as raw JSON, so operator-set metadata (priority, note, weight,
// nested values) survives the rewrite.
func setAuthRecordDisabled(raw []byte, disabled bool) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if disabled {
		fields["disabled"] = json.RawMessage("true")
	} else {
		delete(fields, "disabled")
	}
	return json.Marshal(fields)
}

// shortAuthID is the 12-hex-prefix form used in logs.
func shortAuthID(id string) string {
	hash := strings.TrimPrefix(id, managedAuthPrefix)
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
