// Package catalog fetches the live OpenCode Go /v1/models catalog,
// normalizes metadata, assigns routes, and serves the last good snapshot
// (FR-002, FR-003, FR-004, FR-010).
//
// Route resolution priority follows arch spec §5: user route override >
// built-in compatibility table > unsupported (excluded from the routable set,
// kept in diagnostics).
//
// No scheduling lives here; M3 wires the refresh cadence.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"easycliproxyapi-opencode-plugin/internal/config"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

// Route is the upstream protocol a model is served through (FR-004).
type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
)

// EndpointPath returns the upstream endpoint path for the route (FR-004).
// routeEndpoints maps each route to its upstream endpoint path.
var routeEndpoints = map[Route]string{
	RouteChatCompletions: "/v1/chat/completions",
	RouteMessages:        "/v1/messages",
	RouteResponses:       "/v1/responses",
}

// EndpointPath returns the route's upstream endpoint path, or "" for
// unknown routes.
func (r Route) EndpointPath() string {
	return routeEndpoints[r]
}

func routeFromString(s string) (Route, bool) {
	if _, ok := routeEndpoints[Route(s)]; ok {
		return Route(s), true
	}
	return "", false
}

// ModelRecord is one routable normalized catalog entry (arch §7, FR-003).
// Plan/BaseURL identify the upstream endpoint family the model is served
// from (go = zen/go/v1, zen = zen/v1); the executor routes by these.
type ModelRecord struct {
	PublicID     string
	UpstreamID   string
	DisplayName  string
	Protocol     Route
	EndpointPath string
	ContextLimit int64
	OutputLimit  int64
	InputModes   []string
	OutputModes  []string
	Thinking     *pluginapi.ThinkingSupport
	PlanName     string
	BaseURL      string
}

// planRaw is one upstream plan's decoded catalog entries retained for
// SeedFrom rebuilds; plan metadata (base-url, prefix) is always taken from
// the current config, never from the retained raw.
type planRaw struct {
	PlanName string
	Entries  []rawModel
	Warns    []string
}

// UnsupportedModel is a discovered model excluded from the routable set,
// retained for diagnostics (FR-004, open questions §10).
type UnsupportedModel struct {
	UpstreamID string
	Reason     string
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics); M3's host callback satisfies it directly.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one catalog entry; unknown fields —
// including protocol/endpoint fields the catalog does not provide and
// reasoning/tool_calling flags the plugin does not act on — are ignored via
// json tags. Modality metadata accepts both spellings.
//
// Reasoning capability arrives in one of two shapes: the host-specific
// `thinking` object and/or a `supported_reasoning_levels` list. Both are
// decoded because catalogs publish them independently.
type rawModel struct {
	ID               string              `json:"id"`
	DisplayName      string              `json:"display_name"`
	ContextLength    int64               `json:"context_length"`
	MaxOutputTokens  int64               `json:"max_output_tokens"`
	InputModes       []string            `json:"input_modes"`
	OutputModes      []string            `json:"output_modes"`
	InputModalities  []string            `json:"input_modalities"`
	OutputModalities []string            `json:"output_modalities"`
	Thinking         *rawThinking        `json:"thinking"`
	ReasoningLevels  []rawReasoningLevel `json:"supported_reasoning_levels"`
}

// rawReasoningLevel is one entry of the catalog's declared reasoning-level
// list; only the effort name identifies a usable level.
type rawReasoningLevel struct {
	Effort string `json:"effort"`
}

// rawThinking tolerantly decodes the optional per-model thinking object;
// pluginapi.ThinkingSupport itself is untagged, so snake_case upstream keys
// are decoded here and mapped field-by-field (partial objects decode to a
// partial struct).
type rawThinking struct {
	Min            *int     `json:"min"`
	Max            *int     `json:"max"`
	ZeroAllowed    *bool    `json:"zero_allowed"`
	DynamicAllowed *bool    `json:"dynamic_allowed"`
	Levels         []string `json:"levels"`
}

// prefixRoutes covers unknown variants of known families;
// longest prefix wins (list is ordered longest-first).
var prefixRoutes = []struct {
	prefix string
	route  Route
}{
	{"muse-spark", RouteResponses},
	{"deepseek", RouteChatCompletions},
	{"minimax", RouteMessages},
	{"longcat", RouteChatCompletions},
	{"grok", RouteResponses},
	{"gpt", RouteResponses},
	{"kimi", RouteChatCompletions},
	{"qwen", RouteMessages},
	{"glm", RouteChatCompletions},
	{"mimo", RouteChatCompletions},
	{"hy", RouteChatCompletions},
}

