// Package realtime implements OpenAI's realtime transcription API over a
// websocket: GET /v1/realtime.
//
// What a session is here, and what it is not. OpenAI's realtime API has two
// intents, conversation and transcription, and this is the transcription half
// only: audio goes up, transcripts come back, and there is no model generating
// replies. The conversation events (response.create and the rest) are answered
// with an error that says so rather than ignored.
//
// Deliberate divergences, each reported to the client rather than left to be
// discovered:
//
//   - The model is the server's, not the session's. A streaming recogniser
//     settles its weights, its decoding method and its endpoint timings when it
//     is loaded, so a session cannot choose them; asking for another model is
//     an error and asking for other turn-detection timings is a warning.
//   - prompt is ignored. On the offline API it maps to hotword biasing, which
//     on the streaming path would need a second resident copy of the model per
//     bias list.
//   - Transcripts have no punctuation or capitals when the loaded model's
//     vocabulary has none, which is the case for every Russian streaming model
//     there is. The session object says so.
//   - input_audio_format also accepts an object with a rate, because these
//     models are 8 and 16 kHz and forcing every caller to resample to 24 kHz
//     would lose quality for nothing.
//   - A binary websocket frame is accepted as raw audio in the declared
//     format, which saves a third of the bandwidth that base64 costs.
//   - Every delta event carries the full hypothesis in a nanoasr object
//     beside the increment, because a streaming decoder revises as well as
//     extends and no sequence of deltas can express a retraction.
package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/httpx"
)

func init() { adapter.Register(&Adapter{}) }

const (
	// maxMessageBytes caps one client frame. Audio arrives base64-encoded
	// inside JSON, so two mebibytes is about twenty seconds at 24 kHz — far
	// more than any sane client sends at once, and bounded so that a frame
	// cannot be used to make the server allocate.
	maxMessageBytes = 2 << 20
	// closeReasonMax is the websocket limit on a close reason, in bytes.
	closeReasonMax = 123
)

// The timings, which nothing configures.
const (
	// writeTimeout bounds how long a stalled client can hold a write. The
	// event channel is already bounded, so this is the second half of the same
	// protection: a reader that has stopped reading is disconnected.
	writeTimeout = 10 * time.Second
	// pingInterval detects a peer that has gone away without closing. A
	// realtime socket can be silent for minutes — a push-to-talk client sends
	// nothing between presses — so TCP alone is not enough to tell live from
	// dead.
	pingInterval = 20 * time.Second
	// sessionGrace is how long a connection may hold a socket without sending
	// any audio. It is not the idle timeout of a session, which is longer and
	// configured: this one only catches a client that connected and never
	// started, and it costs nothing to close.
	sessionGrace = 60 * time.Second
)

// Adapter is the dialect. The three durations are zero everywhere but in the
// tests that shorten them: they are fields rather than package variables so
// that one test cannot change the behaviour of a connection another test still
// has open.
type Adapter struct {
	pingInterval time.Duration
	writeTimeout time.Duration
	sessionGrace time.Duration
}

func (*Adapter) Name() string { return "realtime" }

func (a *Adapter) timings() (ping, write, grace time.Duration) {
	return orDefault(a.pingInterval, pingInterval),
		orDefault(a.writeTimeout, writeTimeout),
		orDefault(a.sessionGrace, sessionGrace)
}

func orDefault(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

func (a *Adapter) Mount(mux *http.ServeMux, _ core.Service, deps adapter.Deps) {
	mux.HandleFunc("GET /v1/realtime", a.connect(deps))
	// OpenAI clients that run in a browser first POST here for an ephemeral
	// token. There is nothing to mint: this server authenticates with its own
	// API keys, and the answer says how to connect with one.
	mux.HandleFunc("POST /v1/realtime/transcription_sessions", a.ephemeral())
	mux.HandleFunc("POST /v1/realtime/sessions", a.ephemeral())
}

func (*Adapter) ephemeral() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotImplemented, eventError{
			Type:    serverError,
			EventID: newEventID(),
			Error: errorBody{
				Type: "invalid_request_error",
				Code: "ephemeral_tokens_unsupported",
				Message: "this server issues no ephemeral tokens: connect to /v1/realtime with " +
					"Authorization: Bearer <api key>, or from a browser with the subprotocol " +
					"openai-insecure-api-key.<api key>",
			},
		})
	}
}

