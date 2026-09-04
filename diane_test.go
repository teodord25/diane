package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// clones makes a bare "GitHub" and n clones of it, each with one commit.
func clones(t *testing.T, n int) []Vault {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	run(t, dir, "git", "init", "-q", "--bare", "-b", "main", bare)
	var out []Vault
	for i := 0; i < n; i++ {
		root := filepath.Join(dir, "clone"+string(rune('A'+i)))
		run(t, dir, "git", "clone", "-q", bare, root)
		run(t, root, "git", "commit", "-q", "--allow-empty", "-m", "init")
		run(t, root, "git", "push", "-q", "-u", "origin", "main")
		v, err := openVault(root)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func read(t *testing.T, v Vault, rel string) string {
	t.Helper()
	b, _ := os.ReadFile(v.path(rel))
	return string(b)
}

func TestEntry(t *testing.T) {
	now := time.Date(2026, 9, 4, 11, 30, 0, 0, time.UTC)
	got := entry(now, "  first\r\nsecond\n")
	want := "- 2026-09-04 11:30  first\n  second\n"
	if got != want {
		t.Errorf("entry = %q, want %q", got, want)
	}
}

// Two machines capture while apart, then one gathers: nothing is lost, nothing
// conflicts, and the other machine sees the result after its own gather.
func TestDropsSyncThroughRemoteWithoutConflict(t *testing.T) {
	v := clones(t, 2)
	pc, laptop := v[0], v[1]

	if _, err := pc.drop("from the pc"); err != nil {
		t.Fatal(err)
	}
	// The laptop drops without having pulled the pc's drop first.
	if _, err := laptop.drop("from the laptop"); err != nil {
		t.Fatal(err)
	}

	n, err := laptop.gather()
	if err != nil || n != 2 {
		t.Fatalf("laptop gather = %d, %v", n, err)
	}
	if got := read(t, laptop, inboxFile); !strings.Contains(got, "from the pc") || !strings.Contains(got, "from the laptop") {
		t.Errorf("laptop inbox missing a drop:\n%s", got)
	}
	if read(t, laptop, rawFile) != read(t, laptop, inboxFile) {
		t.Error("raw.md and inbox.md differ after gather")
	}
	if names, _ := laptop.drops(); len(names) != 0 {
		t.Errorf("drops left behind: %v", names)
	}

	// The pc gathers: nothing pending of its own, but it pulls the laptop's fold.
	if n, err := pc.gather(); err != nil || n != 0 {
		t.Fatalf("pc gather = %d, %v", n, err)
	}
	if read(t, pc, inboxFile) != read(t, laptop, inboxFile) {
		t.Errorf("clones diverged:\n%s\n---\n%s", read(t, pc, inboxFile), read(t, laptop, inboxFile))
	}
}

func TestApplySkipsFilesChangedSinceRead(t *testing.T) {
	v := clones(t, 1)[0]
	if err := v.write(inboxFile, []byte("- a\n")); err != nil {
		t.Fatal(err)
	}
	if err := v.write("todo.md", []byte("- x\n")); err != nil {
		t.Fatal(err)
	}
	files, err := v.load()
	if err != nil {
		t.Fatal(err)
	}
	// A capture is gathered while the model is thinking.
	v.drop("late note")
	v.gather()

	r := &Reply{
		Writes:  []Write{{Path: inboxFile, Content: "- rewritten"}, {Path: "todo.md", Content: "- x\n- y"}},
		Deletes: []string{"nothing.md"},
	}
	n, err := v.apply(files, r, "test")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("applied %d, want 2 (todo write + delete of a missing file)", n)
	}
	if got := read(t, v, inboxFile); !strings.Contains(got, "late note") || strings.Contains(got, "rewritten") {
		t.Errorf("inbox overwritten despite change underneath:\n%s", got)
	}
	if got := read(t, v, "todo.md"); got != "- x\n- y\n" {
		t.Errorf("todo = %q", got)
	}
}

func TestLoadHidesProtectedPaths(t *testing.T) {
	v := clones(t, 1)[0]
	for rel, body := range map[string]string{
		rawFile: "raw", inboxFile: "inbox", "drops/x.md": "drop", "media/a.md": "media", "todo.md": "todo", "notes/b.txt": "b",
	} {
		v.write(rel, []byte(body))
	}
	files, err := v.load()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{"inbox.md", "notes/b.txt", "todo.md"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("load = %v, want %v", paths, want)
	}
}

func TestSafePath(t *testing.T) {
	for _, bad := range []string{"", "../evil.md", "a/../../evil.md", "/etc/passwd.md", "notes.sh", "notes", rawFile, "drops/x.md", "media/x.md"} {
		if _, err := safePath(bad); err == nil {
			t.Errorf("safePath accepted %q", bad)
		}
	}
	for _, good := range []string{"todo.md", "a/b/notes.txt", "./todo.md", "log.org", inboxFile} {
		if _, err := safePath(good); err != nil {
			t.Errorf("safePath rejected %q: %v", good, err)
		}
	}
}

func TestLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><title>\n  A &amp; B  \n</title></head></html>"))
	}))
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	got := label("see " + srv.URL + "/x). and " + dead.URL + "/y done\n")
	want := "see " + srv.URL + "/x — A & B). and " + dead.URL + "/y done\n"
	if got != want {
		t.Errorf("label =\n%q\nwant\n%q", got, want)
	}
}

func TestParseReply(t *testing.T) {
	for _, in := range []string{
		`{"speak":"hi","writes":[],"deletes":[]}`,
		"```json\n{\"speak\":\"hi\",\"writes\":[]}\n```",
		"Sure! Here you go:\n{\"speak\":\"hi\",\"writes\":[{\"path\":\"a.md\",\"content\":\"x\"}]}\nHope that helps.",
		`{"speak":"brace } inside a string is fine","writes":[]}`,
	} {
		r, err := parseReply(in)
		if err != nil || r.Speak == "" {
			t.Errorf("parseReply(%q) = %v, %v", truncate(in, 40), r, err)
		}
	}
	if _, err := parseReply("no json here at all"); err == nil {
		t.Error("parseReply accepted non-JSON")
	}
}

func TestSentences(t *testing.T) {
	got := sentences("  Here is your list.\nOne, milk. Two, bread! Done  ")
	want := []string{"Here is your list.", "One, milk.", "Two, bread!", "Done"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sentences() = %q, want %q", got, want)
	}
}

func TestImageExt(t *testing.T) {
	cases := map[string]string{
		"\xFF\xD8\xFFxx": ".jpg", "\x89PNG\r\n\x1a\nxx": ".png",
		"RIFF....WEBPxx": ".webp", "....ftypheicxx": ".heic", "hello": "",
	}
	for in, want := range cases {
		if got, _ := imageExt([]byte(in)); got != want {
			t.Errorf("imageExt(%q) = %q, want %q", in, got, want)
		}
	}
}
