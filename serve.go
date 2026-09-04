package main

import (
	"crypto/subtle"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// serve is the vault's door for things that cannot run git themselves: the
// phone, a browser. Every request is authenticated with the token, given as
// `Authorization: Bearer <token>` or, for the browser page, a cookie set by
// visiting /?token=<token> once.
//
//	POST /drop            body is the note              -> the entry as written
//	POST /photo?caption=  body is the image             -> the entry as written
//	GET  /inbox           inbox.md plus pending drops   -> text/plain
//	GET  /                textbox above the inbox       -> text/html
//	GET  /health          -> ok
func serve(cfg Config, v Vault) error {
	if cfg.Token == "" {
		return fmt.Errorf("DIANE_TOKEN is not set")
	}
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if c, err := r.Cookie("diane"); err == nil && got == "" {
				got = c.Value
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.Token)) != 1 {
				http.Error(w, "unauthorized: open /?token=<token> once, or send a bearer token", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	read := func(w http.ResponseWriter) (string, bool) {
		v.locked(v.sync) // show what the other machines have done, not just this clone
		inbox, _ := os.ReadFile(v.path(inboxFile))
		pend, err := v.pending()
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return "", false
		}
		return string(inbox) + pend, true
	}
	reply := func(w http.ResponseWriter, entry string, err error) {
		if err != nil {
			log.Printf("capture failed: %v", err)
			http.Error(w, "write failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, entry)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /drop", auth(func(w http.ResponseWriter, r *http.Request) {
		text, ok := body(w, r, 64<<10)
		if !ok {
			return
		}
		if strings.TrimSpace(string(text)) == "" {
			http.Error(w, "empty note", http.StatusBadRequest)
			return
		}
		e, err := v.drop(string(text))
		reply(w, e, err)
	}))
	mux.HandleFunc("POST /photo", auth(func(w http.ResponseWriter, r *http.Request) {
		data, ok := body(w, r, 20<<20)
		if !ok {
			return
		}
		ext, ok := imageExt(data)
		if !ok {
			http.Error(w, "not a recognised image", http.StatusUnsupportedMediaType)
			return
		}
		file := fmt.Sprintf("%s/%s-%s%s", mediaDir, time.Now().Format(fileFmt), nonce(), ext)
		text := "[img] " + file
		if c := strings.TrimSpace(r.URL.Query().Get("caption")); c != "" {
			text += "  " + c
		}
		e, err := v.capture(text, file, data)
		reply(w, e, err)
	}))
	mux.HandleFunc("GET /inbox", auth(func(w http.ResponseWriter, r *http.Request) {
		if text, ok := read(w); ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			io.WriteString(w, text)
		}
	}))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("token"); t != "" {
			http.SetCookie(w, &http.Cookie{Name: "diane", Value: t, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		auth(func(w http.ResponseWriter, r *http.Request) {
			if text, ok := read(w); ok {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, page, html.EscapeString(text))
			}
		})(w, r)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	srv := &http.Server{Addr: cfg.Addr, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second}
	log.Printf("diane listening on %s, vault %s", cfg.Addr, v.Root)
	return srv.ListenAndServe()
}

func body(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return nil, false
	}
	if int64(len(b)) > max {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return b, true
}

// imageExt sniffs magic bytes rather than trusting Content-Type, which phone
// clients set carelessly.
func imageExt(b []byte) (string, bool) {
	s := string(b)
	switch {
	case strings.HasPrefix(s, "\xFF\xD8\xFF"):
		return ".jpg", true
	case strings.HasPrefix(s, "\x89PNG\r\n\x1a\n"):
		return ".png", true
	case len(s) >= 12 && s[:4] == "RIFF" && s[8:12] == "WEBP":
		return ".webp", true
	case len(s) >= 12 && s[4:8] == "ftyp" && strings.Contains("heic heix mif1 msf1", s[8:12]):
		return ".heic", true
	}
	return "", false
}

// The page is deliberately the least it can be: a textbox, and the inbox
// below it. Restyle it, or replace it; the endpoints are the interface.
const page = `<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>diane</title>
<style>
body{font:16px/1.4 monospace;max-width:44em;margin:1em auto;padding:0 1em;background:#111;color:#ddd}
textarea{width:100%%;height:5em;font:inherit;background:#000;color:#eee;border:1px solid #444;padding:.5em}
button{font:inherit;padding:.4em 1em;margin:.5em 0}
pre{white-space:pre-wrap;word-break:break-word;border-top:1px solid #444;padding-top:1em}
</style>
<textarea id="t" autofocus placeholder="diane…"></textarea>
<button id="b">drop</button>
<pre>%s</pre>
<script>
const t=document.getElementById('t'),b=document.getElementById('b');
b.onclick=async()=>{if(!t.value.trim())return;b.disabled=true;
const r=await fetch('/drop',{method:'POST',body:t.value});
if(r.ok)location.reload();else{alert('drop FAILED: '+r.status);b.disabled=false}};
t.onkeydown=e=>{if(e.key==='Enter'&&(e.ctrlKey||e.metaKey))b.onclick()};
</script>
`
