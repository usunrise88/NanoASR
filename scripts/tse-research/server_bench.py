"""End-to-end bench through a running server with isolation enabled:
two-speaker dialogues with turns and one 3-second overlap, each voice
isolated, scored against its own track.

  NANOASR_URL=http://127.0.0.1:8080 python -I server_bench.py OUT

OUT holds audio/en/*.wav and audio/ru/*.wav from fetch_speakers.py.
"""
import io, itertools, json, os, sys, time, urllib.request, uuid
import numpy as np, soundfile as sf
SR = 16000
BASE = os.environ.get("NANOASR_URL", "http://127.0.0.1:8080") + "/api/v1/isolations"
root = sys.argv[1]
sets = {"en": ["1580", "2961", "4507", "6930", "8455"], "ru": ["blk0", "blk500", "blk750"]}

def post_file(url, name, data):
    b = uuid.uuid4().hex
    body = (f"--{b}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{name}\"\r\n"
            "Content-Type: audio/wav\r\n\r\n").encode() + data + f"\r\n--{b}--\r\n".encode()
    r = urllib.request.Request(url, body, {"Content-Type": f"multipart/form-data; boundary={b}"})
    return json.load(urllib.request.urlopen(r))
def get(url): return json.load(urllib.request.urlopen(url))
def wait(id_):
    while True:
        d = get(f"{BASE}/{id_}")
        if d["status"] not in ("queued", "analysing", "extracting"): return d
        time.sleep(0.3)
def extract(id_, spk):
    r = urllib.request.Request(f"{BASE}/{id_}/extract", json.dumps({"speaker": spk}).encode(), {"Content-Type": "application/json"})
    urllib.request.urlopen(r); t0 = time.time(); d = wait(id_); dt = time.time() - t0
    y, _ = sf.read(io.BytesIO(urllib.request.urlopen(f"{BASE}/{id_}/result").read()), dtype="float32")
    return d, y, dt

def sisdr(e, r):
    a = (e @ r) / (r @ r); t = a * r; return 10 * np.log10((t @ t) / ((e - t) @ (e - t)))
rms = lambda v: np.sqrt((v ** 2).mean() + 1e-12)

def dialogue(A, B):
    # A 8 s, B 7 s, A 6 s with B over its last 3 s, B 6 s, A 7 s; 0.3 s gaps
    b = B * rms(A) / rms(B)
    plan = [("A", 0, 8), ("B", 0, 7), ("A", 8, 14), ("B", 7, 13), ("A", 14, 21)]
    x = np.zeros(0, np.float32); ra = np.zeros(0, np.float32); rb = np.zeros(0, np.float32); gap = np.zeros(int(0.3 * SR), np.float32)
    for who, s, e in plan:
        seg = (A if who == "A" else b)[s * SR:e * SR]; z = np.zeros_like(seg)
        x = np.concatenate([x, seg, gap]); ra = np.concatenate([ra, seg if who == "A" else z, gap]); rb = np.concatenate([rb, z if who == "A" else seg, gap])
    st = int((8 + 0.3 + 7 + 0.3 + 3) * SR); ov = b[20 * SR:23 * SR]
    x[st:st + len(ov)] += ov; rb[st:st + len(ov)] += ov
    return x, ra, rb

for lang, spk in sets.items():
    rows = []
    for p, q in itertools.combinations(spk, 2):
        A = sf.read(f"{root}/audio/{lang}/{p}.wav", dtype="float32")[0]; B = sf.read(f"{root}/audio/{lang}/{q}.wav", dtype="float32")[0]
        x, ra, rb = dialogue(A, B)
        buf = io.BytesIO(); sf.write(buf, x, SR, format="WAV", subtype="PCM_16")
        iso = post_file(BASE, f"{p}-{q}.wav", buf.getvalue()); d = wait(iso["id"])
        voices = [v for v in d.get("voices", []) if v.get("sample")]
        for v in voices:
            st = v["sample"]["start"]; i, j = int(st * SR), int((st + 3) * SR)
            who = "A" if rms(ra[i:j]) > rms(rb[i:j]) else "B"
            ref, oth = (ra, rb) if who == "A" else (rb, ra)
            _, y, dt = extract(iso["id"], v["speaker"])
            own_only = (np.abs(oth) < 1e-6) & (np.abs(ref) > 0)
            oth_only = (np.abs(ref) < 1e-6) & (np.abs(oth) > 0)
            rows.append(dict(pair=f"{p}/{q}", who=who, n=len(d["voices"]),
                             sisdr_in=sisdr(x, ref), sisdr_out=sisdr(y, ref),
                             keep=20 * np.log10(rms(y[own_only]) / rms(x[own_only])),
                             sup=20 * np.log10(rms(y[oth_only]) / rms(x[oth_only])),
                             rtf=dt / (len(x) / SR)))
        urllib.request.urlopen(urllib.request.Request(f"{BASE}/{iso['id']}", method="DELETE"))
    for r in rows:
        print(f"[{lang}] {r['pair']:12s} {r['who']} voices={r['n']} SI-SDR {r['sisdr_in']:5.1f} -> {r['sisdr_out']:5.1f}  own {r['keep']:+5.1f} dB  other {r['sup']:+6.1f} dB  {r['rtf']:.2f}x", flush=True)
    imp = np.array([r["sisdr_out"] - r["sisdr_in"] for r in rows])
    print(f"[{lang}] {len(rows)} isolations: SI-SDR +{imp.mean():.1f} dB (worst +{imp.min():.1f}), own {np.mean([r['keep'] for r in rows]):+.1f} dB (worst {min(r['keep'] for r in rows):+.1f}), other {np.median([r['sup'] for r in rows]):+.1f} dB median (worst {max(r['sup'] for r in rows):+.1f}), {np.mean([r['rtf'] for r in rows]):.2f}x real time", flush=True)
