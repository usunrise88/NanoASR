"""Golden output of the Voxlock reference for internal/tse/voxlock.

The reference is voxlock 1.1.1 (pip install voxlock==1.1.1), whose core.py the
Go package ports. Both read testdata/audio/ru-16k.wav (scripts/fetch-testdata.sh)
and build the same input from it, so no audio is committed:

  sample = the first 3 s of the clip                       (the voice to keep)
  mix    = the rest of the clip + 0.7 x the clip reversed   (the voice to drop)

Reversed speech has the spectrum and the level of speech and no words, which
is all an interferer needs to be, and it is derived without ffmpeg, whose
filters change between versions.

Writes testdata/golden/voxlock/{golden.json,output.f32}: every 8th output
sample as little-endian float32, and the parameters the Go test checks.

  python -I scripts/tse-research/golden_voxlock.py .models/voxlock-int8@2026-10-12
"""
import hashlib, json, os, sys
import numpy as np
import soundfile as sf
from voxlock import core

DECIMATE = 8


def main():
    model_dir = sys.argv[1]
    clip_path = sys.argv[2] if len(sys.argv) > 2 else "testdata/audio/ru-16k.wav"
    out_dir = sys.argv[3] if len(sys.argv) > 3 else "testdata/golden/voxlock"

    clip, sr = sf.read(clip_path, dtype="float32")
    assert sr == 16000 and clip.ndim == 1, (sr, clip.shape)
    n = 3 * sr
    sample = clip[:n]
    rest = clip[n:]
    mix = (rest + np.float32(0.7) * clip[::-1][: len(rest)]).astype(np.float32)

    vl = core.VoxLock(model_dir=model_dir, precision="int8", threads=1)
    out = vl.isolate(mix, sample=sample)

    os.makedirs(out_dir, exist_ok=True)
    out[::DECIMATE].astype("<f4").tofile(os.path.join(out_dir, "output.f32"))
    with open(os.path.join(out_dir, "golden.json"), "w") as f:
        json.dump({
            "reference": "voxlock 1.1.1 core.VoxLock(precision='int8').isolate",
            "model_revision": core.REVISION,
            "clip_sha256": hashlib.sha256(open(clip_path, "rb").read()).hexdigest(),
            "interferer_gain": 0.7,
            "samples": len(out),
            "decimate": DECIMATE,
            "output_rms": float(np.sqrt((out ** 2).mean())),
        }, f, indent=2)
        f.write("\n")
    print(f"{len(out)} samples, rms {np.sqrt((out ** 2).mean()):.4f}")


if __name__ == "__main__":
    main()
