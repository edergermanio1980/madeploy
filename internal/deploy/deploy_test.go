package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/files"
	"github.com/vitalsoft/madeploy/internal/remote"
)

// TestMain provides sha256sum on machines (macOS) that only ship shasum.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		dir, _ := os.MkdirTemp("", "shim")
		_ = os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0o755)
		os.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	}
	os.Exit(m.Run())
}

type fixture struct {
	t      *testing.T
	repo   string
	server string
	d      *Deployer
	health error
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "<ausente>"
	}
	return string(b)
}

func (f *fixture) git(args ...string) {
	f.t.Helper()
	c := exec.Command("git", args...)
	c.Dir = f.repo
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (f *fixture) commit(msg string) {
	f.git("add", "-A")
	f.git("commit", "-qm", msg)
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, repo: t.TempDir(), server: filepath.Join(t.TempDir(), "app")}
	// the server as it is today, with runtime data and production config
	for rel, c := range map[string]string{
		"index.php":                    "<?php // v1",
		"app/control/PacienteList.php": "<?php class PacienteList {}",
		"lib/old.js":                   "old",
		"app/config/sisvita.php":       "<?php return ['pass' => 'segredo'];",
		"tmp/upload.bin":               "dado",
		"app/output/relatorio.pdf":     "pdf",
	} {
		write(t, f.server, rel, c)
	}
	// the repository seeded from production (config and runtime dirs never ship)
	f.git("init", "-q", "-b", "main")
	write(t, f.repo, "index.php", "<?php // v1")
	write(t, f.repo, "app/control/PacienteList.php", "<?php class PacienteList {}")
	write(t, f.repo, "lib/old.js", "old")
	write(t, f.repo, "app/config/sisvita.php", "<?php // dev config, nunca enviado")
	f.commit("seed")

	cfg, err := config.Parse([]byte(`
environments:
  test:
    ssh: local
    path: ` + f.server + `
    branch: main
    php: "true"
    keep: 2
    healthcheck: {url: "http://example.invalid/"}
permissions:
  writable: [tmp, app/output]
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Lint.Local = "true"
	env, _ := cfg.Env("test")
	n := 0
	f.d = &Deployer{
		Host: remote.Local{}, Cfg: cfg, EnvName: "test", Env: env, Root: f.repo, Operator: "tester",
		Health: func(context.Context, config.Healthcheck) error { return f.health },
		Now: func() time.Time {
			n++
			return time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Second)
		},
	}
	return f
}

func TestDeployLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	n, err := f.d.Baseline(ctx, false)
	if err != nil || n != 3 {
		t.Fatalf("baseline: n=%d err=%v (esperado 3 arquivos, sem config nem tmp)", n, err)
	}
	if _, err := f.d.Baseline(ctx, false); err == nil {
		t.Fatal("baseline repetido deveria exigir --force")
	}

	// first deploy after baseline: same content, nothing to do
	plan, err := f.d.Run(ctx, Options{})
	if err != nil || !plan.NoOp() {
		t.Fatalf("primeiro deploy deveria ser no-op: %+v err=%v", plan.Files, err)
	}

	// change one file, add one, delete one
	write(t, f.repo, "app/control/PacienteList.php", "<?php class PacienteList { /* v2 */ }")
	write(t, f.repo, "app/control/AgendaForm.php", "<?php class AgendaForm {}")
	os.Remove(filepath.Join(f.repo, "lib/old.js"))
	f.commit("v2")

	plan, err = f.d.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	if len(plan.Files.Upload) != 2 || len(plan.Files.Delete) != 1 {
		t.Fatalf("plano v2 inesperado: %+v", plan.Files)
	}
	if !strings.Contains(read(t, f.server, "app/control/PacienteList.php"), "v2") || read(t, f.server, "lib/old.js") != "<ausente>" {
		t.Fatal("arquivos do v2 não aplicados")
	}
	if read(t, f.server, "app/config/sisvita.php") != "<?php return ['pass' => 'segredo'];" || read(t, f.server, "tmp/upload.bin") != "dado" {
		t.Fatal("config de produção ou dados de runtime foram tocados")
	}
	st, _ := f.d.ReadState()
	if st.Commit != plan.Commit || st.Baseline {
		t.Fatalf("state.json não atualizado: %+v", st)
	}

	// idempotence: deploying again changes nothing
	if plan, err = f.d.Run(ctx, Options{}); err != nil || !plan.NoOp() {
		t.Fatalf("redeploy deveria ser no-op: %+v %v", plan.Files, err)
	}
	v2 := plan.Commit

	// drift: someone edits production by hand
	write(t, f.server, "index.php", "<?php // hotfix manual")
	write(t, f.repo, "index.php", "<?php // v3")
	f.commit("v3")
	_, err = f.d.Run(ctx, Options{})
	var de *DriftError
	if !errors.As(err, &de) || len(de.Drift) != 1 || de.Drift[0].Path != "index.php" {
		t.Fatalf("drift não detectado: %v", err)
	}
	if read(t, f.server, "index.php") != "<?php // hotfix manual" {
		t.Fatal("drift bloqueado não pode alterar arquivos")
	}

	// healthcheck fails: files go back to the previous state automatically
	f.health = errors.New("500")
	_, err = f.d.Run(ctx, Options{OnDrift: "overwrite"})
	if err == nil || !strings.Contains(err.Error(), "restaurados") {
		t.Fatalf("falha de healthcheck deveria restaurar: %v", err)
	}
	if read(t, f.server, "index.php") != "<?php // hotfix manual" {
		t.Fatalf("restauração não devolveu o arquivo: %q", read(t, f.server, "index.php"))
	}

	// healthy again: overwrite the drift, then roll the deploy back
	f.health = nil
	if _, err = f.d.Run(ctx, Options{OnDrift: "overwrite"}); err != nil {
		t.Fatalf("deploy v3: %v", err)
	}
	if read(t, f.server, "index.php") != "<?php // v3" {
		t.Fatal("v3 não aplicado")
	}
	if err := f.d.Rollback(ctx, ""); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if read(t, f.server, "index.php") != "<?php // hotfix manual" {
		t.Fatalf("rollback não restaurou: %q", read(t, f.server, "index.php"))
	}
	if st, _ = f.d.ReadState(); st.Commit != v2 {
		t.Fatalf("rollback deveria restaurar o state.json do v2, veio %q", st.Commit)
	}

	// lock blocks concurrent runs and is released afterwards
	if err := f.d.Lock(ctx); err != nil {
		t.Fatalf("lock deveria estar livre: %v", err)
	}
	if _, err := f.d.Run(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "em andamento") {
		t.Fatalf("lock não bloqueou: %v", err)
	}
	f.d.Unlock(ctx)

	all, _ := f.d.Backups(ctx)
	if len(all) > 2 {
		t.Fatalf("retenção falhou: %d backups", len(all))
	}
}

func TestDeployRefusesDirtyTreeAndWrongBranch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	write(t, f.repo, "index.php", "<?php // sujo")
	if _, err := f.d.Run(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "não commitadas") {
		t.Fatalf("árvore suja deveria bloquear: %v", err)
	}
	f.git("checkout", "-q", "--", "index.php")
	f.git("checkout", "-qb", "feature")
	if _, err := f.d.Run(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "branch") {
		t.Fatalf("branch errada deveria bloquear: %v", err)
	}
}

func TestRemoteLintFailureLeavesServerUntouched(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.d.Baseline(ctx, false); err != nil {
		t.Fatal(err)
	}
	write(t, f.repo, "index.php", "<?php syntax error")
	f.commit("quebrado")
	f.d.Env.PHP = "false" // php -l fails on the server
	_, err := f.d.Run(ctx, Options{})
	if err == nil || !strings.Contains(err.Error(), "nenhum arquivo da aplicação foi alterado") {
		t.Fatalf("lint remoto deveria abortar antes da troca: %v", err)
	}
	if read(t, f.server, "index.php") != "<?php // v1" {
		t.Fatal("servidor alterado apesar do lint falhar")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.d.Baseline(ctx, false); err != nil {
		t.Fatal(err)
	}
	write(t, f.repo, "novo.php", "<?php")
	f.commit("novo")
	plan, err := f.d.Run(ctx, Options{DryRun: true})
	if err != nil || len(plan.Files.Upload) != 1 {
		t.Fatalf("dry-run: %+v %v", plan, err)
	}
	if read(t, f.server, "novo.php") != "<ausente>" {
		t.Fatal("dry-run enviou arquivo")
	}
}

func TestOnlyPermsDoesNotShipFiles(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.d.Baseline(ctx, false); err != nil {
		t.Fatal(err)
	}
	write(t, f.repo, "novo.php", "<?php")
	f.commit("novo")
	plan, err := f.d.Run(ctx, Options{Only: map[string]bool{"perms": true}})
	if err != nil {
		t.Fatalf("--only perms: %v", err)
	}
	if len(plan.Files.Upload) != 0 || read(t, f.server, "novo.php") != "<ausente>" {
		t.Fatal("--only perms enviou arquivos")
	}
	if st, _ := f.d.ReadState(); st.Commit != "" {
		t.Fatalf("--only perms não pode mover o ponteiro de commit: %q", st.Commit)
	}
}

func TestPullSeedsWithoutSecrets(t *testing.T) {
	f := newFixture(t)
	dest := t.TempDir()
	var last files.Progress
	f.d.Progress = func(p files.Progress) { last = p }
	n, err := f.d.Pull(context.Background(), dest, nil)
	if err != nil || n != 3 {
		t.Fatalf("pull: n=%d err=%v", n, err)
	}
	if !last.Done || last.TotalFiles != 3 || last.TotalBytes != last.Bytes {
		t.Fatalf("progresso: a contagem prévia tem de bater com o download: %+v", last)
	}
	if read(t, dest, "app/config/sisvita.php") != "<ausente>" || read(t, dest, "tmp/upload.bin") != "<ausente>" {
		t.Fatal("pull trouxe config de produção ou dados de runtime")
	}
}
