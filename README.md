# OpenCode Go CLIProxyAPI Plugin

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) that exposes OpenCode Go as a single provider (`opencode-go`).

The plugin unifies model discovery, protocol translation, and execution across OpenCode Go's upstream endpoints while leveraging CLIProxyAPI's built-in authentication, scheduling, keys rotation, and cooldown management.

## The Problem

OpenCode Go exposes models across multiple API protocols (OpenAI Chat Completions `/v1/chat/completions`, Anthropic Messages `/v1/messages`, and OpenAI Responses `/v1/responses`).

Without this plugin, using OpenCode Go in CLIProxyAPI requires configuring separate provider blocks for each protocol family. This leads to:
- **Duplicated configuration & keys**: The same API keys must be configured across multiple provider blocks.
- **Fragmented scheduling & rotation**: Keys rotation, rate limits, and cooldowns cannot be shared across protocols—exhausting quota on one protocol does not coordinate with another.
- **Client protocol burden**: Clients must know beforehand which upstream protocol and endpoint each model requires.
- **Fragmented catalog**: Models are split across disjoint provider namespaces instead of a unified model list.

## The Solution

This plugin exposes OpenCode Go as a single provider (`opencode-go`) backed by a shared keys pool:
- **Unified auth pool**: Configure keys once; CLIProxyAPI schedules, rotates, and cools down keys across all protocols.
- **Transparent protocol translation & routing**: Clients request models (e.g. `opencode-go/glm-5.2`, `opencode-go/gpt-5.6-luna`) without needing to know the upstream protocol format.
- **Single model catalog**: All models are discovered and published under the `opencode-go` provider namespace in `/v1/models`.

## Features

- **Single Provider Namespace**: Exposes models under the `opencode-go` provider prefix (e.g. `opencode-go/glm-5.2`, `opencode-go/qwen3.7-max`, `opencode-go/gpt-5.6-luna`).
- **Multi-Protocol Translation**: Translates requests and streaming responses between client formats and upstream endpoints:
  - OpenAI Chat Completions (`/v1/chat/completions`)
  - Anthropic Messages (`/v1/messages`)
  - OpenAI Responses (`/v1/responses`)
