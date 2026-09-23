"""Check their step.onnx against the NeMo checkpoint's own encoder + head."""
import warnings, numpy as np, torch
warnings.filterwarnings("ignore")
from nemo.collections.asr.models import SortformerEncLabelModel
import flexpatch, onnxruntime as ort

model = SortformerEncLabelModel.restore_from("models/Nemotron-3-Diarization.nemo", map_location="cpu").eval()
flexpatch.apply()
inputs = list(model.streaming_input_examples(batch_size=1))
# streaming_input_examples marks only half the speaker cache as valid, so NeMo
# masks those positions and their graph — which takes no lengths at all — cannot.
# A real caller feeds a cache whose length matches its extent; do that here, so
# the comparison measures the graphs and not the padding convention.
inputs[3] = torch.full_like(inputs[3], inputs[2].shape[1])
inputs = tuple(inputs)
chunk, chunk_lengths, spkcache, spkcache_lengths, fifo, fifo_lengths = inputs
print(f"   spkcache: {tuple(spkcache.shape)}, lengths now {spkcache_lengths.tolist()}")

with torch.no_grad():
    reference = model.forward_for_export(*inputs)[0]          # (1, 528, 8) @ 80 ms
    chunk_embs, chunk_len = model._call_pre_encode(chunk, chunk_lengths)
    embs, _ = model.sortformer_modules.concat_and_pad(
        [spkcache, fifo, chunk_embs],
        [spkcache_lengths, fifo_lengths, chunk_len.to(torch.int64)],
        output_length=spkcache.shape[1] + fifo.shape[1] + chunk_embs.shape[1],
    )
print(f"   concatenated embeddings: {tuple(embs.shape)}")

sess = ort.InferenceSession("models/step.onnx", providers=["CPUExecutionProvider"])
logits = sess.run(None, {"embeds": embs.numpy()})[0]
print(f"   their logits: {logits.shape}  (10 ms resolution)")

probs = torch.sigmoid(torch.from_numpy(logits))
# Fold 10 ms back to the 80 ms grid with the model's own pooling, so the
# comparison cannot be accused of using a different reduction.
theirs = model.sortformer_modules.downsample_preds(probs, model.upsample_factor)
print(f"   after the model's own downsample: {tuple(theirs.shape)} vs reference {tuple(reference.shape)}")

d = (theirs - reference).abs().max().item()
agree = ((theirs > 0.5) == (reference > 0.5)).float().mean().item()
print(f"   max|diff| = {d:.3e}")
print(f"   agreement on thresholded decisions = {agree*100:.4f}%")
print("   VERDICT:", "OK" if d < 1e-3 else "MISMATCH")
