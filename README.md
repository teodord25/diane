# diane

A plain-text note vault that lives in a git repository, and Anton, the
secretary who works inside it. One Go binary, no dependencies, ~1200 lines plus tests.

```
phone / browser ──HTTP──▶ diane serve ──┐
terminal, keybind ──────▶ diane drop  ──┤ git commit + push
microphone ─────────────▶ diane dictate ┘        │
                                                 ▼
                                    private GitHub repo  ◀──▶  every machine's clone
                                                 ▲
                                                 │ git pull + push
                            diane gather ────────┤   fold captures into raw.md / inbox.md
                            diane anton  ────────┘   model rewrites inbox.md and friends
```

## The vault

```
drops/     one file per capture, not yet folded in. Only ever created and removed.
raw.md     append-only log of every capture ever. Only gather writes here.
inbox.md   the working file. Captures land here too; Anton rewrites it.
media/     photos. A drop line points at them.
todo.md…   whatever else Anton keeps.
```

**Why a file per capture.** Git is the sync, and git conflicts when two clones
change the same file. A capture is a new, uniquely named file, so two machines
capturing while apart never conflict, and nothing has to be running for them
to stay level: each command pulls before it reads and pushes after it writes.

**Why two copies.** `raw.md` is the recovery log: Anton never sees it and can
never write it, so a bad generation cannot destroy a capture. `inbox.md` is
the copy Anton is allowed to eat.

**Where a note can be lost.** Only `raw.md` and `inbox.md` are ever rewritten,
by `gather` and `anton`. Run those on two machines without a sync in between
and the pull will conflict. diane aborts the rebase, keeps your local commits,
and tells you: `git -C ~/vault pull --rebase`, resolve, push. That is the
whole failure mode. Captures are never involved in it.

## Commands

```
diane drop <text>       capture (or: echo text | diane drop)
diane dictate           capture from the microphone
diane photo <file> [caption]
diane gather            fold drops/ into raw.md and inbox.md (Anton does this before every turn)
diane serve             HTTP endpoint for the phone, plus a one-page UI
diane anton             typed conversation; -v voice loop; -t "one utterance"; -q don't speak
```

## Setup

### 1. The vault repo (once)

Create a **private** repo on GitHub, e.g. `vault`, and give every machine a
clone at `~/vault` over SSH:

```sh
git clone git@github.com:<you>/vault.git ~/vault
```

The daemon pushes as the user it runs as, so that user's SSH key must be added
to GitHub (a deploy key with write access on the vault repo is the tidy
option) and `github.com` must already be in `~/.ssh/known_hosts` — do one
manual push from that machine and both are true.

### 2. This repo (each machine)

```sh
nix develop      # or: nix build, and put result/bin on PATH
./setup.sh       # whisper model + piper voice into ~/.local/share/diane
```

Shell rc:

```sh
export DIANE_VAULT="$HOME/vault"
export DIANE_TOKEN=…            # from your password manager, same on every machine
export DIANE_HOST=<pc-tailscale-name>
```

### 3. Roles

**PC — runs the model.** `DIANE_FETCH_MODEL=1 ./setup.sh`, then keep
`DIANE_LLM_HOST=0.0.0.0 ./serve-llm.sh` running (bind to the tailscale IP if
you prefer). That is the PC's only job; you can also use it as an interface
exactly like the laptop.

**Laptop — an interface.** `DIANE_LLM_URL=http://$DIANE_HOST:8080/v1/chat/completions diane anton`.
Whisper and TTS run on the laptop; only the model call crosses the network.
Notes never leave your machines.

**Phone endpoint — `diane serve`.** Runs wherever is on when you reach for
your phone. It needs nothing but a clone and push rights, so it can live on
the PC, the laptop, or an always-on box (a Pi, a Hetzner server); several at
once is fine, they can't conflict. NixOS:

