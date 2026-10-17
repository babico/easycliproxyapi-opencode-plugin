package plugin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// managedAuthPrefix marks the auth records this plugin materializes from
// cfg.APIKeys: the canonical file name is managedAuthPrefix + sha256(api_key)
// + ".json". The digest is non-secret and independent of config ordering.
const managedAuthPrefix = "opencode-go-key-"

// managedAuthStagingSuffix names the sibling a removal stages the credential
// under while it is being proven. It deliberately does not end in .json, so the
// host's auth-directory watcher ignores it.
const managedAuthStagingSuffix = ".reconcile-removing-"

// authRecordID is the canonical record id for one configured key.
func authRecordID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return managedAuthPrefix + hex.EncodeToString(digest[:])
}

// reconcileAuthRecords makes CPA-visible auth files track cfg.APIKeys, the
// single source of truth for this provider's credentials:
//
//   - a configured key without a record is created (enabled), and a disabled
//     managed record for a configured key is re-enabled;
//   - a managed record whose key left the config is DELETED, so CPA stops
//     scheduling a key the operator already removed (a stale record used to
//     stay active and answer 401 on every cooldown);
//   - a record the plugin cannot prove it created (canonical file name, but the
//     raw api_key does not hash to that name) is never touched.
//
// Deletion cannot go through the host ABI — it exposes list/get/save but no
// delete/disable — so the plugin removes the auth file at the path the host
// reported and lets CPA's auth-directory watcher unregister the credential.
// Only a regular file whose name is the canonical managed name and whose raw
// record proves ownership is ever unlinked.
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
	// A timed-out save may still be writing. Reading now could observe the
	// pre-write state and then report success while the orphan lands behind
	// us, so wait for outstanding writes to settle (or fail honestly).
	if err := m.bridge.WaitAuthSaves(ctx); err != nil {
		return err
	}
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("list existing auth records: %w", err)
	}
	expected := make(map[string]struct{}, len(cfg.APIKeys))
	keyByID := make(map[string]string, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		id := authRecordID(key.Value)
		expected[id] = struct{}{}
		keyByID[id] = key.Value
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
		if err := m.saveManagedAuthRecord(ctx, id, key.Value); err != nil {
			return err
		}
		known[id] = struct{}{}
		known[id+".json"] = struct{}{}
	}
	return m.reconcileManagedAuthState(ctx, entries, expected, keyByID)
}

// saveManagedAuthRecord writes the canonical record for one configured key.
func (m *Manager) saveManagedAuthRecord(ctx context.Context, id, value string) error {
	record, err := json.Marshal(struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Label  string `json:"label"`
		APIKey string `json:"api_key"`
	}{
		Type: ProviderID, ID: id,
		Label:  "OpenCode Go credential " + strings.TrimPrefix(id, managedAuthPrefix),
		APIKey: value,
	})
	if err != nil {
		return fmt.Errorf("build auth record")
	}
	if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: id + ".json", JSON: record}); err != nil {
		return err
	}
	debugTrace("auth materialized id=%s file=%s", id, id+".json")
	return nil
}

