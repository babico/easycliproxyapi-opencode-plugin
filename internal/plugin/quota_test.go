package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"easycliproxyapi-opencode-plugin/internal/config"
)

func TestQuotaProviderCapability(t *testing.T) {
	m := NewManager(nil)
	var reg registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &reg)
	if !reg.Capabilities.QuotaProvider {
		t.Fatal("registration did not advertise quota_provider")
	}
}

func TestQuotaDescribe(t *testing.T) {
	m := NewManager(nil)
	got, err := (quotaProvider{m}).DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SupportedProviders) != 1 || got.SupportedProviders[0] != ProviderID {
		t.Fatalf("SupportedProviders = %+v", got.SupportedProviders)
	}
	if got.DisplayName != "OpenCode Go" || got.SupportsReset {
		t.Fatalf("describe = %+v", got)
	}
}

func TestQuotaFetchFromAttribute(t *testing.T) {
	const key = "quota-fetch-secret"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Method != http.MethodGet ||
			wire.URL != "https://quota.test/v1/usage" ||
			wire.Headers.Get("Authorization") != "Bearer "+key ||
			wire.Headers.Get("Accept") != "application/json" {
			t.Fatalf("bad quota request: %s", payload)
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"rolling":{"status":"ok","percent":9,"resetsAt":"2026-09-05T12:20:04Z"},"weekly":{"status":"ok","percent":12,"resetsAt":"2026-09-10T00:00:00Z"},"monthly":{"status":"ok","percent":6,"resetsAt":"2026-10-01T00:00:00Z"}}}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1/", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}

	req := pluginapi.QuotaFetchRequest{AuthID: authRecordID(key), Provider: ProviderID, Attributes: map[string]string{"api_key": key}}
	resp, err := (quotaProvider{m}).FetchQuota(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 3 {
		t.Fatalf("groups = %+v", resp.Groups)
	}
	b := resp.Groups[0].Buckets
	// remainingFraction = 1 - percent/100
	if want := 0.91; b[0].Window != "5h" || absF(b[0].RemainingFraction-want) > 1e-9 || b[0].ResetTime != "2026-09-05T12:20:04Z" {
		t.Fatalf("rolling bucket = %+v", b[0])
	}
	if b[1].Window != "weekly" || absF(b[1].RemainingFraction-0.88) > 1e-9 {
		t.Fatalf("weekly bucket = %+v", b[1])
	}
	if b[2].Window != "monthly" || absF(b[2].RemainingFraction-0.94) > 1e-9 {
		t.Fatalf("monthly bucket = %+v", b[2])
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "go" {
		t.Fatalf("subscription = %+v", resp.Subscription)
	}
}

func TestQuotaFetchResolvesConfigPoolByAuthID(t *testing.T) {
	const key = "pool-resolve-secret"
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"x"},"weekly":{"status":"ok","percent":0,"resetsAt":"x"},"monthly":{"status":"ok","percent":0,"resetsAt":"x"}}}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
	// Attributes/StorageJSON bos; AuthID ile config pool'dan esles
	resp, err := (quotaProvider{m}).FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{AuthID: authRecordID(key), Provider: ProviderID})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 1 || len(resp.Groups) != 1 {
		t.Fatalf("calls=%v groups=%+v", f.callsOf(pluginabi.MethodHostHTTPDo), resp.Groups)
	}
}

func TestQuotaFetchFromStorage(t *testing.T) {
	const key = "storage-secret"
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"x"},"weekly":{"status":"ok","percent":1,"resetsAt":"x"},"monthly":{"status":"ok","percent":1,"resetsAt":"x"}}}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout}
	// StorageJSON'dan api_key cikar
	resp, err := (quotaProvider{m}).FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: ProviderID, StorageJSON: []byte(`{"type":"opencode-go","api_key":"` + key + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 1 || len(resp.Groups) != 1 {
		t.Fatalf("calls=%v", f.callsOf(pluginabi.MethodHostHTTPDo))
	}
}

func TestQuotaFetchNoCredential(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout}
	if _, err := (quotaProvider{m}).FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: ProviderID}); err == nil || !strings.Contains(err.Error(), "no opencode-go credential") {
		t.Fatalf("expected no-credential error, got %v", err)
	}
}

func TestQuotaResetUnsupported(t *testing.T) {
	m := NewManager(nil)
	resp, err := (quotaProvider{m}).ResetQuota(context.Background(), pluginapi.QuotaResetRequest{Provider: ProviderID})
	if err != nil || resp.Success {
		t.Fatalf("reset = %+v err=%v (must be unsupported)", resp, err)
	}
}

func TestQuotaErrorsAreRedacted(t *testing.T) {
	const key = "quota-error-secret"
	for name, responder := range map[string]func(string, []byte) ([]byte, error){
		"bridge": func(string, []byte) ([]byte, error) { return nil, context.Canceled },
		"status": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(key + " upstream body")}), nil
		},
		"json": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(key + " not json")}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{responder: responder}
			m := NewManager(NewHostBridge(f.call))
			m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
			_, err := (quotaProvider{m}).FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": key}})
			if err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "upstream body") {
				t.Fatalf("error leaked upstream detail: %v", err)
			}
		})
	}
}

func TestQuotaBucketClampsPercent(t *testing.T) {
	if got := quotaBucket("5h", quotaWindow{Percent: 140}, "x").RemainingFraction; got != 0 {
		t.Fatalf("percent>100 must clamp remaining to 0, got %v", got)
	}
	if got := quotaBucket("5h", quotaWindow{Percent: -5}, "x").RemainingFraction; got != 1 {
		t.Fatalf("percent<0 must clamp remaining to 1, got %v", got)
	}
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func mustHashPrefix(key string) string {
	_, label := quotaIdentity(key)
	return strings.TrimPrefix(label, "OpenCode Go credential ")
}
