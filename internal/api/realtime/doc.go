package realtime

import "github.com/usunrise88/nanoasr/internal/api/adapter"

// Doc describes this dialect for /docs.
func (*Adapter) Doc() adapter.Doc {
	return adapter.Doc{
		Name:  "realtime",
		Title: "OpenAI realtime transcription (websocket)",
		Summary: "Recognition while the audio is still arriving, over a websocket, in the " +
			"event protocol of OpenAI's realtime API with intent=transcription. " +
			"Needs a streaming model: realtime.model in the configuration.",
		Routes: []adapter.Route{{
			Method:  "GET",
			Path:    "/v1/realtime?intent=transcription",
			Summary: "Open a streaming recognition session (websocket upgrade).",
			Detail: "Client events: session.update (or transcription_session.update), " +
				"input_audio_buffer.append with base64 audio, .commit and .clear. A " +
				"binary frame is accepted as raw audio in the declared format, which " +
				"saves base64's third of overhead. Server events: " +
				"transcription_session.created and .updated, " +
				"input_audio_buffer.speech_started, .speech_stopped, .committed, " +
				".cleared, conversation.item.created, " +
				"conversation.item.input_audio_transcription.delta and .completed, " +
				"error, and nanoasr.warning for a parameter that was accepted and " +
				"ignored. Every delta also carries the full hypothesis in " +
				"nanoasr.text, because a streaming decoder can retract a word it " +
				"already sent and no sequence of deltas expresses that. " +
				"input_audio_format takes pcm16 (24 kHz by default), g711_ulaw, " +
				"g711_alaw, or an object {\"type\":\"audio/pcm\",\"rate\":16000}. " +
				"The model, the decoding method and the endpoint timings belong to " +
				"the server and cannot be chosen per session; prompt is not applied. " +
				"Authenticate with Authorization: Bearer, or from a browser with the " +
				"subprotocol openai-insecure-api-key.<key>.",
		}, {
			Method:  "POST",
			Path:    "/v1/realtime/transcription_sessions",
			Summary: "Not implemented: this server mints no ephemeral tokens.",
			Detail:  "Answers 501 and says how to connect with an ordinary API key instead.",
		}},
	}
}
