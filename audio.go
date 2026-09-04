package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// listen records from the default input until you stop talking, then hands
// the clip to whisper.cpp. sox's `rec` does the voice-activity detection:
// start when sound exceeds the threshold, stop after cfg.Silence seconds
// below it.
func listen(cfg Config) (string, error) {
	dir, err := os.MkdirTemp("", "diane-in-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	wav := filepath.Join(dir, "in.wav")

	rec := exec.Command(cfg.RecBin, "-q", "-r", "16000", "-c", "1", "-b", "16", wav,
		"silence", "1", "0.1", "2%", "1", cfg.Silence, "2%")
	rec.Stderr = os.Stderr
	if err := rec.Run(); err != nil {
		return "", fmt.Errorf("record (%s): %w", cfg.RecBin, err)
	}

	out, err := exec.Command(cfg.WhisperBin,
		"-m", cfg.WhisperModel, "-f", wav, "-l", "en", "-t", cfg.Threads,
		"-nt", "-np", // no timestamps, no progress chatter
	).Output()
	if err != nil {
		return "", fmt.Errorf("transcribe (%s): %w", cfg.WhisperBin, err)
	}
	text := strings.TrimSpace(string(out))
	if !usable(text) {
		return "", nil
	}
	return text, nil
}

// usable rejects whisper's placeholders for silence and its favourite
// hallucinations on empty audio.
func usable(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch strings.ToLower(s) {
	case "[blank_audio]", "(silence)", "[silence]", "[ silence ]", "you", "thank you.", "thanks for watching!":
		return false
	}
	return true
}

// speak synthesises sentence by sentence and plays each clip as soon as it is
// ready, so audio starts before the whole reply has been rendered.
//
// The synthesiser is any program invoked as `tts <out.wav> <text>` with the
// voice in DIANE_VOICE; bin/ has wrappers.
func speak(cfg Config, text string) error {
	dir, err := os.MkdirTemp("", "diane-out-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	parts := sentences(text)
	clips := make(chan string, len(parts))
	synthErr := make(chan error, 1)
	go func() {
		defer close(clips)
		for i, s := range parts {
			wav := filepath.Join(dir, fmt.Sprintf("%03d.wav", i))
			cmd := exec.Command(cfg.TTSBin, wav, s)
			cmd.Env = append(os.Environ(), "DIANE_VOICE="+cfg.Voice)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				synthErr <- fmt.Errorf("synthesise (%s): %w", cfg.TTSBin, err)
				return
			}
			clips <- wav
		}
	}()
	for wav := range clips {
		cmd := exec.Command(cfg.PlayBin, "-q", wav)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("play (%s): %w", cfg.PlayBin, err)
		}
	}
	select {
	case err := <-synthErr:
		return err
	default:
		return nil
	}
}

// sentences splits on terminal punctuation followed by a space. Crude on
// abbreviations; the cost of a wrong split is one extra short clip.
func sentences(text string) []string {
	text = strings.Join(strings.Fields(text), " ")
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		if !strings.ContainsRune(".!?;", rune(text[i])) || (i+1 < len(text) && text[i+1] != ' ') {
			continue
		}
		if s := strings.TrimSpace(text[start : i+1]); s != "" {
			out = append(out, s)
		}
		start = i + 1
	}
	if s := strings.TrimSpace(text[start:]); s != "" {
		out = append(out, s)
	}
	return out
}
