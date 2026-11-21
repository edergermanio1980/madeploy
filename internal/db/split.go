// Package db handles schema diff (sqldef), migrations and database backups.
package db

import (
	"strings"
	"unicode"
)

// SplitSQL splits a script into statements. It understands quotes, comments,
// the DELIMITER directive and BEGIN…END bodies of triggers/procedures, so a
// trigger written without DELIMITER is still one statement.
func SplitSQL(script string) []string {
	var (
		out   []string
		cur   strings.Builder
		delim = []rune(";")
		depth int
	)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" && !onlyComments(s) {
			out = append(out, s)
		}
		cur.Reset()
	}
	rs := []rune(script)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if (i == 0 || rs[i-1] == '\n') && depth == 0 && hasPrefixFold(rs[i:], "DELIMITER ") {
			end := i + indexOf(rs[i:], []rune("\n"))
			flush()
			delim = []rune(strings.TrimSpace(string(rs[i+len("DELIMITER ") : end])))
			i = end
			continue
		}
		end := i
		switch {
		case c == '\'' || c == '"' || c == '`':
			end = skipQuoted(rs, i)
		case c == '#' || (c == '-' && i+1 < len(rs) && rs[i+1] == '-'):
			end = i + indexOf(rs[i:], []rune("\n"))
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			end = i + indexOf(rs[i:], []rune("*/")) + 2
		case isWordStart(rs, i):
			word, next := readWord(rs, i)
			depth = adjustDepth(depth, strings.ToUpper(word), rs, next)
			end = next
		case depth == 0 && hasPrefix(rs[i:], delim):
			flush()
			i += len(delim) - 1
			continue
		}
		if end > len(rs) {
			end = len(rs)
		}
		if end > i {
			cur.WriteString(string(rs[i:end]))
			i = end - 1
			continue
		}
		cur.WriteRune(c)
	}
	flush()
	return out
}

// adjustDepth tracks compound statements: BEGIN and CASE open a block, END
// closes one, and END IF/WHILE/LOOP/REPEAT close blocks that never opened depth.
func adjustDepth(depth int, w string, rs []rune, next int) int {
	switch w {
	case "BEGIN":
		// "BEGIN;" / "BEGIN WORK" start a transaction, not a compound block
		at := skipSpace(rs, next)
		follow, _ := readWord(rs, at)
		if at >= len(rs) || rs[at] == ';' || strings.EqualFold(follow, "WORK") {
			return depth
		}
		return depth + 1
	case "CASE":
		if depth > 0 {
			return depth + 1
		}
	case "END":
		if depth == 0 {
			return 0
		}
		follow, _ := readWord(rs, skipSpace(rs, next))
		switch strings.ToUpper(follow) {
		case "IF", "WHILE", "LOOP", "REPEAT":
			return depth
		}
		return depth - 1
	}
	return depth
}

func onlyComments(s string) bool {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "--") && !strings.HasPrefix(l, "#") {
			return false
		}
	}
	return true
}

func skipQuoted(rs []rune, i int) int {
	q := rs[i]
	for j := i + 1; j < len(rs); j++ {
		if rs[j] == '\\' && q != '`' {
			j++
			continue
		}
		if rs[j] == q {
			if j+1 < len(rs) && rs[j+1] == q { // doubled quote
				j++
				continue
			}
			return j + 1
		}
	}
	return len(rs)
}

func isWordStart(rs []rune, i int) bool {
	if !unicode.IsLetter(rs[i]) {
		return false
	}
	return i == 0 || !isWordRune(rs[i-1])
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func readWord(rs []rune, i int) (string, int) {
	j := i
	for j < len(rs) && isWordRune(rs[j]) {
		j++
	}
	return string(rs[i:j]), j
}

func skipSpace(rs []rune, i int) int {
	for i < len(rs) && unicode.IsSpace(rs[i]) {
		i++
	}
	return i
}

// indexOf returns the offset of sub in rs, or len(rs) when absent.
func indexOf(rs, sub []rune) int {
	for j := 0; j+len(sub) <= len(rs); j++ {
		if hasPrefix(rs[j:], sub) {
			return j
		}
	}
	return len(rs)
}

func hasPrefix(rs, p []rune) bool {
	if len(rs) < len(p) {
		return false
	}
	for k := range p {
		if rs[k] != p[k] {
			return false
		}
	}
	return true
}

func hasPrefixFold(rs []rune, p string) bool {
	pr := []rune(p)
	return len(rs) >= len(pr) && strings.EqualFold(string(rs[:len(pr)]), p)
}