```nix
{
  imports = [ diane.nixosModules.default ];
  services.diane = {
    enable = true;
    vault = "/home/teodor/vault";
    user = "teodor";
    environmentFile = "/etc/diane.env";   # DIANE_TOKEN=…, mode 0400
  };
}
```

### 4. Phone

**HTTP Shortcuts** (F-Droid), all against `http://$DIANE_HOST:7777` with header
`Authorization: Bearer <token>`:

- *diane* — `POST /drop`, body `{{textInput}}` (plain text), toast on success, error dialog on failure
- *→ diane* — same, body `{{sharedText}}`, "add to share menu": sharing a link from any app drops it
- *diane cam* — `POST /photo?caption={{textInput}}`, body type file from camera

For reading and typing in a browser, open `http://$DIANE_HOST:7777/?token=<token>`
once; it sets a cookie and shows a textbox above the inbox. It is deliberately
the least page that works — the endpoints are the interface, restyle freely.

### 5. Keybind

```sh
#!/bin/sh
note=$(: | fuzzel --dmenu --prompt 'diane> ' --lines 0) || exit 0
[ -n "$note" ] && diane drop "$note" >/dev/null \
  && notify-send -t 1500 diane dropped \
  || notify-send -u critical diane "drop FAILED"
```

The failure notification matters. A capture tool that silently loses notes
is worse than none.

## Links

At gather time every URL in a capture gets its page title appended:

```
- 2026-09-04 11:30  https://github.com/go-shiori/go-readability — GitHub - go-shiori/go-readability: …
```

The URL is never altered; if the fetch fails the line stays as captured. Done
at gather rather than at capture so a slow site can't slow a drop.

## Anton

One model call per turn: the whole vault goes in the prompt, the model returns
`{"speak", "writes", "deletes"}` with full-file replacements, diane applies
them under the vault lock, commits, pushes. No tool loop, no agent framework.

A write to a file that changed on disk since the model read it (a gather ran,
a sync pulled) is skipped with a warning: Anton's edit can be redone by
asking again, a note buried under it could not.

Backends, chosen with `DIANE_BACKEND`: `local` (any OpenAI-compatible
`/v1/chat/completions`; default is llama-server on the PC) or `anthropic`
(`ANTHROPIC_API_KEY`, `DIANE_CLAUDE_MODEL`).

TTS is any program invoked as `tts <out.wav> <text>` with the voice in
`DIANE_VOICE`. `bin/piper-tts` is the default; `bin/kokoro-tts` is the
Kokoro-82M one (`pip install kokoro-onnx soundfile`, model files into
`~/.local/share/diane`), set `DIANE_TTS_BIN=kokoro-tts DIANE_VOICE=am_michael`.

## Environment

| variable | default |
|---|---|
| `DIANE_VAULT` | `~/vault` |
| `DIANE_TOKEN` | — (required by `serve`) |
| `DIANE_ADDR` | `0.0.0.0:7777` |
| `DIANE_BACKEND` | `local` |
| `DIANE_LLM_URL` | `http://127.0.0.1:8080/v1/chat/completions` |
| `DIANE_LLM_MODEL` | `qwen2.5-14b-instruct` |
| `DIANE_CLAUDE_MODEL` | `claude-haiku-4-5-20251001` |
| `DIANE_REC_BIN` / `DIANE_SILENCE` | `rec` / `2.0` |
| `DIANE_WHISPER_BIN` / `DIANE_WHISPER_MODEL` / `DIANE_THREADS` | `whisper-cli` / `~/.local/share/diane/ggml-base.en.bin` / `4` |
| `DIANE_TTS_BIN` / `DIANE_VOICE` | `piper-tts` / `~/.local/share/diane/en_US-lessac-medium.onnx` |
| `DIANE_PLAY_BIN` | `aplay` |

## Reading order

`main.go` (config, commands) → `vault.go` (everything about the repo) →
`serve.go` → `anton.go` → `audio.go` → `links.go`. `diane_test.go` runs the
two-clones-through-a-remote scenario against real git.