func (a *Adapter) connect(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Realtime == nil {
			// Answered as HTTP, before any upgrade: a client that gets a
			// websocket and then an error event has a harder time telling this
			// from a transient failure.
			writeJSON(w, http.StatusNotImplemented, eventError{
				Type: serverError, EventID: newEventID(),
				Error: errorBody{
					Type: "invalid_request_error",
					Code: "realtime_unavailable",
					Message: "realtime recognition is not configured on this server: " +
						"add \"realtime\" to api.dialects and name a streaming model in realtime.model",
				},
			})
			return
		}
		if intent := r.URL.Query().Get("intent"); intent != "" && intent != "transcription" {
			writeJSON(w, http.StatusBadRequest, eventError{
				Type: serverError, EventID: newEventID(),
				Error: errorBody{
					Type: "invalid_request_error", Code: "unsupported_intent", Param: "intent",
					Message: "this server serves intent=transcription only; it has no model that replies",
				},
			})
			return
		}

		// OpenAI's clients put the model in the query string, so a copy-pasted
		// "gpt-4o-transcribe" arrives here rather than in session.update. It
		// is answered before the upgrade, where a client sees it as a failed
		// connection with a readable body instead of a socket that opens and
		// then complains.
		if asked := r.URL.Query().Get("model"); asked != "" {
			if loaded := deps.Realtime.Describe().Model; !sameModel(asked, loaded) {
				writeJSON(w, http.StatusBadRequest, eventError{
					Type: serverError, EventID: newEventID(),
					Error: errorBody{
						Type: "invalid_request_error", Code: "model_not_found", Param: "model",
						Message: "this server streams with " + loaded +
							"; a session cannot choose another model",
					},
				})
				return
			}
		}

		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// "realtime" is what OpenAI's clients ask for; the key-bearing
			// subprotocol is consumed by the authentication middleware and
			// must not be echoed back as the negotiated one.
			Subprotocols: []string{"realtime", "openai-beta.realtime-v1"},
			// Any origin. The alternative — refusing a cross-origin upgrade —
			// protects against a hostile page acting as the user, and it
			// cannot happen here: this server authenticates with a bearer
			// token or a subprotocol, never with a cookie, so a page that does
			// not already have an API key gains nothing by connecting.
			OriginPatterns:  []string{"*"},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			// Accept has already written the failure.
			return
		}

		ping, write, grace := a.timings()
		c := &conn{
			ws:     ws,
			rt:     deps.Realtime,
			ping:   ping,
			write:  write,
			grace:  grace,
			format: defaultFormat(),
			params: core.RealtimeParams{
				APIKeyID:  httpx.APIKeyID(r.Context()),
				ModelID:   r.URL.Query().Get("model"),
				AutoTurns: true,
			},
			warned: map[string]bool{},
		}
		c.run(r.Context())
	}
}

// conn is one websocket connection.
//
// The division of work is two goroutines and no locks around the state. The
// request goroutine reads the socket and owns everything describing the
// session: the format, the parameters, the carried-over audio byte. A second
// goroutine, started when the session opens, forwards recognition events to the
// socket and owns nothing else. Writes need no lock because the websocket
// library serialises them.
type conn struct {
	ws *websocket.Conn
	rt core.RealtimeService
	// ping, write and grace are the timings above, resolved once.
	ping  time.Duration
	write time.Duration
	grace time.Duration

	// --- request goroutine ---
	sess   core.RealtimeSession
	format inputFormat
	params core.RealtimeParams
	carry  []byte
	warned map[string]bool

	// --- event goroutine ---
	lastItem string

	// --- shared ---
	opened atomic.Bool
	// handling is true while the read loop is acting on a message. It matters
	// for one reason: a websocket ping is answered by a pong that only the
	// read loop can collect, and that loop blocks on purpose while the
	// recogniser catches up with a client sending faster than real time. A
	// ping sent then would time out and close a connection whose peer is
	// demonstrably alive — it has just sent audio.
	handling atomic.Bool
	pumpDone chan struct{}
	cancel   context.CancelFunc
}

