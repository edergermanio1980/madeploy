package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/remote"
)

const minFreeKB = 200 * 1024 // refuse to deploy with less than 200 MB free

// LockInfo is .madeploy/deploy.lock/info.json.
type LockInfo struct {
	Operator string    `json:"operator"`
	PID      int       `json:"pid"`
	Since    time.Time `json:"since"`
}

// Meta is .madeploy/backups/<id>/meta.json.
type Meta struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	Operator     string    `json:"operator"`
	CommitBefore string    `json:"commit_before"`
	CommitAfter  string    `json:"commit_after"`
	Files        int       `json:"files"`
	Added        int       `json:"added"`
	Previous     string    `json:"previous,omitempty"` // releases: release this one was built from
	Current      bool      `json:"current,omitempty"`  // releases: served by current
}

func (d *Deployer) preflight(ctx context.Context) error {
	cmd := "cd " + remote.Quote(d.Env.Path) + " && for t in tar sha256sum find; do command -v $t >/dev/null || { echo \"falta $t\"; exit 1; }; done && df -Pk ."
	if d.releases() {
		cmd = "[ -L " + remote.Quote(d.Env.Path+"/current") + " ] || { echo " +
			remote.Quote("sem "+d.Env.Path+"/current: rode `madeploy init "+d.EnvName+" --migrate-layout` primeiro") + " >&2; exit 1; }; " + cmd
	}
	out, err := remote.Run(ctx, d.Host, cmd)
	if err != nil {
		return fmt.Errorf("pré-checagem no servidor: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if f := strings.Fields(lines[len(lines)-1]); len(f) >= 4 {
		if free, err := strconv.ParseInt(f[3], 10, 64); err == nil && free < minFreeKB {
			return fmt.Errorf("pouco espaço livre no servidor: %d MB", free/1024)
		}
	}
	if ver, err := remote.Run(ctx, d.Host, remote.Cmd(d.Env.PHP, "-r", "echo PHP_VERSION;")); err == nil {
		d.emit("preflight", "info", "PHP remoto "+strings.TrimSpace(string(ver)))
	} else {
		d.emit("preflight", "warn", "php remoto ("+d.Env.PHP+") não respondeu; o lint remoto vai falhar")
	}
	return nil
}

// Lock creates the lock directory atomically (mkdir fails if it exists).
func (d *Deployer) Lock(ctx context.Context) error {
	dir := d.MD("deploy.lock")
	if _, err := remote.Run(ctx, d.Host, remote.Cmd("mkdir", "-p", d.MD())+" && "+remote.Cmd("mkdir", dir)); err != nil {
		var info LockInfo
		_, _ = readJSON(d.Host, dir+"/info.json", &info)
		return fmt.Errorf("deploy em andamento em %s por %s desde %s (pid %d); se ele morreu, use `madeploy unlock %s`",
			d.EnvName, info.Operator, info.Since.Format(time.RFC3339), info.PID, d.EnvName)
	}
	return writeJSON(d.Host, dir+"/info.json", LockInfo{Operator: d.Operator, PID: os.Getpid(), Since: d.now().UTC()})
}

// Unlock removes the lock unconditionally.
func (d *Deployer) Unlock(ctx context.Context) error {
	_, err := remote.Run(ctx, d.Host, remote.Cmd("rm", "-rf", "--", d.MD("deploy.lock")))
	return err
}

func phpFiles(p *Plan) []string {
	var out []string
	for _, f := range p.Files.Upload {
		if strings.HasSuffix(f.Path, ".php") {
			out = append(out, f.Path)
		}
	}
	return out
}

// lintLocal runs php -l before anything is sent. A missing php is only a
// warning: the remote lint on the production PHP is the mandatory one (ADR-12).
func (d *Deployer) lintLocal(ctx context.Context, p *Plan) error {
	bin := d.Cfg.Lint.Local
	if bin == "" {
		bin = "php"
	}
	if _, err := exec.LookPath(bin); err != nil {
		d.emit("lint-local", "warn", bin+" não encontrado localmente; só o lint remoto será feito")
		return nil
	}
	var bad []string
	for _, f := range phpFiles(p) {
		c := exec.CommandContext(ctx, bin, "-l", f)
		c.Dir = d.Root
		if out, err := c.CombinedOutput(); err != nil {
			bad = append(bad, strings.TrimSpace(string(out)))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("php -l falhou:\n%s", strings.Join(bad, "\n"))
	}
	return nil
}

func (d *Deployer) lintRemote(ctx context.Context, p *Plan, id string) error {
	php := remote.Quote(d.Env.PHP)
	for _, part := range chunk(phpFiles(p), 200) {
		cmd := "cd " + remote.Quote(d.MD("staging", id)) + " && for f in " + remote.Cmd(part...) +
			"; do out=$(" + php + " -l \"$f\" 2>&1) || { printf '%s\\n' \"$out\"; exit 1; }; done"
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return fmt.Errorf("php -l no servidor: %w", err)
		}
	}
	return nil
}

func (d *Deployer) backup(ctx context.Context, p *Plan, id string) error {
	bdir := d.MD("backups", id)
	var existing, added []string
	for _, f := range p.Files.Upload {
		if _, ok := p.live[f.Path]; ok {
			existing = append(existing, f.Path)
		} else {
			added = append(added, f.Path)
		}
	}
	existing = append(existing, p.Files.Delete...)
	if err := d.Host.WriteFile(bdir+"/files.list", []byte(lines(existing)), 0o640); err != nil {
		return err
	}
	if err := d.Host.WriteFile(bdir+"/added.list", []byte(lines(added)), 0o640); err != nil {
		return err
	}
	if len(existing) > 0 {
		cmd := "cd " + remote.Quote(d.base()) + " && " + remote.Cmd("tar", "-czf", bdir+"/files.tar.gz", "-T", bdir+"/files.list")
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return err
		}
	}
	for _, f := range []string{"manifest.json", "state.json"} {
		src := d.MD(f)
		cmd := "if [ -f " + remote.Quote(src) + " ]; then " + remote.Cmd("cp", "-p", src, bdir+"/"+f) + "; fi"
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return err
		}
	}
	meta := Meta{ID: id, CreatedAt: d.now().UTC(), Operator: d.Operator, CommitBefore: p.DeployedCommit,
		CommitAfter: p.Commit, Files: len(existing), Added: len(added)}
	d.emit("backup", "info", fmt.Sprintf("%d arquivo(s) salvos em %s", len(existing), bdir))
	return writeJSON(d.Host, bdir+"/meta.json", meta)
}

