# AGENTS.md — easycliproxyapi-opencode-plugin

Native Go plugin for CLIProxyAPI/EasyCLI that exposes **OpenCode Go** as a
single provider (`opencode-go`) inside the org's EasyCLI bridge
(simple-central-proxy-manager `easycli-proxy`, LAN port `:8792`): one model
catalog, one key pool, protocol translation across OpenAI Chat Completions,
Anthropic Messages and OpenAI Responses, config-authoritative credential
reconciliation, and an "OpenCode Go Quota" page in the EasyCLI management panel.

This repo keeps its functional `easycliproxyapi-<provider>-plugin` name — no SC
short name — because the plugin id and the plugin-store-polled repo URL are
bound to the binary identity (org brand-rule exception, see the folder-root
`AGENTS.md`).

## Known gap (tracked): repo source v1.0.3 vs live deployment v2.3.0

This repo currently holds the **upstream v1.0.3 source**:

- plugin id `opencode-go-provider` (`internal/plugin/plugin.go`, `registry.json`,
  CI `PLUGIN_ID`)
- library filename `opencode-go-provider.so` / `.dll` / `.dylib`
- config block `plugins.configs.opencode-go-provider` with a single `base-url`
  schema (no `plans:`)
- CLIProxyAPI **v7** SDK (`github.com/router-for-me/CLIProxyAPI/v7 v7.2.138` in
  `go.mod`)
- Go module `opencode-go-cliproxyapi`; latest tag `v1.0.3`

The **live plugin on the deploy host** is `easycliproxyapi-opencode-provider.so`
**v2.3.0**: plugin id renamed to `easycliproxyapi-opencode-provider`, config
block `plugins.configs.easycliproxyapi-opencode-provider`, running under
CLIProxyAPI v8 with a `plans:`-based config schema (per-plan base-url and model
prefix, e.g. `go` + `zen`) that the repo's v1.0.3 config code does not support.

The v2.3.0 source was **never pushed** to any repo. The host binary is
canonical until the source is recovered or re-derived.

Consequences while the gap is open:

- Do not build from this repo and overwrite the live `.so`. The repo's v1.0.3
  source targets the CPA v7 plugin SDK while the live host runs CLIProxyAPI v8;
  a v1.0.3 build is not the deployed artifact and its ABI compatibility with
  the live host is unverified.
- The README, `registry.json` and `RELEASE_NOTES.md` in this repo describe the
  v1.0.3 identity; the docs hub page (`simple-central-ai-things/docs` →
  `repos/easycliproxyapi-opencode-plugin.md`) documents the live v2.3.0
  identity. The two intentionally disagree until the source is recovered.
- SCPM `easycli-proxy`'s `config.yaml.template` and config-contract tests still
  pin the v1.0.3 identity (`opencode-go-provider`); the live EasyCLI config is
  panel-managed, so the template is not proof of what the running host uses.

Resolution: recover or re-derive the v2.3.0 source, push it here, bump
`registry.json` to the new identity, then retire the v1.0.3 identity. Until
then treat the deployed binary as the reference implementation.

## Layout

```text
main.go                    CGO glue: cliproxy_plugin_init / cliproxyPluginCall ABI exports; every RPC forwarded to internal/plugin.HandleCall
registry.json              plugin-store identity: id opencode-go-provider, version, upstream fork repo URL
resources/quota_page.html  embedded Management Center page (/quota, menu "OpenCode Go Quota")
internal/plugin/           dispatcher, registration metadata, executor, credential reconciliation, quota handler
internal/adapter/          protocol translation: chatcompletions/, messages/, responses/, shared/
internal/catalog/          model catalog discovery + normalization
internal/config/           plugins.configs.<id> schema: api-keys, base-url, catalog-url, model-prefix, catalog, protocols, route-overrides, request-timeout, max-response-bytes, allow-http
internal/errclass/         error classification + explainable upstream error reporting
internal/thinking/         numeric Anthropic budget -> effort-level conversion (explicit effort values never touched)
tests/live_*_test.go       live-environment regression tests behind the `live` build tag (CI runs offline)
.github/workflows/         tests.yml (vet + test + glibc c-shared build) and build.yml (7-target release matrix + tag releases)
.github/scripts/           package-release.go (release zip + sha256 packaging)
```

## Contracts

- **Plugin id (repo source): `opencode-go-provider`**; provider id and model
  prefix: `opencode-go` (models publish as `opencode-go/<model>`).
- **Live plugin id: `easycliproxyapi-opencode-provider`** (v2.3.0, see Known
  gap above). Provider/model identity is unchanged across the rename.
- **Config**: `plugins.configs.opencode-go-provider` in the EasyCLI
  `config.yaml`. `api-keys` is required and supports `${ENV_VAR}` expansion
  (`os.ExpandEnv` at config load). Full option table: README → Configuration.