func (c *conn) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	defer cancel()
	// CloseNow rather than Close: by the time this runs the connection has
	// either been closed politely already or is being abandoned, and a close
	// handshake with a peer that has gone away is a wait for nothing.
	defer c.ws.CloseNow()

	c.ws.SetReadLimit(maxMessageBytes)

	if err := c.send(ctx, c.sessionState(serverSessionCreated)); err != nil {
		return
	}
	go c.keepalive(ctx)

	c.read(ctx)

	// The client is gone. Release the decoder slot, then give the event
	// goroutine a moment to finish so a final transcript still reaches the
	// socket if it is on its way.
	if c.sess != nil {
		_ = c.sess.Close()
		select {
		case <-c.pumpDone:
		case <-time.After(2 * time.Second):
		}
	}
}

// read is the client event loop. It returns when the socket does.
func (c *conn) read(ctx context.Context) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		c.handling.Store(true)
		keep := true
		switch typ {
		case websocket.MessageText:
			keep = c.handleText(ctx, data)
		case websocket.MessageBinary:
			// The documented extension: a binary frame is audio in the
			// declared format, with none of base64's third of overhead.
			keep = c.appendAudio(ctx, data, "")
		}
		c.handling.Store(false)
		if !keep {
			return
		}
	}
}

// handleText acts on one client event. It returns false to end the connection.
func (c *conn) handleText(ctx context.Context, data []byte) bool {
	var ev clientEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return c.sendError(ctx, errorBody{
			Type: "invalid_request_error", Code: "invalid_json",
			Message: "the message is not valid JSON",
		}) == nil
	}

	switch ev.Type {
	case clientAudioAppend:
		raw, err := base64.StdEncoding.DecodeString(ev.Audio)
		if err != nil {
			return c.sendError(ctx, errorBody{
				Type: "invalid_request_error", Code: "invalid_audio", Param: "audio",
				EventID: ev.EventID,
				Message: "audio must be base64; send a binary frame to avoid encoding it at all",
			}) == nil
		}
		return c.appendAudio(ctx, raw, ev.EventID)

	case clientAudioCommit:
		sess, ok := c.ensureSession(ctx)
		if !ok {
			return false
		}
		sess.Commit()
		return true

	case clientAudioClear:
		c.carry = nil
		if c.sess != nil {
			c.sess.Clear()
		} else {
			// Nothing has been recognised yet, so the acknowledgement is all
			// there is to do.
			return c.send(ctx, eventCleared{Type: serverCleared, EventID: newEventID()}) == nil
		}
		return true

	case clientSessionUpdate, clientTranscriptionSessionUpdate:
		return c.updateSession(ctx, ev)

	case "":
		return c.sendError(ctx, errorBody{
			Type: "invalid_request_error", Code: "missing_type",
			Message: "every client event needs a type",
		}) == nil

	default:
		return c.sendError(ctx, errorBody{
			Type: "invalid_request_error", Code: "unsupported_event", EventID: ev.EventID,
			Message: "this server transcribes and does not reply, so it serves only " +
				"session.update, input_audio_buffer.append, .commit and .clear; got " + ev.Type,
		}) == nil
	}
}

// appendAudio decodes a frame and queues it.
func (c *conn) appendAudio(ctx context.Context, raw []byte, eventID string) bool {
	if len(raw) == 0 {
		return true
	}
	sess, ok := c.ensureSession(ctx)
	if !ok {
		return false
	}

	// A frame boundary has nothing to do with a sample boundary, so a byte
	// left over from the previous frame starts this one.
	if len(c.carry) > 0 {
		raw = append(c.carry, raw...)
		c.carry = nil
	}
	samples, rest := c.format.decode(raw)
	c.carry = rest

	if err := sess.Append(c.format.rate, samples); err != nil {
		e := core.AsError(err)
		_ = c.send(ctx, eventError{
			Type: serverError, EventID: newEventID(),
			Error: errorBody{
				Type: errorTypeFor(e.Code), Code: string(e.Code),
				Message: e.Message, EventID: eventID,
			},
		})
		c.closeWith(statusFor(e.Code), e.Message)
		return false
	}
	return true
}

// ensureSession opens the recognition session on first use.
//
// Lazily, not at connect: a client that opens a socket and sends nothing would
// otherwise hold a decoder slot, and the session parameters it is about to send
// would arrive too late to be honoured.
func (c *conn) ensureSession(ctx context.Context) (core.RealtimeSession, bool) {
	if c.sess != nil {
		return c.sess, true
	}

	sess, err := c.rt.Open(ctx, c.params)
	if err != nil {
		e := core.AsError(err)
		_ = c.send(ctx, eventError{
			Type: serverError, EventID: newEventID(),
			Error: errorBody{
				Type: errorTypeFor(e.Code), Code: string(e.Code),
				Message: e.Message, Param: e.Param,
			},
		})
		c.closeWith(statusFor(e.Code), e.Message)
		return nil, false
	}

	c.sess = sess
	c.opened.Store(true)
	c.pumpDone = make(chan struct{})
	go c.pump(ctx, sess)
	return sess, true
}

