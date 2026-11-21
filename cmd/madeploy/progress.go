package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vitalsoft/madeploy/internal/files"
)

// progressLine renders Pull progress on stderr: one self-updating line on a
// terminal, a line every 5s otherwise (logs, CI). Silent with --json.
func progressLine() func(files.Progress) {
	if g.json {
		return nil
	}
	tty := isTTY(os.Stderr)
	every := 5 * time.Second
	if tty {
		every = 150 * time.Millisecond
	}
	start := time.Now()
	var last time.Time
	return func(p files.Progress) {
		now := time.Now()
		if !p.Done && now.Sub(last) < every {
			return
		}
		last = now
		line := progressText(p, now.Sub(start))
		switch {
		case tty && p.Done:
			fmt.Fprint(os.Stderr, "\r\x1b[K"+line+"\n")
		case tty:
			fmt.Fprint(os.Stderr, "\r\x1b[K"+line)
		case !p.Done:
			fmt.Fprintln(os.Stderr, line)
		}
	}
}

// progressText: "⇣ 120/5678 arquivos · 1.2 MB/80.0 MB · 1% · 600.0 KB/s · restam ~2m10s · app/x.php".
func progressText(p files.Progress, elapsed time.Duration) string {
	count := fmt.Sprintf("%d", p.Files)
	size := files.HumanBytes(p.Bytes)
	if p.TotalFiles > 0 {
		count += fmt.Sprintf("/%d", p.TotalFiles)
	}
	parts := []string{"⇣ " + count + " arquivos"}
	if p.TotalBytes > 0 {
		size += "/" + files.HumanBytes(p.TotalBytes)
		parts = append(parts, size, fmt.Sprintf("%d%%", min(100, p.Bytes*100/p.TotalBytes)))
	} else {
		parts = append(parts, size)
	}
	if secs := elapsed.Seconds(); secs >= 1 && p.Bytes > 0 {
		parts = append(parts, files.HumanBytes(int64(float64(p.Bytes)/secs))+"/s")
		if left := p.TotalBytes - p.Bytes; !p.Done && left > 0 && secs >= 2 {
			eta := time.Duration(float64(elapsed) * float64(left) / float64(p.Bytes))
			parts = append(parts, "restam ~"+eta.Round(time.Second).String())
		}
	}
	if p.Done {
		parts = append(parts, "em "+elapsed.Round(time.Second).String())
	} else if p.Name != "" {
		parts = append(parts, shorten(p.Name, 40))
	}
	return strings.Join(parts, " · ")
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n+1:])
	}
	return s
}
