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
// The summary is fetched at most every 5 minutes. A tab whose cached numbers
// are older than that waits up to wkWait for the fetch; a slow or offline API
// costs a tab at most that, and the old numbers are shown.
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

const (
	wkRefresh = 5 * time.Minute
	// How long a tab waits for a fetch when the cache is out of date, so the
	// first tab after a refresh is due (e.g. after waking from sleep) shows
	// fresh numbers instead of the old ones.
	wkWait = 800 * time.Millisecond
)

// wall strips Go's monotonic clock reading. The monotonic clock stops while
// the machine is suspended, so "5 minutes since the last fetch" measured with
// it doesn't count sleep: after waking, an hours-old cache looked fresh.
// Wall-clock arithmetic counts the sleep.
func wall(t time.Time) time.Time { return t.Round(0) }

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
	done     chan struct{} // closed when the in-flight fetch finishes
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
// returns immediately; the fetch runs in the background, and the returned
// channel closes when it's done (nil if no fetch was started or is running).
// Failures are retried no more often than every 5 minutes too.
func (wk *wanikani) refresh(now time.Time) <-chan struct{} {
	now = wall(now)
	wk.mu.Lock()
	if wk.inFlight {
		ch := wk.done
		wk.mu.Unlock()
		return ch
	}
	if now.Sub(wk.tried) < wkRefresh {
		wk.mu.Unlock()
		return nil
	}
	token := wkToken()
	if token == "" {
		wk.mu.Unlock()
		return nil
	}
	wk.inFlight = true
	wk.tried = now
	done := make(chan struct{})
	wk.done = done
	wk.mu.Unlock()

	go func() {
		defer close(done)
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
		wk.fetched = wall(time.Now())
		wk.err = ""
	}()
	return done
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

// due reports whether the cache is older than the refresh interval (or empty).
func (wk *wanikani) due(now time.Time) bool {
	wk.mu.Lock()
	defer wk.mu.Unlock()
	return wk.fetched.IsZero() || wall(now).Sub(wk.fetched) >= wkRefresh
}

// fresh starts a refresh if one is due and, when the cached numbers are out
// of date, waits up to wkWait for it, then returns the view. A slow or
// offline API costs a tab at most wkWait, and the old numbers are shown.
func (wk *wanikani) fresh(now time.Time) *wkView {
	stale := wk.due(now)
	if ch := wk.refresh(now); ch != nil && stale {
		select {
		case <-ch:
		case <-time.After(wkWait):
		}
	}
	return wk.view(time.Now())
}

func (wk *wanikani) view(now time.Time) *wkView {
	now = wall(now)
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
