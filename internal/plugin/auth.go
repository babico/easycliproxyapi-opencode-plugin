package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"easycliproxyapi-opencode-plugin/internal/config"
)

// authProvider — credential file parse. Ikinci sekil de destekte edilir:
//   - managed reconcile dosyasi: {"type":"opencode-go","id","label","api_key"}
//   - OAuth device-login: {"type":"opencode-go","access_token","refresh_token",...}
//
// Yalnizca api_key'li kayitlar zen/go/v1 (BaseURL), OAuth token'lar
// inference/go (InferenceBaseURL) endpoint ailesine yonlendirilir.
type authProvider struct{}

func (authProvider) Identifier() string { return ProviderID }

// pendingLoginStates: device flow start->poll arasi durum (sanitize state anahtarli)
var pendingLoginStates sync.Map

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type deviceTokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    int     `json:"expires_in"`
	OrgID        *string `json:"org_id"`
	Error        string  `json:"error"`
}

type consoleUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type consoleOrg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type loginStatePayload struct {
	ConsoleURL string `json:"console_url"`
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
}

type storedCredential struct {
	Type         string `json:"type"`
	ID           string `json:"id,omitempty"`
	Label        string `json:"label,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	ExpiresAtMs  int64  `json:"expires_at_ms,omitempty"`
	ConsoleURL   string `json:"console_url,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email,omitempty"`
	OrgID        string `json:"org_id,omitempty"`
	OrgName      string `json:"org_name,omitempty"`
	Plan         string `json:"plan,omitempty"`
}

func (authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	debugTrace("auth parse request provider=%s file=%s raw_bytes=%d", req.Provider, req.FileName, len(req.RawJSON))
	trimmed := bytes.TrimSpace(req.RawJSON)
	if len(trimmed) == 0 {
		return pluginapi.AuthParseResponse{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{}, nil
	}
	rawType := strings.ToLower(strings.TrimSpace(stringFromMap(raw, "type", "provider")))
	if rawType != ProviderID && rawType != "opencode-go" && rawType != "opencode_go" {
		if meta, ok := raw["metadata"].(map[string]any); ok {
			// OpenCode CLI native credential sekli (metadata.server = opencode.ai)
			if strings.Contains(strings.ToLower(stringFromMap(meta, "server")), "opencode.ai") && stringFromMap(raw, "access") != "" {
				rawType = ProviderID
			}
		}
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (rawType == "" && req.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if rawType != ProviderID && rawType != "opencode-go" && rawType != "opencode_go" {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	cred, okParse := parseStoredCredential(raw)
	if !okParse {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("opencode-go auth record has neither api_key nor access_token")
		}
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	cfg := config.Config{} // parse host-config'dan console defaulti cekemez; buildAuthData empty cfg'de de gorunur
	authData := buildAuthDataFromCredential(cred, cfg, req.FileName, req.Path, req.RawJSON)
	debugTrace("auth parse handled provider=%s file=%s id=%s api_key_present=%t", req.Provider, req.FileName, authData.ID, strings.TrimSpace(authData.Attributes["api_key"]) != "")
	return pluginapi.AuthParseResponse{Handled: true, Auth: authData}, nil
}

func parseStoredCredential(raw map[string]any) (storedCredential, bool) {
	cred := storedCredential{
		Type:         ProviderID,
		ID:           stringFromMap(raw, "id", "filename"),
		Label:        stringFromMap(raw, "label"),
		AccessToken:  firstNonEmpty(stringFromMap(raw, "access_token", "access")),
		RefreshToken: firstNonEmpty(stringFromMap(raw, "refresh_token", "refresh")),
		APIKey:       firstNonEmpty(stringFromMap(raw, "api_key", "key")),
		ConsoleURL:   stringFromMap(raw, "console_url", "server"),
		AccountID:    stringFromMap(raw, "account_id", "accountID"),
		Email:        stringFromMap(raw, "email"),
		OrgID:        stringFromMap(raw, "org_id", "orgID", "project_id"),
		OrgName:      stringFromMap(raw, "org_name", "orgName"),
		Plan:         stringFromMap(raw, "plan", "plan_type"),
	}
	if meta, ok := raw["metadata"].(map[string]any); ok {
		cred.ConsoleURL = firstNonEmpty(stringFromMap(meta, "server"), cred.ConsoleURL)
		cred.AccountID = firstNonEmpty(cred.AccountID, stringFromMap(meta, "accountID", "account_id"))
		cred.Email = firstNonEmpty(cred.Email, stringFromMap(meta, "email"))
		cred.OrgID = firstNonEmpty(cred.OrgID, stringFromMap(meta, "orgID", "org_id"))
		cred.OrgName = firstNonEmpty(cred.OrgName, stringFromMap(meta, "orgName", "org_name"))
	}
	if expMs := int64FromMap(raw, "expires_at_ms", "expires"); expMs > 0 {
		cred.ExpiresAtMs = expMs
	} else if expStr := stringFromMap(raw, "expires_at", "expired"); expStr != "" {
		if t, err := time.Parse(time.RFC3339, expStr); err == nil {
			cred.ExpiresAtMs = t.UnixMilli()
		}
	}
	// managed reconcile dosyalari storage'ini degistirmeden geri doner:
	// ParseAuth orijinal RawJSON'u StorageJSON olarak tasiyor (aasi).
	return cred, cred.AccessToken != "" || cred.APIKey != ""
}

// buildAuthDataFromCredential: AuthData uret. OAuth credential'larinda
// storage, esauvisky sekliyle normalize edilir (refresh tekrar parse edebilsin);
// api-key credential'lari orijinal storage bytes'ini korur (reconcile kaniti).
func buildAuthDataFromCredential(cred storedCredential, cfg config.Config, fileName, path string, originalStorage []byte) pluginapi.AuthData {
	if cred.ConsoleURL == "" {
		cred.ConsoleURL = cfg.ConsoleURL
	}
	isOAuth := cred.AccessToken != ""
	label := firstNonEmpty(cred.Label, cred.Email, cred.OrgName, "OpenCode Go")
	if cred.OrgName != "" && cred.Email != "" && !strings.EqualFold(cred.OrgName, cred.Email) {
		label = fmt.Sprintf("%s (%s)", cred.Email, cred.OrgName)
	}
	if strings.TrimSpace(fileName) == "" {
		slug := sanitizeFileNamePart(firstNonEmpty(cred.Email, cred.OrgID, "account"))
		fileName = "opencode-go-" + slug + ".json"
	}
	id := strings.TrimSpace(cred.ID)
	if id == "" {
		id = strings.TrimSpace(fileName)
	}
	if id == "" {
		id = strings.TrimSpace(path)
	}
	if id == "" {
		id = "opencode-go.json"
	}

	storageJSON := originalStorage
	if isOAuth {
		storageMap := map[string]any{
			"type":          ProviderID,
			"access_token":  cred.AccessToken,
			"refresh_token": cred.RefreshToken,
			"api_key":       cred.APIKey,
			"expires_at_ms": cred.ExpiresAtMs,
			"console_url":   cred.ConsoleURL,
			"account_id":    cred.AccountID,
			"email":         cred.Email,
			"org_id":        cred.OrgID,
			"org_name":      cred.OrgName,
			"plan":          cred.Plan,
		}
		if cred.ExpiresAtMs > 0 {
			storageMap["expired"] = time.UnixMilli(cred.ExpiresAtMs).UTC().Format(time.RFC3339)
		}
		if b, err := marshalJSONNoEscape(storageMap); err == nil {
			storageJSON = b
		}
	}

	var nextRefresh time.Time
	if cred.ExpiresAtMs > 0 {
		expTime := time.UnixMilli(cred.ExpiresAtMs).UTC()
		if cred.RefreshToken != "" {
			nextRefresh = expTime.Add(-5 * time.Minute)
			if nextRefresh.Before(time.Now().UTC()) {
				nextRefresh = time.Now().UTC().Add(10 * time.Second)
			}
		}
	}

	token := firstNonEmpty(cred.AccessToken, cred.APIKey)
	attrs := map[string]string{"api_key": token}
	if isOAuth {
		// OAuth token'lar inference endpoint ailesinde (zen/go/v1 degil)
		attrs["base_url"] = firstNonEmpty(cfg.InferenceBaseURL, config.DefaultInferenceBaseURL) + "/openai/v1"
	}
	if cred.OrgID != "" {
		attrs["org_id"] = cred.OrgID
	}
	if cred.Email != "" {
		attrs["email"] = cred.Email
	}

	meta := map[string]any{
		"type":       ProviderID,
		"email":      cred.Email,
		"project_id": cred.OrgID,
		"account":    cred.OrgName,
	}
	if cred.Plan != "" {
		meta["plan"] = cred.Plan
	}
	if cred.ExpiresAtMs > 0 {
		meta["expired"] = time.UnixMilli(cred.ExpiresAtMs).UTC().Format(time.RFC3339)
	}

	return pluginapi.AuthData{
		Provider:         ProviderID,
		ID:               id,
		FileName:         fileName,
		Label:            label,
		StorageJSON:      storageJSON,
		Metadata:         meta,
		Attributes:       attrs,
		NextRefreshAfter: nextRefresh,
	}
}

// --- device login (auth.login.start / auth.login.poll) ---

func (m *Manager) authLoginStart(hostCallbackID string, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if m.bridge == nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("login requires host bridge")
	}
	consoleURL := strings.TrimRight(cfg.ConsoleURL, "/")
	bodyBytes, _ := marshalJSONNoEscape(map[string]any{
		"client_id":          cfg.OAuthClientID,
		"supports_org_scope": true,
	})
	resp, err := m.bridge.DoCallback(context.Background(), hostCallbackID, pluginapi.HTTPRequest{
		Method: http.MethodPost, URL: consoleURL + "/auth/device/code",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Accept":       []string{"application/json"},
			"User-Agent":   []string{cfg.UserAgent},
		},
		Body: bodyBytes,
	})
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("request OpenCode device code: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("OpenCode device code HTTP %d", resp.StatusCode)
	}
	var codeResp deviceCodeResponse
	if err := json.Unmarshal(resp.Body, &codeResp); err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("decode OpenCode device code response")
	}
	if strings.TrimSpace(codeResp.DeviceCode) == "" {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("OpenCode device code response missing device_code")
	}

	verifyURL := strings.TrimSpace(codeResp.VerificationURIComplete)
	if verifyURL == "" {
		verifyURL = strings.TrimSpace(codeResp.VerificationURI)
	}
	if strings.HasPrefix(verifyURL, "/") {
		verifyURL = strings.TrimSuffix(consoleURL, "/console") + verifyURL
	}

	stateKey := sanitizeOAuthState(codeResp.DeviceCode, codeResp.UserCode)
	pendingLoginStates.Store(stateKey, loginStatePayload{
		ConsoleURL: consoleURL,
		DeviceCode: codeResp.DeviceCode,
		UserCode:   codeResp.UserCode,
	})

	expiresIn := codeResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ProviderID,
		URL:       verifyURL,
		State:     stateKey,
		ExpiresAt: time.Now().UTC().Add(time.Duration(expiresIn) * time.Second),
		Metadata: map[string]any{
			"device_code": codeResp.DeviceCode,
			"user_code":   codeResp.UserCode,
			"console_url": consoleURL,
			"interval":    codeResp.Interval,
		},
	}, nil
}

func (m *Manager) authLoginPoll(hostCallbackID string, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	stateKey := strings.TrimSpace(req.State)
	var state loginStatePayload
	if loaded, ok := pendingLoginStates.Load(stateKey); ok {
		if typed, okTyped := loaded.(loginStatePayload); okTyped {
			state = typed
		}
	}
	if state.DeviceCode == "" && req.Metadata != nil {
		state.DeviceCode = stringFromMap(req.Metadata, "device_code")
		state.UserCode = stringFromMap(req.Metadata, "user_code")
		state.ConsoleURL = stringFromMap(req.Metadata, "console_url")
	}
	if state.DeviceCode == "" {
		state.DeviceCode = stateKey
	}
	if state.DeviceCode == "" {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "invalid login state"}, nil
	}

	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	consoleURL := strings.TrimRight(firstNonEmpty(state.ConsoleURL, cfg.ConsoleURL), "/")
	pollBody, _ := marshalJSONNoEscape(map[string]any{
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
		"device_code": state.DeviceCode,
		"client_id":   cfg.OAuthClientID,
	})
	resp, err := m.bridge.DoCallback(context.Background(), hostCallbackID, pluginapi.HTTPRequest{
		Method: http.MethodPost, URL: consoleURL + "/auth/device/token",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Accept":       []string{"application/json"},
			"User-Agent":   []string{cfg.UserAgent},
		},
		Body: pollBody,
	})
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for device authorization"}, nil
	}

	var tokenResp deviceTokenResponse
	if err := json.Unmarshal(resp.Body, &tokenResp); err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for device authorization"}, nil
	}
	if tokenResp.AccessToken == "" {
		switch strings.ToLower(strings.TrimSpace(tokenResp.Error)) {
		case "", "authorization_pending", "slow_down":
			msg := "Waiting for browser approval"
			if state.UserCode != "" {
				msg = fmt.Sprintf("Waiting for approval (code: %s)", state.UserCode)
			}
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: msg}, nil
		default:
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Device authorization failed: " + tokenResp.Error}, nil
		}
	}

	cred := m.completeOAuthCredential(hostCallbackID, consoleURL, cfg, tokenResp)
	pendingLoginStates.Delete(stateKey)
	authData := buildAuthDataFromCredential(cred, cfg, "", "", nil)
	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "OpenCode Go login complete",
		Auth:    authData,
	}, nil
}

// completeOAuthCredential: token ile hesap/org kimligini cozer (best-effort).
func (m *Manager) completeOAuthCredential(hostCallbackID, consoleURL string, cfg config.Config, tokenResp deviceTokenResponse) storedCredential {
	headers := http.Header{
		"Authorization": []string{"Bearer " + tokenResp.AccessToken},
		"Accept":        []string{"application/json"},
		"User-Agent":    []string{cfg.UserAgent},
	}
	var user consoleUser
	if r, err := m.bridge.DoCallback(context.Background(), hostCallbackID, pluginapi.HTTPRequest{Method: http.MethodGet, URL: consoleURL + "/api/user", Headers: headers}); err == nil && r.StatusCode == http.StatusOK {
		_ = json.Unmarshal(r.Body, &user)
	}
	var orgs []consoleOrg
	if r, err := m.bridge.DoCallback(context.Background(), hostCallbackID, pluginapi.HTTPRequest{Method: http.MethodGet, URL: consoleURL + "/api/orgs", Headers: headers}); err == nil && r.StatusCode == http.StatusOK {
		_ = json.Unmarshal(r.Body, &orgs)
	}
	var selectedOrg consoleOrg
	if tokenResp.OrgID != nil && strings.TrimSpace(*tokenResp.OrgID) != "" {
		targetOrg := strings.TrimSpace(*tokenResp.OrgID)
		selectedOrg.ID = targetOrg
		for _, o := range orgs {
			if o.ID == targetOrg {
				selectedOrg = o
				break
			}
		}
	} else if len(orgs) > 0 {
		sort.Slice(orgs, func(i, j int) bool {
			if orgs[i].Name != orgs[j].Name {
				return orgs[i].Name < orgs[j].Name
			}
			return orgs[i].ID < orgs[j].ID
		})
		selectedOrg = orgs[0]
	}
	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 2592000
	}
	return storedCredential{
		Type:         ProviderID,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAtMs:  time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).UnixMilli(),
		ConsoleURL:   consoleURL,
		AccountID:    user.ID,
		Email:        user.Email,
		OrgID:        selectedOrg.ID,
		OrgName:      selectedOrg.Name,
		Plan:         "go",
	}
}

// --- refresh (auth.refresh) ---

func (m *Manager) authRefresh(hostCallbackID string, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	debugTrace("auth refresh request provider=%s id=%s storage_json_bytes=%d", req.AuthProvider, req.AuthID, len(req.StorageJSON))
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	var cred storedCredential
	if len(req.StorageJSON) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(req.StorageJSON, &raw); err == nil {
			parsed, ok := parseStoredCredential(raw)
			if ok {
				cred = parsed
			}
		}
	}
	if cred.RefreshToken == "" {
		// api-key credential: refresh no-op (mevcut davranis)
		attrs := req.Attributes
		if attrs == nil {
			attrs = map[string]string{}
		}
		return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{
			Provider: req.AuthProvider, ID: req.AuthID, StorageJSON: req.StorageJSON,
			Metadata: req.Metadata, Attributes: attrs,
		}}, nil
	}

	consoleURL := strings.TrimRight(firstNonEmpty(cred.ConsoleURL, cfg.ConsoleURL), "/")
	refreshBody, _ := marshalJSONNoEscape(map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": cred.RefreshToken,
		"client_id":     cfg.OAuthClientID,
	})
	resp, err := m.bridge.DoCallback(context.Background(), hostCallbackID, pluginapi.HTTPRequest{
		Method: http.MethodPost, URL: consoleURL + "/auth/device/token",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Accept":       []string{"application/json"},
			"User-Agent":   []string{cfg.UserAgent},
		},
		Body: refreshBody,
	})
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("refresh OpenCode token failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("refresh OpenCode token HTTP %d", resp.StatusCode)
	}
	var tokenResp deviceTokenResponse
	if err := json.Unmarshal(resp.Body, &tokenResp); err != nil || tokenResp.AccessToken == "" {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("invalid OpenCode refresh token response")
	}

	cred.AccessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		cred.RefreshToken = tokenResp.RefreshToken
	}
	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 2592000
	}
	cred.ExpiresAtMs = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	if tokenResp.OrgID != nil && strings.TrimSpace(*tokenResp.OrgID) != "" && cred.OrgID != strings.TrimSpace(*tokenResp.OrgID) {
		cred.OrgID = strings.TrimSpace(*tokenResp.OrgID)
		cred.OrgName = cred.OrgID
	}

	authData := buildAuthDataFromCredential(cred, cfg, "", "", nil)
	authData.ID = req.AuthID
	return pluginapi.AuthRefreshResponse{Auth: authData, NextRefreshAfter: authData.NextRefreshAfter}, nil
}

// --- helpers ---

func marshalJSONNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func stringFromMap(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, okStr := v.(string); okStr && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func int64FromMap(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch typed := v.(type) {
			case float64:
				return int64(typed)
			case int64:
				return typed
			case int:
				return int64(typed)
			}
		}
	}
	return 0
}

func sanitizeOAuthState(deviceCode, userCode string) string {
	raw := strings.TrimSpace(deviceCode)
	if raw == "" {
		raw = "oc-" + strings.TrimSpace(userCode)
	}
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 96 {
		out = out[:96]
	}
	if out == "" {
		out = fmt.Sprintf("oc-%d", time.Now().UnixNano())
	}
	return out
}

func sanitizeFileNamePart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == '@' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		return "account"
	}
	return out
}
