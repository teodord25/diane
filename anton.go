package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// Anton is the secretary. Every turn he is shown the whole vault and one
// utterance, and answers with one JSON object: what to say, which files to
// replace in full, which to delete. One round trip per turn, no tool loop.

const systemPrompt = `You are Anton, a terse personal secretary.

You look after one small vault of plain-text notes and to-do lists belonging to
one person. Every turn you are given the entire vault and one utterance. The
utterance may have been transcribed from speech, so it may contain small
errors. Infer the obvious intent and act on it; only ask for clarification when
the request is genuinely ambiguous.

inbox.md is where new captures land, newest last. When asked to file, sort or
process the inbox, move each item into the file where it belongs and remove it
from inbox.md.

Reply with a single JSON object and nothing else. No prose, no code fence.

{
  "speak":   "what to say out loud",
  "edits":   [{"path": "todo.md", "search": "- old text", "replace": "- new text"}],
  "moves":   [{"from": "inbox.md", "lines": [3, 7, 12], "to": "topics/games.md"}],
  "deletes": ["obsolete.md"]
}

Every file in the vault is shown to you with a line number before each line.
The numbers are not part of the file; they are how you point at a line.

Filing and sorting:

- To move whole lines from one file to another, use "moves". Give the line
  numbers as shown, and they are cut from "from" and appended to "to". The
  destination file is created if it does not exist.
- Prefer a move over an edit whenever you are relocating a line unchanged.
  Sorting a long list into topics is a handful of moves, one per topic, not
  hundreds of edits.
- Line numbers refer to the files exactly as shown to you. Do not renumber as
  you go; every move is resolved against what you were given.
- Use an edit, not a move, when the text itself has to change.

Editing:

- Each edit changes one file in one place. "search" must be text that appears
  in that file exactly once, copied character for character. "replace" is what
  it becomes.
- To add to the end of a file, or to create a file, use an empty "search" and
  put the new text in "replace".
- To remove something, put the text in "search" and leave "replace" empty.
- Several edits may touch the same file; they are applied in the order given.
- Never quote a whole file in "search" when a single line identifies the place.
- If any edit or move fails, none of them are applied, so make every "search"
  exact and every line number one you were actually shown.

Rules:

- "speak" is required. Always say something, even if only "Done."
- "speak" must stand alone. Never end it with a colon or a promise of content
  that only exists in a file: if you were asked to produce something, say it in
  "speak" as well as writing it.
- Use empty lists when you are changing nothing.
- Paths are relative to the vault root, must stay inside it, and must end in
  .md, .txt or .org.
- "speak" is fed to a speech synthesiser. Plain spoken prose only: no markdown,
  no asterisks, no hyphens as bullets, no backticks, no URLs, no file paths.
  Write list positions as words: "one", "two", "three".
- Be brief. One or two sentences, unless you are reading a list back.
- If the person only asked a question, answer it and change nothing.
- Preserve the vault's existing formatting and file layout. Do not tidy,
  reorganise or rename anything unless you were asked to.
- Prefer adding to an existing file over creating a new one.`

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// An Edit replaces one exact stretch of text in one file. Search must match
// the file in exactly one place; an empty Search appends instead, creating the
// file if it does not exist, and an empty Replace deletes what it matched.
//
// Edits rather than whole new file contents because a model asked to re-emit a
// 250-line file will quietly emit a shortened one when it runs out of room.
// With an edit, a truncated reply produces a Search that does not match, and
// apply refuses it loudly instead of writing seven lines over two hundred.
type Edit struct {
	Path    string `json:"path"`
	Search  string `json:"search"`
	Replace string `json:"replace"`
}

// A Move takes whole lines out of one file and appends them to another,
// naming them by the line numbers shown in the prompt.
//
// This exists because filing a long list is two jobs: deciding where each line
// belongs, which needs a model, and cutting and pasting it, which does not.
// An Edit makes the model retype every line it moves, so a hundred lines is
// thousands of tokens of exact transcription and one typo away from failing.
// A Move is a list of integers.
type Move struct {
	From  string `json:"from"`
	Lines []int  `json:"lines"`
	To    string `json:"to"`
}

type Reply struct {
	Speak   string   `json:"speak"`
	Edits   []Edit   `json:"edits"`
	Moves   []Move   `json:"moves"`
	Deletes []string `json:"deletes"`
}

// turn is one complete interaction: fold in new captures, read the vault, ask
// the model, apply what it decided, and return what to say.
func turn(cfg Config, v Vault, history *[]Message, utterance string) (string, error) {
	if _, err := v.gather(); err != nil {
		return "", fmt.Errorf("gather: %w", err)
	}
	files, err := v.load()
	if err != nil {
		return "", fmt.Errorf("read vault: %w", err)
	}
	reply, err := ask(cfg, files, *history, utterance)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(reply.Speak) == "" {
		reply.Speak = "Done."
	}
	*history = append(*history,
		Message{Role: "user", Content: utterance},
		Message{Role: "assistant", Content: reply.Speak},
	)
	if len(*history) > 8 {
		*history = (*history)[len(*history)-8:]
	}
	// An edit that will not apply makes whatever anton just said untrue, so
	// say that instead of letting the claim stand.
	n, err := v.apply(files, reply, utterance)
	switch {
	case err != nil:
		return "I could not make that change. " + err.Error(), nil
	case n > 0:
		warn("%d file(s) changed", n)
	}
	return reply.Speak, nil
}

