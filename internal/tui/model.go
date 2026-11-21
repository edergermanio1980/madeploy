// Package tui is the interactive layer over the deploy core (F5). It has no
// deploy logic of its own: every read and action goes through Actions, which
// the CLI wires to the same functions the non-interactive commands use.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/vitalsoft/madeploy/internal/deploy"
)

// Stmt is one SQL statement of a schema diff with its risk.
type Stmt struct {
	SQL     string
	Level   string // safe, warning, destructive
	Reasons []string
}

// ConnDiff is the schema diff of one connection (never saved by the TUI).
type ConnDiff struct {
	Conn    string
	Stmts   []Stmt
	Skipped []string
	Err     string
}

// Snapshot is everything the screens show.
type Snapshot struct {
	Env, SSH, Path, Strategy string
	Plan                     *deploy.Plan
	State                    deploy.State
	Locked                   bool
	DB                       []ConnDiff
	Releases                 []deploy.Meta
}

// Actions connects the TUI to the core.
type Actions struct {
	Load     func(ctx context.Context) (Snapshot, error)
	FileDiff func(ctx context.Context, path string) (string, error)
	// Deploy must refuse if the plan computed under the lock differs from shown.
	Deploy   func(ctx context.Context, emit deploy.Emitter, shown *deploy.Plan) error
	Rollback func(ctx context.Context, id string, emit deploy.Emitter) error
}

const (
	scrDash = iota
	scrFiles
	scrDB
	scrPerms
	scrExec
	scrReleases
	numScreens
)

var screenNames = [numScreens]string{"Painel", "Arquivos", "Banco", "Permissões", "Execução", "Releases"}

type (
	loadedMsg struct {
		snap Snapshot
		err  error
	}
	eventMsg  deploy.Event
	doneMsg   struct{ err error }
	detailMsg struct {
		title, body string
		err         error
	}
)

type stepState struct {
	name, status, duration string // status: wait, run, ok, fail
}

type confirmState struct {
	what  string
	input textinput.Model
	run   func(m *model) tea.Cmd
}

// sender lets actions running in commands push events into the program.
type sender struct{ send func(tea.Msg) }

type model struct {
	ctx     context.Context
	act     Actions
	env     string
	out     *sender
	noColor bool

	w, h    int
	screen  int
	cursor  [numScreens]int
	snap    *Snapshot
	loading bool
	err     error
	help    bool

	filter    textinput.Model
	filtering bool

	detail      bool
	detailTitle string
	vp          viewport.Model

	confirm *confirmState

	running   bool
	cancel    context.CancelFunc
	steps     []stepState
	log       []string
	logvp     viewport.Model
	result    string
	resultErr bool
}

func newModel(ctx context.Context, env string, act Actions, out *sender) model {
	f := textinput.New()
	f.Placeholder = "filtro"
	return model{ctx: ctx, act: act, env: env, out: out, w: 80, h: 24, loading: true, filter: f,
		noColor: os.Getenv("NO_COLOR") != "",
		vp:      viewport.New(viewport.WithWidth(80), viewport.WithHeight(18)),
		logvp:   viewport.New(viewport.WithWidth(80), viewport.WithHeight(10))}
}

func (m model) Init() tea.Cmd { return m.load() }

func (m model) load() tea.Cmd {
	return func() tea.Msg {
		s, err := m.act.Load(m.ctx)
		return loadedMsg{s, err}
	}
}

func (m model) emit(e deploy.Event) {
	if m.out != nil && m.out.send != nil {
		m.out.send(eventMsg(e))
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.SetWidth(m.w)
		m.vp.SetHeight(max(m.bodyHeight()-1, 3))
		m.logvp.SetWidth(m.w)
		m.logvp.SetHeight(max(m.bodyHeight()-len(m.steps)-3, 3))
		return m, nil
	case loadedMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			s := msg.snap
			m.snap = &s
		}
		return m, nil
	case detailMsg:
		m.detail, m.detailTitle = true, msg.title
		body := msg.body
		if msg.err != nil {
			body = "erro: " + msg.err.Error()
		}
		m.vp.SetContent(body)
		m.vp.GotoTop()
		return m, nil
	case eventMsg:
		return m.onEvent(deploy.Event(msg)), nil
	case doneMsg:
		m.running, m.cancel = false, nil
		m.resultErr = msg.err != nil
		m.result = "concluído"
		if msg.err != nil {
			m.result = msg.err.Error()
		}
		m.loading = true
		return m, m.load()
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m model) onEvent(e deploy.Event) model {
	line := fmt.Sprintf("%s %-13s %s", e.Time.Local().Format("15:04:05"), e.Step, e.Msg)
	if e.Duration != "" {
		line += " (" + e.Duration + ")"
	}
	m.log = append(m.log, line)
	idx := -1
	for i := range m.steps {
		if m.steps[i].name == e.Step {
			idx = i
		}
	}
	if idx < 0 {
		m.steps = append(m.steps, stepState{name: e.Step, status: "wait"})
		idx = len(m.steps) - 1
	}
	switch {
	case e.Level == "error":
		m.steps[idx].status = "fail"
	case e.Level == "done":
		m.steps[idx].status, m.steps[idx].duration = "ok", e.Duration
	case e.Msg == "início":
		m.steps[idx].status = "run"
	}
	m.logvp.SetHeight(max(m.bodyHeight()-len(m.steps)-3, 3))
	m.logvp.SetContent(strings.Join(m.log, "\n"))
	m.logvp.GotoBottom()
	return m
}

