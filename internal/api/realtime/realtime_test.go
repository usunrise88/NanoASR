package realtime

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

// serve mounts the dialect on a real HTTP server. The tests then speak the
// protocol over a real websocket, because the handshake, the frame types and
// the close codes are part of what is being tested — a handler called directly
// would exercise none of them.
func serve(t *testing.T, svc core.RealtimeService) string {
	return serveWith(t, svc, &Adapter{})
}

func serveWith(t *testing.T, svc core.RealtimeService, a *Adapter) string {
	t.Helper()
	mux := http.NewServeMux()
	a.Mount(mux, nil, adapter.Deps{Realtime: svc})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	ws, _, err := websocket.Dial(ctx, url+"/v1/realtime?intent=transcription", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws, ctx
}

// next reads one server event as a map, so a test can assert on the wire shape
// rather than on a struct this package also writes.
func next(t *testing.T, ctx context.Context, ws *websocket.Conn) map[string]any {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	typ, data, err := ws.Read(readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("server sent a %v frame", typ)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("server sent invalid JSON %q: %v", data, err)
	}
	if out["event_id"] == "" {
		t.Errorf("event has no event_id: %s", data)
	}
	return out
}

// expect reads events until one of the wanted type arrives.
func expect(t *testing.T, ctx context.Context, ws *websocket.Conn, want string) map[string]any {
	t.Helper()
	var seen []string
	for range 20 {
		ev := next(t, ctx, ws)
		got, _ := ev["type"].(string)
		if got == want {
			return ev
		}
		seen = append(seen, got)
	}
	t.Fatalf("never saw %s; got %v", want, seen)
	return nil
}

func sendJSON(t *testing.T, ctx context.Context, ws *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// pcm16 encodes samples the way a client does.
func pcm16Bytes(values ...int16) []byte {
	out := make([]byte, 0, len(values)*2)
	for _, v := range values {
		out = binary.LittleEndian.AppendUint16(out, uint16(v))
	}
	return out
}

func TestTheSessionIsDescribedBeforeAnyAudio(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))

	ev := expect(t, ctx, ws, serverSessionCreated)
	session, ok := ev["session"].(map[string]any)
	if !ok {
		t.Fatalf("created event has no session object: %v", ev)
	}
	if session["model"] != "t-one-ctc-ru@2025-09-08" {
		t.Errorf("model is %v", session["model"])
	}
	if session["input_audio_format"] != "pcm16" {
		t.Errorf("default format is %v, want pcm16", session["input_audio_format"])
	}

	extra, ok := session["nanoasr"].(map[string]any)
	if !ok {
		t.Fatalf("no nanoasr object in the session: %v", session)
	}
	if extra["model_sample_rate"] != float64(8000) || extra["input_sample_rate"] != float64(24000) {
		t.Errorf("rates are wrong: %v", extra)
	}
	if extra["punctuation"] != false {
		t.Errorf("punctuation is %v, want false", extra["punctuation"])
	}
	// The model punctuates nothing, so the session has to say so: it is the
	// first difference a caller coming from the offline API will hit.
	notes, _ := extra["notes"].([]any)
	if !containsSubstring(notes, "no punctuation") {
		t.Errorf("the session does not mention the missing punctuation: %v", notes)
	}

	// Describing a session must not consume a decoder slot.
	if svc.count() != 0 {
		t.Errorf("%d sessions were opened before any audio arrived", svc.count())
	}
}

func TestAudioArrivesAtTheSessionInTheDeclaredFormat(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{
		"type": "transcription_session.update",
		"session": map[string]any{
			"input_audio_format":        map[string]any{"type": "audio/pcm", "rate": 16000},
			"input_audio_transcription": map[string]any{"language": "ru"},
		},
	})
	updated := expect(t, ctx, ws, serverSessionUpdated)
	session := updated["session"].(map[string]any)
	if session["input_audio_format"] != "pcm16;rate=16000" {
		t.Errorf("format is %v, want pcm16;rate=16000", session["input_audio_format"])
	}

	sendJSON(t, ctx, ws, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm16Bytes(0, 16384, -16384)),
	})

	sess := <-svc.opened
	waitFor(t, func() bool { return len(sess.samples()) == 3 })

	audio, _, _, _ := sess.snapshot()
	if audio[0].rate != 16000 {
		t.Errorf("audio arrived at %d Hz, want 16000", audio[0].rate)
	}
	got := sess.samples()
	want := []float32{0, 0.5, -0.5}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d is %v, want %v", i, got[i], want[i])
		}
	}
	if sess.params.Language != "ru" {
		t.Errorf("the session was opened with language %q", sess.params.Language)
	}
}

