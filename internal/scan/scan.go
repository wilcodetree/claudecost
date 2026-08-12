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

	// The two session folder names Claude Desktop has used. Cowork and Chat
	// sessions live in local-agent-mode-sessions; a 2026 update moved Desktop
	// Code sessions to claude-code-sessions (today that folder holds only
	// sidebar state, the Code transcripts land in ~\.claude\projects via the
	// embedded CLI runtime, but scan it anyway in case transcripts follow).
	names := []string{"local-agent-mode-sessions", "claude-code-sessions"}

	for _, name := range names {
		// Packaged (Microsoft Store / MSIX) install. LocalCache\Roaming is
		// the package's view of %APPDATA%, LocalCache\Local of %LOCALAPPDATA%.
		if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
			for _, mapped := range []string{"Roaming", "Local"} {
				hits, _ := filepath.Glob(filepath.Join(lad, "Packages", "Claude_*",
					"LocalCache", mapped, "Claude", name))
				for _, h := range hits {
					add(h)
				}
			}
			// Regular install after the documented Roaming-to-Local move.
			add(filepath.Join(lad, "Claude", name))
		}

		// Regular install, Roaming (the location current builds use).
		if ad := os.Getenv("APPDATA"); ad != "" {
			add(filepath.Join(ad, "Claude", name))
		}

		if home != "" {
			// macOS
			add(filepath.Join(home, "Library", "Application Support", "Claude", name))
			// Linux
			add(filepath.Join(home, ".config", "Claude", name))
		}
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
