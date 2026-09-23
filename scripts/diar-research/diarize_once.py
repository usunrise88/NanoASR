"""Run the whole recording as one window and read off the speaker turns.

The benchmark says this mode is fast. This says whether it diarizes: the test
audio is the reference clip, then a pitched-up copy, then a pitched-down copy,
then the first two again — five turns by three distinct voices.
"""
import numpy as np, onnxruntime as ort

feats = np.load("bench_feats.npy").astype(np.float32)
so = ort.SessionOptions(); so.intra_op_num_threads = 4
emb = ort.InferenceSession("models/embed.onnx", so, providers=["CPUExecutionProvider"])
step = ort.InferenceSession("models/step.onnx", so, providers=["CPUExecutionProvider"])

embeds = emb.run(None, {"features": feats})[0]
logits = step.run(None, {"embeds": embeds})[0]
probs = 1 / (1 + np.exp(-logits))[0]          # (frames, 8) at 10 ms
print(f"   {feats.shape[1]} mel frames -> {embeds.shape[1]} embeddings -> {probs.shape[0]} decision frames")

active = probs > 0.5
for spk in range(probs.shape[1]):
    a = active[:, spk]
    if not a.any():
        continue
    # contiguous runs, in seconds
    d = np.diff(np.concatenate([[0], a.view(np.int8), [0]]))
    starts, ends = np.where(d == 1)[0], np.where(d == -1)[0]
    keep = (ends - starts) >= 30          # ignore anything under 300 ms
    starts, ends = starts[keep], ends[keep]
    if len(starts) == 0:
        continue
    total = (ends - starts).sum() * 0.01
    turns = ", ".join(f"{s*0.01:.1f}-{e*0.01:.1f}" for s, e in zip(starts[:6], ends[:6]))
    print(f"   speaker{spk}: {len(starts)} turns, {total:.1f}s of speech | {turns}")
