"""Generate the golden data the Go Sortformer diarizer is tested against.

Everything comes from the transformers implementation at a pinned revision —
the one the published ONNX graphs were exported from — so a Go test failure
means the port drifted from the reference, not that two references disagree.

Writes testdata/golden/sortformer/:

  reference.json  every parameter of the offline pass, read off the loaded model
                  and processor rather than copied from a README
  frontend.json   log-mel features at the lengths where the arithmetic has edges
  frontend.bin    their rows, float32
  cache.json      the speaker cache and chunk loop, driven model-free
  offline.json    the real model over real speech
  offline.bin     its speaker probabilities at 10 ms, float32

The cache vectors need no model and almost no bytes. A fake encoder and head
replace the network, and the logits they produce are a pure function of which
recording frame an embedding came from, computed with splitmix64 — so Go can
regenerate every logit bit for bit, and only the cache's decisions have to be
stored: which frames it kept, in what order, at every step.

Run in an environment with transformers from git main:
    venv-hf/bin/python golden_sortformer.py --audio ../../testdata/audio/ru-16k.wav
"""

import argparse
import json
import math
import os
import wave

import numpy as np
import torch
from transformers import AutoProcessor, Nemotron3DiarizationForAudioFrameClassification
from transformers.modeling_outputs import BaseModelOutput

MODEL_ID = "nvidia/Nemotron-3-Diarization"
MODEL_REVISION = "a435e9867d79e789e90053f9b6d6834053af564a"
TRANSFORMERS_REVISION = "f324707307757d9c0b8dac1c4462eceff911fa2f"
OUT = os.path.join(os.path.dirname(__file__), "..", "..", "testdata", "golden", "sortformer")

# ---------------------------------------------------------------------------
# splitmix64, identical to the Go side

MASK = (1 << 64) - 1


def splitmix64(x):
    x = (x + 0x9E3779B97F4A7C15) & MASK
    z = x
    z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & MASK
    z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & MASK
    return z ^ (z >> 31)


def unit(key):
    """A float64 in [0, 1) from a non-negative integer key."""
    return (splitmix64(key) >> 11) * (1.0 / (1 << 53))


