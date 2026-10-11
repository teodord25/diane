package main

// WaniKani block in the new-tab margin: reviews available now, lessons, and
// when the next reviews unlock. Turns red past a threshold.
//
// Token (read-only is enough; WaniKani → Settings → API Tokens), first found:
//
//	DIANE_WANIKANI_TOKEN       the token itself
//	DIANE_WANIKANI_TOKEN_FILE  a file holding it
//	$XDG_CONFIG_HOME/diane/wanikani-token   (default ~/.config/diane/...)
//
// No token, no block. The token is only ever sent to api.wanikani.com, never
// to the page.
//
// The summary is fetched in the background, at most every 5 minutes, and the
// page renders from the cache, so a slow or offline API never delays a tab.
// The cache keeps the hourly buckets rather than counts, so "available now"
// stays right as time passes between fetches.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const wkRefresh = 5 * time.Minute

type wkBucket struct {
	at time.Time
	n  int
}

type wanikani struct {
	base      string // API base URL; tests point it elsewhere
	client    *http.Client
	threshold int

	mu       sync.Mutex
	reviews  []wkBucket
	lessons  []wkBucket
	fetched  time.Time // last successful fetch
	tried    time.Time // last attempt
	inFlight bool
	err      string
}

func newWaniKani() *wanikani {
	th := 40
	if n, err := strconv.Atoi(os.Getenv("DIANE_WANIKANI_THRESHOLD")); err == nil && n > 0 {
		th = n
	}
	base := "https://api.wanikani.com/v2"
	if b := os.Getenv("DIANE_WANIKANI_API"); b != "" { // for testing against a fake API
		base = strings.TrimRight(b, "/")
	}
	return &wanikani{
		base:      base,
		client:    &http.Client{Timeout: 15 * time.Second},
		threshold: th,
	}
}

// wkToken is read on every refresh, so adding the file later needs no restart.
func wkToken() string {
	if t := strings.TrimSpace(os.Getenv("DIANE_WANIKANI_TOKEN")); t != "" {
		return t
	}
	path := os.Getenv("DIANE_WANIKANI_TOKEN_FILE")
	if path == "" {
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			cfg = filepath.Join(home, ".config")
		}
		path = filepath.Join(cfg, "diane", "wanikani-token")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type wkSummaryJSON struct {
	Data struct {
		Lessons []struct {
			AvailableAt time.Time `json:"available_at"`
			SubjectIDs  []int     `json:"subject_ids"`
		} `json:"lessons"`
		Reviews []struct {
			AvailableAt time.Time `json:"available_at"`
			SubjectIDs  []int     `json:"subject_ids"`
		} `json:"reviews"`
	} `json:"data"`
}

func (wk *wanikani) fetch(token string) (reviews, lessons []wkBucket, err error) {
	req, err := http.NewRequest("GET", wk.base+"/summary", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Wanikani-Revision", "20170710")
	resp, err := wk.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, nil, errors.New("token rejected")
		}
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var s wkSummaryJSON
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, nil, err
	}
	for _, r := range s.Data.Reviews {
		reviews = append(reviews, wkBucket{r.AvailableAt, len(r.SubjectIDs)})
	}
	for _, l := range s.Data.Lessons {
		lessons = append(lessons, wkBucket{l.AvailableAt, len(l.SubjectIDs)})
	}
	return reviews, lessons, nil
}

// refresh fetches if the cache is old and nothing is already fetching. It
// returns immediately; the fetch runs in the background. Failures are
// retried no more often than every 5 minutes too.
func (wk *wanikani) refresh(now time.Time) {
	wk.mu.Lock()
	if wk.inFlight || now.Sub(wk.tried) < wkRefresh {
		wk.mu.Unlock()
		return
	}
	token := wkToken()
	if token == "" {
		wk.mu.Unlock()
		return
	}
	wk.inFlight = true
	wk.tried = now
	wk.mu.Unlock()

	go func() {
		reviews, lessons, err := wk.fetch(token)
		wk.mu.Lock()
		defer wk.mu.Unlock()
		wk.inFlight = false
		if err != nil {
			wk.err = err.Error()
			warn("wanikani: %v", err)
			return
		}
		wk.reviews, wk.lessons = reviews, lessons
		wk.fetched = time.Now()
		wk.err = ""
	}()
}

// wkView is what the page shows.
type wkView struct {
	Reviews   int
	Lessons   int
	Next      string // "next +2h", "next +40m", "none scheduled"
	Threshold int
	Over      bool
	BarPct    int    // reviews against 2× threshold, capped at 100
	Note      string // "stale · token rejected", empty when fresh
}

func (wk *wanikani) view(now time.Time) *wkView {
	wk.mu.Lock()
	defer wk.mu.Unlock()
	if wk.fetched.IsZero() {
		if wk.err != "" {
			return &wkView{Threshold: wk.threshold, Next: "—", Note: wk.err}
		}
		return nil // no token, or the first fetch hasn't landed yet
	}
	v := &wkView{Threshold: wk.threshold}
	var next time.Time
	for _, b := range wk.reviews {
		if !b.at.After(now) {
			v.Reviews += b.n
		} else if b.n > 0 && (next.IsZero() || b.at.Before(next)) {
			next = b.at
		}
	}
	for _, b := range wk.lessons {
		if !b.at.After(now) {
			v.Lessons += b.n
		}
	}
	v.Next = nextLabel(next, now)
	v.Over = v.Reviews > wk.threshold
	v.BarPct = min(100, v.Reviews*100/(2*wk.threshold))
	if age := now.Sub(wk.fetched); age > time.Hour || wk.err != "" {
		note := "stale"
		if wk.err != "" {
			note += " · " + wk.err
		}
		v.Note = note
	}
	return v
}

func nextLabel(next, now time.Time) string {
	if next.IsZero() {
		return "no more in 24h" // the summary only looks a day ahead
	}
	d := next.Sub(now)
	if d < time.Hour {
		return fmt.Sprintf("next +%dm", max(1, int(d.Minutes())))
	}
	return fmt.Sprintf("next +%dh", int((d + 30*time.Minute).Hours()))
}
