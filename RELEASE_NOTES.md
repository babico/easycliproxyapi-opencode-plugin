## What's Changed

### Fixed

- Preserve upstream Chat Completions `reasoning` text as well as `reasoning_content` when converting to Responses, in streaming and non-streaming replies. OpenCode can return either spelling for the same model; dropping `reasoning` lost the thinking item and could make DeepSeek tool continuations fail with HTTP 400 requiring `reasoning_content`.
- Prefer non-empty `reasoning_content` when both fields occur in a message or delta, without concatenating or duplicating the text. Whitespace is preserved, and empty fields do not create placeholder reasoning.

### Regression Coverage

- Exercise the complete Chat Completions → Responses history → Chat Completions request round trip, including commentary and parallel tool calls. Both assistant messages retain their reasoning, and tool-call IDs stay paired.
- Cover both field spellings, precedence, empty/null canonical values, whitespace, fragmented streams, absent reasoning, and unchanged native passthrough.

### Upgrade Notes

- No configuration migration is required from v1.0.0. The plugin ID remains `opencode-go-provider`; the provider name, model prefixes, native passthrough, and reasoning effort settings are unchanged.
- This fix preserves future replies. It cannot reconstruct reasoning already lost from saved client history; a previously broken tool turn may need to be regenerated.

**Full Changelog**: https://github.com/xspeed1989/opencode-go-cliproxyapi/compare/v1.0.0...v1.0.1