// reconcileManagedAuthState deletes managed records whose key is no longer
// configured and re-enables configured ones that are disabled. Reads come from
// the PHYSICAL JSON, so a disk/runtime disagreement is repaired; the enable
// write happens only when a state differs, which keeps repeated reconfigures
// and manager restarts save-free.
//
// A record that cannot be read, proven or written is left alone and reported as
// a warn: this function never reports a state it did not verify, and it never
// fails the caller, because failing registration takes the whole provider down.
func (m *Manager) reconcileManagedAuthState(ctx context.Context, entries []pluginapi.HostAuthFileEntry, expected map[string]struct{}, keyByID map[string]string) error {
	deferred := 0
	for _, entry := range entries {
		id, ok := managedAuthID(entry)
		if !ok {
			continue
		}
		if listed := strings.TrimSpace(entry.ID); listed != "" && listed != id {
			// The listed id is host input that may have been copied from
			// credential JSON: never echo it, only the validated digest prefix.
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: the host id does not match the canonical file name",
				shortAuthID(id)), nil)
			continue
		}
		_, configured := expected[id]
		if entry.RuntimeOnly {
			// No backing file: there is no record to patch, and writing one
			// would materialize a credential nobody configured.
			debugTrace("auth reconcile skipped runtime-only record id=%s", id)
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) == "" {
			// Disk-only listing: the file state is visible, the raw JSON (and
			// with it the ownership proof) is not.
			if entry.Disabled && configured {
				deferred++
			}
			if !configured {
				deferred++
			}
			continue
		}
		fetched, err := m.bridge.AuthGet(ctx, entry.AuthIndex)
		if err != nil {
			// The record cannot be read: it was just unlinked and the host has
			// not dropped it from its runtime list yet, the index is stale, or
			// the read timed out. Ownership cannot be proven without the raw
			// JSON. A configured key whose backing file is provably absent is
			// the one case that needs no proof — there is nothing to overwrite
			// — so recreate it; anything else is left alone and reported.
			value, configured := keyByID[id]
			if configured && managedAuthFileMissing(entry.Name, entry.Path) {
				if err := m.saveManagedAuthRecord(ctx, id, value); err != nil {
					_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not restored: %v",
						shortAuthID(id), err), nil)
					continue
				}
				_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s restored: its file was gone while the host still listed it",
					shortAuthID(id)), nil)
				continue
			}
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: %v",
				shortAuthID(id), err), nil)
			continue
		}
		if fetched.AuthIndex != entry.AuthIndex {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: the host returned a different index",
				shortAuthID(id)), nil)
			continue
		}
		if name := strings.TrimSpace(fetched.Name); name != "" && name != strings.TrimSpace(entry.Name) {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: the host returned a different file name",
				shortAuthID(id)), nil)
			continue
		}
		// The read must describe the file the listing named, or the ownership
		// proof would apply to one path while the mutation targets another.
		if path := strings.TrimSpace(fetched.Path); path != "" && path != strings.TrimSpace(entry.Path) {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: the host listed a different path than it read",
				shortAuthID(id)), nil)
			continue
		}
		physicalDisabled, managed, err := inspectManagedAuthRecord(id, fetched.JSON)
		if err != nil {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: undecodable credential JSON",
				shortAuthID(id)), nil)
			continue
		}
		if !managed {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not reconciled: the record is not plugin-managed",
				shortAuthID(id)), nil)
			continue
		}
		if !configured {
			if err := m.removeManagedAuthRecord(ctx, id, entry.Name, entry.Path); err != nil {
				_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not removed: %v",
					shortAuthID(id), err), nil)
				continue
			}
			_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s removed: no longer listed in configured api-keys",
				shortAuthID(id)), nil)
			continue
		}
		if !physicalDisabled && !entry.Disabled {
			continue
		}
		patched, err := setAuthRecordDisabled(fetched.JSON, false)
		if err != nil {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not re-enabled: undecodable credential JSON",
				shortAuthID(id)), nil)
			continue
		}
		if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: entry.Name, JSON: patched}); err != nil {
			_ = m.bridge.Log("warn", fmt.Sprintf("opencode-go credential %s not re-enabled: %v",
				shortAuthID(id), err), nil)
			continue
		}
		_ = m.bridge.Log("info", fmt.Sprintf("opencode-go credential %s enabled: listed in configured api-keys",
			shortAuthID(id)), nil)
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
	if !managedAuthName(name) {
		return "", false
	}
	id := strings.TrimSuffix(name, ".json")
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

// managedAuthName reports whether name is a canonical managed auth file name.
func managedAuthName(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	suffix, ok := strings.CutPrefix(strings.TrimSuffix(name, ".json"), managedAuthPrefix)
	return ok && len(suffix) == 64 && isLowerHex(suffix)
}

