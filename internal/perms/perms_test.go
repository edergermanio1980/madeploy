package perms

import (
	"strings"
	"testing"

	"github.com/vitalsoft/madeploy/internal/config"
)

func cfg() config.Permissions {
	return config.Permissions{
		Owner: "deploy", Group: "www-data",
		Default:    config.Modes{Dir: "0755", File: "0644"},
		Writable:   []string{"tmp", "app/output"},
		Restricted: map[string]string{"app/config/*": "0640"},
		Executable: []string{"bin/*.sh"},
	}
}

func TestFileMode(t *testing.T) {
	p := cfg()
	for rel, want := range map[string]string{
		"index.php":              "0644",
		"app/config/sisvita.php": "0640",
		"bin/backup.sh":          "0755",
	} {
		if got := FileMode(p, rel, false); got != want {
			t.Errorf("%s: %s, want %s", rel, got, want)
		}
	}
	if FileMode(p, "cmd.php", true) != "0755" {
		t.Error("bit de execução do repositório deveria ser mantido")
	}
}

func TestOpsOnlyTouchChangedFilesAndWritableDirs(t *testing.T) {
	ops := Ops(cfg(), "/var/www/app", map[string]bool{"app/control/A.php": false, "bin/x.sh": false}, false)
	var all []string
	for _, op := range ops {
		all = append(all, op.Cmd)
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{
		"chmod -- 0644 app/control/A.php",
		"chmod -- 0755 bin/x.sh",
		"chmod -- 0755 app app/control bin",
		"chgrp -- www-data app/control/A.php bin/x.sh app app/control bin",
		"setfacl -R -m g:www-data:rwX -m d:g:www-data:rwX -- tmp",
		"chmod -- 2775 app/output",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("faltou %q em:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "chmod -R") || strings.Contains(joined, "sudo") {
		t.Errorf("não deveria ser recursivo na árvore nem usar sudo:\n%s", joined)
	}
	withSudo := Ops(cfg(), "/var/www/app", map[string]bool{"a.php": false}, true)
	found := false
	for _, op := range withSudo {
		found = found || strings.Contains(op.Cmd, "sudo -n chown -- deploy:www-data")
	}
	if !found {
		t.Error("com sudo deveria usar chown owner:group")
	}
}

func TestOpsQuoteHostilePaths(t *testing.T) {
	ops := Ops(cfg(), "/var/www/app", map[string]bool{"app/x'; rm -rf /; '.php": false}, false)
	if !strings.Contains(ops[0].Cmd, `'app/x'\''; rm -rf /; '\''.php'`) {
		t.Fatalf("caminho não escapado: %s", ops[0].Cmd)
	}
}
