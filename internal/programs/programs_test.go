package programs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestScanCompareAndSQL(t *testing.T) {
	root := t.TempDir()
	put(t, root, "app/control/cadastros/PacienteList.php", "<?php\nclass PacienteList extends TPage {}\n")
	put(t, root, "app/control/cadastros/AgendaForm.php", "<?php\nclass AgendaForm extends TPage {}\n")
	put(t, root, "app/control/base/Base.php", "<?php\nabstract class BaseScreen extends TPage {}\n")
	put(t, root, "app/control/rel/NovoRelatorio.php", "<?php\nclass NovoRelatorio extends TPage {}\n")
	put(t, root, "menu.xml", `<?xml version="1.0"?><menu>
  <menuitem label="Pacientes"><action>PacienteList#method=onShow</action></menuitem>
  <menuitem label="Agenda"><action>AgendaForm</action></menuitem>
  <menuitem label="Sumiu"><action>TelaApagada#method=onShow</action></menuitem>
  <menuitem label="Site"><action>https://example.com</action></menuitem>
</menu>`)

	classes, err := Scan(root, "app/control", nil)
	if err != nil || len(classes) != 3 {
		t.Fatalf("scan: %+v %v (abstract não conta)", classes, err)
	}
	menu, err := MenuActions(root)
	if err != nil || len(menu) != 3 {
		t.Fatalf("menu: %+v %v (URL não conta)", menu, err)
	}
	st := DBState{Programs: map[string]bool{"PacienteList": true, "ProgramaVelho": true}, Groups: map[string]bool{"Admin": true}}
	r := Compare(classes, menu, st, []string{"Admin", "Financeiro"})

	if len(r.New) != 2 || r.New[0].Name != "AgendaForm" || r.New[1].Name != "NovoRelatorio" {
		t.Fatalf("novos: %+v", r.New)
	}
	if strings.Join(r.Orphans, ",") != "ProgramaVelho" {
		t.Fatalf("órfãos: %v", r.Orphans)
	}
	if len(r.MenuWithoutClass) != 1 || r.MenuWithoutClass[0].Controller != "TelaApagada" {
		t.Fatalf("menu sem classe: %+v", r.MenuWithoutClass)
	}
	if len(r.MenuWithoutProgram) != 1 || r.MenuWithoutProgram[0].Controller != "AgendaForm" {
		t.Fatalf("menu sem programa: %+v", r.MenuWithoutProgram)
	}
	if strings.Join(r.NotInMenu, ",") != "NovoRelatorio" || strings.Join(r.MissingGroups, ",") != "Financeiro" {
		t.Fatalf("fora do menu=%v grupos=%v", r.NotInMenu, r.MissingGroups)
	}

	stmts, err := SQL(r.New, map[string]string{"AgendaForm": "Agenda d'Ouro"}, []string{"Admin"}, false)
	if err != nil || len(stmts) != 4 {
		t.Fatalf("sql: %v %v", stmts, err)
	}
	if !strings.Contains(stmts[0], "'Agenda d''Ouro'") || !strings.Contains(stmts[0], "MAX(id)") || !strings.Contains(stmts[0], "NOT EXISTS") {
		t.Fatalf("insert de programa: %s", stmts[0])
	}
	auto, _ := SQL(r.New[:1], nil, []string{"Admin"}, true)
	if strings.Contains(auto[0], "MAX(id)") {
		t.Fatalf("com AUTO_INCREMENT não deve calcular id: %s", auto[0])
	}
	if _, err := SQL([]Class{{Name: "X'; DROP TABLE y; --"}}, nil, nil, false); err == nil {
		t.Fatal("nome de classe inválido deveria ser recusado")
	}
}
