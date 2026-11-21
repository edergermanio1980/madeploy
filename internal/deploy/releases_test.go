package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func current(t *testing.T, f *fixture) string {
	t.Helper()
	l, err := os.Readlink(filepath.Join(f.server, "current"))
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	return filepath.Base(l)
}

func TestReleasesLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// in-place install → releases layout, without touching what is served
	msg, err := f.d.MigrateLayout(ctx, false)
	if err != nil {
		t.Fatalf("migrate-layout: %v", err)
	}
	if !strings.Contains(msg, "DocumentRoot") || read(t, f.server, "index.php") != "<?php // v1" {
		t.Fatalf("instalação antiga alterada ou instruções ausentes:\n%s", msg)
	}
	first := current(t, f)
	if read(t, f.server, "current/app/config/sisvita.php") != "<?php return ['pass' => 'segredo'];" {
		t.Fatal("config de produção não foi compartilhada")
	}
	if l, _ := os.Readlink(filepath.Join(f.server, "current/tmp")); !strings.HasSuffix(l, "/shared/tmp") {
		t.Fatalf("tmp deveria ser link para shared/: %q", l)
	}
	if _, err := f.d.MigrateLayout(ctx, false); err == nil {
		t.Fatal("migrar duas vezes deveria falhar")
	}
	f.d.Env.Strategy = "releases"

	// no changes: no new release
	if plan, err := f.d.Run(ctx, Options{}); err != nil || !plan.NoOp() || current(t, f) != first {
		t.Fatalf("deploy sem mudança: %v", err)
	}

	write(t, f.repo, "index.php", "<?php // v2")
	f.commit("v2")
	if _, err := f.d.Run(ctx, Options{}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	second := current(t, f)
	if second == first || read(t, f.server, "current/index.php") != "<?php // v2" {
		t.Fatal("release nova não ativada")
	}
	if read(t, f.server, "releases/"+first+"/index.php") != "<?php // v1" {
		t.Fatal("a release anterior foi alterada (hard link reescrito?)")
	}
	write(t, f.server, "shared/tmp/upload.bin", "novo upload")
	if read(t, f.server, "releases/"+first+"/tmp/upload.bin") != "novo upload" {
		t.Fatal("tmp não é compartilhado entre releases")
	}

	// healthcheck failure: current goes back, the failed release is removed
	write(t, f.repo, "index.php", "<?php // quebrado")
	f.commit("v3")
	f.health = errors.New("500")
	if _, err := f.d.Run(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "current voltou para "+second) {
		t.Fatalf("falha de healthcheck: %v", err)
	}
	if current(t, f) != second {
		t.Fatal("current não voltou")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.server, "releases")); len(entries) != 2 {
		t.Fatalf("release que falhou deveria ser removida: %d releases", len(entries))
	}

	// rollback to the first release, then forward again with --to
	f.health = nil
	if err := f.d.Rollback(ctx, ""); err != nil || current(t, f) != first {
		t.Fatalf("rollback: %v (current %s)", err, current(t, f))
	}
	if st, _ := f.d.ReadState(); st.Release != first {
		t.Fatalf("state.json deveria voltar para %s: %+v", first, st)
	}
	if err := f.d.Rollback(ctx, second); err != nil || current(t, f) != second {
		t.Fatalf("rollback --to: %v", err)
	}

	// retention (keep: 2) never removes current
	for i := 4; i <= 6; i++ {
		write(t, f.repo, "index.php", fmt.Sprintf("<?php // v%d", i))
		f.commit("v")
		if _, err := f.d.Run(ctx, Options{}); err != nil {
			t.Fatalf("deploy %d: %v", i, err)
		}
	}
	all, _ := f.d.Backups(ctx)
	if len(all) != 2 || !all[0].Current {
		t.Fatalf("retenção: %+v", all)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.server, "releases")); len(entries) != 2 {
		t.Fatalf("diretórios de release além do keep: %d", len(entries))
	}
}

func TestReleasesNeedMigrateLayout(t *testing.T) {
	f := newFixture(t)
	f.d.Env.Strategy = "releases"
	if err := os.MkdirAll(filepath.Join(f.server, ".madeploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, f.repo, "x.php", "<?php")
	f.commit("x")
	if _, err := f.d.Run(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "migrate-layout") {
		t.Fatalf("sem current deveria pedir migrate-layout: %v", err)
	}
}
