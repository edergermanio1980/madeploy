//go:build integration

// End-to-end test of the madeploy binary against a simulated production:
// SSH + Apache + PHP 8.2 container and a MySQL 8.0 container on a private
// network (the database is reachable only through the SSH tunnel).
//
//	go test -tags integration -v ./test/integration/
package integration

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const initSQL = `
CREATE DATABASE sisvita CHARACTER SET utf8mb3 COLLATE utf8mb3_unicode_ci;
CREATE USER 'app'@'%' IDENTIFIED WITH mysql_native_password BY 'app-s3cret';
GRANT ALL ON sisvita.* TO 'app'@'%';
USE sisvita;
CREATE TABLE system_group (id int NOT NULL, name varchar(256) NOT NULL, uuid varchar(36) NULL, PRIMARY KEY (id));
CREATE TABLE system_program (id int NOT NULL, name varchar(256) NOT NULL, controller varchar(256) NOT NULL, actions text NULL, PRIMARY KEY (id));
CREATE TABLE system_group_program (id int NOT NULL, system_group_id int NULL, system_program_id int NULL, actions text NULL, PRIMARY KEY (id));
CREATE TABLE pessoa (id int NOT NULL AUTO_INCREMENT, nome varchar(200) NOT NULL, deleted_at datetime NULL, PRIMARY KEY (id), KEY idx_pessoa_ativo_nome (deleted_at, nome));
CREATE VIEW vw_pessoas AS SELECT id, nome FROM pessoa WHERE deleted_at IS NULL;
CREATE TRIGGER pessoa_nome BEFORE INSERT ON pessoa FOR EACH ROW SET NEW.nome = TRIM(NEW.nome);
INSERT INTO system_group VALUES (1, 'Admin', NULL), (2, 'Standard', NULL);
INSERT INTO system_program VALUES (1, 'Pacientes', 'PacienteList', NULL);
INSERT INTO system_group_program VALUES (1, 1, 1, NULL);
INSERT INTO pessoa (nome) VALUES ('Fulano');
`

const connPHP = `<?php
return [
    'host' => "db",
    'port' => "3306",
    'name' => "sisvita",
    'user' => "app",
    'pass' => "app-s3cret",
    'type' => "mysql",
];
`

