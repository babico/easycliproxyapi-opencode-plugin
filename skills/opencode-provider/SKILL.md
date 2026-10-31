---
name: opencode-provider
description: Use when building, debugging, testing, or deploying the OpenCode Go EasyCLI provider plugin (easycliproxyapi-opencode-plugin), or when touching the opencode-go provider inside the EasyCLI/CLIProxyAPI bridge (SCPM easycli-proxy, port 8792). Trigger words: opencode plugin, opencode-go-provider, easycliproxyapi-opencode-plugin, OpenCode Go provider, opencode-go model prefix, OpenCode Go quota page, CPA provider plugin .so.
---

# OpenCode provider plugin

Native Go CLIProxyAPI/EasyCLI plugin that exposes OpenCode Go as a single
provider (`opencode-go`) inside the EasyCLI bridge: one model catalog, one key
pool, multi-protocol translation (Chat Completions / Anthropic Messages /
Responses), config-authoritative credential reconciliation, and an OpenCode Go
quota page in the management panel.

## Key paths

- `easycliproxyapi-opencode-plugin/` — repo checkout
  (github.com/simple-central-ai-things/easycliproxyapi-opencode-plugin)
- `main.go` — CGO glue: `cliproxy_plugin_init` / `cliproxyPluginCall` ABI exports
- `internal/plugin/plugin.go` — dispatcher + identity constants
  (`ProviderID`, `pluginName`, `pluginVersion`); release checklist bumps this
- `internal/plugin/auth_reconcile.go` — credential reconciliation
- `internal/adapter/{chatcompletions,messages,responses,shared}/` — protocol translation
- `internal/catalog/` — model catalog discovery; `internal/config/` — config schema
- `registry.json` — plugin-store identity (points at the upstream fork, not this repo)
- `resources/quota_page.html` — embedded management page (`/quota`)
- `tests/live_*_test.go` — behind the `live` build tag; CI runs offline
- `simple-central-proxy-manager/easycli-proxy/` — the EasyCLI stack this plugin runs in
  (config template, deploy.sh, `:8792`, SCSM `tag:easycli` rendering)

## Contracts / env

- Plugin id (repo source): `opencode-go-provider`; provider id + model prefix:
  `opencode-go` (models publish as `opencode-go/<model>`).
- Live plugin id: `easycliproxyapi-opencode-provider` (v2.3.0 binary — see Gotchas).
- Config: `plugins.configs.opencode-go-provider` (v1.0.3 schema: `api-keys`,
  `base-url` default `https://opencode.ai/zen/go/v1`, `catalog-url`,
  `model-prefix`, `catalog`, `protocols`, `route-overrides`, `request-timeout`,
  `max-response-bytes`, `allow-http`). Live v2.3.0 config uses `plans:` — not
  supported by the repo schema.
- `api-keys` values support `${ENV_VAR}` expansion (`os.ExpandEnv`). Live keys
  are `OPENCODE_GO_API_KEY_1/2`, rendered from the SCSM `tag:easycli` export by
  SCPM `easycli-proxy/deploy.sh` (names only — values never in this repo).
- Auth records: `opencode-go-key-<sha256(api_key)>.json` in the CPA auth dir;
  reconciled on every register/reconfigure; removals use staged prove-then-delete.
- Management API: `POST /plugins/opencode-go-provider/quota-usage`; quota
  display never affects routing. API-key only: auth login, `executor.count_tokens`
  and `executor.http_request` return `unsupported`.
- SDK: `github.com/router-for-me/CLIProxyAPI/v7 v7.2.138`; `cliproxy_plugin_init`
  rejects an ABI version mismatch; registration metadata must keep
  `GitHubRepository` set (host `validPlugin` gate).

## Common operations

```sh
cd easycliproxyapi-opencode-plugin
go vet ./... && go test ./...   # offline; live_*_test.go need the `live` tag
TMPDIR=$HOME/.cache/opencode/tmp CGO_ENABLED=1 \
  go build -tags debug -buildmode=c-shared -o plugins/linux/amd64/opencode-go-provider.so .
```

- Always `-tags debug` for local builds (CONTRIBUTING.md); release packaging is
  CI's job. Machine rule: never `/tmp` (RAM tmpfs) — `TMPDIR=` / `GOCACHE=`
  under `~/.cache/opencode/tmp`.
- CI: `.github/workflows/tests.yml` (vet + test + glibc linux/amd64 c-shared
  build on push/PR); `.github/workflows/build.yml` (7-target matrix + tag
  release from `RELEASE_NOTES.md`). Release = bump `pluginVersion` +
  `registry.json` version, rewrite `RELEASE_NOTES.md`, tag `vX.Y.Z`.
- Deploy is a hand install (not store-managed): glibc `.so` into the EasyCLI
  bind-mounted `plugins/linux/amd64/`, `plugins.configs.<id>` block in the
  EasyCLI config (panel-managed), recreate the `easycli-api` container.
  Real host values: folder-level `.opencode/ops/local-infrastructure.md` (private).

## Gotchas

- **Tracked known gap — v1.0.3 repo vs v2.3.0 live.** The repo holds upstream
  v1.0.3 source (id `opencode-go-provider`, CPA v7 SDK, single-`base-url`
  schema). The live plugin is `easycliproxyapi-opencode-provider.so` v2.3.0
  (renamed id, CLIProxyAPI v8 host, `plans:` config schema) whose source was
  never pushed. The host binary is canonical until the source is recovered or
  re-derived. Do NOT build from this repo over the live binary, and do not
  "fix" live behavior from this source — the docs hub page documents the live
  identity, not this source.
- The plugin-store identity is upstream's: `registry.json` points at
  `github.com/xspeed1989/opencode-go-cliproxyapi`, so store installs resolve
  that fork's releases, not this repo's.
- Credential reconciliation is best-effort on purpose: housekeeping failures
  are `warn`s; a failed registration would drop the whole provider.
- The live EasyCLI config is panel-managed; SCPM's template/tests still pin
  `opencode-go-provider` — template edits prove nothing about the running host.
- Registration metadata with an empty `GitHubRepository` is rejected by the
  host `validPlugin` gate.
