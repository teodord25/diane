package main

// GET /fonts/{name}: serves font files to the capture page as web fonts.
//
// Why not just name the fonts in CSS: Firefox's fingerprinting protection
// (on with strict tracking protection, e.g. Betterfox) hides user-installed
// fonts from every web page, localhost included. Web fonts loaded with
// @font-face aren't affected, so diane serves the files itself.
//
// Files are looked up by base name in DIANE_FONT_DIRS (colon-separated,
// searched recursively). Default: ~/.local/share/fonts, ~/.fonts,
// /run/current-system/sw/share/X11/fonts. A missing file is a 404, and the
// page's CSS lists fallbacks, so nothing breaks without them.

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var fontTypes = map[string]string{
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
}

type fontIndex struct {
	dirs []string

	mu      sync.Mutex
	files   map[string]string // base name -> path
	scanned time.Time
}

func newFontIndex() *fontIndex {
	var dirs []string
	if env := os.Getenv("DIANE_FONT_DIRS"); env != "" {
		for _, d := range strings.Split(env, ":") {
			if d = strings.TrimSpace(d); d != "" {
				dirs = append(dirs, d)
			}
		}
	} else {
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, ".local", "share", "fonts"), filepath.Join(home, ".fonts"))
		}
		dirs = append(dirs, "/run/current-system/sw/share/X11/fonts")
	}
	return &fontIndex{dirs: dirs}
}

func (fi *fontIndex) scan() {
	files := map[string]string{}
	for _, root := range fi.dirs {
		// Nix profiles are symlink farms; WalkDir doesn't follow a symlinked root.
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable dir: skip it, keep walking
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if _, ok := fontTypes[strings.ToLower(filepath.Ext(name))]; !ok {
				return nil
			}
			if _, dup := files[name]; !dup { // first directory wins
				files[name] = path
			}
			return nil
		})
	}
	fi.files = files
	fi.scanned = time.Now()
}

// lookup finds a font by base name. A miss rescans, at most once a minute, so
// fonts installed while serve runs are picked up.
func (fi *fontIndex) lookup(name string) (string, bool) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.files == nil {
		fi.scan()
	}
	if p, ok := fi.files[name]; ok {
		return p, true
	}
	if time.Since(fi.scanned) > time.Minute {
		fi.scan()
		p, ok := fi.files[name]
		return p, ok
	}
	return "", false
}

func (s *server) font(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	name := r.PathValue("name")
	ctype, ok := fontTypes[strings.ToLower(filepath.Ext(name))]
	// Base names only: no paths, no dotfiles.
	if !ok || name != filepath.Base(name) || strings.HasPrefix(name, ".") || s.fonts == nil {
		http.NotFound(w, r)
		return
	}
	path, ok := s.fonts.lookup(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "max-age=604800")
	http.ServeFile(w, r, path)
}
