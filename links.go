package main

// Link enrichment. For every URL in captured text, fetch the page once and
// write links/<id>.md: front matter (url, title, site) + a prose excerpt.
// The excerpt is context for the agent, not for you to read.
//
//   YouTube:  og:title + channel + full description (from the player JSON)
//   HTML:     og/meta description + best-guess body text (<article> > <main> > <body>)
//   other:    (PDF, images...) title = file name, no text
//
// Failures are skipped silently: no sidecar means the next gather retries.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxPage = 3 << 20 // bytes of HTML read per page
	maxText = 4000    // bytes of prose kept per link
)

// linkClient honors DIANE_TIMEOUT (default 10s) per fetch.
func linkClient() *http.Client {
	timeout := 10 * time.Second
	if d, err := time.ParseDuration(os.Getenv("DIANE_TIMEOUT")); err == nil {
		timeout = d
	}
	return &http.Client{Timeout: timeout}
}

type link struct {
	URL, Title, Site, Text string
}

// ---- public: what gather calls ----

var urlRe = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `]+`)

// findURLs returns the URLs in text, trailing punctuation trimmed.
func findURLs(text string) []string {
	var out []string
	for _, u := range urlRe.FindAllString(text, -1) {
		out = append(out, strings.TrimRight(u, ".,;:!?"))
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
		u := strings.TrimRight(text[m[0]:m[1]], ".,;:!?")
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

	if !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		l.Title = path.Base(final.Path)
		return l, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return link{}, err
	}
	page := string(b)
	m := metaTags(page)

	l.Title = first(m["og:title"], m["twitter:title"], titleTag(page), final.String())
	desc := first(m["og:description"], m["description"], m["twitter:description"])

	if isYouTube(final.Host) {
		if ch := jsonString(page, "ownerChannelName"); ch != "" {
			l.Title += " — " + ch
		}
		l.Text = first(jsonString(page, "shortDescription"), desc)
	} else {
		l.Text = joinNonEmpty(desc, bodyText(page))
	}
	l.Text = truncate(l.Text, maxText)
	return l, nil
}

func isYouTube(host string) bool {
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	return host == "youtube.com" || host == "youtu.be" || host == "music.youtube.com"
}

// ---- HTML scraping without a parser ----

var (
	metaRe  = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	attrRe  = regexp.MustCompile(`(?is)([a-z][\w:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// metaTags maps <meta name|property=K content=V> to K -> V (first wins).
func metaTags(page string) map[string]string {
	out := map[string]string{}
	for _, tag := range metaRe.FindAllString(page, -1) {
		a := map[string]string{}
		for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
			a[strings.ToLower(m[1])] = m[2] + m[3]
		}
		k := strings.ToLower(first(a["property"], a["name"]))
		if k != "" && a["content"] != "" && out[k] == "" {
			out[k] = clean(a["content"])
		}
	}
	return out
}

func titleTag(page string) string {
	if m := titleRe.FindStringSubmatch(page); m != nil {
		return clean(m[1])
	}
	return ""
}

// Elements whose contents are never prose.
var junkRes = func() []*regexp.Regexp {
	var rs []*regexp.Regexp
	for _, t := range []string{"script", "style", "noscript", "svg", "template",
		"nav", "header", "footer", "aside", "form", "button", "select"} {
		rs = append(rs, regexp.MustCompile(`(?is)<`+t+`\b.*?</`+t+`\s*>`))
	}
	return append(rs, regexp.MustCompile(`(?s)<!--.*?-->`))
}()

var (
	regionRes = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<article\b[^>]*>(.*?)</article>`),
		regexp.MustCompile(`(?is)<main\b[^>]*>(.*?)</main>`),
		regexp.MustCompile(`(?is)<body\b[^>]*>(.*)</body>`),
	}
	blockRe = regexp.MustCompile(`(?i)</?(p|div|br|li|h[1-6]|tr|td|section|blockquote|pre|dd|dt)\b[^>]*>`)
	tagRe   = regexp.MustCompile(`<[^>]*>`)
)

// bodyText is a crude readability pass: strip junk elements, take the most
// specific content region, turn block tags into line breaks, drop tags, and
// keep only lines that look like sentences (menus and buttons are short).
func bodyText(page string) string {
	for _, r := range junkRes {
		page = r.ReplaceAllString(page, "")
	}
	for _, r := range regionRes {
		if m := r.FindStringSubmatch(page); m != nil && len(tagRe.ReplaceAllString(m[1], "")) > 200 {
			page = m[1]
			break
		}
	}
	page = blockRe.ReplaceAllString(page, "\n")
	page = html.UnescapeString(tagRe.ReplaceAllString(page, " "))

	var keep []string
	for _, line := range strings.Split(page, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if len(strings.Fields(line)) >= 6 {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
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

// ---- small helpers ----

func clean(s string) string { return strings.Join(strings.Fields(html.UnescapeString(s)), " ") }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func first(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func joinNonEmpty(a, b string) string {
	if a == "" || strings.Contains(b, a) {
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
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n] + " …"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
