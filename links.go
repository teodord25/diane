package main

// Link enrichment. For every URL in captured text, fetch the page once and
// write links/<id>.md: front matter (url, title, site) + a prose excerpt.
// The excerpt is context for the agent, not for you to read.
//
//   HTML:     go-readability (Mozilla's Readability.js, the Firefox Reader
//             View algorithm) picks the title and the main article text
//   YouTube:  same title, plus channel name and the full description,
//             which lives in the page's inline player JSON
//   other:    (PDF, images...) title = file name, no text
//
// Failures are skipped silently: no sidecar means the next gather (or
// `diane links`) retries.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	maxPage = 3 << 20 // bytes of HTML read per page
	maxText = 4000    // bytes of prose kept per link
)

type link struct {
	URL, Title, Site, Text string
}

// ---- public: what gather and `diane links` call ----

// Parens are allowed inside URLs (Wikipedia: .../No_Reason_(horse));
// trimURL strips a trailing ")" only when it's unbalanced, so
// "(see https://x.com)" and "[t](https://x.com)" still come out right.
var urlRe = regexp.MustCompile(`https?://[^\s<>\[\]"'` + "`" + `]+`)

func trimURL(u string) string {
	for {
		t := strings.TrimRight(u, ".,;:!?")
		if strings.HasSuffix(t, ")") && strings.Count(t, "(") < strings.Count(t, ")") {
			t = t[:len(t)-1]
		}
		if t == u {
			return u
		}
		u = t
	}
}

// findURLs returns the URLs in text, trailing punctuation trimmed.
func findURLs(text string) []string {
	var out []string
	for _, u := range urlRe.FindAllString(text, -1) {
		out = append(out, trimURL(u))
	}
	return out
}

// enrichLinks ensures each URL in text has a sidecar in vault/links and
// returns url -> title for everything it knows about.
func enrichLinks(vault, text string) map[string]string {
	titles := map[string]string{}
	for _, u := range findURLs(text) {
		p := sidecarPath(vault, u)
		if t, ok := readSidecarTitle(p); ok {
			titles[u] = t
			continue
		}
		l, err := fetchLink(u)
		if err != nil {
			continue
		}
		if err := writeSidecar(p, l); err == nil {
			titles[u] = l.Title
		}
	}
	return titles
}

// labelLinks rewrites bare URLs as [title](url). URLs already inside a
// markdown link are left alone. Use on inbox text; keep raw.md verbatim.
func labelLinks(text string, titles map[string]string) string {
	var b strings.Builder
	last := 0
	for _, m := range urlRe.FindAllStringIndex(text, -1) {
		u := trimURL(text[m[0]:m[1]])
		end := m[0] + len(u)
		t := titles[u]
		if t == "" || strings.HasSuffix(text[:m[0]], "](") {
			continue
		}
		b.WriteString(text[last:m[0]])
		fmt.Fprintf(&b, "[%s](%s)", strings.ReplaceAll(t, "]", ")"), u)
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}

// ---- sidecar files ----

func sidecarPath(vault, u string) string {
	h := sha256.Sum256([]byte(u))
	return filepath.Join(vault, "links", hex.EncodeToString(h[:])[:12]+".md")
}

func writeSidecar(p string, l link) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	s := fmt.Sprintf("---\nurl: %s\ntitle: %s\nsite: %s\nfetched: %s\n---\n\n%s\n",
		l.URL, oneLine(l.Title), l.Site, time.Now().Format("2006-01-02"), l.Text)
	return os.WriteFile(p, []byte(s), 0o644)
}

func readSidecarTitle(p string) (string, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if t, ok := strings.CutPrefix(line, "title: "); ok {
			return t, true
		}
	}
	return "", true
}

// ---- fetching ----

// linkClient honors DIANE_TIMEOUT (default 10s) per fetch.
func linkClient() *http.Client {
	timeout := 10 * time.Second
	if d, err := time.ParseDuration(os.Getenv("DIANE_TIMEOUT")); err == nil {
		timeout = d
	}
	return &http.Client{Timeout: timeout}
}

func fetchLink(u string) (link, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return link{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0")
	req.Header.Set("Accept-Language", "en")
	if isYouTube(req.URL.Host) {
		req.Header.Set("Cookie", "SOCS=CAI") // skip the EU consent interstitial
	}
	resp, err := linkClient().Do(req)
	if err != nil {
		return link{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return link{}, fmt.Errorf("%s: %s", u, resp.Status)
	}

	final := resp.Request.URL // after redirects
	l := link{URL: u, Site: strings.TrimPrefix(final.Host, "www.")}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") {
		l.Title = path.Base(final.Path)
		return l, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return link{}, err
	}
	// Convert to UTF-8 using the Content-Type header or <meta charset>.
	utf8r, err := charset.NewReader(bytes.NewReader(raw), ct)
	if err != nil {
		return link{}, err
	}
	page, err := io.ReadAll(utf8r)
	if err != nil {
		return link{}, err
	}

	art, err := readability.FromReader(bytes.NewReader(page), final)
	if err != nil {
		l.Title = final.String() // unparseable: keep the link, no context
		return l, nil
	}
	l.Title = first(oneLine(art.Title), final.String())
	if art.SiteName != "" {
		l.Site = art.SiteName
	}

	if isYouTube(final.Host) {
		if ch := jsonString(string(page), "ownerChannelName"); ch != "" {
			l.Title += " — " + ch
		}
		l.Text = first(jsonString(string(page), "shortDescription"), art.Excerpt)
	} else {
		l.Text = joinNonEmpty(oneLine(art.Excerpt), blockText(art.Node))
	}
	l.Text = truncate(l.Text, maxText)
	return l, nil
}

func isYouTube(host string) bool {
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	return host == "youtube.com" || host == "youtu.be" || host == "music.youtube.com"
}

// jsonString finds "key":"..." anywhere in the page (e.g. YouTube's inline
// player JSON) and decodes the string literal properly (\n, \u0026, ...).
func jsonString(page, key string) string {
	i := strings.Index(page, `"`+key+`":`)
	if i < 0 {
		return ""
	}
	var s string
	if json.NewDecoder(strings.NewReader(page[i+len(key)+3:])).Decode(&s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// ---- readable text from the article node ----

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "section": true,
	"blockquote": true, "pre": true, "dd": true, "dt": true, "figcaption": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// blockText flattens the article's DOM to plain text with a line break per
// block element, so paragraphs stay paragraphs.
func blockText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			// Tables (Wikipedia infoboxes, data grids) and <sup> citation
			// markers eat the 4 KB budget without saying what the page is about.
			switch n.Data {
			case "script", "style", "table", "sup":
				return
			}
			if blockTags[n.Data] {
				b.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			b.WriteString("\n")
		}
	}
	walk(n)

	var lines []string
	for _, line := range strings.Split(b.String(), "\n") {
		if line = oneLine(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// ---- small helpers ----

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func first(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// joinNonEmpty prepends a to b unless b already opens the same way (the
// excerpt is usually the first paragraph, possibly with citation markers).
func joinNonEmpty(a, b string) string {
	head := a
	if len(head) > 60 {
		head = strings.TrimSuffix(truncate(head, 60), " …")
	}
	if a == "" || strings.Contains(b, head) {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n\n" + b
}

// truncate cuts to at most n bytes without splitting a UTF-8 rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + " …"
}
