#!/usr/bin/env bash
# Starts the local model server Anton talks to. Leave it running on the PC.
# DIANE_LLM_HOST=0.0.0.0 (or the PC's tailscale IP) lets the laptop use it too.
set -euo pipefail
exec llama-server \
  -m "${DIANE_GGUF:-$HOME/.local/share/diane/Qwen2.5-14B-Instruct-Q4_K_M.gguf}" \
  -c "${DIANE_CTX:-8192}" \
  -ngl 999 \
  --host "${DIANE_LLM_HOST:-127.0.0.1}" \
  --port "${DIANE_LLM_PORT:-8080}" \
  --jinja --no-webui
