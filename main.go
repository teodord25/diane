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
	"path/filepath"
	"strings"
)

// Config comes entirely from the environment, so the Nix module, the
// devshell and a shell rc can all set it without touching the code.
type Config struct {
	Vault string
	Token string // serve: shared secret for the phone and the page
	Addr  string // serve: listen address

	Backend      string // "local" or "anthropic"
	LLMURL       string // local: OpenAI-compatible chat endpoint
	LLMModel     string // local: model name to request, mostly cosmetic
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
		LLMModel:     env("DIANE_LLM_MODEL", "qwen2.5-14b-instruct"),
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
		die("usage: diane drop|dictate|photo|gather|serve|anton")
	}
	cmd, args := os.Args[1], os.Args[2:]
	cfg := loadConfig()
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

// anton runs the conversation loop. Typed mode reads a line per turn from
// stdin; voice mode records an utterance per turn.
func anton(cfg Config, v Vault, once string, voice, quiet bool) {
	var history []Message
	say := func(utterance string) {
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
