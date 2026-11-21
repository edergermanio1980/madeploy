package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path"
	"strings"
	"time"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/files"
	"github.com/vitalsoft/madeploy/internal/perms"
	"github.com/vitalsoft/madeploy/internal/remote"
)

// State is .madeploy/state.json on the server.
type State struct {
	Commit     string    `json:"commit"`
	DeployedAt time.Time `json:"deployed_at"`
	Operator   string    `json:"operator"`
	Backup     string    `json:"backup,omitempty"`
	Release    string    `json:"release,omitempty"`  // releases strategy: id behind current
	Baseline   bool      `json:"baseline,omitempty"` // set by init: manifest taken from the live server
}

// Migrator is implemented by the db package; nil when no database is configured.
type Migrator interface {
	Pending(ctx context.Context) ([]string, error)
	Apply(ctx context.Context, backupDir string, emit Emitter) error
}

// Deployer holds everything a deploy needs. Fields are set by the CLI.
type Deployer struct {
	Host     remote.Host
	Cfg      *config.Config
	EnvName  string
	Env      config.Environment
	Root     string // local repository
	Operator string
	Emit     Emitter
	Migrator Migrator
	Now      func() time.Time
	Health   func(ctx context.Context, hc config.Healthcheck) error
	ID       string               // deploy id (backup and log name); NewID(now) when empty
	Progress func(files.Progress) // optional: per-file progress of Pull
}

// NewID formats a deploy id: AAAAMMDD-HHMMSS.mmm in UTC (sortable, unique per deploy).
func NewID(t time.Time) string { return t.UTC().Format("20060102-150405.000") }

// base is the live application root: Path itself (inplace) or Path/current (releases).
func (d *Deployer) base() string {
	if d.releases() {
		return d.Env.Path + "/current"
	}
	return d.Env.Path
}

func (d *Deployer) releases() bool { return d.Env.Strategy == "releases" }

// MD returns a path inside the server state directory.
func (d *Deployer) MD(p ...string) string {
	return path.Join(append([]string{d.Env.Path, ".madeploy"}, p...)...)
}

func (d *Deployer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Deployer) emit(step, level, msg string) {
	if d.Emit != nil {
		d.Emit(Event{Time: d.now().UTC(), Step: step, Level: level, Msg: Redact(msg)})
	}
}

// Operator returns "user@host" for the local user.
func Operator() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return name + "@" + host
}

// Plan is everything a deploy would do. It is shown before anything changes.
type Plan struct {
	Environment    string         `json:"environment"`
	Path           string         `json:"path"`
	Commit         string         `json:"commit"`
	Branch         string         `json:"branch"`
	DeployedCommit string         `json:"deployed_commit"`
	Baseline       bool           `json:"baseline"`
	Git            []files.Change `json:"git"`
	Files          files.Delta    `json:"files"`
	Migrations     []string       `json:"migrations"`
	Permissions    []perms.Op     `json:"permissions"`
	BeforeSwitch   []string       `json:"before_switch"`
	AfterSwitch    []string       `json:"after_switch"`
	Healthcheck    string         `json:"healthcheck,omitempty"`
	Warnings       []string       `json:"warnings"`

	local    files.Manifest
	deployed files.Manifest
	live     files.Manifest
}

// NoOp reports a plan with nothing to do.
func (p *Plan) NoOp() bool { return p.Files.Empty() && len(p.Migrations) == 0 }

// readJSON reads a remote JSON file; a missing file leaves v untouched and returns false.
func readJSON(h remote.Host, p string, v any) (bool, error) {
	raw, err := h.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lendo %s: %w", p, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("%s corrompido: %w", p, err)
	}
	return true, nil
}

func writeJSON(h remote.Host, p string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return h.WriteFile(p, append(raw, '\n'), 0o640)
}

// ReadState returns the server state (zero value if never deployed).
func (d *Deployer) ReadState() (State, error) {
	var st State
	_, err := readJSON(d.Host, d.MD("state.json"), &st)
	return st, err
}

// Matcher builds the exclude matcher for this repository.
func (d *Deployer) Matcher() (*files.Matcher, error) {
	var shared []string
	for _, s := range d.Env.Shared { // shared paths are server data, never shipped
		shared = append(shared, "/"+strings.Trim(s, "/"))
	}
	return files.NewMatcher(d.Root, config.DefaultExclude, d.Cfg.Exclude, shared)
}

// LiveManifest hashes the files currently on the server.
func (d *Deployer) LiveManifest(ctx context.Context, m *files.Matcher) (files.Manifest, error) {
	out, err := remote.Run(ctx, d.Host, files.RemoteHashCmd(d.base(), m, remote.Quote))
	if err != nil {
		return nil, fmt.Errorf("calculando hashes no servidor: %w", err)
	}
	return files.ParseHashes(out, m)
}

// Inspect computes the plan. It only reads.
func (d *Deployer) Inspect(ctx context.Context) (*Plan, error) {
	g := files.Git{Dir: d.Root}
	p := &Plan{Environment: d.EnvName, Path: d.base(), Healthcheck: d.Env.Healthcheck.URL,
		BeforeSwitch: d.Env.Hooks.BeforeSwitch, AfterSwitch: d.Env.Hooks.AfterSwitch}
	var err error
	if p.Commit, err = g.Head(); err != nil {
		return nil, err
	}
	if p.Branch, err = g.Branch(); err != nil {
		return nil, err
	}
	m, err := d.Matcher()
	if err != nil {
		return nil, err
	}
	tracked, err := g.Tracked()
	if err != nil {
		return nil, err
	}
	if p.local, err = files.BuildLocal(d.Root, tracked, d.Cfg.Extra, m); err != nil {
		return nil, err
	}
	st, err := d.ReadState()
	if err != nil {
		return nil, err
	}
	p.DeployedCommit, p.Baseline = st.Commit, st.Baseline
	var deployed files.Manifest
	found, err := readJSON(d.Host, d.MD("manifest.json"), &deployed)
	if err != nil {
		return nil, err
	}
	if found {
		p.deployed = deployed
	} else {
		p.Warnings = append(p.Warnings, "servidor sem manifesto: rode `madeploy init "+d.EnvName+"` antes do primeiro deploy (sem ele não há detecção de drift nem remoções)")
	}
	if p.live, err = d.LiveManifest(ctx, m); err != nil {
		return nil, err
	}
	switch {
	case st.Commit != "" && g.Exists(st.Commit):
		if p.Git, err = g.Diff(st.Commit, p.Commit); err != nil {
			return nil, err
		}
	case st.Commit != "":
		p.Warnings = append(p.Warnings, "commit implantado "+st.Commit+" não existe neste repositório; usando só o manifesto")
	}
	p.Files = files.Compute(p.local, p.deployed, p.live, p.Git)
	changed := map[string]bool{}
	for _, f := range p.Files.Upload {
		changed[f.Path] = p.local[f.Path].Mode == "0755"
	}
	p.Permissions = perms.Ops(d.Cfg.Permissions, d.base(), changed, d.Env.Sudo)
	if d.Migrator != nil {
		if p.Migrations, err = d.Migrator.Pending(ctx); err != nil {
			return nil, fmt.Errorf("migrações pendentes: %w", err)
		}
	}
	if d.Env.Healthcheck.URL == "" {
		p.Warnings = append(p.Warnings, "sem healthcheck: falhas da aplicação após o deploy não serão detectadas")
	}
	return p, nil
}
