// diane is a plain-text note vault that lives in a git repository, with a
// secretary, Anton, who works inside it.
//
//	diane drop <text>      capture a note (or pipe it in)
//	diane dictate          capture from the microphone
//	diane photo <file>     capture an image, e.g. a notebook page
//	diane gather           fold new captures into raw.md and inbox.md
//	diane serve            HTTP endpoints for the phone, and a one-page UI
//	diane anton            talk to Anton: typed by default, -v for voice, -t for one-shot
//
// Every machine holds a clone of the vault. Commands pull before they read
// and push after they write, so the clones stay level through the remote and
// nothing needs to be running for that to work. See vault.go for the layout.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config comes entirely from the environment, so the Nix module, the
// devshell and a shell rc can all set it without touching the code.
type Config struct {
	Vault string
	Token string // serve: shared secret for the phone and the page
	Addr  string // serve: listen address

	Backend    string // "local" or "anthropic"
	LLMURL     string // local: OpenAI-compatible chat endpoint
	LLMModel   string // local: model name to request, mostly cosmetic
	SmartURL   string // where a "!" utterance goes instead: another
	SmartModel string // OpenAI-compatible server, or the word "anthropic"
	Timeout    time.Duration
	MaxTokens  int // ceiling per reply. A thinking model needs room for the
	//                 reasoning as well as the answer.
	ClaudeModel  string // anthropic
	APIKey       string // anthropic
	RecBin       string
	Silence      string // seconds of silence that end an utterance
	WhisperBin   string
	WhisperModel string
	Threads      string
	TTSBin       string // `tts <out.wav> <text>`, voice in DIANE_VOICE
	Voice        string
	PlayBin      string
}

func loadConfig() Config {
	home, _ := os.UserHomeDir()
	share := filepath.Join(home, ".local", "share", "diane")
	return Config{
		Vault:        env("DIANE_VAULT", filepath.Join(home, "vault")),
		Token:        os.Getenv("DIANE_TOKEN"),
		Addr:         env("DIANE_ADDR", "0.0.0.0:7777"),
		Backend:      env("DIANE_BACKEND", "local"),
		LLMURL:       env("DIANE_LLM_URL", "http://127.0.0.1:8080/v1/chat/completions"),
		LLMModel:     env("DIANE_LLM_MODEL", "gemma-4-12b-it-qat"),
		SmartURL:     os.Getenv("DIANE_SMART_URL"),
		SmartModel:   env("DIANE_SMART_MODEL", "glm-5.2"),
		Timeout:      duration("DIANE_TIMEOUT", 30*time.Minute),
		MaxTokens:    number("DIANE_MAX_TOKENS", 16384),
		ClaudeModel:  env("DIANE_CLAUDE_MODEL", "claude-haiku-4-5-20251001"),
		APIKey:       os.Getenv("ANTHROPIC_API_KEY"),
		RecBin:       env("DIANE_REC_BIN", "rec"),
		Silence:      env("DIANE_SILENCE", "2.0"),
		WhisperBin:   env("DIANE_WHISPER_BIN", "whisper-cli"),
		WhisperModel: env("DIANE_WHISPER_MODEL", filepath.Join(share, "ggml-base.en.bin")),
		Threads:      env("DIANE_THREADS", "4"),
		TTSBin:       env("DIANE_TTS_BIN", "piper-tts"),
		Voice:        env("DIANE_VOICE", filepath.Join(share, "en_US-lessac-medium.onnx")),
		PlayBin:      env("DIANE_PLAY_BIN", "aplay"),
	}
}

