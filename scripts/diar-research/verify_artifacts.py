"""Verify the third-party ONNX export against the original NVIDIA weights.

The files come from a repository published an hour ago with no downloads. The
point of this script is that we do not have to trust it: the NeMo checkpoint is
the same weights the exporter started from, so its own pre-encode and its own
learned silence embedding are the ground truth those files must reproduce.
"""
import warnings, numpy as np, torch
warnings.filterwarnings("ignore")
from nemo.collections.asr.models import SortformerEncLabelModel
import flexpatch, onnxruntime as ort

model = SortformerEncLabelModel.restore_from("models/Nemotron-3-Diarization.nemo", map_location="cpu").eval()
flexpatch.apply()

print("== 1. silence_embeds.bin vs the checkpoint's learnable_sil_emb ==")
theirs = np.fromfile("models/silence_embeds.bin", dtype="<f4")
ours = model.sortformer_modules.learnable_sil_emb.detach().numpy().ravel()
print(f"   shapes: theirs {theirs.shape}, ours {ours.shape}")
if theirs.shape == ours.shape:
    d = np.abs(theirs - ours).max()
    print(f"   max|diff| = {d:.3e}  {'OK' if d < 1e-5 else 'MISMATCH'}")
else:
    print("   MISMATCH: different shapes")

print("== 2. embed.onnx vs the checkpoint's pre-encode ==")
feats = torch.randn(1, 2112, 128)
lengths = torch.tensor([2112], dtype=torch.long)
with torch.no_grad():
    ours_emb, ours_len = model._call_pre_encode(feats, lengths)
sess = ort.InferenceSession("models/embed.onnx", providers=["CPUExecutionProvider"])
theirs_emb = sess.run(None, {"features": feats.numpy()})[0]
print(f"   shapes: theirs {theirs_emb.shape}, ours {tuple(ours_emb.shape)}")
if theirs_emb.shape == tuple(ours_emb.shape):
    d = np.abs(theirs_emb - ours_emb.numpy()).max()
    print(f"   max|diff| = {d:.3e}  {'OK' if d < 1e-3 else 'MISMATCH'}")
else:
    print("   MISMATCH: different shapes")

print("== 3. mel_filters.bin: shape and sanity ==")
mel = np.fromfile("models/mel_filters.bin", dtype="<f4")
print(f"   {mel.size} floats = {mel.size/257:.1f} x 257 (expected 128 x 257)")
mel = mel.reshape(-1, 257)
print(f"   shape {mel.shape}, min {mel.min():.4f}, max {mel.max():.4f}, "
      f"all non-negative: {bool((mel >= 0).all())}")
print(f"   filter sums: first {mel[0].sum():.4f}, mean {mel.sum(1).mean():.4f}")
