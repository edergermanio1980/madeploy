package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vitalsoft/madeploy/internal/files"
	"github.com/vitalsoft/madeploy/internal/perms"
	"github.com/vitalsoft/madeploy/internal/remote"
)

// Options control one deploy run.
type Options struct {
	DryRun  bool
	OnDrift string          // "" (block), "overwrite", "ignore"
	Only    map[string]bool // files, db, perms; empty = all
	Confirm func(*Plan) bool
}

func (o Options) want(part string) bool { return len(o.Only) == 0 || o.Only[part] }

// ErrAborted is returned when the operator declines the plan.
var ErrAborted = errors.New("deploy cancelado pelo operador")

// DriftError blocks a deploy until the operator decides what to do.
type DriftError struct{ Drift []files.Drift }

func (e *DriftError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d arquivo(s) alterado(s) no servidor fora do madeploy:\n", len(e.Drift))
	for _, d := range e.Drift {
		fmt.Fprintf(&b, "  %-8s %s\n", d.Kind, d.Path)
	}
	b.WriteString("decida: --on-drift=overwrite (sobrescrever), --on-drift=ignore (manter a versão do servidor) ou `madeploy drift pull` (trazer para o repositório)")
	return b.String()
}

// Run executes the full pipeline. On any error after the lock, it releases the
// lock and reports what was done and how to revert.
func (d *Deployer) Run(ctx context.Context, o Options) (plan *Plan, err error) {
	g := files.Git{Dir: d.Root}
	clean, dirty, err := g.Clean()
	if err != nil {
		return nil, err
	}
	if !clean {
		return nil, fmt.Errorf("árvore Git com alterações não commitadas:\n  %s", strings.Join(dirty, "\n  "))
	}
	if branch, _ := g.Branch(); d.Env.Branch != "" && branch != d.Env.Branch {
		return nil, fmt.Errorf("branch %q não é permitida em %s (esperado %q)", branch, d.EnvName, d.Env.Branch)
	}
	if err := d.preflight(ctx); err != nil {
		return nil, err
	}
	if err := d.Lock(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if uerr := d.Unlock(ctx); uerr != nil {
			err = errors.Join(err, fmt.Errorf("liberando lock: %w", uerr))
		}
	}()

	if plan, err = d.Inspect(ctx); err != nil {
		return nil, err
	}
	if !o.want("files") {
		plan.Files = files.Delta{Drift: plan.Files.Drift}
		// nothing is uploaded: only the writable dirs can be fixed
		plan.Permissions = perms.Writable(d.Cfg.Permissions, d.base(), d.Env.Sudo)
	}
	if !o.want("db") {
		plan.Migrations = nil
	}
	if err := applyDriftPolicy(plan, o.OnDrift); err != nil {
		return plan, err
	}
	if o.DryRun {
		return plan, nil
	}
	if plan.NoOp() && !o.Only["perms"] {
		// idempotent: nothing on the server changes; only the commit pointer moves
		if plan.DeployedCommit != plan.Commit && o.want("files") {
			if err := d.saveState(plan, "", o); err != nil {
				return plan, err
			}
		}
		d.emit("plan", "done", "nada a fazer: servidor já está em "+short(plan.Commit))
		return plan, nil
	}
	if o.Confirm != nil && !o.Confirm(plan) {
		return plan, ErrAborted
	}
	id := d.ID
	if id == "" {
		id = NewID(d.now())
	}
	if d.releases() {
		return plan, d.executeReleases(ctx, plan, id, o)
	}
	return plan, d.execute(ctx, plan, id, o)
}

func applyDriftPolicy(p *Plan, policy string) error {
	if len(p.Files.Drift) == 0 {
		return nil
	}
	switch policy {
	case "overwrite":
		return nil
	case "ignore":
		skip := map[string]bool{}
		for _, dr := range p.Files.Drift {
			skip[dr.Path] = true
		}
		var up []files.FileChange
		for _, f := range p.Files.Upload {
			if !skip[f.Path] {
				up = append(up, f)
			}
		}
		var del []string
		for _, f := range p.Files.Delete {
			if !skip[f] {
				del = append(del, f)
			}
		}
		p.Files.Upload, p.Files.Delete = up, del
		return nil
	case "":
		return &DriftError{Drift: p.Files.Drift}
	default:
		return fmt.Errorf("--on-drift inválido: %q", policy)
	}
}

