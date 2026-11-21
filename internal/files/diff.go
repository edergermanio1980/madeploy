package files

import "sort"

// FileChange is a file that will be written to the server.
type FileChange struct {
	Status string `json:"status"` // A (new on server), M (content differs), R (renamed per git)
	Path   string `json:"path"`
	From   string `json:"from,omitempty"`
}

// Drift is a file changed on the server outside madeploy.
type Drift struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // modified, deleted, added
}

// Delta is what a deploy does to files.
type Delta struct {
	Upload []FileChange `json:"upload"`
	Delete []string     `json:"delete"`
	Drift  []Drift      `json:"drift"`
}

func (d Delta) Empty() bool { return len(d.Upload) == 0 && len(d.Delete) == 0 }

// Compute compares the local build with the live server, using the manifest of
// the last deploy to tell our own changes from changes made by hand.
//
//   - upload: local file whose content differs from (or is missing on) the server
//   - delete: file we deployed before, gone locally, still on the server
//   - drift:  live differs from what we deployed AND from what we would deploy
//
// Without a deployed manifest (never initialised) nothing is deleted and no drift is reported.
func Compute(local, deployed, live Manifest, git []Change) Delta {
	renamed := map[string]string{}
	for _, c := range git {
		if c.Status == "R" {
			renamed[c.Path] = c.From
		}
	}
	var d Delta
	for _, p := range local.Paths() {
		l := local[p]
		v, onServer := live[p]
		switch {
		case !onServer:
			d.Upload = append(d.Upload, FileChange{Status: statusFor(p, "A", renamed), Path: p, From: renamed[p]})
		case v.SHA256 != l.SHA256:
			d.Upload = append(d.Upload, FileChange{Status: statusFor(p, "M", renamed), Path: p, From: renamed[p]})
		}
	}
	if deployed == nil {
		return d
	}
	for _, p := range deployed.Paths() {
		_, inLocal := local[p]
		v, onServer := live[p]
		if !inLocal && onServer {
			d.Delete = append(d.Delete, p)
		}
		dep := deployed[p]
		switch {
		case !onServer && inLocal:
			d.Drift = append(d.Drift, Drift{Path: p, Kind: "deleted"})
		case onServer && v.SHA256 != dep.SHA256 && (!inLocal || v.SHA256 != local[p].SHA256):
			d.Drift = append(d.Drift, Drift{Path: p, Kind: "modified"})
		}
	}
	for _, p := range live.Paths() {
		if _, known := deployed[p]; known {
			continue
		}
		if l, inLocal := local[p]; inLocal && l.SHA256 != live[p].SHA256 {
			d.Drift = append(d.Drift, Drift{Path: p, Kind: "added"})
		}
	}
	sort.Slice(d.Drift, func(i, j int) bool { return d.Drift[i].Path < d.Drift[j].Path })
	return d
}

func statusFor(p, def string, renamed map[string]string) string {
	if _, ok := renamed[p]; ok {
		return "R"
	}
	return def
}
