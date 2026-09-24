package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A realistic-ish page: nav, footer, script, and a Wikipedia-style infobox
// whose data-mw attribute holds JSON with ">" inside (the case the old
// hand-rolled regexes kept leaking).
const blog = `<html><head><title>Fallback</title>
<meta property="og:title" content="Why Nix &amp; Go get along">
<meta name="description" content="A short post about flakes.">
<script>var junk = "should never appear in output at all ok";</script></head>
<body><nav><a href="/">Home</a> <a href="/about">About this site and other links here</a></nav>
<article><h1>Why Nix</h1>
<div class="infobox" data-mw='{"x":{"wt":"<ref>a</ref>"},"pronounce":{"wt":"sil DEN a fil and some more words"}}'><span>Infobox</span></div>
<p>Flakes pin every input so builds are reproducible across machines, which matters more than it sounds once you have several of them.</p>
<p>buildGoModule needs a vendorHash, which is annoying the first time you see it, but it is what makes the dependency fetch reproducible.</p>
<p>The rest of this paragraph exists so readability has enough text to be confident this is the main content of the page and not a sidebar.</p></article>
<footer>Copyright some company name 2026 all rights reserved</footer></body></html>`

const yt = `<html><head><meta property="og:title" content="Cool Video">
<meta name="description" content="truncated desc..."></head><body>
<script>var ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"Line one \u0026 more.\nChapters:\n0:00 intro","ownerChannelName":"Some Channel"}};</script></body></html>`

// windows-1252 page with no UTF-8: "café" as 0xE9.
var latin1 = "<html><head><meta charset=\"windows-1252\"><title>Caf\xe9 notes</title></head><body><article><p>" +
	strings.Repeat("A caf\xe9 paragraph with enough words to count as real prose here. ", 6) + "</p></article></body></html>"

func testServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blog":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, blog)
		case "/latin1":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, latin1)
		case "/paper.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchBlog(t *testing.T) {
	srv := testServer(t)
	l, err := fetchLink(srv.URL + "/blog")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("TITLE: %s\nTEXT:\n%s\n\n", l.Title, l.Text)
	for _, bad := range []string{"junk", "Copyright", "pronounce", `"wt"`, "About this site"} {
		if strings.Contains(l.Text, bad) {
			t.Errorf("leaked %q", bad)
		}
	}
	if !strings.Contains(l.Title, "Nix") || !strings.Contains(l.Text, "vendorHash") {
		t.Errorf("missing content: title=%q", l.Title)
	}
	if !strings.Contains(l.Text, "\n") {
		t.Error("paragraphs collapsed into one line")
	}
}

func TestCharset(t *testing.T) {
	srv := testServer(t)
	l, err := fetchLink(srv.URL + "/latin1")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("TITLE: %s\n", l.Title)
	if !strings.Contains(l.Title+l.Text, "café") {
		t.Errorf("charset not converted: %q", l.Title)
	}
}

func TestEnrichAndLabel(t *testing.T) {
	srv := testServer(t)
	dir := t.TempDir()
	in := "read " + srv.URL + "/blog. and " + srv.URL + "/paper.pdf, broken " + srv.URL + "/404 and [x](" + srv.URL + "/blog)"
	titles := enrichLinks(dir, in)
	out := labelLinks(in, titles)
	fmt.Println(out)
	ents, _ := os.ReadDir(dir + "/links")
	if len(ents) != 2 || strings.Count(out, "[Why Nix") != 1 || !strings.Contains(out, "[paper.pdf](") {
		t.Fatalf("enrich/label wrong: %d sidecars", len(ents))
	}
	srv.Close() // second run must be served from sidecars
	if t2 := enrichLinks(dir, in); len(t2) != 2 {
		t.Fatal("cache miss")
	}
}

func TestYouTubeJSON(t *testing.T) {
	if jsonString(yt, "shortDescription") != "Line one & more.\nChapters:\n0:00 intro" ||
		jsonString(yt, "ownerChannelName") != "Some Channel" {
		t.Fatal("yt parse")
	}
}

func TestParenURLs(t *testing.T) {
	in := "a https://en.wikipedia.org/wiki/No_Reason_(horse) b (see https://x.com/a) c [t](https://en.wikipedia.org/wiki/X_(film)). d https://y.com/z."
	got := findURLs(in)
	want := []string{"https://en.wikipedia.org/wiki/No_Reason_(horse)", "https://x.com/a", "https://en.wikipedia.org/wiki/X_(film)", "https://y.com/z"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q", got)
	}
	out := labelLinks(in, map[string]string{want[0]: "No Reason", want[1]: "X", want[2]: "SHOULD NOT APPEAR", want[3]: "Z"})
	if out != "a [No Reason](https://en.wikipedia.org/wiki/No_Reason_(horse)) b (see [X](https://x.com/a)) c [t](https://en.wikipedia.org/wiki/X_(film)). d [Z](https://y.com/z)." {
		t.Fatal("label wrong: " + out)
	}
}
