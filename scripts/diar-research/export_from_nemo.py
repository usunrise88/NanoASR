"""Spike: export Nemotron-3-Diarization to ONNX and prove the result is the model.

Order matters. The FlexAttention substitution is checked against the untouched
model BEFORE anything is exported, so that a later mismatch cannot be blamed on
the exporter, and an exporter mismatch cannot hide behind the patch.
"""
import os, time, warnings
warnings.filterwarnings("ignore")
import numpy as np
import torch

# torch >= 2.9 flipped torch.onnx.export to dynamo=True; NeMo's Exportable was
# written against the old default and does not forward the flag.
_orig_export = torch.onnx.export
torch.onnx.export = lambda *a, **kw: _orig_export(*a, **{**kw, "dynamo": kw.get("dynamo", False)})

from nemo.collections.asr.models import SortformerEncLabelModel
import flexpatch

CKPT, OUT = "models/Nemotron-3-Diarization.nemo", "models/sortformer.onnx"
AUDIO_SECONDS = 2112 * 0.01  # the chunk is 2112 mel frames at 10 ms

print("== load ==", flush=True)
model = SortformerEncLabelModel.restore_from(CKPT, map_location="cpu").eval()
inputs = model.streaming_input_examples(batch_size=1)

print("== 1. reference: untouched model, FlexAttention ==", flush=True)
t0 = time.time()
with torch.no_grad():
    reference = model.forward_for_export(*inputs)
flex_s = time.time() - t0
print(f"   {flex_s:.2f}s for {AUDIO_SECONDS:.1f}s audio -> RTF {flex_s/AUDIO_SECONDS:.3f}", flush=True)

print("== 2. equivalence: same model, SDPA instead of FlexAttention ==", flush=True)
flexpatch.apply()
t0 = time.time()
with torch.no_grad():
    patched = model.forward_for_export(*inputs)
sdpa_s = time.time() - t0
print(f"   {sdpa_s:.2f}s -> RTF {sdpa_s/AUDIO_SECONDS:.3f}", flush=True)

equiv = True
for name, a, b in zip(model.output_names, patched, reference):
    d = (a - b).abs().max().item()
    good = torch.allclose(a, b, rtol=1e-4, atol=1e-4)
    equiv &= good
    print(f"   {name:28} max|diff|={d:.3e}  {'OK' if good else 'MISMATCH'}")
print("   EQUIVALENCE:", "PASS" if equiv else "FAIL", flush=True)
if not equiv:
    raise SystemExit("patch changes the model; stopping before export")

print("== 3. ONNX export ==", flush=True)
t0 = time.time()
model.export(OUT, input_example=inputs, dynamic_axes={})
print(f"   exported in {time.time()-t0:.1f}s, {os.path.getsize(OUT)/1e6:.0f} MB", flush=True)

print("== 4. onnxruntime parity against the ORIGINAL model ==", flush=True)
import onnxruntime as ort
so = ort.SessionOptions()
so.intra_op_num_threads = os.cpu_count()
sess = ort.InferenceSession(OUT, so, providers=["CPUExecutionProvider"])
feed = {n: v.detach().cpu().numpy() for n, v in zip(model.input_names, inputs)}

sess.run(None, feed)  # warm up
t0 = time.time()
actual = sess.run(None, feed)
ort_s = time.time() - t0
print(f"   {ort_s:.2f}s -> RTF {ort_s/AUDIO_SECONDS:.3f}", flush=True)

ok = True
for name, a, e in zip(model.output_names, actual, reference):
    e = e.detach().cpu().numpy()
    d = np.abs(a - e).max()
    good = np.allclose(a, e, rtol=1e-4, atol=1e-4)
    ok &= good
    print(f"   {name:28} max|diff|={d:.3e}  {'OK' if good else 'MISMATCH'}")

print("PARITY:", "PASS" if ok else "FAIL")
print(f"SUMMARY rtf_flex={flex_s/AUDIO_SECONDS:.3f} rtf_sdpa={sdpa_s/AUDIO_SECONDS:.3f} "
      f"rtf_onnx={ort_s/AUDIO_SECONDS:.3f} threads={os.cpu_count()}")
