package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/vitalsoft/madeploy/internal/deploy"
)

// renderPlan prints the full plan; this exact text is covered by a golden file.
func renderPlan(w io.Writer, p *deploy.Plan) {
	fmt.Fprintf(w, "PLANO — %s (%s)\n", p.Environment, p.Path)
	fmt.Fprintf(w, "  commit: %s → %s (%s)\n", short(p.DeployedCommit), short(p.Commit), p.Branch)
	if p.Baseline {
		fmt.Fprintln(w, "  primeiro deploy após o baseline do init")
	}
	renderFiles(w, p)
	section(w, "Migrações", p.Migrations)
	var perms []string
	for _, op := range p.Permissions {
		perms = append(perms, op.Desc)
	}
	section(w, "Permissões", perms)
	section(w, "Hooks antes da troca", p.BeforeSwitch)
	section(w, "Hooks depois da troca", p.AfterSwitch)
	if p.Healthcheck != "" {
		fmt.Fprintf(w, "\nHealthcheck: %s\n", p.Healthcheck)
	}
	section(w, "Avisos", p.Warnings)
}

func renderFiles(w io.Writer, p *deploy.Plan) {
	fmt.Fprintf(w, "\nArquivos (%d a enviar, %d a remover)\n", len(p.Files.Upload), len(p.Files.Delete))
	for _, f := range p.Files.Upload {
		if f.From != "" {
			fmt.Fprintf(w, "  %s %s ← %s\n", f.Status, f.Path, f.From)
			continue
		}
		fmt.Fprintf(w, "  %s %s\n", f.Status, f.Path)
	}
	for _, f := range p.Files.Delete {
		fmt.Fprintf(w, "  D %s\n", f)
	}
	if len(p.Files.Upload)+len(p.Files.Delete) == 0 {
		fmt.Fprintln(w, "  (nada)")
	}
	if len(p.Files.Drift) > 0 {
		fmt.Fprintf(w, "\n!! DRIFT: %d arquivo(s) alterado(s) no servidor fora do madeploy\n", len(p.Files.Drift))
		for _, d := range p.Files.Drift {
			fmt.Fprintf(w, "  %-8s %s\n", d.Kind, d.Path)
		}
	}
}

func section(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n  %s\n", title, strings.Join(items, "\n  "))
}
