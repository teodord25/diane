// diane — append-only note capture over a git-synced vault.
//
// Subcommands: drop, gather, serve, help.
// No dependencies outside the standard library.
//
// Vault layout:
//
//	raw.md    append-only log; nothing ever edits it
//	inbox.md  working file; agents and humans may rewrite it
//	drops/    one file per capture, folded in by `gather`
//	media/    photos moved out of drops/ by `gather`
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const usage = `diane — append-only note capture over a git-synced vault

USAGE
  diane drop [text...]        capture text (or --file PATH, or stdin)
  diane gather                fold drops/ into raw.md + inbox.md
  diane serve                 HTTP capture endpoint + textbox page
  diane help                  this text

DROP FLAGS
  --file PATH                 capture the contents of PATH (repeatable)

ENVIRONMENT
  DIANE_VAULT     vault clone            (default ~/vault)
  DIANE_ADDR      serve listen address   (default 127.0.0.1:7777)
  DIANE_TOKEN     serve auth token       (required unless DIANE_OPEN=1)
  DIANE_OPEN      set to 1 to serve without auth (localhost only)
  DIANE_TIMEOUT   link title fetch timeout (default 10s)
`

// ---------------------------------------------------------------- vault

type Vault struct {
	dir    string
	mu     sync.Mutex // serializes git + file work inside one process
	upOnce sync.Once  // upstream is checked once per process
	up     bool
}

func openVault() (*Vault, error) {
	dir := os.Getenv("DIANE_VAULT")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, "vault")
	}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a git clone (set DIANE_VAULT)", dir)
	}
	return &Vault{dir: dir}, nil
}

func (v *Vault) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = v.dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// hasUpstream reports whether the branch tracks anything. A clone with no
// remote is a legitimate setup (single machine), so it should be quiet, not
// an error on every command.
func (v *Vault) hasUpstream() bool {
	v.upOnce.Do(func() {
		_, err := v.git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
		v.up = err == nil
		if !v.up {
			warn("no upstream configured; working locally")
		}
	})
	return v.up
}

// abortRebase undoes a conflicted `pull --rebase`. Without this, git stays
// mid-rebase and every later commit compounds the mess. Local commits are
// kept; the merge is the user's problem, and only once.
func (v *Vault) abortRebase() bool {
	gitDir, err := v.git("rev-parse", "--git-dir")
	if err != nil {
		return false
	}
	base := strings.TrimSpace(gitDir)
	if !filepath.IsAbs(base) {
		base = filepath.Join(v.dir, base)
	}
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(base, d)); err != nil {
			continue
		}
		if _, err := v.git("rebase", "--abort"); err != nil {
			warn("rebase in progress and --abort failed: %v", err)
			warn("resolve by hand in %s before capturing again", v.dir)
			return true
		}
		warn("pull conflicted (raw.md/inbox.md diverged); rebase aborted, "+
			"local commits kept — reconcile %s by hand", v.dir)
		return true
	}
	return false
}

// pull is best-effort: a capture must never be lost because the network is
// down. Failures are reported and execution continues.
func (v *Vault) pull() {
	if !v.hasUpstream() {
		return
	}
	if _, err := v.git("pull", "--rebase", "--autostash"); err != nil {
		if !v.abortRebase() {
			warn("pull failed, continuing offline: %v", err)
		}
	}
}

// push is likewise best-effort. The commit is what matters; the push can
// happen on the next command.
func (v *Vault) push(msg string) {
	if _, err := v.git("add", "-A"); err != nil {
		warn("%v", err)
		return
	}
	if _, err := v.git("diff", "--cached", "--quiet"); err == nil {
		return // nothing staged
	}
	if _, err := v.git("commit", "-m", msg); err != nil {
		warn("%v", err)
		return
	}
	if !v.hasUpstream() {
		return
	}
	if _, err := v.git("push"); err != nil {
		warn("push failed, commit is local: %v", err)
	}
}

func (v *Vault) path(parts ...string) string {
	return filepath.Join(append([]string{v.dir}, parts...)...)
}

