package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestRefusedStreamErrorReport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantType string
	}{
		{"auth", 401, `{"error":{"type":"invalid_request_error","message":"invalid key"}}`, "authentication_error"},
		{"permission", 403, `{"error":{"type":"bad_request_error","message":"denied"}}`, "permission_error"},
		{"upstream", 503, `{"error":{"type":"invalid_request","message":"busy"}}`, "server_error"},
		{"request fault", 400, `{"error":{"type":"invalid_request_error","message":"bad input"}}`, "invalid_request_error"},
		{"specific request fault", 503, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long"}}`, "invalid_request_error"},
		{"rate limit already authoritative", 429, `{"error":{"type":"invalid_request_error","message":"slow down"}}`, "invalid_request_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, caller := newStreamManager(t, streamScript{
				startStatus: tc.status, upstreamID: "refused-upstream", frames: []string{tc.body},
			})
			env := decodeEnv(t, mustHandle(t, m, "executor.execute_stream",
				execStreamReqBody("opencode-go/glm-5.3", "openai", []byte(ccRequestBody), "refused-downstream")))
			if env.OK || env.Error == nil || env.Error.HTTPStatus != tc.status {
				t.Fatalf("unexpected envelope: %+v", env)
			}
			var report, original struct {
				Error struct{ Type, Message string }
			}
			if err := json.Unmarshal([]byte(tc.body), &original); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(env.Error.Message), &report); err != nil {
				t.Fatal(err)
			}
			if report.Error.Type != tc.wantType || report.Error.Message != original.Error.Message {
				t.Fatalf("wrong type or lost upstream reason: %s", env.Error.Message)
			}
			for _, method := range []string{pluginabi.MethodHostStreamEmit, pluginabi.MethodHostStreamClose} {
				if len(caller.callsOf(method)) != 0 {
					t.Fatalf("refused stream touched downstream via %s", method)
				}
			}
			if len(caller.callsOf(pluginabi.MethodHostHTTPDoStream)) != 1 || len(caller.callsOf(pluginabi.MethodHostHTTPStreamClose)) != 1 {
				t.Fatal("must make one upstream attempt and close it exactly once")
			}
		})
	}
}
