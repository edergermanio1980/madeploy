// Package deploy plans and executes a deploy. It is the only package that
// changes the server; the CLI (and a future TUI) only consume its events.
package deploy

import (
	"encoding/json"
	"regexp"
	"time"
)

// Event is one line of the structured deploy log.
type Event struct {
	Time     time.Time `json:"time"`
	Step     string    `json:"step"`
	Level    string    `json:"level"` // info, warn, error, done
	Msg      string    `json:"msg"`
	Duration string    `json:"duration,omitempty"`
}

// Emitter receives events as they happen.
type Emitter func(Event)

var secretRe = regexp.MustCompile(`(?i)((?:pass(?:word)?|pwd|token|secret|key)\s*[=:]\s*)\S+`)

// Redact masks values that look like credentials before anything is logged.
func Redact(s string) string { return secretRe.ReplaceAllString(s, "${1}***") }

// JSON renders the event as a single log line.
func (e Event) JSON() []byte {
	b, _ := json.Marshal(e)
	return append(b, '\n')
}
