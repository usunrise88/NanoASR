"""Build a realistic benchmark input: real speech -> mel -> embeddings.

Quantization error depends on the distribution of activations, so measuring it
on torch.randn would say nothing about what the model does to speech. This uses
the project's own reference clip, doubled into a second, lower "voice" by
resampling, so the encoder sees speaker turns rather than one continuous talker.
"""
import warnings, wave, numpy as np, torch
warnings.filterwarnings("ignore")
from nemo.collections.asr.models import SortformerEncLabelModel
import onnxruntime as ort

SR = 16000

w = wave.open("models/ru-16k.wav")
assert w.getnchannels() == 1 and w.getframerate() == SR and w.getsampwidth() == 2
pcm = np.frombuffer(w.readframes(w.getnframes()), dtype="<i2").astype(np.float32) / 32768.0
print(f"   clip: {len(pcm)/SR:.1f}s")

def resample(x, factor):
    """Crude linear resample: shifts pitch and rate together, enough for a second voice."""
    n = int(len(x) / factor)
    idx = np.arange(n) * factor
    lo = np.floor(idx).astype(int).clip(0, len(x) - 2)
    frac = idx - lo
    return (x[lo] * (1 - frac) + x[lo + 1] * frac).astype(np.float32)

gap = np.zeros(int(0.4 * SR), dtype=np.float32)
voice_b = resample(pcm, 1.18)           # shorter and higher
voice_c = resample(pcm, 0.87)           # longer and lower
audio = np.concatenate([pcm, gap, voice_b, gap, voice_c, gap, pcm, gap, voice_b])
print(f"   assembled: {len(audio)/SR:.1f}s of speech with turns")
np.save("bench_audio.npy", audio)

model = SortformerEncLabelModel.restore_from("models/Nemotron-3-Diarization.nemo", map_location="cpu").eval()
with torch.no_grad():
    feats, feat_len = model.preprocessor(
        input_signal=torch.from_numpy(audio)[None, :],
        length=torch.tensor([len(audio)]),
    )
feats = feats.transpose(1, 2)           # (B, D, T) -> (B, T, D)
print(f"   mel features: {tuple(feats.shape)}  (10 ms frames, 128 mel)")
np.save("bench_feats.npy", feats.numpy())

sess = ort.InferenceSession("models/embed.onnx", providers=["CPUExecutionProvider"])
embeds = sess.run(None, {"features": feats.numpy()})[0]
print(f"   embeddings: {embeds.shape}  (80 ms frames, 512)")
print(f"   stats: mean {embeds.mean():.4f}, std {embeds.std():.4f}, "
      f"min {embeds.min():.3f}, max {embeds.max():.3f}")
np.save("bench_embeds.npy", embeds)
