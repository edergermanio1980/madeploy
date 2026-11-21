package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vitalsoft/madeploy/internal/deploy"
	"github.com/vitalsoft/madeploy/internal/files"
	"github.com/vitalsoft/madeploy/internal/perms"
)

type fake struct {
	deploys   int
	rollbacks []string
}

func (f *fake) actions() Actions {
	return Actions{
		Load: func(context.Context) (Snapshot, error) {
			return Snapshot{
				Env: "production", SSH: "sisvita-prod", Path: "/var/www/sisvita", Strategy: "releases",
				Plan: &deploy.Plan{Commit: "5d7ed2faf0aa", Branch: "main",
					Files: files.Delta{
						Upload: []files.FileChange{{Status: "A", Path: "app/control/AgendaForm.php"}, {Status: "M", Path: "index.php"}},
						Delete: []string{"lib/old.js"},
						Drift:  []files.Drift{{Path: "menu.xml", Kind: "modified"}},
					},
					Migrations:  []string{"sisvita: migrations/1_x.sql (pending)"},
					Permissions: []perms.Op{{Desc: "chmod 0644 em 2 arquivo(s)"}}},
				State:    deploy.State{Commit: "510230a327bb", Operator: "eder@mac", DeployedAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)},
				DB:       []ConnDiff{{Conn: "sisvita", Stmts: []Stmt{{SQL: "ALTER TABLE `pessoa` DROP COLUMN `senha`", Level: "destructive", Reasons: []string{"DROP COLUMN"}}}}},
				Releases: []deploy.Meta{{ID: "20261118-100000.000", Current: true}, {ID: "20261117-100000.000"}},
			}, nil
		},
		FileDiff: func(_ context.Context, p string) (string, error) {
			return strings.Join(LineDiff("a\nb\n", "a\nc\n"), "\n"), nil
		},
		Deploy: func(_ context.Context, emit deploy.Emitter, shown *deploy.Plan) error {
			f.deploys++
			return nil
		},
		Rollback: func(_ context.Context, id string, emit deploy.Emitter) error {
			f.rollbacks = append(f.rollbacks, id)
			return nil
		},
	}
}

