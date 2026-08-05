// diane is an append-only note capture daemon.
//
// One endpoint takes text and appends it to two files in a git-tracked vault:
//
//	raw.md    write-ahead log. Nothing but diane ever writes here.
//	inbox.md  working file. Anton reads and rewrites this one.
//
// Both writes and the git commit happen under an exclusive flock on
// vault/.lock, which is the same lock Anton takes before its full-file
// replacements. That is the entire concurrency story.
package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	maxBody    = 64 << 10 // 64 KiB per drop; a note, not a file upload
	maxPhoto   = 20 << 20 // 20 MiB, enough for a phone camera sketch
	rawFile    = "raw.md"
	inboxFile  = "inbox.md"
	mediaDir   = "media"
	lockFile   = ".lock"
	timeLayout = "2006-01-02 15:04"
	fileLayout = "2006-01-02-150405"
)

type server struct {
	vault string
	token []byte
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	vault := env("DIANE_VAULT", "")
	if vault == "" {
		log.Fatal("DIANE_VAULT is not set")
	}
	abs, err := filepath.Abs(vault)
	if err != nil {
		log.Fatalf("resolving DIANE_VAULT: %v", err)
	}
	vault = abs

	token := env("DIANE_TOKEN", "")
	if token == "" {
		log.Fatal("DIANE_TOKEN is not set")
	}

	// Fail loudly at boot rather than silently dropping commits later.
	if fi, err := os.Stat(filepath.Join(vault, ".git")); err != nil || !fi.IsDir() {
		log.Fatalf("%s is not a git repository", vault)
	}
	if _, err := exec.LookPath("git"); err != nil {
		log.Fatal("git not found on PATH")
	}

	s := &server{vault: vault, token: []byte(token)}

	mux := http.NewServeMux()
	mux.HandleFunc("/drop", s.auth(s.handleDrop))
	mux.HandleFunc("/photo", s.auth(s.handlePhoto))
	mux.HandleFunc("/raw", s.auth(s.serveFile(rawFile)))
	mux.HandleFunc("/inbox", s.auth(s.serveFile(inboxFile)))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	addr := env("DIANE_ADDR", "0.0.0.0:7777")
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	log.Printf("diane listening on %s, vault %s", addr, vault)
	log.Fatal(srv.ListenAndServe())
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// auth wraps a handler with a constant-time bearer token check.
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), s.token) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) handleDrop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "note too large", http.StatusRequestEntityTooLarge)
		return
	}

	text := strings.TrimSpace(string(body))
	if text == "" {
		http.Error(w, "empty note", http.StatusBadRequest)
		return
	}

	entry := format(time.Now(), text)

	if err := s.write(entry); err != nil {
		log.Printf("drop failed: %v", err)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, entry)
}

// handlePhoto stores an image in the vault and logs a line pointing at it.
// Deliberately no OCR: capture stays instant and the transcription problem is
// deferred to whenever the text is actually needed.
func (s *server) handlePhoto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPhoto+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > maxPhoto {
		http.Error(w, "photo too large", http.StatusRequestEntityTooLarge)
		return
	}

	ext, ok := imageExt(body)
	if !ok {
		http.Error(w, "not a recognised image", http.StatusUnsupportedMediaType)
		return
	}

	now := time.Now()
	caption := strings.TrimSpace(r.URL.Query().Get("caption"))

	entry, err := s.writePhoto(now, ext, caption, body)
	if err != nil {
		log.Printf("photo failed: %v", err)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, entry)
}

// imageExt sniffs the magic bytes rather than trusting Content-Type, which
// phone clients set carelessly. Returns the extension to store under.
func imageExt(b []byte) (string, bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return ".jpg", true
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png", true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp", true
	case len(b) >= 12 && string(b[4:8]) == "ftyp" &&
		(string(b[8:12]) == "heic" || string(b[8:12]) == "heix" ||
			string(b[8:12]) == "mif1" || string(b[8:12]) == "msf1"):
		return ".heic", true
	}
	return "", false
}

// writePhoto saves the image and logs an entry naming it, all under the vault
// lock. It returns the entry as written, which is the only place the final
// filename is known.
func (s *server) writePhoto(now time.Time, ext, caption string, data []byte) (string, error) {
	var entry string
	err := s.withLock(func() error {
		dir := filepath.Join(s.vault, mediaDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", mediaDir, err)
		}

		rel := filepath.ToSlash(filepath.Join(mediaDir, now.Format(fileLayout)+ext))
		// Photographing several notebook pages in a burst lands multiple
		// images in the same second. Never clobber; O_EXCL under the lock
		// makes the check-and-create atomic.
		base := strings.TrimSuffix(rel, ext)
		var f *os.File
		var err error
		for i := 0; ; i++ {
			candidate := rel
			if i > 0 {
				candidate = fmt.Sprintf("%s-%d%s", base, i, ext)
			}
			f, err = os.OpenFile(filepath.Join(s.vault, filepath.FromSlash(candidate)),
				os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err == nil {
				rel = candidate
				break
			}
			if !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("creating %s: %w", candidate, err)
			}
			if i > 100 {
				return fmt.Errorf("too many collisions for %s", rel)
			}
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return fmt.Errorf("writing %s: %w", rel, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("closing %s: %w", rel, err)
		}
		text := "[img] " + rel
		if caption != "" {
			text += "  " + caption
		}
		entry = format(now, text)

		for _, name := range []string{rawFile, inboxFile} {
			if err := appendTo(filepath.Join(s.vault, name), entry); err != nil {
				return fmt.Errorf("appending to %s: %w", name, err)
			}
		}
		return s.commit(rel)
	})
	return entry, err
}

// format turns arbitrary text into one line-oriented log entry. Multi-line
// notes keep their shape but continuation lines are indented, so raw.md stays
// greppable one-entry-per-leading-dash.
func format(now time.Time, text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "- %s  %s\n", now.Format(timeLayout), lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	return b.String()
}

// write appends the entry to both files and commits, all under the vault lock.
func (s *server) write(entry string) error {
	return s.withLock(func() error {
		for _, name := range []string{rawFile, inboxFile} {
			if err := appendTo(filepath.Join(s.vault, name), entry); err != nil {
				return fmt.Errorf("appending to %s: %w", name, err)
			}
		}
		return s.commit()
	})
}

func (s *server) withLock(fn func() error) error {
	f, err := os.OpenFile(filepath.Join(s.vault, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening lock: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}

func appendTo(path, entry string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(f, entry); err != nil {
		return err
	}
	return f.Sync()
}

func (s *server) commit(extra ...string) error {
	if out, err := s.git(append([]string{"add", rawFile, inboxFile}, extra...)...); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	out, err := s.git("commit", "-q", "-m", "diane: drop")
	if err != nil {
		// A concurrent Anton commit may have already swept our change in.
		if strings.Contains(out, "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	return nil
}

func (s *server) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", s.vault}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=diane", "GIT_AUTHOR_EMAIL=diane@localhost",
		"GIT_COMMITTER_NAME=diane", "GIT_COMMITTER_EMAIL=diane@localhost",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// serveFile hands back a vault file as plain text. Rendering is your problem,
// which is the point.
func (s *server) serveFile(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		b, err := os.ReadFile(filepath.Join(s.vault, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("reading %s: %v", name, err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(b)
	}
}
