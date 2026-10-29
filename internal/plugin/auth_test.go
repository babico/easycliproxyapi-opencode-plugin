package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"easycliproxyapi-opencode-plugin/internal/config"
)

func TestAuthProviderParseAndRefresh(t *testing.T) {
	secret := "sk-auth-secret-1"
	p := authProvider{}
	rawBytes := []byte(`{"type":"opencode-go","id":"stable","label":"Primary","api_key":"` + secret + `"}`)
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		Provider: ProviderID, FileName: "key.json",
		RawJSON: rawBytes,
	})
	if err != nil || !parsed.Handled || parsed.Auth.ID != "stable" || parsed.Auth.Label != "Primary" || parsed.Auth.Attributes["api_key"] != secret || string(parsed.Auth.StorageJSON) != string(rawBytes) {
		t.Fatalf("parsed auth = %#v, err=%v", parsed, err)
	}
	if _, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"other","api_key":"x1"}`)}); err != nil {
		t.Fatalf("unrelated auth error = %v", err)
	}
	invalid, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, RawJSON: []byte(`{"type":"opencode-go"}`)})
	if err == nil || invalid.Handled || strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid auth = %#v, err=%v", invalid, err)
	}
	refreshed, err := NewManager(nil).authRefresh("", pluginapi.AuthRefreshRequest{AuthID: "stable", AuthProvider: ProviderID, Attributes: parsed.Auth.Attributes})
	if err != nil || refreshed.Auth.Attributes["api_key"] != secret {
		t.Fatalf("refresh = %#v, err=%v", refreshed, err)
	}
}

func TestAuthProviderAliasesAndFilenameFallback(t *testing.T) {
	p := authProvider{}
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "custom-file.json",
		RawJSON:  []byte(`{"provider":"opencode-go","api_key":"sk-alias-key"}`),
	})
	if err != nil || !parsed.Handled || parsed.Auth.ID != "custom-file.json" || parsed.Auth.Attributes["api_key"] != "sk-alias-key" {
		t.Fatalf("alias parse = %#v, err=%v", parsed, err)
	}

	unhandled, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "other.json",
		RawJSON:  []byte(`{"provider":"other-provider","api_key":"sk-other-key"}`),
	})
	if err != nil || unhandled.Handled {
		t.Fatalf("unhandled parse = %#v, err=%v", unhandled, err)
	}
}

func TestAuthProviderMalformedRecognizedInput(t *testing.T) {

	p := authProvider{}
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		Provider: ProviderID,
		RawJSON:  []byte(`{"type":"opencode-go","api_key":"sk-malformed-secret-1"`),
	})
	if err == nil || parsed.Handled || strings.Contains(err.Error(), "sk-malformed-secret-1") {
		t.Fatalf("malformed recognized auth = %#v, err=%v", parsed, err)
	}
	if err.Error() != "opencode-go auth record has invalid JSON" {
		t.Fatalf("malformed recognized auth error = %v", err)
	}

	unrelated, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		RawJSON: []byte(`{"type":"opencode-go","api_key":"sk-malformed-secret-2"`),
	})
	if err != nil || unrelated.Handled {
		t.Fatalf("malformed unrelated auth = %#v, err=%v", unrelated, err)
	}
}

func TestAuthDispatchMethods(t *testing.T) {
	m := NewManager(nil)
	var got pluginapi.AuthParseResponse
	raw, err := m.HandleCall(pluginabi.MethodAuthIdentifier, nil)
	if err != nil || !bytesContain(raw, []byte(ProviderID)) {
		t.Fatalf("identifier = %s, err=%v", raw, err)
	}
	raw, err = m.HandleCall(pluginabi.MethodAuthParse, mustJSON(pluginapi.AuthParseRequest{Provider: ProviderID, RawJSON: []byte(`{"type":"opencode-go","api_key":"sk-dispatch-1"}`)}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, &got)
	if !got.Handled || got.Auth.Attributes["api_key"] != "sk-dispatch-1" {
		t.Fatalf("parse dispatch = %#v", got)
	}
	// login.start: nil bridge -> auth_failure
	raw, _ = m.HandleCall(pluginabi.MethodAuthLoginStart, []byte(`{}`))
	if env := decodeEnv(t, raw); env.OK || env.Error == nil || env.Error.Code != "auth_failure" {
		t.Fatalf("login.start = %#v (nil bridge -> auth_failure beklenir)", env.Error)
	}
	// login.poll: bos state -> OK ama Status=error (bridge'e dokunmadan)
	raw, _ = m.HandleCall(pluginabi.MethodAuthLoginPoll, []byte(`{}`))
	{
		var poll pluginapi.AuthLoginPollResponse
		decodeResult(t, raw, &poll)
		if poll.Status != pluginapi.AuthLoginStatusError {
			t.Fatalf("login.poll status = %q, want error", poll.Status)
		}
	}
	raw, err = m.HandleCall(pluginabi.MethodAuthRefresh, mustJSON(struct{ pluginapi.AuthRefreshRequest }{pluginapi.AuthRefreshRequest{AuthID: "id", AuthProvider: ProviderID, Attributes: map[string]string{"api_key": "sk-refresh-1"}}}))
	if err != nil {
		t.Fatal(err)
	}
	var refreshed pluginapi.AuthRefreshResponse
	decodeResult(t, raw, &refreshed)
	if refreshed.Auth.ID != "id" || refreshed.Auth.Attributes["api_key"] != "sk-refresh-1" {
		t.Fatalf("refresh dispatch = %#v", refreshed)
	}
}

func TestAuthParseOAuthCredential(t *testing.T) {
	p := authProvider{}
	raw := `{"type":"opencode-go","access_token":"tok-1","refresh_token":"ref-1","expires_at_ms":9999999999999,` +
		`"email":"user@x.com","org_id":"org-9","org_name":"MyOrg","console_url":"https://console.test"}`
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		Provider: ProviderID, FileName: "oauth.json", RawJSON: []byte(raw),
	})
	if err != nil || !parsed.Handled {
		t.Fatalf("oauth parse = %#v err=%v", parsed, err)
	}
	if parsed.Auth.Attributes["api_key"] != "tok-1" {
		t.Fatalf("api_key attr = %q", parsed.Auth.Attributes["api_key"])
	}
	if got := parsed.Auth.Attributes["base_url"]; got != "https://opencode.ai/inference/go/openai/v1" {
		t.Fatalf("base_url override = %q (OAuth -> inference/go)", got)
	}
	if parsed.Auth.Attributes["org_id"] != "org-9" {
		t.Fatalf("org_id = %q", parsed.Auth.Attributes["org_id"])
	}
	if !strings.Contains(string(parsed.Auth.StorageJSON), "tok-1") || !strings.Contains(string(parsed.Auth.StorageJSON), "ref-1") {
		t.Fatalf("storage did not normalize oauth tokens: %s", parsed.Auth.StorageJSON)
	}
	if parsed.Auth.NextRefreshAfter.IsZero() {
		t.Fatal("oauth credential should schedule a refresh")
	}
}

func TestAuthParseAPIKeyHasNoBaseOverride(t *testing.T) {
	p := authProvider{}
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		Provider: ProviderID, FileName: "k.json",
		RawJSON: []byte(`{"type":"opencode-go","api_key":"sk-x"}`),
	})
	if err != nil || !parsed.Handled {
		t.Fatalf("parse = %#v err=%v", parsed, err)
	}
	if _, ok := parsed.Auth.Attributes["base_url"]; ok {
		t.Fatal("api-key credential must not override base_url (stays on zen/go)")
	}
}

func TestAuthDeviceLoginFlow(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(payload, &wire)
		switch wire.URL {
		case "https://console.test/auth/device/code":
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"device_code":"dev-1","user_code":"ABCD","verification_uri_complete":"https://console.test/device?code=ABCD","expires_in":600,"interval":5}`)}), nil
		case "https://console.test/auth/device/token":
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"access_token":"tok-d","refresh_token":"ref-d","expires_in":3600,"org_id":"org-d"}`)}), nil
		case "https://console.test/api/user":
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"u1","email":"dev@x.com"}`)}), nil
		case "https://console.test/api/orgs":
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`[{"id":"org-d","name":"DevOrg"}]`)}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: config.DefaultBaseURL, ConsoleURL: "https://console.test", OAuthClientID: "opencode-cli", UserAgent: "opencode/1.19.5"}

	start, err := m.authLoginStart("cb-1", pluginapi.AuthLoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if start.Provider != ProviderID || start.URL != "https://console.test/device?code=ABCD" || start.State == "" {
		t.Fatalf("start = %+v", start)
	}

	poll, err := m.authLoginPoll("cb-1", pluginapi.AuthLoginPollRequest{Provider: ProviderID, State: start.State, Metadata: start.Metadata})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess || poll.Auth.Attributes["api_key"] != "tok-d" {
		t.Fatalf("poll = %+v", poll)
	}
	if poll.Auth.Attributes["org_id"] != "org-d" || poll.Auth.Attributes["base_url"] != "https://opencode.ai/inference/go/openai/v1" {
		t.Fatalf("poll attrs = %+v", poll.Auth.Attributes)
	}
	if poll.Auth.Label == "" || poll.Auth.Metadata["email"] != "dev@x.com" {
		t.Fatalf("poll identity = %+v", poll.Auth)
	}
	if _, ok := pendingLoginStates.Load(start.State); ok {
		t.Fatal("pending state not cleared after success")
	}
}

