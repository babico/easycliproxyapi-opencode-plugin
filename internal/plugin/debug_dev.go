//go:build debug

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// debugBodyMeta records constant-time metadata about a request body without
// parsing or inspecting it: the byte length plus an explicit "unchecked"
// JSON marker. The request path is hot, so neither a generic json.Unmarshal
// nor a full-body json.Valid scan is acceptable here, and the body itself may
// carry prompts or credentials, so no bytes are ever echoed.
func debugBodyMeta(body []byte) string {
	return fmt.Sprintf("len=%d json=unchecked", len(body))
}

var debugMu sync.Mutex

// debugTrace is temporary diagnosis instrumentation. It records only control
// flow and non-secret metadata; credentials, headers, prompts, and bodies are
// deliberately excluded.
func debugTrace(format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()

	path := filepath.Join(filepath.Dir(os.Args[0]), "opencode-go-debug.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	values := append([]any{time.Now().Format(time.RFC3339Nano)}, args...)
	_, _ = fmt.Fprintf(f, "%s "+format+"\n", values...)
}
