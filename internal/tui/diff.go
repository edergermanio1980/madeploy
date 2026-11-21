package tui

import "strings"

// maxDiffLines bounds the O(n·m) LCS table.
// ponytail: quadratic LCS, switch to Myers if files above this limit need diffs.
const maxDiffLines = 4000

// LineDiff returns a unified-style line diff of a (production) → b (local):
// " " unchanged, "-" only in production, "+" only in the repository.
func LineDiff(a, b string) []string {
	la, lb := splitLines(a), splitLines(b)
	if len(la) > maxDiffLines || len(lb) > maxDiffLines {
		return []string{"(arquivo grande demais para o diff na TUI: use git diff ou madeploy drift pull)"}
	}
	n, m := len(la), len(lb)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if la[i] == lb[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case la[i] == lb[j]:
			out = append(out, " "+la[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "-"+la[i])
			i++
		default:
			out = append(out, "+"+lb[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "-"+la[i])
	}
	for ; j < m; j++ {
		out = append(out, "+"+lb[j])
	}
	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
