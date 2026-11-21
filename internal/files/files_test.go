package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vitalsoft/madeploy/internal/config"
)

func e(sha string) Entry { return Entry{SHA256: sha} }

func TestCompute(t *testing.T) {
	local := Manifest{"a.php": e("a2"), "b.php": e("b1"), "new.php": e("n1"), "c.php": e("c1"), "same.php": e("s2")}
	deployed := Manifest{"a.php": e("a1"), "b.php": e("b1"), "gone.php": e("g1"), "c.php": e("c1"), "hand.php": e("h1"), "same.php": e("s1")}
	live := Manifest{"a.php": e("a1"), "b.php": e("b1"), "gone.php": e("g1"), "hand.php": e("hX"), "same.php": e("s2"), "new.php": e("other")}

	d := Compute(local, deployed, live, []Change{{Status: "R", From: "old.php", Path: "new.php"}})

	wantUp := map[string]string{"a.php": "M", "new.php": "R", "c.php": "A"}
	if len(d.Upload) != len(wantUp) {
		t.Fatalf("upload: %+v", d.Upload)
	}
	for _, f := range d.Upload {
		if wantUp[f.Path] != f.Status {
			t.Errorf("%s: status %s, want %s", f.Path, f.Status, wantUp[f.Path])
		}
	}
	if strings.Join(d.Delete, ",") != "gone.php,hand.php" {
		t.Fatalf("delete: %v", d.Delete)
	}
	// hand.php changed by hand; c.php deleted on the server; new.php created by hand.
	// same.php got by hand exactly the change we deploy: not drift.
	kinds := map[string]string{}
	for _, dr := range d.Drift {
		kinds[dr.Path] = dr.Kind
	}
	want := map[string]string{"hand.php": "modified", "c.php": "deleted", "new.php": "added"}
	if len(kinds) != len(want) {
		t.Fatalf("drift: %+v", d.Drift)
	}
	for p, k := range want {
		if kinds[p] != k {
			t.Errorf("drift %s: %q, want %q", p, kinds[p], k)
		}
	}
}

func TestComputeWithoutBaselineNeverDeletes(t *testing.T) {
	d := Compute(Manifest{"a": e("1")}, nil, Manifest{"a": e("0"), "b": e("1")}, nil)
	if len(d.Upload) != 1 || len(d.Delete) != 0 || len(d.Drift) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestMatcherDefaults(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".madeployignore"), []byte("*.bak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewMatcher(root, config.DefaultExclude)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"app/config/sisvita.php":          true,
		"app/config/framework_hashes.php": false,
		"tmp/x.png":                       true,
		"app/output/rel.pdf":              true,
		"files/documents/1/a.pdf":         true,
		".env":                            true,
		".env.production":                 true,
		"app/control/X.php":               false,
		"lib/adianti/.gitignore":          false,
		".gitignore":                      true,
		"x.bak":                           true,
		"database/sisvita/schema/a.sql":   true,
		"madeploy.yaml":                   true,
		"vendor/autoload.php":             false,
	} {
		if got := m.Excluded(p); got != want {
			t.Errorf("Excluded(%s) = %v, want %v", p, got, want)
		}
	}
}

func TestRemoteHashCmdAndParse(t *testing.T) {
	m, _ := NewMatcher(t.TempDir(), config.DefaultExclude)
	cmd := RemoteHashCmd("/var/www/a b", m, func(s string) string { return "'" + s + "'" })
	if !strings.Contains(cmd, "-path './tmp'") || !strings.Contains(cmd, "cd '/var/www/a b'") {
		t.Fatalf("cmd: %s", cmd)
	}
	sum := strings.Repeat("0", 64)
	got, err := ParseHashes([]byte(sum+"  ./index.php\n"+sum+"  ./app/config/sisvita.php\n"), m)
	if err != nil || len(got) != 1 || got["index.php"].SHA256 != sum {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseHashes([]byte("lixo\n"), m); err == nil {
		t.Fatal("saída inválida deveria falhar")
	}
}

func TestPackageRoundTripKeepsExecBit(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "app/a.php"), []byte("<?php"), 0o644)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh"), 0o755)
	pkg := filepath.Join(t.TempDir(), "p.tar.gz")
	if _, err := WritePackage(src, []string{"app/a.php", "run.sh"}, pkg); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if n, err := Extract(f, dst, nil); err != nil || n != 2 {
		t.Fatalf("extract: %d %v", n, err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "run.sh")); st.Mode()&0o111 == 0 {
		t.Fatal("bit de execução perdido")
	}
}

func TestParseNameStatus(t *testing.T) {
	got, err := parseNameStatus("M\x00a.php\x00R100\x00old.php\x00new.php\x00D\x00x.js\x00")
	if err != nil || len(got) != 3 || got[1].From != "old.php" || got[1].Path != "new.php" || got[2].Status != "D" {
		t.Fatalf("%+v %v", got, err)
	}
}

// `init --pull-to .` passes a relative destination.
func TestExtractIntoRelativeDir(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "manifest.json"), []byte("{}"), 0o644)
	pkg := filepath.Join(t.TempDir(), "p.tar.gz")
	if _, err := WritePackage(src, []string{"manifest.json"}, pkg); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dst := t.TempDir()
	t.Chdir(dst)
	if n, err := Extract(f, ".", nil); err != nil || n != 1 {
		t.Fatalf("extract: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}