const usage = `diane - a plain-text note vault in a git repo, and Anton, the
secretary who works inside it.

CAPTURE                       fast, offline, never touches the model

  diane drop <text>           capture a note. Also reads stdin, so
                              wl-paste | diane drop drops the clipboard.
  diane dictate               record until you stop talking, transcribe
                              locally with whisper, capture the text.
  diane photo <file> [caption]
                              copy an image into media/ and capture a line
                              pointing at it.

Each capture is written as its own file under drops/, committed, and pushed.
Its own file because git conflicts when two clones edit the same file: this
way your laptop and your PC can both capture while apart, with nothing
running, and never collide.

PROCESS

  diane gather                pull, fold everything in drops/ into raw.md and
                              inbox.md, delete the drops, push. Appends each
                              URL's page title as it goes, so a link you
                              dropped last week is readable without opening
                              it. Anton runs this before every turn, so you
                              rarely need it by hand.

  diane anton                 talk to Anton. Types by default: one line in,
                              one reply out.
       -v                     listen on the microphone instead, in a loop.
       -q                     print replies instead of speaking them.
       -t "..."               handle one utterance and exit. Good for scripts
                              and keybinds.

Each turn sends the whole vault plus your utterance to the model, which
returns what to say and which files to rewrite in full. diane applies that,
commits, pushes. Anton can never see or write raw.md, so a bad generation
can lose an edit but never a capture.

  diane serve                 HTTP endpoint for the phone, plus a one-page
                              browser UI. POST /drop, POST /photo,
                              GET /inbox, GET /?token=<token> for the page.
                              Needs DIANE_TOKEN. Run it wherever is on when
                              you reach for your phone.

SYNC

The vault is a git repo, so the machines talk through your remote, not to
each other. Every command pulls before it reads and pushes after it writes.
An offline push is not an error; the next command carries it.

The one way to get a conflict: run gather or anton on two machines without a
sync in between, since both rewrite inbox.md. diane aborts the rebase, keeps
your commits, and tells you to run git pull --rebase in the vault.

ENVIRONMENT

  DIANE_VAULT        the git clone holding your notes
  DIANE_BACKEND      local (any OpenAI-compatible server) or anthropic
  DIANE_LLM_URL      where the local model server is. Point this at another
                     machine to run the model on your PC and the interface
                     on your laptop.
  DIANE_LLM_MODEL    model name sent to that server
  DIANE_SMART_URL    where a ! utterance goes: another OpenAI-compatible
                     server, or the word anthropic
  DIANE_SMART_MODEL  model name sent to that server
  DIANE_TIMEOUT      how long to wait for a reply, e.g. 30m
  DIANE_MAX_TOKENS   ceiling per reply; a thinking model needs room for the
                     reasoning as well as the answer
  DIANE_CLAUDE_MODEL, ANTHROPIC_API_KEY   used when DIANE_BACKEND=anthropic
  DIANE_TOKEN        shared secret for serve. Same on every device.
  DIANE_ADDR         what serve listens on
  DIANE_SILENCE      seconds of silence that end a dictated utterance
  DIANE_WHISPER_BIN, DIANE_WHISPER_MODEL, DIANE_THREADS      speech in
  DIANE_TTS_BIN, DIANE_VOICE, DIANE_PLAY_BIN, DIANE_REC_BIN  speech out

The README covers the phone shortcuts, the NixOS module and the setup.`

// help prints the usage plus what this machine is actually configured to do,
// which is the question you have when a command misbehaves.
func help(cfg Config) {
	fmt.Println(usage)
	fmt.Printf(`
IN EFFECT HERE

  vault      %s
  backend    %s
  model      %s
  voice      %s
`, cfg.Vault, cfg.Backend, modelDesc(cfg), cfg.TTSBin+" "+cfg.Voice)
}

func modelDesc(cfg Config) string {
	if cfg.Backend == "anthropic" {
		key := "ANTHROPIC_API_KEY set"
		if cfg.APIKey == "" {
			key = "ANTHROPIC_API_KEY MISSING"
		}
		return cfg.ClaudeModel + " (" + key + ")"
	}
	return cfg.LLMModel + " at " + cfg.LLMURL
}

