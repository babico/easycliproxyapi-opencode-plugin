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
- **Explainable failures**: a streaming request the upstream refuses before the first byte reports that status plus up to 2 KiB of its error body verbatim, instead of an empty message the host renders as `plugin call failed`.
- **Dynamic Catalog Discovery**: Fetches remote model catalogs with local fallback and custom route overrides.
- **Multi-Key Auth Scheduling**: Pools multiple API keys with CLIProxyAPI's native scheduler for rotation, retries, and error cooldowns across all protocols.
- **OpenCode Go Quota Page**: Management Center includes a separate `OpenCode Go Quota` page. Page load lists credentials without contacting OpenCode; each card is refreshed manually and independently, and quota values do not affect routing or CPA's native quota page.

## Requirements

- **CLIProxyAPI**: `v7.2.138+`
- **Go Toolchain**: Go 1.24+ (with CGO enabled for C-shared build mode)

## Build

Build the dynamic shared library for your platform:

### Windows (AMD64)
```powershell
go build -buildmode=c-shared -o plugins/windows/amd64/opencode-go-cliproxyapi.dll .
```

### Linux (AMD64)
```bash
go build -buildmode=c-shared -o plugins/linux/amd64/opencode-go-cliproxyapi.so .
```

### macOS (ARM64)
```bash
go build -buildmode=c-shared -o plugins/darwin/arm64/opencode-go-cliproxyapi.dylib .
```

Place the compiled binary into your CLIProxyAPI plugin directory (e.g. `<cliproxyapi_root>/plugins/<os>/<arch>/`).

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

The upstreams this plugin fronts require their chain of thought back once `tools` are in play, and the replayed reasoning is spliced into the model's context. Missing it does not fail loudly: the tool loop simply continues without the earlier plan and intermediate conclusions.

| Path | Behaviour |
|---|---|
| Responses passthrough | Verbatim both ways (native `reasoning` items, plaintext `reasoning_text`) |
| Chat Completions passthrough | Verbatim both ways (`reasoning_content`) |
| Chat Completions upstream → Responses client | `reasoning_content` becomes a leading reasoning item (`response.reasoning_text.delta` / `.done`), also in non-streaming conversions; applies to every model that streams the field |
| Responses client → Chat Completions upstream | Replayed reasoning items become `reasoning_content` on every assistant message of their own turn — the text message and the tool-call message a turn may be split into — only for the `deepseek*` family, whose endpoints require the replay |
| Anthropic `thinking` blocks → Chat Completions upstream | Same `deepseek*`-only rule; `redacted_thinking` stays omitted (encrypted metadata) |
| Responses upstream → Anthropic client | Not carried: the upstream has no Anthropic `signature` to send |

The client → upstream replay is gated because replaying a field an endpoint never asked for is the only direction that can fail a request; the upstream → client direction is ungated because it only forwards a chain of thought the upstream actually streamed. Extend `reasoningReplayFamilies` in `internal/adapter/chatcompletions/request.go` if another family turns out to need the replay.

## Configuration

Configure the plugin in your CLIProxyAPI `config.yaml` under `plugins.configs.opencode-go-cliproxyapi`:

```yaml
plugins:
  configs:
    opencode-go-cliproxyapi:
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

## Install via the plugin store (this fork)

This fork publishes its own plugin-store registry so CLIProxyAPI can install and update it from this repository's GitHub releases. The release assets already follow the store layout (`opencode-go-cliproxyapi_<version>_<goos>_<goarch>.zip` plus `checksums.txt`).

Add the registry as an extra store source and reload the config:

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/xspeed1989/opencode-go-cliproxyapi/main/registry.json"
```

Then open Management Center → Plugin Store and install `opencode-go-cliproxyapi`. The built-in official registry also lists that plugin id for the upstream repository, so when the store asks to choose a source, select the `raw.githubusercontent.com` source.

The store writes `plugins/<goos>/<goarch>/opencode-go-cliproxyapi_<version>.<ext>` and hot-swaps the loaded plugin, so a running server picks up the new version without an overwrite conflict. In Docker deployments keep the plugins directory on a mounted volume, otherwise the installed file disappears when the container is recreated. If the container cannot reach `raw.githubusercontent.com` / `api.github.com` directly, configure `proxy-url` so store requests use the proxy.

## Testing

```powershell
# Run all tests
go test ./...

# Run tests with coverage
go test ./... -cover

# Run linter / vetting
go vet ./...
```
