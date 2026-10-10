package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWisdom(t *testing.T) {
	src := `# my sayings
# comments are skipped

The tab is not the task.

---

When you reach for a new tab
to save something for later, drop it instead.
— me, after the 100-tab cleanup

---

First paragraph.

Second paragraph.
-- someone

---
---

# archive
Retired, never shown.
---
Also retired.
`
	got := parseWisdom(src)
	if len(got) != 3 {
		t.Fatalf("got %d sayings, want 3: %#v", len(got), got)
	}
	if got[0].Text != "The tab is not the task." || got[0].Source != "" {
		t.Errorf("first = %#v", got[0])
	}
	if got[1].Text != "When you reach for a new tab to save something for later, drop it instead." {
		t.Errorf("soft breaks not joined: %q", got[1].Text)
	}
	if got[1].Source != "me, after the 100-tab cleanup" {
		t.Errorf("source = %q", got[1].Source)
	}
	if got[2].Text != "First paragraph.\nSecond paragraph." || got[2].Source != "someone" {
		t.Errorf("paragraphs = %#v", got[2])
	}
	for _, s := range got {
		if strings.Contains(s.Text, "Retired") {
			t.Error("archive leaked")
		}
	}
	// Whitespace/case changes don't reset "last seen".
	if a, b := parseWisdom("Hello   World"), parseWisdom("hello world"); a[0].key != b[0].key {
		t.Error("key should ignore case and spacing")
	}
}

func TestWisdomWeight(t *testing.T) {
	now := time.Now()
	tau := 12 * time.Hour
	if w := wisdomWeight(time.Time{}, now, tau); w != 1 {
		t.Errorf("never seen = %v, want 1", w)
	}
	if w := wisdomWeight(now, now, tau); w != 0 {
		t.Errorf("just seen = %v, want 0", w)
	}
	if w := wisdomWeight(now.Add(-tau), now, tau); math.Abs(w-(1-1/math.E)) > 1e-9 {
		t.Errorf("one tau ago = %v", w)
	}
	if w := wisdomWeight(now.Add(-10*tau), now, tau); w < 0.9999 {
		t.Errorf("long ago = %v", w)
	}
}

func TestPickWeighted(t *testing.T) {
	if i := pickWeighted(nil, 0.5); i != -1 {
		t.Errorf("empty = %d", i)
	}
	if i := pickWeighted([]float64{0, 0}, 0.75); i != 1 {
		t.Errorf("all-zero fallback = %d, want uniform pick 1", i)
	}
	w := []float64{0, 1, 0, 3}
	for r, want := range map[float64]int{0: 1, 0.2: 1, 0.26: 3, 0.999: 3} {
		if i := pickWeighted(w, r); i != want {
			t.Errorf("pick(%v) = %d, want %d", r, i, want)
		}
	}
}

func TestWisdomNextAvoidsRepeats(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "wisdom.md"), []byte("a\n---\nb\n---\nc\n"), 0o644)
	w := &wisdom{
		file:      filepath.Join(dir, "wisdom.md"),
		statePath: filepath.Join(dir, "state", "seen.json"),
		tau:       12 * time.Hour,
	}
	now := time.Now()
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		s, ago, ok := w.next(now.Add(time.Duration(i) * time.Second))
		if !ok {
			t.Fatal("no saying")
		}
		if ago >= 0 {
			t.Errorf("draw %d: %q already seen %v ago", i, s.Text, ago)
		}
		seen[s.Text] = true
	}
	// With tau = 12h, anything shown seconds ago has weight ~0, so three
	// draws a second apart must cover all three sayings.
	if len(seen) != 3 {
		t.Errorf("saw %v, want all three", seen)
	}
	if _, err := os.Stat(w.statePath); err != nil {
		t.Errorf("state not written: %v", err)
	}
}

func TestWisdomMissingFile(t *testing.T) {
	w := &wisdom{file: filepath.Join(t.TempDir(), "nope.md"), statePath: filepath.Join(t.TempDir(), "s.json"), tau: time.Hour}
	if _, _, ok := w.next(time.Now()); ok {
		t.Error("ok with no wisdom.md")
	}
}

// The template only fails at render time, so render it once.
func TestPageRenders(t *testing.T) {
	var b strings.Builder
	say := saying{Text: "The tab is not the task.", Source: "me"}
	d := pageData{Saying: &say, SayingFit: "xl", SeenLabel: "never seen",
		Lately: markNewest([]lately{{When: "sun 23:14", Text: "a <b>note</b>"}, {When: "sat 01:40", Text: "two"}})}
	if err := pageTmpl.Execute(&b, d); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"The tab is not the task.", "· me ·", `class="new"`, "a &lt;b&gt;note&lt;/b&gt;"} {
		if !strings.Contains(out, want) {
			t.Errorf("page missing %q", want)
		}
	}
	b.Reset()
	if err := pageTmpl.Execute(&b, pageData{}); err != nil || !strings.Contains(b.String(), "Add sayings") {
		t.Errorf("empty page: err=%v", err)
	}
}

func TestSummarizeTabDump(t *testing.T) {
	dump := "https://a.example/x\tFlakes — NixOS Wiki\t1\t2026-10-01T00:00:00Z\n" +
		"https://b.example/\tSecond\t1\t2026-10-01T00:00:00Z\n"
	if got := summarize(dump); got != "2 tabs from :wq — Flakes — NixOS Wiki" {
		t.Errorf("tab dump = %q", got)
	}
	if got := summarize("\n\nfirst line\nsecond"); got != "first line" {
		t.Errorf("note = %q", got)
	}
	if got := summarize("![](media/x.jpg)"); got != "photo" {
		t.Errorf("photo = %q", got)
	}
}

func TestFontEndpoint(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "truetype"), 0o755)
	os.WriteFile(filepath.Join(dir, "truetype", "Good.ttf"), []byte("font"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644)
	s := &server{fonts: &fontIndex{dirs: []string{dir}}} // open mode
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1:7777"+path, nil)
		r.Host = "127.0.0.1:7777"
		w := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /fonts/{name}", s.font)
		mux.ServeHTTP(w, r)
		return w
	}
	if w := get("/fonts/Good.ttf"); w.Code != 200 || w.Body.String() != "font" || w.Header().Get("Content-Type") != "font/ttf" {
		t.Errorf("good font: %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/fonts/Missing.ttf", "/fonts/secret.txt", "/fonts/..%2Fsecret.txt", "/fonts/.hidden.ttf"} {
		if w := get(p); w.Code == 200 {
			t.Errorf("%s served: %q", p, w.Body.String())
		}
	}
}
