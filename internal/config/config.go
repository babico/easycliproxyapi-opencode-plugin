// Package config loads and validates the opencode-go plugin configuration
// per spec 04 (configuration) and spec 05 §2 (HTTPS/timeouts).
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (spec 04 §2/§4).
const (
	DefaultBaseURL          = "https://opencode.ai/zen/go/v1"
	DefaultModelPrefix      = "opencode-go"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB

	// OAuth device-login defaults (console + inference endpoints). API-key
	// credentials route to BaseURL (zen/go/v1); OAuth tokens use
	// InferenceBaseURL so both upstream families are supported.
	DefaultConsoleURL        = "https://opencode.ai/console"
	DefaultOAuthClientID     = "opencode-cli"
	DefaultUserAgent         = "opencode/1.19.5" // anomalyco/opencode en yeni release (2026-10-28)
	DefaultSessionID         = "cpa-main"
	DefaultInferenceBaseURL  = "https://opencode.ai/inference/go"

	// Default upstream plans. "go" = OpenCode Go subscription catalog
	// (zen/go/v1); "zen" = the broader OpenCode Zen catalog (zen/v1).
	DefaultPlanName    = "go"
	DefaultPlanPrefix  = "opencode-go"
	DefaultZenBaseURL  = "https://opencode.ai/zen/v1"
	DefaultZenName     = "zen"
	DefaultZenPrefix   = "opencode-zen"
)

type ModelPrefix struct {
	Enabled bool
	Value   string
}

// Plan is one upstream OpenCode endpoint family. OpenCode "Go" (the paid
// plan) and "Zen" (the broader/free-hosted catalog) are DIFFERENT endpoints
// with different catalogs; a shared account credential serves both. When the
// config omits `plans`, Load synthesizes a single plan from the legacy
// base-url/catalog-url/model-prefix fields (backward compatible).
type Plan struct {
	Name       string
	BaseURL    string
	CatalogURL string
	Prefix     ModelPrefix
	Enabled    bool
}

type APIKey struct {
	Value string
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool
	Messages        bool
	Responses       bool
}

type RouteOverride struct {
	Protocol string `yaml:"protocol"`
	Endpoint string `yaml:"endpoint"`
}

type Config struct {
	BaseURL          string
	CatalogURL       string
	ModelPrefix      ModelPrefix
	APIKeys          []APIKey
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64

	// OAuth / login
	ConsoleURL        string
	OAuthClientID     string
	UserAgent         string
	SessionID         string
	InferenceBaseURL  string

	// Plans is the set of upstream OpenCode endpoint families (go + zen).
	// Always non-empty after Load (synthesized from legacy fields if unset).
	Plans []Plan
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	BaseURL          *string                  `yaml:"base-url"`
	CatalogURL       *string                  `yaml:"catalog-url"`
	ModelPrefix      rawPrefix                `yaml:"model-prefix"`
	APIKeys          []rawKey                 `yaml:"api-keys"`
	Catalog          rawCatalog               `yaml:"catalog"`
	Protocols        rawProtocols             `yaml:"protocols"`
	RouteOverrides   map[string]RouteOverride `yaml:"route-overrides"`
	AllowHTTP        bool                     `yaml:"allow-http"`
	RequestTimeout   *string                  `yaml:"request-timeout"`
	MaxResponseBytes *int64                   `yaml:"max-response-bytes"`

	ConsoleURL        *string `yaml:"console-url"`
	OAuthClientID     *string `yaml:"oauth-client-id"`
	UserAgent         *string `yaml:"user-agent"`
	SessionID         *string `yaml:"session-id"`
	InferenceBaseURL  *string `yaml:"inference-base-url"`
	Plans             []rawPlan `yaml:"plans"`
}

