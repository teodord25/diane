package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
  "writes":  [{"path": "todo.md", "content": "the complete new contents of that file"}],
  "deletes": ["obsolete.md"]
}

Rules:

- "speak" is required. Always say something, even if only "Done."
- "speak" must stand alone. Never end it with a colon or a promise of content
  that only exists in a file — if you were asked to produce something, say it
  in "speak" as well as writing it.
- "writes" replaces a file's entire contents. Include every line you intend to
  keep, not just the changed ones. Omit files you are not changing. Use an empty
  list when you are changing nothing.
- Paths are relative to the vault root, must stay inside it, and must end in
  .md, .txt or .org.
- "speak" is fed to a speech synthesiser. Plain spoken prose only: no markdown,
  no asterisks, no hyphens as bullets, no backticks, no URLs, no file paths.
  Write list positions as words: "one", "two", "three".
- Be brief. One or two sentences, unless you are reading a list back.
- If the person only asked a question, answer it and write nothing.
- Preserve the vault's existing formatting and file layout. Do not tidy,
  reorganise or rename anything unless you were asked to.
- Prefer adding to an existing file over creating a new one.`

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Write struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Reply struct {
	Speak   string   `json:"speak"`
	Writes  []Write  `json:"writes"`
	Deletes []string `json:"deletes"`
}

var modelClient = &http.Client{Timeout: 180 * time.Second}

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
	if n, err := v.apply(files, reply, utterance); err != nil {
		return "", fmt.Errorf("apply: %w", err)
	} else if n > 0 {
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

// prompt renders the whole vault plus the utterance, identically for both backends.
func prompt(files []File, utterance string) string {
	var b strings.Builder
	b.WriteString("<vault>\n")
	for _, f := range files {
		fmt.Fprintf(&b, "<file path=%q>\n%s\n</file>\n", f.Path, f.Content)
	}
	if len(files) == 0 {
		b.WriteString("(the vault is empty)\n")
	}
	fmt.Fprintf(&b, "</vault>\n\n<utterance>\n%s\n</utterance>", utterance)
	return b.String()
}

// post sends JSON and returns the response body, turning HTTP-level failures
// into errors that name the server that failed.
func post(url string, headers map[string]string, payload any) ([]byte, error) {
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
	resp, err := modelClient.Do(req)
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
	out, err := post(cfg.LLMURL, nil, map[string]any{
		"model":       cfg.LLMModel,
		"messages":    msgs,
		"temperature": 0,
		"max_tokens":  2048,
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
	out, err := post("https://api.anthropic.com/v1/messages", map[string]string{
		"x-api-key":         cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}, map[string]any{
		"model":      cfg.ClaudeModel,
		"max_tokens": 2048,
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