func (m model) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		if m.running {
			return m.cancelRun(), nil
		}
		return m, tea.Quit
	}
	if m.confirm != nil {
		return m.onConfirmKey(k)
	}
	if m.filtering {
		switch key {
		case "enter", "esc":
			m.filtering = false
			m.filter.Blur()
			m.cursor[scrFiles] = 0
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(k)
		return m, cmd
	}
	if m.detail {
		switch key {
		case "esc", "q", "enter":
			m.detail = false
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return m, cmd
	}
	if m.help {
		m.help = false
		return m, nil
	}
	switch key {
	case "q":
		if m.running {
			m.result = "deploy em andamento: c cancela entre etapas"
			return m, nil
		}
		return m, tea.Quit
	case "?":
		m.help = true
	case "tab", "right", "l":
		m.screen = (m.screen + 1) % numScreens
	case "shift+tab", "left", "h":
		m.screen = (m.screen + numScreens - 1) % numScreens
	case "1", "2", "3", "4", "5", "6":
		m.screen = int(key[0] - '1')
	case "up", "k":
		m.cursor[m.screen] = max(m.cursor[m.screen]-1, 0)
	case "down", "j":
		m.cursor[m.screen] = min(m.cursor[m.screen]+1, max(m.listLen()-1, 0))
	case "r":
		if !m.running {
			m.loading = true
			return m, m.load()
		}
	case "/":
		if m.screen == scrFiles {
			m.filtering = true
			return m, m.filter.Focus()
		}
	case "c":
		if m.running {
			return m.cancelRun(), nil
		}
	case "d":
		return m.askDeploy()
	case "enter":
		return m.open()
	default:
		if m.screen == scrExec {
			var cmd tea.Cmd
			m.logvp, cmd = m.logvp.Update(k)
			return m, cmd
		}
	}
	return m, nil
}

func (m model) cancelRun() model {
	if m.cancel != nil {
		m.cancel()
		m.result = "cancelamento pedido: o deploy para antes da próxima etapa"
	}
	return m
}

// onConfirmKey handles the "type the environment name" prompt.
func (m model) onConfirmKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.confirm = nil
		return m, nil
	case "enter":
		c := m.confirm
		m.confirm = nil
		if strings.TrimSpace(c.input.Value()) != m.env {
			m.result, m.resultErr = "confirmação não confere: nada foi feito", true
			return m, nil
		}
		cmd := c.run(&m)
		return m, cmd
	}
	var cmd tea.Cmd
	m.confirm.input, cmd = m.confirm.input.Update(k)
	return m, cmd
}

func (m model) ask(what string, run func(m *model) tea.Cmd) (tea.Model, tea.Cmd) {
	in := textinput.New()
	in.Placeholder = m.env
	m.confirm = &confirmState{what: what, input: in, run: run}
	return m, m.confirm.input.Focus()
}

func (m model) askDeploy() (tea.Model, tea.Cmd) {
	if m.running || m.snap == nil || m.snap.Plan == nil {
		return m, nil
	}
	if m.snap.Plan.NoOp() {
		m.result, m.resultErr = "nada a fazer: o servidor já está no commit local", false
		return m, nil
	}
	shown := m.snap.Plan
	return m.ask("o deploy em "+m.env, func(mm *model) tea.Cmd {
		return mm.start(func(ctx context.Context) error { return mm.act.Deploy(ctx, mm.emit, shown) })
	})
}

// start switches to the execution screen and runs fn in a command.
func (m *model) start(fn func(ctx context.Context) error) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.running, m.cancel, m.steps, m.log, m.result, m.resultErr = true, cancel, nil, nil, "", false
	m.screen = scrExec
	return func() tea.Msg { return doneMsg{fn(ctx)} }
}

func (m model) open() (tea.Model, tea.Cmd) {
	if m.snap == nil {
		return m, nil
	}
	i := m.cursor[m.screen]
	switch m.screen {
	case scrFiles:
		rows := m.fileRows()
		if i >= len(rows) || rows[i].kind == "D" {
			return m, nil
		}
		p := rows[i].path
		return m, func() tea.Msg {
			body, err := m.act.FileDiff(m.ctx, p)
			return detailMsg{"diff " + p + "  (- produção, + repositório)", body, err}
		}
	case scrDB:
		rows := m.dbRows()
		if i < len(rows) {
			r := rows[i]
			body := r.sql
			if len(r.reasons) > 0 {
				body += "\n\n-- " + strings.Join(r.reasons, "\n-- ")
			}
			m.detail, m.detailTitle = true, r.conn+" · "+strings.ToUpper(r.level)
			m.vp.SetContent(body)
			m.vp.GotoTop()
		}
	case scrReleases:
		rels := m.snap.Releases
		if i >= len(rels) || rels[i].Current || m.running {
			return m, nil
		}
		id := rels[i].ID
		return m.ask("o rollback para "+id, func(mm *model) tea.Cmd {
			return mm.start(func(ctx context.Context) error { return mm.act.Rollback(ctx, id, mm.emit) })
		})
	}
	return m, nil
}

func (m model) bodyHeight() int { return max(m.h-4, 4) }

// Run starts the TUI for one environment and blocks until it exits.
func Run(ctx context.Context, env string, act Actions) error {
	out := &sender{}
	p := tea.NewProgram(newModel(ctx, env, act, out), tea.WithContext(ctx))
	out.send = p.Send
	_, err := p.Run()
	return err
}
