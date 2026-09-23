"""Peak RSS for one session at a given window length, measured in its own process.

Attention is quadratic in memory as well as time, and NanoASR budgets model
residency (approx_rss_mb), so the window cap is a memory decision as much as a
latency one.
"""
import resource, sys
import numpy as np, onnxruntime as ort

path, T = sys.argv[1], int(sys.argv[2])
base = np.load("bench_embeds.npy").astype(np.float32)
x = np.tile(base, (1, T // base.shape[1] + 1, 1))[:, :T, :].copy()
so = ort.SessionOptions(); so.intra_op_num_threads = 4; so.inter_op_num_threads = 1
sess = ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])
sess.run(None, {"embeds": x})
print(f"{T:>6} {resource.getrusage(resource.RUSAGE_SELF).ru_maxrss/1024:>9.0f} MB")
