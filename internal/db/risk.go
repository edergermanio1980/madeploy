package db

import (
	"regexp"
	"strings"
)

// Risk levels, ordered.
const (
	Safe        = "safe"
	Warning     = "warning"
	Destructive = "destructive"
)

// Risk is the classification of one SQL statement.
type Risk struct {
	Level   string   `json:"level"`
	Reasons []string `json:"reasons,omitempty"`
}

var (
	dropTableRe  = regexp.MustCompile(`(?is)^\s*DROP\s+(TABLE|DATABASE|SCHEMA)\b`)
	truncateRe   = regexp.MustCompile(`(?is)^\s*TRUNCATE\b`)
	deleteRe     = regexp.MustCompile(`(?is)^\s*DELETE\s+FROM\b`)
	updateRe     = regexp.MustCompile(`(?is)^\s*UPDATE\b`)
	whereRe      = regexp.MustCompile(`(?is)\bWHERE\b`)
	dropObjRe    = regexp.MustCompile(`(?is)^\s*DROP\s+(VIEW|TRIGGER|PROCEDURE|FUNCTION)\b`)
	renameTblRe  = regexp.MustCompile(`(?is)^\s*RENAME\s+TABLE\b`)
	alterRe      = regexp.MustCompile("(?is)^\\s*ALTER\\s+TABLE\\s+`?(\\w+)`?")
	notNullRe    = regexp.MustCompile(`(?is)\bNOT\s+NULL\b`)
	hasDefRe     = regexp.MustCompile(`(?is)\bDEFAULT\b|\bAUTO_INCREMENT\b|\bGENERATED\b`)
	addColRe     = regexp.MustCompile(`(?is)^\s*ADD\s+(COLUMN\s+)?`)
	addNonColRe  = regexp.MustCompile(`(?is)^\s*ADD\s+(INDEX|KEY|UNIQUE|CONSTRAINT|PRIMARY|FOREIGN|FULLTEXT|SPATIAL|CHECK)\b`)
	dropKwRe     = regexp.MustCompile(`(?is)^\s*DROP\s+(INDEX|KEY|FOREIGN|CONSTRAINT|PRIMARY|CHECK)\b`)
	dropColRe    = regexp.MustCompile(`(?is)^\s*DROP\s+(COLUMN\s+)?` + "`?" + `[A-Za-z_]\w*` + "`?" + `\s*$`)
	dropUniqueRe = regexp.MustCompile(`(?is)^\s*DROP\s+(INDEX|KEY)\s+` + "`?" + `(uk_|uniq|unique)`)
	modifyRe     = regexp.MustCompile(`(?is)^\s*(MODIFY|CHANGE)\b`)
	renameToRe   = regexp.MustCompile(`(?is)^\s*RENAME\s+(TO|AS)\b`)
	touchedRe    = regexp.MustCompile("(?is)^\\s*(?:ALTER\\s+TABLE|DROP\\s+TABLE(?:\\s+IF\\s+EXISTS)?|TRUNCATE(?:\\s+TABLE)?|INSERT\\s+(?:IGNORE\\s+)?INTO|REPLACE\\s+INTO|UPDATE|DELETE\\s+FROM|CREATE\\s+(?:UNIQUE\\s+)?INDEX\\s+\\S+\\s+ON)\\s+`?(\\w+)`?")
)

// StripComments removes comment lines and leading /* */ blocks, so anchored
// rules see the statement itself (migration files carry "-- RISCO" headers).
func StripComments(stmt string) string {
	s := strings.TrimSpace(stmt)
	for {
		switch {
		case strings.HasPrefix(s, "--"), strings.HasPrefix(s, "#"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = strings.TrimSpace(s[i+1:])
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = strings.TrimSpace(s[i+2:])
				continue
			}
			return ""
		}
		return s
	}
}

// Classify returns the worst risk found in stmt, with every reason.
func Classify(stmt string) Risk {
	s := StripComments(stmt)
	r := Risk{Level: Safe}
	add := func(level, reason string) {
		if rank(level) > rank(r.Level) {
			r.Level = level
		}
		r.Reasons = append(r.Reasons, reason)
	}
	switch {
	case dropTableRe.MatchString(s):
		add(Destructive, "DROP TABLE apaga a tabela e os dados")
	case truncateRe.MatchString(s):
		add(Destructive, "TRUNCATE apaga todas as linhas")
	case deleteRe.MatchString(s) && !whereRe.MatchString(s):
		add(Destructive, "DELETE sem WHERE")
	case updateRe.MatchString(s) && !whereRe.MatchString(s):
		add(Destructive, "UPDATE sem WHERE")
	case dropObjRe.MatchString(s):
		add(Warning, "remove objeto do esquema")
	case renameTblRe.MatchString(s):
		add(Warning, "renomear tabela quebra o código antigo durante o deploy")
	case alterRe.MatchString(s):
		for _, clause := range alterClauses(s) {
			classifyClause(clause, add)
		}
	}
	return r
}

func classifyClause(c string, add func(string, string)) {
	switch {
	case dropUniqueRe.MatchString(c):
		add(Destructive, "remove índice único (passa a aceitar duplicatas)")
	case dropKwRe.MatchString(c):
		add(Warning, "remove índice/constraint")
	case dropColRe.MatchString(c):
		add(Destructive, "DROP COLUMN apaga os dados da coluna")
	case modifyRe.MatchString(c):
		add(Warning, "mudança de tipo pode truncar ou arredondar dados")
	case addColRe.MatchString(c) && !addNonColRe.MatchString(c) && notNullRe.MatchString(c) && !hasDefRe.MatchString(c):
		add(Destructive, "coluna NOT NULL sem DEFAULT falha (ou zera valores) em tabela com dados")
	case renameToRe.MatchString(c):
		add(Warning, "renomear tabela quebra o código antigo durante o deploy")
	}
}

// alterClauses splits "ALTER TABLE t a, b, c" into its top-level clauses.
func alterClauses(s string) []string {
	loc := alterRe.FindStringIndex(s)
	body := s[loc[1]:]
	var out []string
	depth, start := 0, 0
	var inQ rune
	for i, c := range body {
		switch {
		case inQ != 0:
			if c == inQ {
				inQ = 0
			}
		case c == '\'' || c == '"' || c == '`':
			inQ = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	return append(out, body[start:])
}

// AlteredTable returns the table name of an ALTER TABLE, or "".
func AlteredTable(stmt string) string {
	if m := alterRe.FindStringSubmatch(StripComments(stmt)); m != nil {
		return m[1]
	}
	return ""
}

// TouchedTable returns the table a statement changes (for targeted backups), or "".
func TouchedTable(stmt string) string {
	if m := touchedRe.FindStringSubmatch(StripComments(stmt)); m != nil {
		return m[1]
	}
	return ""
}

func rank(level string) int {
	switch level {
	case Destructive:
		return 2
	case Warning:
		return 1
	}
	return 0
}

// Worse reports whether a is a higher risk than b.
func Worse(a, b string) bool { return rank(a) > rank(b) }
