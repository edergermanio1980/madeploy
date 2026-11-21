package deploy

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/perms"
	"github.com/vitalsoft/madeploy/internal/remote"
)

// Releases layout (ADR-04, F2):
//
//	<path>/releases/<id>/      full application tree, one per deploy
//	<path>/shared/             runtime data and production config, symlinked into each release
//	<path>/current -> releases/<id>
//	<path>/.madeploy/          state, manifest, shared.json, releases/<id>/{meta,manifest,state}.json

func (d *Deployer) relDir(id string) string { return path.Join(d.Env.Path, "releases", id) }

// currentRelease returns the id current points to ("" when there is no current link).
func (d *Deployer) currentRelease(ctx context.Context) (string, error) {
	out, err := remote.Run(ctx, d.Host, "readlink "+remote.Quote(d.Env.Path+"/current")+" || true")
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return path.Base(s), nil
	}
	return "", nil
}

// sharedList is what lives in shared/: recorded by migrate-layout, plus config.
func (d *Deployer) sharedList() []string {
	var list []string
	_, _ = readJSON(d.Host, d.MD("shared.json"), &list)
	if len(list) == 0 {
		list = append(list, config.SharedDirs...)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range append(list, d.Env.Shared...) {
		p = strings.Trim(p, "/")
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// linkShared replaces each shared path in rel by a symlink to shared/<path>
// (only for paths that exist in shared/).
func (d *Deployer) linkShared(ctx context.Context, rel string) error {
	shared := d.Env.Path + "/shared"
	for _, p := range d.sharedList() {
		target, link := shared+"/"+p, rel+"/"+p
		cmd := "if [ -e " + remote.Quote(target) + " ]; then " +
			remote.Cmd("rm", "-rf", "--", link) + " && " + remote.Cmd("mkdir", "-p", path.Dir(link)) + " && " +
			remote.Cmd("ln", "-s", target, link) + "; fi"
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return fmt.Errorf("link de %s: %w", p, err)
		}
	}
	return nil
}

// switchTo points current at releases/id atomically: rename(2) of a temp
// symlink with mv -T; ln -sfn only where mv -T does not exist.
func (d *Deployer) switchTo(ctx context.Context, id string) error {
	cur, tmp := d.Env.Path+"/current", d.Env.Path+"/current.tmp-madeploy"
	target := "releases/" + id
	cmd := remote.Cmd("ln", "-sfn", target, tmp) + " && { " + remote.Cmd("mv", "-T", tmp, cur) + " 2>/dev/null || { " +
		remote.Cmd("rm", "-f", tmp) + "; " + remote.Cmd("ln", "-sfn", target, cur) + "; }; }"
	_, err := remote.Run(ctx, d.Host, cmd)
	return err
}

func (d *Deployer) executeReleases(parent context.Context, p *Plan, id string, o Options) error {
	ctx := context.WithoutCancel(parent)
	prev, err := d.currentRelease(ctx)
	if err != nil {
		return err
	}
	if prev == "" {
		return fmt.Errorf("%s não tem %s/current: rode `madeploy init %s --migrate-layout` primeiro", d.EnvName, d.Env.Path, d.EnvName)
	}
	// --only db/perms: no new release, the current one stays
	wantFiles := o.want("files")
	rel := d.base()
	if wantFiles {
		rel = d.relDir(id)
	}
	hasUpload := wantFiles && len(p.Files.Upload) > 0
	changed := map[string]bool{}
	for _, f := range p.Files.Upload {
		changed[f.Path] = p.local[f.Path].Mode == "0755"
	}
	steps := []step{
		{"lint-local", func() error { return d.lintLocal(ctx, p) }, hasUpload},
		{"upload", func() error { return d.upload(ctx, p, id) }, hasUpload},
		{"lint-remote", func() error { return d.lintRemote(ctx, p, id) }, hasUpload},
		{"release", func() error { return d.prepareRelease(ctx, p, prev, id, hasUpload) }, wantFiles},
		{"permissions", func() error { return d.releasePermissions(ctx, rel, changed) }, wantFiles || o.want("perms")},
		{"before_switch", func() error { return d.hooksIn(ctx, "before_switch", p.BeforeSwitch, id, rel) }, true},
		{"migrate", func() error { return d.migrate(ctx, id) }, len(p.Migrations) > 0 && d.Env.MigrateWhen == "before"},
		{"switch", func() error { return d.switchTo(ctx, id) }, wantFiles},
		{"migrate", func() error { return d.migrate(ctx, id) }, len(p.Migrations) > 0 && d.Env.MigrateWhen == "after"},
		{"after_switch", func() error { return d.hooks(ctx, "after_switch", p.AfterSwitch, id) }, true},
		{"healthcheck", func() error { return d.healthcheck(ctx) }, d.Env.Healthcheck.URL != ""},
		{"state", func() error {
			if wantFiles {
				return d.saveRelease(p, prev, id, o)
			}
			return d.saveState(p, id, o)
		}, true},
		{"cleanup", func() error { return d.cleanupReleases(ctx) }, true},
	}
	switched := false
	for _, s := range steps {
		if !s.when {
			continue
		}
		if err := parent.Err(); err != nil {
			d.emit(s.name, "warn", "cancelado pelo operador antes desta etapa")
			return d.failRelease(ctx, s.name, prev, id, switched, err)
		}
		start := d.now()
		d.emit(s.name, "info", "início")
		if err := s.fn(); err != nil {
			d.emit(s.name, "error", err.Error())
			return d.failRelease(ctx, s.name, prev, id, switched, err)
		}
		switched = switched || s.name == "switch"
		d.emitDone(s.name, start)
	}
	d.emit("deploy", "done", fmt.Sprintf("release %s ativa em %s (%s)", id, d.EnvName, short(p.Commit)))
	return nil
}

// prepareRelease builds releases/id from the previous release (hard links when
// cp -al works) and applies only the delta. Changed paths are unlinked first so
// the previous release's inodes are never written to.
func (d *Deployer) prepareRelease(ctx context.Context, p *Plan, prev, id string, hasUpload bool) error {
	src, rel := d.relDir(prev), d.relDir(id)
	cp := remote.Cmd("cp", "-al", src, rel) + " 2>/dev/null || { " + remote.Cmd("rm", "-rf", "--", rel) + " && " + remote.Cmd("cp", "-a", src, rel) + "; }"
	if _, err := remote.Run(ctx, d.Host, remote.Cmd("mkdir", "-p", d.Env.Path+"/releases")+" && "+cp); err != nil {
		return fmt.Errorf("copiando a release %s: %w", prev, err)
	}
	var touched []string
	for _, f := range p.Files.Upload {
		touched = append(touched, f.Path)
	}
	for _, part := range chunk(append(touched, p.Files.Delete...), 200) {
		if _, err := remote.Run(ctx, d.Host, "cd "+remote.Quote(rel)+" && "+remote.Cmd(append([]string{"rm", "-f", "--"}, part...)...)); err != nil {
			return err
		}
	}
	if hasUpload {
		cmd := remote.Cmd("tar", "-xzf", d.MD("staging", id+".tar.gz"), "-C", rel, "--no-same-owner")
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return err
		}
	}
	d.emit("release", "info", fmt.Sprintf("%s a partir de %s: %d enviado(s), %d removido(s)", id, prev, len(p.Files.Upload), len(p.Files.Delete)))
	return d.linkShared(ctx, rel)
}

// releasePermissions fixes the changed files in the new release and the
// writable dirs where they really live: shared/, not through the symlinks.
func (d *Deployer) releasePermissions(ctx context.Context, rel string, changed map[string]bool) error {
	files := d.Cfg.Permissions
	files.Writable = nil
	ops := append(perms.Ops(files, rel, changed, d.Env.Sudo), perms.Writable(d.Cfg.Permissions, d.Env.Path+"/shared", d.Env.Sudo)...)
	for _, op := range ops {
		if _, err := remote.Run(ctx, d.Host, op.Cmd); err != nil {
			return fmt.Errorf("%s: %w", op.Desc, err)
		}
		d.emit("permissions", "info", op.Desc)
	}
	return nil
}

// failRelease points current back to the previous release and drops the new one.
func (d *Deployer) failRelease(ctx context.Context, step, prev, id string, switched bool, cause error) error {
	if switched {
		d.emit("rollback", "warn", "voltando current para "+prev)
		if err := d.switchTo(ctx, prev); err != nil {
			return fmt.Errorf("etapa %s falhou (%v) E não foi possível voltar para %s: %w\nà mão: ln -sfn releases/%s %s/current", step, cause, prev, err, prev, d.Env.Path)
		}
		_ = d.hooks(ctx, "after_switch", d.Env.Hooks.AfterSwitch, id)
	}
	if cur, _ := d.currentRelease(ctx); cur != id { // never delete what is being served
		_, _ = remote.Run(ctx, d.Host, remote.Cmd("rm", "-rf", "--", d.relDir(id), d.MD("staging")))
	}
	if !switched {
		return fmt.Errorf("deploy interrompido na etapa %s; a release em produção (%s) não mudou: %w", step, prev, cause)
	}
	msg := fmt.Sprintf("etapa %s falhou; current voltou para %s (arquivos restaurados)", step, prev)
	if d.Migrator != nil {
		msg += "\nATENÇÃO: o banco NÃO é revertido automaticamente. Dumps em " + d.MD("backups", id) + " (veja docs/runbook.md)"
	}
	d.emit("rollback", "done", msg)
	return fmt.Errorf("%s: %w", msg, cause)
}

// saveRelease records state, manifest and a per-release copy used by rollback.
func (d *Deployer) saveRelease(p *Plan, prev, id string, o Options) error {
	if err := d.saveState(p, id, o); err != nil {
		return err
	}
	st, err := d.ReadState()
	if err != nil {
		return err
	}
	st.Release = id
	if err := writeJSON(d.Host, d.MD("state.json"), st); err != nil {
		return err
	}
	added := 0
	for _, f := range p.Files.Upload {
		if f.Status == "A" {
			added++
		}
	}
	meta := Meta{ID: id, CreatedAt: d.now().UTC(), Operator: d.Operator, CommitBefore: p.DeployedCommit,
		CommitAfter: st.Commit, Files: len(p.Files.Upload) - added + len(p.Files.Delete), Added: added, Previous: prev}
	return d.snapshotRelease(id, meta)
}

func (d *Deployer) snapshotRelease(id string, meta Meta) error {
	for _, name := range []string{"manifest.json", "state.json"} {
		raw, err := d.Host.ReadFile(d.MD(name))
		if err != nil {
			return err
		}
		if err := d.Host.WriteFile(d.MD("releases", id, name), raw, 0o640); err != nil {
			return err
		}
	}
	return writeJSON(d.Host, d.MD("releases", id, "meta.json"), meta)
}

// listReleases returns completed releases, newest first, marking current.
func (d *Deployer) listReleases(ctx context.Context) ([]Meta, error) {
	cur, err := d.currentRelease(ctx)
	if err != nil {
		return nil, err
	}
	out, err := remote.Run(ctx, d.Host, "ls -1 "+remote.Quote(d.MD("releases"))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var res []Meta
	for _, id := range strings.Fields(string(out)) {
		m := Meta{ID: id}
		if ok, err := readJSON(d.Host, d.MD("releases", id, "meta.json"), &m); err != nil || !ok {
			continue
		}
		m.Current = id == cur
		res = append(res, m)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].ID > res[j].ID })
	return res, nil
}

