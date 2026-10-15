package plugin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// awaitEmitted polls the downstream emits until one payload contains needle,
// then returns the observed events. The caller controls when the upstream is
// allowed to produce later frames, so a bounded poll here cannot race a fixed
// upstream sleep.
func awaitEmitted(t *testing.T, f *fakeCaller, needle string) []string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		events := emittedEvents(t, f)
		for _, ev := range events {
			if strings.Contains(ev, needle) {
				return events
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no downstream event containing %q within timeout; events=%v", needle, emittedEvents(t, f))
	return nil
}

func newPromptnessManager(t *testing.T, upstreamURL string) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{responder: newForwardingCaller()}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(integrationYAML(upstreamURL)))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("register failed: %s", resp)
	}
	return m, f
}

// TestStreamForwardsFirstDeltaBeforeUpstreamContinues proves the stream pump
// relays each upstream delta as it arrives instead of buffering the turn: the
// upstream withholds the second delta and the terminal frames until the test
// has observed the first delta downstream. Channel gates replace fixed sleeps,
// so the only timing bound is the deadlock guard, not a machine-dependent
// budget.
func TestStreamForwardsFirstDeltaBeforeUpstreamContinues(t *testing.T) {
	releaseSecond := make(chan struct{})
	releaseTerminal := make(chan struct{})
	defer func() {
		// Unblock the handler if a fatal stopped the test early.
		select {
		case <-releaseSecond:
		default:
			close(releaseSecond)
		}
		select {
		case <-releaseTerminal:
		default:
			close(releaseTerminal)
		}
	}()

	const guard = 15 * time.Second
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"deepseek-v4-pro"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		frames := []string{
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"first"}}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"second"}}]}` + "\n\n",
			`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			"data: [DONE]\n\n",
		}
		_, _ = io.WriteString(w, frames[0])
		fl.Flush()
		select {
		case <-releaseSecond:
		case <-time.After(guard):
			return
		}
		_, _ = io.WriteString(w, frames[1])
		fl.Flush()
		select {
		case <-releaseTerminal:
		case <-time.After(guard):
			return
		}
		for _, frame := range frames[2:] {
			_, _ = io.WriteString(w, frame)
		}
		fl.Flush()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m, f := newPromptnessManager(t, srv.URL)
	body := []byte(`{"model":"x","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	mustHandle(t, m, "executor.execute_stream",
		execStreamReqBodyForKey("opencode-go/deepseek-v4-pro", "openai-response", body, "down-1", "sk-test-1"))

	// The upstream is still blocked on releaseSecond; seeing "first" means the
	// plugin already relayed it downstream.
	awaitEmitted(t, f, "first")
	close(releaseSecond)

	awaitEmitted(t, f, "second")
	close(releaseTerminal)

	if !m.bridge.WaitForInFlight(guard) {
		t.Fatal("stream did not finish after the upstream sent its terminal frames")
	}
	events := emittedEvents(t, f)
	if len(events) < 2 {
		t.Fatalf("expected at least the two text deltas downstream, got %v", events)
	}
	if !strings.Contains(strings.Join(events, ""), "second") {
		t.Fatalf("second delta lost: %v", events)
	}
}
