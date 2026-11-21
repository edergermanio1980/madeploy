// Package config loads and validates madeploy.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultFile is the config file name looked up in the repository root.
const DefaultFile = "madeploy.yaml"

// DefaultExclude are paths never packaged, hashed for drift or deleted.
// They are runtime data or production-only configuration (docs/discovery.md §3).
var DefaultExclude = []string{
	".git/", ".madeploy/", ".env", ".env.*", "node_modules/", "/docs/", "/tests/",
	".idea/", ".vscode/", ".DS_Store", "*.swp",
	"/app/config/*", "!/app/config/framework_hashes.php",
	"/tmp/", "/files/", "/app/output/", "/app/images/photos/", "/app/database/.cache.db",
	"/madeploy.yaml", "/.madeployignore", "/database/", "/.gitignore", "/.gitattributes",
}

// SharedDirs survive between releases (runtime data, docs/discovery.md §3).
// init --migrate-layout also shares every app/config file except framework_hashes.php.
var SharedDirs = []string{"tmp", "files", "app/output", "app/images/photos"}

type Config struct {
	Project      string                 `yaml:"project"`
	Exclude      []string               `yaml:"exclude"`
	Extra        []string               `yaml:"extra"` // untracked dirs shipped anyway (e.g. vendor built from composer.lock)
	Lint         Lint                   `yaml:"lint"`
	Permissions  Permissions            `yaml:"permissions"`
	Databases    map[string]Database    `yaml:"databases"`
	Programs     Programs               `yaml:"programs"`
	Environments map[string]Environment `yaml:"environments"`
}

type Lint struct {
	Local string `yaml:"local"` // local php binary; empty = "php" if found, skipped otherwise
}

type Environment struct {
	SSH         string      `yaml:"ssh"`  // ~/.ssh/config alias or user@host[:port]
	Path        string      `yaml:"path"` // absolute app path on the server
	Branch      string      `yaml:"branch"`
	Strategy    string      `yaml:"strategy"` // inplace | releases
	Shared      []string    `yaml:"shared"`   // releases: extra paths kept in shared/ (SharedDirs are always shared)
	PHP         string      `yaml:"php"`      // remote php binary for lint
	Keep        int         `yaml:"keep"`
	Sudo        bool        `yaml:"sudo"` // prefix privileged commands with "sudo -n"
	Hooks       Hooks       `yaml:"hooks"`
	Healthcheck Healthcheck `yaml:"healthcheck"`
	MigrateWhen string      `yaml:"migrate"` // before|after (file switch)
}

type Hooks struct {
	BeforeSwitch []string `yaml:"before_switch"`
	AfterSwitch  []string `yaml:"after_switch"`
}

type Healthcheck struct {
	URL          string        `yaml:"url"`
	ExpectStatus int           `yaml:"expect_status"`
	ExpectBody   string        `yaml:"expect_body"`
	Timeout      time.Duration `yaml:"timeout"`
}

type Permissions struct {
	Owner      string            `yaml:"owner"`
	Group      string            `yaml:"group"`
	Default    Modes             `yaml:"default"`
	Writable   []string          `yaml:"writable"`
	Restricted map[string]string `yaml:"restricted"`
	Executable []string          `yaml:"executable"`
}

type Modes struct {
	Dir  string `yaml:"dir"`
	File string `yaml:"file"`
}

type Database struct {
	Config         string   `yaml:"config"` // app/config/<conn>.php on the server
	Backup         string   `yaml:"backup"` // tables|full|none
	LargeTableRows int64    `yaml:"large_table_rows"`
	DumpOptions    []string `yaml:"dump_options"` // extra mysqldump flags, e.g. --skip-ssl-verify-server-cert
}

type Programs struct {
	Connection string            `yaml:"connection"`
	Groups     []string          `yaml:"groups"`
	Names      map[string]string `yaml:"names"`
	Exclude    []string          `yaml:"exclude"`
	ControlDir string            `yaml:"control_dir"`
}

