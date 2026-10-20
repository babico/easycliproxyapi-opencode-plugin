## What's Changed

### Fixed

- Accept Responses `function_call_output.output` as either a string or a content-part array. Responses clients using tool images no longer fail locally with `cannot unmarshal array ... output of type string` when routed to DeepSeek, GLM, or another Chat Completions model.
- Keep all tool text, including error text, in its original tool role and preserve `call_id` pairing. Use numbered markers for image positions and forward images in order after the consecutive tool results, so parallel calls finish before the image-bearing user message. Preserve image URL/data and Responses detail hints.
- Preserve text/image order inside Anthropic `tool_result` blocks. Native Responses passthrough, normal string results, reasoning replay and model routing retain their existing behavior. Unsupported file/custom-tool conversions fail explicitly rather than silently losing content.

### Regression Coverage

- Reproduce the previous array decode failure with synthetic text/image outputs, then cover string/array mixtures, empty/text/error results, multiple tool calls, image ordering/detail, tool-role isolation and malformed/unsupported parts.
- Exercise DeepSeek and GLM through both public plugin execution paths, session derivation, isolated HTTP serialization and Responses stream/non-stream conversion. Verify native Responses requests remain byte-identical.

### Upgrade Notes

- Update **OpenCode Go Provider** (`opencode-go-provider`) through CLIProxyAPI Management Center → Plugin Store. No configuration migration is required from v1.0.2; the model prefix remains `opencode-go/`.
- The store hot-swaps the versioned native library. No Pi update or restart is required for this server-side fix. Manually installed libraries must be replaced/reloaded using the host's existing plugin procedure; no running services are changed by this release.
- Image forwarding preserves the payload; image understanding still requires vision support in the selected upstream model. Private sessions or live credentials are not used in the regression tests.

**Full Changelog**: https://github.com/xspeed1989/opencode-go-cliproxyapi/compare/v1.0.2...v1.0.3