// updateSession applies what can be applied and reports what cannot.
func (c *conn) updateSession(ctx context.Context, ev clientEvent) bool {
	if len(ev.Session) == 0 {
		return c.sendError(ctx, errorBody{
			Type: "invalid_request_error", Code: "missing_session", Param: "session",
			EventID: ev.EventID, Message: "session.update needs a session object",
		}) == nil
	}

	var patch sessionPatch
	if err := json.Unmarshal(ev.Session, &patch); err != nil {
		return c.sendError(ctx, errorBody{
			Type: "invalid_request_error", Code: "invalid_session", Param: "session",
			EventID: ev.EventID, Message: "the session object could not be read: " + err.Error(),
		}) == nil
	}

	// Audio format: the beta field, or the GA nesting under audio.input.
	format := patch.InputAudioFormat
	if len(format) == 0 && patch.Audio != nil && patch.Audio.Input != nil {
		format = patch.Audio.Input.Format
	}
	if len(format) > 0 && string(format) != "null" {
		f, err := parseFormat(format)
		if err != nil {
			e := core.AsError(err)
			return c.sendError(ctx, errorBody{
				Type: "invalid_request_error", Code: string(e.Code), Param: e.Param,
				EventID: ev.EventID, Message: e.Message,
			}) == nil
		}
		// The carried byte belongs to the old format's sample stream; keeping
		// it would misalign the first sample of the new one.
		c.carry = nil
		c.format = f
	}

	info := c.rt.Describe()
	if t := patch.InputAudioTranscription; t != nil {
		if t.Model != "" && !sameModel(t.Model, info.Model) {
			return c.sendError(ctx, errorBody{
				Type: "invalid_request_error", Code: "model_not_found",
				Param: "input_audio_transcription.model", EventID: ev.EventID,
				Message: "this server streams with " + info.Model +
					"; a session cannot choose another model",
			}) == nil
		}
		if t.Language != "" {
			c.params.Language = t.Language
			if !supports(info.Languages, t.Language) {
				c.warn(ctx, "language_ignored", "input_audio_transcription.language",
					"the loaded model is "+strings.Join(info.Languages, ",")+
						" only, so language has no effect")
			}
		}
		if t.Prompt != "" {
			c.warn(ctx, "prompt_ignored", "input_audio_transcription.prompt",
				"prompt is not applied: biasing a streaming recogniser means a second "+
					"resident copy of the model, which a session cannot ask for")
		}
	}

	// turn_detection: null switches automatic turns off, which this server can
	// honour per session. The timings inside an object it cannot.
	if len(patch.TurnDetection) > 0 {
		auto := string(patch.TurnDetection) != "null"
		if auto != c.params.AutoTurns && c.sess != nil {
			c.warn(ctx, "turn_detection_fixed", "turn_detection",
				"turn detection cannot be switched after audio has been sent; "+
					"set it before the first append")
		} else {
			c.params.AutoTurns = auto
		}
		if auto {
			var td turnDetection
			if err := json.Unmarshal(patch.TurnDetection, &td); err == nil && td.SilenceDurationMS > 0 {
				c.warn(ctx, "turn_detection_fixed", "turn_detection.silence_duration_ms",
					"the silence that ends an utterance is fixed when the model is loaded "+
						"(realtime.endpoint in the server configuration)")
			}
		}
	}

	for _, ignored := range ignoredFields(patch) {
		c.warn(ctx, "field_ignored", ignored,
			"this server transcribes audio and produces no audio or text of its own")
	}

	return c.send(ctx, c.sessionState(serverSessionUpdated)) == nil
}

// pump forwards recognition events to the socket.
func (c *conn) pump(ctx context.Context, sess core.RealtimeSession) {
	defer close(c.pumpDone)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sess.Events():
			if !ok {
				// The session ended on its own terms and said why in the event
				// before this one, if there was a reason worth sending.
				c.closeWith(websocket.StatusNormalClosure, "the session ended")
				c.cancel()
				return
			}
			if !c.emit(ctx, ev) {
				c.cancel()
				return
			}
		}
	}
}

