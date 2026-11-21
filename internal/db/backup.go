package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vitalsoft/madeploy/internal/deploy"
	"github.com/vitalsoft/madeploy/internal/remote"
)

// backup dumps the tables the pending migrations touch (or the whole database)
// on the server, with mysqldump --single-transaction, before anything runs.
func (r *Runner) backup(ctx context.Context, s *Session, todo []Migration, backupDir string, emit deploy.Emitter) error {
	mode := r.Cfg.Databases[s.Conn.Name].Backup
	if mode == "none" {
		return nil
	}
	var tables []string
	if mode == "tables" {
		seen := map[string]bool{}
		for _, m := range todo {
			for _, st := range m.Stmts {
				if t := TouchedTable(st); t != "" && !seen[t] {
					seen[t] = true
					tables = append(tables, t)
				}
			}
		}
		sort.Strings(tables)
		if tables = existingTables(ctx, s, tables); len(tables) == 0 {
			return nil // only new objects: nothing to save
		}
	}
	out := backupDir + "/db-" + s.Conn.Name + ".sql"
	cnf := backupDir + "/.my-" + randHex() + ".cnf"
	if err := r.Host.WriteFile(cnf, []byte(MyCnf(s.Conn)), 0o600); err != nil {
		return err
	}
	if _, err := remote.Run(ctx, r.Host, DumpCmd(cnf, s.Conn.DB, tables, out, r.Cfg.Databases[s.Conn.Name].DumpOptions...)); err != nil {
		return err
	}
	if emit != nil {
		emit(deploy.Event{Time: r.now().UTC(), Step: "migrate", Level: "info",
			Msg: fmt.Sprintf("%s: backup (%s) em %s.gz", s.Conn.Name, describe(tables), out)})
	}
	return nil
}

func describe(tables []string) string {
	if len(tables) == 0 {
		return "banco completo"
	}
	return strings.Join(tables, ", ")
}

// existingTables drops names that do not exist yet (created by the migration).
func existingTables(ctx context.Context, s *Session, names []string) []string {
	var out []string
	for _, t := range names {
		var n int
		err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", s.Conn.DB, t).Scan(&n)
		if err == nil && n > 0 {
			out = append(out, t)
		}
	}
	return out
}

// MyCnf is a client option file, so the password never shows up in ps or logs.
func MyCnf(c Conn) string {
	q := func(v string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"` }
	return "[client]\nuser=" + q(c.User) + "\npassword=" + q(c.Pass) + "\nhost=" + q(c.Host) + "\nport=" + strconv.Itoa(c.Port) + "\n"
}

// DumpCmd removes the option file even when mysqldump fails.
func DumpCmd(cnf, db string, tables []string, out string, extra ...string) string {
	args := []string{"mysqldump", "--defaults-extra-file=" + cnf, "--single-transaction", "--no-tablespaces", "--triggers", "--routines"}
	args = append(append(append(args, extra...), db), tables...)
	return "trap " + remote.Quote("rm -f "+remote.Quote(cnf)) + " EXIT; umask 077; " +
		remote.Cmd(args...) + " > " + remote.Quote(out) + " && gzip -f " + remote.Quote(out)
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