func (v *Vault) appendTo(name, text string) error {
	f, err := os.OpenFile(v.path(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

// ---------------------------------------------------------------- drop

// dropName is timestamp-host-nonce so that two machines capturing at the same
// second still produce different files. That is the whole conflict story:
// captures are never edited, so git has nothing to merge.
func dropName(ext string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	host = strings.NewReplacer("/", "-", " ", "-", ".", "-").Replace(host)
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%s-%d%s", time.Now().UTC().Format("20060102T150405Z"), host, time.Now().UnixNano()%65536, ext)
	}
	return fmt.Sprintf("%s-%s-%s%s",
		time.Now().UTC().Format("20060102T150405Z"), host, hex.EncodeToString(b[:]), ext)
}

func (v *Vault) writeDrop(text, ext string, raw []byte) (string, error) {
	if err := os.MkdirAll(v.path("drops"), 0o755); err != nil {
		return "", err
	}
	name := dropName(ext)
	body := raw
	if body == nil {
		body = []byte(strings.TrimRight(text, "\n") + "\n")
	}
	if err := os.WriteFile(v.path("drops", name), body, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func (v *Vault) drop(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("nothing to drop")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pull()
	name, err := v.writeDrop(text, ".md", nil)
	if err != nil {
		return "", err
	}
	v.push("drop " + name)
	return name, nil
}

func cmdDrop(v *Vault, args []string) error {
	var chunks []string
	var words []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--file" || args[i] == "-f" {
			if i+1 >= len(args) {
				return errors.New("--file needs a path")
			}
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				return err
			}
			chunks = append(chunks, string(b))
			i++
			continue
		}
		words = append(words, args[i])
	}
	if len(words) > 0 {
		chunks = append(chunks, strings.Join(words, " "))
	}
	if len(chunks) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		chunks = append(chunks, string(b))
	}
	name, err := v.drop(strings.Join(chunks, "\n\n"))
	if err != nil {
		return err
	}
	fmt.Println(name)
	return nil
}

// ---------------------------------------------------------------- gather

var urlRe = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)
var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
var wsRe = regexp.MustCompile(`\s+`)

func fetchTitle(url string) string {
	timeout := 10 * time.Second
	if s := os.Getenv("DIANE_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			timeout = d
		}
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "diane/1 (+note capture)")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return ""
	}
	m := titleRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	title := wsRe.ReplaceAllString(html.UnescapeString(string(m[1])), " ")
	return strings.TrimSpace(title)
}

// label annotates bare URLs with the page title, so a dump of links is
// readable (and clusterable) without opening any of them.
func label(text string) string {
	seen := map[string]bool{}
	for _, url := range urlRe.FindAllString(text, -1) {
		if seen[url] {
			continue
		}
		seen[url] = true
		if title := fetchTitle(url); title != "" {
			text = strings.Replace(text, url, url+" — "+title, 1)
		}
	}
	return text
}

// stampOf turns 20260921T142233Z-host-ab12 back into a readable header.
func stampOf(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.SplitN(base, "-", 2)
	t, err := time.Parse("20060102T150405Z", parts[0])
	if err != nil {
		return base
	}
	host := ""
	if len(parts) > 1 {
		if i := strings.LastIndex(parts[1], "-"); i > 0 {
			host = " (" + parts[1][:i] + ")"
		}
	}
	return t.Local().Format("2006-01-02 15:04") + host
}

func cmdGather(v *Vault) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pull()

	entries, err := os.ReadDir(v.path("drops"))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("nothing to gather")
			return nil
		}
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Println("nothing to gather")
		return nil
	}

	var b strings.Builder
	for _, name := range names {
		if filepath.Ext(name) != ".md" {
			// Media: move it out of drops/ and leave a reference behind.
			if err := os.MkdirAll(v.path("media"), 0o755); err != nil {
				return err
			}
			if err := os.Rename(v.path("drops", name), v.path("media", name)); err != nil {
				return err
			}
			fmt.Fprintf(&b, "\n## %s\n\n![](media/%s)\n", stampOf(name), name)
			continue
		}
		raw, err := os.ReadFile(v.path("drops", name))
		if err != nil {
			return err
		}
		body := strings.TrimSpace(label(string(raw)))
		if body == "" {
			os.Remove(v.path("drops", name))
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", stampOf(name), body)
	}

	if b.Len() > 0 {
		if err := v.appendTo("raw.md", b.String()); err != nil {
			return err
		}
		if err := v.appendTo("inbox.md", b.String()); err != nil {
			return err
		}
	}
	for _, name := range names {
		if filepath.Ext(name) == ".md" {
			os.Remove(v.path("drops", name))
		}
	}
	v.push(fmt.Sprintf("gather %d", len(names)))
	fmt.Printf("gathered %d\n", len(names))
	return nil
}

// ---------------------------------------------------------------- serve

