package main

// The page `diane serve` shows at /: Firefox's new tab (/?newtab) and the
// phone's capture page. One saying from wisdom.md, the note box, and a margin
// with the last few captures.

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------- lately

type lately struct {
	When string
	Text string
	New  bool
}

var tabDumpLine = regexp.MustCompile(`^https?://\S+\t`)

// summarize turns a capture body into one short line for the margin.
func summarize(body string) string {
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	dumps := 0
	for _, l := range lines {
		if tabDumpLine.MatchString(l) {
			dumps++
		}
	}
	if dumps == len(lines) {
		first := strings.SplitN(lines[0], "\t", 3)
		title := ""
		if len(first) > 1 {
			title = strings.TrimSpace(first[1])
		}
		s := fmt.Sprintf("%d tabs from :wq", dumps)
		if dumps == 1 {
			s = "1 tab from :wq"
		}
		if title != "" {
			s += " — " + title
		}
		return clip(s, 90)
	}
	if strings.HasPrefix(lines[0], "![](") {
		return "photo"
	}
	return clip(lines[0], 90)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func whenLabel(t, now time.Time) string {
	if now.Sub(t) < 6*24*time.Hour {
		return strings.ToLower(t.Format("Mon 15:04"))
	}
	return strings.ToLower(t.Format("2 Jan"))
}

// recentCaptures returns up to n captures, newest first: pending drops, then
// the tail of raw.md. Errors just mean fewer entries.
func recentCaptures(v *Vault, n int, now time.Time) []lately {
	var out []lately

	if entries, err := os.ReadDir(v.path("drops")); err == nil {
		var names []string
		for _, e := range entries {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				names = append(names, e.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
		for _, name := range names {
			if len(out) >= n {
				return out
			}
			t, err := time.Parse("20060102T150405Z", strings.SplitN(name, "-", 2)[0])
			if err != nil {
				continue
			}
			text := "photo"
			if filepath.Ext(name) == ".md" {
				b, err := os.ReadFile(v.path("drops", name))
				if err != nil {
					continue
				}
				text = summarize(string(b))
			}
			if text == "" {
				continue
			}
			out = append(out, lately{When: whenLabel(t.Local(), now), Text: text})
		}
	}

	// raw.md: "## 2006-01-02 15:04 (host)" headers. Only the tail is read.
	f, err := os.Open(v.path("raw.md"))
	if err != nil {
		return markNewest(out)
	}
	defer f.Close()
	const tail = 64 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		f.Seek(fi.Size()-tail, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	sections := strings.Split("\n"+string(b), "\n## ")
	for i := len(sections) - 1; i >= 1 && len(out) < n; i-- {
		head, body, _ := strings.Cut(sections[i], "\n")
		stamp := head
		if j := strings.Index(head, " ("); j > 0 {
			stamp = head[:j]
		}
		t, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(stamp), time.Local)
		if err != nil {
			continue
		}
		text := summarize(body)
		if text == "" {
			continue
		}
		out = append(out, lately{When: whenLabel(t, now), Text: text})
	}
	return markNewest(out)
}

func markNewest(l []lately) []lately {
	if len(l) > 0 {
		l[0].New = true
	}
	return l
}

// ---------------------------------------------------------------- page

type pageData struct {
	Saying    *saying
	SayingFit string // xl | l | m: font size by length
	SeenLabel string
	Lately    []lately
	WaniKani  *wkView // nil: no token, or no data yet
}

func sayingFit(text string) string {
	switch n := utf8.RuneCountInString(text); {
	case n <= 32:
		return "xl"
	case n <= 100:
		return "l"
	default:
		return "m"
	}
}

func seenLabel(ago time.Duration) string {
	switch {
	case ago < 0:
		return "never seen"
	case ago < time.Hour:
		return "last seen just now"
	case ago < 48*time.Hour:
		return fmt.Sprintf("last seen %d hours ago", int(ago.Hours()))
	default:
		return fmt.Sprintf("last seen %d days ago", int(ago.Hours()/24))
	}
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	now := time.Now()
	d := pageData{Lately: recentCaptures(s.v, 4, now)}
	if s.wisdom != nil {
		if say, ago, ok := s.wisdom.next(now); ok {
			d.Saying = &say
			d.SayingFit = sayingFit(say.Text)
			d.SeenLabel = seenLabel(ago)
		}
	}
	if s.wanikani != nil {
		s.wanikani.refresh(now) // background; this tab shows the cached numbers
		d.WaniKani = s.wanikani.view(now)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a new saying every tab
	if err := pageTmpl.Execute(w, d); err != nil {
		warn("render: %v", err)
	}
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang=en>
<meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>diane</title>
<style>
/* Served by diane (fonts.go): Firefox's fingerprinting protection hides
   installed fonts from pages, web fonts are exempt. Missing files 404 and the
   next family in each stack takes over. */
@font-face{font-family:"d-display";font-weight:100 900;
  src:url(/fonts/EVA-Matisse_Standard.ttf);
  /* everything but spaces: Matisse is a CJK face and its spaces are wide,
     so spaces come from the next family (Source Serif) */
  unicode-range:U+0-1F,U+21-9F,U+A1-2FFF,U+3001-10FFFF}
@font-face{font-family:"d-serif";font-weight:400;
  src:url(/fonts/SourceSerif4-Regular.ttf),url(/fonts/SourceSerif4-Regular.otf)}
@font-face{font-family:"d-serif";font-weight:400;font-style:italic;
  src:url(/fonts/SourceSerif4-It.ttf),url(/fonts/SourceSerif4-It.otf)}
@font-face{font-family:"d-serif";font-weight:600 900;
  src:url(/fonts/SourceSerif4-Semibold.ttf),url(/fonts/SourceSerif4-Semibold.otf)}
@font-face{font-family:"d-mono";font-weight:400;
  src:url(/fonts/JetBrainsMono-Regular.woff2),url(/fonts/JetBrainsMono-Regular.ttf),
      url(/fonts/JetBrainsMonoNerdFont-Regular.ttf)}
:root{
  color-scheme:dark;
  --ground:#0E0E0E; --panel:#151515; --line:#262626;
  --ink:#D6D6D6; --hi:#ECECEC; --dim:#8A8A8A;
  --deep:#9A0000; --red:#E84848;
  --display:"d-display","d-serif","Source Serif 4",serif;
  --serif:"d-serif","Source Serif 4",serif;
  --mono:"d-mono","JetBrains Mono",monospace;
}
*{box-sizing:border-box}
html,body{margin:0;min-height:100%;background:var(--ground);color:var(--ink);
  font:16px/1.5 var(--serif)}
body{display:flex;min-height:100dvh}
main{flex:1;display:flex;gap:56px;padding:64px 64px 56px 72px}
.write{flex:1;min-width:0;display:flex;flex-direction:column;justify-content:flex-end;gap:18px}
.rule{width:64px;height:3px;background:var(--deep)}
.saying{margin:0;font-family:var(--display);font-weight:800;color:var(--hi);
  letter-spacing:-.015em;line-height:1.08;white-space:pre-line;text-wrap:balance}
.saying.xl{font-size:clamp(56px,8vw,120px)}
.saying.l{font-size:clamp(36px,4.6vw,64px)}
.saying.m{font-size:clamp(28px,3.4vw,46px)}
.meta{font:11px var(--mono);color:var(--dim);margin-bottom:18px}
.empty{font-style:italic;color:var(--dim);margin-bottom:18px}
.empty code{font:13px var(--mono);color:var(--ink)}
label{font-size:14px;font-style:italic;color:var(--dim)}
textarea{height:96px;resize:none;background:var(--panel);border:1px solid var(--line);
  color:var(--ink);font:18px/1.55 var(--serif);padding:14px 16px;outline:0;border-radius:0}
textarea:focus{border-color:var(--red)}
.keys{display:flex;flex-wrap:wrap;gap:8px 28px;font-size:14px;color:var(--dim)}
.keys b{font:12px var(--mono);color:var(--ink)}
#s{font:12px var(--mono);color:var(--dim);min-height:1em}
#s.err{color:var(--red)}
button{display:none}
aside{width:260px;flex-shrink:0;display:flex;flex-direction:column;justify-content:flex-end;
  padding-left:28px;border-left:1px solid var(--line)}
aside h2{margin:0;font:italic 14px var(--serif);color:var(--dim);padding:10px 0 12px}
aside .lately-h{border-top:1px solid var(--line)}
.wk{display:flex;flex-direction:column;gap:8px;padding-bottom:22px}
.wk-head{display:flex;justify-content:space-between;align-items:baseline}
.wk-head h2{padding:0}
.wk-head span,.wk-foot{font:11px var(--mono);color:var(--dim)}
.wk-num{display:flex;align-items:baseline;gap:10px;font-size:15px}
.wk-num b{font:500 30px var(--mono);color:var(--hi)}
.wk.over .wk-num b,.wk.over .wk-head span{color:var(--red)}
.wk-go{margin-left:auto;font:12px var(--mono);color:var(--ink);text-decoration:none;
  border:1px solid var(--line);padding:4px 10px}
.wk-go:hover,.wk-go:focus-visible{border-color:var(--red);color:var(--hi);outline:0}
.wk.over .wk-go{background:var(--deep);border-color:var(--deep);color:var(--hi)}
.wk-foot a{color:inherit;text-decoration:underline;text-decoration-color:var(--line);text-underline-offset:3px}
.wk-foot a:hover{color:var(--ink)}
.wk-bar{height:4px;background:var(--line)}
.wk-bar i{display:block;height:100%;background:var(--deep)}
aside ol{list-style:none;margin:0;padding:0}
aside li{display:flex;flex-direction:column;gap:4px;padding:11px 0;border-top:1px solid var(--line)}
aside time{font:11px var(--mono);color:var(--dim)}
aside .new time{color:var(--red)}
aside li span{font-size:15px;overflow-wrap:anywhere}
@media (max-width:820px){
  main{flex-direction:column;padding:28px 20px;gap:32px}
  aside{width:auto;padding-left:0;border-left:0}
  button{display:block;padding:14px;border:0;background:var(--deep);color:var(--hi);font:inherit}
}
</style>
<main>
  <section class=write>
    <div class=rule></div>
    {{with .Saying}}
    <p class="saying {{$.SayingFit}}">{{.Text}}</p>
    <div class=meta>wisdom.md{{with .Source}} · {{.}}{{end}} · {{$.SeenLabel}}</div>
    {{else}}
    <p class=empty>Add sayings to <code>wisdom.md</code> in the vault, separated by <code>---</code>.</p>
    {{end}}
    <label for=t>Note</label>
    <textarea id=t placeholder="…"></textarea>
    <div id=s></div>
    <button onclick=send()>drop</button>
    <div class=keys><span><b>gi</b> write a note</span><span><b>ctrl+enter</b> drop</span><span><b>:wq</b> dump tabs and quit</span></div>
  </section>
  <aside>
    {{with .WaniKani}}
    <section class="wk{{if .Over}} over{{end}}" aria-label=WaniKani>
      <div class=wk-head><h2>WaniKani</h2><span>{{if .Over}}over {{.Threshold}} — do them{{else}}under {{.Threshold}}{{end}}</span></div>
      <div class=wk-num><b>{{.Reviews}}</b> reviews{{if .Reviews}}<a class=wk-go href="https://www.wanikani.com/subjects/review">review →</a>{{end}}</div>
      <div class=wk-bar><i style="width:{{.BarPct}}%"></i></div>
      <div class=wk-foot>{{if .Lessons}}<a href="https://www.wanikani.com/dashboard">{{.Lessons}} lessons</a>{{else}}0 lessons{{end}} · {{.Next}}{{with .Note}} · {{.}}{{end}}</div>
    </section>
    {{end}}
    <h2 class=lately-h>Lately</h2>
    <ol id=lately>
    {{range .Lately}}<li class="{{if .New}}new{{end}}"><time>{{.When}}{{if .New}} · new{{end}}</time><span>{{.Text}}</span></li>
    {{end}}</ol>
  </aside>
</main>
<script>
// As a browser new-tab page (/?newtab) the textbox must not grab focus:
// that would put vim-style extensions in insert mode and eat navigation keys.
const t=document.getElementById('t'), s=document.getElementById('s');
if(!new URLSearchParams(location.search).has('newtab')) t.focus();
async function send(){
  const text=t.value;
  if(!text.trim())return;
  s.className=''; s.textContent='…';
  try{
    const r=await fetch('/drop',{method:'POST',body:text});
    if(!r.ok){ s.className='err'; s.textContent='failed: '+r.status; return; }
    s.textContent='dropped'; t.value='';
    const ol=document.getElementById('lately');
    ol.querySelector('.new')?.classList.remove('new');
    ol.querySelectorAll('time').forEach(x=>x.textContent=x.textContent.replace(' · new',''));
    const li=document.createElement('li'); li.className='new';
    const tm=document.createElement('time'); tm.textContent='just now · new';
    const sp=document.createElement('span'); sp.textContent=text.trim().split('\n')[0].slice(0,90);
    li.append(tm,sp); ol.prepend(li);
    while(ol.children.length>4) ol.lastElementChild.remove();
  }catch(e){ s.className='err'; s.textContent='failed: '+e; }
}
document.addEventListener('keydown',e=>{
  if((e.metaKey||e.ctrlKey)&&e.key==='Enter')send();
});
</script>
</html>
`))
