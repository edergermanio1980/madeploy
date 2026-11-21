package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	colGreen  = lipgloss.Color("2")
	colYellow = lipgloss.Color("3")
	colRed    = lipgloss.Color("1")
	colDim    = lipgloss.Color("8")
	colAccent = lipgloss.Color("6")
)

// paint styles s unless NO_COLOR is set.
func (m model) paint(s string, st lipgloss.Style) string {
	if m.noColor {
		return s
	}
	return st.Render(s)
}

func fg(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// fit cuts a plain line to the terminal width.
func fit(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:max(w, 0)])
	}
	return string(r[:w-1]) + "…"
}

type fileRow struct{ kind, path string }

func (m model) fileRows() []fileRow {
	if m.snap == nil || m.snap.Plan == nil {
		return nil
	}
	p := m.snap.Plan
	q := strings.ToLower(m.filter.Value())
	var rows []fileRow
	add := func(kind, path string) {
		if q == "" || strings.Contains(strings.ToLower(path), q) {
			rows = append(rows, fileRow{kind, path})
		}
	}
	for _, d := range p.Files.Drift {
		add("!", d.Path)
	}
	for _, f := range p.Files.Upload {
		add(f.Status, f.Path)
	}
	for _, f := range p.Files.Delete {
		add("D", f)
	}
	return rows
}

type dbRow struct {
	conn, level, sql string
	reasons          []string
}

func (m model) dbRows() []dbRow {
	if m.snap == nil {
		return nil
	}
	var rows []dbRow
	for _, c := range m.snap.DB {
		for _, s := range c.Stmts {
			rows = append(rows, dbRow{c.Conn, s.Level, s.SQL, s.Reasons})
		}
		for _, s := range c.Skipped {
			rows = append(rows, dbRow{c.Conn, "skipped", s, []string{"destrutiva: fora do plano sem --allow-destructive"}})
		}
	}
	return rows
}

func (m model) listLen() int {
	switch m.screen {
	case scrFiles:
		return len(m.fileRows())
	case scrDB:
		return len(m.dbRows())
	case scrReleases:
		if m.snap != nil {
			return len(m.snap.Releases)
		}
	}
	return 0
}

func (m model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "madeploy · " + m.env
	return v
}