func ask(cfg Config, files []File, history []Message, utterance string) (*Reply, error) {
	switch cfg.Backend {
	case "local":
		return askLocal(cfg, files, history, utterance)
	case "anthropic":
		return askAnthropic(cfg, files, history, utterance)
	}
	return nil, fmt.Errorf("DIANE_BACKEND=%q, want local or anthropic", cfg.Backend)
}

// loaded asks the local server which model it actually has open. The
// configured name is only a label, and which model is running is decided
// outside diane, so this is the only honest answer to "which model is this".
func loaded(cfg Config) string {
	if cfg.Backend == "anthropic" {
		return cfg.ClaudeModel
	}
	base, ok := strings.CutSuffix(cfg.LLMURL, "/chat/completions")
	if !ok {
		return cfg.LLMModel
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/models")
	if err != nil {
		return cfg.LLMModel + " (server unreachable)"
	}
	defer resp.Body.Close()
	var parsed struct {
		Data []struct{ ID string } `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil || len(parsed.Data) == 0 {
		return cfg.LLMModel
	}
	// llama-server reports the model's path; the file name is the useful part.
	return path.Base(parsed.Data[0].ID)
}

// prompt renders the whole vault plus the utterance, identically for both backends.
func prompt(files []File, utterance string) string {
	var b strings.Builder
	b.WriteString("<vault>\n")
	for _, f := range files {
		fmt.Fprintf(&b, "<file path=%q>\n", f.Path)
		// Numbered, because a move refers to lines by number. The numbers are
		// not part of the file; they are how the model points at it.
		for i, line := range lines(f.Content) {
			fmt.Fprintf(&b, "%d\t%s\n", i+1, line)
		}
		b.WriteString("</file>\n")
	}
	if len(files) == 0 {
		b.WriteString("(the vault is empty)\n")
	}
	fmt.Fprintf(&b, "</vault>\n\n<utterance>\n%s\n</utterance>", utterance)
	return b.String()
}

// post sends JSON and returns the response body, turning HTTP-level failures
// into errors that name the server that failed.
func post(cfg Config, url string, headers map[string]string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	resp, err := (&http.Client{Timeout: cfg.Timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s unreachable: %w", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s: %s", url, resp.Status, truncate(string(out), 400))
	}
	return out, nil
}

// --- local: any OpenAI-compatible /v1/chat/completions (llama-server, Ollama, vLLM)

func askLocal(cfg Config, files []File, history []Message, utterance string) (*Reply, error) {
	msgs := append([]Message{{Role: "system", Content: systemPrompt}}, history...)
	msgs = append(msgs, Message{Role: "user", Content: prompt(files, utterance)})
	out, err := post(cfg, cfg.LLMURL, nil, map[string]any{
		"model":       cfg.LLMModel,
		"messages":    msgs,
		"temperature": 0,
		"max_tokens":  cfg.MaxTokens,
		"stream":      false,
		// Constrains llama-server to valid JSON. parseReply still guards
		// against servers that ignore it.
		"response_format": map[string]string{"type": "json_object"},
	})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("local server returned no completion: %s", truncate(string(out), 400))
	}
	return parseReply(parsed.Choices[0].Message.Content)
}

// --- anthropic

func askAnthropic(cfg Config, files []File, history []Message, utterance string) (*Reply, error) {
	msgs := append(append([]Message{}, history...),
		Message{Role: "user", Content: prompt(files, utterance)},
		Message{Role: "assistant", Content: "{"}, // prefill so the model emits bare JSON
	)
	out, err := post(cfg, "https://api.anthropic.com/v1/messages", map[string]string{
		"x-api-key":         cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}, map[string]any{
		"model":      cfg.ClaudeModel,
		"max_tokens": cfg.MaxTokens,
		"system": []map[string]any{{
			"type": "text", "text": systemPrompt,
			"cache_control": map[string]string{"type": "ephemeral"},
		}},
		"messages": msgs,
	})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Content []struct{ Type, Text string } `json:"content"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("api returned: %s", truncate(string(out), 400))
	}
	var text strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return parseReply("{" + text.String())
}

// parseReply tolerates a code fence or stray prose around the JSON, which the
// local model occasionally emits despite being told not to.
func parseReply(raw string) (*Reply, error) {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	if i := strings.LastIndexByte(s, '}'); i >= 0 {
		s = s[:i+1]
	}
	var r Reply
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("model did not return valid JSON:\n%s", truncate(raw, 600))
	}
	return &r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
