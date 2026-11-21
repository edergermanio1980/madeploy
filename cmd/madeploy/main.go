// Command madeploy deploys MadBuilder/Adianti applications over SSH.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/db"
	"github.com/vitalsoft/madeploy/internal/deploy"
	"github.com/vitalsoft/madeploy/internal/remote"
)

// Exit codes (documented in README).
const (
	exitOK      = 0
	exitError   = 1
	exitBlocked = 2 // drift, destructive or large-table migration needs a decision
	exitAborted = 3
	exitConfig  = 4
	exitLocked  = 5
)

var version = "dev"

type globals struct {
	configPath string
	json       bool
	yes        bool
}

var g globals

// configError marks failures loading madeploy.yaml.
type configError struct{ error }

func (e configError) Unwrap() error { return e.error }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(exitCode(newRoot().ExecuteContext(ctx)))
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "madeploy",
		Short:         "Deploy de aplicações MadBuilder/Adianti por SSH, com backup, drift e migrações",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&g.configPath, "config", "c", config.DefaultFile, "arquivo de configuração")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "saída JSON em stdout (eventos em stderr)")
	root.PersistentFlags().BoolVarP(&g.yes, "yes", "y", false, "não perguntar (CI); obrigatório sem terminal")
	root.AddCommand(initCmd(), statusCmd(), diffCmd(), deployCmd(), rollbackCmd(), releasesCmd(),
		unlockCmd(), logsCmd(), driftCmd(), doctorCmd(), dbCmd(), syncCmd(), tuiCmd())
	return root
}

func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	fmt.Fprintln(os.Stderr, "erro:", deploy.Redact(err.Error()))
	var de *deploy.DriftError
	var ce configError
	switch {
	case errors.As(err, &de), strings.Contains(err.Error(), "migração bloqueada"):
		return exitBlocked
	case errors.Is(err, deploy.ErrAborted):
		return exitAborted
	case errors.As(err, &ce):
		return exitConfig
	case strings.Contains(err.Error(), "deploy em andamento"):
		return exitLocked
	}
	return exitError
}

// app is everything a command needs for one environment.
type app struct {
	cfg  *config.Config
	root string
	env  string
	host remote.Host
	d    *deploy.Deployer
}

func loadConfig() (*config.Config, string, error) {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return nil, "", configError{err}
	}
	root, err := filepath.Abs(filepath.Dir(g.configPath))
	return cfg, root, err
}

// connect loads config, dials the environment and builds the deployer.
func connect(envName string) (*app, error) {
	cfg, root, err := loadConfig()
	if err != nil {
		return nil, err
	}
	env, err := cfg.Env(envName)
	if err != nil {
		return nil, configError{err}
	}
	var h remote.Host
	if env.SSH == "local" { // test/demo target on this machine
		h = remote.Local{}
	} else if h, err = remote.Dial(env.SSH); err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, root: root, env: envName, host: h}
	a.d = &deploy.Deployer{Host: h, Cfg: cfg, EnvName: envName, Env: env, Root: root,
		Operator: deploy.Operator(), Emit: printer()}
	return a, nil
}

func (a *app) close() { a.host.Close() }

// runner builds the migration runner for this environment.
func (a *app) runner(conns []string, opts db.Options, release string) *db.Runner {
	return &db.Runner{Host: a.host, Cfg: a.cfg, AppPath: a.d.Env.Path, Root: a.root, Conns: conns,
		Operator: a.d.Operator, Release: release, Opts: opts}
}

// printer renders events for humans on stderr, or as JSON lines with --json.
func printer() deploy.Emitter {
	noColor := os.Getenv("NO_COLOR") != "" || !isTTY(os.Stderr)
	return func(e deploy.Event) {
		if g.json {
			os.Stderr.Write(e.JSON())
			return
		}
		mark := map[string]string{"info": "·", "warn": "!", "error": "✗", "done": "✓"}[e.Level]
		if color := map[string]string{"warn": "33", "error": "31", "done": "32"}[e.Level]; color != "" && !noColor {
			mark = "\x1b[" + color + "m" + mark + "\x1b[0m"
		}
		dur := ""
		if e.Duration != "" {
			dur = " (" + e.Duration + ")"
		}
		fmt.Fprintf(os.Stderr, "%s %-13s %s%s\n", mark, e.Step, e.Msg, dur)
	}
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// confirm asks the operator to type the environment name. Without a terminal it
// requires --yes, so CI never hangs on a prompt.
func confirm(env, what string) error {
	if g.yes {
		return nil
	}
	if !isTTY(os.Stdin) {
		return fmt.Errorf("sem terminal para confirmar %s: use --yes", what)
	}
	fmt.Fprintf(os.Stderr, "\nPara confirmar %s, digite o nome do ambiente (%s): ", what, env)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != env {
		return deploy.ErrAborted
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