type rawPlan struct {
	Name       *string     `yaml:"name"`
	BaseURL    *string     `yaml:"base-url"`
	CatalogURL *string     `yaml:"catalog-url"`
	Prefix     rawPrefix   `yaml:"model-prefix"`
	Enabled    *bool       `yaml:"enabled"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Value string `yaml:"value"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

type rawProtocols struct {
	ChatCompletions *bool `yaml:"chat-completions"`
	Messages        *bool `yaml:"messages"`
	Responses       *bool `yaml:"responses"`
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true}
)

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates (spec 04 §6). Decode errors never echo
// decoded node values — a malformed entry (e.g. a bare-scalar API key)
// must not leak into the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: os.ExpandEnv(k.Value)}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	c := Config{
		BaseURL: orDefault(raw.BaseURL, DefaultBaseURL),
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: Protocols{
			ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
			Messages:        orDefault(raw.Protocols.Messages, true),
			Responses:       orDefault(raw.Protocols.Responses, true),
		},
		RouteOverrides:   raw.RouteOverrides,
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),

		ConsoleURL:       orDefault(raw.ConsoleURL, DefaultConsoleURL),
		OAuthClientID:    orDefault(raw.OAuthClientID, DefaultOAuthClientID),
		UserAgent:        orDefault(raw.UserAgent, DefaultUserAgent),
		SessionID:        orDefault(raw.SessionID, DefaultSessionID),
		InferenceBaseURL: orDefault(raw.InferenceBaseURL, DefaultInferenceBaseURL),
	}
	if raw.CatalogURL != nil {
		// Mirror the derived-default trim so an explicit trailing-slash
		// catalog-url cannot double up separators downstream.
		c.CatalogURL = strings.TrimRight(*raw.CatalogURL, "/")
	} else {
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	c.Plans = BuildPlans(raw.Plans, c.BaseURL, c.CatalogURL, c.ModelPrefix)
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// buildPlans materializes the upstream plan set. When the operator omits
// `plans`, a single "go" plan is synthesized from the legacy base-url/
// catalog-url/model-prefix fields so existing configs and tests are
// unchanged. When `plans` is present, each entry becomes its own endpoint
// family (e.g. go = zen/go/v1 + zen = zen/v1).
func BuildPlans(rawPlans []rawPlan, legacyBase, legacyCatalog string, legacyPrefix ModelPrefix) []Plan {
	if len(rawPlans) == 0 {
		return []Plan{{
			Name:       DefaultPlanName,
			BaseURL:    legacyBase,
			CatalogURL: legacyCatalog,
			Prefix:     legacyPrefix,
			Enabled:    true,
		}}
	}
	plans := make([]Plan, 0, len(rawPlans))
	for _, rp := range rawPlans {
		enabled := orDefault(rp.Enabled, true)
		base := orDefault(rp.BaseURL, legacyBase)
		catalog := base
		if rp.CatalogURL != nil {
			catalog = strings.TrimRight(*rp.CatalogURL, "/")
		} else {
			catalog = strings.TrimRight(base, "/") + "/models"
		}
		plans = append(plans, Plan{
			Name:       orDefault(rp.Name, DefaultPlanName),
			BaseURL:    strings.TrimRight(base, "/"),
			CatalogURL: catalog,
			Prefix:     ModelPrefix{
				Enabled: orDefault(rp.Prefix.Enabled, true),
				Value:   orDefault(rp.Prefix.Value, DefaultPlanPrefix),
			},
			Enabled: enabled,
		})
	}
	return plans
}

// PublicIDForPlan returns the client-facing model ID under one plan's prefix.
func PublicIDForPlan(p Plan, upstreamID string) string {
	if p.Prefix.Enabled {
		return p.Prefix.Value + "/" + upstreamID
	}
	return upstreamID
}

// PublicID returns the client-facing model ID (FR-003).
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
	}
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	for name, o := range c.RouteOverrides {
		if !validProtocols[o.Protocol] {
			return fmt.Errorf("route-overrides[%s].protocol: unsupported protocol %q", name, o.Protocol)
		}
		if o.Endpoint == "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be empty", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	for _, p := range c.Plans {
		if !p.Enabled {
			continue
		}
		if err := validateURL("plans."+p.Name+".base-url", p.BaseURL, c.AllowHTTP); err != nil {
			return err
		}
		if err := validateURL("plans."+p.Name+".catalog-url", p.CatalogURL, c.AllowHTTP); err != nil {
			return err
		}
		if p.Prefix.Enabled && !validPrefix(p.Prefix.Value) {
			return fmt.Errorf("plans.%s.model-prefix.value: invalid provider-ID characters %q", p.Name, p.Prefix.Value)
		}
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist (spec 05 §2): https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
