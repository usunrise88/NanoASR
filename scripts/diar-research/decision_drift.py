"""Where do the int8 decisions differ from fp32 — and does it matter?

"99.8% of frames agree" is not by itself an answer: a flip on a frame where the
model reported 0.50 is the model being undecided, and moves a segment boundary
by one 10 ms frame. A flip on a frame where it reported 0.95 would be the
quantization inventing speech. These are different failures and need separating.
"""
import numpy as np, onnxruntime as ort

T = 684
x = np.load("bench_embeds.npy")[:, :T, :].astype(np.float32)

def probs(path):
    so = ort.SessionOptions(); so.intra_op_num_threads = 4
    s = ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])
    return 1 / (1 + np.exp(-s.run(None, {"embeds": x})[0]))

ref = probs("models/step.onnx")
for name, path in (("int8 as published", "models/step_int8.onnx"), ("int8 per-channel", "models/q_perchan.onnx")):
    p = probs(path)
    flip = (p > 0.5) != (ref > 0.5)
    n = int(flip.sum())
    print(f"{name}: {n} of {ref.size} decisions differ ({n/ref.size*100:.4f}%)")
    if n:
        conf = ref[flip]
        # how far the reference was from the 0.5 boundary on the frames that flipped
        margin = np.abs(conf - 0.5)
        print(f"   reference confidence on those frames: median {np.median(conf):.4f}, "
              f"min {conf.min():.4f}, max {conf.max():.4f}")
        print(f"   distance from the 0.5 threshold: median {np.median(margin):.5f}, max {margin.max():.5f}")
        for thr in (0.01, 0.05, 0.10):
            share = float((margin < thr).mean())
            print(f"   share of differences within +/-{thr:.2f} of the threshold: {share*100:.1f}%")
    # speech/no-speech per frame, which is what a segment boundary is made of
    sp_ref, sp_p = (ref > 0.5).any(-1), (p > 0.5).any(-1)
    print(f"   frames carrying speech: reference {int(sp_ref.sum())}, this {int(sp_p.sum())}, "
          f"agreement {float((sp_ref == sp_p).mean())*100:.4f}%")
    print()
