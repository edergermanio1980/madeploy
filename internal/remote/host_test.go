package remote

import (
	"context"
	"strings"
	"testing"
)

func TestQuoteSurvivesTheShell(t *testing.T) {
	hostile := []string{"plain", "with space", "it's", `"$(rm -rf /)"`, "`id`", "a;b|c&d", "", "*"}
	cmd := Cmd(append([]string{"printf", "%s\n"}, hostile...)...)
	out, err := Run(context.Background(), Local{}, cmd)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(hostile) {
		t.Fatalf("got %q", got)
	}
	for i := range hostile {
		if got[i] != hostile[i] {
			t.Errorf("argumento %d: %q virou %q", i, hostile[i], got[i])
		}
	}
}

func TestExitErrorCarriesStderr(t *testing.T) {
	_, err := Run(context.Background(), Local{}, "echo falhou >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "falhou") {
		t.Fatalf("stderr não reportado: %v", err)
	}
}

func TestResolveParsesTargets(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.ssh/config
	e := resolve("deploy@10.0.0.5:2222")
	if e.user != "deploy" || e.host != "10.0.0.5" || e.port != "2222" {
		t.Fatalf("%+v", e)
	}
	if e = resolve("servidor"); e.port != "22" || e.host != "servidor" {
		t.Fatalf("%+v", e)
	}
}

func TestClientConfigRequiresKnownHosts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := clientConfig(endpoint{host: "x", port: "22"}); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("sem known_hosts deveria recusar: %v", err)
	}
}