const page = `<!doctype html>
<meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>diane</title>
<style>
 :root{color-scheme:dark light}
 body{margin:0;font:16px/1.4 system-ui,sans-serif;display:flex;
      flex-direction:column;height:100dvh}
 textarea{flex:1;width:100%;box-sizing:border-box;border:0;padding:1rem;
          font:inherit;resize:none;background:transparent;color:inherit}
 textarea:focus{outline:0}
 button{padding:1rem;border:0;font:inherit;background:#3a6ea5;color:#fff}
 #s{padding:.5rem 1rem;opacity:.7;font-size:.85rem}
</style>
<textarea id=t autofocus placeholder="…"></textarea>
<div id=s></div>
<button onclick=send()>drop</button>
<script>
async function send(){
  const t=document.getElementById('t'), s=document.getElementById('s');
  if(!t.value.trim())return;
  s.textContent='…';
  try{
    const r=await fetch('/drop',{method:'POST',body:t.value});
    s.textContent = r.ok ? 'dropped' : 'failed: '+r.status;
    if(r.ok) t.value='';
  }catch(e){ s.textContent='failed: '+e; }
  t.focus();
}
document.addEventListener('keydown',e=>{
  if((e.metaKey||e.ctrlKey)&&e.key==='Enter')send();
});
</script>
`

type server struct {
	v     *Vault
	token string
}

func (s *server) authed(w http.ResponseWriter, r *http.Request) bool {
	if s.token == "" {
		return true // DIANE_OPEN
	}
	if t := r.URL.Query().Get("token"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1 {
			http.SetCookie(w, &http.Cookie{
				Name: "diane", Value: s.token, Path: "/",
				MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			return true
		}
	}
	if c, err := r.Cookie("diane"); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1 {
			return true
		}
	}
	http.Error(w, "nope", http.StatusUnauthorized)
	return false
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, page)
}

func (s *server) drop(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	text := string(body)
	if v := r.FormValue("text"); v != "" { // tolerate form posts too
		text = v
	}
	name, err := s.v.drop(text)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fmt.Fprintln(w, name)
}

func (s *server) photo(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f, hdr, err := r.FormFile("photo")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if ext == "" {
		ext = ".jpg"
	}
	s.v.mu.Lock()
	defer s.v.mu.Unlock()
	s.v.pull()
	name, err := s.v.writeDrop("", ext, raw)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.v.push("photo " + name)
	fmt.Fprintln(w, name)
}

func (s *server) inbox(w http.ResponseWriter, r *http.Request) {
	if !s.authed(w, r) {
		return
	}
	s.v.mu.Lock()
	defer s.v.mu.Unlock()
	s.v.pull()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if b, err := os.ReadFile(s.v.path("inbox.md")); err == nil {
		w.Write(b)
	}
	entries, err := os.ReadDir(s.v.path("drops"))
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".md" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "\n\n--- pending (%d) ---\n", len(names))
	for _, name := range names {
		if b, err := os.ReadFile(s.v.path("drops", name)); err == nil {
			fmt.Fprintf(w, "\n## %s\n\n%s\n", stampOf(name), strings.TrimSpace(string(b)))
		}
	}
}

func cmdServe(v *Vault) error {
	token := os.Getenv("DIANE_TOKEN")
	if token == "" && os.Getenv("DIANE_OPEN") != "1" {
		return errors.New("set DIANE_TOKEN, or DIANE_OPEN=1 to serve without auth")
	}
	addr := os.Getenv("DIANE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:7777"
	}
	s := &server{v: v, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("POST /drop", s.drop)
	mux.HandleFunc("POST /photo", s.photo)
	mux.HandleFunc("GET /inbox", s.inbox)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "diane serving %s on %s\n", v.dir, addr)
	return srv.ListenAndServe()
}

// ---------------------------------------------------------------- main

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "diane: "+format+"\n", args...)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def + " (default)"
}

func cmdHelp(v *Vault) {
	fmt.Print(usage)
	fmt.Println("\nIN EFFECT HERE")
	dir := "unset"
	if v != nil {
		dir = v.dir
	}
	fmt.Printf("  vault    %s\n", dir)
	fmt.Printf("  addr     %s\n", envOr("DIANE_ADDR", "127.0.0.1:7777"))
	token := "unset"
	if os.Getenv("DIANE_TOKEN") != "" {
		token = "set"
	} else if os.Getenv("DIANE_OPEN") == "1" {
		token = "unset, DIANE_OPEN=1"
	}
	fmt.Printf("  token    %s\n", token)
	fmt.Printf("  timeout  %s\n", envOr("DIANE_TIMEOUT", "10s"))
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"help"}
	}

	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		v, _ := openVault() // help works without a vault
		cmdHelp(v)
		return
	}

	v, err := openVault()
	if err != nil {
		warn("%v", err)
		os.Exit(1)
	}

	switch args[0] {
	case "drop":
		err = cmdDrop(v, args[1:])
	case "gather":
		err = cmdGather(v)
	case "serve":
		err = cmdServe(v)
	default:
		err = fmt.Errorf("unknown command %q (try: diane help)", args[0])
	}
	if err != nil {
		warn("%v", err)
		os.Exit(1)
	}
}