// emit writes one recognition event as the protocol's events. It returns false
// when the connection is finished.
func (c *conn) emit(ctx context.Context, ev core.RealtimeEvent) bool {
	switch ev.Kind {
	case core.RealtimeSpeechStarted:
		return c.send(ctx, eventSpeech{
			Type: serverSpeechStarted, EventID: newEventID(),
			ItemID: ev.Item, AudioStartMS: ptr(ev.StartMS),
		}) == nil

	case core.RealtimePartial:
		if ev.Delta == "" && ev.Text == "" {
			return true
		}
		return c.send(ctx, eventDelta{
			Type: serverDelta, EventID: newEventID(), ItemID: ev.Item,
			Delta:   ev.Delta,
			NanoASR: &deltaExtra{Text: ev.Text, StartMS: ev.StartMS},
		}) == nil

	case core.RealtimeSpeechStopped:
		return c.send(ctx, eventSpeech{
			Type: serverSpeechStopped, EventID: newEventID(),
			ItemID: ev.Item, AudioEndMS: ptr(ev.EndMS),
		}) == nil

	case core.RealtimeFinal:
		// One utterance ends as three events, in the order OpenAI sends them:
		// the buffer is committed, the item that holds the audio is created,
		// and its transcription completes.
		if err := c.send(ctx, eventCommitted{
			Type: serverCommitted, EventID: newEventID(),
			ItemID: ev.Item, PreviousItemID: c.lastItem,
		}); err != nil {
			return false
		}
		if err := c.send(ctx, eventItemCreated{
			Type: serverItemCreated, EventID: newEventID(), PreviousItemID: c.lastItem,
			Item: item{
				ID: ev.Item, Object: "realtime.item", Type: "message",
				Role: "user", Status: "completed",
				Content: []itemContent{{Type: "input_audio", Transcript: ptr(ev.Text)}},
			},
		}); err != nil {
			return false
		}
		c.lastItem = ev.Item
		return c.send(ctx, eventCompleted{
			Type: serverCompleted, EventID: newEventID(), ItemID: ev.Item,
			Transcript: ev.Text,
			NanoASR: &completedExtra{
				StartMS: ev.StartMS, EndMS: ev.EndMS,
				Model: c.rt.Describe().Model, Words: ev.Words,
			},
		}) == nil

	case core.RealtimeCleared:
		return c.send(ctx, eventCleared{Type: serverCleared, EventID: newEventID()}) == nil

	case core.RealtimeError:
		e := core.AsError(ev.Err)
		_ = c.send(ctx, eventError{
			Type: serverError, EventID: newEventID(),
			Error: errorBody{
				Type: errorTypeFor(e.Code), Code: string(e.Code), Message: e.Message,
			},
		})
		c.closeWith(statusFor(e.Code), e.Message)
		return false

	default:
		return true
	}
}