func lines(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return strings.Join(s, "\n") + "\n"
}

// restore undoes one deploy: removes files it added, puts back what it
// overwrote or deleted, and restores the previous manifest/state.
func (d *Deployer) restore(ctx context.Context, id string) error {
	bdir := d.MD("backups", id)
	raw, err := d.Host.ReadFile(bdir + "/added.list")
	if err != nil {
		return fmt.Errorf("backup %s incompleto: %w", id, err)
	}
	var added []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			added = append(added, l)
		}
	}
	for _, part := range chunk(added, 200) {
		cmd := "cd " + remote.Quote(d.base()) + " && " + remote.Cmd(append([]string{"rm", "-f", "--"}, part...)...)
		if _, err := remote.Run(ctx, d.Host, cmd); err != nil {
			return err
		}
	}
	tgz := bdir + "/files.tar.gz"
	cmd := "if [ -f " + remote.Quote(tgz) + " ]; then " + remote.Cmd("tar", "-xzf", tgz, "-C", d.base(), "--no-same-owner") + "; fi"
	for _, f := range []string{"manifest.json", "state.json"} {
		cmd += " && if [ -f " + remote.Quote(bdir+"/"+f) + " ]; then " + remote.Cmd("cp", "-p", bdir+"/"+f, d.MD(f)) + "; fi"
	}
	_, err = remote.Run(ctx, d.Host, cmd)
	return err
}

// Backups lists what can be rolled back to, newest first: releases (releases
// strategy) or file backups (inplace, already reverted ones skipped).
func (d *Deployer) Backups(ctx context.Context) ([]Meta, error) {
	if d.releases() {
		return d.listReleases(ctx)
	}
	out, err := remote.Run(ctx, d.Host, "ls -1 "+remote.Quote(d.MD("backups"))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var res []Meta
	for _, id := range strings.Fields(string(out)) {
		if strings.HasPrefix(id, "reverted-") {
			continue
		}
		m := Meta{ID: id}
		if _, err := readJSON(d.Host, d.MD("backups", id, "meta.json"), &m); err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].ID > res[j].ID })
	return res, nil
}

// Rollback reverts deploys from the newest down to (and including) target.
// An empty target reverts only the last deploy. The database is never touched.
func (d *Deployer) Rollback(ctx context.Context, target string) (err error) {
	if err := d.Lock(ctx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Unlock(ctx)) }()
	if d.releases() {
		if err := d.rollbackReleases(ctx, target); err != nil {
			return err
		}
		return d.afterRollback(ctx, target)
	}
	all, err := d.Backups(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return errors.New("nenhum backup disponível para reverter")
	}
	if target == "" {
		target = all[0].ID
	}
	if !hasBackup(all, target) {
		return fmt.Errorf("backup %s não encontrado (veja `madeploy releases %s`)", target, d.EnvName)
	}
	for _, m := range all {
		if m.ID < target {
			break
		}
		d.emit("rollback", "info", "revertendo deploy "+m.ID)
		if err := d.restore(ctx, m.ID); err != nil {
			return fmt.Errorf("revertendo %s: %w", m.ID, err)
		}
		// a reverted backup must never be applied twice
		if _, err := remote.Run(ctx, d.Host, remote.Cmd("mv", d.MD("backups", m.ID), d.MD("backups", "reverted-"+m.ID))); err != nil {
			return err
		}
	}
	return d.afterRollback(ctx, target)
}

