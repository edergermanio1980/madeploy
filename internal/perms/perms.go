// Package perms turns the declarative permissions config into shell commands
// applied only to changed files and to the writable directories.
package perms

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/vitalsoft/madeploy/internal/config"
	"github.com/vitalsoft/madeploy/internal/remote"
)

const chunk = 200 // files per command, keeps us far from ARG_MAX

// Op is one permission action, with a human description for the plan.
type Op struct {
	Desc string `json:"desc"`
	Cmd  string `json:"-"`
}

// FileMode returns the mode a deployed file gets.
func FileMode(p config.Permissions, rel string, executable bool) string {
	for _, glob := range sortedKeys(p.Restricted) {
		if ok, _ := path.Match(glob, rel); ok {
			return p.Restricted[glob]
		}
	}
	for _, glob := range p.Executable {
		if ok, _ := path.Match(glob, rel); ok {
			return "0755"
		}
	}
	if executable {
		return "0755"
	}
	return p.Default.File
}

// Ops builds the commands for the changed files (rel path → has exec bit)
// and for the writable dirs. Every command starts with cd appPath.
func Ops(p config.Permissions, appPath string, changed map[string]bool, sudo bool) []Op {
	cd := "cd " + remote.Quote(appPath) + " && "
	priv := ""
	if sudo {
		priv = "sudo -n "
	}
	var ops []Op

	byMode := map[string][]string{}
	dirs := map[string]bool{}
	for rel, exec := range changed {
		m := FileMode(p, rel, exec)
		byMode[m] = append(byMode[m], rel)
		for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, m := range sortedKeys(byMode) {
		files := byMode[m]
		sort.Strings(files)
		for _, part := range chunks(files) {
			ops = append(ops, Op{
				Desc: fmt.Sprintf("chmod %s em %d arquivo(s)", m, len(part)),
				Cmd:  cd + remote.Cmd(append([]string{"chmod", "--", m}, part...)...),
			})
		}
	}
	dirList := sortedKeys(dirs)
	for _, part := range chunks(dirList) {
		ops = append(ops, Op{
			Desc: fmt.Sprintf("chmod %s em %d diretório(s)", p.Default.Dir, len(part)),
			Cmd:  cd + remote.Cmd(append([]string{"chmod", "--", p.Default.Dir}, part...)...),
		})
	}
	if owner := ownerSpec(p, sudo); owner != "" {
		all := append(sortedKeys(changed), dirList...)
		tool := "chgrp"
		if strings.Contains(owner, ":") {
			tool = "chown"
		}
		for _, part := range chunks(all) {
			ops = append(ops, Op{
				Desc: fmt.Sprintf("%s %s em %d item(ns)", tool, owner, len(part)),
				Cmd:  cd + priv + remote.Cmd(append([]string{tool, "--", owner}, part...)...),
			})
		}
	}
	return append(ops, Writable(p, appPath, sudo)...)
}

// Writable makes the writable dirs exist and group-writable, preferring ACLs.
func Writable(p config.Permissions, appPath string, sudo bool) []Op {
	if p.Group == "" {
		return nil
	}
	cd := "cd " + remote.Quote(appPath) + " && "
	priv := ""
	if sudo {
		priv = "sudo -n "
	}
	acl := fmt.Sprintf("g:%s:rwX", p.Group)
	var ops []Op
	for _, dir := range p.Writable {
		cmd := cd + remote.Cmd("mkdir", "-p", "--", dir) +
			" && if command -v setfacl >/dev/null 2>&1; then " +
			priv + remote.Cmd("setfacl", "-R", "-m", acl, "-m", "d:"+acl, "--", dir) +
			"; else " + priv + remote.Cmd("chgrp", "--", p.Group, dir) +
			" && " + priv + remote.Cmd("chmod", "--", "2775", dir) + "; fi"
		ops = append(ops, Op{Desc: fmt.Sprintf("%s gravável pelo grupo %s (ACL ou setgid 2775)", dir, p.Group), Cmd: cmd})
	}
	return ops
}

// ownerSpec returns "owner:group" when root is available, "group" otherwise
// (a non-root user can only chgrp to a group it belongs to).
func ownerSpec(p config.Permissions, sudo bool) string {
	switch {
	case sudo && p.Owner != "" && p.Group != "":
		return p.Owner + ":" + p.Group
	case p.Group != "":
		return p.Group
	}
	return ""
}

func chunks(s []string) [][]string {
	var out [][]string
	for len(s) > chunk {
		out = append(out, s[:chunk])
		s = s[chunk:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