// rollbackReleases points current at target (default: the release before current).
func (d *Deployer) rollbackReleases(ctx context.Context, target string) error {
	all, err := d.listReleases(ctx)
	if err != nil {
		return err
	}
	cur := ""
	for i, m := range all {
		if m.Current {
			cur = m.ID
			if target == "" && i+1 < len(all) {
				target = all[i+1].ID
			}
		}
	}
	if target == "" {
		return errors.New("não há release anterior para voltar")
	}
	if !hasBackup(all, target) {
		return fmt.Errorf("release %s não encontrada (veja `madeploy releases %s`)", target, d.EnvName)
	}
	if target == cur {
		return fmt.Errorf("%s já é a release atual", target)
	}
	d.emit("rollback", "info", "current: "+cur+" → "+target)
	if err := d.switchTo(ctx, target); err != nil {
		return err
	}
	for _, f := range []string{"manifest.json", "state.json"} {
		raw, err := d.Host.ReadFile(d.MD("releases", target, f))
		if err != nil {
			return err
		}
		if err := d.Host.WriteFile(d.MD(f), raw, 0o640); err != nil {
			return err
		}
	}
	return nil
}

// cleanupReleases keeps the newest Keep releases (current always survives) and
// removes release dirs that never completed.
func (d *Deployer) cleanupReleases(ctx context.Context) error {
	if _, err := remote.Run(ctx, d.Host, remote.Cmd("rm", "-rf", "--", d.MD("staging"))); err != nil {
		return err
	}
	all, err := d.listReleases(ctx)
	if err != nil {
		return err
	}
	cur, err := d.currentRelease(ctx)
	if err != nil {
		return err
	}
	keep := map[string]bool{cur: true}
	for i, m := range all {
		if i < d.Env.Keep {
			keep[m.ID] = true
		}
	}
	out, err := remote.Run(ctx, d.Host, "ls -1 "+remote.Quote(d.Env.Path+"/releases")+" 2>/dev/null || true")
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(string(out)) {
		if keep[id] {
			continue
		}
		if _, err := remote.Run(ctx, d.Host, remote.Cmd("rm", "-rf", "--", d.relDir(id), d.MD("releases", id))); err != nil {
			return err
		}
		d.emit("cleanup", "info", "release antiga removida: "+id)
	}
	return nil
}

