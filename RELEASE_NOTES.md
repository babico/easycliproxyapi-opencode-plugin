## What's Changed

### Fix

- Attach the replayed chain of thought to every assistant message of its own turn. A Responses assistant turn can span several Chat Completions messages — one per assistant text message, plus the tool-call message that carries the calls — and the replay used to stop at the first one, so the message carrying `tool_calls` went out without `reasoning_content`: the shape a DeepSeek endpoint rejects with HTTP 400 `The reasoning_content in the thinking mode must be passed back to the API.` The turn's reasoning is now dropped only at a real boundary (a user, system or developer message, or the tool result that closes the turn), and a fresh reasoning item opens a new assistant output: it replaces the value and closes the open tool-call merge, so the calls after it carry the new chain of thought in their own message.
- The replay stays limited to the `deepseek*` family — `reasoning_content` keeps its `omitempty` tag and the merge flag is reset only when a reasoning item is actually replayed — so every other Chat Completions-routed model (`glm*`, `kimi*`, `mimo*`, `hy*`, `longcat*`) keeps the exact wire shape of earlier releases. An input message without content (an empty `user`/`system`/`developer` item) also no longer splits a merged tool-call message, for any family. The Anthropic Messages route needs no change: one Anthropic turn is already one Chat Completions message.

### Why

The DeepSeek endpoints require the replayed chain of thought on the message that carries the tool calls, and the split-turn shape is not rare: any assistant turn that emits text before its calls produces it, as does this proxy's own Responses stream conversion, which assigns output indices in arrival order — a Chat Completions upstream that streams `reasoning → tool → text → tool` hands the client exactly that order.

### Verification

- `internal/adapter/chatcompletions/request_test.go` covers a turn that holds text and tool calls in both arrival orders, a text message between two calls, the turn boundaries, a mid-turn reasoning item, and the unchanged tool-call merge shape for a non-replay model. The new tests fail against the v0.1.16 code.
- `go test ./...`, `go test -tags debug ./...`, `go vet ./...` and `gofmt -l .` are clean, and CI builds the six release targets.
- Residual risk: no credentialed end-to-end run was possible, so upstream acceptance of the replayed `reasoning_content` on the tool-call message is unconfirmed. What is pinned is the conversion defect — before this change the replay could not reach the message the upstream validates.

### Upgrade Notes

- Update from this fork's plugin-store source (or replace the plugin binary) and reload CLIProxyAPI.
- DeepSeek-routed models behave as in v0.1.16 apart from the split-turn replay fix above.
- Every other Chat Completions-routed model is unchanged for the request direction, while still surfacing whatever reasoning the upstream streams. The Messages and Responses routes are untouched.

**Full Changelog**: https://github.com/xspeed1989/opencode-go-cliproxyapi/compare/v0.1.16...v0.1.17
