//go:build debug

package plugin

import (
	"fmt"
	"strings"
	"testing"
)

// TestDebugBodyMetaIsConstantTimeMetadataOnly pins the -tags debug request-path
// behavior: debugBodyMeta reports only the byte length and an explicit
// "unchecked" JSON marker. It must neither parse the body (a full generic
// json.Unmarshal or json.Valid scan is far too expensive on the hot path) nor
// echo any input bytes, which may carry prompts or credentials.
func TestDebugBodyMetaIsConstantTimeMetadataOnly(t *testing.T) {
	const secret = "sk-live-DEADBEEF-do-not-log"
	cases := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "valid", body: `{"messages":[{"role":"user","content":"hello"}]}`},
		{name: "malformed", body: `{"messages":`},
		{name: "credential-looking", body: `{"api_key":"` + secret + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := debugBodyMeta([]byte(tc.body))
			want := fmt.Sprintf("len=%d json=unchecked", len(tc.body))
			if got != want {
				t.Fatalf("debugBodyMeta(%q) = %q, want %q", tc.body, got, want)
			}
			for _, leak := range []string{secret, "messages", "api_key", "hello", "first=", "last=", "error_offset"} {
				if strings.Contains(got, leak) {
					t.Fatalf("debugBodyMeta leaked %q in %q", leak, got)
				}
			}
		})
	}
}
