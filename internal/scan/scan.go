// Package scan finds and parses the JSONL session transcripts Cowork and
// Claude Code write on this machine.
package scan

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultSources lists every place Cowork or Claude Code is known to write
// JSONL transcripts, keeping only those that exist on this machine.
func DefaultSources() []string {
	var out []string
	add := func(p string) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	home, _ := os.UserHomeDir()

	// Claude Code CLI
	if home != "" {
		add(filepath.Join(home, ".claude", "projects"))
	}

	// Cowork, packaged (Microsoft Store / MSIX) install
	if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
		hits, _ := filepath.Glob(filepath.Join(lad, "Packages", "Claude_*",
			"LocalCache", "Roaming", "Claude", "local-agent-mode-sessions"))
		for _, h := range hits {
			add(h)
		}
	}

	// Cowork, regular install
	if ad := os.Getenv("APPDATA"); ad != "" {
		add(filepath.Join(ad, "Claude", "local-agent-mode-sessions"))
	}

	if home != "" {
		// macOS
		add(filepath.Join(home, "Library", "Application Support", "Claude", "local-agent-mode-sessions"))
		// Linux
		add(filepath.Join(home, ".config", "Claude", "local-agent-mode-sessions"))
	}
	return out
}

// FindJSONL walks the sources and returns one path per transcript file name.
// The same session can be mirrored in several places; keep the most recently
// modified copy, like the Python extractor does.
func FindJSONL(sources []string) []string {
	type pick struct {
		path  string
		mtime time.Time
	}
	best := map[string]pick{}
	for _, src := range sources {
		fi, err := os.Stat(src)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if strings.HasSuffix(src, ".jsonl") {
				best[filepath.Base(src)] = pick{src, fi.ModTime()}
			}
			continue
		}
		_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				// Cowork session workspaces hold scratch copies of other
				// sessions' transcripts (mirror_copy, local_sessions, ...)
				// under outputs\. Picking those up mis-tags Claude Code
				// sessions as Cowork, because the copy's path wins the
				// newest-mtime dedup and then the surface classification.
				if d.Name() == "outputs" || d.Name() == "uploads" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			prev, ok := best[d.Name()]
			if !ok || info.ModTime().After(prev.mtime) {
				best[d.Name()] = pick{p, info.ModTime()}
			}
			return nil
		})
	}
	paths := make([]string, 0, len(best))
	for _, v := range best {
		paths = append(paths, v.path)
	}
	sort.Strings(paths)
	return paths
}
