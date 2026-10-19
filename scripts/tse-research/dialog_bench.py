"""Both speakers of a real dialogue isolated through a running server, scored
against the reference turns: how much of each speaker is kept, how far the
other is pushed down.

  UTTERANCES=40 ./scripts/fetch-diar-eval.sh
  NANOASR_URL=http://127.0.0.1:8080 python -I dialog_bench.py \
      testdata/audio/diar-masha_dima_part1-40.wav testdata/audio/diar-masha_dima_part1-40.rttm OUT
"""
import io, json, os, sys, time, urllib.request, uuid
import numpy as np, soundfile as sf
SR = 16000; BASE = os.environ.get("NANOASR_URL", "http://127.0.0.1:8080") + "/api/v1/isolations"
wav, rttm = sys.argv[1], sys.argv[2]
x, sr = sf.read(wav, dtype="float32"); assert sr == SR
lab = {}
for line in open(rttm):
    f = line.split(); s, d, who = float(f[3]), float(f[4]), f[7]
    m = lab.setdefault(who, np.zeros(len(x), bool)); m[int(s * SR):int((s + d) * SR)] = True
def post(data):
    b = uuid.uuid4().hex
    body = (f"--{b}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"dialog.wav\"\r\n\r\n").encode() + data + f"\r\n--{b}--\r\n".encode()
    return json.load(urllib.request.urlopen(urllib.request.Request(BASE, body, {"Content-Type": f"multipart/form-data; boundary={b}"})))
def wait(i):
    while True:
        d = json.load(urllib.request.urlopen(f"{BASE}/{i}"))
        if d["status"] not in ("queued", "analysing", "extracting"): return d
        time.sleep(0.5)
rms = lambda v: np.sqrt((v ** 2).mean() + 1e-12)
t0 = time.time(); iso = post(open(wav, "rb").read()); d = wait(iso["id"]); ta = time.time() - t0
print(f"analysis {ta:.1f} s for {len(x)/SR:.0f} s; voices:", [(v["speaker"], v["total_speech"], (v.get("sample") or {}).get("start")) for v in d["voices"]])
for v in d["voices"]:
    if not v.get("sample"): continue
    st = v["sample"]["start"]; i, j = int(st * SR), int((st + 3) * SR)
    who = max(lab, key=lambda k: lab[k][i:j].mean())
    purity = lab[who][i:j].mean()
    r = urllib.request.Request(f"{BASE}/{iso['id']}/extract", json.dumps({"speaker": v["speaker"]}).encode(), {"Content-Type": "application/json"})
    urllib.request.urlopen(r); t1 = time.time(); wait(iso["id"]); te = time.time() - t1
    y, _ = sf.read(io.BytesIO(urllib.request.urlopen(f"{BASE}/{iso['id']}/result").read()), dtype="float32")
    own = lab[who] & ~np.logical_or.reduce([m for k, m in lab.items() if k != who])
    oth = np.logical_or.reduce([m for k, m in lab.items() if k != who]) & ~lab[who]
    print(f"{v['speaker']} = {who} (sample {st:.1f}s, {purity:.0%} of it {who}): own speech {20*np.log10(rms(y[own])/rms(x[own])):+.1f} dB, "
          f"other speaker {20*np.log10(rms(y[oth])/rms(x[oth])):+.1f} dB, corr in own {np.corrcoef(y[own], x[own])[0,1]:.3f}, extract {te:.0f} s ({te/(len(x)/SR):.2f}x)")
    sf.write(sys.argv[3] + f"/dialog-{who}.wav", y, SR)
