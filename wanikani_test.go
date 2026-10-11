package main

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// summaryAt builds a /summary body with buckets relative to now: reviews at
// -1h (3 items), now (2), +1h (0), +3h (5); lessons now (7).
func summaryAt(now time.Time) string {
	h := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	return fmt.Sprintf(`{"object":"report","data":{
	  "lessons":[{"available_at":%q,"subject_ids":[1,2,3,4,5,6,7]}],
	  "next_reviews_at":%q,
	  "reviews":[
	    {"available_at":%q,"subject_ids":[1,2,3]},
	    {"available_at":%q,"subject_ids":[4,5]},
	    {"available_at":%q,"subject_ids":[]},
	    {"available_at":%q,"subject_ids":[6,7,8,9,10]}]}}`,
		h(0), h(0), h(-time.Hour), h(0), h(time.Hour), h(3*time.Hour))
}

func waitFetched(t *testing.T, wk *wanikani) {
	t.Helper()
	for i := 0; i < 200; i++ {
		wk.mu.Lock()
		done := !wk.inFlight
		wk.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fetch never finished")
}

func TestWaniKaniSummary(t *testing.T) {
	now := time.Now()
	var gotAuth, gotRev string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotRev = r.Header.Get("Authorization"), r.Header.Get("Wanikani-Revision")
		if r.URL.Path != "/summary" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, summaryAt(now))
	}))
	defer api.Close()
	t.Setenv("DIANE_WANIKANI_TOKEN", "tok")

	wk := &wanikani{base: api.URL, client: api.Client(), threshold: 4}
	if v := wk.view(now); v != nil {
		t.Fatalf("view before any fetch = %+v, want nil", v)
	}
	wk.refresh(now)
	waitFetched(t, wk)
	if gotAuth != "Bearer tok" || gotRev != "20170710" {
		t.Errorf("headers: auth=%q revision=%q", gotAuth, gotRev)
	}
	v := wk.view(now)
	if v == nil {
		t.Fatal("no view after fetch")
	}
	if v.Reviews != 5 || v.Lessons != 7 {
		t.Errorf("reviews=%d lessons=%d, want 5 and 7", v.Reviews, v.Lessons)
	}
	if v.Next != "next +3h" { // the +1h bucket is empty
		t.Errorf("next = %q", v.Next)
	}
	if !v.Over || v.BarPct != 62 || v.Note != "" {
		t.Errorf("over=%v bar=%d note=%q", v.Over, v.BarPct, v.Note)
	}
	// Later, without refetching: the +3h bucket has become available.
	if v := wk.view(now.Add(3 * time.Hour)); v.Reviews != 10 || v.Next != "none in the next day" || v.Note != "stale" {
		t.Errorf("3h later: %+v", v)
	}
	// A refresh within 5 minutes is a no-op.
	wk.refresh(now.Add(time.Minute))
	if wk.inFlight {
		t.Error("refetched within 5 minutes")
	}
}

func TestWaniKaniErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer api.Close()
	t.Setenv("DIANE_WANIKANI_TOKEN", "bad")
	wk := &wanikani{base: api.URL, client: api.Client(), threshold: 40}
	wk.refresh(time.Now())
	waitFetched(t, wk)
	if v := wk.view(time.Now()); v == nil || v.Note != "token rejected" {
		t.Errorf("bad token view = %+v", v)
	}
}

func TestWaniKaniNoToken(t *testing.T) {
	t.Setenv("DIANE_WANIKANI_TOKEN", "")
	t.Setenv("DIANE_WANIKANI_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	wk := &wanikani{base: "http://127.0.0.1:1", client: http.DefaultClient, threshold: 40}
	wk.refresh(time.Now())
	if wk.inFlight || wk.view(time.Now()) != nil {
		t.Error("fetched or showed a block without a token")
	}
}

func TestWaniKaniTokenFile(t *testing.T) {
	t.Setenv("DIANE_WANIKANI_TOKEN", "")
	f := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(f, []byte("  from-file\n"), 0o600)
	t.Setenv("DIANE_WANIKANI_TOKEN_FILE", f)
	if got := wkToken(); got != "from-file" {
		t.Errorf("token = %q", got)
	}
}

func TestPageRendersWaniKani(t *testing.T) {
	var b strings.Builder
	d := pageData{WaniKani: &wkView{Reviews: 58, Lessons: 3, Next: "next +2h", Threshold: 40, Over: true, BarPct: 72}}
	if err := pageTmpl.Execute(&b, d); err != nil {
		t.Fatal(err)
	}
	out := html.UnescapeString(b.String()) // the template writes + as &#43;
	for _, want := range []string{`class="wk over"`, "<b>58</b> reviews", "over 40 — do them", "3 lessons · next +2h", "width:72%"} {
		if !strings.Contains(out, want) {
			t.Errorf("page missing %q", want)
		}
	}
}
