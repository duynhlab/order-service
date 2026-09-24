package reconcile

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/duynhlab/pkg/logger/slogx"
)

// observed is a test sink for the logging facade: the facade writes JSON lines
// into it, and tests query them the way they used zap's observer.
type observed struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	entries []observedEntry // set on filtered views
	view    bool
}

// observedEntry is one decoded record.
type observedEntry map[string]any

// ContextMap returns the record's fields.
func (e observedEntry) ContextMap() map[string]any { return e }

func (o *observed) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

// newObserver returns a facade logger at level and the sink it writes to.
func newObserver(level string) (*slogx.Logger, *observed) {
	o := &observed{}
	return slogx.New(slogx.Config{Level: level, Stdout: o}), o
}

// All decodes every record written so far.
func (o *observed) All() []observedEntry {
	if o.view {
		return o.entries
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []observedEntry
	for _, line := range strings.Split(strings.TrimSpace(o.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// Len counts the records.
func (o *observed) Len() int { return len(o.All()) }

// FilterMessageSnippet keeps the records whose message contains s.
func (o *observed) FilterMessageSnippet(s string) *observed {
	v := &observed{view: true}
	for _, e := range o.All() {
		if msg, _ := e["message"].(string); strings.Contains(msg, s) {
			v.entries = append(v.entries, e)
		}
	}
	return v
}
