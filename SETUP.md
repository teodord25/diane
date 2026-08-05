# diane — setup

## 1. Vault

Whatever directory Anton already reads. `.lock` must not be tracked:

```bash
cd ~/vault
echo '.lock' >> .gitignore
touch raw.md inbox.md
git add .gitignore raw.md inbox.md && git commit -m "diane: init"
```

## 2. Secret

```bash
head -c 32 /dev/urandom | base64 > /tmp/tok
printf 'DIANE_TOKEN=%s\n' "$(cat /tmp/tok)" | sudo tee /etc/diane.env
sudo chmod 400 /etc/diane.env
```

Put the same token in your password manager — you need it on the phone and on
every machine.

## 3. Host config

```nix
{
  imports = [ diane.nixosModules.default ];

  services.diane = {
    enable = true;
    vault = "/home/teodor/vault";
    user = "teodor";
    environmentFile = "/etc/diane.env";
  };
}
```

Verify: `curl http://localhost:7777/health` → `ok`.

## 4. Terminal (every machine)

In your shell config. `DIANE_TOKEN` and `DIANE_HOST` come from your private env
file, not from the dotfiles repo.

```sh
d() {
  if [ $# -eq 0 ]; then
    echo "usage: d <note>" >&2
    return 1
  fi
  curl -sS --max-time 5 \
    -H "Authorization: Bearer $DIANE_TOKEN" \
    --data-binary "$*" \
    "http://$DIANE_HOST:7777/drop"
}

# read it back
dr() {
  curl -sS -H "Authorization: Bearer $DIANE_TOKEN" "http://$DIANE_HOST:7777/inbox"
}
```

Usage: `d fix the dramatiq retry thing`

## 5. Hyprland keybind

`~/.local/bin/diane-prompt`:

```sh
#!/bin/sh
note=$(: | fuzzel --dmenu --prompt 'diane> ' --lines 0) || exit 0
[ -n "$note" ] || exit 0
curl -sS --max-time 5 \
  -H "Authorization: Bearer $DIANE_TOKEN" \
  --data-binary "$note" \
  "http://$DIANE_HOST:7777/drop" >/dev/null \
  && notify-send -t 1500 "diane" "dropped" \
  || notify-send -u critical "diane" "drop FAILED"
```

```
bind = SUPER, N, exec, ~/.local/bin/diane-prompt
```

The failure notification matters. A capture tool that silently loses notes is
worse than no capture tool.

## 5b. Photos

`POST /photo` with raw image bytes. JPEG, PNG, WebP and HEIC are detected by
magic bytes — the `Content-Type` header is ignored, because phone clients set
it carelessly. Optional `?caption=`.

The image lands in `media/`, and a line goes in the log:

```
- 2026-07-31 11:40  [img] media/2026-07-31-114007.png  diane architecture sketch
```

No OCR. The photo is captured instantly and the transcription problem is
deferred until you actually want the text, at which point Anton can do it on
demand.

```sh
dpic() {
  f="$1"; shift
  curl -sS --max-time 30 \
    -H "Authorization: Bearer $DIANE_TOKEN" \
    --data-binary "@$f" \
    "http://$DIANE_HOST:7777/photo?caption=$(printf %s "$*" | jq -sRr @uri)"
}
```

**Repo growth is the thing to watch.** Photos go into git like everything else,
which is fine at a few sketches a week and not fine if you start dumping camera
rolls in. If `.git` gets uncomfortable, move `media/` out of the repo and keep
only the log line — the design doesn't depend on the images being versioned.

## 6. Phone

**HTTP Shortcuts** (F-Droid). Two shortcuts, same endpoint:

*Shortcut A — "diane"* (home screen icon)
- Method `POST`, URL `http://<tailscale-name>:7777/drop`
- Header `Authorization: Bearer <token>`
- Body: `{{textInput}}` with a "Ask for text" variable, request body type "plain text"
- Response handling: toast on success, error dialog on failure

*Shortcut B — "→ diane"* (share target)
- Same as A, body `{{sharedText}}`
- Enable "Add to share menu"

*Shortcut C — "diane cam"* (sketches)
- Method `POST`, URL `http://<tailscale-name>:7777/photo`
- Request body type "file", file source: camera / image picker
- Optionally append `?caption={{textInput}}` and prompt for it

Now sharing a link from any app drops it in the vault, and a photo of a
notebook page is two taps.

## 7. Reading on the phone

`http://<tailscale-name>:7777/inbox` in the browser. Plain text, no app.
The token has to go in a header, so either use HTTP Shortcuts with a
"display response" action, or write your own page later — the endpoint stays
dumb on purpose.

## 8. Anton changes

Three edits, none of them large:

1. **Exclude `raw.md` and `media/` from the vault-to-prompt walk.** Anton can't
   rewrite a file it never sees, and it halves the context growth. `media/` is
   binary and would destroy the prompt outright — its existing non-text
   extension check covers writes, not the read walk.
2. **Deny-list `raw.md` in `safeJoin`'s write path**, next to the existing
   `..` and extension checks. Reject, don't clamp — same bug you already fixed
   once.
3. **Add `ReadOnlyPaths = [ "/home/teodor/vault/raw.md" ]`** to Anton's
   systemd unit. Holds even if the Go code regresses.

Also: Anton must take `vault/.lock` via `syscall.Flock` before its full-file
writes, and re-read inside the lock. Without this, a drop that lands between
Anton's read and write gets clobbered.

## Verified

`go vet` and `go build` clean; end-to-end tested against a real git repo:
auth rejection (401), empty note rejection (400), multi-line entries indented
correctly, `raw.md` and `inbox.md` byte-identical, one commit per drop,
12 parallel drops all landed with no interleaving. Latency 10–60 ms per drop
including the git commit, so there's no reason to move committing off the
request path.

Photo path tested too: PNG and JPEG stored byte-identical, non-images rejected
(415), unauthed rejected (401), captions landing in the log line, images
staged and committed, and a burst of five uploads inside the same second
getting distinct filenames with every logged path resolving to a real file.

The flake and the module are **not** evaluated — no nix in the environment I
built this in. Expect to fix a typo on first `nixos-rebuild`.
