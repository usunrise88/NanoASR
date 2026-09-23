# era dialect: whisperx reference renderings

These files are the output of whisperX's own writers — `WriteTXT`, `WriteSRT`,
`WriteVTT`, `WriteTSV` and `WriteJSON` from `whisperx/utils.py` — run over the
fixtures in `internal/api/era/conformance_test.go`, with the options
whisper-asr-webservice passes them: `max_line_width=1000`, `max_line_count=2`,
`highlight_words=false` (the `SUBTITLE_*` defaults from its `app/config.py`).

They exist because the era dialect replaces a deployment of that service, and
"follows the reference" is a claim that decays silently. `TestRenderingMatchesTheWhisperxGolden`
checks the four text formats byte for byte and the json one as data: Python's
`json.dump` writes `", "` and `": "` separators that Go's encoder does not, which
no parser can observe.

`short` covers the cue break: three segments whose words run on, with a pause of
more than three seconds before the third. `long` crosses the hour, where WebVTT
starts writing the hours field that SubRip always writes. `wrapping` is nine
hundred short words with no pause at all, which is the only way to reach the
other two branches of the writer: the wrap onto a second line, and the cue break
that follows once a cue holds `max_line_count` of them.

To regenerate, render the same fixtures through whisperX's writers — the service
reaches them via `app/asr_models/mbain_whisperx_engine.py`.
