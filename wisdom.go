package main

// wisdom.md: sayings shown one per new tab, weighted toward the ones not seen
// in a while.
//
// Format (lives in the vault, so it syncs and you edit it anywhere):
//
//	# anything starting with # is a comment
//
//	The tab is not the task.
//
//	---
//
//	When you reach for a new tab to save something for later,
//	drop it instead.
//	— me, after the 100-tab cleanup
//
//	---
//
//	# archive
//	everything below this heading is kept but never shown
//
// Entries are separated by a line of just "---". A last line starting with
// "— " or "-- " is the source. Retire a saying by moving it under "# archive"
// (or deleting it).
//
// Weighting: a saying last seen Δt ago gets weight 1 − e^(−Δt/τ). Just seen ≈ 0,
// never seen or long ago ≈ 1. τ is DIANE_WISDOM_TAU (default 12h).
//
// "Last seen" is per machine, in $XDG_STATE_HOME/diane/wisdom-seen.json, not
// in the vault: writing it to git on every new tab would be a commit per tab.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type saying struct {
	Text   string
	Source string
	key    string
}

func parseWisdom(src string) []saying {
	var out []saying
	var cur []string
	flush := func() {
		lines := cur
		cur = nil
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
		if len(lines) == 0 {
			return
		}
		s := saying{}
		last := strings.TrimSpace(lines[len(lines)-1])
		for _, p := range []string{"— ", "-- ", "—"} {
			if strings.HasPrefix(last, p) && len(lines) > 1 {
				s.Source = strings.TrimSpace(strings.TrimPrefix(last, p))
				lines = lines[:len(lines)-1]
				break
			}
		}
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		// Soft line breaks inside an entry become spaces; a blank line stays a break.
		s.Text = strings.TrimSpace(strings.ReplaceAll(strings.Join(lines, "\n"), "\n\n", "\x00"))
		s.Text = strings.ReplaceAll(s.Text, "\n", " ")
		s.Text = strings.ReplaceAll(s.Text, "\x00", "\n")
		if s.Text == "" {
			return
		}
		h := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(s.Text), " "))))
		s.key = hex.EncodeToString(h[:8])
		out = append(out, s)
	}
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			if strings.EqualFold(strings.TrimSpace(strings.TrimLeft(t, "#")), "archive") {
				break
			}
			continue
		}
		if t == "---" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

// weight of a saying last seen at `seen` (zero time = never).
func wisdomWeight(seen, now time.Time, tau time.Duration) float64 {
	if seen.IsZero() {
		return 1
	}
	dt := now.Sub(seen)
	if dt <= 0 {
		return 0
	}
	return 1 - math.Exp(-float64(dt)/float64(tau))
}

// pickWeighted returns an index, or -1 for an empty list. If every weight is
// zero (one saying, just seen) it falls back to uniform.
func pickWeighted(weights []float64, r float64) int {
	if len(weights) == 0 {
		return -1
	}
	var total float64
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		return int(r * float64(len(weights)))
	}
	x := r * total
	for i, w := range weights {
		if x < w {
			return i
		}
		x -= w
	}
	return len(weights) - 1
}

type wisdom struct {
	mu        sync.Mutex
	file      string // wisdom.md in the vault
	statePath string
	tau       time.Duration
}

func newWisdom(vaultDir string) *wisdom {
	tau := 12 * time.Hour
	if d, err := time.ParseDuration(os.Getenv("DIANE_WISDOM_TAU")); err == nil && d > 0 {
		tau = d
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		if home, err := os.UserHomeDir(); err == nil {
			state = filepath.Join(home, ".local", "state")
		}
	}
	return &wisdom{
		file:      filepath.Join(vaultDir, "wisdom.md"),
		statePath: filepath.Join(state, "diane", "wisdom-seen.json"),
		tau:       tau,
	}
}

// next picks a saying, records it as seen now, and returns it. ok is false
// when there's no wisdom.md or it has no entries.
func (w *wisdom) next(now time.Time) (s saying, seenAgo time.Duration, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	b, err := os.ReadFile(w.file)
	if err != nil {
		return saying{}, 0, false
	}
	all := parseWisdom(string(b))
	if len(all) == 0 {
		return saying{}, 0, false
	}

	seen := map[string]int64{}
	if raw, err := os.ReadFile(w.statePath); err == nil {
		json.Unmarshal(raw, &seen) // a corrupt file just means "never seen"
	}

	weights := make([]float64, len(all))
	for i, s := range all {
		var t time.Time
		if u, ok := seen[s.key]; ok {
			t = time.Unix(u, 0)
		}
		weights[i] = wisdomWeight(t, now, w.tau)
	}
	i := pickWeighted(weights, rand.Float64())
	s = all[i]
	if u, ok := seen[s.key]; ok {
		seenAgo = now.Sub(time.Unix(u, 0))
	} else {
		seenAgo = -1 // never
	}

	// Record, and drop keys for sayings that no longer exist.
	keep := make(map[string]int64, len(all))
	for _, x := range all {
		if u, ok := seen[x.key]; ok {
			keep[x.key] = u
		}
	}
	keep[s.key] = now.Unix()
	if out, err := json.Marshal(keep); err == nil {
		if err := os.MkdirAll(filepath.Dir(w.statePath), 0o755); err == nil {
			tmp := w.statePath + ".tmp"
			if os.WriteFile(tmp, out, 0o644) == nil {
				os.Rename(tmp, w.statePath)
			}
		}
	}
	return s, seenAgo, true
}
