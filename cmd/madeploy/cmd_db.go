package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vitalsoft/madeploy/internal/db"
	"github.com/vitalsoft/madeploy/internal/deploy"
	"github.com/vitalsoft/madeploy/internal/programs"
)

// connsFlag returns the selected connections, validating the name.
func connsFlag(a *app, conn string) ([]string, error) {
	if conn == "" {
		return a.cfg.DatabaseNames(), nil
	}
	if _, ok := a.cfg.Databases[conn]; !ok {
		return nil, configError{fmt.Errorf("conexão %q não está em databases", conn)}
	}
	return []string{conn}, nil
}

func open(cmd *cobra.Command, a *app, conn string) (*db.Session, error) {
	return db.Open(cmd.Context(), a.host, a.d.Env.Path, conn, a.cfg.Databases[conn].Config)
}

func dbCmd() *cobra.Command {
	var conn string
	c := &cobra.Command{Use: "db", Short: "Esquema e migrações (pull, diff, apply, status, resolve)"}
	c.PersistentFlags().StringVar(&conn, "conn", "", "conexão (padrão: todas de databases)")
	c.AddCommand(dbPullCmd(&conn), dbDiffCmd(&conn), dbApplyCmd(&conn), dbStatusCmd(&conn), dbResolveCmd(&conn))
	return c
}

func dbPullCmd(conn *string) *cobra.Command {
	return &cobra.Command{
		Use:   "pull <ambiente>",
		Short: "Importa o esquema de produção para database/<conn>/schema (um arquivo por objeto)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			names, err := connsFlag(a, *conn)
			if err != nil {
				return err
			}
			for _, n := range names {
				s, err := open(cmd, a, n)
				if err != nil {
					return err
				}
				export, err := db.Mysqldef{}.Export(cmd.Context(), s)
				s.Close()
				if err != nil {
					return fmt.Errorf("%s: %w", n, err)
				}
				dir := filepath.Join(a.root, "database", n, "schema")
				count, err := db.WriteSchema(dir, export)
				if err != nil {
					return fmt.Errorf("%s: %w", n, err)
				}
				fmt.Fprintf(os.Stderr, "%s: %d objeto(s) em %s\n", n, count, dir)
			}
			return nil
		},
	}
}

type stmtOut struct {
	SQL  string  `json:"sql"`
	Risk db.Risk `json:"risk"`
}

type diffResult struct {
	Conn    string    `json:"conn"`
	File    string    `json:"file,omitempty"`
	Stmts   []stmtOut `json:"statements"`
	Skipped []string  `json:"skipped_destructive"`
}

