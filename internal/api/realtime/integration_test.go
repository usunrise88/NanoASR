//go:build integration

// The whole realtime path, end to end: a websocket client, the dialect, the
// session engine, sherpa-onnx and real weights. Needs the models and the test
// audio:
//
//	./scripts/fetch-dev-models.sh
//	./scripts/fetch-testdata.sh
//	go test -tags integration ./internal/api/realtime/ -v
package realtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/audio"
	engine "github.com/usunrise88/nanoasr/internal/realtime"
	"github.com/usunrise88/nanoasr/internal/registry"
)

const (
	streamingModel = "t-one-ctc-ru"
	// Fragments the offline default also produces. Streaming costs accuracy,
	// so the assertion is on words either model gets right rather than on a
	// whole transcript.
	wantStart = "не требуя похвал"
	wantEnd   = "лукоморья"
)

// liveServer is the real stack behind a real socket.
func liveServer(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.NewLocal(filepath.Join(root, ".models"))
	if err != nil {
		t.Fatal(err)
	}
	man, err := reg.Resolve(t.Context(), streamingModel)
	if err != nil {
		t.Skipf("model %s is absent; run ./scripts/fetch-dev-models.sh (%v)", streamingModel, err)
	}
	dir, err := reg.Dir(streamingModel)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := sherpa.NewOnline(t.Context(), man, dir, sherpa.OnlineOptions{
		NumThreads:              2,
		EnableEndpoint:          true,
		Rule1MinTrailingSilence: 2.4,
		Rule2MinTrailingSilence: 1.2,
		Rule3MinUtteranceLength: 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	eng := engine.New(rec, engine.Options{
		MaxSessions:        2,
		MaxBufferedSeconds: 30,
		Languages:          man.Languages,
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); eng.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-stopped
		_ = rec.Close()
	})

	mux := http.NewServeMux()
	(&Adapter{}).Mount(mux, nil, adapter.Deps{Realtime: eng})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestAWebsocketClientGetsALiveTranscript streams the reference clip at the
// speed a microphone would and collects what comes back.
//
// Faster than real time would be a different test: it would prove the decoder
// works but not that the protocol does, because every partial would arrive in
// one burst at the end. The point here is that transcripts arrive while the
// audio is still going.
func TestAWebsocketClientGetsALiveTranscript(t *testing.T) {
	url := liveServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url+"/v1/realtime?intent=transcription", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()

	// The session description arrives before any audio.
	created := readEvent(t, ctx, ws)
	if created["type"] != serverSessionCreated {
		t.Fatalf("first event is %v", created["type"])
	}

	// 16 kHz PCM16, which is what a browser sends; the model is 8 kHz and the
	// resampling happens inside sherpa-onnx.
	if err := ws.Write(ctx, websocket.MessageText, update(t, 16000)); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ctx, ws); ev["type"] != serverSessionUpdated {
		t.Fatalf("the session was not updated: %v", ev)
	}

	pcm := referenceClip(t, 16000)

	type observation struct {
		at   time.Duration
		kind string
		text string
	}
	seen := make(chan observation, 256)
	go func() {
		start := time.Now()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				close(seen)
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var ev map[string]any
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			kind, _ := ev["type"].(string)
			switch kind {
			case serverDelta:
				extra, _ := ev["nanoasr"].(map[string]any)
				seen <- observation{time.Since(start), kind, str(extra["text"])}
			case serverCompleted:
				seen <- observation{time.Since(start), kind, str(ev["transcript"])}
			case serverError:
				body, _ := ev["error"].(map[string]any)
				t.Errorf("server error: %v", body["message"])
			}
		}
	}()

	// 100 ms of audio every 100 ms: a microphone, not a file.
	const chunkMS = 100
	chunk := 16000 * chunkMS / 1000
	audioStart := time.Now()
	for off := 0; off < len(pcm); off += chunk {
		end := min(off+chunk, len(pcm))
		if err := ws.Write(ctx, websocket.MessageBinary, pcm16LE(pcm[off:end])); err != nil {
			t.Fatal(err)
		}
		time.Sleep(chunkMS * time.Millisecond)
	}
	streamed := time.Since(audioStart)

	// Nothing more is coming, so ask for the tail.
	if err := ws.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
		t.Fatal(err)
	}

	var partials int
	var firstPartial time.Duration
	var finals []string
	deadline := time.After(30 * time.Second)