func TestABinaryFrameIsAudio(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(32767, -32768)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened
	waitFor(t, func() bool { return len(sess.samples()) == 2 })
}

// The case a frame-oriented protocol makes easy to get wrong: a frame that
// ends in the middle of a sample.
func TestAnOddByteIsCarriedToTheNextFrame(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	whole := pcm16Bytes(1000, 2000, 3000)
	if err := ws.Write(ctx, websocket.MessageBinary, whole[:3]); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened
	waitFor(t, func() bool { return len(sess.samples()) == 1 })

	if err := ws.Write(ctx, websocket.MessageBinary, whole[3:]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sess.samples()) == 3 })

	want, _ := pcm16Decode(whole)
	got := sess.samples()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d is %v, want %v: the frame boundary shifted the stream",
				i, got[i], want[i])
		}
	}
}

func TestG711IsAccepted(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"input_audio_format": "g711_ulaw"},
	})
	updated := expect(t, ctx, ws, serverSessionUpdated)
	session := updated["session"].(map[string]any)
	if session["input_audio_format"] != "g711_ulaw" {
		t.Fatalf("format is %v", session["input_audio_format"])
	}
	if rate := session["nanoasr"].(map[string]any)["input_sample_rate"]; rate != float64(8000) {
		t.Errorf("mu-law was accepted at %v Hz, want 8000", rate)
	}

	// One byte per sample, so sixteen bytes are sixteen samples.
	if err := ws.Write(ctx, websocket.MessageBinary, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened
	waitFor(t, func() bool { return len(sess.samples()) == 16 })
}

func TestAnUtteranceIsReportedAsTheProtocolRequires(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1, 2, 3)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened

	sess.emit(core.RealtimeEvent{Kind: core.RealtimeSpeechStarted, Item: "item_1", StartMS: 120})
	sess.emit(core.RealtimeEvent{
		Kind: core.RealtimePartial, Item: "item_1", Text: "раз", Delta: "раз", StartMS: 120,
	})
	sess.emit(core.RealtimeEvent{
		Kind: core.RealtimePartial, Item: "item_1", Text: "раз два", Delta: " два", StartMS: 120,
	})
	sess.emit(core.RealtimeEvent{
		Kind: core.RealtimeSpeechStopped, Item: "item_1", StartMS: 120, EndMS: 1500,
	})
	sess.emit(core.RealtimeEvent{
		Kind: core.RealtimeFinal, Item: "item_1", Text: "раз два",
		StartMS: 120, EndMS: 1500,
		Words: []core.Word{{Word: "раз", Start: 0.12, End: 0.4}},
	})

	started := expect(t, ctx, ws, serverSpeechStarted)
	if started["item_id"] != "item_1" || started["audio_start_ms"] != float64(120) {
		t.Errorf("speech_started is wrong: %v", started)
	}

	first := expect(t, ctx, ws, serverDelta)
	if first["delta"] != "раз" {
		t.Errorf("first delta is %v", first["delta"])
	}
	second := expect(t, ctx, ws, serverDelta)
	if second["delta"] != " два" {
		t.Errorf("second delta is %v", second["delta"])
	}
	// The full hypothesis travels beside the increment, because a streaming
	// decoder can retract and no delta expresses that.
	if extra := second["nanoasr"].(map[string]any); extra["text"] != "раз два" {
		t.Errorf("the delta does not carry the full hypothesis: %v", extra)
	}

	stopped := expect(t, ctx, ws, serverSpeechStopped)
	if stopped["audio_end_ms"] != float64(1500) {
		t.Errorf("speech_stopped is wrong: %v", stopped)
	}

	committed := expect(t, ctx, ws, serverCommitted)
	if committed["item_id"] != "item_1" {
		t.Errorf("committed is wrong: %v", committed)
	}
	created := expect(t, ctx, ws, serverItemCreated)
	if item, ok := created["item"].(map[string]any); !ok || item["id"] != "item_1" {
		t.Errorf("item.created is wrong: %v", created)
	}

	done := expect(t, ctx, ws, serverCompleted)
	if done["transcript"] != "раз два" {
		t.Errorf("transcript is %v", done["transcript"])
	}
	extra, _ := done["nanoasr"].(map[string]any)
	if extra["end_ms"] != float64(1500) || extra["model"] != "t-one-ctc-ru@2025-09-08" {
		t.Errorf("completed extras are wrong: %v", extra)
	}
	if words, _ := extra["words"].([]any); len(words) != 1 {
		t.Errorf("word timings did not survive: %v", extra["words"])
	}
}