// Manager owns the catalog snapshot. All accessors are safe for
// concurrent use alongside Refresh.
type Manager struct {
	cfg    config.Config
	client HostClient

	mu sync.Mutex
	// raw retains the decoded upstream entries behind the current snapshot,
	// grouped by plan name, so SeedFrom can rebuild records against a NEW
	// config (route overrides, protocol flags, and prefix settings may all
	// have changed since the snapshot was built).
	raw    []planRaw
	models []ModelRecord
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1); rebuilt atomically with models on every swap.
	index map[string]ModelRecord
	unsup []UnsupportedModel
	warns []string
}

// New returns a Manager serving cfg through the host client. When cfg.Plans
// is empty (direct construction bypassing config.Load) it synthesizes the
// single legacy plan from base-url/catalog-url/model-prefix so Refresh always
// has at least one endpoint to fetch.
func New(cfg config.Config, client HostClient) *Manager {
	if len(cfg.Plans) == 0 {
		cfg.Plans = config.BuildPlans(nil, cfg.BaseURL, cfg.CatalogURL, cfg.ModelPrefix)
	}
	return &Manager{cfg: cfg, client: client}
}

// SeedFrom republishes prev's last-good snapshot into m by REBUILDING every
// record from prev's raw upstream entries through the same
// resolution/validation path a refresh applies, but against m's OWN cfg:
// routes re-resolve (a removed override drops its model), the protocolEnabled
// gate re-applies, PublicIDs recompute under m's prefix, endpoints re-check
// against m's base-url, and collisions/diagnostics regenerate (arch §5).
//
// m keeps its OWN cfg: subsequent Refresh calls use m's base-url/catalog-url/
// protocols/prefix (F5: a failed-refresh reconfigure must adopt the new
// manager rather than keep ticking the old URL). Because seeding rebuilds
// instead of copying, a config change between snapshots takes effect at seed
// time — e.g. a protocol kill-switch flipped during a catalog outage demotes
// its routes immediately instead of serving stale routable records until the
// outage ends.
func (m *Manager) SeedFrom(prev *Manager) {
	prev.mu.Lock()
	raw := prev.raw
	prev.mu.Unlock()
	m.swap(raw)
}

// Refresh fetches and swaps the catalog snapshot. On failure it returns a
// classified, key-free error; the previous snapshot keeps serving while
// catalog.stale-while-unavailable is enabled, otherwise the routable set
// is cleared until the next success (FR-002).
func (m *Manager) Refresh(ctx context.Context, apiKey string) error {
	if m.client == nil {
		// Only direct construction can produce a nil host client
		// (production always wires the bridge); fail loudly through the
		// classified path instead of panicking inside Do.
		return m.fail("host client unavailable")
	}
	var raws []planRaw
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	for _, plan := range m.cfg.Plans {
		if !plan.Enabled {
			continue
		}
		req := pluginapi.HTTPRequest{
			Method: http.MethodGet,
			URL:    plan.CatalogURL,
			Headers: http.Header{
				"Authorization": []string{"Bearer " + apiKey},
				"Accept":        []string{"application/json"},
			},
		}
		resp, err := m.client.Do(ctx, req)
		if err != nil {
			return m.fail("network error")
		}
		if int64(len(resp.Body)) > budget {
			return m.fail("response exceeds max-response-bytes")
		}
		if resp.StatusCode != http.StatusOK {
			return m.fail(fmt.Sprintf("http %d", resp.StatusCode))
		}
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &env); err != nil {
			return m.fail("invalid json")
		}
		var entries []rawModel
		var warns []string
		if env.Data == nil {
			// Key ABSENCE is upstream shape drift, never an intended clear:
			// clear like an empty plan (swap(nil)) but leave a diagnostic.
			// A present-but-empty array stays silent (tested-intended state).
			warns = append(warns, fmt.Sprintf(`plan %q catalog response missing "data" field`, plan.Name))
		} else if err := json.Unmarshal(env.Data, &entries); err != nil {
			return m.fail("invalid json")
		}
		raws = append(raws, planRaw{PlanName: plan.Name, Entries: entries, Warns: warns})
	}
	m.swap(raws)
	return nil
}

// fail applies the stale policy and returns the classified error.
func (m *Manager) fail(category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Catalog.StaleWhileUnavailable {
		// Clear the index too — Lookup must stop resolving IDs whose
		// records are gone (FR-002). Raw entries go with them: a cleared
		// snapshot has nothing to seed.
		m.raw, m.models, m.index, m.unsup, m.warns = nil, nil, nil, nil, nil
	}
	return fmt.Errorf("catalog refresh failed: %s", category)
}

