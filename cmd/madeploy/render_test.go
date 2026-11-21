package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalsoft/madeploy/internal/deploy"
	"github.com/vitalsoft/madeploy/internal/files"
	"github.com/vitalsoft/madeploy/internal/perms"
)

var update = flag.Bool("update", false, "regrava os golden files")

func TestRenderPlanGolden(t *testing.T) {
	p := &deploy.Plan{
		Environment: "production", Path: "/var/www/sisvita",
		Commit: "5d7ed2faf0aa11", Branch: "main", DeployedCommit: "510230a327bb22",
		Files: files.Delta{
			Upload: []files.FileChange{
				{Status: "A", Path: "app/control/AgendaForm.php"},
				{Status: "M", Path: "index.php"},
				{Status: "R", Path: "app/control/novo/PacienteList.php", From: "app/control/PacienteList.php"},
			},
			Delete: []string{"lib/old.js"},
			Drift:  []files.Drift{{Path: "menu.xml", Kind: "modified"}},
		},
		Migrations:  []string{"sisvita: migrations/20261118093000_add_apelido.sql (pending)"},
		Permissions: []perms.Op{{Desc: "chmod 0644 em 3 arquivo(s)"}, {Desc: "tmp gravável pelo grupo www-data (ACL ou setgid 2775)"}},
		AfterSwitch: []string{"sudo -n systemctl reload php8.2-fpm"},
		Healthcheck: "https://example.test/index.php?class=LoginForm",
		Warnings:    []string{"aviso de exemplo"},
	}
	var got bytes.Buffer
	renderPlan(&got, p)
	golden := filepath.Join("testdata", "plan.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (rode go test ./cmd/madeploy -update)", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("plano mudou; revise e rode com -update se for intencional\n--- got\n%s\n--- want\n%s", got.String(), want)
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{&deploy.DriftError{Drift: []files.Drift{{Path: "a", Kind: "modified"}}}, exitBlocked},
		{deploy.ErrAborted, exitAborted},
		{configError{os.ErrNotExist}, exitConfig},
	}
	for _, tt := range tests {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestBuilderToken(t *testing.T) {
	if !builderToken("[builder]\nmanager_url = x\ntoken = \"abc\"\n[general]\n") {
		t.Fatal("token presente não detectado")
	}
	if builderToken("[builder]\ntoken = \n[general]\ntoken = xyz\n") {
		t.Fatal("token vazio no [builder] (e token de outra seção) não deveria alertar")
	}
}

func TestProgressText(t *testing.T) {
	p := files.Progress{Files: 120, TotalFiles: 5678, Bytes: 10 << 20, TotalBytes: 40 << 20, Name: "app/control/AgendaForm.php"}
	got := progressText(p, 10*time.Second)
	want := "⇣ 120/5678 arquivos · 10.0 MB/40.0 MB · 25% · 1.0 MB/s · restam ~30s · app/control/AgendaForm.php"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	p.Done = true
	if got := progressText(p, 10*time.Second); strings.Contains(got, "restam") || !strings.HasSuffix(got, "em 10s") {
		t.Fatal(got)
	}
	if got := progressText(files.Progress{Files: 3, Bytes: 10}, 0); got != "⇣ 3 arquivos · 10 B" {
		t.Fatal(got) // sem totais: sem porcentagem nem previsão
	}
}