collect:
	for {
		select {
		case o, ok := <-seen:
			if !ok {
				break collect
			}
			switch o.kind {
			case serverDelta:
				if partials == 0 {
					firstPartial = o.at
				}
				partials++
			case serverCompleted:
				finals = append(finals, o.text)
				if len(finals) > 0 && o.text != "" {
					break collect
				}
			}
		case <-deadline:
			t.Fatal("no final transcript arrived")
		}
	}

	full := strings.Join(finals, " ")
	t.Logf("audio      %.2fs, streamed in %.2fs", float64(len(pcm))/16000, streamed.Seconds())
	t.Logf("partials   %d, first after %.0fms", partials, firstPartial.Seconds()*1000)
	t.Logf("final      %s", full)

	if partials == 0 {
		t.Error("no partial hypotheses arrived: the stream produced nothing until the end")
	}
	// The whole point of a streaming API: a hypothesis before the audio is
	// over. The clip is eleven seconds long.
	if firstPartial > 5*time.Second {
		t.Errorf("the first partial took %v, which is not a live transcript", firstPartial)
	}
	for _, want := range []string{wantStart, wantEnd} {
		if !strings.Contains(full, want) {
			t.Errorf("the transcript does not contain %q:\n%s", want, full)
		}
	}
	if strings.ContainsAny(full, ".,?!") {
		t.Errorf("this model cannot punctuate, yet the transcript has marks:\n%s", full)
	}
}

// TestTwoClientsAreTranscribedAtOnce is what the batched decoder exists for.
func TestTwoClientsAreTranscribedAtOnce(t *testing.T) {
	url := liveServer(t)
	pcm := referenceClip(t, 16000)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	transcripts := make(chan string, 2)
	for i := range 2 {
		go func(offset int) {
			ws, _, err := websocket.Dial(ctx, url+"/v1/realtime", nil)
			if err != nil {
				transcripts <- "dial: " + err.Error()
				return
			}
			defer ws.CloseNow()

			if err := ws.Write(ctx, websocket.MessageText, update(t, 16000)); err != nil {
				transcripts <- "update: " + err.Error()
				return
			}
			// Different starting points, so an answer that mixed the two
			// sessions up would be visible.
			samples := pcm[offset*16000:]
			const chunk = 1600
			go func() {
				for off := 0; off < len(samples); off += chunk {
					end := min(off+chunk, len(samples))
					if ws.Write(ctx, websocket.MessageBinary, pcm16LE(samples[off:end])) != nil {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
				_ = ws.Write(ctx, websocket.MessageText,
					[]byte(`{"type":"input_audio_buffer.commit"}`))
			}()

			for {
				typ, data, err := ws.Read(ctx)
				if err != nil {
					transcripts <- "read: " + err.Error()
					return
				}
				if typ != websocket.MessageText {
					continue
				}
				var ev map[string]any
				if json.Unmarshal(data, &ev) != nil {
					continue
				}
				if ev["type"] == serverCompleted {
					if text := str(ev["transcript"]); text != "" {
						transcripts <- text
						return
					}
				}
			}
		}(i * 2)
	}

	first := <-transcripts
	second := <-transcripts
	t.Logf("client A: %.70s", first)
	t.Logf("client B: %.70s", second)

	for _, got := range []string{first, second} {
		if strings.HasPrefix(got, "dial:") || strings.HasPrefix(got, "read:") ||
			strings.HasPrefix(got, "update:") {
			t.Fatalf("a client failed: %s", got)
		}
	}
	if first == second {
		t.Error("both clients got the same transcript although they sent different audio")
	}
}

// --- fixtures ---------------------------------------------------------------

func update(t *testing.T, rate int) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "transcription_session.update",
		"session": map[string]any{
			"input_audio_format":        map[string]any{"type": "audio/pcm", "rate": rate},
			"input_audio_transcription": map[string]any{"language": "ru"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readEvent(t *testing.T, ctx context.Context, ws *websocket.Conn) map[string]any {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, data, err := ws.Read(readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("invalid JSON %q", data)
	}
	return out
}

func referenceClip(t *testing.T, rate int) []float32 {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "testdata", "audio", "ru-16k.wav")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("test audio is absent; run ./scripts/fetch-testdata.sh (%v)", err)
	}
	defer f.Close()

	pcms, err := audio.NewWAVDecoder().Decode(t.Context(), f, audio.Options{TargetSampleRate: rate})
	if err != nil {
		t.Fatal(err)
	}
	return pcms[0].Samples
}

// pcm16LE encodes samples the way a client would before sending them.
func pcm16LE(samples []float32) []byte {
	out := make([]byte, 0, len(samples)*2)
	for _, s := range samples {
		v := s * 32767
		switch {
		case v > 32767:
			v = 32767
		case v < -32768:
			v = -32768
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(v)))
	}
	return out
}
