package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Drive both public execution methods through session derivation, real HTTP
// serialization to the isolated upstream and Responses response conversion.
// This never uses a live model or any user conversation/configuration.
func TestResponsesToolArrayExecutor(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", "glm-5.3"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				m, f, st, yamlText := newIntegrationManager(t)
				st.setCatalogBody(`{"data":[{"id":"` + model + `"}]}`)
				if env := decodeEnv(t, mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(yamlText))); !env.OK {
					t.Fatalf("reconfigure: %+v", env.Error)
				}
				body := []byte(fmt.Sprintf(`{"stream":%t,"input":[
     {"role":"user","content":"synthetic fixture"},
     {"type":"function_call","call_id":"a","name":"read","arguments":"{}"},
     {"type":"function_call","call_id":"b","name":"read","arguments":"{}"},
     {"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},
     {"type":"function_call_output","call_id":"b","output":[{"type":"input_text","text":"[error] fixture"},{"type":"input_image","image_url":"data:image/png;base64,AQ=="}]}]}`, stream))
				if stream {
					resp, err := m.HandleCall("executor.execute_stream", execStreamReqBodyForKey("opencode-go/"+model, "openai-response", body, "down-images", "sk-test-1"))
					if err != nil {
						t.Fatal(err)
					}
					if env := decodeEnv(t, resp); !env.OK {
						t.Fatalf("execute_stream: %+v", env.Error)
					}
					m.bridge.WaitForInFlight(5 * time.Second)
					assertCleanStreamClose(t, f)
				} else if env := mustExecute(t, m, "opencode-go/"+model, "openai-response", body); !env.OK {
					t.Fatalf("execute: %+v", env.Error)
				}
				path, _, upstream := st.chatRequest(t)
				if path != "/v1/chat/completions" || upstream["model"] != model {
					t.Fatalf("route: %s %v", path, upstream["model"])
				}
				raw, err := json.Marshal(upstream["messages"])
				if err != nil {
					t.Fatal(err)
				}
				var msgs []struct {
					Role    string
					Content json.RawMessage
					ID      string                `json:"tool_call_id"`
					Calls   []struct{ ID string } `json:"tool_calls"`
				}
				if err := json.Unmarshal(raw, &msgs); err != nil {
					t.Fatal(err)
				}
				if len(msgs) != 5 || len(msgs[1].Calls) != 2 || msgs[2].Role != "tool" || msgs[2].ID != msgs[1].Calls[0].ID || msgs[3].Role != "tool" || msgs[3].ID != msgs[1].Calls[1].ID || msgs[4].Role != "user" {
					t.Fatalf("tool pairing/order: %s", raw)
				}
				if string(msgs[2].Content) != `"[tool image 1]"` || string(msgs[3].Content) != `"[error] fixture[tool image 2]"` {
					t.Fatalf("tool content: %s", raw)
				}
				var parts []struct {
					Text     string
					ImageURL struct{ URL string } `json:"image_url"`
				}
				if err := json.Unmarshal(msgs[4].Content, &parts); err != nil {
					t.Fatal(err)
				}
				if len(parts) != 4 || parts[0].Text != "[tool image 1]" || parts[1].ImageURL.URL != "data:image/png;base64,AA==" || parts[2].Text != "[tool image 2]" || parts[3].ImageURL.URL != "data:image/png;base64,AQ==" {
					t.Fatalf("images lost/reordered: %s", msgs[4].Content)
				}
			})
		}
	}
}

// Even without an explicit session ID, decoding native array outputs must
// not force the native route through the Chat image relocation policy.
func TestResponsesToolArrayNativeExecutor(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-luna","stream":%t,"input":[{"role":"user","content":"fixture"},{"type":"function_call","call_id":"call_00_ET_fixture","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_00_ET_fixture","output":[{"type":"input_text","text":"tool text"},{"type":"input_image","image_url":"https://example.invalid/image.png","detail":"high"}]}]}`, stream))
			var m *Manager
			var f *fakeCaller
			method := pluginabi.MethodHostHTTPDo
			if stream {
				m, f = newStreamManager(t, streamScript{upstreamID: "native-array", frames: responsesSSEFrames})
				resp, err := m.HandleCall("executor.execute_stream", execStreamReqBody("opencode-go/gpt-5.6-luna", "openai-response", body, "native-down"))
				if err != nil {
					t.Fatal(err)
				}
				if env := decodeEnv(t, resp); !env.OK {
					t.Fatalf("native stream: %+v", env.Error)
				}
				m.bridge.WaitForInFlight(5 * time.Second)
				assertCleanStreamClose(t, f)
				method = pluginabi.MethodHostHTTPDoStream
			} else {
				m, f = newExecManager(t)
				if env := mustExecute(t, m, "opencode-go/gpt-5.6-luna", "openai-response", body); !env.OK {
					t.Fatalf("native execute: %+v", env.Error)
				}
			}
			got := wireBody(t, lastWire(t, f, method), "body")
			if !bytes.Equal(got, body) {
				t.Fatalf("native tool output altered:\n got: %s\nwant: %s", got, body)
			}
		})
	}
}