type env struct {
	t                 *testing.T
	id                string
	home, repo, bin   string
	sshPort, httpPort string
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func put(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *env {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker indisponível")
	}
	if _, err := exec.LookPath("mysqldef"); err != nil {
		t.Skip("mysqldef indisponível (go install github.com/sqldef/sqldef/v3/cmd/mysqldef@v3.11.30)")
	}
	e := &env{t: t, id: fmt.Sprintf("madeploy-it-%d", time.Now().UnixNano()), home: t.TempDir(), repo: t.TempDir()}
	keys, seed, dbinit := t.TempDir(), t.TempDir(), t.TempDir()

	// SSH key for the deploy user
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	put(t, e.home, ".ssh/id_ed25519", string(pem.EncodeToMemory(block)))
	if err := os.Chmod(filepath.Join(e.home, ".ssh/id_ed25519"), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	put(t, keys, "authorized_keys", string(ssh.MarshalAuthorizedKey(sshPub)))

	// production as the MadBuilder left it
	put(t, seed, "index.php", `<?php echo "LoginForm v1";`)
	put(t, seed, "app/control/PacienteList.php", "<?php\nclass PacienteList extends TPage {}\n")
	put(t, seed, "menu.xml", `<?xml version="1.0"?><menu><menuitem label="Pacientes"><action>PacienteList#method=onShow</action></menuitem></menu>`)
	put(t, seed, "app/config/sisvita.php", connPHP)
	put(t, seed, "app/config/permission.php", connPHP)
	put(t, seed, "app/config/application.ini", "[builder]\ntoken = abc123\n[general]\nseed = x\n")
	put(t, seed, "tmp/.keep", "")
	put(t, seed, "app/output/relatorio.pdf", "pdf")
	put(t, dbinit, "init.sql", initSQL)

	sh(t, "docker", "network", "create", e.id)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", e.id+"-db", e.id+"-app").Run()
		_ = exec.Command("docker", "network", "rm", e.id).Run()
	})
	sh(t, "docker", "run", "-d", "--name", e.id+"-db", "--network", e.id, "--network-alias", "db",
		"-e", "MYSQL_ROOT_PASSWORD=root", "-v", dbinit+":/docker-entrypoint-initdb.d:ro",
		"mysql:8.0", "--default-authentication-plugin=mysql_native_password")
	sh(t, "docker", "build", "-q", "-t", "madeploy-it-target", "target")
	sh(t, "docker", "run", "-d", "--name", e.id+"-app", "--network", e.id,
		"-v", seed+":/seed:ro", "-v", keys+":/keys:ro", "-p", "127.0.0.1::22", "-p", "127.0.0.1::80", "madeploy-it-target")
	e.sshPort = portOf(t, e.id+"-app", "22")
	e.httpPort = portOf(t, e.id+"-app", "80")

	waitFor(t, "mysql", func() bool {
		return exec.Command("docker", "exec", e.id+"-db", "mysql", "-uapp", "-papp-s3cret", "-e", "SELECT 1 FROM sisvita.pessoa").Run() == nil
	})
	var hostKeys string
	waitFor(t, "ssh", func() bool {
		out, err := exec.Command("ssh-keyscan", "-p", e.sshPort, "127.0.0.1").Output()
		hostKeys = string(out)
		return err == nil && strings.Contains(hostKeys, "ssh-ed25519")
	})
	put(t, e.home, ".ssh/known_hosts", hostKeys)

	e.bin = filepath.Join(t.TempDir(), "madeploy")
	if out, err := exec.Command("go", "build", "-o", e.bin, "../../cmd/madeploy").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	put(t, e.repo, "madeploy.yaml", fmt.Sprintf(`
permissions:
  owner: deploy
  group: www-data
  writable: [tmp, app/output]
databases:
  sisvita: {config: app/config/sisvita.php, dump_options: [--skip-ssl-verify-server-cert]}
  permission: {config: app/config/permission.php, backup: none}
programs:
  connection: permission
  groups: [Admin]
environments:
  it:
    ssh: deploy@127.0.0.1:%s
    path: /var/www/html
    branch: main
    php: php
    keep: 3
    hooks:
      # clears OPcache (revalidate_freq=2 in the image), like reloading php-fpm in production
      after_switch: ["echo hook-ok", "sudo -n /usr/sbin/apache2ctl graceful"]
    healthcheck:
      url: http://127.0.0.1:%s/index.php?class=LoginForm
      expect_body: LoginForm
      timeout: 6s
`, e.sshPort, e.httpPort))
	put(t, e.repo, ".gitignore", ".madeploy/\n")
	return e
}

