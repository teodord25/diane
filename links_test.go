package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const blog = `<html><head><title>Fallback</title>
<meta property="og:title" content="Why Nix &amp; Go get along">
<meta name="description" content="A short post about flakes.">
<script>var junk = "should never appear in output at all ok";</script></head>
<body><nav><a>Home</a> <a>About this site and other links here</a></nav>
<article><h1>Why Nix</h1><p>Flakes pin every input so builds are reproducible across machines.</p>
<p>Short.</p><div>buildGoModule needs a vendorHash, which is annoying the first time you see it.</div></article>
<footer>Copyright some company name 2026 all rights reserved</footer></body></html>`

const yt = `<html><head><meta property="og:title" content="Cool Video">
<meta name="description" content="truncated desc..."></head><body>
<script>var ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"Line one \u0026 more.\nChapters:\n0:00 intro","ownerChannelName":"Some Channel"}};</script></body></html>`

func TestEnrich(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blog":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, blog)
		case "/paper.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	l, err := fetchLink(srv.URL + "/blog")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("TITLE: %s\nTEXT:\n%s\n\n", l.Title, l.Text)
	if l.Title != "Why Nix & Go get along" || strings.Contains(l.Text, "junk") ||
		strings.Contains(l.Text, "Copyright") || !strings.Contains(l.Text, "vendorHash") {
		t.Fatal("blog extraction wrong")
	}

	dir := t.TempDir()
	in := "read " + srv.URL + "/blog. and " + srv.URL + "/paper.pdf, broken " + srv.URL + "/404 and [x](" + srv.URL + "/blog)"
	titles := enrichLinks(dir, in)
	out := labelLinks(in, titles)
	fmt.Println(out)
	ents, _ := os.ReadDir(dir + "/links")
	if len(ents) != 2 || strings.Count(out, "[Why Nix") != 1 || !strings.Contains(out, "[paper.pdf](") {
		t.Fatalf("enrich/label wrong: %d sidecars", len(ents))
	}
	// second run hits cache, no refetch needed
	srv.Close()
	if t2 := enrichLinks(dir, in); len(t2) != 2 {
		t.Fatal("cache miss")
	}
	b, _ := os.ReadFile(sidecarPath(dir, srv.URL+"/blog"))
	fmt.Println(string(b))
}

func TestYouTubeJSON(t *testing.T) {
	fmt.Println(jsonString(yt, "shortDescription"), "|", jsonString(yt, "ownerChannelName"))
	if jsonString(yt, "shortDescription") != "Line one & more.\nChapters:\n0:00 intro" {
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
	fmt.Println(out)
	if out != "a [No Reason](https://en.wikipedia.org/wiki/No_Reason_(horse)) b (see [X](https://x.com/a)) c [t](https://en.wikipedia.org/wiki/X_(film)). d [Z](https://y.com/z)." {
		t.Fatal("label wrong")
	}
}