func TestAuthLoginPollPending(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(payload, &wire)
		if wire.URL == "https://console.test/auth/device/code" {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"device_code":"dev-p","user_code":"WXYZ","verification_uri_complete":"/device?code=WXYZ","expires_in":600,"interval":5}`)}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"error":"authorization_pending"}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: config.DefaultBaseURL, ConsoleURL: "https://console.test", OAuthClientID: "opencode-cli", UserAgent: "opencode/1.19.5"}
	start, err := m.authLoginStart("cb", pluginapi.AuthLoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	poll, err := m.authLoginPoll("cb", pluginapi.AuthLoginPollRequest{Provider: ProviderID, State: start.State, Metadata: start.Metadata})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("poll = %+v err=%v (want pending)", poll, err)
	}
}

func TestAuthRefreshOAuth(t *testing.T) {
	const refresh = "ref-live"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"access_token":"tok-new","refresh_token":"ref-new","expires_in":3600}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: config.DefaultBaseURL, ConsoleURL: "https://console.test", OAuthClientID: "opencode-cli", UserAgent: "opencode/1.19.5"}
	storage := []byte(`{"type":"opencode-go","access_token":"tok-old","refresh_token":"` + refresh + `","console_url":"https://console.test","expires_at_ms":1}`)
	resp, err := m.authRefresh("cb", pluginapi.AuthRefreshRequest{AuthID: "id", AuthProvider: ProviderID, StorageJSON: storage})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Auth.Attributes["api_key"] != "tok-new" {
		t.Fatalf("refresh attrs = %+v (want tok-new)", resp.Auth.Attributes)
	}
	if !strings.Contains(string(resp.Auth.StorageJSON), "ref-new") {
		t.Fatal("refresh did not rotate refresh_token in storage")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func bytesContain(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}
