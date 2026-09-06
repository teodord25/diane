#!/usr/bin/env bash
# Starts a model server Anton talks to. Every knob is an environment variable
# so one script serves both the fast and the smart model; the systemd units in
# your NixOS config set them from ~/.config/diane/{fast,smart}.env.
#
#   DIANE_GGUF   the model file
#   DIANE_CTX    context window
#   DIANE_EXTRA  per-model flags, word-split on purpose. --reasoning-budget 0
#                for a model whose thinking you do not want to wait for;
#                --n-cpu-moe N to put expert layers in system RAM.
#   DIANE_LLM_HOST=0.0.0.0 lets other machines on the tailnet use it.
set -euo pipefail
# shellcheck disable=SC2086  # DIANE_EXTRA must word-split into separate flags
exec llama-server \
  -m "${DIANE_GGUF:-$HOME/.local/share/diane/gemma-4-12B-it-qat-UD-Q4_K_XL.gguf}" \
  -c "${DIANE_CTX:-8192}" \
  -np 1 \
  -ngl 999 \
  --host "${DIANE_LLM_HOST:-127.0.0.1}" \
  --port "${DIANE_LLM_PORT:-8080}" \
  --jinja --no-webui ${DIANE_EXTRA:-} "$@"