func TestASecondUtteranceReferencesTheFirst(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)
	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened

	sess.emit(core.RealtimeEvent{Kind: core.RealtimeFinal, Item: "item_1", Text: "раз"})
	sess.emit(core.RealtimeEvent{Kind: core.RealtimeFinal, Item: "item_2", Text: "два"})

	expect(t, ctx, ws, serverCompleted)
	committed := expect(t, ctx, ws, serverCommitted)
	if committed["previous_item_id"] != "item_1" {
		t.Errorf("the second utterance does not reference the first: %v", committed)
	}
}

func TestCommitAndClearReachTheSession(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{"type": "input_audio_buffer.commit"})
	sess := <-svc.opened
	waitFor(t, func() bool { _, commits, _, _ := sess.snapshot(); return commits == 1 })

	sendJSON(t, ctx, ws, map[string]any{"type": "input_audio_buffer.clear"})
	waitFor(t, func() bool { _, _, clears, _ := sess.snapshot(); return clears == 1 })
}

func TestClearBeforeAnythingWasSentIsAcknowledged(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{"type": "input_audio_buffer.clear"})
	expect(t, ctx, ws, serverCleared)
	if svc.count() != 0 {
		t.Error("clearing an empty buffer opened a decoder session")
	}
}

func TestTurnDetectionCanBeSwitchedOffBeforeAudio(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"turn_detection": nil},
	})
	updated := expect(t, ctx, ws, serverSessionUpdated)
	if session := updated["session"].(map[string]any); session["turn_detection"] != nil {
		t.Errorf("turn_detection is still %v", session["turn_detection"])
	}

	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened
	if sess.params.AutoTurns {
		t.Error("the session was opened with automatic turns although they were switched off")
	}
}

func TestIgnoredParametersAreReported(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"input_audio_transcription": map[string]any{"prompt": "Иванов, Петров"},
			"turn_detection":            map[string]any{"type": "server_vad", "silence_duration_ms": 200},
			"modalities":                []string{"text"},
			"temperature":               0.4,
		},
	})

	codes := map[string]bool{}
	for range 10 {
		ev := next(t, ctx, ws)
		switch ev["type"] {
		case serverWarning:
			codes[ev["code"].(string)+":"+str(ev["param"])] = true
		case serverSessionUpdated:
			// Warnings come before the acknowledgement.
			if !codes["prompt_ignored:input_audio_transcription.prompt"] {
				t.Errorf("prompt was dropped without a word: %v", codes)
			}
			if !codes["turn_detection_fixed:turn_detection.silence_duration_ms"] {
				t.Errorf("the fixed endpoint timing was not reported: %v", codes)
			}
			if !codes["field_ignored:modalities"] || !codes["field_ignored:temperature"] {
				t.Errorf("conversation parameters were dropped silently: %v", codes)
			}
			return
		}
	}
	t.Fatalf("no session.updated arrived; warnings were %v", codes)
}

func TestAskingForAnotherModelIsAnError(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"input_audio_transcription": map[string]any{"model": "gpt-4o-transcribe"},
		},
	})
	ev := expect(t, ctx, ws, serverError)
	body := ev["error"].(map[string]any)
	if body["code"] != "model_not_found" {
		t.Errorf("error code is %v", body["code"])
	}
	if !strings.Contains(str(body["message"]), "t-one-ctc-ru") {
		t.Errorf("the error does not name the loaded model: %v", body["message"])
	}

	// The connection survives: the client may correct itself.
	sendJSON(t, ctx, ws, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"input_audio_transcription": map[string]any{"model": "t-one-ctc-ru"}},
	})
	expect(t, ctx, ws, serverSessionUpdated)
}

