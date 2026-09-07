package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The vault is a git repository containing:
//
//	drops/    one file per capture, not yet folded in. Never edited, only created and removed.
//	raw.md    append-only log of every capture ever. Nothing but gather writes here.
//	inbox.md  the working file. Captures land here too; anton rewrites it.
//	media/    photos. A drop points at them.
//	*.md      anything else anton keeps (todo.md, ...).
//
// Sync is git: pull --rebase before reading, push after writing. Because every
// capture is its own uniquely named file, two machines capturing at once can
// never produce a merge conflict. Only raw.md and inbox.md are ever rewritten,
// and only by gather and anton, so a conflict there means you ran those on two
// machines without syncing in between; git will say so and you resolve by hand.
const (
	rawFile   = "raw.md"
	inboxFile = "inbox.md"
	dropsDir  = "drops"
	mediaDir  = "media"
	timeFmt   = "2006-01-02 15:04"
	fileFmt   = "20060102-150405"

	// Files larger than this are not shown to the model. If you hit it, the
	// whole-vault-in-the-prompt design has stopped being the right one.
	maxFileBytes = 64 << 10
)

var textExts = map[string]bool{".md": true, ".txt": true, ".org": true}

type Vault struct {
	Root string
	// Deferred suppresses the pull and push inside every command, leaving
	// commits local. A long conversation sets it so it is not paying for
	// four round trips to the remote per turn; see anton() for the entry
	// and exit syncs that make that safe.
	Deferred bool
}

type File struct{ Path, Content string }

func openVault(root string) (Vault, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Vault{}, err
	}
	if fi, err := os.Stat(filepath.Join(abs, ".git")); err != nil || !fi.IsDir() {
		return Vault{}, fmt.Errorf("%s is not a git repository; clone your vault there first", abs)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return Vault{}, fmt.Errorf("git not found on PATH")
	}
	return Vault{Root: abs}, nil
}

func (v Vault) path(rel string) string { return filepath.Join(v.Root, filepath.FromSlash(rel)) }

// locked runs fn under an exclusive flock. Git cannot update its index from
// two processes at once, so everything that commits goes through here. The
// lock file lives inside .git, so it can never be committed by accident.
func (v Vault) locked(fn func() error) error {
	f, err := os.OpenFile(filepath.Join(v.Root, ".git", "diane.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (v Vault) git(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", v.Root}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=diane", "GIT_AUTHOR_EMAIL=diane@localhost",
		"GIT_COMMITTER_NAME=diane", "GIT_COMMITTER_EMAIL=diane@localhost",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// save commits everything and syncs with the remote. A failed sync (offline,
// or a conflict) is reported but is not an error: the commit is on disk and
// the next save will carry it.
func (v Vault) save(msg string) error {
	if out, err := v.git("add", "-A"); err != nil {
		return fmt.Errorf("git add: %s", out)
	}
	if _, err := v.git("diff", "--cached", "--quiet"); err == nil {
		return nil // nothing changed
	}
	if out, err := v.git("commit", "-q", "-m", msg); err != nil {
		return fmt.Errorf("git commit: %s", out)
	}
	if err := v.sync(); err != nil {
		warn("%v", err)
	}
	return nil
}

// sync rebases local commits onto the remote and pushes. No remote configured
// means a purely local vault, which is fine. Deferred makes it do nothing;
// syncNow ignores that, and is how a deferred session syncs on purpose. A rebase that conflicts is aborted
// so the clone is never left half-merged; the local commits are kept.
func (v Vault) sync() error {
	if v.Deferred {
		return nil
	}
	if _, err := v.git("remote", "get-url", "origin"); err != nil {
		return nil
	}
	if out, err := v.git("pull", "-q", "--rebase"); err != nil {
		v.git("rebase", "--abort")
		return fmt.Errorf("pull failed, vault not synced (fix with `git -C %s pull --rebase`): %s", v.Root, brief(out))
	}
	if out, err := v.git("push", "-q"); err != nil {
		return fmt.Errorf("push failed, vault not synced: %s", brief(out))
	}
	return nil
}

// syncNow syncs even in a deferred session.
func (v Vault) syncNow() error { v.Deferred = false; return v.locked(v.sync) }

// brief keeps git's first real line and drops its paragraphs of hints.
func brief(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "hint:") {
			return l
		}
	}
	return out
}

