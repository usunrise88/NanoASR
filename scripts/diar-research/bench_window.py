"""int8 on the same curve, plus the chunked/single-window crossover and memory."""
import resource, statistics, time
import numpy as np, onnxruntime as ort

base = np.load("bench_embeds.npy").astype(np.float32)
CTX = 264 + 40 + 40   # speaker cache + FIFO + right context, the offline config

def session(path, threads=4):
    so = ort.SessionOptions(); so.intra_op_num_threads = threads; so.inter_op_num_threads = 1
    return ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])

def timed(sess, T, repeats=5):
    x = np.tile(base, (1, T // base.shape[1] + 1, 1))[:, :T, :].copy()
    sess.run(None, {"embeds": x})
    ts = []
    for _ in range(repeats):
        t0 = time.perf_counter(); sess.run(None, {"embeds": x}); ts.append(time.perf_counter() - t0)
    return statistics.median(ts)

fp32, int8 = session("models/step.onnx"), session("models/q_perchan.onnx")

print(f"{'T':>6} {'audio':>8} {'fp32 one window':>17} {'int8 one window':>17} {'fp32 chunked':>14}")
for T in (344, 684, 1024, 1536, 2048, 3072):
    a = T * 0.08
    t32, t8 = timed(fp32, T), timed(int8, T)
    # same T as one step, but only T-CTX frames of it are new audio
    chunk = T - CTX
    rtf_chunked = t32 / (chunk * 0.08) if chunk > 0 else float("nan")
    print(f"{T:>6} {a:>7.1f}s {t32/a:>17.4f} {t8/a:>18.4f} {rtf_chunked:>15.4f}")

print()
print("peak process RSS: %.0f MB" % (resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024))