func TestAConversationEventIsRefusedWithoutClosing(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	sendJSON(t, ctx, ws, map[string]any{"type": "response.create"})
	ev := expect(t, ctx, ws, serverError)
	if body := ev["error"].(map[string]any); body["code"] != "unsupported_event" {
		t.Errorf("error code is %v", body["code"])
	}

	sendJSON(t, ctx, ws, map[string]any{"type": "input_audio_buffer.clear"})
	expect(t, ctx, ws, serverCleared)
}

func TestInvalidJSONIsRefusedWithoutClosing(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	if err := ws.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	ev := expect(t, ctx, ws, serverError)
	if body := ev["error"].(map[string]any); body["code"] != "invalid_json" {
		t.Errorf("error code is %v", body["code"])
	}
}

func TestAFullServerClosesWithTryAgainLater(t *testing.T) {
	svc := newFakeService()
	svc.openErr = core.Errorf(core.CodeQueueFull, "all 4 realtime sessions are in use")
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1)); err != nil {
		t.Fatal(err)
	}
	ev := expect(t, ctx, ws, serverError)
	body := ev["error"].(map[string]any)
	if body["type"] != "rate_limit_error" {
		t.Errorf("error type is %v, want rate_limit_error", body["type"])
	}

	if status := closeStatus(t, ctx, ws); status != websocket.StatusTryAgainLater {
		t.Errorf("close status is %v, want %v", status, websocket.StatusTryAgainLater)
	}
}

func TestASessionThatEndsClosesTheSocket(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)
	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened

	sess.emit(core.RealtimeEvent{
		Kind: core.RealtimeError,
		Err:  core.Errorf(core.CodeProcessingTimeout, "no audio for 1m0s: closing the session"),
	})
	sess.end()

	ev := expect(t, ctx, ws, serverError)
	if body := ev["error"].(map[string]any); !strings.Contains(str(body["message"]), "no audio") {
		t.Errorf("the error does not explain itself: %v", body)
	}
	// A limit that was always going to be reached is not a failure.
	if status := closeStatus(t, ctx, ws); status != websocket.StatusNormalClosure {
		t.Errorf("close status is %v, want %v", status, websocket.StatusNormalClosure)
	}
}

func TestABackedUpSessionClosesTheSocket(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)

	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened
	sess.mu.Lock()
	sess.appendErr = core.Errorf(core.CodeQueueFull, "the server cannot keep up with this stream")
	sess.mu.Unlock()

	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1, 2, 3)); err != nil {
		t.Fatal(err)
	}
	ev := expect(t, ctx, ws, serverError)
	if !strings.Contains(str(ev["error"].(map[string]any)["message"]), "keep up") {
		t.Errorf("the error does not explain itself: %v", ev)
	}
	if status := closeStatus(t, ctx, ws); status != websocket.StatusTryAgainLater {
		t.Errorf("close status is %v", status)
	}
}

func TestClosingTheSocketReleasesTheSession(t *testing.T) {
	svc := newFakeService()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)
	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened

	if err := ws.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, _, _, closes := sess.snapshot(); return closes > 0 })
}

func TestWithoutAStreamingModelTheUpgradeIsRefusedAsHTTP(t *testing.T) {
	mux := http.NewServeMux()
	(&Adapter{}).Mount(mux, nil, adapter.Deps{})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime", nil)
	if err == nil {
		t.Fatal("the upgrade succeeded on a server with no streaming model")
	}
	if resp == nil || resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status is %v, want 501", resp)
	}
	var body eventError
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "realtime_unavailable" {
		t.Errorf("error code is %q", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "api.dialects") {
		t.Errorf("the error does not say how to fix it: %q", body.Error.Message)
	}
}

func TestAConversationIntentIsRefused(t *testing.T) {
	url := serve(t, newFakeService())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, url+"/v1/realtime?intent=conversation", nil)
	if err == nil {
		t.Fatal("intent=conversation was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status is %v, want 400", resp)
	}
}

func TestEphemeralTokensAreRefusedWithAnExplanation(t *testing.T) {
	mux := http.NewServeMux()
	(&Adapter{}).Mount(mux, nil, adapter.Deps{Realtime: newFakeService()})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/realtime/transcription_sessions", "application/json",
		strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status is %d, want 501", resp.StatusCode)
	}
	var body eventError
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error.Message, "openai-insecure-api-key") {
		t.Errorf("the refusal does not say how to connect instead: %q", body.Error.Message)
	}
}

