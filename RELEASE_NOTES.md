## What's Changed

### Breaking

- Rename the plugin id from `opencode-go-cliproxyapi` to `opencode-go-provider`. The official CLIProxyAPI plugin registry already lists `opencode-go-cliproxyapi` for the upstream repository `massiveits/opencode-go-cliproxyapi`, and a registry cannot contain the same id twice, so this fork could only be published under its own id. The plugin id is also the shared-library filename and the `plugins.configs.<id>` key, so all three move together: the release assets are now `opencode-go-provider_<version>_<goos>_<goarch>.zip`, each zip holds `opencode-go-provider.<ext>` at its root, and the configuration block is now `plugins.configs.opencode-go-provider`.
- Nothing else is renamed. The provider name stays `opencode-go`, every model prefix stays `opencode-go/...`, the management quota route becomes `/v0/management/plugins/opencode-go-provider/quota-usage`, and no request, response or routing behaviour changes in this release.
- The version jumps to `1.0.0` rather than the arithmetically next `0.2.0`, because the upstream repository already carries a `v0.2.0` tag. Reusing that tag here would make every clone that tracks both remotes fight over it, so this fork runs its own version line from `1.0.0` on.

### Upgrade Notes

- Rename the `plugins.configs.opencode-go-cliproxyapi` block in `config.yaml` to `plugins.configs.opencode-go-provider`. The nested keys (`api-keys`, `base-url`, `catalog-url`, `model-prefix`, `catalog`, `protocols`, `route-overrides`, `request-timeout`, `max-response-bytes`, `allow-http`) are unchanged; a block left under the old id is ignored, and the host reports the plugin as unconfigured.
- Replace the installed library. The store installs `plugins/<goos>/<goarch>/opencode-go-provider-v1.0.0.<ext>` as a new file, so a previous `opencode-go-cliproxyapi-v<version>.<ext>` keeps being loaded as well: delete it, or the host loads two libraries that both claim the `opencode-go` provider. For hand installs, drop the new library into `plugins/<goos>/<goarch>/` and remove the old one.
- The plugin's own `registry.json` (used by `plugins.store-sources`) moves to the new id too, so an extra source pinned at `https://raw.githubusercontent.com/xspeed1989/opencode-go-cliproxyapi/main/registry.json` keeps working; it now lists `opencode-go-provider`. The official registry carries the same id, so the plugin no longer needs a second source at all.
- An install made before this release is not offered an update, because the store derives a plugin's identity from the library filename and `opencode-go-cliproxyapi` is not an id this repository publishes any more. Uninstall the old entry and install `opencode-go-provider`.
- Credentials are unaffected. The auth records the plugin materializes are named from the API key digest (`opencode-go-key-<sha256>`), not from the plugin id, so reconciliation finds and keeps every existing record; the first reconfigure after the upgrade performs no writes.

**Full Changelog**: https://github.com/xspeed1989/opencode-go-cliproxyapi/compare/v0.1.18...v1.0.0