func (m model) render() string {
	var b strings.Builder
	head := "madeploy · " + m.env
	if m.snap != nil {
		head += fmt.Sprintf(" · %s:%s [%s]", m.snap.SSH, m.snap.Path, m.snap.Strategy)
	}
	b.WriteString(m.paint(fit(head, m.w), lipgloss.NewStyle().Bold(true)) + "\n")
	b.WriteString(m.tabs() + "\n")
	lines := strings.Split(strings.TrimRight(m.body(), "\n"), "\n")
	h := m.bodyHeight()
	if m.confirm != nil { // the prompt takes two footer lines
		h--
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	b.WriteString(strings.Join(lines, "\n") + "\n\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m model) tabs() string {
	var parts []string
	for i, n := range screenNames {
		label := fmt.Sprintf(" %d %s ", i+1, n)
		if i == m.screen {
			label = m.paint(fmt.Sprintf("[%d %s]", i+1, n), lipgloss.NewStyle().Bold(true).Foreground(colAccent))
		}
		parts = append(parts, label)
	}
	if s := strings.Join(parts, ""); lipgloss.Width(s) <= m.w {
		return s
	}
	parts = parts[:0] // narrow terminal: numbers only, name of the active screen
	for i := range screenNames {
		l := fmt.Sprintf(" %d ", i+1)
		if i == m.screen {
			l = m.paint(fmt.Sprintf("[%d %s]", i+1, screenNames[i]), lipgloss.NewStyle().Bold(true))
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, "")
}

func (m model) footer() string {
	switch {
	case m.confirm != nil:
		return fit("Para confirmar "+m.confirm.what+", digite o nome do ambiente:", m.w) + "\n" + m.confirm.input.View()
	case m.filtering:
		return "/ " + m.filter.View()
	case m.result != "":
		st := fg(colGreen)
		if m.resultErr {
			st = fg(colRed)
		}
		return m.paint(fit(strings.ReplaceAll(m.result, "\n", " · "), m.w), st)
	case m.running:
		return fit("executando… c cancela entre etapas · ↑↓ rola o log · 1-6 telas", m.w)
	}
	return m.paint(fit("1-6/tab telas · ↑↓ mover · enter abrir · / filtro · d deploy · r recarregar · ? ajuda · q sair", m.w), fg(colDim))
}

func (m model) body() string {
	switch {
	case m.help:
		return helpText
	case m.detail:
		return m.paint(fit(m.detailTitle, m.w), lipgloss.NewStyle().Bold(true)) + "\n" + m.vp.View()
	case m.err != nil:
		return m.paint("erro ao carregar: ", fg(colRed)) + m.err.Error() + "\n\nr tenta de novo"
	case m.snap == nil:
		return "carregando…"
	}
	switch m.screen {
	case scrDash:
		return m.dashboard()
	case scrFiles:
		return m.filesView()
	case scrDB:
		return m.dbView()
	case scrPerms:
		return m.permsView()
	case scrExec:
		return m.execView()
	}
	return m.releasesView()
}

const helpText = `Telas
  1 Painel      commit implantado × local, pendências, último deploy
  2 Arquivos    A novo · M alterado · R renomeado · D removido · ! drift
                enter mostra o diff local × produção
  3 Banco       instruções do diff de esquema por risco; enter = SQL
  4 Permissões  o que será ajustado
  5 Execução    etapas e log ao vivo do deploy/rollback
  6 Releases    deploys/releases; enter = rollback

Teclas
  tab ← → 1-6 telas · ↑↓ j k mover · enter abrir · esc voltar
  / filtra arquivos · d deploy · c cancela entre etapas
  r recarrega · q sai

O plano é recalculado sob o lock no deploy; se mudou, o deploy
recusa. Qualquer tecla fecha esta ajuda.`

func (m model) dashboard() string {
	s, p := m.snap, m.snap.Plan
	var b strings.Builder
	row := func(k, v string) { b.WriteString(fmt.Sprintf("%-11s %s\n", k, v)) }
	row("implantado", fit(orDash(s.State.Commit), 12)+" por "+orDash(s.State.Operator)+" em "+when(s.State.DeployedAt))
	if p != nil {
		row("local", fit(p.Commit, 12)+" ("+p.Branch+")")
		row("arquivos", fmt.Sprintf("%d a enviar, %d a remover", len(p.Files.Upload), len(p.Files.Delete)))
		drift := fmt.Sprintf("%d arquivo(s)", len(p.Files.Drift))
		if len(p.Files.Drift) > 0 {
			drift = m.paint(drift+" alterado(s) à mão: deploy bloqueado", fg(colRed))
		}
		row("drift", drift)
		row("migrações", fmt.Sprintf("%d pendente(s)", len(p.Migrations)))
	}
	if s.State.Release != "" {
		row("release", s.State.Release)
	}
	if s.Locked {
		row("lock", m.paint("ATIVO: deploy em andamento ou interrompido", fg(colYellow)))
	}
	destructive := 0
	for _, r := range m.dbRows() {
		if r.level == "destructive" || r.level == "skipped" {
			destructive++
		}
	}
	if destructive > 0 {
		row("banco", m.paint(fmt.Sprintf("%d instrução(ões) destrutiva(s) no esquema", destructive), fg(colRed)))
	}
	if p == nil {
		return b.String()
	}
	for _, w := range p.Warnings {
		b.WriteString(m.paint(fit("! "+w, m.w), fg(colYellow)) + "\n")
	}
	if p.NoOp() {
		b.WriteString("\nnada a fazer: o servidor já está no commit local\n")
	} else {
		b.WriteString("\nd inicia o deploy (o plano completo está nas telas 2 a 4)\n")
	}
	return b.String()
}

// window returns the visible rows [from, to) keeping the cursor on screen.
func (m model) window(n, cursor int) (int, int) {
	h := m.bodyHeight() - 1
	from := 0
	if cursor >= h {
		from = cursor - h + 1
	}
	return from, min(n, from+h)
}

func (m model) filesView() string {
	rows := m.fileRows()
	var b strings.Builder
	title := fmt.Sprintf("%d item(ns)", len(rows))
	if m.filter.Value() != "" {
		title += " · filtro: " + m.filter.Value()
	}
	b.WriteString(m.paint(title, fg(colDim)) + "\n")
	if len(rows) == 0 {
		return b.String() + "nada a enviar"
	}
	cur := min(m.cursor[scrFiles], len(rows)-1)
	styles := map[string]lipgloss.Style{"A": fg(colGreen), "M": fg(colYellow), "R": fg(colAccent), "D": fg(colRed), "!": fg(colRed).Bold(true)}
	from, to := m.window(len(rows), cur)
	for i := from; i < to; i++ {
		r := rows[i]
		b.WriteString(m.paint(fit(fmt.Sprintf("%s %s %s", pointer(i == cur), r.kind, r.path), m.w), styles[r.kind]) + "\n")
	}
	return b.String()
}

func (m model) dbView() string {
	var b strings.Builder
	for _, c := range m.snap.DB {
		if c.Err != "" {
			b.WriteString(m.paint(fit(c.Conn+": "+c.Err, m.w), fg(colRed)) + "\n")
		}
	}
	if m.snap.Plan != nil && len(m.snap.Plan.Migrations) > 0 {
		b.WriteString(fit("migrações pendentes: "+strings.Join(m.snap.Plan.Migrations, ", "), m.w) + "\n")
	}
	rows := m.dbRows()
	if len(rows) == 0 {
		return b.String() + "esquema do repositório = produção (sem instruções)"
	}
	cur := min(m.cursor[scrDB], len(rows)-1)
	tags := map[string]string{"safe": "OK  ", "warning": "ATEN", "destructive": "!!  ", "skipped": "PULA"}
	styles := map[string]lipgloss.Style{"safe": fg(colGreen), "warning": fg(colYellow), "destructive": fg(colRed).Bold(true), "skipped": fg(colRed)}
	from, to := m.window(len(rows), cur)
	for i := from; i < to; i++ {
		r := rows[i]
		line := fit(fmt.Sprintf("%s %s %-10s %s", pointer(i == cur), tags[r.level], r.conn, oneLine(r.sql)), m.w)
		b.WriteString(m.paint(line, styles[r.level]) + "\n")
	}
	return b.String()
}

func (m model) permsView() string {
	if m.snap.Plan == nil || len(m.snap.Plan.Permissions) == 0 {
		return "nenhum ajuste de permissão"
	}
	var b strings.Builder
	for _, op := range m.snap.Plan.Permissions {
		b.WriteString(fit("· "+op.Desc, m.w) + "\n")
	}
	return b.String()
}

func (m model) execView() string {
	if len(m.steps) == 0 && !m.running {
		return "nenhuma execução nesta sessão: d inicia o deploy; enter em Releases faz rollback"
	}
	done := 0
	for _, s := range m.steps {
		if s.status == "ok" {
			done++
		}
	}
	barW := max(min(m.w-20, 40), 10)
	filled := 0
	if len(m.steps) > 0 {
		filled = barW * done / len(m.steps)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[%s%s] %d/%d etapas\n", strings.Repeat("#", filled), strings.Repeat("-", barW-filled), done, len(m.steps)))
	marks := map[string]string{"wait": "·", "run": ">", "ok": "✓", "fail": "✗"}
	styles := map[string]lipgloss.Style{"wait": fg(colDim), "run": fg(colAccent), "ok": fg(colGreen), "fail": fg(colRed)}
	for _, s := range m.steps {
		b.WriteString(m.paint(fit(fmt.Sprintf("%s %-13s %s", marks[s.status], s.name, s.duration), m.w), styles[s.status]) + "\n")
	}
	b.WriteString(m.paint(strings.Repeat("─", m.w), fg(colDim)) + "\n")
	b.WriteString(m.logvp.View())
	return b.String()
}

func (m model) releasesView() string {
	rels := m.snap.Releases
	if len(rels) == 0 {
		return "nenhum deploy com backup/release ainda"
	}
	var b strings.Builder
	b.WriteString(m.paint(fit(fmt.Sprintf("   %-20s %-10s %-10s %s", "ID", "ANTES", "DEPOIS", "OPERADOR"), m.w), fg(colDim)) + "\n")
	cur := min(m.cursor[scrReleases], len(rels)-1)
	from, to := m.window(len(rels), cur)
	for i := from; i < to; i++ {
		r := rels[i]
		mark := " "
		if r.Current {
			mark = "*"
		}
		line := fit(fmt.Sprintf("%s%s %-20s %-10s %-10s %s", pointer(i == cur), mark, r.ID, short(r.CommitBefore), short(r.CommitAfter), r.Operator), m.w)
		if r.Current {
			line = m.paint(line, fg(colGreen))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nenter reverte o código para a linha (o banco não é alterado)")
	return b.String()
}

func pointer(on bool) string {
	if on {
		return "›"
	}
	return " "
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func short(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	return orDash(c)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func when(t time.Time) string {
	if t.IsZero() {
		return "nunca"
	}
	return t.Local().Format("02/01 15:04")
}
