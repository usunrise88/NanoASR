package realtime

import (
	"encoding/base64"
	"slices"
	"testing"

	"github.com/coder/websocket"
)

func multilingualService() *fakeService {
	svc := newFakeService()
	svc.info.Model = "nemotron-3.5-streaming-multi@2026-06-11"
	svc.info.Languages = []string{"multi"}
	svc.info.Locales = []string{"en-US", "en-GB", "pt-BR", "pt-PT", "ru-RU"}
	svc.info.SampleRate = 16000
	return svc
}

// updateLanguage sends a session.update naming a language and returns the
// warning codes that came before the acknowledgement, and the acknowledged
// session.
func updateLanguage(t *testing.T, svc *fakeService, language string) (warnings []string, session map[string]any, ws *websocket.Conn) {
	t.Helper()
	ws, ctx := dial(t, serve(t, svc))
	expect(t, ctx, ws, serverSessionCreated)
	sendJSON(t, ctx, ws, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"input_audio_transcription": map[string]any{"language": language}},
	})
	for range 6 {
		ev := next(t, ctx, ws)
		switch ev["type"] {
		case serverWarning:
			warnings = append(warnings, ev["code"].(string))
		case serverSessionUpdated:
			return warnings, ev["session"].(map[string]any), ws
		}
	}
	t.Fatal("no session.updated arrived")
	return
}

func transcriptionLanguage(session map[string]any) string {
	t, _ := session["input_audio_transcription"].(map[string]any)
	return str(t["language"])
}

func TestTheSessionOffersTheModelsLocales(t *testing.T) {
	ws, ctx := dial(t, serve(t, multilingualService()))
	ev := expect(t, ctx, ws, serverSessionCreated)
	extra := ev["session"].(map[string]any)["nanoasr"].(map[string]any)
	locales, _ := extra["locales"].([]any)
	if len(locales) != 5 || locales[2] != "pt-BR" {
		t.Errorf("locales = %v", locales)
	}
}

func TestAMultilingualSessionResolvesItsLanguage(t *testing.T) {
	for asked, want := range map[string]string{
		"ru-RU": "ru-RU", "ru": "ru-RU", "pt": "pt-BR", "PT_pt": "pt-PT", "auto": "auto",
	} {
		t.Run(asked, func(t *testing.T) {
			warnings, session, _ := updateLanguage(t, multilingualService(), asked)
			if len(warnings) != 0 {
				t.Errorf("warnings: %v", warnings)
			}
			if got := transcriptionLanguage(session); got != want {
				t.Errorf("session language = %q, want %q", got, want)
			}
		})
	}
}

// A language the model does not offer leaves the session as it was and says
// so: a transcript in the wrong language with no explanation is the failure.
func TestAnUnofferedLanguageIsReportedAndNotApplied(t *testing.T) {
	warnings, session, _ := updateLanguage(t, multilingualService(), "de-DE")
	if !slices.Contains(warnings, "language_ignored") {
		t.Errorf("warnings %v should include language_ignored", warnings)
	}
	if got := transcriptionLanguage(session); got != "" {
		t.Errorf("session language = %q; an unoffered language must not be applied", got)
	}
}

// The language a session opened with arrives at the session already resolved,
// and a later change reaches the stream that is running.
func TestTheLanguageReachesTheSessionAndCanChange(t *testing.T) {
	svc := multilingualService()
	_, _, ws := updateLanguage(t, svc, "pt")
	ctx := t.Context()

	sendJSON(t, ctx, ws, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm16Bytes(0, 100, -100)),
	})
	sess := <-svc.opened
	if sess.params.Language != "pt-BR" {
		t.Fatalf("the session was opened with language %q, want pt-BR", sess.params.Language)
	}

	sendJSON(t, ctx, ws, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"input_audio_transcription": map[string]any{"language": "en"}},
	})
	waitFor(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return slices.Equal(sess.languages, []string{"en-US"})
	})
}

// A model with one language keeps the behaviour it had: a language it does not
// cover is warned about, and never forwarded.
func TestASingleLanguageModelStillWarnsAboutAnotherLanguage(t *testing.T) {
	warnings, _, _ := updateLanguage(t, newFakeService(), "en")
	if !slices.Contains(warnings, "language_ignored") {
		t.Errorf("warnings %v should include language_ignored", warnings)
	}
	warnings, session, _ := updateLanguage(t, newFakeService(), "ru-RU")
	if len(warnings) != 0 {
		t.Errorf("warnings for a language the model has: %v", warnings)
	}
	if got := transcriptionLanguage(session); got != "ru-RU" {
		t.Errorf("session language = %q", got)
	}
}