// swap builds the new snapshot from decoded per-plan entries and installs it
// atomically under the mutex (FR-010 dedup, arch §5 route priority). Each plan
// contributes models under its own prefix and routes them to its own base-url.
func (m *Manager) swap(raws []planRaw) {
	total := 0
	for _, r := range raws {
		total += len(r.Entries)
	}
	models := make([]ModelRecord, 0, total)
	index := make(map[string]ModelRecord, total*2)
	var unsup []UnsupportedModel
	var warns []string
	upstreamSeen := make(map[string]bool, total)
	// plansByName resolves retained raw back to CURRENT config plan metadata,
	// so a reconfigure that changes a plan's base-url/prefix takes effect at
	// seed time (F5) rather than serving stale routing.
	plansByName := make(map[string]config.Plan, len(m.cfg.Plans))
	for _, p := range m.cfg.Plans {
		if p.Enabled {
			plansByName[p.Name] = p
		}
	}
	for _, raw := range raws {
		warns = append(warns, raw.Warns...)
		plan, ok := plansByName[raw.PlanName]
		if !ok {
			// Plan disabled/removed since the snapshot was built: drop its
			// models rather than route them to a stale base-url.
			continue
		}
		seen := make(map[string]bool, len(raw.Entries))
		for _, e := range raw.Entries {
			if e.ID == "" {
				unsup = append(unsup, UnsupportedModel{Reason: "missing id"})
				continue
			}
			if seen[e.ID] {
				warns = append(warns, fmt.Sprintf("duplicate model %q ignored; keeping first occurrence", e.ID))
				continue
			}
			seen[e.ID] = true
			route, endpoint, rok, warn := m.resolve(e)
			if warn != "" {
				warns = append(warns, warn)
			}
			if !rok {
				unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: "no route determined"})
				continue
			}
			if !m.protocolEnabled(route) {
				// Flags apply at snapshot build; a stale snapshot kept per
				// FR-002 reflects the flags it was built with until the next
				// successful refresh.
				unsup = append(unsup, UnsupportedModel{
					UpstreamID: e.ID,
					Reason:     fmt.Sprintf("protocol %s disabled by config", route),
				})
				continue
			}
			if endpoint != "" && endpointEscapesBase(plan.BaseURL, endpoint) {
				// The joined URL must stay on the configured upstream
				// authority so overrides cannot rewrite the host and send
				// credentials/prompts to an attacker (spec 05 §2).
				unsup = append(unsup, UnsupportedModel{
					UpstreamID: e.ID,
					Reason:     "resolved endpoint escapes the configured upstream host",
				})
				continue
			}
			if endpoint == "" {
				endpoint = route.EndpointPath()
			}
			inputModes := firstNonEmpty(e.InputModes, e.InputModalities)
			outputModes := firstNonEmpty(e.OutputModes, e.OutputModalities)
			display := e.DisplayName
			if display == "" {
				display = e.ID
			}
			rec := ModelRecord{
				PublicID:     config.PublicIDForPlan(plan, e.ID),
				UpstreamID:   e.ID,
				DisplayName:  display,
				Protocol:     route,
				EndpointPath: endpoint,
				ContextLimit: e.ContextLength,
				OutputLimit:  e.MaxOutputTokens,
				InputModes:   inputModes,
				OutputModes:  outputModes,
				Thinking:     normalizeThinking(e.Thinking, e.ReasoningLevels),
				PlanName:     plan.Name,
				BaseURL:      plan.BaseURL,
			}
			// PublicID is unique across plans (distinct prefixes), so it is
			// always safe to index. UpstreamID is indexed only on first sight
			// because the SAME upstream id can legitimately live on two plans
			// (go + zen); bare-id lookup is a convenience, never canonical.
			if other, pok := index[rec.PublicID]; ok && pok {
				reason := fmt.Sprintf("public id %q collides with upstream id %q", rec.PublicID, other.UpstreamID)
				warns = append(warns, reason)
				unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
				continue
			}
			if !upstreamSeen[rec.UpstreamID] {
				if other, iok := index[rec.UpstreamID]; iok {
					reason := fmt.Sprintf("public id %q collides with upstream id %q", other.PublicID, rec.UpstreamID)
					warns = append(warns, reason)
					unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
					continue
				}
				upstreamSeen[rec.UpstreamID] = true
			}
			models = append(models, rec)
			index[rec.PublicID] = rec
			index[rec.UpstreamID] = rec
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw, m.models, m.index, m.unsup, m.warns = raws, models, index, unsup, warns
}

