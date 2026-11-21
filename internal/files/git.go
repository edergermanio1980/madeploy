package files

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Git runs read-only git commands in a repository.
type Git struct{ Dir string }

func (g Git) run(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g Git) Head() (string, error) {
	out, err := g.run("rev-parse", "HEAD")
	return strings.TrimSpace(string(out)), err
}

func (g Git) Branch() (string, error) {
	out, err := g.run("rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(string(out)), err
}

// Clean reports uncommitted changes to tracked files. Untracked files are
// ignored: they are never shipped unless listed in extra.
func (g Git) Clean() (bool, []string, error) {
	out, err := g.run("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, nil, err
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return len(lines) == 0, lines, nil
}

// Tracked lists files committed at HEAD.
func (g Git) Tracked() ([]string, error) {
	out, err := g.run("ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var res []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			res = append(res, p)
		}
	}
	return res, nil
}

// Exists reports whether the commit is known locally.
func (g Git) Exists(commit string) bool {
	_, err := g.run("cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// Change is one line of git diff --name-status.
type Change struct {
	Status string `json:"status"` // A, M, D, R
	Path   string `json:"path"`
	From   string `json:"from,omitempty"` // for renames
}

// Diff returns name-status changes between two commits with rename detection.
func (g Git) Diff(from, to string) ([]Change, error) {
	out, err := g.run("diff", "--name-status", "-z", "-M", from, to)
	if err != nil {
		return nil, err
	}
	return parseNameStatus(string(out))
}

func parseNameStatus(out string) ([]Change, error) {
	parts := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var res []Change
	for i := 0; i < len(parts); i++ {
		st := parts[i]
		if st == "" {
			continue
		}
		if st[0] == 'R' || st[0] == 'C' {
			if i+2 >= len(parts) {
				return nil, fmt.Errorf("git diff: saída truncada")
			}
			res = append(res, Change{Status: st[:1], From: parts[i+1], Path: parts[i+2]})
			i += 2
			continue
		}
		if i+1 >= len(parts) {
			return nil, fmt.Errorf("git diff: saída truncada")
		}
		res = append(res, Change{Status: st[:1], Path: parts[i+1]})
		i++
	}
	return res, nil
}
