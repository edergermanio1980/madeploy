// Package remote runs commands and moves files on the target server.
package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Host is everything the deploy needs from a server.
// Commands are shell strings built with Cmd so every argument is escaped.
type Host interface {
	Stream(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error
	ReadFile(path string) ([]byte, error) // os.ErrNotExist when missing
	WriteFile(path string, data []byte, mode os.FileMode) error
	Upload(localPath, remotePath string) error
	Close() error
}

// Run executes cmd and returns its stdout.
func Run(ctx context.Context, h Host, cmd string) ([]byte, error) {
	var out bytes.Buffer
	err := h.Stream(ctx, cmd, nil, &out)
	return out.Bytes(), err
}

var safeArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Quote escapes one argument for POSIX sh.
func Quote(s string) string {
	if s != "" && safeArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Cmd joins argv into a shell command with every element quoted.
func Cmd(argv ...string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// ExitError carries stderr so callers can show what failed.
type ExitError struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	return fmt.Sprintf("comando remoto falhou (%v): %s\n  %s", e.Err, e.Cmd, msg)
}

func (e *ExitError) Unwrap() error { return e.Err }

// Local runs everything on this machine through sh. Used by tests; it
// exercises the exact same command strings the SSH host receives.
type Local struct{}

func (Local) Stream(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = append(os.Environ(), "COPYFILE_DISABLE=1") // macOS tar: no ._ AppleDouble entries
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, &stderr
	if err := c.Run(); err != nil {
		return &ExitError{Cmd: cmd, Stderr: stderr.String(), Err: err}
	}
	return nil
}

func (Local) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (Local) WriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func (Local) Upload(localPath, remotePath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	return Local{}.WriteFile(remotePath, data, 0o600)
}

func (Local) Close() error { return nil }
