package config

import (
	"strings"
	"testing"
)

func TestParseDefaultsAndValidation(t *testing.T) {
	c, err := Parse([]byte(`
databases:
  sisvita: {}
  permission: {}
environments:
  production: {ssh: prod, path: /var/www/app}
`))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := c.Env("production")
	if e.Keep != 5 || e.Strategy != "inplace" || e.MigrateWhen != "before" || e.Healthcheck.ExpectStatus != 200 {
		t.Fatalf("defaults: %+v", e)
	}
	if c.Databases["sisvita"].Config != "app/config/sisvita.php" || c.Databases["sisvita"].Backup != "tables" {
		t.Fatalf("db defaults: %+v", c.Databases["sisvita"])
	}
	if _, err := c.Env("staging"); err == nil || !strings.Contains(err.Error(), "production") {
		t.Fatalf("erro deveria listar ambientes: %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`
permissions:
  default: {file: "644"}
  writable: [../etc]
databases:
  x: {config: /etc/passwd, dump_options: [--defaults-file=/tmp/x]}
environments:
  Prod: {path: relative, strategy: bogus}
`))
	if err == nil {
		t.Fatal("config inválida aceita")
	}
	for _, want := range []string{"nome inválido", "ssh: obrigatório", "path: precisa ser absoluto", "inplace ou releases", "644", "..", "/etc/passwd", "--defaults", "programs.connection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("faltou %q em:\n%v", want, err)
		}
	}
}

func TestUnknownKeysAreErrors(t *testing.T) {
	if _, err := Parse([]byte("environments:\n  p: {ssh: a, path: /x, passwd: oops}\n")); err == nil {
		t.Fatal("chave desconhecida deveria falhar (evita erro de digitação silencioso)")
	}
}