// entry turns text into one log entry. Continuation lines are indented so
// the log stays one-entry-per-leading-dash and therefore greppable.
func entry(now time.Time, text string) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "- %s  %s\n", now.Format(timeFmt), lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	return b.String()
}

// drop captures text: one new file in drops/, committed and synced.
// Returns the entry as written.
func (v Vault) drop(text string) (string, error) { return v.capture(text, "", nil) }

// capture is drop plus an optional file stored alongside the note.
func (v Vault) capture(text, file string, data []byte) (string, error) {
	now := time.Now()
	e := entry(now, text)
	name := fmt.Sprintf("%s/%s-%s.md", dropsDir, now.Format(fileFmt), nonce())
	err := v.locked(func() error {
		if data != nil {
			if err := v.write(file, data); err != nil {
				return err
			}
		}
		if err := v.write(name, []byte(e)); err != nil {
			return err
		}
		return v.save("drop")
	})
	return e, err
}

func (v Vault) write(rel string, data []byte) error {
	p := v.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// nonce keeps two captures in the same second, on any machines, apart.
func nonce() string {
	b := make([]byte, 3)
	rand.Read(b)
	host, _ := os.Hostname()
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return host + "-" + hex.EncodeToString(b)
}

func (v Vault) drops() ([]string, error) {
	names, err := filepath.Glob(v.path(dropsDir + "/*.md"))
	sort.Strings(names) // timestamp-first filenames sort chronologically
	return names, err
}

// pending is everything captured but not yet gathered, oldest first.
func (v Vault) pending() (string, error) {
	names, err := v.drops()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range names {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		b.Write(data)
	}
	return b.String(), nil
}

// gather syncs, then folds pending drops into raw.md and inbox.md and removes
// them. Links get their page title appended on the way through: this is the
// one moment that is neither the capture path (which must be instant) nor the
// model's (which must see the final text).
func (v Vault) gather() (int, error) {
	n := 0
	err := v.locked(func() error {
		if err := v.sync(); err != nil {
			warn("%v", err)
		}
		text, err := v.pending()
		if err != nil || text == "" {
			return err
		}
		text = label(text)
		for _, f := range []string{rawFile, inboxFile} {
			if err := appendTo(v.path(f), text); err != nil {
				return err
			}
		}
		names, _ := v.drops()
		for _, p := range names {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
		n = len(names)
		return v.save(fmt.Sprintf("gather %d", n))
	})
	return n, err
}

func appendTo(path, text string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

// protected paths are never shown to the model and never written by it.
// raw.md is the recovery log, drops/ are captures in flight, media/ is binary.
func protected(rel string) bool {
	return rel == rawFile || strings.HasPrefix(rel, dropsDir+"/") || strings.HasPrefix(rel, mediaDir+"/")
}

// load reads every text file the model may see, sorted, with slash-separated
// paths relative to the root.
func (v Vault) load() ([]File, error) {
	var out []File
	err := filepath.WalkDir(v.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(v.Root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || protected(rel+"/") {
				return fs.SkipDir
			}
			return nil
		}
		if protected(rel) || !textExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxFileBytes {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, File{Path: rel, Content: string(b)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// safePath normalises a model-supplied path and refuses anything outside the
// vault, non-text, or protected. Refuse, never clamp: a path the model must
// not touch is an error to surface, not something to quietly rewrite.
func safePath(rel string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	switch {
	case rel == "" || strings.HasPrefix(clean, "/") || filepath.IsAbs(rel):
		return "", fmt.Errorf("absolute or empty path: %q", rel)
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("path escapes vault: %q", rel)
	case !textExts[strings.ToLower(filepath.Ext(clean))]:
		return "", fmt.Errorf("not a text file: %q", rel)
	case protected(clean):
		return "", fmt.Errorf("protected path: %q", rel)
	}
	return clean, nil
}

// gutter matches the "N| " line numbers the prompt puts in front of every
// line. Models copy them back despite being told not to, so strip them rather
// than fail an edit that was otherwise correct.
var gutter = regexp.MustCompile(`(?m)^\d+\| `)

// lines splits a file into its lines, without a phantom empty last line for
// the trailing newline. Line n in the prompt is lines(content)[n-1].
func lines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

func join(ls []string) string {
	if len(ls) == 0 {
		return ""
	}
	return strings.Join(ls, "\n") + "\n"
}

// apply makes the model's edits, moves and deletions, then saves.
//
// All or nothing: every edit is resolved against the snapshot first, and if
// any of them fails to match, nothing is written. Half of a "move these links
// from A to B" is either a duplicate or a loss, and a loss is unacceptable in
// a note vault, so a reply that does not fully apply does not apply at all.
//
// A file that changed on disk since the snapshot was taken (a gather ran, a
// sync pulled) is skipped rather than overwritten: anton's edit can be redone
// by asking again, a note lost under it cannot.
func (v Vault) apply(snapshot []File, r *Reply, msg string) (int, error) {
	before := make(map[string]string, len(snapshot))
	for _, f := range snapshot {
		before[f.Path] = f.Content
	}

	// Resolve every edit against the snapshot, in order, so several edits to
	// one file compose. Nothing touches the disk until they all succeed.
	after := map[string]string{}
	current := func(rel string) string {
		if text, ok := after[rel]; ok {
			return text
		}
		return before[rel]
	}
	for _, e := range r.Edits {
		rel, err := safePath(e.Path)
		if err != nil {
			return 0, err
		}
		text, search := current(rel), gutter.ReplaceAllString(e.Search, "")
		switch {
		case e.Search == "": // append, creating the file if it does not exist
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			after[rel] = text + e.Replace
		case strings.Count(text, search) == 1:
			after[rel] = strings.Replace(text, search, gutter.ReplaceAllString(e.Replace, ""), 1)
		case !strings.Contains(text, search):
			return 0, fmt.Errorf("no line in %s matches %q", rel, truncate(search, 60))
		default:
			return 0, fmt.Errorf("%q appears more than once in %s; it has to match one place exactly",
				truncate(search, 60), rel)
		}
	}

	// Moves are resolved against the snapshot, never against the result of an
	// earlier move: the line numbers the model was given describe the files as
	// they were shown to it. Cuts from one file therefore accumulate across
	// every move that takes from it, which is the normal case when a list is
	// being sorted into several topics at once.
	cut := map[string]map[int]bool{}
	for _, m := range r.Moves {
		from, err := safePath(m.From)
		if err != nil {
			return 0, err
		}
		to, err := safePath(m.To)
		if err != nil {
			return 0, err
		}
		if from == to {
			return 0, fmt.Errorf("move from %s to itself", from)
		}
		src := lines(before[from])
		if cut[from] == nil {
			cut[from] = map[int]bool{}
		}
		var taken []string
		for _, n := range m.Lines {
			switch {
			case n < 1 || n > len(src):
				return 0, fmt.Errorf("%s has %d lines; there is no line %d", from, len(src), n)
			case cut[from][n]:
				return 0, fmt.Errorf("%s line %d is moved twice", from, n)
			}
			cut[from][n] = true
			taken = append(taken, src[n-1])
		}
		after[to] = join(append(lines(current(to)), taken...))
	}
	// Now that every cut is known, rebuild each source once.
	for from, gone := range cut {
		var kept []string
		for i, l := range lines(before[from]) {
			if !gone[i+1] {
				kept = append(kept, l)
			}
		}
		after[from] = join(kept)
	}

	deletes := make([]string, 0, len(r.Deletes))
	for _, d := range r.Deletes {
		rel, err := safePath(d)
		if err != nil {
			return 0, err
		}
		deletes = append(deletes, rel)
	}

	n := 0
	err := v.locked(func() error {
		changed := func(rel string) bool {
			cur, _ := os.ReadFile(v.path(rel))
			if string(cur) == before[rel] {
				return false
			}
			warn("skipping %s: it changed since it was read", rel)
			return true
		}
		for rel, text := range after {
			if changed(rel) {
				continue
			}
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			if err := v.write(rel, []byte(text)); err != nil {
				return err
			}
			n++
		}
		for _, rel := range deletes {
			if changed(rel) {
				continue
			}
			if rel == inboxFile { // the inbox is emptied, never removed; gather expects it
				if err := v.write(rel, nil); err != nil {
					return err
				}
			} else if err := os.Remove(v.path(rel)); err != nil && !os.IsNotExist(err) {
				return err
			}
			n++
		}
		if n == 0 {
			return nil
		}
		msg = strings.Join(strings.Fields(msg), " ")
		if len(msg) > 60 {
			msg = msg[:60]
		}
		return v.save("anton: " + msg)
	})
	return n, err
}
