"""Per-speaker clips for the isolation benchmarks, from Hugging Face's dataset
viewer: 25 consecutive rows from each offset, the longest run of one speaker,
40 seconds of it.

  python -I fetch_speakers.py OUT/en "openslr/librispeech_asr&config=clean&split=test" 0,500,1000,1500,2000
  python -I fetch_speakers.py OUT/ru "bond005/rulibrispeech&config=default&split=test" 0,500,750

Russian LibriSpeech carries no speaker ids: offsets 0 and 250, and 750 and
1000, turned out to be the same readers (CAM++ cosine 0.78 and 0.79 against
0.14-0.28 between different ones), which is why the Russian set is 0, 500, 750.
"""
import json, sys, urllib.request, io, os
import numpy as np, soundfile as sf
out, ds, offsets = sys.argv[1], sys.argv[2], [int(x) for x in sys.argv[3].split(',')]
def rows(off, n):
    u = f"https://datasets-server.huggingface.co/rows?dataset={ds}&offset={off}&length={n}"
    return json.load(urllib.request.urlopen(u))["rows"]
for off in offsets:
    groups = {}
    for r in rows(off, 25):
        row = r["row"]
        spk = row.get("speaker_id", f"blk{off}")
        a = row["audio"][0]["src"]
        x, sr = sf.read(io.BytesIO(urllib.request.urlopen(a).read()), dtype="float32")
        assert sr == 16000, sr
        g = groups.setdefault(spk, {"x": [], "t": []})
        g["x"].append(x); g["t"].append(row.get("text") or row.get("transcription"))
    spk, g = max(groups.items(), key=lambda kv: sum(len(v) for v in kv[1]["x"]))
    x = np.concatenate(g["x"])[: 16000 * 40]
    sf.write(os.path.join(out, f"{spk}.wav"), x, 16000)
    print(spk, round(len(x) / 16000, 1), "s |", g["t"][0][:70])
