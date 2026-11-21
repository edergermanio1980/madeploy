package db

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaDiffer is the seam around the declarative diff tool (ADR-03).
type SchemaDiffer interface {
	Export(ctx context.Context, s *Session) (string, error)
	// Diff returns the DDL to go from the live schema to desired, plus the
	// destructive statements the tool skipped because allowDrop is false.
	Diff(ctx context.Context, s *Session, desired string, allowDrop bool) (stmts, skipped []string, err error)
}

// Mysqldef runs the sqldef `mysqldef` binary on this machine against the tunnel.
type Mysqldef struct{ Bin string }

func (m Mysqldef) bin() string {
	if m.Bin != "" {
		return m.Bin
	}
	return "mysqldef"
}

func (m Mysqldef) run(ctx context.Context, s *Session, stdin string, args ...string) (string, error) {
	host, port, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return "", err
	}
	if _, err := exec.LookPath(m.bin()); err != nil {
		return "", fmt.Errorf("mysqldef não encontrado: instale com `go install github.com/sqldef/sqldef/v3/cmd/mysqldef@v3.11.30`")
	}
	base := []string{"-u", s.Conn.User, "-h", host, "-P", port}
	cmd := exec.CommandContext(ctx, m.bin(), append(append(base, args...), s.Conn.DB)...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+s.Conn.Pass) // never on argv
	cmd.Stdin = strings.NewReader(stdin)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mysqldef: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

func (m Mysqldef) Export(ctx context.Context, s *Session) (string, error) {
	return m.run(ctx, s, "", "--export")
}

func (m Mysqldef) Diff(ctx context.Context, s *Session, desired string, allowDrop bool) ([]string, []string, error) {
	// statements are applied one by one by the Runner (ADR-08); no BEGIN/COMMIT wrapper
	args := []string{"--dry-run", "--disable-ddl-transaction"}
	if allowDrop {
		args = append(args, "--enable-drop")
	}
	out, err := m.run(ctx, s, desired, args...)
	if err != nil {
		return nil, nil, err
	}
	stmts, skipped := parseDryRun(out)
	return withoutOwnTable(stmts), withoutOwnTable(skipped), nil
}

var ownTableRe = regexp.MustCompile("(?i)`?madeploy_migrations`?")

// withoutOwnTable drops statements about madeploy's bookkeeping table: it is
// never in the application schema, so sqldef would create or drop it.
func withoutOwnTable(stmts []string) []string {
	var out []string
	for _, s := range stmts {
		if !ownTableRe.MatchString(s) {
			out = append(out, s)
		}
	}
	return out
}

var skippedRe = regexp.MustCompile(`(?m)^--\s*Skipped:\s*(.+)$`)

// parseDryRun separates the statements from "-- Skipped: ..." lines.
func parseDryRun(out string) (stmts, skipped []string) {
	for _, m := range skippedRe.FindAllStringSubmatch(out, -1) {
		skipped = append(skipped, strings.TrimSuffix(strings.TrimSpace(m[1]), ";"))
	}
	var body []string
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			body = append(body, l)
		}
	}
	return SplitSQL(strings.Join(body, "\n")), skipped
}

var objRe = regexp.MustCompile("(?is)^\\s*CREATE\\s+(?:OR\\s+REPLACE\\s+)?(?:ALGORITHM\\s*=\\s*\\w+\\s+)?(?:DEFINER\\s*=\\s*\\S+\\s+)?(?:SQL\\s+SECURITY\\s+\\w+\\s+)?(TABLE|VIEW|TRIGGER|PROCEDURE|FUNCTION)\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?`?(\\w+)`?")

// order makes the concatenated desired schema load tables before views and triggers.
var order = map[string]int{"table": 0, "view": 1, "trigger": 2, "procedure": 3, "function": 3}

// WriteSchema splits an export into one file per object under dir, replacing
// the previous *.sql files (git shows what changed).
func WriteSchema(dir, export string) (int, error) {
	var stmts []string
	for _, stmt := range SplitSQL(export) { // validate before deleting anything
		m := objRe.FindStringSubmatch(stmt)
		if m == nil {
			return 0, fmt.Errorf("instrução não reconhecida no export: %.80s", stmt)
		}
		if m[2] != "madeploy_migrations" { // ours, not the application's
			stmts = append(stmts, stmt)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	old, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	for _, f := range old {
		if err := os.Remove(f); err != nil {
			return 0, err
		}
	}
	for _, stmt := range stmts {
		m := objRe.FindStringSubmatch(stmt)
		name := strings.ToLower(m[1]) + "_" + m[2] + ".sql"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stmt+";\n"), 0o644); err != nil {
			return 0, err
		}
	}
	return len(stmts), nil
}

// DesiredSQL concatenates the schema files: tables, then views, then triggers.
func DesiredSQL(dir string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("%s vazio: rode `madeploy db pull` primeiro", dir)
	}
	kind := func(p string) int {
		k, _, _ := strings.Cut(filepath.Base(p), "_")
		if v, ok := order[k]; ok {
			return v
		}
		return 9
	}
	sort.SliceStable(paths, func(i, j int) bool {
		if kind(paths[i]) != kind(paths[j]) {
			return kind(paths[i]) < kind(paths[j])
		}
		return paths[i] < paths[j]
	})
	var b strings.Builder
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		b.Write(bytes.TrimSpace(raw))
		b.WriteString("\n\n")
	}
	return b.String(), nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// SaveMigration writes statements to dir/<timestamp>_<slug>.sql with a risk header per statement.
func SaveMigration(dir, slug string, now time.Time, stmts []string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	slug = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(slug), "_"), "_")
	if slug == "" {
		slug = "schema"
	}
	file := filepath.Join(dir, now.UTC().Format("20060102150405")+"_"+slug+".sql")
	var b strings.Builder
	fmt.Fprintf(&b, "-- gerado por madeploy em %s\n\n", now.UTC().Format(time.RFC3339))
	for _, s := range stmts {
		if r := Classify(s); r.Level != Safe {
			fmt.Fprintf(&b, "-- RISCO %s: %s\n", strings.ToUpper(r.Level), strings.Join(r.Reasons, "; "))
		}
		b.WriteString(strings.TrimSpace(s) + ";\n\n")
	}
	return file, os.WriteFile(file, []byte(b.String()), 0o644)
}