func TestBadFormatsAreRefused(t *testing.T) {
	for _, bad := range []any{
		"mp3",
		map[string]any{"type": "audio/pcm", "rate": 192000},
		map[string]any{"type": "opus"},
	} {
		svc := newFakeService()
		ws, ctx := dial(t, serve(t, svc))
		expect(t, ctx, ws, serverSessionCreated)

		sendJSON(t, ctx, ws, map[string]any{
			"type":    "session.update",
			"session": map[string]any{"input_audio_format": bad},
		})
		ev := expect(t, ctx, ws, serverError)
		body := ev["error"].(map[string]any)
		if body["param"] != "input_audio_format" {
			t.Errorf("format %v: error does not name the parameter: %v", bad, body)
		}
		_ = ws.CloseNow()
	}
}

// --- helpers ---------------------------------------------------------------

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the server to catch up")
}

func closeStatus(t *testing.T, ctx context.Context, ws *websocket.Conn) websocket.StatusCode {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		_, _, err := ws.Read(readCtx)
		if err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func containsSubstring(values []any, want string) bool {
	for _, v := range values {
		if strings.Contains(str(v), want) {
			return true
		}
	}
	return false
}

// pcm16Decode mirrors the client side of the format, for assertions.
func pcm16Decode(b []byte) ([]float32, []byte) {
	out := make([]float32, len(b)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(b[i*2:]))) / 32768
	}
	if len(b)%2 == 1 {
		return out, b[len(b)-1:]
	}
	return out, nil
}

// OpenAI's clients name the model in the query string. One this server does not
// have is refused before the upgrade, where the client sees a readable body.
func TestAModelInTheQueryStringIsChecked(t *testing.T) {
	url := serve(t, newFakeService())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, url+"/v1/realtime?model=gpt-4o-transcribe", nil)
	if err == nil {
		t.Fatal("a session asking for another model was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status is %v, want 400", resp)
	}
	var body eventError
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "model_not_found" || !strings.Contains(body.Error.Message, "t-one-ctc-ru") {
		t.Errorf("the refusal does not name the loaded model: %+v", body.Error)
	}

	// The configured one, with or without its revision, connects.
	for _, model := range []string{"t-one-ctc-ru", "t-one-ctc-ru@2025-09-08"} {
		ws, _, err := websocket.Dial(ctx, url+"/v1/realtime?model="+model, nil)
		if err != nil {
			t.Fatalf("model %q was refused: %v", model, err)
		}
		_ = ws.CloseNow()
	}
}

// A client sending faster than the recogniser can keep up is throttled by the
// read loop blocking in Append. The keepalive must not mistake that for a dead
// peer: a ping is answered by a pong that only the read loop can collect, so
// one sent while it is blocked would time out and close a live connection.
func TestBackpressureDoesNotTripTheKeepalive(t *testing.T) {
	svc := newFakeService()

	// Shortened so that what this is about happens in milliseconds: a ping
	// every 10 ms, each giving up after 50, against appends that take 120.
	ws, ctx := dial(t, serveWith(t, svc, &Adapter{
		pingInterval: 10 * time.Millisecond,
		writeTimeout: 50 * time.Millisecond,
	}))
	expect(t, ctx, ws, serverSessionCreated)

	if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1)); err != nil {
		t.Fatal(err)
	}
	sess := <-svc.opened

	// Every append from here on blocks for longer than the ping deadline.
	sess.mu.Lock()
	sess.appendDelay = 120 * time.Millisecond
	sess.mu.Unlock()

	for range 3 {
		if err := ws.Write(ctx, websocket.MessageBinary, pcm16Bytes(1, 2, 3)); err != nil {
			t.Fatalf("the connection was closed while the server was catching up: %v", err)
		}
	}

	// All of it has to arrive: three appends at 120 ms is a third of a second
	// during which the keepalive fires thirty times, and a connection the
	// server gave up on would stop reading here.
	waitFor(t, func() bool { return len(sess.samples()) == 1+3*3 })

	// And the connection is still usable afterwards: the dialect answers this
	// one itself, so it proves the socket rather than the fake.
	sess.mu.Lock()
	sess.appendDelay = 0
	sess.mu.Unlock()
	sendJSON(t, ctx, ws, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"input_audio_format": "g711_alaw"},
	})
	updated := expect(t, ctx, ws, serverSessionUpdated)
	if session := updated["session"].(map[string]any); session["input_audio_format"] != "g711_alaw" {
		t.Errorf("the session did not take the update: %v", session)
	}
}
