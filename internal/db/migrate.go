package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/deploy"
	"github.com/vitalsoft/madeploy/internal/remote"
)

const tableDDL = `CREATE TABLE IF NOT EXISTS madeploy_migrations (
  id VARCHAR(255) NOT NULL PRIMARY KEY,
  checksum CHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL,
  statements_total INT NOT NULL,
  statements_done INT NOT NULL,
  applied_at DATETIME NULL,
  duration_ms INT NOT NULL DEFAULT 0,
  operator VARCHAR(255) NOT NULL,
  release_id VARCHAR(32) NOT NULL
)`

// Migration is one versioned SQL file, applied once.
type Migration struct {
	Conn     string   `json:"conn"`
	ID       string   `json:"id"` // "migrations/<file>" or "data/<file>"
	Checksum string   `json:"checksum"`
	Stmts    []string `json:"-"`
}

// Record is a row of madeploy_migrations.
type Record struct {
	ID       string
	Checksum string
	Status   string // running, failed, applied
	Total    int
	Done     int
}

// StatusRow is what `db status` prints.
type StatusRow struct {
	Conn   string `json:"conn"`
	ID     string `json:"id"`
	State  string `json:"state"` // pending, applied, partial, checksum-mismatch
	Detail string `json:"detail,omitempty"`
}

// Options gate risky migrations.
type Options struct {
	AllowDestructive bool
	AllowLarge       bool
	Resume           bool // continue a partial migration after statements_done
}

// Runner applies migrations of the configured connections. It implements deploy.Migrator.
type Runner struct {
	Host     remote.Host
	Cfg      *config.Config
	AppPath  string
	Root     string
	Conns    []string // empty = all configured
	Operator string
	Release  string
	Opts     Options
	Now      func() time.Time
}

func (r *Runner) conns() []string {
	if len(r.Conns) > 0 {
		return r.Conns
	}
	return r.Cfg.DatabaseNames()
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Local reads the migration files of a connection, ordered by file name
// (timestamp prefix), schema and data interleaved.
func (r *Runner) Local(conn string) ([]Migration, error) {
	var out []Migration
	for _, kind := range []string{"migrations", "data"} {
		paths, err := filepath.Glob(filepath.Join(r.Root, "database", conn, kind, "*.sql"))
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			raw, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(raw)
			out = append(out, Migration{Conn: conn, ID: kind + "/" + filepath.Base(p), Checksum: hex.EncodeToString(sum[:]), Stmts: SplitSQL(string(raw))})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return path.Base(out[i].ID) < path.Base(out[j].ID) })
	return out, nil
}

func (r *Runner) open(ctx context.Context, conn string) (*Session, error) {
	db, ok := r.Cfg.Databases[conn]
	if !ok {
		return nil, fmt.Errorf("conexão %q não está em databases", conn)
	}
	return Open(ctx, r.Host, r.AppPath, conn, db.Config)
}

func records(ctx context.Context, s *Session) (map[string]Record, error) {
	if _, err := s.DB.ExecContext(ctx, tableDDL); err != nil {
		return nil, fmt.Errorf("criando madeploy_migrations: %w", err)
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT id, checksum, status, statements_total, statements_done FROM madeploy_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := map[string]Record{}
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.Checksum, &rec.Status, &rec.Total, &rec.Done); err != nil {
			return nil, err
		}
		res[rec.ID] = rec
	}
	return res, rows.Err()
}

func classify(m Migration, recs map[string]Record) StatusRow {
	row := StatusRow{Conn: m.Conn, ID: m.ID, State: "pending"}
	rec, ok := recs[m.ID]
	switch {
	case !ok:
	case rec.Checksum != m.Checksum:
		row.State, row.Detail = "checksum-mismatch", "arquivo alterado depois de aplicado; crie uma migração nova"
	case rec.Status == "applied":
		row.State = "applied"
	default:
		row.State, row.Detail = "partial", fmt.Sprintf("%d de %d instruções aplicadas (%s)", rec.Done, rec.Total, rec.Status)
	}
	return row
}

// Status compares local files with madeploy_migrations for every connection.
func (r *Runner) Status(ctx context.Context) ([]StatusRow, error) {
	var out []StatusRow
	for _, conn := range r.conns() {
		local, err := r.Local(conn)
		if err != nil {
			return nil, err
		}
		if len(local) == 0 {
			continue
		}
		s, err := r.open(ctx, conn)
		if err != nil {
			return nil, err
		}
		recs, err := records(ctx, s)
		s.Close()
		if err != nil {
			return nil, err
		}
		for _, m := range local {
			out = append(out, classify(m, recs))
		}
	}
	return out, nil
}

// Pending lists "conn: id" of migrations still to apply (deploy.Migrator).
func (r *Runner) Pending(ctx context.Context) ([]string, error) {
	rows, err := r.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows {
		if row.State != "applied" {
			out = append(out, row.Conn+": "+row.ID+" ("+row.State+")")
		}
	}
	return out, nil
}

// Apply runs pending migrations connection by connection, statement by
// statement, recording progress after each one (MySQL DDL is not transactional).
func (r *Runner) Apply(ctx context.Context, backupDir string, emit deploy.Emitter) error {
	for _, conn := range r.conns() {
		if err := r.applyConn(ctx, conn, backupDir, emit); err != nil {
			return fmt.Errorf("%s: %w", conn, err)
		}
	}
	return nil
}