var (
	modeRe = regexp.MustCompile(`^0[0-7]{3}$`)
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	userRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]*$`)
)

// Load reads, defaults and validates the file. Unknown keys are errors.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lendo %s: %w", path, err)
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("madeploy.yaml inválido: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Permissions.Default.Dir == "" {
		c.Permissions.Default.Dir = "0755"
	}
	if c.Permissions.Default.File == "" {
		c.Permissions.Default.File = "0644"
	}
	if c.Programs.Connection == "" {
		c.Programs.Connection = "permission"
	}
	if len(c.Programs.Groups) == 0 {
		c.Programs.Groups = []string{"Admin"}
	}
	if c.Programs.ControlDir == "" {
		c.Programs.ControlDir = "app/control"
	}
	for name, db := range c.Databases {
		if db.Config == "" {
			db.Config = "app/config/" + name + ".php"
		}
		if db.Backup == "" {
			db.Backup = "tables"
		}
		if db.LargeTableRows == 0 {
			db.LargeTableRows = 100000
		}
		c.Databases[name] = db
	}
	for name, e := range c.Environments {
		if e.Strategy == "" {
			e.Strategy = "inplace"
		}
		if e.PHP == "" {
			e.PHP = "php"
		}
		if e.Keep == 0 {
			e.Keep = 5
		}
		if e.MigrateWhen == "" {
			e.MigrateWhen = "before"
		}
		if e.Healthcheck.ExpectStatus == 0 {
			e.Healthcheck.ExpectStatus = 200
		}
		if e.Healthcheck.Timeout == 0 {
			e.Healthcheck.Timeout = 20 * time.Second
		}
		c.Environments[name] = e
	}
}

// Validate returns every problem found, not just the first.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Environments) == 0 {
		add("environments: defina ao menos um ambiente")
	}
	for name, e := range c.Environments {
		if !nameRe.MatchString(name) {
			add("environments.%s: nome inválido (use [a-z0-9_-])", name)
		}
		if e.SSH == "" {
			add("environments.%s.ssh: obrigatório", name)
		}
		if !filepath.IsAbs(e.Path) || filepath.Clean(e.Path) == "/" {
			add("environments.%s.path: precisa ser absoluto e diferente de /", name)
		}
		if e.Strategy != "inplace" && e.Strategy != "releases" {
			add("environments.%s.strategy: use inplace ou releases", name)
		}
		for _, s := range e.Shared {
			if err := RelPath(s); err != nil {
				add("environments.%s.shared: %v", name, err)
			}
		}
		if e.MigrateWhen != "before" && e.MigrateWhen != "after" {
			add("environments.%s.migrate: use before ou after", name)
		}
		if e.Keep < 1 {
			add("environments.%s.keep: mínimo 1", name)
		}
		if e.Healthcheck.URL != "" && !strings.HasPrefix(e.Healthcheck.URL, "http") {
			add("environments.%s.healthcheck.url: precisa começar com http(s)://", name)
		}
	}
	p := c.Permissions
	for _, m := range []string{p.Default.Dir, p.Default.File} {
		if !modeRe.MatchString(m) {
			add("permissions.default: modo %q inválido (ex.: 0644)", m)
		}
	}
	for glob, m := range p.Restricted {
		if !modeRe.MatchString(m) {
			add("permissions.restricted[%s]: modo %q inválido", glob, m)
		}
	}
	for _, u := range []string{p.Owner, p.Group} {
		if u != "" && !userRe.MatchString(u) {
			add("permissions: usuário/grupo %q inválido", u)
		}
	}
	for _, w := range p.Writable {
		if err := RelPath(w); err != nil {
			add("permissions.writable: %v", err)
		}
	}
	for _, x := range c.Extra {
		if err := RelPath(x); err != nil {
			add("extra: %v", err)
		}
	}
	for name, db := range c.Databases {
		if !nameRe.MatchString(name) {
			add("databases.%s: nome inválido", name)
		}
		if err := RelPath(db.Config); err != nil {
			add("databases.%s.config: %v", name, err)
		}
		if db.Backup != "tables" && db.Backup != "full" && db.Backup != "none" {
			add("databases.%s.backup: use tables, full ou none", name)
		}
		for _, o := range db.DumpOptions {
			if !strings.HasPrefix(o, "--") || strings.HasPrefix(o, "--defaults") {
				add("databases.%s.dump_options: %q precisa ser uma flag --opcao (e não --defaults*)", name, o)
			}
		}
	}
	if len(c.Databases) > 0 {
		if _, ok := c.Databases[c.Programs.Connection]; !ok {
			add("programs.connection: %q não está em databases", c.Programs.Connection)
		}
	}
	return errors.Join(errs...)
}

// RelPath rejects absolute paths and parent traversal.
func RelPath(p string) error {
	if p == "" || filepath.IsAbs(p) {
		return fmt.Errorf("caminho %q precisa ser relativo", p)
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return fmt.Errorf("caminho %q não pode conter ..", p)
		}
	}
	return nil
}

// Env returns the environment by name with a helpful error.
func (c *Config) Env(name string) (Environment, error) {
	e, ok := c.Environments[name]
	if !ok {
		names := make([]string, 0, len(c.Environments))
		for n := range c.Environments {
			names = append(names, n)
		}
		sort.Strings(names)
		return e, fmt.Errorf("ambiente %q não existe (disponíveis: %s)", name, strings.Join(names, ", "))
	}
	return e, nil
}

// DatabaseNames returns configured connection names in stable order.
func (c *Config) DatabaseNames() []string {
	names := make([]string, 0, len(c.Databases))
	for n := range c.Databases {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
