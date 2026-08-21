# Test fixtures

Short audio plus golden transcripts for integration tests (SPEC §15).

Rules:

- Keep clips under ~10 seconds; the repository is not an audio archive.
- Cover the telephony case explicitly: 8 kHz, a-law and mu-law, mono.
- Golden transcripts store word timings; tests compare with a ±50 ms tolerance,
  because token timestamps are frame-aligned and exact equality would make the
  suite fail on an unrelated model update.
- Regenerate with `make golden-update`, never by hand.
- Only clips that may be redistributed under the repository licence.
