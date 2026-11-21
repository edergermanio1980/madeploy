// Package programs registers new Adianti screens in system_program and the
// configured groups, and checks the XML menus against code and database (ADR-09, ADR-10).
package programs

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Class is a screen class found in app/control.
type Class struct {
	Name string `json:"name"`
	File string `json:"file"`
}

var (
	classRe      = regexp.MustCompile(`(?m)^\s*(abstract\s+|final\s+)?class\s+([A-Za-z_]\w*)\s+extends\s+\w+`)
	identifierRe = regexp.MustCompile(`^[A-Za-z_]\w*$`)
)

// Scan finds the non-abstract classes declared under controlDir.
func Scan(root, controlDir string, exclude []string) ([]Class, error) {
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[e] = true
	}
	var out []Class
	err := filepath.WalkDir(filepath.Join(root, controlDir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".php") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, m := range classRe.FindAllStringSubmatch(string(raw), -1) {
			if strings.TrimSpace(m[1]) == "abstract" || skip[m[2]] {
				continue
			}
			out = append(out, Class{Name: m[2], File: filepath.ToSlash(rel)})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("varrendo %s: %w", controlDir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MenuAction is one <action> of a menu file.
type MenuAction struct {
	File       string `json:"file"`
	Controller string `json:"controller"`
}

// MenuActions reads every menu*.xml and top_menu.xml in root.
func MenuActions(root string) ([]MenuAction, error) {
	files, _ := filepath.Glob(filepath.Join(root, "menu*.xml"))
	if top := filepath.Join(root, "top_menu.xml"); fileExists(top) {
		files = append(files, top)
	}
	sort.Strings(files)
	var out []MenuAction
	for _, f := range files {
		acts, err := readMenu(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		out = append(out, acts...)
	}
	return out, nil
}

func readMenu(f string) ([]MenuAction, error) {
	fh, err := os.Open(f)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	dec := xml.NewDecoder(fh)
	var out []MenuAction
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "action" {
			continue
		}
		var v string
		if err := dec.DecodeElement(&v, &se); err != nil {
			return nil, err
		}
		ctrl, _, _ := strings.Cut(strings.TrimSpace(v), "#")
		if identifierRe.MatchString(ctrl) { // skip URLs and empty actions
			out = append(out, MenuAction{File: filepath.Base(f), Controller: ctrl})
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// DBState is what the permission database already has.
type DBState struct {
	Programs      map[string]bool // controller → exists
	Groups        map[string]bool // group name → exists
	AutoIncrement bool            // system_program.id has AUTO_INCREMENT (ADR 0015 applied)
}

// LoadDB reads system_program, system_group and the id strategy.
func LoadDB(ctx context.Context, db *sql.DB, schema string) (DBState, error) {
	st := DBState{Programs: map[string]bool{}, Groups: map[string]bool{}}
	load := func(q string, into map[string]bool) error {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				return err
			}
			into[v.String] = true
		}
		return rows.Err()
	}
	if err := load("SELECT controller FROM system_program", st.Programs); err != nil {
		return st, fmt.Errorf("lendo system_program (a conexão aponta para o banco certo? Q4): %w", err)
	}
	if err := load("SELECT name FROM system_group", st.Groups); err != nil {
		return st, fmt.Errorf("lendo system_group: %w", err)
	}
	var extra string
	err := db.QueryRowContext(ctx, `SELECT EXTRA FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'system_program' AND COLUMN_NAME = 'id'`, schema).Scan(&extra)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.AutoIncrement = strings.Contains(strings.ToLower(extra), "auto_increment")
	return st, nil
}

// Report is the result of comparing code, menus and database.
type Report struct {
	New                []Class      `json:"new"`                  // classes without system_program
	Orphans            []string     `json:"orphans"`              // programs without class (only reported)
	MenuWithoutClass   []MenuAction `json:"menu_without_class"`   // menu points to a class that does not exist
	MenuWithoutProgram []MenuAction `json:"menu_without_program"` // menu item hidden: no program row
	NotInMenu          []string     `json:"not_in_menu"`          // new screens no menu reaches (warning)
	MissingGroups      []string     `json:"missing_groups"`
	AutoIncrement      bool         `json:"auto_increment"`
}

// Compare builds the report.
func Compare(classes []Class, menu []MenuAction, st DBState, groups []string) Report {
	r := Report{AutoIncrement: st.AutoIncrement}
	known := map[string]bool{}
	for _, c := range classes {
		known[c.Name] = true
		if !st.Programs[c.Name] {
			r.New = append(r.New, c)
		}
	}
	for p := range st.Programs {
		if p != "" && !known[p] {
			r.Orphans = append(r.Orphans, p)
		}
	}
	sort.Strings(r.Orphans)
	inMenu := map[string]bool{}
	for _, a := range menu {
		inMenu[a.Controller] = true
		switch {
		case !known[a.Controller]:
			r.MenuWithoutClass = append(r.MenuWithoutClass, a)
		case !st.Programs[a.Controller]:
			r.MenuWithoutProgram = append(r.MenuWithoutProgram, a)
		}
	}
	for _, c := range r.New {
		if !inMenu[c.Name] {
			r.NotInMenu = append(r.NotInMenu, c.Name)
		}
	}
	for _, g := range groups {
		if !st.Groups[g] {
			r.MissingGroups = append(r.MissingGroups, g)
		}
	}
	return r
}

func quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// SQL generates idempotent statements that register the new programs and link
// them to the groups. Like the MadBuilder (BuilderPermissionUpdate.php:117-127),
// actions stay NULL (every method allowed) and nothing is ever deleted.
//
// ponytail: MAX(id)+1 races with a concurrent insert by the PHP app within one
// statement; apply sv:ops/schema/001-auto-increment-system.sql to remove the race.
func SQL(newProgs []Class, names map[string]string, groups []string, autoInc bool) ([]string, error) {
	var out []string
	for _, c := range newProgs {
		if !identifierRe.MatchString(c.Name) {
			return nil, fmt.Errorf("nome de classe inválido: %q", c.Name)
		}
		name := c.Name
		if n := names[c.Name]; n != "" {
			name = n
		}
		ctrl := quote(c.Name)
		notExists := " WHERE NOT EXISTS (SELECT 1 FROM system_program x WHERE x.controller = " + ctrl + ")"
		if autoInc {
			out = append(out, "INSERT INTO system_program (name, controller) SELECT "+quote(name)+", "+ctrl+" FROM DUAL"+notExists)
		} else {
			out = append(out, "INSERT INTO system_program (id, name, controller) SELECT m.next_id, "+quote(name)+", "+ctrl+
				" FROM (SELECT COALESCE(MAX(id), 0) + 1 AS next_id FROM system_program) m"+notExists)
		}
		for _, g := range groups {
			join := "system_group g JOIN system_program p ON p.controller = " + ctrl +
				" WHERE g.id = (SELECT MIN(id) FROM system_group WHERE name = " + quote(g) + ")" +
				" AND NOT EXISTS (SELECT 1 FROM system_group_program gp WHERE gp.system_group_id = g.id AND gp.system_program_id = p.id)"
			if autoInc {
				out = append(out, "INSERT INTO system_group_program (system_group_id, system_program_id) SELECT g.id, p.id FROM "+join)
			} else {
				out = append(out, "INSERT INTO system_group_program (id, system_group_id, system_program_id) SELECT m.next_id, g.id, p.id"+
					" FROM (SELECT COALESCE(MAX(id), 0) + 1 AS next_id FROM system_group_program) m CROSS JOIN "+join)
			}
		}
	}
	return out, nil
}
