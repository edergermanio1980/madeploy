package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitSQL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"simples", "CREATE TABLE a (id int);\nINSERT INTO a VALUES (1);", 2},
		{"ponto e vírgula em string", "INSERT INTO a VALUES ('x;y'); SELECT 1;", 2},
		{"comentários", "-- um; dois\nSELECT 1; /* a; b */ SELECT 2; # fim;\n", 2},
		{"só comentário", "-- nada aqui;\n", 0},
		{"trigger sem DELIMITER", "CREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN\n SET NEW.x = 1;\n IF NEW.y THEN SET NEW.z = 2; END IF;\nEND;\nSELECT 1;", 2},
		{"trigger com CASE", "CREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN SET NEW.x = CASE WHEN 1 THEN 2 ELSE 3 END; END; SELECT 1;", 2},
		{"DELIMITER", "DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END$$\nDELIMITER ;\nSELECT 3;", 2},
		{"aspas duplicadas", "INSERT INTO a VALUES ('it''s; ok');", 1},
		{"trigger de uma linha", "CREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW SET NEW.c = (SELECT 1);\nSELECT 2;", 2},
		{"transação BEGIN/COMMIT do sqldef", "BEGIN;\nALTER TABLE a ADD b int;\nCOMMIT;", 3},
		{"BEGIN WORK", "BEGIN WORK; SELECT 1; COMMIT;", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitSQL(tt.in); len(got) != tt.want {
				t.Fatalf("got %d statements: %q", len(got), got)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		stmt string
		want string
	}{
		{"CREATE TABLE x (id int)", Safe},
		{"ALTER TABLE `pessoa` ADD COLUMN `apelido` varchar(60) NULL", Safe},
		{"ALTER TABLE `pessoa` ADD COLUMN `ativo` char(1) NOT NULL DEFAULT 'S'", Safe},
		{"ALTER TABLE `pessoa` ADD INDEX `idx_nome` (`nome`)", Safe},
		{"ALTER TABLE `pessoa` ADD COLUMN `cpf` varchar(14) NOT NULL", Destructive},
		{"ALTER TABLE `pessoa` DROP COLUMN `senha`", Destructive},
		{"ALTER TABLE `pessoa` DROP `senha`", Destructive},
		{"ALTER TABLE `pessoa` MODIFY COLUMN `nome` varchar(50)", Warning},
		{"ALTER TABLE `pessoa` DROP INDEX `idx_nome`", Warning},
		{"ALTER TABLE `tb_cnab_remessa` DROP INDEX `uk_tb_cnab_remessa_nsa`", Destructive},
		{"ALTER TABLE `pessoa` DROP FOREIGN KEY `fk_x`", Warning},
		{"DROP TABLE `pessoa`", Destructive},
		{"TRUNCATE TABLE conta", Destructive},
		{"DELETE FROM conta", Destructive},
		{"DELETE FROM conta WHERE id = 1", Safe},
		{"UPDATE conta SET x = 1", Destructive},
		{"DROP VIEW vw_x", Warning},
		{"ALTER TABLE a ADD COLUMN b int NULL, DROP COLUMN c", Destructive},
		// statements read back from a migration file carry comment headers
		{"-- gerado por madeploy\n-- RISCO DESTRUCTIVE: x\nDROP TABLE `madeploy_migrations`", Destructive},
		{"/* x */ TRUNCATE conta", Destructive},
	}
	for _, tt := range tests {
		if got := Classify(tt.stmt); got.Level != tt.want {
			t.Errorf("%s: got %s %v, want %s", tt.stmt, got.Level, got.Reasons, tt.want)
		}
	}
}

func TestTouchedTable(t *testing.T) {
	for stmt, want := range map[string]string{
		"ALTER TABLE `conta` ADD x int":               "conta",
		"INSERT INTO system_program (id) VALUES (1)":  "system_program",
		"UPDATE `pessoa` SET a=1 WHERE id=2":          "pessoa",
		"CREATE INDEX idx_a ON tb_atendimento (data)": "tb_atendimento",
		"CREATE TABLE nova (id int)":                  "",
		"CREATE OR REPLACE VIEW vw AS SELECT 1":       "",
	} {
		if got := TouchedTable(stmt); got != want {
			t.Errorf("%s: got %q want %q", stmt, got, want)
		}
	}
}

