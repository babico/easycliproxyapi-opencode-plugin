package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"easycliproxyapi-opencode-plugin/internal/config"
)

// quotaProvider — native CPA Quota Management sayfasina entegrasyon
// (pluginapi.QuotaProvider, v8 SDK). Onceki kisisel management sayfasi yerine:
// host, her opencode-go credential'i icin quota.fetch cagirir ve kotayi
// standart Quota kartinda gosterir. Kaynak ayni: {base-url}/usage endpoint'i
// (rolling/weekly/monthly, percent + resetsAt).

type quotaProvider struct {
	m *Manager
}

func (quotaProvider) Identifier() string { return ProviderID }

func (quotaProvider) DescribeQuota(_ context.Context, _ pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderID},
		DisplayName:        "OpenCode Go",
		SupportsReset:      false,
	}, nil
}

func (p quotaProvider) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	m := p.m
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	keys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	m.mu.RUnlock()

	// credential'in api_key'i: host Attributes'te; yoksa StorageJSON'dan;
	// hala yoksa AuthID ile config poolundaki kaydan esles
	key := strings.TrimSpace(req.Attributes["api_key"])
	if key == "" {
		key = apiKeyFromStorage(req.StorageJSON)
	}
	if key == "" {
		for _, k := range keys {
			if authRecordID(k.Value) == req.AuthID {
				key = k.Value
				break
			}
		}
	}
	if key == "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("no opencode-go credential available for quota fetch")
	}

	usage, err := fetchQuota(ctx, m.bridge, baseURL, timeout, key)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "go", TierName: "OpenCode Go", TierID: "go"},
		Groups: []pluginapi.QuotaGroup{{
			DisplayName: "OpenCode Go",
			Buckets: []pluginapi.QuotaBucket{
				quotaBucket("5h", usage.Rolling, "Rolling 5-hour window"),
				quotaBucket("weekly", usage.Weekly, "Weekly window"),
				quotaBucket("monthly", usage.Monthly, "Monthly window"),
			},
		}},
	}, nil
}

func (quotaProvider) ResetQuota(_ context.Context, _ pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "OpenCode Go does not support quota reset"}, nil
}

func quotaBucket(window string, w quotaWindow, desc string) pluginapi.QuotaBucket {
	pct := float64(w.Percent) / 100
	if pct > 1 {
		pct = 1
	}
	if pct < 0 {
		pct = 0
	}
	return pluginapi.QuotaBucket{
		Window:            window,
		RemainingFraction: 1 - pct,
		ResetTime:         w.ResetsAt,
		Description:       fmt.Sprintf("%s — %d%% used", desc, w.Percent),
	}
}

func apiKeyFromStorage(storage []byte) string {
	if len(storage) == 0 {
		return ""
	}
	var s struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(storage, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s.APIKey)
}

func quotaIdentity(key string) (id, label string) {
	id = authRecordID(key)
	return id, "OpenCode Go credential " + id[len(managedAuthPrefix):][:12]
}

func fetchQuota(ctx context.Context, bridge *HostBridge, baseURL string, timeout time.Duration, key string) (quotaUsage, error) {
	if bridge == nil {
		return quotaUsage{}, fmt.Errorf("quota bridge unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := bridge.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: strings.TrimRight(baseURL, "/") + "/usage", Headers: http.Header{"Authorization": []string{"Bearer " + key}, "Accept": []string{"application/json"}}})
	if err != nil || resp.StatusCode != http.StatusOK {
		return quotaUsage{}, fmt.Errorf("quota upstream request failed")
	}
	var decoded usageResponse
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		return quotaUsage{}, fmt.Errorf("quota response invalid")
	}
	return quotaUsage{
		Rolling: quotaWindow{Status: decoded.Usage.Rolling.Status, Percent: decoded.Usage.Rolling.Percent, ResetsAt: decoded.Usage.Rolling.ResetsAt},
		Weekly:  quotaWindow{Status: decoded.Usage.Weekly.Status, Percent: decoded.Usage.Weekly.Percent, ResetsAt: decoded.Usage.Weekly.ResetsAt},
		Monthly: quotaWindow{Status: decoded.Usage.Monthly.Status, Percent: decoded.Usage.Monthly.Percent, ResetsAt: decoded.Usage.Monthly.ResetsAt},
	}, nil
}

type quotaWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resets_at"`
}

type quotaUsage struct {
	Rolling quotaWindow `json:"rolling"`
	Weekly  quotaWindow `json:"weekly"`
	Monthly quotaWindow `json:"monthly"`
}

type usageResponse struct {
	Usage struct {
		Rolling quotaUpstreamWindow `json:"rolling"`
		Weekly  quotaUpstreamWindow `json:"weekly"`
		Monthly quotaUpstreamWindow `json:"monthly"`
	} `json:"usage"`
}

type quotaUpstreamWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}