// drive applies a message and runs the returned command once (synchronously).
func drive(t *testing.T, m tea.Model, msg tea.Msg) tea.Model {
	t.Helper()
	m, cmd := m.Update(msg)
	if cmd != nil {
		if out := cmd(); out != nil {
			if _, quit := out.(tea.QuitMsg); !quit {
				m, _ = m.Update(out)
			}
		}
	}
	return m
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func typeText(m tea.Model, s string) tea.Model {
	for _, r := range s {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func screen(m tea.Model) string { return m.(model).render() }

func checkSize(t *testing.T, m tea.Model, w, h int) {
	t.Helper()
	lines := strings.Split(screen(m), "\n")
	if len(lines) > h {
		t.Fatalf("%d linhas > %d:\n%s", len(lines), h, screen(m))
	}
	for i, l := range lines {
		if lipgloss.Width(l) > w {
			t.Fatalf("linha %d com %d colunas > %d: %q", i, lipgloss.Width(l), w, l)
		}
	}
}

func newTest(t *testing.T) (*fake, tea.Model) {
	t.Setenv("NO_COLOR", "1")
	f := &fake{}
	var m tea.Model = newModel(context.Background(), "production", f.actions(), &sender{})
	m = drive(t, m, m.Init()())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return f, m
}

func TestScreensFit80x24WithoutColor(t *testing.T) {
	_, m := newTest(t)
	for i := 1; i <= 6; i++ {
		m = drive(t, m, key(string(rune('0'+i))))
		checkSize(t, m, 80, 24)
		if strings.Contains(screen(m), "\x1b[") {
			t.Fatalf("tela %d com cor apesar de NO_COLOR", i)
		}
	}
	m = drive(t, m, key("?"))
	checkSize(t, m, 80, 24)
	if !strings.Contains(screen(m), "Teclas") {
		t.Fatal("ajuda não abriu")
	}
	m = drive(t, m, key("x")) // any key closes help
	m = drive(t, m, key("d"))
	checkSize(t, m, 80, 24) // confirmation prompt included
}

func TestDashboardAndFiles(t *testing.T) {
	_, m := newTest(t)
	s := screen(m)
	for _, want := range []string{"510230a327", "1 a remover", "alterado(s) à mão", "1 instrução(ões) destrutiva(s)", "[releases]"} {
		if !strings.Contains(s, want) {
			t.Fatalf("painel sem %q:\n%s", want, s)
		}
	}
	m = drive(t, m, key("2"))
	s = screen(m)
	for _, want := range []string{"! menu.xml", "A app/control/AgendaForm.php", "M index.php", "D lib/old.js"} {
		if !strings.Contains(s, want) {
			t.Fatalf("arquivos sem %q:\n%s", want, s)
		}
	}
	m = drive(t, m, key("/"))
	m = typeText(m, "agenda")
	m = drive(t, m, key("enter"))
	if s = screen(m); strings.Contains(s, "index.php") || !strings.Contains(s, "AgendaForm") {
		t.Fatalf("filtro não aplicado:\n%s", s)
	}
	m = drive(t, m, key("enter")) // diff of the selected file
	if s = screen(m); !strings.Contains(s, "-b") || !strings.Contains(s, "+c") {
		t.Fatalf("diff não exibido:\n%s", s)
	}
	m = drive(t, m, key("esc"))
	if strings.Contains(screen(m), "+c") {
		t.Fatal("esc deveria fechar o diff")
	}
	m = drive(t, m, key("3"))
	m = drive(t, m, key("enter"))
	if s = screen(m); !strings.Contains(s, "DESTRUCTIVE") || !strings.Contains(s, "DROP COLUMN `senha`") {
		t.Fatalf("SQL completo não exibido:\n%s", s)
	}
}

func TestDeployRequiresTypingTheEnvironment(t *testing.T) {
	f, m := newTest(t)
	m = drive(t, m, key("d"))
	if !strings.Contains(screen(m), "digite o nome do ambiente") {
		t.Fatalf("sem confirmação:\n%s", screen(m))
	}
	m = typeText(m, "prod")
	m = drive(t, m, key("enter"))
	if f.deploys != 0 || !strings.Contains(screen(m), "não confere") {
		t.Fatal("deploy rodou com confirmação errada")
	}
	m = drive(t, m, key("d"))
	m = typeText(m, "production")
	m, cmd := m.Update(key("enter"))
	if m.(model).screen != scrExec || !m.(model).running {
		t.Fatal("deveria ir para a tela de execução")
	}
	// events from the core drive the step list and the log
	m, _ = m.Update(eventMsg(deploy.Event{Time: time.Now(), Step: "upload", Level: "info", Msg: "início"}))
	m, _ = m.Update(eventMsg(deploy.Event{Time: time.Now(), Step: "upload", Level: "done", Msg: "ok", Duration: "1s"}))
	m, _ = m.Update(eventMsg(deploy.Event{Time: time.Now(), Step: "switch", Level: "info", Msg: "início"}))
	s := screen(m)
	if !strings.Contains(s, "✓ upload") || !strings.Contains(s, "> switch") || !strings.Contains(s, "1/2 etapas") {
		t.Fatalf("progresso:\n%s", s)
	}
	m = drive(t, m, cmd()) // the deploy command finishes
	if f.deploys != 1 || m.(model).running || !strings.Contains(screen(m), "concluído") {
		t.Fatalf("deploy não concluiu: %d\n%s", f.deploys, screen(m))
	}
}

func TestRollbackFromReleases(t *testing.T) {
	f, m := newTest(t)
	m = drive(t, m, key("6"))
	m = drive(t, m, key("enter")) // current release: nothing happens
	if m.(model).confirm != nil {
		t.Fatal("não deveria oferecer rollback para a release atual")
	}
	m = drive(t, m, key("down"))
	m = drive(t, m, key("enter"))
	m = typeText(m, "production")
	m, cmd := m.Update(key("enter"))
	drive(t, m, cmd())
	if len(f.rollbacks) != 1 || f.rollbacks[0] != "20261117-100000.000" {
		t.Fatalf("rollback: %v", f.rollbacks)
	}
}

func TestCancelIsBetweenSteps(t *testing.T) {
	_, m := newTest(t)
	mm := m.(model)
	cancelled := false
	mm.running, mm.cancel = true, func() { cancelled = true }
	m, _ = mm.Update(key("c"))
	if !cancelled || !strings.Contains(screen(m), "antes da próxima etapa") {
		t.Fatal("c deveria pedir cancelamento entre etapas")
	}
	if _, cmd := m.Update(key("q")); cmd != nil {
		t.Fatal("q não pode sair no meio de um deploy")
	}
}

func TestLineDiff(t *testing.T) {
	if got := strings.Join(LineDiff("a\nb\nc\n", "a\nx\nc\nd\n"), "|"); got != " a|-b|+x| c|+d" {
		t.Fatalf("diff: %s", got)
	}
}
