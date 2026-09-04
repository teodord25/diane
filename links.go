package main

import (
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	urlRe   = regexp.MustCompile(`https?://[^\s<>"']+`)
	titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

	// A bare Go client gets 403'd by a surprising number of sites.
	linkClient = &http.Client{Timeout: 5 * time.Second}
	userAgent  = "Mozilla/5.0 (X11; Linux x86_64) diane/1.0"
)

// label appends the page title after every URL in text, "url — Title", so a
// link dropped from the phone is readable later without opening it. The URL
// itself is never altered; a fetch that fails leaves the line as it was.
func label(text string) string {
	return urlRe.ReplaceAllStringFunc(text, func(u string) string {
		core := strings.TrimRight(u, ".,;:)]") // punctuation glued to the URL stays outside it
		u, trail := core, u[len(core):]
		if t := title(u); t != "" {
			return u + " — " + t + trail
		}
		return u + trail
	})
}

func title(u string) string {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := linkClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	m := titleRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	if r := []rune(t); len(r) > 120 {
		t = string(r[:120]) + "…"
	}
	return t
}
