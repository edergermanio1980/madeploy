package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vitalsoft/madeploy/internal/remote"
)

// finding is one problem found by doctor, with the command that fixes it.
type finding struct {
	Level string `json:"level"` // error, warn, info
	What  string `json:"what"`
	Fix   string `json:"fix,omitempty"`
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor <ambiente>",
		Short: "Verifica permissões, lock, atualizador do MadBuilder e rotas expostas (só leitura)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := connect(args[0])
			if err != nil {
				return err
			}
			defer a.close()
			fs, err := diagnose(cmd, a)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(fs)
			}
			for _, f := range fs {
				fmt.Printf("[%s] %s\n", strings.ToUpper(f.Level), f.What)
				if f.Fix != "" {
					fmt.Printf("        correção: %s\n", f.Fix)
				}
			}
			return nil
		},
	}
}

func diagnose(cmd *cobra.Command, a *app) ([]finding, error) {
	ctx := cmd.Context()
	base := a.d.Env.Path
	cd := "cd " + remote.Quote(base) + " && "
	lines := func(c string) ([]string, error) {
		b, err := remote.Run(ctx, a.host, cd+c)
		if err != nil {
			return nil, err
		}
		var res []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				res = append(res, strings.TrimPrefix(l, "./"))
			}
		}
		return res, nil
	}
	var out []finding

	ww, err := lines("find . -path ./.madeploy -prune -o ! -type l -perm -0002 -print | head -n 50")
	if err != nil {
		return nil, err
	}
	if len(ww) > 0 {
		out = append(out, finding{"error", fmt.Sprintf("%d item(ns) graváveis por qualquer usuário (777/666), ex.: %s", len(ww), strings.Join(first(ww, 5), ", ")),
			remote.Cmd("chmod", "-R", "o-w", "--", base)})
	}
	cfgOpen, err := lines("if [ -d app/config ]; then find app/config -type f -perm -0004 -print; fi")
	if err != nil {
		return nil, err
	}
	if len(cfgOpen) > 0 {
		out = append(out, finding{"error", "config com credenciais legível por todos: " + strings.Join(first(cfgOpen, 5), ", "),
			cd + "chmod o-rwx app/config/*"})
	}
	if grp := a.cfg.Permissions.Group; grp != "" {
		for _, dir := range a.cfg.Permissions.Writable {
			d := remote.Quote(dir)
			res, err := lines("if [ ! -d " + d + " ]; then echo ausente; elif [ -z \"$(find " + d + " -maxdepth 0 -perm -0020)\" ] && ! getfacl -p " + d +
				" 2>/dev/null | grep -q " + remote.Quote("^group:"+grp+":rw") + "; then echo sem-escrita; fi")
			if err != nil {
				return nil, err
			}
			if len(res) > 0 {
				out = append(out, finding{"warn", fmt.Sprintf("%s: %s para o grupo %s", dir, res[0], grp), "madeploy deploy " + a.env + " --only perms"})
			}
		}
	}
	if raw, err := a.host.ReadFile(base + "/app/config/application.ini"); err == nil && builderToken(string(raw)) {
		out = append(out, finding{"warn", "application.ini [builder] tem token: o atualizador da nuvem do MadBuilder pode reescrever código, menu e permissões em produção (gera drift)",
			"remova o token da seção [builder] depois de adotar o madeploy (ADR-11)"})
	}
	if raw, err := a.host.ReadFile(a.d.MD("deploy.lock", "info.json")); err == nil {
		out = append(out, finding{"warn", "lock ativo: " + strings.TrimSpace(string(raw)), "madeploy unlock " + a.env})
	}
	exposed, err := lines("for f in db-mad-manager.php install.php; do if [ -f \"$f\" ]; then echo \"$f\"; fi; done")
	if err != nil {
		return nil, err
	}
	for _, f := range exposed {
		out = append(out, finding{"info", f + " está publicado (risco R4 do plano; o deploy não altera)", ""})
	}
	if ver, err := remote.Run(ctx, a.host, remote.Cmd(a.d.Env.PHP, "-r", "echo PHP_VERSION;")); err != nil {
		out = append(out, finding{"error", "php remoto (" + a.d.Env.PHP + ") não executa: o lint remoto vai falhar", "ajuste environments." + a.env + ".php"})
	} else {
		out = append(out, finding{"info", "PHP remoto " + strings.TrimSpace(string(ver)), ""})
	}
	return out, nil
}

// builderToken reports a non-empty token in the [builder] section without returning it.
func builderToken(ini string) bool {
	section := ""
	sc := bufio.NewScanner(strings.NewReader(ini))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			section = strings.Trim(l, "[]")
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if ok && section == "builder" && strings.TrimSpace(k) == "token" && strings.Trim(strings.TrimSpace(v), `"'`) != "" {
			return true
		}
	}
	return false
}

func first(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
