"""Benchmark step.onnx variants on CPU, with quality measured against fp32.

One step in the offline configuration is [spkcache 264 | FIFO 40 | chunk 340 |
right context 40] = 684 embedding frames, and it advances the transcript by the
chunk alone: 340 frames x 80 ms = 27.2 s. RTF is therefore time / 27.2, not
time / (684 x 0.08) — the context is re-read every step and must not be counted
as progress.
"""
import json, os, statistics, sys, time
import numpy as np, onnxruntime as ort

SPKCACHE, FIFO, CHUNK, RC = 264, 40, 340, 40
T = SPKCACHE + FIFO + CHUNK + RC          # 684
AUDIO_PER_STEP = CHUNK * 0.08             # 27.2 s
REPEATS = int(os.environ.get("REPEATS", 7))

embeds = np.load("bench_embeds.npy")[:, :T, :].astype(np.float32)
assert embeds.shape[1] == T, embeds.shape


def run(path, threads, graph_opt=ort.GraphOptimizationLevel.ORT_ENABLE_ALL,
        spinning=True, arena=True):
    so = ort.SessionOptions()
    so.intra_op_num_threads = threads
    so.inter_op_num_threads = 1
    so.graph_optimization_level = graph_opt
    so.enable_cpu_mem_arena = arena
    if not spinning:
        so.add_session_config_entry("session.intra_op.allow_spinning", "0")
    sess = ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])
    feed = {"embeds": embeds}
    sess.run(None, feed)                  # warm up
    times = []
    for _ in range(REPEATS):
        t0 = time.perf_counter()
        out = sess.run(None, feed)[0]
        times.append(time.perf_counter() - t0)
    return statistics.median(times), min(times), out


def quality(out, ref):
    p_out, p_ref = 1 / (1 + np.exp(-out)), 1 / (1 + np.exp(-ref))
    return {
        "max_abs": float(np.abs(p_out - p_ref).max()),
        "mean_abs": float(np.abs(p_out - p_ref).mean()),
        "agree": float(((p_out > 0.5) == (p_ref > 0.5)).mean()),
    }


if __name__ == "__main__":
    cases = json.loads(sys.argv[1])
    ref = None
    print(f"{'variant':38} {'thr':>4} {'median':>9} {'best':>8} {'RTF':>7}  quality")
    for c in cases:
        med, best, out = run(c["path"], c["threads"],
                             getattr(ort.GraphOptimizationLevel, c.get("opt", "ORT_ENABLE_ALL")),
                             c.get("spinning", True), c.get("arena", True))
        if ref is None:
            ref = out
            q = ""
        else:
            m = quality(out, ref)
            q = f"max {m['max_abs']:.2e}  mean {m['mean_abs']:.2e}  decisions {m['agree']*100:.4f}%"
        print(f"{c['name']:38} {c['threads']:>6} {med*1000:>8.0f}ms {best*1000:>7.0f}ms "
              f"{med/AUDIO_PER_STEP:>7.4f}  {q}")