// keepalive pings, and closes a connection that never got started.
func (c *conn) keepalive(ctx context.Context) {
	ticker := time.NewTicker(c.ping)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.opened.Load() && time.Since(start) > c.grace {
				c.closeWith(websocket.StatusPolicyViolation, "no audio was sent")
				c.cancel()
				return
			}
			if c.handling.Load() {
				// Nothing to find out: the peer sent something a moment ago,
				// and the pong could not be read while that is being acted on.
				continue
			}
			pingCtx, cancel := context.WithTimeout(ctx, c.write)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

// sessionState describes the session as it stands.
func (c *conn) sessionState(kind string) eventSessionState {
	info := c.rt.Describe()

	var notes []string
	if !info.Punctuation {
		notes = append(notes,
			"this model writes no punctuation and no capitals; the offline API does")
	}
	if c.format.rate != info.SampleRate {
		notes = append(notes,
			"audio is resampled from the input rate to the model's on the way in")
	}
	notes = append(notes,
		"a delta event also carries the full hypothesis in nanoasr.text, "+
			"because a streaming decoder can retract a word it already sent")

	var turn *turnDetection
	if c.params.AutoTurns {
		turn = &turnDetection{Type: "server_vad"}
	}

	id := "sess_pending"
	if c.sess != nil {
		id = c.sess.ID()
	}
	return eventSessionState{
		Type:    kind,
		EventID: newEventID(),
		Session: sessionObject{
			ID:               id,
			Object:           "realtime.transcription_session",
			Model:            info.Model,
			InputAudioFormat: c.format.String(),
			InputAudioTranscription: &transcriptionPatch{
				Model:    info.Model,
				Language: c.params.Language,
			},
			TurnDetection: turn,
			NanoASR: sessionExtra{
				ModelSampleRate: info.SampleRate,
				InputSampleRate: c.format.rate,
				Languages:       info.Languages,
				Punctuation:     info.Punctuation,
				WordTimestamps:  info.WordTimestamps,
				MaxSessions:     info.MaxSessions,
				ActiveSessions:  info.Active,
				Notes:           notes,
			},
		},
	}
}

// warn reports a field that was accepted and ignored, once per field.
func (c *conn) warn(ctx context.Context, code, param, message string) {
	if c.warned[code+param] {
		return
	}
	c.warned[code+param] = true
	_ = c.send(ctx, eventWarning{
		Type: serverWarning, EventID: newEventID(),
		Code: code, Param: param, Message: message,
	})
}

func (c *conn) sendError(ctx context.Context, body errorBody) error {
	return c.send(ctx, eventError{Type: serverError, EventID: newEventID(), Error: body})
}

// send writes one event. No lock: the websocket library serialises writes, and
// the per-write deadline is what keeps a stalled client from holding one.
func (c *conn) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, c.write)
	defer cancel()
	return c.ws.Write(writeCtx, websocket.MessageText, b)
}

// closeWith sends a close frame, truncating the reason to what the protocol
// allows. A reason over 123 bytes is not a long reason, it is a dropped frame.
func (c *conn) closeWith(status websocket.StatusCode, reason string) {
	if len(reason) > closeReasonMax {
		reason = reason[:closeReasonMax-3] + "..."
	}
	_ = c.ws.Close(status, reason)
}

// statusFor maps a domain error onto a close code, so that a client can tell
// "come back later" from "you did something wrong" without parsing prose.
func statusFor(code core.Code) websocket.StatusCode {
	switch code {
	case core.CodeQueueFull, core.CodeRateLimited:
		return websocket.StatusTryAgainLater
	case core.CodeDraining:
		return websocket.StatusGoingAway
	case core.CodeInvalidRequest, core.CodeModelNotFound, core.CodeCapabilityUnavailable:
		return websocket.StatusUnsupportedData
	case core.CodeProcessingTimeout:
		// Not a failure: the session reached a limit it was always going to.
		return websocket.StatusNormalClosure
	default:
		return websocket.StatusInternalError
	}
}

// errorTypeFor is OpenAI's coarse error class, which clients branch on.
func errorTypeFor(code core.Code) string {
	switch code {
	case core.CodeInternal:
		return "server_error"
	case core.CodeRateLimited, core.CodeQueueFull:
		return "rate_limit_error"
	default:
		return "invalid_request_error"
	}
}

func sameModel(asked, loaded string) bool {
	if asked == loaded {
		return true
	}
	base, _, _ := strings.Cut(loaded, "@")
	return asked == base
}

func supports(languages []string, lang string) bool {
	if len(languages) == 0 || lang == "" {
		return true
	}
	lang = strings.ToLower(strings.TrimSpace(lang))
	// A client may send "ru-RU" where the manifest says "ru".
	base, _, _ := strings.Cut(lang, "-")
	for _, l := range languages {
		l = strings.ToLower(l)
		if l == lang || l == base || l == "multi" {
			return true
		}
	}
	return false
}

// ignoredFields names the parameters of the conversation API that a
// transcription server is handed and cannot use.
func ignoredFields(p sessionPatch) []string {
	var out []string
	add := func(present bool, name string) {
		if present {
			out = append(out, name)
		}
	}
	add(len(p.Modalities) > 0, "modalities")
	add(p.Instructions != "", "instructions")
	add(p.Temperature != nil, "temperature")
	add(p.Voice != "", "voice")
	add(hasValue(p.OutputAudioFormat), "output_audio_format")
	add(hasValue(p.MaxResponseOutput), "max_response_output_tokens")
	add(hasValue(p.Tools), "tools")
	add(hasValue(p.ToolChoice), "tool_choice")
	add(hasValue(p.InputAudioNoise), "input_audio_noise_reduction")
	return out
}

func hasValue(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
