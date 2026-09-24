"""Export Nemotron-3-Diarization to the graphs the Go diarizer loads.

Adapted from NealCaren/Nemotron-3-Diarization-ONNX (export_onnx.py), with the
model and transformers revisions pinned, so that the artifacts we ship are ones
we produced rather than ones we downloaded.

Writes into --out:
  embed.onnx          log-mel [1, N, 128] -> embeddings [1, ceil(N/8), 512]
  step.onnx           embeddings [1, T, 512] -> speaker logits [1, 8T, 8] at 10 ms
  mel_filters.bin     float32 [128, 257], the extractor's own Slaney filterbank
  silence_embeds.bin  float32 [512], the speaker cache's learned silence slot

step.onnx has no attention-mask input: whatever it is given, it attends to all
of it. Callers must feed densely packed sequences and never padding.

Run in an environment with transformers from git main:
    venv-hf/bin/python export_onnx.py --out models/
"""

import argparse
import os

import numpy as np
import torch
from transformers import AutoProcessor, Nemotron3DiarizationForAudioFrameClassification

MODEL_ID = "nvidia/Nemotron-3-Diarization"
MODEL_REVISION = "a435e9867d79e789e90053f9b6d6834053af564a"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)

    model = Nemotron3DiarizationForAudioFrameClassification.from_pretrained(
        MODEL_ID, revision=MODEL_REVISION, attn_implementation="eager").eval()
    processor = AutoProcessor.from_pretrained(MODEL_ID, revision=MODEL_REVISION)

    class Embed(torch.nn.Module):
        def __init__(self):
            super().__init__()
            self.e = model.model.audio_tower.embedder

        def forward(self, feats):
            return self.e(feats)

    class Step(torch.nn.Module):
        """[speaker cache, FIFO, chunk, right context] embeddings -> speaker logits at 10 ms."""

        def __init__(self):
            super().__init__()
            self.m, self.c = model.model, model.classifier

        def forward(self, embeds):
            # positions restart at every step, as in the offline forward
            pos = torch.arange(embeds.shape[1], device=embeds.device)[None, :]
            return self.c(self.m(inputs_embeds=embeds, position_ids=pos).last_hidden_state)

    with torch.no_grad():
        torch.onnx.export(Embed(), (torch.randn(1, 340 * 8, 128),), os.path.join(args.out, "embed.onnx"),
                          input_names=["features"], output_names=["embeds"],
                          dynamic_axes={"features": {1: "n"}, "embeds": {1: "t"}},
                          opset_version=17, dynamo=False)
        torch.onnx.export(Step(), (torch.randn(1, 420, 512),), os.path.join(args.out, "step.onnx"),
                          input_names=["embeds"], output_names=["logits"],
                          dynamic_axes={"embeds": {1: "t"}, "logits": {1: "t8"}},
                          opset_version=17, dynamo=False)

    model.silence_embeds.detach().numpy().astype("<f4").tofile(os.path.join(args.out, "silence_embeds.bin"))
    mel = processor.feature_extractor.mel_filters.numpy().astype("<f4")
    assert mel.shape == (128, 257), mel.shape
    mel.tofile(os.path.join(args.out, "mel_filters.bin"))

    # the exported step must reproduce the network it came from
    import onnxruntime as ort
    x = torch.randn(1, 380, 512)
    with torch.no_grad():
        ref = Step()(x).numpy()
    got = ort.InferenceSession(os.path.join(args.out, "step.onnx")).run(None, {"embeds": x.numpy()})[0]
    print("step.onnx max|diff| vs torch:", float(np.abs(ref - got).max()))


if __name__ == "__main__":
    main()