func portOf(t *testing.T, container, port string) string {
	out := sh(t, "docker", "port", container, port)
	return out[strings.LastIndex(out, ":")+1:]
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 90; i++ {
		if ok() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timeout esperando %s", what)
}

// run executes the CLI from the repo and returns stdout, stderr and exit code.
func (e *env) run(args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "HOME="+e.home, "SSH_AUTH_SOCK=", "NO_COLOR=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func (e *env) expectExit(want int, args ...string) string {
	e.t.Helper()
	out, errs, code := e.run(args...)
	if code != want {
		e.t.Fatalf("madeploy %s → exit %d, esperado %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, want, out, errs)
	}
	return out + errs
}

func (e *env) ok(args ...string) string { e.t.Helper(); return e.expectExit(0, args...) }

func (e *env) git(args ...string) {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (e *env) commit(msg string) {
	e.git("add", "-A")
	e.git("commit", "-qm", msg)
}

func (e *env) server(cmd string) string {
	return sh(e.t, "docker", "exec", "-u", "deploy", e.id+"-app", "sh", "-c", cmd)
}

// sql returns stdout only (the mysql client warns about -p on stderr).
// asRoot runs a command in the app container as root (what the operator does by hand).
func (e *env) asRoot(cmd string) string {
	return sh(e.t, "docker", "exec", e.id+"-app", "sh", "-c", cmd)
}

func TestReleasesEndToEnd(t *testing.T) {
	e := setup(t)
	e.ok("init", "it", "--pull-to", e.repo)
	e.git("init", "-q", "-b", "main")
	e.commit("seed")

	t.Log("F2: migrate-layout sem downtime; o DocumentRoot é trocado pelo operador")
	mustContain(t, e.ok("init", "it", "--migrate-layout", "--yes"), "DocumentRoot", "/var/www/html/current")
	mustContain(t, e.page("LoginForm v1"), "LoginForm v1") // still served by the old tree
	e.asRoot(`sed -i 's#DocumentRoot /var/www/html$#DocumentRoot /var/www/html/current#' /etc/apache2/sites-available/000-default.conf && apache2ctl graceful`)
	cfg, _ := os.ReadFile(filepath.Join(e.repo, "madeploy.yaml"))
	put(t, e.repo, "madeploy.yaml", strings.Replace(string(cfg), "    path: /var/www/html\n", "    path: /var/www/html\n    strategy: releases\n", 1))
	e.commit("strategy releases")
	e.server("echo enviado-no-meio > /var/www/html/tmp/durante.txt")
	e.ok("init", "it", "--migrate-layout", "--finish", "--yes")
	if e.server("cat /var/www/html/shared/tmp/durante.txt") != "enviado-no-meio" {
		t.Fatal("--finish não copiou o upload gravado no meio tempo")
	}
	mustContain(t, e.page("LoginForm v1"), "LoginForm v1")
	mustContain(t, e.ok("deploy", "it", "--yes"), "nada a fazer")
	first := e.server("readlink /var/www/html/current")

	t.Log("F2: release nova com hard links, troca atômica, shared intacto")
	put(t, e.repo, "index.php", `<?php echo "LoginForm r2";`)
	e.commit("r2")
	e.ok("deploy", "it", "--yes")
	mustContain(t, e.page("r2"), "LoginForm r2")
	second := e.server("readlink /var/www/html/current")
	if second == first {
		t.Fatal("current não mudou")
	}
	if links := e.server("stat -c %h /var/www/html/" + second + "/app/control/PacienteList.php"); links == "1" {
		t.Fatal("arquivo não alterado deveria ser hard link da release anterior (cp -al)")
	}
	if e.server("cat /var/www/html/"+first+"/index.php") != `<?php echo "LoginForm v1";` {
		t.Fatal("a release anterior foi alterada")
	}
	mustContain(t, e.server("cat /var/www/html/current/app/config/sisvita.php"), "app-s3cret")
	if e.server("cat /var/www/html/current/app/output/relatorio.pdf") != "pdf" {
		t.Fatal("app/output não está compartilhado")
	}

	t.Log("F2: healthcheck falha → current volta sozinho")
	put(t, e.repo, "index.php", `<?php nao_existe(); echo "LoginForm";`)
	e.commit("quebra")
	mustContain(t, e.expectExit(1, "deploy", "it", "--yes"), "current voltou para")
	if e.server("readlink /var/www/html/current") != second {
		t.Fatal("current não voltou para a release anterior")
	}
	mustContain(t, e.page("r2"), "LoginForm r2")

	t.Log("F2: rollback para a release inicial")
	e.git("revert", "--no-edit", "HEAD")
	mustContain(t, e.ok("releases", "it"), "* ")
	e.ok("rollback", "it", "--yes")
	if e.server("readlink /var/www/html/current") != first {
		t.Fatal("rollback não apontou current para a primeira release")
	}
	mustContain(t, e.page("LoginForm v1"), "LoginForm v1")

	t.Log("F5: a TUI abre num terminal real, carrega o painel pelo SSH e sai com q")
	e.tuiSmoke()
}

// tuiSmoke runs `madeploy tui` under a pseudo-terminal (script(1)), waits for
// the dashboard loaded over SSH, switches to the Releases screen and quits.
func (e *env) tuiSmoke() {
	t := e.t
	args := []string{"-q", "/dev/null", e.bin, "tui", "it"} // BSD script
	if runtime.GOOS == "linux" {
		args = []string{"-qec", e.bin + " tui it", "/dev/null"}
	}
	cmd := exec.Command("script", args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "HOME="+e.home, "SSH_AUTH_SOCK=", "NO_COLOR=1", "TERM=xterm-256color", "COLUMNS=80", "LINES=24")
	in, _ := cmd.StdinPipe()
	var out safeBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Skipf("script(1) indisponível: %v", err)
	}
	wait := func(want string) {
		t.Helper()
		for i := 0; i < 60; i++ {
			if strings.Contains(out.String(), want) {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		_ = cmd.Process.Kill()
		t.Fatalf("TUI não mostrou %q:\n%q", want, out.String())
	}
	wait("implantado")
	_, _ = in.Write([]byte("6"))
	wait("ANTES")
	_, _ = in.Write([]byte("q"))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TUI saiu com erro: %v\n%q", err, out.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("TUI não saiu com q")
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (e *env) sql(q string) string {
	e.t.Helper()
	out, err := exec.Command("docker", "exec", e.id+"-db", "mysql", "-uroot", "-proot", "-N", "-B", "sisvita", "-e", q).Output()
	if err != nil {
		e.t.Fatalf("sql %q: %v", q, err)
	}
	return strings.TrimSpace(string(out))
}

// page waits up to 5s for the page to contain want (apache graceful is asynchronous)
// and returns the last body seen.
func (e *env) page(want ...string) string {
	var body string
	for i := 0; i < 10; i++ {
		resp, err := http.Get("http://127.0.0.1:" + e.httpPort + "/index.php?class=LoginForm")
		if err != nil {
			body = err.Error()
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
		}
		if len(want) == 0 || strings.Contains(body, want[0]) {
			return body
		}
		time.Sleep(500 * time.Millisecond)
	}
	return body
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("saída sem %q:\n%s", w, out)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)

	t.Log("F1: semente a partir de produção, baseline, primeiro deploy é no-op")
	seed := e.ok("init", "it", "--pull-to", e.repo)
	// contagem prévia (GNU find -printf) tem de bater com o que foi baixado
	if want := regexp.MustCompile(`(\d+) arquivo\(s\), [^\n]* a baixar`).FindStringSubmatch(seed); want == nil ||
		!strings.Contains(seed, want[1]+" arquivo(s), ") || !regexp.MustCompile(want[1]+` arquivo\(s\), [^\n]* baixados`).MatchString(seed) {
		t.Fatalf("contagem do pull não bate com o download:\n%s", seed)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "app/config/sisvita.php")); err == nil {
		t.Fatal("init --pull-to trouxe a config de produção")
	}
	if _, err := os.Stat(filepath.Join(e.repo, "app/control/PacienteList.php")); err != nil {
		t.Fatal("init --pull-to não trouxe o código")
	}
	e.git("init", "-q", "-b", "main")
	e.commit("seed a partir de produção")
	mustContain(t, e.ok("deploy", "it", "--yes"), "nada a fazer")

	t.Log("F1: tela nova + item de menu + página alterada")
	put(t, e.repo, "index.php", `<?php echo "LoginForm v2";`)
	put(t, e.repo, "app/control/AgendaForm.php", "<?php\nclass AgendaForm extends TPage {}\n")
	put(t, e.repo, "menu.xml", `<?xml version="1.0"?><menu><menuitem label="Pacientes"><action>PacienteList#method=onShow</action></menuitem><menuitem label="Agenda"><action>AgendaForm</action></menuitem></menu>`)
	e.commit("agenda")
	mustContain(t, e.ok("deploy", "it", "--dry-run"), "A app/control/AgendaForm.php", "M index.php", "M menu.xml")
	mustContain(t, e.ok("deploy", "it", "--yes"), "hook-ok", "healthcheck", "AgendaForm", "sync programs")
	mustContain(t, e.page("LoginForm v2"), "LoginForm v2")
	if got := e.server("stat -c '%a %G' /var/www/html/app/control/AgendaForm.php"); got != "644 www-data" {
		t.Fatalf("permissão do arquivo novo: %s", got)
	}
	if e.server("cat /var/www/html/app/output/relatorio.pdf") != "pdf" {
		t.Fatal("dados de runtime foram tocados")
	}
	mustContain(t, e.server("cat /var/www/html/app/config/sisvita.php"), "app-s3cret")

	t.Log("F3: db pull pelo túnel (spike do sqldef: views e triggers) e diff vazio")
	e.ok("db", "pull", "it", "--conn", "sisvita")
	for _, f := range []string{"table_pessoa.sql", "view_vw_pessoas.sql", "trigger_pessoa_nome.sql"} {
		if _, err := os.Stat(filepath.Join(e.repo, "database/sisvita/schema", f)); err != nil {
			t.Fatalf("db pull não gerou %s: %v", f, err)
		}
	}
	e.commit("schema de produção")
	mustContain(t, e.ok("db", "diff", "it", "--conn", "sisvita"), "sisvita: 0 instrução")

	t.Log("F3: mudança aditiva → migração → aplicada pelo deploy; F4: tela nova registrada")
	schema := filepath.Join(e.repo, "database/sisvita/schema/table_pessoa.sql")
	raw, _ := os.ReadFile(schema)
	withCol := strings.Replace(string(raw), "`deleted_at` datetime DEFAULT NULL,", "`deleted_at` datetime DEFAULT NULL,\n  `apelido` varchar(60) DEFAULT NULL,", 1)
	if withCol == string(raw) {
		t.Fatalf("formato inesperado do export:\n%s", raw)
	}
	put(t, e.repo, "database/sisvita/schema/table_pessoa.sql", withCol)
	mustContain(t, e.ok("db", "diff", "it", "--conn", "sisvita", "--name", "add apelido"), "`apelido`", "[SAFE]", "salvo em")
	mustContain(t, e.ok("sync", "programs", "it"), "+ AgendaForm", "MAX(id)+1", "migração de dados gerada")
	e.ok("sync", "programs", "it") // rerun before deploying must reuse the same file
	if n, _ := filepath.Glob(filepath.Join(e.repo, "database/permission/data/*_sync_programs.sql")); len(n) != 1 {
		t.Fatalf("sync repetido gerou %d arquivos", len(n))
	}
	e.commit("apelido + programas")
	mustContain(t, e.ok("deploy", "it", "--dry-run"), "Migrações", "add_apelido", "sync_programs")
	e.ok("deploy", "it", "--yes")
	if e.sql("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='sisvita' AND TABLE_NAME='pessoa' AND COLUMN_NAME='apelido'") != "1" {
		t.Fatal("coluna apelido não criada")
	}
	if got := e.sql("SELECT CONCAT(p.id, ':', g.name) FROM system_program p JOIN system_group_program gp ON gp.system_program_id = p.id JOIN system_group g ON g.id = gp.system_group_id WHERE p.controller = 'AgendaForm'"); got != "2:Admin" {
		t.Fatalf("AgendaForm deveria ter id 2 no grupo Admin: %q", got)
	}
	if got := e.sql("SELECT COUNT(*) FROM madeploy_migrations WHERE status = 'applied'"); got != "2" {
		t.Fatalf("madeploy_migrations aplicadas: %s", got)
	}
	if e.server("ls /var/www/html/.madeploy/backups/*/db-sisvita.sql.gz | wc -l") == "0" {
		t.Fatal("backup do banco não gerado")
	}
	if strings.Contains(e.server("ls -a /var/www/html/.madeploy/backups/*/"), ".cnf") {
		t.Fatal("arquivo de credenciais do mysqldump ficou no servidor")
	}
	mustContain(t, e.ok("sync", "programs", "it"), "sem system_program: 0")

	t.Log("F3: mudança destrutiva fica fora do plano sem autorização explícita")
	put(t, e.repo, "database/sisvita/schema/table_pessoa.sql", string(raw))
	mustContain(t, e.ok("db", "diff", "it", "--conn", "sisvita"), "fora do plano")
	mustContain(t, e.ok("db", "diff", "it", "--conn", "sisvita", "--allow-destructive", "--name", "drop apelido"), "DESTRUCTIVE")
	e.commit("remove apelido")
	mustContain(t, e.expectExit(2, "deploy", "it", "--yes"), "--allow-destructive")
	e.ok("deploy", "it", "--yes", "--allow-destructive")

	t.Log("F3: falha no meio de uma migração")
	put(t, e.repo, "database/sisvita/data/20991231000000_quebrada.sql", "INSERT INTO pessoa (nome) VALUES ('Beltrano');\nINSERT INTO nao_existe VALUES (1);\n")
	e.commit("migração quebrada")
	mustContain(t, e.expectExit(1, "deploy", "it", "--yes"), "instrução 2 de 2 falhou", "nenhum arquivo da aplicação foi alterado")
	mustContain(t, e.ok("db", "status", "it", "--conn", "sisvita"), "partial", "1 de 2")
	mustContain(t, e.expectExit(1, "deploy", "it", "--yes"), "db resolve")
	e.ok("db", "resolve", "it", "data/20991231000000_quebrada.sql", "--conn", "sisvita", "--yes")
	e.ok("deploy", "it", "--yes")

	t.Log("F1: drift bloqueia, drift pull traz para o repositório, overwrite resolve")
	e.server(`echo '<?php echo "LoginForm hotfix";' > /var/www/html/index.php`)
	put(t, e.repo, "index.php", `<?php echo "LoginForm v3";`)
	e.commit("v3")
	mustContain(t, e.expectExit(2, "deploy", "it", "--yes"), "modified", "index.php")
	e.ok("drift", "pull", "it")
	if b, _ := os.ReadFile(filepath.Join(e.repo, "index.php")); !strings.Contains(string(b), "hotfix") {
		t.Fatal("drift pull não trouxe o arquivo do servidor")
	}
	e.git("checkout", "--", "index.php")
	e.ok("deploy", "it", "--yes", "--on-drift=overwrite")
	mustContain(t, e.page("v3"), "v3")

	t.Log("F1: falha de healthcheck restaura os arquivos sozinha")
	put(t, e.repo, "index.php", `<?php nao_existe(); echo "LoginForm";`)
	e.commit("quebra em runtime")
	out := e.expectExit(1, "deploy", "it", "--yes")
	mustContain(t, out, "restaurados", "banco NÃO é revertido")
	if !strings.Contains(e.page("v3"), "v3") {
		t.Fatalf("produção não voltou para v3: %s\nservidor: %s\ndeploy:\n%s", e.page(), e.server("cat /var/www/html/index.php"), out)
	}

	t.Log("F1: rollback manual do último deploy bom")
	e.git("revert", "--no-edit", "HEAD")
	releases := e.ok("releases", "it")
	rb := e.ok("rollback", "it", "--yes")
	if !strings.Contains(e.page("hotfix"), "hotfix") {
		t.Fatalf("rollback não voltou para o hotfix: %s\nreleases:\n%s\nrollback:\n%s\nbackups: %s", e.page(), releases, rb,
			e.server("ls /var/www/html/.madeploy/backups/"))
	}
	mustContain(t, e.ok("logs", "it"), `"step":"rollback"`, "código revertido")

	t.Log("lock, doctor, logs")
	e.server("mkdir /var/www/html/.madeploy/deploy.lock")
	e.expectExit(5, "deploy", "it", "--yes")
	e.ok("unlock", "it", "--yes")
	doc := e.ok("doctor", "it")
	mustContain(t, doc, "[builder] tem token")
	if strings.Contains(doc, "abc123") {
		t.Fatal("doctor mostrou o token")
	}
	if all := e.server("cat /var/www/html/.madeploy/logs/*.jsonl"); strings.Contains(all, "app-s3cret") {
		t.Fatal("senha vazou no log")
	}
}