func dbDiffCmd(conn *string) *cobra.Command {
	var allowDestructive bool
	var slug string
	c := &cobra.Command{
		Use:   "diff <ambiente>",
		Short: "Compara database/<conn>/schema com produção e salva a migração gerada",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			names, err := connsFlag(a, *conn)
			if err != nil {
				return err
			}
			var results []diffResult
			for _, n := range names {
				dir := filepath.Join(a.root, "database", n, "schema")
				if _, err := os.Stat(dir); os.IsNotExist(err) {
					continue // connection without a versioned schema
				}
				r, err := diffOne(cmd.Context(), a, n, dir, slug, allowDestructive, true)
				if err != nil {
					return fmt.Errorf("%s: %w", n, err)
				}
				results = append(results, r)
			}
			if g.json {
				return printJSON(results)
			}
			for _, r := range results {
				renderStmts(r.Conn, r.Stmts)
				for _, s := range r.Skipped {
					fmt.Printf("  [fora do plano — destrutiva, exige --allow-destructive] %s\n", s)
				}
				if r.File != "" {
					fmt.Printf("  salvo em %s (revise e faça commit; aplica no próximo deploy ou com `db apply`)\n", r.File)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&allowDestructive, "allow-destructive", false, "inclui DROPs no plano")
	c.Flags().StringVar(&slug, "name", "schema", "nome curto da migração")
	return c
}

// diffOne computes the schema diff of one connection; save writes the migration file.
func diffOne(ctx context.Context, a *app, conn, dir, slug string, allowDrop, save bool) (diffResult, error) {
	r := diffResult{Conn: conn}
	desired, err := db.DesiredSQL(dir)
	if err != nil {
		return r, err
	}
	s, err := db.Open(ctx, a.host, a.d.Env.Path, conn, a.cfg.Databases[conn].Config)
	if err != nil {
		return r, err
	}
	stmts, skipped, err := db.Mysqldef{}.Diff(ctx, s, desired, allowDrop)
	s.Close()
	if err != nil {
		return r, err
	}
	r.Skipped = skipped
	for _, st := range stmts {
		r.Stmts = append(r.Stmts, stmtOut{SQL: st, Risk: db.Classify(st)})
	}
	if len(stmts) > 0 && save {
		r.File, err = db.SaveMigration(filepath.Join(a.root, "database", conn, "migrations"), slug, time.Now(), stmts)
	}
	return r, err
}

func dbApplyCmd(conn *string) *cobra.Command {
	var allowDestructive, allowLarge, resume bool
	c := &cobra.Command{
		Use:   "apply <ambiente>",
		Short: "Aplica as migrações pendentes (com backup das tabelas afetadas)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			names, err := connsFlag(a, *conn)
			if err != nil {
				return err
			}
			id := deploy.NewID(time.Now())
			r := a.runner(names, db.Options{AllowDestructive: allowDestructive, AllowLarge: allowLarge, Resume: resume}, id)
			pending, err := r.Pending(cmd.Context())
			if err != nil {
				return err
			}
			if len(pending) == 0 {
				fmt.Fprintln(os.Stderr, "nenhuma migração pendente")
				return nil
			}
			fmt.Fprintf(os.Stderr, "Migrações pendentes:\n  %s\n", strings.Join(pending, "\n  "))
			if err := confirm(a.env, "as migrações"); err != nil {
				return err
			}
			return withLock(cmd, a, func() error {
				return r.Apply(cmd.Context(), a.d.MD("db-backups", id), a.d.Emit)
			})
		},
	}
	c.Flags().BoolVar(&allowDestructive, "allow-destructive", false, "permite instruções destrutivas")
	c.Flags().BoolVar(&allowLarge, "allow-large", false, "permite ALTER em tabela grande")
	c.Flags().BoolVar(&resume, "resume", false, "continua uma migração parcial a partir da instrução que falhou")
	return c
}

// withLock runs fn holding the deploy lock, so db changes never overlap a deploy.
func withLock(cmd *cobra.Command, a *app, fn func() error) (err error) {
	if err := a.d.Lock(cmd.Context()); err != nil {
		return err
	}
	defer func() {
		if uerr := a.d.Unlock(cmd.Context()); err == nil {
			err = uerr
		}
	}()
	return fn()
}

func dbStatusCmd(conn *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status <ambiente>",
		Short: "Migrações aplicadas, pendentes e parciais",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			names, err := connsFlag(a, *conn)
			if err != nil {
				return err
			}
			rows, err := a.runner(names, db.Options{}, "").Status(cmd.Context())
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(rows)
			}
			for _, r := range rows {
				fmt.Printf("%-12s %-18s %s %s\n", r.Conn, r.State, r.ID, r.Detail)
			}
			return nil
		},
	}
}

func dbResolveCmd(conn *string) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <ambiente> <migração>",
		Short: "Marca uma migração como aplicada depois de correção manual (ex.: migrations/2026..._x.sql)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if *conn == "" {
				return configError{fmt.Errorf("informe --conn")}
			}
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			if err := confirm(a.env, "marcar "+args[1]+" como aplicada"); err != nil {
				return err
			}
			return a.runner([]string{*conn}, db.Options{}, "").MarkApplied(cmd.Context(), *conn, args[1])
		},
	}
}

func renderStmts(conn string, stmts []stmtOut) {
	fmt.Printf("\n%s: %d instrução(ões)\n", conn, len(stmts))
	noColor := os.Getenv("NO_COLOR") != "" || !isTTY(os.Stdout)
	for i, s := range stmts {
		tag := strings.ToUpper(s.Risk.Level)
		if !noColor {
			tag = map[string]string{db.Safe: "\x1b[32m", db.Warning: "\x1b[33m", db.Destructive: "\x1b[31m"}[s.Risk.Level] + tag + "\x1b[0m"
		}
		fmt.Printf("  %2d [%s] %s\n", i+1, tag, s.SQL)
		for _, r := range s.Risk.Reasons {
			fmt.Printf("       ↳ %s\n", r)
		}
	}
}

