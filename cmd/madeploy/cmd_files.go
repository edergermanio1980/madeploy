package main

import (
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
	"github.com/vitalsoft/madeploy/internal/remote"
)

func initCmd() *cobra.Command {
	var pullTo, phpService string
	var force, migrate, finish bool
	c := &cobra.Command{
		Use:   "init [ambiente]",
		Short: "Sem ambiente: cria madeploy.yaml. Com ambiente: prepara o servidor e grava o baseline",
		Long: `Sem argumentos, cria um madeploy.yaml de exemplo.

Com um ambiente: testa SSH e host key, verifica as ferramentas do servidor,
cria .madeploy/, sugere o trecho de sudoers e grava o baseline (o servidor
atual vira "o que está implantado", para detectar drift no primeiro deploy).

--pull-to DIR baixa a árvore de produção (sem config, uploads e dados de runtime)
para semear o repositório Git a partir de produção (ADR-05).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return writeTemplate(g.configPath)
			}
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			ctx := cmd.Context()
			probe := "for t in sh tar sha256sum find gzip; do command -v $t >/dev/null || echo \"FALTA:$t\"; done; " +
				"command -v setfacl >/dev/null && echo acl || echo sem-acl; " +
				"command -v mysqldump >/dev/null && echo mysqldump || echo sem-mysqldump; " +
				remote.Cmd(a.d.Env.PHP, "-r", "echo 'php-'.PHP_VERSION;") + " 2>/dev/null; true"
			out, err := remote.Run(ctx, a.host, probe)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "servidor %s: %s\n", a.d.Env.SSH, strings.Join(strings.Fields(string(out)), " "))
			if strings.Contains(string(out), "FALTA:") {
				return errors.New("ferramentas obrigatórias ausentes no servidor")
			}
			if migrate {
				what := "a conversão para o layout de releases"
				if finish {
					what = "a sincronização final de shared/"
				}
				if err := confirm(a.env, what); err != nil {
					return err
				}
				msg, err := a.d.MigrateLayout(ctx, finish)
				if err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, msg)
				return nil
			}
			if pullTo != "" {
				a.d.Progress = progressLine()
				n, err := a.d.Pull(ctx, pullTo, nil)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "%d arquivo(s) baixados para %s; crie o commit inicial a partir deles\n", n, pullTo)
			}
			n, err := a.d.Baseline(ctx, force)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "baseline gravado: %d arquivo(s) em %s\n\nsudoers sugerido (só se os hooks usarem sudo):\n%s",
				n, a.d.MD("manifest.json"), deploy.SudoersSnippet(orDash(a.cfg.Permissions.Owner), phpService))
			return nil
		},
	}
	c.Flags().StringVar(&pullTo, "pull-to", "", "baixa a árvore de produção para este diretório")
	c.Flags().StringVar(&phpService, "php-service", "php8.2-fpm", "serviço do PHP-FPM para o sudoers sugerido")
	c.Flags().BoolVar(&force, "force", false, "refaz o baseline mesmo se já existir")
	c.Flags().BoolVar(&migrate, "migrate-layout", false, "converte a instalação in-place para releases/ + shared/ + current (sem downtime; o DocumentRoot é com você)")
	c.Flags().BoolVar(&finish, "finish", false, "com --migrate-layout: copia para shared/ os uploads gravados depois da conversão")
	return c
}

const template = `# madeploy.yaml — referência completa em docs/config.md
project: sisvita

# Diretórios não versionados que vão no pacote (vendor montado de composer.lock, ADR-06).
extra: [vendor]

# Somados ao padrão (config de produção, tmp, files, app/output, fotos, .env, docs...).
exclude: []

permissions:
  owner: deploy
  group: www-data
  default: { dir: "0755", file: "0644" }
  writable: [tmp, app/output, files, app/images/photos]
  restricted: { "app/config/*": "0640" }
  executable: []

databases:
  sisvita:    { config: app/config/sisvita.php }
  permission: { config: app/config/permission.php }
  log:        { config: app/config/log.php, backup: none }

programs:
  connection: permission
  groups: [Admin]

environments:
  production:
    ssh: sisvita-prod            # alias do ~/.ssh/config (HostName, User, ProxyJump...)
    path: /var/www               # Apache (Q3)
    branch: main
    php: php8.2
    keep: 5
    migrate: before
    hooks:
      after_switch:
        # limpa o OPcache. PHP-FPM: systemctl reload php8.2-fpm; mod_php: apache2ctl graceful
        - sudo -n systemctl reload php8.2-fpm
    healthcheck:
      url: https://sistema.buonavitahomecare.com.br/index.php?class=LoginForm
      expect_status: 200
      expect_body: LoginForm
      timeout: 20s
`

func writeTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s já existe", path)
	}
	if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s criado; ajuste ssh, path e healthcheck e rode `madeploy init production`\n", path)
	return nil
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <ambiente>",
		Short: "Commit implantado × HEAD, pendências de arquivos e banco, drift e lock",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			if len(a.cfg.Databases) > 0 {
				a.d.Migrator = a.runner(nil, db.Options{}, "")
			}
			p, err := a.d.Inspect(cmd.Context())
			if err != nil {
				return err
			}
			st, err := a.d.ReadState()
			if err != nil {
				return err
			}
			_, lockErr := a.host.ReadFile(a.d.MD("deploy.lock", "info.json"))
			locked := lockErr == nil
			if g.json {
				return printJSON(map[string]any{"state": st, "plan": p, "locked": locked})
			}
			fmt.Printf("ambiente   %s (%s:%s)\n", a.env, a.d.Env.SSH, a.d.Env.Path)
			fmt.Printf("implantado %s em %s por %s\n", orDash(st.Commit), st.DeployedAt.Local().Format(time.DateTime), orDash(st.Operator))
			fmt.Printf("local      %s (%s)\n", p.Commit, p.Branch)
			fmt.Printf("pendente   %d arquivo(s) a enviar, %d a remover, %d migração(ões)\n", len(p.Files.Upload), len(p.Files.Delete), len(p.Migrations))
			fmt.Printf("drift      %d arquivo(s)\n", len(p.Files.Drift))
			if locked {
				fmt.Println("lock       ATIVO (deploy em andamento ou interrompido; veja `madeploy unlock`)")
			}
			for _, w := range p.Warnings {
				fmt.Println("aviso     ", w)
			}
			return nil
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <ambiente>",
		Short: "Lista arquivos A/M/D/R e drift, sem enviar nada",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			p, err := a.d.Inspect(cmd.Context())
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(p.Files)
			}
			renderFiles(os.Stdout, p)
			return nil
		},
	}
}

func deployCmd() *cobra.Command {
	var dryRun, allowDestructive, allowLarge bool
	var only []string
	var onDrift string
	c := &cobra.Command{
		Use:   "deploy <ambiente>",
		Short: "Mostra o plano, pede confirmação e implanta (arquivos, banco, permissões, hooks)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			onlySet := map[string]bool{}
			for _, o := range only {
				if o != "files" && o != "db" && o != "perms" {
					return configError{fmt.Errorf("--only aceita files, db, perms (veio %q)", o)}
				}
				onlySet[o] = true
			}
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			ctx := cmd.Context()
			id := deploy.NewID(time.Now())
			a.d.ID = id
			if len(a.cfg.Databases) > 0 && (len(onlySet) == 0 || onlySet["db"]) {
				a.d.Migrator = a.runner(nil, db.Options{AllowDestructive: allowDestructive, AllowLarge: allowLarge}, id)
				warnUnregisteredScreens(cmd, a)
			}
			if !dryRun {
				local, closeLog := localLog(a.root, id, a.d.Emit)
				defer closeLog()
				a.d.Emit = deploy.AppendLog(ctx, a.host, a.d.MD(), id, local)
			}
			plan, err := a.d.Run(ctx, deploy.Options{
				DryRun: dryRun, OnDrift: onDrift, Only: onlySet,
				Confirm: func(p *deploy.Plan) bool {
					renderPlan(os.Stderr, p)
					return confirm(a.env, "o deploy") == nil
				},
			})
			if dryRun && plan != nil {
				if g.json {
					return errors.Join(printJSON(plan), err)
				}
				renderPlan(os.Stdout, plan)
			}
			return err
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "só mostra o plano")
	c.Flags().StringSliceVar(&only, "only", nil, "files, db, perms (separados por vírgula)")
	c.Flags().StringVar(&onDrift, "on-drift", "", "overwrite ou ignore quando houver drift")
	c.Flags().BoolVar(&allowDestructive, "allow-destructive", false, "permite migrações destrutivas")
	c.Flags().BoolVar(&allowLarge, "allow-large", false, "permite ALTER em tabela acima de large_table_rows")
	return c
}

// warnUnregisteredScreens tells the operator about screens that would be
// invisible in production (no system_program row and no pending sync migration).
func warnUnregisteredScreens(cmd *cobra.Command, a *app) {
	if _, ok := a.cfg.Databases[a.cfg.Programs.Connection]; !ok {
		return
	}
	rep, stmts, _, err := programsReport(cmd, a)
	if err != nil {
		a.d.Emit(deploy.Event{Time: time.Now().UTC(), Step: "programs", Level: "warn", Msg: "não foi possível conferir system_program: " + err.Error()})
		return
	}
	dir := filepath.Join(a.root, "database", a.cfg.Programs.Connection, "data")
	if len(rep.New) > 0 && sameMigration(dir, stmts) == "" {
		var names []string
		for _, c := range rep.New {
			names = append(names, c.Name)
		}
		a.d.Emit(deploy.Event{Time: time.Now().UTC(), Step: "programs", Level: "warn",
			Msg: fmt.Sprintf("%d tela(s) sem system_program não aparecerão para ninguém: %s — rode `madeploy sync programs %s` e faça commit",
				len(names), strings.Join(names, ", "), a.env)})
	}
}

// localLog mirrors every event to .madeploy/logs/<id>.jsonl in the repository.
func localLog(root, id string, next deploy.Emitter) (deploy.Emitter, func()) {
	dir := filepath.Join(root, ".madeploy", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return next, func() {}
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return next, func() {}
	}
	return func(e deploy.Event) {
		next(e)
		_, _ = f.Write(e.JSON())
	}, func() { f.Close() }
}

func rollbackCmd() *cobra.Command {
	var to string
	c := &cobra.Command{
		Use:   "rollback <ambiente>",
		Short: "Reverte o código do último deploy (ou até --to); o banco não é alterado",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			what := "o rollback do último deploy"
			if to != "" {
				what = "o rollback até " + to
			}
			if err := confirm(a.env, what); err != nil {
				return err
			}
			id := deploy.NewID(time.Now())
			local, closeLog := localLog(a.root, id, a.d.Emit)
			defer closeLog()
			a.d.Emit = deploy.AppendLog(cmd.Context(), a.host, a.d.MD(), id, local)
			return a.d.Rollback(cmd.Context(), to)
		},
	}
	c.Flags().StringVar(&to, "to", "", "id do deploy (veja `madeploy releases`)")
	return c
}

func releasesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "releases <ambiente>",
		Short: "Lista os deploys com backup disponível para rollback",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			all, err := a.d.Backups(cmd.Context())
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(all)
			}
			fmt.Printf("  %-20s %-12s %-12s %-24s %s\n", "ID", "ANTES", "DEPOIS", "OPERADOR", "ARQUIVOS")
			for _, m := range all {
				mark := " "
				if m.Current {
					mark = "*"
				}
				fmt.Printf("%s %-20s %-12s %-12s %-24s %d alterados, %d novos\n", mark, m.ID, short(m.CommitBefore), short(m.CommitAfter), m.Operator, m.Files, m.Added)
			}
			return nil
		},
	}
}

func short(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return orDash(c)
}

func unlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <ambiente>",
		Short: "Remove o lock de um deploy interrompido",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			if raw, err := a.host.ReadFile(a.d.MD("deploy.lock", "info.json")); err == nil {
				fmt.Fprintf(os.Stderr, "lock atual: %s\n", strings.TrimSpace(string(raw)))
			}
			if err := confirm(a.env, "a remoção do lock"); err != nil {
				return err
			}
			return a.d.Unlock(cmd.Context())
		},
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var id string
	c := &cobra.Command{
		Use:   "logs <ambiente>",
		Short: "Mostra o log do último deploy (ou --id); --follow acompanha ao vivo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			dir := a.d.MD("logs")
			if id == "" {
				out, err := remote.Run(cmd.Context(), a.host, "ls -1 "+remote.Quote(dir)+" 2>/dev/null | sort | tail -n 1")
				if err != nil {
					return err
				}
				if id = strings.TrimSuffix(strings.TrimSpace(string(out)), ".jsonl"); id == "" {
					return errors.New("nenhum log no servidor")
				}
			}
			file := dir + "/" + id + ".jsonl"
			if follow {
				err := a.host.Stream(cmd.Context(), remote.Cmd("tail", "-n", "+1", "-f", file), nil, os.Stdout)
				if cmd.Context().Err() != nil {
					return nil // Ctrl-C ends the follow
				}
				return err
			}
			raw, err := a.host.ReadFile(file)
			if err != nil {
				return fmt.Errorf("log %s: %w", id, err)
			}
			_, err = os.Stdout.Write(raw)
			return err
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "acompanha o log ao vivo")
	c.Flags().StringVar(&id, "id", "", "id do deploy")
	return c
}

func driftCmd() *cobra.Command {
	drift := &cobra.Command{Use: "drift", Short: "Arquivos alterados no servidor fora do madeploy"}
	drift.AddCommand(&cobra.Command{
		Use:   "pull <ambiente> [arquivo...]",
		Short: "Traz os arquivos com drift (ou os indicados) do servidor para o repositório local",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := args[1:]
			for _, p := range paths {
				if err := config.RelPath(p); err != nil {
					return err
				}
			}
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			if len(paths) == 0 {
				p, err := a.d.Inspect(cmd.Context())
				if err != nil {
					return err
				}
				for _, d := range p.Files.Drift {
					if d.Kind != "deleted" {
						paths = append(paths, d.Path)
					}
				}
			}
			if len(paths) == 0 {
				fmt.Fprintln(os.Stderr, "sem drift para trazer")
				return nil
			}
			n, err := a.d.Pull(cmd.Context(), a.root, paths)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "%d arquivo(s) copiados para o repositório; revise com git diff e faça commit\n", n)
			return nil
		},
	})
	return drift
}
