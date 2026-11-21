// Package files computes what to ship: ignore rules, manifests, git changes, packages.
package files

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// Entry describes one file. Mode is "0644" or "0755" (only the exec bit matters).
type Entry struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode,omitempty"`
}

// Manifest maps a slash-separated relative path to its entry.
type Manifest map[string]Entry

// Paths returns the manifest keys sorted.
func (m Manifest) Paths() []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Matcher decides which paths are excluded from deploy, drift and deletion.
type Matcher struct {
	gi     *ignore.GitIgnore
	prunes []string
}

// NewMatcher combines patterns (gitignore syntax) with root/.madeployignore if present.
func NewMatcher(root string, patterns ...[]string) (*Matcher, error) {
	var lines []string
	for _, p := range patterns {
		lines = append(lines, p...)
	}
	if raw, err := os.ReadFile(filepath.Join(root, ".madeployignore")); err == nil {
		lines = append(lines, strings.Split(string(raw), "\n")...)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("lendo .madeployignore: %w", err)
	}
	m := &Matcher{gi: ignore.CompileIgnoreLines(lines...)}
	for _, l := range lines {
		// "/tmp/" style anchored dirs without globs are pruned during the remote find.
		if strings.HasPrefix(l, "/") && strings.HasSuffix(l, "/") && !strings.ContainsAny(l, "*?[!") {
			m.prunes = append(m.prunes, strings.Trim(l, "/"))
		}
	}
	m.prunes = append(m.prunes, ".git", ".madeploy")
	return m, nil
}

// Excluded reports whether rel (slash-separated) must not be touched.
func (m *Matcher) Excluded(rel string) bool { return m.gi.MatchesPath(rel) }

// Prunes are top-level directories skipped entirely when hashing the server.
func (m *Matcher) Prunes() []string { return m.prunes }

// HashFile returns the entry for a local file.
func HashFile(path string) (Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Entry{}, err
	}
	mode := "0644"
	if st.Mode()&0o111 != 0 {
		mode = "0755"
	}
	return Entry{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n, Mode: mode}, nil
}

// BuildLocal hashes the tracked files plus the extra (untracked) directories.
func BuildLocal(root string, tracked []string, extra []string, m *Matcher) (Manifest, error) {
	out := Manifest{}
	add := func(rel string) error {
		rel = filepath.ToSlash(rel)
		if m.Excluded(rel) {
			return nil
		}
		e, err := HashFile(filepath.Join(root, rel))
		if err != nil {
			return fmt.Errorf("hash %s: %w", rel, err)
		}
		out[rel] = e
		return nil
	}
	for _, rel := range tracked {
		if err := add(rel); err != nil {
			return nil, err
		}
	}
	for _, dir := range extra {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			return add(rel)
		})
		if err != nil {
			return nil, fmt.Errorf("extra %s: %w", dir, err)
		}
	}
	return out, nil
}

// RemoteHashCmd lists "sha256  ./path" for every file under the app path,
// skipping pruned directories. Output is parsed by ParseHashes.
func RemoteHashCmd(appPath string, m *Matcher, quote func(string) string) string {
	return findCmd(appPath, m, quote, " -type f -exec sha256sum {} +")
}

// RemoteSizeCmd lists "<bytes> <path>" for the tree RemoteHashCmd walks
// (GNU find -printf; BSD stat as fallback). ParseSize applies the excludes.
func RemoteSizeCmd(appPath string, m *Matcher, quote func(string) string) string {
	return "{ " + findCmd(appPath, m, quote, " -type f -printf '%s %p\\n'") + "; } 2>/dev/null || { " +
		findCmd(appPath, m, quote, " -type f -exec stat -f '%z %N' {} +") + "; }"
}

// ParseSize totals RemoteSizeCmd output, skipping excluded paths so the count
// matches what Pull extracts.
func ParseSize(out []byte, m *Matcher) (int, int64, error) {
	var n int
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		size, p, ok := strings.Cut(line, " ")
		if line == "" {
			continue
		}
		v, err := strconv.ParseInt(size, 10, 64)
		if !ok || err != nil {
			return 0, 0, fmt.Errorf("contagem do servidor ilegível: %q", line)
		}
		if m.Excluded(strings.TrimPrefix(p, "./")) {
			continue
		}
		n++
		total += v
	}
	return n, total, nil
}

func findCmd(appPath string, m *Matcher, quote func(string) string, action string) string {
	var b strings.Builder
	b.WriteString("cd " + quote(appPath) + " && find .")
	if len(m.prunes) > 0 {
		b.WriteString(" \\(")
		for i, p := range m.prunes {
			if i > 0 {
				b.WriteString(" -o")
			}
			b.WriteString(" -path " + quote("./"+p))
		}
		b.WriteString(" \\) -prune -o")
	}
	b.WriteString(action)
	return b.String()
}

// ParseHashes reads sha256sum output into a manifest (size/mode unknown), applying excludes.
func ParseHashes(out []byte, m *Matcher) (Manifest, error) {
	res := Manifest{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		sum, rel, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 {
			// sha256sum escapes names with newlines/backslashes; refuse instead of guessing.
			return nil, fmt.Errorf("saída de sha256sum inesperada (nome de arquivo incomum?): %q", line)
		}
		rel = strings.TrimPrefix(rel, "./")
		if m.Excluded(rel) {
			continue
		}
		res[rel] = Entry{SHA256: sum}
	}
	return res, sc.Err()
}
