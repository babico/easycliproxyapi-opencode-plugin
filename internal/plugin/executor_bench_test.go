package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/catalog"
)

// benchSink* absorb benchmark outputs so the compiler cannot elide the work.
var (
	benchSinkBytes  []byte
	benchSinkString string
)

const benchPad = 8192

// benchChatBody builds a valid Chat Completions body of at least targetBytes.
func benchChatBody(targetBytes int) []byte {
	pad := strings.Repeat("x", benchPad)
	var b strings.Builder
	b.WriteString(`{"model":"client","messages":[`)
	for first := true; b.Len() < targetBytes; first = false {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"` + pad + `"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// benchClaudeBody builds a valid Anthropic Messages body of at least targetBytes.
func benchClaudeBody(targetBytes int) []byte {
	pad := strings.Repeat("x", benchPad)
	var b strings.Builder
	b.WriteString(`{"model":"client","max_tokens":64,"messages":[`)
	for first := true; b.Len() < targetBytes; first = false {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"` + pad + `"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// benchResponsesBody builds a valid Responses body of at least targetBytes.
func benchResponsesBody(targetBytes int) []byte {
	pad := strings.Repeat("x", benchPad)
	var b strings.Builder
	b.WriteString(`{"model":"client","input":[`)
	for first := true; b.Len() < targetBytes; first = false {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + pad + `"}]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// benchLeg is one conversion fixture: the six cross-format legs plus the three
// native passthrough controls.
type benchLeg struct {
	name   string
	route  catalog.Route
	format string
	model  string
	body   []byte
}

func benchLegs(size int) []benchLeg {
	chat := func(format, model string, body []byte) benchLeg {
		return benchLeg{name: "chat/" + format, route: catalog.RouteChatCompletions, format: format, model: model, body: body}
	}
	resp := func(format, model string, body []byte) benchLeg {
		return benchLeg{name: "responses/" + format, route: catalog.RouteResponses, format: format, model: model, body: body}
	}
	msg := func(format, model string, body []byte) benchLeg {
		return benchLeg{name: "messages/" + format, route: catalog.RouteMessages, format: format, model: model, body: body}
	}
	return []benchLeg{
		chat("openai", "deepseek-v4-pro", benchChatBody(size)),
		chat("claude", "deepseek-v4-pro", benchClaudeBody(size)),
		chat("openai-response", "deepseek-v4-pro", benchResponsesBody(size)),
		resp("openai", "gpt-5.6-luna", benchChatBody(size)),
		resp("claude", "gpt-5.6-luna", benchClaudeBody(size)),
		resp("openai-response", "gpt-5.6-luna", benchResponsesBody(size)),
		msg("openai", "minimax-m3", benchChatBody(size)),
		msg("claude", "minimax-m3", benchClaudeBody(size)),
		msg("openai-response", "minimax-m3", benchResponsesBody(size)),
	}
}

// identityRequest builds the executor envelope fields session derivation reads.
func identityRequest(format string, body []byte, canonical, header string) executorRequest {
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		SourceFormat: format, OriginalRequest: body,
	}}
	if canonical != "" {
		req.Metadata = map[string]any{"canonical_session_id": canonical}
	}
	if header != "" {
		req.Headers = http.Header{"X-Claude-Code-Session-Id": []string{header}}
	}
	return req
}

// BenchmarkRequestPreparation measures the plugin's request-preparation work
// (envelope already decoded): derive the session identity, then translate to
// the upstream route. Each iteration creates a fresh SourceRequest, so the
// lazy cache is exercised exactly as on one real request. All nine legs and
// the three native controls are covered at ~256 KiB.
func BenchmarkRequestPreparation(b *testing.B) {
	for _, leg := range benchLegs(256 << 10) {
		b.Run(leg.name, func(b *testing.B) {
			req := identityRequest(leg.format, leg.body, "", "")
			b.ReportAllocs()
			b.SetBytes(int64(len(leg.body)))
			for b.Loop() {
				source := shared.NewSourceRequest(leg.format, leg.body)
				if _, eErr := resolveOpenCodeSessionIDFrom(req, source); eErr != nil {
					b.Fatal(eErr)
				}
				out, eErr := buildUpstreamRequestFrom(leg.route, leg.model, source, nil)
				if eErr != nil {
					b.Fatal(eErr)
				}
				benchSinkBytes = out
			}
		})
	}
}

// BenchmarkRequestPreparationIdentity isolates the three session-identity
// sources on the heaviest leg (a Responses client converted to Chat
// Completions): the content-digest fallback forces the typed decode, while
// canonical metadata and a session header short-circuit it.
func BenchmarkRequestPreparationIdentity(b *testing.B) {
	leg := benchLegs(256 << 10)[2] // chat <- openai-response, deepseek (reasoning replay)
	modes := []struct {
		name      string
		canonical string
		header    string
	}{
		{"fallback", "", ""},
		{"canonical", "canonical-session", ""},
		{"header", "", "header-session"},
	}
	for _, mode := range modes {
		b.Run(mode.name, func(b *testing.B) {
			req := identityRequest(leg.format, leg.body, mode.canonical, mode.header)
			b.ReportAllocs()
			b.SetBytes(int64(len(leg.body)))
			for b.Loop() {
				source := shared.NewSourceRequest(leg.format, leg.body)
				sid, eErr := resolveOpenCodeSessionIDFrom(req, source)
				if eErr != nil {
					b.Fatal(eErr)
				}
				benchSinkString = sid
				out, eErr := buildUpstreamRequestFrom(leg.route, leg.model, source, nil)
				if eErr != nil {
					b.Fatal(eErr)
				}
				benchSinkBytes = out
			}
		})
	}
}

// BenchmarkRequestPreparationSize measures the same heaviest leg at three body
// sizes to show the linear cost of one decode per request.
func BenchmarkRequestPreparationSize(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"256KiB", 256 << 10},
		{"1MiB", 1 << 20},
		{"4MiB", 4 << 20},
	}
	for _, sz := range sizes {
		leg := benchLegs(sz.size)[2]
		b.Run(sz.name, func(b *testing.B) {
			req := identityRequest(leg.format, leg.body, "", "")
			b.ReportAllocs()
			b.SetBytes(int64(len(leg.body)))
			for b.Loop() {
				source := shared.NewSourceRequest(leg.format, leg.body)
				if _, eErr := resolveOpenCodeSessionIDFrom(req, source); eErr != nil {
					b.Fatal(eErr)
				}
				out, eErr := buildUpstreamRequestFrom(leg.route, leg.model, source, nil)
				if eErr != nil {
					b.Fatal(eErr)
				}
				benchSinkBytes = out
			}
		})
	}
}

// BenchmarkRequestPreparationSharedVsSplit contrasts the shipped one-decode
// path with the pre-refactor two-decode shape (a fresh derivation and a fresh
// conversion, each parsing the body) on the heaviest leg.
func BenchmarkRequestPreparationSharedVsSplit(b *testing.B) {
	leg := benchLegs(256 << 10)[2]
	req := identityRequest(leg.format, leg.body, "", "")

	b.Run("shared", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(leg.body)))
		for b.Loop() {
			source := shared.NewSourceRequest(leg.format, leg.body)
			if _, eErr := resolveOpenCodeSessionIDFrom(req, source); eErr != nil {
				b.Fatal(eErr)
			}
			out, eErr := buildUpstreamRequestFrom(leg.route, leg.model, source, nil)
			if eErr != nil {
				b.Fatal(eErr)
			}
			benchSinkBytes = out
		}
	})

	b.Run("split", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(leg.body)))
		for b.Loop() {
			if _, eErr := deriveOpenCodeSessionID(leg.format, leg.body); eErr != nil {
				b.Fatal(eErr)
			}
			out, eErr := buildUpstreamRequest(leg.route, leg.model, leg.format, leg.body, nil)
			if eErr != nil {
				b.Fatal(eErr)
			}
			benchSinkBytes = out
		}
	})
}

// BenchmarkFullExecutor times the whole non-stream executor over the bridged
// boundary: envelope decode (incl. base64), preparation, host HTTP call, and
// response conversion. The executor envelope is marshalled OUTSIDE the timed
// loop — base64-encoding the body there is harness cost, not plugin cost.
func BenchmarkFullExecutor(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"256KiB", 256 << 10},
		{"1MiB", 1 << 20},
		{"4MiB", 4 << 20},
	}
	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			m, err := newBenchManager(b)
			if err != nil {
				b.Fatal(err)
			}
			body := benchResponsesBody(sz.size)
			payload := execReqBodyWithKey("opencode-go/deepseek-v4-pro", "openai-response", body, false, testKey)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				resp, callErr := m.HandleCall("executor.execute", payload)
				if callErr != nil {
					b.Fatal(callErr)
				}
				env := decodeBenchEnvelope(b, resp)
				if !env.OK {
					b.Fatalf("executor failed: %s", resp)
				}
				benchSinkBytes = env.Result
			}
		})
	}
}

// newBenchManager wires a manager whose outbound calls are answered by canned
// route responses behind a catalog body listing deepseek-v4-pro.
func newBenchManager(b *testing.B) (*Manager, error) {
	b.Helper()
	const catalogJSON = `{"data":[{"id":"deepseek-v4-pro"}]}`
	f := &fakeCaller{noRecordPayloads: true}
	m := NewManager(NewHostBridge(f.call))
	b.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(catalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if strings.HasSuffix(url, "/chat/completions") {
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(ccResponseBody),
			}), nil
		}
		b.Errorf("no canned upstream for url %v", url)
		return hostErr("test", "unrouted"), nil
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		return nil, err
	}
	return m, nil
}

func decodeBenchEnvelope(b *testing.B, raw []byte) pluginabi.Envelope {
	b.Helper()
	env, err := decodeEnvelope(raw)
	if err != nil {
		b.Fatalf("decode envelope: %v", err)
	}
	return env
}