func TestParseConnection(t *testing.T) {
	src := []byte(`<?php
return [
    'host' => "localhost",
    'port' => "3306",
    'name' => "sisvita",
    'user' => "app",
    'pass' => 'p\'a"ss',
    'type' => "mysql",
    'prep' => "1",
];`)
	c, err := ParseConnection("sisvita", src)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "127.0.0.1" || c.Port != 3306 || c.DB != "sisvita" || c.User != "app" || c.Pass != `p'a"ss` {
		t.Fatalf("parse errado: %+v", c)
	}
	if strings.Contains(c.String(), "p'a") {
		t.Fatal("String() não pode conter a senha")
	}
	t.Setenv("MADEPLOY_SISVITA_PASS", "do-ambiente")
	if c, _ = ParseConnection("sisvita", src); c.Pass != "do-ambiente" {
		t.Fatal("variável de ambiente deveria sobrescrever a senha")
	}
	if _, err := ParseConnection("x", []byte(`<?php return ['type' => 'pgsql', 'name' => 'a', 'user' => 'b'];`)); err == nil {
		t.Fatal("pgsql deveria ser recusado")
	}
}

func TestParseDryRun(t *testing.T) {
	out := "-- dry run --\nALTER TABLE `pessoa` ADD COLUMN `apelido` varchar(60);\n-- Skipped: ALTER TABLE `pessoa` DROP COLUMN `senha`;\nCREATE TABLE `nova` (\n  `id` int NOT NULL\n);\n"
	stmts, skipped := parseDryRun(out)
	if len(stmts) != 2 || len(skipped) != 1 || !strings.Contains(skipped[0], "DROP COLUMN") {
		t.Fatalf("stmts=%q skipped=%q", stmts, skipped)
	}
}

func TestSchemaFilesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	export := "CREATE TABLE `b` (\n  `id` int\n);\n\nCREATE TABLE `a` (`id` int);\n\nCREATE VIEW `vw_a` AS select 1 AS `x`;\n\nCREATE TRIGGER `t1` BEFORE INSERT ON `a` FOR EACH ROW SET NEW.id = 1;\n"
	n, err := WriteSchema(dir, export)
	if err != nil || n != 4 {
		t.Fatalf("WriteSchema n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "table_a.sql")); err != nil {
		t.Fatal(err)
	}
	sql, err := DesiredSQL(dir)
	if err != nil {
		t.Fatal(err)
	}
	ia, iv, it := strings.Index(sql, "TABLE `a`"), strings.Index(sql, "VIEW"), strings.Index(sql, "TRIGGER")
	if !(ia < iv && iv < it) {
		t.Fatalf("ordem errada (tabelas, views, triggers):\n%s", sql)
	}
}

func TestSaveMigrationMarksRisk(t *testing.T) {
	f, err := SaveMigration(t.TempDir(), "Remove senha!", time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC),
		[]string{"ALTER TABLE `pessoa` DROP COLUMN `senha`"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(f) != "20261118093000_remove_senha.sql" {
		t.Fatalf("nome: %s", f)
	}
	raw, _ := os.ReadFile(f)
	if !strings.Contains(string(raw), "-- RISCO DESTRUCTIVE") {
		t.Fatalf("sem marcação de risco:\n%s", raw)
	}
}

func TestDumpCmdKeepsPasswordOffArgv(t *testing.T) {
	c := Conn{User: "app", Pass: `s3"cr\et`, Host: "127.0.0.1", Port: 3306, DB: "sisvita"}
	cmd := DumpCmd("/x/.my.cnf", c.DB, []string{"pessoa"}, "/x/db.sql")
	if strings.Contains(cmd, "s3") || !strings.Contains(cmd, "--defaults-extra-file=/x/.my.cnf") || !strings.Contains(cmd, "trap") {
		t.Fatalf("comando inseguro: %s", cmd)
	}
	if cnf := MyCnf(c); !strings.Contains(cnf, `password="s3\"cr\\et"`) {
		t.Fatalf("cnf: %s", cnf)
	}
}
