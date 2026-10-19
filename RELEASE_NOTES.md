## What's Changed

### Fixed

- Preserve explicitly empty upstream `reasoning_content` and `reasoning` strings through Chat Completions → Responses history → DeepSeek tool replay, in streaming and non-streaming replies. An empty string is different from a missing field: dropping an observed `reasoning_content:""` could cause the next tool turn to fail with HTTP 400.
- Carry that observed empty value onto both the commentary and parallel-tool-call assistant messages of the same turn. Later null/absent deltas do not erase it. Truly absent/null values and encrypted-only items without plaintext do not acquire invented reasoning.
- Keep reasoning attached to its own turn, including when an empty user/system/developer boundary is omitted during translation. Preserve existing reasoning-free tool-call merging, model-family gating, nonempty-field precedence, native passthrough, and Claude behavior.

### Regression Coverage

- Add self-contained tests for explicit-empty versus absent/null data, alternate spelling precedence, stream lifecycle and terminal events, nullable summaries, parallel-call replay, and turn boundaries.
- Replay the captured synthetic empty-reasoning stream through the real converter, installed Pi parser and JSON-persisted signature replay, then back through the request converter; verify empty fields survive on both assistant messages while absence/null/encrypted-only controls remain omitted.

### Upgrade Notes

- No configuration migration is required from v1.0.1. The plugin ID remains `opencode-go-provider`; reasoning effort settings and model routing are unchanged.
- This fixes future round trips. It cannot reconstruct reasoning already lost from saved history; a previously broken tool turn may need to be regenerated.

**Full Changelog**: https://github.com/xspeed1989/opencode-go-cliproxyapi/compare/v1.0.1...v1.0.2