func syncCmd() *cobra.Command {
	sync := &cobra.Command{Use: "sync", Short: "Sincroniza dados de sistema"}
	var applyNow, allowBrokenMenu bool
	progs := &cobra.Command{
		Use:   "programs <ambiente>",
		Short: "Registra telas novas em system_program e nos grupos, e valida os menus XML",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			rep, stmts, database, err := programsReport(cmd, a)
			if err != nil {
				return err
			}
			pc := a.cfg.Programs
			file := ""
			if len(stmts) > 0 && len(rep.MissingGroups) == 0 {
				dir := filepath.Join(a.root, "database", pc.Connection, "data")
				if file = sameMigration(dir, stmts); file == "" {
					if file, err = db.SaveMigration(dir, "sync_programs", time.Now(), stmts); err != nil {
						return err
					}
				}
			}
			if g.json {
				if err := printJSON(map[string]any{"report": rep, "file": file, "statements": stmts, "database": database}); err != nil {
					return err
				}
			} else {
				renderReport(rep, database, file)
			}
			if len(rep.MissingGroups) > 0 {
				return fmt.Errorf("grupos inexistentes em system_group: %s", strings.Join(rep.MissingGroups, ", "))
			}
			if len(rep.MenuWithoutClass) > 0 && !allowBrokenMenu {
				return fmt.Errorf("o menu aponta para %d classe(s) inexistente(s); corrija o XML ou use --allow-broken-menu", len(rep.MenuWithoutClass))
			}
			if !applyNow || file == "" {
				return nil
			}
			if err := confirm(a.env, "o registro dos programas"); err != nil {
				return err
			}
			id := deploy.NewID(time.Now())
			return withLock(cmd, a, func() error {
				return a.runner([]string{pc.Connection}, db.Options{}, id).Apply(cmd.Context(), a.d.MD("db-backups", id), a.d.Emit)
			})
		},
	}
	progs.Flags().BoolVar(&applyNow, "apply", false, "aplica agora (senão a migração gerada vai no próximo deploy)")
	progs.Flags().BoolVar(&allowBrokenMenu, "allow-broken-menu", false, "não falha quando o menu aponta para classe inexistente")
	sync.AddCommand(progs)
	return sync
}

// sameMigration returns an existing sync_programs file with exactly these
// statements (generated before and not deployed yet), so reruns don't pile up files.
func sameMigration(dir string, stmts []string) string {
	files, _ := filepath.Glob(filepath.Join(dir, "*_sync_programs.sql"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		got := db.SplitSQL(string(raw))
		if len(got) != len(stmts) {
			continue
		}
		equal := true
		for i := range got {
			equal = equal && db.StripComments(got[i]) == strings.TrimSpace(stmts[i])
		}
		if equal {
			return f
		}
	}
	return ""
}

func programsReport(cmd *cobra.Command, a *app) (programs.Report, []string, string, error) {
	pc := a.cfg.Programs
	if _, ok := a.cfg.Databases[pc.Connection]; !ok {
		return programs.Report{}, nil, "", configError{fmt.Errorf("programs.connection %q não está em databases", pc.Connection)}
	}
	classes, err := programs.Scan(a.root, pc.ControlDir, pc.Exclude)
	if err != nil {
		return programs.Report{}, nil, "", err
	}
	menu, err := programs.MenuActions(a.root)
	if err != nil {
		return programs.Report{}, nil, "", err
	}
	s, err := open(cmd, a, pc.Connection)
	if err != nil {
		return programs.Report{}, nil, "", err
	}
	defer s.Close()
	st, err := programs.LoadDB(cmd.Context(), s.DB, s.Conn.DB)
	if err != nil {
		return programs.Report{}, nil, "", err
	}
	rep := programs.Compare(classes, menu, st, pc.Groups)
	stmts, err := programs.SQL(rep.New, pc.Names, pc.Groups, rep.AutoIncrement)
	return rep, stmts, s.Conn.String(), err
}

func renderReport(r programs.Report, database, file string) {
	idMode := "MAX(id)+1"
	if r.AutoIncrement {
		idMode = "AUTO_INCREMENT"
	}
	fmt.Printf("banco de permissões: %s (id por %s)\n", database, idMode)
	fmt.Printf("\nTelas novas sem system_program: %d\n", len(r.New))
	for _, c := range r.New {
		fmt.Printf("  + %s  (%s)\n", c.Name, c.File)
	}
	list := func(title string, items []string) {
		if len(items) > 0 {
			fmt.Printf("\n%s: %d\n  %s\n", title, len(items), strings.Join(items, "\n  "))
		}
	}
	var noClass, noProg []string
	for _, m := range r.MenuWithoutClass {
		noClass = append(noClass, m.Controller+" ("+m.File+")")
	}
	for _, m := range r.MenuWithoutProgram {
		noProg = append(noProg, m.Controller+" ("+m.File+")")
	}
	list("ERRO: menu aponta para classe inexistente", noClass)
	list("Item de menu invisível (sem system_program; esta sincronização resolve se a classe for nova)", noProg)
	list("Aviso: telas novas fora de qualquer menu (acessíveis só por URL)", r.NotInMenu)
	list("Programas sem classe no código (só informativo; nada é apagado)", r.Orphans)
	if file != "" {
		fmt.Printf("\nmigração de dados gerada: %s\n", file)
	}
}