def fake_logit(scenario, frame, sub, speaker):
    """Logit of one 10 ms frame for one speaker, as a function of where it came from.

    frame is the recording's encoder-frame index, or -1 for the learned silence
    embedding. Speakers take turns in runs of `run` frames, and in the overlap
    scenario a second speaker talks over the first for part of each run.
    """
    if frame < 0:
        return np.float32(-6.0)
    turn = (frame // scenario["run"]) % scenario["speakers"]
    active = speaker == turn
    if scenario.get("overlap") and frame % scenario["run"] < scenario["run"] // 3:
        active = active or speaker == (turn + 1) % scenario["speakers"]
    key = ((scenario["seed"] * 1_000_003 + frame) * 8 + sub) * 8 + speaker
    noise = (unit(key) - 0.5) * 4.0
    return np.float32((4.0 if active else -4.0) + noise)


# ---------------------------------------------------------------------------


def read_wav(path):
    with wave.open(path) as w:
        assert w.getnchannels() == 1 and w.getframerate() == 16000 and w.getsampwidth() == 2
        pcm = np.frombuffer(w.readframes(w.getnframes()), dtype="<i2")
    # exactly what internal/audio does for 16-bit PCM
    return pcm.astype(np.float32) / np.float32(32768.0)


def tiled(pcm, n):
    reps = n // len(pcm) + 1
    return np.tile(pcm, reps)[:n]


def features(processor, signal):
    """Valid log-mel rows only: the processor zeroes and masks the extra STFT frame."""
    inputs = processor.feature_extractor(signal, sampling_rate=16000, return_tensors="pt")
    feats = inputs["input_features"][0]
    valid = int(inputs["attention_mask"][0].sum())
    return feats[:valid].numpy().astype(np.float32)


def write_reference(model, processor):
    fe = processor.feature_extractor
    c = model.config
    sc = c.streaming_config
    ref = {
        "model_id": MODEL_ID,
        "model_revision": MODEL_REVISION,
        "transformers_revision": TRANSFORMERS_REVISION,
        "sample_rate": fe.sampling_rate,
        "n_fft": fe.n_fft,
        "hop_length": fe.hop_length,
        "win_length": fe.win_length,
        "num_mel_bins": fe.feature_size,
        "preemphasis": fe.preemphasis,
        "log_zero_guard": 2.0 ** -24,
        "subsampling_factor": c.audio_config.subsampling_factor,
        "hidden_size": c.audio_config.hidden_size,
        "max_position_embeddings": c.audio_config.max_position_embeddings,
        "num_speakers": c.head_config.num_speakers,
        "chunk_length": c.chunk_length,
        "chunk_right_context": c.chunk_right_context,
        "fifo_length": c.fifo_length,
        "speaker_cache_update_period": c.speaker_cache_update_period,
        "speaker_cache_length": sc.speaker_cache_length,
        "speaker_cache_silence_frames_per_speaker": sc.speaker_cache_silence_frames_per_speaker,
        "prediction_score_threshold": sc.prediction_score_threshold,
        "latest_frames_score_boost": sc.latest_frames_score_boost,
        "min_positive_scores_rate": sc.min_positive_scores_rate,
        "strong_boost_rate": sc.strong_boost_rate,
        "weak_boost_rate": sc.weak_boost_rate,
        "speech_threshold": 0.5,
    }
    with open(os.path.join(OUT, "reference.json"), "w") as f:
        json.dump(ref, f, indent=2)
    print("reference.json:", {k: ref[k] for k in ("chunk_length", "chunk_right_context", "fifo_length",
                                                  "speaker_cache_update_period", "speaker_cache_length")})


def write_frontend(processor, speech):
    cases = []
    for n in (1, 159, 160, 161, 511, 512, 513, 16000, 435199, 435200, 435201):
        cases.append(("speech", n))
    for kind in ("silence", "clip"):
        for n in (513, 16000):
            cases.append((kind, n))

    blob, index = [], []
    for kind, n in cases:
        if kind == "speech":
            sig = tiled(speech, n)
        elif kind == "silence":
            sig = np.zeros(n, dtype=np.float32)
        else:  # full-scale square wave: the clipping case
            sig = np.where((np.arange(n) // 18) % 2 == 0, 1.0, -1.0).astype(np.float32)
        feats = features(processor, sig)
        head = feats[:8]
        tail = feats[max(len(feats) - 8, len(head)):]
        index.append({"kind": kind, "samples": n, "frames": len(feats),
                      "head_rows": len(head), "tail_rows": len(tail),
                      "offset": sum(b.size for b in blob)})
        blob += [head.ravel(), tail.ravel()]
    np.concatenate(blob).astype("<f4").tofile(os.path.join(OUT, "frontend.bin"))
    with open(os.path.join(OUT, "frontend.json"), "w") as f:
        json.dump({"note": "speech is ru-16k.wav tiled to length, decoded as int16/32768; "
                           "clip is a +/-1 square wave with a 36-sample period",
                   "cases": index}, f, indent=1)
    print(f"frontend: {len(index)} cases, {sum(b.size for b in blob) * 4 / 1024:.0f} KB")


def write_offline(model, processor, speech):
    """The real network over real speech, long enough for the cache to compress."""
    gap = np.zeros(6400, dtype=np.float32)
    audio = np.concatenate([speech, gap, speech, gap, speech])
    inputs = processor(audio, sampling_rate=16000, return_tensors="pt")
    with torch.no_grad():
        logits = model(**inputs).logits[0]
    valid = int(inputs["attention_mask"][0].sum())
    probs = torch.sigmoid(logits[:valid]).numpy().astype("<f4")
    probs.tofile(os.path.join(OUT, "offline.bin"))
    with open(os.path.join(OUT, "offline.json"), "w") as f:
        json.dump({"note": "ru-16k.wav, 0.4 s of silence, ru-16k.wav, 0.4 s, ru-16k.wav",
                   "gap_samples": len(gap), "samples": len(audio), "frames": valid,
                   "speakers": probs.shape[1]}, f, indent=1)
    active = (probs > 0.5).any(0)
    print(f"offline: {len(audio) / 16000:.1f}s, {valid} frames, active channels {np.flatnonzero(active).tolist()}")


class _FakeEmbedder(torch.nn.Module):
    """Encoder frame t of the recording becomes the embedding [t, 0]."""

    def forward(self, input_features):
        n = input_features.shape[1] // 8
        ids = torch.arange(n, dtype=torch.float32)
        return torch.stack([ids, torch.zeros(n)], dim=-1)[None]


class _FakeTower(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.embedder = _FakeEmbedder()


class _FakeModel(torch.nn.Module):
    def __init__(self, log):
        super().__init__()
        self.audio_tower = _FakeTower()
        self.log = log

    def forward(self, inputs_embeds=None, attention_mask=None, position_ids=None, **kw):
        self.log.append([int(v) for v in inputs_embeds[0, :, 0].tolist()])
        return BaseModelOutput(last_hidden_state=inputs_embeds)


class _FakeHead(torch.nn.Module):
    def __init__(self, scenario):
        super().__init__()
        self.scenario = scenario

    def forward(self, hidden):
        frames = [int(v) for v in hidden[0, :, 0].tolist()]
        out = np.empty((len(frames) * 8, 8), dtype=np.float32)
        for r, g in enumerate(frames):
            for sub in range(8):
                for spk in range(8):
                    out[r * 8 + sub, spk] = fake_logit(self.scenario, g, sub, spk)
        return torch.from_numpy(out)[None]


def write_cache(model):
    """Drive the real offline forward with a fake network and record the cache."""
    from transformers.models.nemotron3_diarization import modeling_nemotron3_diarization as mod

    scenarios = [
        {"name": "two speakers", "seed": 1, "speakers": 2, "run": 150, "frames": 800},
        {"name": "four speakers", "seed": 2, "speakers": 4, "run": 60, "frames": 1100},
        {"name": "eight overlapping", "seed": 3, "speakers": 8, "run": 45, "frames": 1100, "overlap": True},
        {"name": "one chunk", "seed": 4, "speakers": 3, "run": 40, "frames": 300},
    ]
    real_model, real_head, real_sil = model.model, model.classifier, model.silence_embeds
    real_update = mod.Nemotron3DiarizationSpeakerCache.update
    out = []
    try:
        for sc in scenarios:
            inputs_log, states = [], []

            def update(self, *a, **kw):
                real_update(self, *a, **kw)
                states.append({
                    "cache": [int(v) for v in self.embeds[0, : self.num_cache_frames, 0].tolist()],
                    "fifo": [int(v) for v in self.fifo[0, : self.num_fifo_frames, 0].tolist()],
                    "compressed": bool(self.is_compressed),
                })

            model.model = _FakeModel(inputs_log)
            model.classifier = _FakeHead(sc)
            model.silence_embeds = torch.nn.Parameter(torch.tensor([-1.0, 0.0]), requires_grad=False)
            mod.Nemotron3DiarizationSpeakerCache.update = update
            with torch.no_grad():
                logits = model(input_features=torch.zeros(1, sc["frames"] * 8, 128)).logits[0]
            # the output rows must be every recording frame, once, in order
            assert logits.shape[0] == sc["frames"] * 8
            out.append({**sc, "steps": [{"input": i, **s} for i, s in zip(inputs_log, states)]})
            print(f"cache: {sc['name']}: {len(states)} steps, "
                  f"compressed from step {next((k for k, s in enumerate(states) if s['compressed']), None)}")
    finally:
        model.model, model.classifier, model.silence_embeds = real_model, real_head, real_sil
        mod.Nemotron3DiarizationSpeakerCache.update = real_update
    with open(os.path.join(OUT, "cache.json"), "w") as f:
        json.dump({"note": "logits are fake_logit(scenario, frame, sub, speaker) from golden_sortformer.py; "
                           "silence is frame -1", "scenarios": out}, f, separators=(",", ":"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--audio", required=True, help="testdata/audio/ru-16k.wav")
    args = ap.parse_args()
    os.makedirs(OUT, exist_ok=True)
    torch.manual_seed(0)

    processor = AutoProcessor.from_pretrained(MODEL_ID, revision=MODEL_REVISION)
    model = Nemotron3DiarizationForAudioFrameClassification.from_pretrained(
        MODEL_ID, revision=MODEL_REVISION, attn_implementation="eager").eval()
    speech = read_wav(args.audio)

    write_reference(model, processor)
    write_frontend(processor, speech)
    write_offline(model, processor, speech)
    write_cache(model)


if __name__ == "__main__":
    main()