func (d *Deployer) afterRollback(ctx context.Context, target string) error {
	if err := d.hooks(ctx, "after_switch", d.Env.Hooks.AfterSwitch, target); err != nil {
		return err
	}
	if d.Env.Healthcheck.URL != "" {
		if err := d.healthcheck(ctx); err != nil {
			return fmt.Errorf("código revertido, mas o healthcheck continua falhando: %w", err)
		}
	}
	d.emit("rollback", "done", "código revertido até "+target+"; o banco não foi alterado")
	return nil
}

func hasBackup(all []Meta, id string) bool {
	for _, m := range all {
		if m.ID == id {
			return true
		}
	}
	return false
}

func (d *Deployer) hooks(ctx context.Context, stage string, cmds []string, id string) error {
	return d.hooksIn(ctx, stage, cmds, id, d.base())
}

// hooksIn runs hooks from dir; {{release}} is dir (the new release before the switch).
func (d *Deployer) hooksIn(ctx context.Context, stage string, cmds []string, id, dir string) error {
	r := strings.NewReplacer("{{path}}", d.Env.Path, "{{release}}", dir, "{{staging}}", d.MD("staging", id), "{{id}}", id)
	for _, c := range cmds {
		c = r.Replace(c)
		d.emit(stage, "info", "$ "+c)
		// hooks are operator-written config and run as a shell line on purpose
		out, err := remote.Run(ctx, d.Host, "cd "+remote.Quote(dir)+" && "+c)
		if s := strings.TrimSpace(string(out)); s != "" {
			d.emit(stage, "info", s)
		}
		if err != nil {
			return fmt.Errorf("hook %q: %w", c, err)
		}
	}
	return nil
}

func (d *Deployer) healthcheck(ctx context.Context) error {
	if d.Health != nil {
		return d.Health(ctx, d.Env.Healthcheck)
	}
	return HTTPCheck(ctx, d.Env.Healthcheck)
}

// HTTPCheck polls the URL until it answers as expected or the timeout ends.
func HTTPCheck(ctx context.Context, hc config.Healthcheck) error {
	ctx, cancel := context.WithTimeout(ctx, hc.Timeout)
	defer cancel()
	for {
		last := probe(ctx, hc)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("healthcheck %s: %w", hc.URL, last)
		case <-time.After(2 * time.Second):
		}
	}
}

func probe(ctx context.Context, hc config.Healthcheck) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hc.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != hc.ExpectStatus {
		return fmt.Errorf("status %d, esperado %d", resp.StatusCode, hc.ExpectStatus)
	}
	if hc.ExpectBody != "" && !strings.Contains(string(body), hc.ExpectBody) {
		return fmt.Errorf("corpo não contém %q", hc.ExpectBody)
	}
	return nil
}

// cleanup removes staging and keeps only the newest Keep backups.
func (d *Deployer) cleanup(ctx context.Context) error {
	if _, err := remote.Run(ctx, d.Host, remote.Cmd("rm", "-rf", "--", d.MD("staging"))); err != nil {
		return err
	}
	all, err := d.Backups(ctx)
	if err != nil {
		return err
	}
	for i, m := range all {
		if i < d.Env.Keep {
			continue
		}
		if _, err := remote.Run(ctx, d.Host, remote.Cmd("rm", "-rf", "--", d.MD("backups", m.ID))); err != nil {
			return err
		}
		d.emit("cleanup", "info", "backup antigo removido: "+m.ID)
	}
	return nil
}

// AppendLog returns an emitter that also appends JSON lines to the server log
// .madeploy/logs/<id>.jsonl, so `madeploy logs --follow` can tail it live.
func AppendLog(ctx context.Context, h remote.Host, md, id string, next Emitter) Emitter {
	logPath := md + "/logs/" + id + ".jsonl"
	ready := false
	return func(e Event) {
		if next != nil {
			next(e)
		}
		if !ready {
			if _, err := remote.Run(ctx, h, remote.Cmd("mkdir", "-p", md+"/logs")); err != nil {
				return
			}
			ready = true
		}
		_ = h.Stream(ctx, "cat >> "+remote.Quote(logPath), strings.NewReader(string(e.JSON())), io.Discard)
	}
}
