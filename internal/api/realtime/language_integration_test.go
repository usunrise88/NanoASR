//go:build integration

package realtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/usunrise88/nanoasr/internal/audio"
)

const multilingualModel = "nemotron-3.5-streaming-multi"

// transcribeOver streams a clip through a live session that asked for a
// language and returns the final transcript, along with the language the
// server says the session is in.
func transcribeOver(t *testing.T, url, language string, pcm []float32) (transcript, sessionLanguage string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url+"/v1/realtime?intent=transcription", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if ev := readEvent(t, ctx, ws); ev["type"] != serverSessionCreated {
		t.Fatalf("first event is %v", ev["type"])
	}

	b, _ := json.Marshal(map[string]any{
		"type": "transcription_session.update",
		"session": map[string]any{
			"input_audio_format":        map[string]any{"type": "audio/pcm", "rate": 16000},
			"input_audio_transcription": map[string]any{"language": language},
		},
	})
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	var updated map[string]any
	for {
		ev := readEvent(t, ctx, ws)
		if ev["type"] == serverSessionUpdated {
			updated = ev
			break
		}
	}
	session, _ := updated["session"].(map[string]any)
	tr, _ := session["input_audio_transcription"].(map[string]any)
	sessionLanguage = str(tr["language"])

	// Half a second of silence first, as a microphone that opened before anyone
	// spoke would send, then the clip as fast as the server will take it: the
	// protocol throttles a client that is ahead of the decoder.
	pcm = append(make([]float32, 8000), pcm...)
	for off := 0; off < len(pcm); off += 1600 {
		if err := ws.Write(ctx, websocket.MessageBinary, pcm16LE(pcm[off:min(off+1600, len(pcm))])); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
		t.Fatal(err)
	}

	var parts []string
	for {
		ev := readEvent(t, ctx, ws)
		switch ev["type"] {
		case serverCompleted:
			parts = append(parts, str(ev["transcript"]))
			if strings.TrimSpace(strings.Join(parts, "")) != "" {
				return strings.Join(parts, " "), sessionLanguage
			}
		case serverError:
			t.Fatalf("server error: %v", ev["error"])
		}
	}
}

func clip(t *testing.T, path string) []float32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("clip %s is absent (%v)", path, err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(t.Context(), f, audio.Options{TargetSampleRate: 16000})
	if err != nil {
		t.Fatal(err)
	}
	return pcms[0].Samples
}

// TestAMultilingualSessionTranscribesInTheLanguageItAskedFor is the language
// key over the real socket and the real weights: a bare code is resolved, the
// session says what it resolved to, and the transcript is in that language.
func TestAMultilingualSessionTranscribesInTheLanguageItAskedFor(t *testing.T) {
	url, dir := liveServerFor(t, multilingualModel)
	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))

	for _, tc := range []struct {
		clip, asked, resolved, contains string
	}{
		{filepath.Join(root, "testdata", "audio", "ru-16k.wav"), "ru", "ru-RU", "не требуя похвал"},
		{filepath.Join(dir, "test_wavs", "de.wav"), "de", "de-DE", "wurst"},
		{filepath.Join(dir, "test_wavs", "fr.wav"), "fr", "fr-FR", "demandez"},
	} {
		got, resolved := transcribeOver(t, url, tc.asked, clip(t, tc.clip))
		t.Logf("%s as %q (%s): %s", filepath.Base(tc.clip), tc.asked, resolved, got)
		if resolved != tc.resolved {
			t.Errorf("asked for %q, the session says %q, want %q", tc.asked, resolved, tc.resolved)
		}
		if !strings.Contains(strings.ToLower(got), tc.contains) {
			t.Errorf("transcript lacks %q: %s", tc.contains, got)
		}
	}
}