func (r *Runner) applyConn(ctx context.Context, conn, backupDir string, emit deploy.Emitter) error {
	local, err := r.Local(conn)
	if err != nil || len(local) == 0 {
		return err
	}
	s, err := r.open(ctx, conn)
	if err != nil {
		return err
	}
	defer s.Close()
	recs, err := records(ctx, s)
	if err != nil {
		return err
	}
	var todo []Migration
	for _, m := range local {
		switch row := classify(m, recs); row.State {
		case "applied":
		case "checksum-mismatch":
			return fmt.Errorf("%s: %s", m.ID, row.Detail)
		case "partial":
			if !r.Opts.Resume {
				return fmt.Errorf("%s: %s; corrija à mão e use `madeploy db apply --resume` ou `madeploy db resolve --conn %s %s`", m.ID, row.Detail, conn, m.ID)
			}
			todo = append(todo, m)
		default:
			todo = append(todo, m)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	if err := r.guard(ctx, s, todo); err != nil {
		return err
	}
	if err := r.backup(ctx, s, todo, backupDir, emit); err != nil {
		return fmt.Errorf("backup do banco: %w", err)
	}
	for _, m := range todo {
		if err := r.applyOne(ctx, s, m, recs[m.ID].Done, emit); err != nil {
			return err
		}
	}
	return nil
}

// guard blocks destructive statements and ALTERs on large tables unless allowed.
func (r *Runner) guard(ctx context.Context, s *Session, todo []Migration) error {
	limit := r.Cfg.Databases[s.Conn.Name].LargeTableRows
	var problems []string
	for _, m := range todo {
		for i, st := range m.Stmts {
			if risk := Classify(st); risk.Level == Destructive && !r.Opts.AllowDestructive {
				problems = append(problems, fmt.Sprintf("%s #%d DESTRUTIVA (%s) — exige --allow-destructive", m.ID, i+1, strings.Join(risk.Reasons, "; ")))
			}
			table := AlteredTable(st)
			if table == "" || r.Opts.AllowLarge {
				continue
			}
			var rows sql.NullInt64
			err := s.DB.QueryRowContext(ctx, "SELECT TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", s.Conn.DB, table).Scan(&rows)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if rows.Int64 > limit {
				problems = append(problems, fmt.Sprintf("%s #%d: ALTER em %s (~%d linhas > %d) pode travar a tabela; prefira gh-ost/pt-online-schema-change ou use --allow-large", m.ID, i+1, table, rows.Int64, limit))
			}
		}
	}
	if len(problems) > 0 {
		return errors.New("migração bloqueada:\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

func (r *Runner) applyOne(ctx context.Context, s *Session, m Migration, from int, emit deploy.Emitter) error {
	say := func(level, msg string) {
		if emit != nil {
			emit(deploy.Event{Time: r.now().UTC(), Step: "migrate", Level: level, Msg: s.Conn.Name + ": " + msg})
		}
	}
	start := r.now()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO madeploy_migrations (id, checksum, status, statements_total, statements_done, operator, release_id)
		VALUES (?, ?, 'running', ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE status = 'running', operator = VALUES(operator), release_id = VALUES(release_id)`,
		m.ID, m.Checksum, len(m.Stmts), from, r.Operator, r.Release)
	if err != nil {
		return err
	}
	for i := from; i < len(m.Stmts); i++ {
		say("info", fmt.Sprintf("%s [%d/%d] %.100s", m.ID, i+1, len(m.Stmts), oneLine(m.Stmts[i])))
		if _, err := s.DB.ExecContext(ctx, m.Stmts[i]); err != nil {
			_, _ = s.DB.ExecContext(context.WithoutCancel(ctx), "UPDATE madeploy_migrations SET status = 'failed' WHERE id = ?", m.ID)
			return fmt.Errorf("%s: instrução %d de %d falhou: %w\n  já aplicadas: %d · não aplicadas: %d..%d\n  SQL: %s",
				m.ID, i+1, len(m.Stmts), err, i, i+1, len(m.Stmts), oneLine(m.Stmts[i]))
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE madeploy_migrations SET statements_done = ? WHERE id = ?", i+1, m.ID); err != nil {
			return err
		}
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE madeploy_migrations SET status = 'applied', applied_at = UTC_TIMESTAMP(), duration_ms = ? WHERE id = ?",
		r.now().Sub(start).Milliseconds(), m.ID)
	say("done", m.ID+" aplicada")
	return err
}

// MarkApplied records a migration as applied without running it (after a manual fix).
func (r *Runner) MarkApplied(ctx context.Context, conn, id string) error {
	local, err := r.Local(conn)
	if err != nil {
		return err
	}
	for _, m := range local {
		if m.ID != id {
			continue
		}
		s, err := r.open(ctx, conn)
		if err != nil {
			return err
		}
		defer s.Close()
		if _, err := records(ctx, s); err != nil {
			return err
		}
		_, err = s.DB.ExecContext(ctx, `INSERT INTO madeploy_migrations (id, checksum, status, statements_total, statements_done, applied_at, operator, release_id)
			VALUES (?, ?, 'applied', ?, ?, UTC_TIMESTAMP(), ?, 'manual')
			ON DUPLICATE KEY UPDATE checksum = VALUES(checksum), status = 'applied', statements_done = VALUES(statements_done), applied_at = UTC_TIMESTAMP(), operator = VALUES(operator)`,
			m.ID, m.Checksum, len(m.Stmts), len(m.Stmts), r.Operator)
		return err
	}
	return fmt.Errorf("migração %s não encontrada em database/%s", id, conn)
}

// oneLine renders a statement on one line, without its comment lines.
func oneLine(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); !strings.HasPrefix(t, "--") && !strings.HasPrefix(t, "#") {
			keep = append(keep, t)
		}
	}
	return strings.Join(strings.Fields(strings.Join(keep, " ")), " ")
}