// removeManagedAuthRecord deletes one unconfigured credential. Three steps,
// each of which matters:
//
//  1. The file is renamed to a non-.json sibling (atomic, and invisible to the
//     host's auth-directory watcher) and the RENAMED file is read. Ownership is
//     therefore proven from the bytes of exactly one inode, and no later writer
//     can swap what that proof refers to: whatever else happens, the file that
//     was proven is the file that gets deleted. A file that does not prove
//     ownership is renamed back untouched, before anything has been written.
//  2. The record is saved with `disabled: true`. That is what makes the host
//     hold the credential as disabled in memory; without it the host keeps the
//     credential enabled and rewrites the file from memory on the next state
//     change (a 401 marks it unauthorized and persists it), so a bare unlink
//     resurrects the credential — observed live, along with the 401 that
//     followed. The save writes the canonical name from the JSON this plugin
//     just produced, so the file it leaves behind is this plugin's own record.
//  3. Both files are removed: the staged inode that was proven, and the file
//     the host wrote in step 2. The host's filestore refuses to recreate a
//     disabled credential whose file is gone, so the deletion sticks.
func (m *Manager) removeManagedAuthRecord(ctx context.Context, id, name, path string) error {
	name = strings.TrimSpace(name)
	path = strings.TrimSpace(path)
	if !managedAuthName(name) || !filepath.IsAbs(path) || filepath.Base(path) != name {
		return fmt.Errorf("host did not report a usable auth file path")
	}
	if info, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing left to delete: the unlink already happened, or the file
			// was removed out of band. The host drops the runtime entry itself.
			return nil
		}
		return fmt.Errorf("inspect auth file failed")
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("auth file is not a regular file")
	}
	staged, err := stageManagedAuthFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(staged)
	if err != nil {
		return restoreStagedAuthFile(staged, path, "read auth file before removal failed")
	}
	if _, owned, errInspect := inspectManagedAuthRecord(id, raw); errInspect != nil || !owned {
		return restoreStagedAuthFile(staged, path, "auth file is not plugin-managed")
	}
	patched, err := setAuthRecordDisabled(raw, true)
	if err != nil {
		return restoreStagedAuthFile(staged, path, "undecodable credential JSON")
	}
	if err := m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: patched}); err != nil {
		// The proven record must not be lost because the host refused the
		// disable: put it back where the host expects it.
		return restoreStagedAuthFile(staged, path, err.Error())
	}
	if err := os.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("unlink auth file failed")
	}
	// The host wrote the canonical name from the JSON this plugin just built.
	// It is read back and checked before being unlinked, so the only file this
	// plugin ever deletes is one whose own bytes prove it is this plugin's
	// record.
	written, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read auth file after disabling failed")
	}
	if _, owned, errInspect := inspectManagedAuthRecord(id, written); errInspect != nil || !owned {
		return fmt.Errorf("auth file changed while it was being removed; left in place")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("unlink auth file failed")
	}
	return nil
}

// stageManagedAuthFile moves the credential aside under a private, unique name
// so that ownership can be proven from one inode — the one that is later
// deleted — and so that nothing the plugin writes can land on a file it has
// not proven. The name never ends in .json, so the host's auth-directory
// watcher ignores it. It is created with O_EXCL before the rename, so a
// leftover from an interrupted attempt can never be silently overwritten.
func stageManagedAuthFile(path string) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("stage auth file for removal failed")
		}
		staged := path + managedAuthStagingSuffix + hex.EncodeToString(suffix[:])
		handle, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", fmt.Errorf("stage auth file for removal failed")
		}
		if err := handle.Close(); err != nil {
			_ = os.Remove(staged)
			return "", fmt.Errorf("stage auth file for removal failed")
		}
		if err := os.Rename(path, staged); err != nil {
			_ = os.Remove(staged)
			if errors.Is(err, fs.ErrNotExist) {
				return "", fs.ErrNotExist
			}
			return "", fmt.Errorf("stage auth file for removal failed")
		}
		return staged, nil
	}
	return "", fmt.Errorf("stage auth file for removal failed")
}

// restoreStagedAuthFile puts a staged credential back under its canonical name
// and returns cause as the error. The link is non-overwriting on purpose: if
// something now occupies the canonical name, the staged file is left in place
// and its path is reported, so the operator can recover a credential this
// plugin never got permission to replace.
func restoreStagedAuthFile(staged, path, cause string) error {
	if err := os.Link(staged, path); err != nil {
		return fmt.Errorf("%s; the credential is preserved at %s", cause, staged)
	}
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("%s; the credential is preserved at %s", cause, staged)
	}
	return fmt.Errorf("%s", cause)
}

// errAuthFileGone marks a removal whose target is already absent.
var errAuthFileGone = errors.New("auth file is gone")

// managedAuthFileMissing reports whether the canonical path a listing named is
// provably absent. Only an absolute path with the canonical file name counts,
// so a hostile or odd listing cannot steer the answer.
func managedAuthFileMissing(name, path string) bool {
	name = strings.TrimSpace(name)
	path = strings.TrimSpace(path)
	if !managedAuthName(name) || !filepath.IsAbs(path) || filepath.Base(path) != name {
		return false
	}
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
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