// protocolEnabled reports whether the resolved route's protocol flag is on
// (spec 04 §4); flags default to true.
func (m *Manager) protocolEnabled(r Route) bool {
	switch r {
	case RouteChatCompletions:
		return m.cfg.Protocols.ChatCompletions
	case RouteMessages:
		return m.cfg.Protocols.Messages
	case RouteResponses:
		return m.cfg.Protocols.Responses
	}
	return true
}

// resolve applies route priority strictly: user override, compatibility
// table (family prefix), none (arch §5).
func (m *Manager) resolve(e rawModel) (Route, string, bool, string) {
	if o, ok := m.cfg.RouteOverrides[e.ID]; ok {
		// Spec 04 §5: an override MUST be visible in diagnostics. Load
		// validation (spec 04 §6) guarantees a known protocol, so the
		// map lookup above is the only decision point here.
		return Route(o.Protocol), o.Endpoint, true, fmt.Sprintf(
			"model %q routed via user override (%s %s)", e.ID, o.Protocol, o.Endpoint)
	}
	id := strings.ToLower(e.ID)
	for _, p := range prefixRoutes {
		if strings.HasPrefix(id, p.prefix) {
			return p.route, "", true, ""
		}
	}
	return "", "", false, ""
}

// JoinUpstreamURL joins the configured base-url onto an endpoint path and
// is the single source of truth for that join (spec 05 §2): base-url
// already carries the version segment (default .../zen/go/v1, catalog at
// {base}/models), so a leading "/v1" on the endpoint is dropped instead of
// doubled. The authority-escape check (endpointEscapesBase) must parse
// exactly the URL this produces — two independent join formulas would be
// free to drift apart and validate one URL while sending another.
func JoinUpstreamURL(baseURL, endpoint string) string {
	return strings.TrimSuffix(baseURL, "/") + strings.TrimPrefix(endpoint, "/v1")
}

// endpointEscapesBase reports whether joining baseURL with endpoint via
// JoinUpstreamURL would send the request to a different authority or scheme
// than the configured base-url. Any parse failure counts as escaping (fail
// closed).
func endpointEscapesBase(baseURL, endpoint string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	got, err := url.Parse(JoinUpstreamURL(baseURL, endpoint))
	if err != nil {
		return true
	}
	return !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// normalizeThinking exposes the catalog's declared reasoning metadata in
// pluginapi form. Merge the two catalog shapes without inferring levels;
// this metadata is informational and never gates request effort values.
func normalizeThinking(rt *rawThinking, declared []rawReasoningLevel) *pluginapi.ThinkingSupport {
	if rt == nil && len(declared) == 0 {
		return nil
	}
	t := &pluginapi.ThinkingSupport{}
	var declaredLevels []string
	if rt != nil {
		if rt.Min != nil {
			t.Min = *rt.Min
		}
		if rt.Max != nil {
			t.Max = *rt.Max
		}
		if rt.ZeroAllowed != nil {
			t.ZeroAllowed = *rt.ZeroAllowed
		}
		if rt.DynamicAllowed != nil {
			t.DynamicAllowed = *rt.DynamicAllowed
		}
		declaredLevels = rt.Levels
	}
	var listLevels []string
	for _, level := range declared {
		listLevels = append(listLevels, level.Effort)
	}
	t.Levels = mergeLevels(declaredLevels, listLevels)
	return t
}

// mergeLevels concatenates level groups, trimming blanks and dropping
// case-insensitive duplicates; the first spelling of a level wins so an
// explicit `thinking.levels` entry keeps its casing.
func mergeLevels(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range groups {
		for _, raw := range group {
			level := strings.TrimSpace(raw)
			if level == "" {
				continue
			}
			key := strings.ToLower(level)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, level)
		}
	}
	return out
}

// Models returns the routable snapshot (FR-003 identity fields). The slice
// is a shallow copy; records are rebuilt fresh on every swap and consumers
// treat them as read-only, so nested slices/thinking are shared safely.
func (m *Manager) Models() []ModelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelRecord, len(m.models))
	copy(out, m.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID
// (with or without prefix) or the bare upstream ID (FR-003): the PublicID
// key wins over UpstreamID when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.unsup))
	copy(out, m.unsup)
	return out
}

// Warnings returns dedup and override diagnostics.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.warns))
	copy(out, m.warns)
	return out
}
