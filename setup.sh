#!/usr/bin/env bash
# Fetches the speech model and a voice into ~/.local/share/diane. Idempotent.
# ~210 MB. The LLM weights (~9 GB) are opt-in: DIANE_FETCH_MODEL=1 ./setup.sh
set -euo pipefail
share="$HOME/.local/share/diane"
mkdir -p "$share"

fetch() { # url dest
  if [ -s "$2" ]; then echo "have  $2"; return; fi
  echo "fetch $2"
  curl -fL --progress-bar -o "$2.part" "$1" && mv "$2.part" "$2"
}

fetch https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin "$share/ggml-base.en.bin"
voice=https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium
fetch "$voice/en_US-lessac-medium.onnx" "$share/en_US-lessac-medium.onnx"
fetch "$voice/en_US-lessac-medium.onnx.json" "$share/en_US-lessac-medium.onnx.json"

if [ "${DIANE_FETCH_MODEL:-0}" = 1 ]; then
  fetch https://huggingface.co/bartowski/Qwen2.5-14B-Instruct-GGUF/resolve/main/Qwen2.5-14B-Instruct-Q4_K_M.gguf \
        "$share/Qwen2.5-14B-Instruct-Q4_K_M.gguf"
else
  echo "skip  Qwen2.5-14B-Instruct-Q4_K_M.gguf (DIANE_FETCH_MODEL=1 to fetch, ~9 GB)"
fi