- **Reasoning Effort Pass-through**: Forwards explicit effort values unchanged, without local capability checks, normalization, or downgrade. The upstream owns validation.
- **Explainable failures**: a streaming request refused before the first byte reports the upstream status and up to 2 KiB of its error body. Reason text is preserved; ambiguous JSON request-fault markers on eligible 401/403/5xx failures are normalized so CPA can try another credential. See [Error reporting and failover](#error-reporting-and-failover).
- **Dynamic Catalog Discovery**: Fetches remote model catalogs with local fallback and custom route overrides.
- **Multi-Key Auth Scheduling**: Pools multiple API keys with CLIProxyAPI's native scheduler for rotation, retries, and error cooldowns across all protocols.
- **Config-authoritative credentials**: `api-keys` is the single source of truth. Every configured key is materialized as a CPA auth record; a key removed from the config has its record **deleted** (the auth file is unlinked, since the plugin ABI has no delete callback, and CPA's auth-directory watcher unregisters it), and putting the key back recreates it. Records the plugin cannot prove it created are never touched. See [Credential reconciliation](#credential-reconciliation).
- **OpenCode Go Quota Page**: Management Center includes a separate `OpenCode Go Quota` page. Page load lists credentials without contacting OpenCode; each card is refreshed manually and independently, and quota values do not affect routing or CPA's native quota page.

## Requirements

- **CLIProxyAPI**: `v7.2.138+`
- **Go Toolchain**: Go 1.24+ (with CGO enabled for C-shared build mode)

## Build

Build the dynamic shared library for your platform:

### Windows (AMD64)
```powershell
go build -buildmode=c-shared -o plugins/windows/amd64/opencode-go-provider.dll .
```

### Linux (AMD64)
```bash
go build -buildmode=c-shared -o plugins/linux/amd64/opencode-go-provider.so .
```

### macOS (ARM64)
```bash
go build -buildmode=c-shared -o plugins/darwin/arm64/opencode-go-provider.dylib .
```

Place the compiled binary into your CLIProxyAPI plugin directory (e.g. `<cliproxyapi_root>/plugins/<os>/<arch>/`).

## Error reporting and failover

Each executor call uses the credential selected by CPA. A refused attempt returns an error to **CPA**, not necessarily to the client: CPA can select another key within the same downstream request. The plugin does not implement a second retry/key-selection loop.

CPA v8.0.32 classifies errors from HTTP status and JSON `type`/`code` fields; its plugin adapter does not consume the envelope's `Retryable` field. For retryable HTTP 401/403/500–599 with matching plugin classes, generic `invalid_request`, `invalid_request_error` and `bad_request_error` markers are normalized at the root, `error`, `response.error` and `body.error`. Types become `authentication_error` (401), `permission_error` (403), or `server_error` (5xx); a generic code becomes the plugin's existing error-class code. HTTP status, envelope metadata, upstream reason text and unrelated JSON values are retained.

Specific request faults (context limits, policy/prompt rejection, invalid/unsupported values, missing previous responses), model-not-found codes, and CPA's existing authentication escape case remain byte-identical. So do 402/429 (CPA already permits credential failover), genuine 400-class request errors, malformed/truncated or duplicate-key JSON, and reports without matching markers. Generic markers remain ambiguous: normalization is a bounded status-based heuristic, not proof that every 403/5xx can be fixed by another key. Network causes and post-emission streaming failures are unchanged; no I/O error is invented to manipulate cooldowns.

## Reasoning effort pass-through

Explicit reasoning levels are opaque upstream values. The plugin does not maintain an allowlist, change their case or whitespace, clamp them to catalog levels, or substitute another level. This applies to streaming and non-streaming requests.

| Upstream protocol | Field receiving the unchanged client value |
|---|---|
| Chat Completions | `reasoning_effort` |
| Responses | `reasoning.effort` |
| Anthropic Messages | `output_config.effort` |

Native requests retain their effort field. Cross-protocol requests only translate the field name; explicit Anthropic `output_config.effort` takes precedence over a numeric thinking budget. When a client supplies only an Anthropic budget and the upstream requires a level, the existing fixed threshold conversion is used without capability clamping. An explicit effort is never converted into a guessed budget, nor does it change sampling settings or output limits.

Catalog reasoning metadata is exposed for clients but is not used as a request allowlist. The upstream can still reject or ignore a value; HTTP success alone does not prove that a requested effort was honored.

## Tool-call id normalization

The upstream Responses endpoint runs DeepSeek in thinking mode and refuses a replayed tool-call history with HTTP 400 `The \`reasoning_text\` in the thinking mode must be passed back to the API.` unless either the turn's reasoning item is replayed, or the call id carries the endpoint's own marker (`call_<NN>_ET_<suffix>`), or the endpoint still recognizes the id from live session state.

Clients that never stored thinking, and histories whose calls were produced through another route, satisfy none of those conditions and failed permanently. On the Responses route the plugin therefore rewrites every upstream-bound `function_call` / `function_call_output` id that lacks the marker into the marked shape, for native Responses, Chat Completions, and Anthropic Messages sources alike. The mapping is a pure function of the original id (marked ids pass through untouched), so a call and its output stay paired and repeated replays are stable; ids are opaque, but the values the upstream sees may differ from the ones the client sent. Requests without tool calls are still forwarded byte-identically.

## Reasoning round-trip

The upstreams this plugin fronts require their chain of thought back once `tools` are in play, and the replayed reasoning is spliced into the model's context. Missing it can cause an upstream HTTP 400 requiring `reasoning_content`, or let the tool loop continue without the earlier plan and intermediate conclusions.

| Path | Behaviour |
|---|---|
| Responses passthrough | Verbatim both ways (native `reasoning` items, plaintext `reasoning_text`) |
| Chat Completions passthrough | Verbatim both ways (`reasoning_content`) |
| Chat Completions upstream → Responses client | `reasoning_content` or `reasoning` becomes a leading reasoning item (`response.reasoning_text.delta` / `.done`), also in non-streaming conversions; applies to every model that returns either field |
| Responses client → Chat Completions upstream | Replayed reasoning items become `reasoning_content` on every assistant message of their own turn — the text message and the tool-call message a turn may be split into — only for the `deepseek*` family, whose endpoints require the replay |
| Anthropic `thinking` blocks → Chat Completions upstream | Same `deepseek*`-only rule; `redacted_thinking` stays omitted (encrypted metadata) |
| Responses upstream → Anthropic client | Not carried: the upstream has no Anthropic `signature` to send |

For Chat Completions → Responses conversion, non-empty `reasoning_content` takes precedence over `reasoning` when both occur in a message or delta; the fields are never concatenated. Text is preserved verbatim. If neither spelling has non-empty text, an explicitly present empty string is preserved as an empty plaintext reasoning item and replayed as `reasoning_content:""` on both assistant messages of a split DeepSeek tool turn. Absent/null fields and encrypted-only items without plaintext do not create a carrier; later absent/null deltas do not erase an observed carrier. Native Chat Completions passthrough remains unchanged. This preserves future replies, but cannot reconstruct reasoning already lost from saved client history.

The client → upstream replay is gated because replaying a field an endpoint never asked for is the only direction that can fail a request; the upstream → client direction is ungated because it only forwards a chain of thought the upstream actually streamed. Extend `reasoningReplayFamilies` in `internal/adapter/chatcompletions/request.go` if another family turns out to need the replay.

## Configuration

Configure the plugin in your CLIProxyAPI `config.yaml` under `plugins.configs.opencode-go-provider`:

```yaml
plugins:
  configs:
    opencode-go-provider:
      # Upstream base URL (default: "https://opencode.ai/zen/go/v1")
      base-url: "https://opencode.ai/zen/go/v1"

      # Optional catalog endpoint override (default: "{base-url}/models")
      # catalog-url: "https://opencode.ai/zen/go/v1/models"

      # Client-facing model ID prefix configuration
      model-prefix:
        enabled: true           # true -> "opencode-go/<model>", false -> bare "<model>" (default: true)
        value: "opencode-go"    # prefix name (default: "opencode-go")

      # OpenCode Go API keys (at least one required). Supports ${ENV_VAR} expansion.
      api-keys:
        - value: "sk-opencode-key-1"
        - value: "sk-opencode-key-2"
        - value: "${OPENCODE_GO_API_KEY}"

      # Catalog discovery settings
      catalog:
        refresh-interval: "15m"          # discovery refresh cadence, min "1m" (default: "15m")
        stale-while-unavailable: true    # retain last good catalog snapshot on refresh failure (default: true)

      # Protocol enable/disable switches (all default to true)
      protocols:
        chat-completions: true   # enables models routed to /v1/chat/completions
        messages: true           # enables models routed to /v1/messages
        responses: true          # enables models routed to /v1/responses

      # Explicit route overrides per model (takes priority over built-in prefix routing)
      route-overrides:
        "custom-model":
          protocol: "messages"           # "chat-completions" | "messages" | "responses"
          endpoint: "/v1/messages"       # must start with /

      # Execution settings
      request-timeout: "5m"              # upstream request timeout (default: "5m")
      max-response-bytes: 67108864       # max non-streaming response body size in bytes (default: 64 MiB)
      allow-http: false                  # allow http:// scheme for local mock/testing (default: false)
```

### Configuration Options

| Option | Type | Default | Description |
|---|---|---|---|
| `api-keys` | `[]object` | *(Required)* | List of API keys (`- value: "..."`). Supports `${ENV_VAR}` expansion. Duplicates and empty values are rejected. |
| `base-url` | `string` | `https://opencode.ai/zen/go/v1` | Upstream base URL. Must be valid HTTPS (or HTTP if `allow-http: true`) without query parameters, fragments, or userinfo. |
| `catalog-url` | `string` | `{base-url}/models` | Full URL for catalog discovery. Defaults to `{base-url}/models`. |
| `model-prefix.enabled` | `bool` | `true` | When `true`, client-facing model names use `<prefix>/<model>`. When `false`, uses bare model IDs. |
| `model-prefix.value` | `string` | `opencode-go` | Provider prefix string when prefixing is enabled. |
| `catalog.refresh-interval` | `duration` | `15m` | Interval between catalog polling refreshes (e.g. `15m`, `1h`). Minimum is `1m`. |
| `catalog.stale-while-unavailable` | `bool` | `true` | When `true`, serves the last valid catalog snapshot if an update fails. |
| `protocols.chat-completions` | `bool` | `true` | Protocol switch for Chat Completions endpoints. |
| `protocols.messages` | `bool` | `true` | Protocol switch for Messages endpoints. |
| `protocols.responses` | `bool` | `true` | Protocol switch for Responses endpoints. |
| `route-overrides` | `map` | `{}` | Map of model ID to `{ protocol: "...", endpoint: "..." }` overriding built-in family routing. Valid protocols: `chat-completions`, `messages`, `responses`. |
| `request-timeout` | `duration` | `5m` | Upstream HTTP request timeout. Must be positive. |
| `max-response-bytes` | `int64` | `67108864` (64 MiB) | Maximum non-streaming response body size in bytes. |
| `allow-http` | `bool` | `false` | When `true`, permits `http://` scheme in `base-url` / `catalog-url` for local testing. |

## Credential reconciliation

Every value in `api-keys` is materialized as a CPA auth file named `opencode-go-key-<sha256(api_key)>.json`, because CPA schedules credentials — not plugin config. Registration and every reconfigure reconcile those records against the config:

| State | Action |
|---|---|
| Key in config, no record yet | Record created, enabled |
| Key in config, record disabled | Record re-enabled (`disabled` removed; all other fields carried over verbatim) |
| Key removed from config, record present | Record **deleted**: disabled first, then its auth file is unlinked, and the host's auth-directory watcher unregisters the credential |
| Canonical file name, but `api_key` does not hash to it, or `id`/`type` disagree | Left untouched, one `warn` line |
| Runtime-only record (no backing file) | Left untouched |

The host ABI has no delete callback (`host.auth.list/get/save` only), so deletion is staged: the file is renamed to a non-`.json` sibling (invisible to the host's watcher) and proven from those bytes, then the record is saved with `disabled: true` — which is what stops the host from rewriting the file from the credential it holds in memory, since a bare unlink came back on a live host and the resurrected credential then answered 401 — and finally both files are unlinked. Three things must hold first: the listing's path and the path the host read must agree; the file must be an absolute, non-symlink regular file named exactly `opencode-go-key-<64 lowercase hex>.json`; and the staged bytes must prove ownership — same type, same canonical id, same `api_key` digest. Because the proof is taken from one renamed inode, the file that was proven is the file that gets deleted, and the file the host writes back is re-read and checked before it is unlinked. A file that fails any check is renamed back untouched, before anything is written; a rollback never overwrites an existing file (it links the proven record back and reports its recovery path if that name is occupied). Anything that cannot be proven is reported as a `warn` and left alone. The staging name is unique per attempt, so an interrupted run never has its leftover overwritten.

Residual limit: the plugin serializes its own lifecycle passes, but no ABI call can make *prove-then-delete* atomic against another process rewriting files in the auth directory. A writer that can already create and replace credentials there is outside what this ABI defends against; the checks above guarantee that the plugin never deletes a file whose bytes it has not just verified as its own. Deletion depends on CPA's auth-directory watcher being enabled (the default); without it the file is gone but the in-memory credential survives until the next reload.

Only the enable path rewrites a record, and it patches only `disabled`; every other field (label, note, priority, nested metadata) is preserved as raw JSON. Writes happen only when the physical JSON or the runtime flag already disagree with the desired state, so repeated reconfigures and host restarts perform no writes.

Two limits are worth knowing:

- **Bootstrap has no runtime index.** Until the host attaches its auth manager, `host.auth.list` reads the auth directory and returns neither `id` nor `auth_index`, so the raw record cannot be read and ownership cannot be proven. Creation still happens; a record whose state disagrees with the config is reported as deferred (`opencode-go credential reconciliation deferred for N record(s)`). CLIProxyAPI v8 applies the plugin config again once the auth manager is attached, which finishes the work at startup; a host that never reconfigures keeps the old record and the deferral line, and never has an index guessed for it.
- **Credential housekeeping never takes the provider down.** A read, write or unlink that fails is reported as a `warn` (e.g. `credential reconciliation failed: …`, `opencode-go credential <hash> not removed: …`) and the plugin still registers its models; the next reconfigure retries. Failing registration would make the host drop the plugin and take every model of this provider off the air, which is worse than a credential that is one pass behind. A save that times out is an **unknown commit** — the host callback keeps running — so the bridge counts it as outstanding: the next reconciliation waits for it to settle before reading or writing, and if it does not settle inside the lifecycle deadline the pass reports that instead of a state the orphan could still overwrite.

## Responses tool output compatibility

Responses `function_call_output.output` accepts a string or an array of text/image parts. On Chat Completions routes (including DeepSeek and GLM), text stays in the associated `role:"tool"` message. Image positions receive numbered `[tool image N]` markers; the image parts follow in one user message after the consecutive tool results, in their original order. This keeps parallel tool replies together and does not promote tool text to user or system instructions. Image URL/data and Responses image detail hints are preserved. Whether a model can interpret images still depends on that upstream model's vision support.

On Anthropic Messages routes, mixed parts stay inside their associated `tool_result` block in order. Native Responses requests retain their original outputs; unsupported custom-tool items and file output parts remain explicit conversion errors on routes without equivalents. This fix requires no Pi client patch or configuration migration.

## Install via the plugin store

This plugin is listed in the official CLIProxyAPI plugin store under the id `opencode-go-provider`, so Management Center → Plugin Store installs and updates it from this repository's GitHub releases without any extra configuration. The release assets follow the store layout (`opencode-go-provider_<version>_<goos>_<goarch>.zip` plus `checksums.txt`).

The plugin id was renamed from `opencode-go-cliproxyapi` to `opencode-go-provider` in v1.0.0. The upstream repository `massiveits/opencode-go-cliproxyapi` owns the old id in the official registry, and a registry cannot list the same id twice, so this fork ships under its own id. The provider name `opencode-go` and every model prefix (`opencode-go/glm-5.2`, …) are unchanged; only the plugin identity, the library filename and the `plugins.configs.<id>` key moved.

Upgrading from v0.1.x: rename the `plugins.configs.opencode-go-cliproxyapi` block to `plugins.configs.opencode-go-provider`, replace the installed library with the new one, and remove the now-unused `opencode-go-cliproxyapi-v<version>.<ext>` file from the plugins directory. The plugin's own `registry.json` stays published at `https://raw.githubusercontent.com/xspeed1989/opencode-go-cliproxyapi/main/registry.json` for installs that pin this fork as an extra source:

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/xspeed1989/opencode-go-cliproxyapi/main/registry.json"
```

The store writes `plugins/<goos>/<goarch>/opencode-go-provider-v<version>.<ext>` and hot-swaps the loaded plugin, so a running server picks up the new version without an overwrite conflict. In Docker deployments keep the plugins directory on a mounted volume, otherwise the installed file disappears when the container is recreated. If the container cannot reach `raw.githubusercontent.com` / `api.github.com` directly, configure `proxy-url` so store requests use the proxy.

The store resolves each plugin's latest version from the GitHub releases API and caches it per repository for ten minutes, falling back to the `version` field of `registry.json` when that lookup fails. Anonymous GitHub API quota is 60 requests per hour per IP and a single store page load queries every plugin of every enabled registry, so the lookup does fail; that is why `registry.json` carries the current release version. Two consequences for a fresh release: an update can take up to ten minutes to appear, and a row whose version column stays empty got neither the lookup nor the fallback. A plugin installed by hand (not through the store) is never offered an update, because the store cannot verify which source it came from — uninstall it and install it from the store.

## Testing

```powershell
# Run all tests
go test ./...

# Run tests with coverage
go test ./... -cover

# Run linter / vetting
go vet ./...
```