// number parses an integer from the environment.
func number(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

// duration parses a Go duration ("90s", "30m") from the environment.
func duration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func warn(format string, args ...any) { fmt.Fprintf(os.Stderr, "diane: "+format+"\n", args...) }

func die(format string, args ...any) { warn(format, args...); os.Exit(1) }

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(1)
	}
	cmd, args := os.Args[1], os.Args[2:]
	cfg := loadConfig()
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		help(cfg)
		return
	}
	v, err := openVault(cfg.Vault)
	if err != nil {
		die("%v", err)
	}

	switch cmd {
	case "drop":
		text := strings.Join(args, " ")
		if len(args) == 0 {
			b, _ := io.ReadAll(os.Stdin)
			text = string(b)
		}
		if strings.TrimSpace(text) == "" {
			die("nothing to drop")
		}
		e, err := v.drop(text)
		if err != nil {
			die("%v", err)
		}
		fmt.Print(e)

	case "dictate":
		text, err := listen(cfg)
		if err != nil {
			die("%v", err)
		}
		if text == "" {
			die("heard nothing")
		}
		e, err := v.drop(text)
		if err != nil {
			die("%v", err)
		}
		fmt.Print(e)

	case "photo":
		if len(args) == 0 {
			die("usage: diane photo <file> [caption]")
		}
		data, err := os.ReadFile(args[0])
		if err != nil {
			die("%v", err)
		}
		ext, ok := imageExt(data)
		if !ok {
			die("%s is not a recognised image", args[0])
		}
		file := mediaDir + "/" + strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0])) + "-" + nonce() + ext
		text := "[img] " + file
		if c := strings.Join(args[1:], " "); c != "" {
			text += "  " + c
		}
		e, err := v.capture(text, file, data)
		if err != nil {
			die("%v", err)
		}
		fmt.Print(e)

	case "gather":
		n, err := v.gather()
		if err != nil {
			die("%v", err)
		}
		fmt.Printf("gathered %d\n", n)

	case "serve":
		die("%v", serve(cfg, v))

	case "anton":
		fs := flag.NewFlagSet("anton", flag.ExitOnError)
		text := fs.String("t", "", "handle this one utterance and exit")
		voice := fs.Bool("v", false, "listen on the microphone instead of reading stdin")
		quiet := fs.Bool("q", false, "print replies instead of speaking them")
		fs.Parse(args)
		if cfg.Backend == "anthropic" && cfg.APIKey == "" {
			die("DIANE_BACKEND=anthropic but ANTHROPIC_API_KEY is not set")
		}
		anton(cfg, v, *text, *voice, *quiet)

	default:
		die("unknown command %q", cmd)
	}
}

// route sends an utterance prefixed with "!" to the smart backend instead of
// the everyday one: a bigger, slower model for the turns worth waiting for.
// It changes only this turn; the next one is back to the fast model.
func route(cfg Config, utterance string) (Config, string) {
	rest, ok := strings.CutPrefix(utterance, "!")
	if !ok {
		return cfg, utterance
	}
	switch {
	case cfg.SmartURL == "":
		warn("DIANE_SMART_URL is not set; using the everyday model")
	case cfg.SmartURL == "anthropic":
		cfg.Backend = "anthropic"
	default:
		cfg.Backend, cfg.LLMURL, cfg.LLMModel = "local", cfg.SmartURL, cfg.SmartModel
	}
	return cfg, strings.TrimSpace(rest)
}

// anton runs the conversation loop. Typed mode reads a line per turn from
// stdin; voice mode records an utterance per turn.
func anton(cfg Config, v Vault, once string, voice, quiet bool) {
	var history []Message
	if once == "" {
		// A conversation syncs on the way in and on the way out, not four
		// times a turn. Between those two points the vault is local: another
		// machine's captures will not appear mid-conversation, and the
		// commits made here sit unpushed until the exit sync. Nothing is
		// lost if that never happens; they are committed, and the next
		// command pushes them.
		if err := v.syncNow(); err != nil {
			warn("%v", err)
		}
		v.Deferred = true
		defer func() {
			if err := v.syncNow(); err != nil {
				warn("%v", err)
			}
		}()
		// Ctrl-C is the normal way out of the voice loop, so it has to run
		// the exit sync too. os.Exit skips defers, hence the explicit call.
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		go func() {
			<-sigint
			if err := v.syncNow(); err != nil {
				warn("%v", err)
			}
			fmt.Println()
			os.Exit(0)
		}()
	}
	say := func(utterance string) {
		cfg, utterance := route(cfg, utterance)
		reply, err := turn(cfg, v, &history, utterance)
		if err != nil {
			warn("%v", err)
			return
		}
		fmt.Printf("anton: %s\n", reply)
		if !quiet {
			if err := speak(cfg, reply); err != nil {
				warn("%v", err)
			}
		}
	}

	if once != "" {
		say(once)
		return
	}
	warn("vault %s, backend %s", v.Root, cfg.Backend)
	if voice {
		for {
			warn("-- listening --")
			utterance, err := listen(cfg)
			if err != nil {
				die("%v", err)
			}
			if utterance == "" {
				continue
			}
			fmt.Printf("you:   %s\n", utterance)
			say(utterance)
		}
	}
	in := bufio.NewScanner(os.Stdin)
	for fmt.Print("you:   "); in.Scan(); fmt.Print("you:   ") {
		if line := strings.TrimSpace(in.Text()); line != "" {
			say(line)
		}
	}
	fmt.Println()
}
