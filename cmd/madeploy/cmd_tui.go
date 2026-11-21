package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/db"
	"github.com/vitalsoft/madeploy/internal/deploy"
	"github.com/vitalsoft/madeploy/internal/tui"
)

func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui <ambiente>",
		Short: "Interface interativa: painel, arquivos, banco, permissões, execução e releases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isTTY(os.Stdin) || !isTTY(os.Stdout) {
				return errors.New("a TUI precisa de um terminal; em CI use os comandos com --yes/--json")
			}
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			return tui.Run(cmd.Context(), a.env, tuiActions(a))
		},
	}
}

// tuiActions wires the TUI to exactly the functions the CLI commands use.
func tuiActions(a *app) tui.Actions {
	if len(a.cfg.Databases) > 0 {
		a.d.Migrator = a.runner(nil, db.Options{}, "")
	}
	return tui.Actions{
		Load: func(ctx context.Context) (tui.Snapshot, error) { return loadSnapshot(ctx, a) },
		FileDiff: func(ctx context.Context, p string) (string, error) {
			if err := config.RelPath(p); err != nil {
				return "", err
			}
			local, _ := os.ReadFile(filepath.Join(a.root, filepath.FromSlash(p)))
			remote, err := a.host.ReadFile(a.d.Env.Path + "/" + currentPrefix(a) + p)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			return strings.Join(tui.LineDiff(string(remote), string(local)), "\n"), nil
		},
		Deploy: func(ctx context.Context, emit deploy.Emitter, shown *deploy.Plan) error {
			id := deploy.NewID(time.Now())
			a.d.ID = id
			if len(a.cfg.Databases) > 0 {
				a.d.Migrator = a.runner(nil, db.Options{}, id)
			}
			local, closeLog := localLog(a.root, id, emit)
			defer closeLog()
			a.d.Emit = deploy.AppendLog(ctx, a.host, a.d.MD(), id, local)
			_, err := a.d.Run(ctx, deploy.Options{Confirm: func(p *deploy.Plan) bool {
				if !samePlan(p, shown) {
					emit(deploy.Event{Time: time.Now().UTC(), Step: "plan", Level: "error", Msg: "o plano mudou desde a tela (r recarrega); nada foi feito"})
					return false
				}
				return true
			}})
			return err
		},
		Rollback: func(ctx context.Context, id string, emit deploy.Emitter) error {
			rid := deploy.NewID(time.Now())
			local, closeLog := localLog(a.root, rid, emit)
			defer closeLog()
			a.d.Emit = deploy.AppendLog(ctx, a.host, a.d.MD(), rid, local)
			return a.d.Rollback(ctx, id)
		},
	}
}

// currentPrefix is where live files are, relative to the environment path.
func currentPrefix(a *app) string {
	if a.d.Env.Strategy == "releases" {
		return "current/"
	}
	return ""
}

// samePlan guards the TUI confirmation: what runs must be what was shown.
func samePlan(a, b *deploy.Plan) bool {
	return a.Commit == b.Commit && len(a.Files.Upload) == len(b.Files.Upload) && len(a.Files.Delete) == len(b.Files.Delete) &&
		len(a.Files.Drift) == len(b.Files.Drift) && len(a.Migrations) == len(b.Migrations)
}

func loadSnapshot(ctx context.Context, a *app) (tui.Snapshot, error) {
	s := tui.Snapshot{Env: a.env, SSH: a.d.Env.SSH, Path: a.d.Env.Path, Strategy: a.d.Env.Strategy}
	var err error
	if s.Plan, err = a.d.Inspect(ctx); err != nil {
		return s, err
	}
	if s.State, err = a.d.ReadState(); err != nil {
		return s, err
	}
	_, lockErr := a.host.ReadFile(a.d.MD("deploy.lock", "info.json"))
	s.Locked = lockErr == nil
	if s.Releases, err = a.d.Backups(ctx); err != nil {
		return s, err
	}
	for _, n := range a.cfg.DatabaseNames() {
		dir := filepath.Join(a.root, "database", n, "schema")
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		cd := tui.ConnDiff{Conn: n}
		r, err := diffOne(ctx, a, n, dir, "", false, false) // never writes files
		if err != nil {
			cd.Err = fmt.Sprintf("diff indisponível: %v", err)
		}
		for _, st := range r.Stmts {
			cd.Stmts = append(cd.Stmts, tui.Stmt{SQL: st.SQL, Level: st.Risk.Level, Reasons: st.Risk.Reasons})
		}
		cd.Skipped = r.Skipped
		s.DB = append(s.DB, cd)
	}
	return s, nil
}