// MigrateLayout converts an in-place install into the releases layout without
// downtime: the running tree is copied (not moved) into releases/<id>, runtime
// data and production config are copied into shared/, and current is created.
// Pointing the web server at current is left to the operator (returned text).
// With finish, it only copies uploads written meanwhile (cp -n never overwrites).
func (d *Deployer) MigrateLayout(ctx context.Context, finish bool) (string, error) {
	d.Env.Strategy = "releases" // the baseline below must hash current, not the old tree
	if finish {
		return d.finishMigration(ctx)
	}
	base := d.Env.Path
	if cur, err := d.currentRelease(ctx); err != nil || cur != "" {
		return "", errors.Join(err, fmt.Errorf("%s já usa releases (current → %s)", base, cur))
	}
	id := NewID(d.now())
	rel := d.relDir(id)
	skip := "case \"$f\" in .madeploy|releases|shared|current|current.tmp-madeploy) continue;; esac"
	copyTree := "cd " + remote.Quote(base) + " && " + remote.Cmd("mkdir", "-p", rel, base+"/shared") +
		" && for f in * .[!.]* ..?*; do [ -e \"$f\" ] || continue; " + skip + "; cp -a \"$f\" " + remote.Quote(rel+"/") + " || exit 1; done"
	if _, err := remote.Run(ctx, d.Host, copyTree); err != nil {
		return "", fmt.Errorf("copiando a instalação atual: %w", err)
	}
	cfgFiles, err := remote.Run(ctx, d.Host, "cd "+remote.Quote(base)+" && for f in app/config/*; do if [ -f \"$f\" ] && [ \"$f\" != app/config/framework_hashes.php ]; then echo \"$f\"; fi; done")
	if err != nil {
		return "", err
	}
	list := append(append(append([]string{}, config.SharedDirs...), strings.Fields(string(cfgFiles))...), d.Env.Shared...)
	for _, p := range list {
		src, dst := base+"/"+p, base+"/shared/"+p
		cmd := "if [ -e " + remote.Quote(src) + " ] && [ ! -e " + remote.Quote(dst) + " ]; then " +
			remote.Cmd("mkdir", "-p", path.Dir(dst)) + " && " + remote.Cmd("cp", "-a", src, dst) + "; fi"
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return "", fmt.Errorf("copiando %s para shared/: %w", p, err)
		}
	}
	if err := writeJSON(d.Host, d.MD("shared.json"), list); err != nil {
		return "", err
	}
	if err := d.linkShared(ctx, rel); err != nil {
		return "", err
	}
	if err := d.switchTo(ctx, id); err != nil {
		return "", err
	}
	if _, err := d.Baseline(ctx, true); err != nil {
		return "", err
	}
	st, err := d.ReadState()
	if err != nil {
		return "", err
	}
	st.Release = id
	if err := writeJSON(d.Host, d.MD("state.json"), st); err != nil {
		return "", err
	}
	if err := d.snapshotRelease(id, Meta{ID: id, CreatedAt: d.now().UTC(), Operator: d.Operator}); err != nil {
		return "", err
	}
	return fmt.Sprintf(`layout de releases criado (%[3]s); a instalação in-place continua no ar, intocada.

Falta (manual — o madeploy não mexe no servidor web):
  1. no vhost, aponte o DocumentRoot para %[1]s/current
     (Apache: DocumentRoot e <Directory %[1]s/current> com FollowSymLinks; nginx: root %[1]s/current;)
  2. teste a configuração e recarregue o servidor web e o PHP-FPM
  3. em madeploy.yaml, use strategy: releases neste ambiente
  4. rode madeploy init %[2]s --migrate-layout --finish (copia os uploads gravados entre a cópia e a troca)
  5. depois de conferir, remova os arquivos antigos do topo de %[1]s (menos .madeploy, releases, shared, current)`, base, d.EnvName, id), nil
}

func (d *Deployer) finishMigration(ctx context.Context) (string, error) {
	base := d.Env.Path
	n := 0
	for _, p := range d.sharedList() {
		src, dst := base+"/"+p, base+"/shared/"+p
		cmd := "if [ -d " + remote.Quote(src) + " ] && [ ! -L " + remote.Quote(src) + " ] && [ -d " + remote.Quote(dst) + " ]; then " +
			remote.Cmd("cp", "-Rpn", src+"/.", dst+"/") + "; fi"
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return "", fmt.Errorf("sincronizando %s: %w", p, err)
		}
		n++
	}
	return fmt.Sprintf("%d caminho(s) compartilhado(s) conferidos; uploads novos copiados sem sobrescrever nada. Confira e remova a instalação antiga do topo de %s", n, base), nil
}