type step struct {
	name string
	fn   func() error
	when bool
}

func (d *Deployer) execute(parent context.Context, p *Plan, id string, o Options) error {
	// steps never see the cancel signal: an interrupted step could leave half a
	// tar extracted. Cancellation is checked between steps instead.
	ctx := context.WithoutCancel(parent)
	wantFiles := o.want("files")
	hasUpload := wantFiles && len(p.Files.Upload) > 0
	steps := []step{
		{"lint-local", func() error { return d.lintLocal(ctx, p) }, hasUpload},
		{"upload", func() error { return d.upload(ctx, p, id) }, hasUpload},
		{"lint-remote", func() error { return d.lintRemote(ctx, p, id) }, hasUpload},
		{"backup", func() error { return d.backup(ctx, p, id) }, true},
		{"before_switch", func() error { return d.hooks(ctx, "before_switch", p.BeforeSwitch, id) }, true},
		{"migrate", func() error { return d.migrate(ctx, id) }, len(p.Migrations) > 0 && d.Env.MigrateWhen == "before"},
		{"switch", func() error { return d.apply(ctx, p, id) }, wantFiles},
		{"permissions", func() error { return d.permissions(ctx, p) }, wantFiles || o.want("perms")},
		{"migrate", func() error { return d.migrate(ctx, id) }, len(p.Migrations) > 0 && d.Env.MigrateWhen == "after"},
		{"after_switch", func() error { return d.hooks(ctx, "after_switch", p.AfterSwitch, id) }, true},
		{"healthcheck", func() error { return d.healthcheck(ctx) }, d.Env.Healthcheck.URL != ""},
		{"state", func() error { return d.saveState(p, id, o) }, true},
		{"cleanup", func() error { return d.cleanup(ctx) }, true},
	}
	switched := false
	for _, s := range steps {
		if !s.when {
			continue
		}
		if err := parent.Err(); err != nil {
			d.emit(s.name, "warn", "cancelado pelo operador antes desta etapa")
			return d.fail(ctx, s.name, id, switched, err)
		}
		start := d.now()
		d.emit(s.name, "info", "início")
		if err := s.fn(); err != nil {
			d.emit(s.name, "error", err.Error())
			return d.fail(ctx, s.name, id, switched, err)
		}
		if s.name == "switch" {
			switched = true
		}
		d.emitDone(s.name, start)
	}
	d.emit("deploy", "done", fmt.Sprintf("deploy %s concluído em %s (%s)", id, d.EnvName, short(p.Commit)))
	return nil
}

func (d *Deployer) emitDone(step string, start time.Time) {
	if d.Emit != nil {
		d.Emit(Event{Time: d.now().UTC(), Step: step, Level: "done", Msg: "ok", Duration: d.now().Sub(start).Round(time.Millisecond).String()})
	}
}

// fail restores files when they were already switched and explains the database situation.
func (d *Deployer) fail(ctx context.Context, step, id string, switched bool, cause error) error {
	if !switched {
		return fmt.Errorf("deploy interrompido na etapa %s; nenhum arquivo da aplicação foi alterado: %w", step, cause)
	}
	d.emit("rollback", "warn", "restaurando arquivos do backup "+id)
	if err := d.restore(ctx, id); err != nil {
		return fmt.Errorf("etapa %s falhou (%v) E a restauração automática falhou: %w\nrestaure à mão: madeploy rollback %s --to %s", step, cause, err, d.EnvName, id)
	}
	// this deploy is undone: its backup must not be offered to `rollback` again
	_, _ = remote.Run(ctx, d.Host, remote.Cmd("mv", d.MD("backups", id), d.MD("backups", "reverted-"+id)))
	_ = d.hooks(ctx, "after_switch", d.Env.Hooks.AfterSwitch, id)
	msg := fmt.Sprintf("etapa %s falhou; arquivos restaurados do backup %s", step, id)
	if d.Migrator != nil {
		msg += "\nATENÇÃO: o banco NÃO é revertido automaticamente. Dumps em " + d.MD("backups", id) + " (veja docs/runbook.md)"
	}
	d.emit("rollback", "done", msg)
	return fmt.Errorf("%s: %w", msg, cause)
}

