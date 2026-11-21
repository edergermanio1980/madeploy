package deploy

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vitalsoft/madeploy/internal/files"
	"github.com/vitalsoft/madeploy/internal/remote"
)

// Baseline records the live server as "what is deployed" so the first real
// deploy can detect drift and deletions (ADR-05). It refuses to overwrite an
// existing manifest unless force is set.
func (d *Deployer) Baseline(ctx context.Context, force bool) (int, error) {
	if _, err := d.Host.ReadFile(d.MD("manifest.json")); err == nil && !force {
		return 0, fmt.Errorf("%s já tem manifesto; use --force para refazer o baseline", d.EnvName)
	}
	m, err := d.Matcher()
	if err != nil {
		return 0, err
	}
	start := d.now()
	d.emit("baseline", "info", "calculando o sha256 de cada arquivo no servidor (só leitura)…")
	live, err := d.LiveManifest(ctx, m)
	if err != nil {
		return 0, err
	}
	if err := writeJSON(d.Host, d.MD("manifest.json"), live); err != nil {
		return 0, err
	}
	st := State{DeployedAt: d.now().UTC(), Operator: d.Operator, Baseline: true}
	if err := writeJSON(d.Host, d.MD("state.json"), st); err != nil {
		return 0, err
	}
	d.emitDoneMsg("baseline", fmt.Sprintf("%d arquivo(s) registrados em %s", len(live), d.MD("")), start)
	return len(live), nil
}

// Pull downloads the deployable part of the server tree into dest. It seeds the
// Git repository from production and backs `drift pull` for single files.
func (d *Deployer) Pull(ctx context.Context, dest string, only []string) (int, error) {
	m, err := d.Matcher()
	if err != nil {
		return 0, err
	}
	args := []string{"tar", "-czf", "-"}
	for _, p := range m.Prunes() {
		args = append(args, "--exclude=./"+p)
	}
	if len(only) == 0 {
		args = append(args, ".")
	}
	for _, p := range only {
		args = append(args, "./"+strings.TrimPrefix(p, "./"))
	}
	start := d.now()
	var total files.Progress
	if len(only) == 0 {
		d.emit("pull", "info", "contando arquivos em "+d.base()+" (sem config, uploads e excluídos)…")
		if out, err := remote.Run(ctx, d.Host, files.RemoteSizeCmd(d.base(), m, remote.Quote)); err != nil {
			d.emit("pull", "warn", "contagem falhou; seguindo sem previsão de tempo: "+err.Error())
		} else if total.TotalFiles, total.TotalBytes, err = files.ParseSize(out, m); err != nil {
			d.emit("pull", "warn", err.Error())
		} else {
			d.emit("pull", "info", fmt.Sprintf("%d arquivo(s), %s a baixar para %s", total.TotalFiles, files.HumanBytes(total.TotalBytes), dest))
		}
	}
	report := func(p files.Progress) {
		if d.Progress != nil {
			p.TotalFiles, p.TotalBytes = total.TotalFiles, total.TotalBytes
			d.Progress(p)
		}
	}
	cmd := "cd " + remote.Quote(d.base()) + " && " + remote.Cmd(args...)
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := d.Host.Stream(ctx, cmd, nil, pw)
		pw.CloseWithError(err)
		errc <- err
	}()
	var last files.Progress
	n, xerr := files.ExtractProgress(pr, dest, m, func(p files.Progress) { last = p; report(p) })
	_, _ = io.Copy(io.Discard, pr) // drain gzip trailer so the remote tar exits cleanly
	pr.Close()
	last.Done = true
	report(last)
	if err := <-errc; err != nil {
		return n, err
	}
	if xerr == nil {
		d.emitDoneMsg("pull", fmt.Sprintf("%d arquivo(s), %s baixados", n, files.HumanBytes(last.Bytes)), start)
	}
	return n, xerr
}

func (d *Deployer) emitDoneMsg(step, msg string, start time.Time) {
	if d.Emit != nil {
		d.Emit(Event{Time: d.now().UTC(), Step: step, Level: "done", Msg: Redact(msg),
			Duration: d.now().Sub(start).Round(time.Second).String()})
	}
}

// SudoersSnippet is the least-privilege sudoers line suggested by `init`.
func SudoersSnippet(user, phpService string) string {
	return fmt.Sprintf("# /etc/sudoers.d/madeploy  (valide com: visudo -cf /etc/sudoers.d/madeploy)\n%s ALL=(root) NOPASSWD: /usr/bin/systemctl reload %s\n", user, phpService)
}
