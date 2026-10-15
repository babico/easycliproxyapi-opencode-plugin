## What's Changed

### Fix

- Attach the replayed chain of thought to every assistant message of its own turn. A Responses assistant turn can span several Chat Completions messages — one per assistant text message, plus the tool-call message that carries the calls — and the replay used to stop at the first one, so the message carrying `tool_calls` went out without `reasoning_content`: the shape a DeepSeek endpoint rejects with HTTP 400 `The reasoning_content in the thinking mode must be passed back to the API.` The turn's reasoning is dropped at a turn boundary (a user, system or developer message, or the tool result that closes the turn), and a fresh reasoning item opens a new assistant output whose calls carry the new chain of thought in their own message.
- The replay stays limited to the `deepseek*` family, so every other Chat Completions-routed model keeps the wire shape of earlier releases. The Anthropic Messages route needs no change: one Anthropic turn is already one Chat Completions message.

### Verification

- `internal/adapter/chatcompletions/request_test.go` covers a turn that holds text and tool calls in both arrival orders, a text message between two calls, the turn boundaries, the mid-turn reasoning item, and the unchanged tool-call merge shape for a non-replay model.


### Change

- Limit the client → upstream reasoning replay (added in v0.1.15) to the upstream families whose Chat Completions endpoints actually require it: the `deepseek*` models. Replayed reasoning items and Anthropic `thinking` blocks still become `reasoning_content` there, and still never do for any other Chat Completions-routed model (`glm*`, `kimi*`, `mimo*`, `hy*`, `longcat*`), which keep the exact wire shape of earlier releases.
- The upstream → client direction is deliberately left ungated: it only forwards a chain of thought the upstream actually streamed, so it cannot fail a request, and it keeps working for every model that emits the field.

### Why

Replaying a field an endpoint never asked for is the only direction that can fail a request. Sending `reasoning_content` to a Chat Completions upstream that has no use for it adds no value, while a stricter endpoint could reject the unknown field; the response direction has no such failure mode and only affects what the client sees.

### Cost

If a non-DeepSeek Chat Completions model turns out to need the replay (the endpoint enforcing the same "must be passed back" rule), that model keeps failing until its prefix is added to `reasoningReplayFamilies` in `internal/adapter/chatcompletions/request.go` — a one-line change plus a test.

### Upgrade Notes

- Update from this fork's plugin-store source (or replace the plugin binary) and reload CLIProxyAPI.
- DeepSeek-routed models behave exactly as in v0.1.15 apart from the split-turn replay fix above. Every other Chat Completions-routed model behaves as it did before v0.1.15 for the request direction, while still surfacing whatever reasoning the upstream streams.
- The Messages and Responses routes are untouched, as are the v0.1.14 tool-call id normalization and the v0.1.13 verbatim error reporting.

**Full Changelog**: https://github.com/xspeed1989/opencode-go-cliproxyapi/compare/v0.1.15...v0.1.16
