package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// OpenCode can stream reasoning rather than reasoning_content for the same
// DeepSeek model. Exercise the response -> Responses history -> Chat request
// seam: both the assistant text and its parallel tool calls need the replay.
func TestReasoningFieldRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		want   string
	}{
		{"canonical", `"reasoning_content":"inspect both files"`, "inspect both files"},
		{"alternate", `"reasoning":"inspect both files"`, "inspect both files"},
		{"canonical-wins", `"reasoning_content":"canonical","reasoning":"alternate"`, "canonical"},
		{"empty-canonical", `"reasoning_content":"","reasoning":"alternate"`, "alternate"},
		{"null-canonical", `"reasoning_content":null,"reasoning":"alternate"`, "alternate"},
		{"whitespace", `"reasoning":"  plan\n"`, "  plan\n"},
		{"empty", `"reasoning_content":"","reasoning":""`, ""},
		{"absent", `"role":"assistant"`, ""},
	}
	for _, tc := range cases {
		for _, streaming := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				output := reasoningRoundTripOutput(t, tc.fields, tc.want, streaming)
				reasoningItems := 0
				for i, raw := range output {
					item := raw.(map[string]any)
					if item["type"] != "reasoning" {
						continue
					}
					reasoningItems++
					if i != 0 {
						t.Fatal("reasoning must precede text and tool calls")
					}
					parts := item["content"].([]any)
					if len(parts) != 1 || parts[0].(map[string]any)["text"] != tc.want {
						t.Fatalf("reasoning item content = %v, want %q", parts, tc.want)
					}
				}
				wantReasoningItems := 0
				if tc.want != "" {
					wantReasoningItems = 1
				}
				if reasoningItems != wantReasoningItems {
					t.Fatalf("reasoning items = %d, want %d", reasoningItems, wantReasoningItems)
				}

				input := []any{map[string]any{"role": "user", "content": "echo test"}}
				input = append(input, output...)
				for _, id := range []string{"call_a", "call_b"} {
					input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": "ok"})
				}
				body, err := json.Marshal(map[string]any{"input": input})
				if err != nil {
					t.Fatal(err)
				}
				upstream, eErr := BuildRequest("deepseek-v4.1-flash", "openai-response", body, nil)
				if eErr != nil {
					t.Fatal(eErr)
				}
				var request struct {
					Messages []struct {
						Role             string
						ReasoningContent *string               `json:"reasoning_content"`
						ToolCalls        []struct{ ID string } `json:"tool_calls"`
					}
				}
				if err := json.Unmarshal(upstream, &request); err != nil {
					t.Fatal(err)
				}
				assistants := 0
				var toolIDs []string
				for _, message := range request.Messages {
					if message.Role != "assistant" {
						if message.ReasoningContent != nil {
							t.Fatalf("reasoning leaked to %s", message.Role)
						}
						continue
					}
					assistants++
					if tc.want == "" {
						if message.ReasoningContent != nil {
							t.Fatal("empty reasoning must not invent a replay field")
						}
					} else if message.ReasoningContent == nil || *message.ReasoningContent != tc.want {
						t.Fatalf("assistant reasoning_content = %v, want %q", message.ReasoningContent, tc.want)
					}
					for _, call := range message.ToolCalls {
						toolIDs = append(toolIDs, call.ID)
					}
				}
				if assistants != 2 || strings.Join(toolIDs, ",") != "call_a,call_b" {
					t.Fatalf("split tool turn changed: assistants=%d tool IDs=%v", assistants, toolIDs)
				}
			})
		}
	}
}

func reasoningRoundTripOutput(t *testing.T, fields, want string, streaming bool) []any {
	t.Helper()
	if streaming {
		sc := NewStreamConverter("openai-response")
		events := feedAll(t, sc,
			`data: {"id":"r1","model":"deepseek-v4.1-flash","choices":[{"delta":{"role":"assistant"}}]}`,
			fmt.Sprintf(`data: {"choices":[{"delta":{%s}}]}`, fields),
			`data: {"choices":[{"delta":{"content":"I will check now."}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"echo","arguments":"{}"}},{"index":1,"id":"call_b","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: {"choices":[]}`)
		var output []any
		var deltaText strings.Builder
		for _, event := range events {
			if event.Name == "response.reasoning_text.delta" {
				deltaText.WriteString(event.Data["delta"].(string))
			}
			if event.Name == "response.completed" {
				output = event.Data["response"].(map[string]any)["output"].([]any)
			}
		}
		if got := deltaText.String(); got != want {
			t.Fatalf("streamed reasoning = %q, want %q", got, want)
		}
		if output == nil {
			t.Fatal("missing completed output")
		}
		return output
	}
	body := fmt.Sprintf(`{"id":"r1","model":"deepseek-v4.1-flash","choices":[{"message":{%s,"content":"I will check now.","tool_calls":[{"id":"call_a","type":"function","function":{"name":"echo","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, fields)
	converted, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(body))
	if eErr != nil {
		t.Fatal(eErr)
	}
	var response struct{ Output []any }
	if err := json.Unmarshal(converted, &response); err != nil {
		t.Fatal(err)
	}
	return response.Output
}

func TestReasoningFieldNativePassthrough(t *testing.T) {
	body := `{"id":"r1","choices":[{"message":{"reasoning":"plan","content":"hi"}}]}`
	converted, eErr := ConvertNonStreamResponse("openai", 200, []byte(body))
	if eErr != nil || string(converted) != body {
		t.Fatalf("native non-stream response changed: %s, %v", converted, eErr)
	}
	chunk := `{"choices":[{"delta":{"reasoning":"plan"}}]}`
	events, _, eErr := NewStreamConverter("openai").Feed([]byte("data: " + chunk + "\n\n"))
	if eErr != nil || len(events) != 1 || string(events[0]) != chunk {
		t.Fatalf("native stream payload changed: %q, %v", events, eErr)
	}
}

func TestReasoningFieldStreamFragments(t *testing.T) {
	sc := NewStreamConverter("openai-response")
	events := feedAll(t, sc,
		`data: {"id":"r1","choices":[{"delta":{"reasoning":"think "}}]}`,
		`data: {"choices":[{"delta":{"reasoning":"hard"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)
	var streamed strings.Builder
	var doneText string
	for _, event := range events {
		switch event.Name {
		case "response.reasoning_text.delta":
			streamed.WriteString(event.Data["delta"].(string))
		case "response.reasoning_text.done":
			doneText = event.Data["text"].(string)
		}
	}
	if streamed.String() != "think hard" || doneText != "think hard" {
		t.Fatalf("reasoning fragments lost: streamed=%q done=%q", streamed.String(), doneText)
	}
}