- **Env keys (names only)**: the live deployment feeds
  `OPENCODE_GO_API_KEY_1/2` from the SCSM `tag:easycli` export rendered by
  SCPM `easycli-proxy/deploy.sh` into the container `.env`. No host-specific
  values in this repo.
- **Auth records**: each configured key is materialized as
  `opencode-go-key-<sha256(api_key)>.json` in the CPA auth directory; every
  register/reconfigure reconciles records against the config (create/enable,
  staged prove-then-delete for removals). API-key only — auth login start/poll,
  `executor.count_tokens` and `executor.http_request` all return `unsupported`.
- **Management API**: `POST /plugins/opencode-go-provider/quota-usage` +
  resource `/quota` (menu "OpenCode Go Quota"). Quota display never affects
  routing or CPA's native quota page.
- **SDK/ABI**: `github.com/router-for-me/CLIProxyAPI/v7 v7.2.138`;
  `cliproxy_plugin_init` rejects a host ABI version mismatch. Registration
  metadata must keep `GitHubRepository` set (host `validPlugin` gate).
- **Cross-repo**: the EasyCLI stack (container, config, `:8792`, deploy) is
  owned by `simple-central-proxy-manager/easycli-proxy`; this repo only ships
  the plugin source.

## Build / test / run

```sh
go vet ./...
go test ./...                       # offline; tests/live_*_test.go are behind the `live` build tag
gofmt -l .

# local development build (debug tag per CONTRIBUTING.md)
TMPDIR=$HOME/.cache/opencode/tmp CGO_ENABLED=1 \
  go build -tags debug -buildmode=c-shared -o plugins/linux/amd64/opencode-go-provider.so .
```

- Machine rules: no `/tmp` (RAM tmpfs) — use `TMPDIR=$HOME/.cache/opencode/tmp`
  and `GOCACHE` under it for Go builds; no-rm (trash-move, never hard-delete).
- CI: `.github/workflows/tests.yml` runs vet + tests + a glibc linux/amd64
  c-shared build on every push/PR (the EasyCLI container is Debian-based, so
  musl builds will not `dlopen`). `.github/workflows/build.yml` runs the
  7-target matrix (linux amd64/arm64, darwin amd64/arm64, windows amd64,
  windows arm64, freebsd amd64) and publishes a GitHub release from
  `RELEASE_NOTES.md` on `v*` tag push.
- Release checklist (CONTRIBUTING.md): bump `pluginVersion` in
  `internal/plugin/plugin.go` **and** `version` in `registry.json` (the store
  falls back to it), rewrite `RELEASE_NOTES.md` for the new version, commit
  `chore(release): vX.Y.Z`, push main, then push the annotated tag.

## Deploy

Hand install — the live deployment is not installed through the plugin store:

1. Build the glibc library: `CGO_ENABLED=1 go build -buildmode=c-shared -o
   plugins/linux/amd64/opencode-go-provider.so .` (TMPDIR per machine rules).
2. Copy it into the EasyCLI bind-mounted plugins directory
   (`plugins/linux/amd64/` next to the EasyCLI `config.yaml` on `<deploy-host>`).
3. Ensure a `plugins.configs.opencode-go-provider` block with env-expanded
   `api-keys` exists in the EasyCLI `config.yaml` (panel-managed; see SCPM
   `easycli-proxy` for template + deploy scripts).
4. Recreate the `easycli-api` container; verify the plugin registers (catalog
   refresh log line) before relying on `opencode-go/<model>`.

**Exception while the Known gap is open:** step 2 must NOT overwrite the live
v2.3.0 binary — deploying a v1.0.3 build would regress the live identity,
config schema and SDK generation. Real host values live only in the private
ops doc (`.opencode/ops/`, folder level).

## Conventions

- English only; no Cyrillic (scan: `grep -rnP '[\x{0400}-\x{04FF}]'`).
- No host-specific details in this public repo: no LAN IPs, ssh targets, host
  nicknames or deploy-host directory layout — placeholders only.
- Secrets never in commits; `api-keys` in examples must be placeholders.
- Commit identity `hello@babico.name.tr`; commit + push after every meaningful
  change; stage only explicit paths (no `git add -A`).
- Docs: `README.md` is the product doc; `RELEASE_NOTES.md` is the release body
  (published verbatim); keep `registry.json` version in lockstep with the tag.

## Cross-repo links

- `simple-central-proxy-manager` (`easycli-proxy/`) — EasyCLI bridge host,
  config template, `:8792`, SCSM `tag:easycli` rendering.
- `simple-central-agent-config` (SCAC) — consumer harness configs that route
  through the gateway.
- `simple-central-ai-things/docs` — public architecture hub; per-repo page
  `repos/easycliproxyapi-opencode-plugin.md` (documents the live v2.3.0
  identity — see Known gap).
- Org brand rules + repo graph: the folder-root `AGENTS.md`.