func (d *Deployer) upload(ctx context.Context, p *Plan, id string) error {
	paths := make([]string, len(p.Files.Upload))
	for i, f := range p.Files.Upload {
		paths[i] = f.Path
	}
	tmp, err := os.MkdirTemp("", "madeploy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	pkg := filepath.Join(tmp, id+".tar.gz")
	sum, err := files.WritePackage(d.Root, paths, pkg)
	if err != nil {
		return err
	}
	dst := d.MD("staging", id+".tar.gz")
	if err := d.Host.Upload(pkg, dst); err != nil {
		return fmt.Errorf("enviando pacote: %w", err)
	}
	out, err := remote.Run(ctx, d.Host, remote.Cmd("sha256sum", dst))
	if err != nil {
		return err
	}
	if got, _, _ := strings.Cut(string(out), " "); got != sum {
		return fmt.Errorf("sha256 do pacote não confere após o upload (local %s, servidor %s)", sum, got)
	}
	dir := d.MD("staging", id)
	_, err = remote.Run(ctx, d.Host, remote.Cmd("mkdir", "-p", dir)+" && "+remote.Cmd("tar", "-xzf", dst, "-C", dir, "--no-same-owner"))
	d.emit("upload", "info", fmt.Sprintf("%d arquivo(s), pacote %s", len(paths), sum[:12]))
	return err
}

func (d *Deployer) apply(ctx context.Context, p *Plan, id string) error {
	if len(p.Files.Upload) > 0 {
		cmd := remote.Cmd("tar", "-xzf", d.MD("staging", id+".tar.gz"), "-C", d.base(), "--no-same-owner")
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return err
		}
	}
	for _, part := range chunk(p.Files.Delete, 200) {
		cmd := "cd " + remote.Quote(d.base()) + " && " + remote.Cmd(append([]string{"rm", "-f", "--"}, part...)...)
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return err
		}
	}
	d.emit("switch", "info", fmt.Sprintf("%d enviado(s), %d removido(s)", len(p.Files.Upload), len(p.Files.Delete)))
	return nil
}

func (d *Deployer) permissions(ctx context.Context, p *Plan) error {
	for _, op := range p.Permissions {
		if _, err := remote.Run(ctx, d.Host, op.Cmd); err != nil {
			return fmt.Errorf("%s: %w", op.Desc, err)
		}
		d.emit("permissions", "info", op.Desc)
	}
	return nil
}

func (d *Deployer) migrate(ctx context.Context, id string) error {
	return d.Migrator.Apply(ctx, d.MD("backups", id), d.Emit)
}

// saveState records what is now deployed. With --on-drift=ignore the drifted
// paths keep their previous manifest entry so they are reported again next time.
func (d *Deployer) saveState(p *Plan, id string, o Options) error {
	if o.want("files") {
		next := files.Manifest{}
		for k, v := range p.local {
			next[k] = v
		}
		if o.OnDrift == "ignore" {
			for _, dr := range p.Files.Drift {
				if prev, ok := p.deployed[dr.Path]; ok {
					next[dr.Path] = prev
				} else {
					delete(next, dr.Path)
				}
			}
		}
		if err := writeJSON(d.Host, d.MD("manifest.json"), next); err != nil {
			return err
		}
	}
	st := State{Commit: p.Commit, DeployedAt: d.now().UTC(), Operator: d.Operator, Backup: id}
	if !o.want("files") {
		st.Commit = p.DeployedCommit // db/perms-only runs don't move the code pointer
	}
	return writeJSON(d.Host, d.MD("state.json"), st)
}

func chunk(s []string, n int) [][]string {
	var out [][]string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

func short(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return c
}
