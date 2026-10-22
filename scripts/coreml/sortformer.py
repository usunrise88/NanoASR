"""Nemotron-3-Diarization's two graphs for CoreML, at fixed shapes:
nemotron-3-diarization-coreml.

  embed  features [1, 8 E, 128]           -> embeds [1, E, 512]
  step   embeds [1, T, 512], mask [1, T]  -> logits [1, 8 T, 8]

The Go loop feeds step a sequence that varies in length (the speaker cache
and FIFO fill up over the first chunks, the last chunk is short). The ONNX
graph attends to everything it is given, so it is fed the exact frames; here
the length is fixed and mask marks the valid prefix:

  * attention ignores the masked keys (an additive -1e4);
  * the projection's output is zeroed past the valid frames before the
    kernel-3 upsampling convolution, which is what the end of a dense
    sequence shows it — the convolution's zero padding.

Rows past the valid frames come out as garbage and the caller drops them.
E and T are the largest the chunk loop asks for.

The model comes through transformers at the revision scripts/diar-research
exports the ONNX graphs from, so both builds start from the same checkpoint.

usage: sortformer.py
"""
import shutil

import torch
from torch import nn
from transformers import AutoProcessor, Nemotron3DiarizationForAudioFrameClassification

import common

MODEL_ID = "nemotron-3-diarization-coreml"
HF_ID = "nvidia/Nemotron-3-Diarization"
HF_REVISION = "a435e9867d79e789e90053f9b6d6834053af564a"
E = 380   # embed frames per call
T = 684   # step frames per call


def main():
    model = Nemotron3DiarizationForAudioFrameClassification.from_pretrained(
        HF_ID, revision=HF_REVISION, attn_implementation="eager").eval()
    # Traced as constants: the weights must not ask for gradients.
    model.requires_grad_(False)
    processor = AutoProcessor.from_pretrained(HF_ID, revision=HF_REVISION)
    tower = model.model.audio_tower
    cfg = tower.config

    class Embed(nn.Module):
        """Feature stacking with literal sizes: traced shapes become integer
        casts CoreML refuses."""

        def __init__(self):
            super().__init__()
            self.proj = tower.embedder.projection

        def forward(self, feats):
            return self.proj(feats.reshape(1, E, 8 * 128))

    class DenseStep(nn.Module):
        """What step.onnx is: the reference for the masked one."""

        def forward(self, embeds):
            pos = torch.arange(embeds.shape[1])[None, :]
            return model.classifier(model.model(inputs_embeds=embeds, position_ids=pos).last_hidden_state)

    class MaskedStep(nn.Module):
        def __init__(self):
            super().__init__()
            pos = torch.arange(T)[None, :]
            cos, sin = tower.rotary_emb(torch.zeros(1, T, cfg.hidden_size), pos)
            self.register_buffer("cos", cos.unsqueeze(1).clone())   # [1, 1, T, rot]
            self.register_buffer("sin", sin.unsqueeze(1).clone())
            self.rot = int(cos.shape[-1])
            a = tower.layers[0].self_attn
            self.h, self.d, self.kv = cfg.num_attention_heads, a.head_dim, cfg.num_key_value_heads

        def rope(self, x):
            rot = self.rot
            xr, xp = x[..., :rot], x[..., rot:]
            half = rot // 2
            rh = torch.cat([-xr[..., half:], xr[..., :half]], dim=-1)
            return torch.cat([xr * self.cos + rh * self.sin, xp], dim=-1)

        def attn(self, a, x, amask):
            b, t = 1, T
            q = a.q_proj(x).view(b, t, self.h, self.d).transpose(1, 2)
            k = a.k_proj(x).view(b, t, self.kv, self.d).transpose(1, 2)
            v = a.v_proj(x).view(b, t, self.h, self.d).transpose(1, 2)
            q, k = self.rope(q), self.rope(k)
            if self.kv != self.h:
                rep = self.h // self.kv
                k = k.repeat_interleave(rep, dim=1)
                v = v.repeat_interleave(rep, dim=1)
            s = torch.matmul(q, k.transpose(2, 3)) * a.scaling + amask
            o = torch.matmul(torch.softmax(s, dim=-1), v).transpose(1, 2).reshape(b, t, self.h * self.d)
            return a.o_proj(o)

        def forward(self, embeds, mask):
            amask = ((1.0 - mask) * -1e4).view(1, 1, 1, T)
            x = tower.input_layer_norm(embeds)
            for L in tower.layers:
                x = x + self.attn(L.self_attn, L.layer_norm1(x), amask)
                x = x + L.mlp(L.layer_norm2(x))
            x = tower.layer_norm(x)
            x = model.model.proj(x) * mask.view(1, T, 1)
            up = model.model.upsampler
            x = up.conv(x.transpose(1, 2)).transpose(1, 2).reshape(1, T * up.upsample_factor, -1)
            return model.classifier(x)

    masked, dense, emb = MaskedStep().eval(), DenseStep().eval(), Embed().eval()
    torch.manual_seed(0)
    with torch.no_grad():
        worst = 0.0
        for n in (T, 380, 517, 41):
            x = torch.randn(1, n, cfg.hidden_size)
            padded = torch.zeros(1, T, cfg.hidden_size)
            padded[:, :n] = x
            m = torch.zeros(1, T)
            m[:, :n] = 1
            worst = max(worst, (masked(padded, m)[:, :8 * n] - dense(x)).abs().max().item())
        print(f"masked step vs dense, torch fp32: max abs diff {worst:.2e}")
        assert worst < 1e-4, worst
        f = torch.randn(1, 8 * E, 128)
        d = (emb(f) - tower.embedder(f)).abs().max().item()
        print(f"embed rewrite vs embedder: max abs diff {d:.2e}")
        assert d < 1e-4, d
        ex_embed = (torch.zeros(1, 8 * E, 128),)
        ex_step = (torch.zeros(1, T, cfg.hidden_size), torch.ones(1, T))
        tr_embed = torch.jit.trace(emb, ex_embed)
        tr_step = torch.jit.trace(masked, ex_step)

    build = common.BUILD / "sortformer"
    embed = common.save(common.convert(tr_embed, [("features", ex_embed[0])], ["embeds"]), build / "embed.mlpackage")
    step = common.save(common.convert(tr_step, list(zip(["embeds", "mask"], ex_step)), ["logits"]), build / "step.mlpackage")

    pkg = common.package_dir(MODEL_ID)
    common.compile_into(embed, pkg / "embed.mlmodelc")
    common.compile_into(step, pkg / "step.mlmodelc")
    model.silence_embeds.detach().numpy().astype("<f4").tofile(pkg / "silence_embeds.bin")
    mel = processor.feature_extractor.mel_filters.numpy().astype("<f4")
    assert mel.shape == (128, 257), mel.shape
    mel.tofile(pkg / "mel_filters.bin")
    # The offline pass's parameters, as the ONNX package carries them.
    shutil.copy(common.ROOT / "testdata" / "golden" / "sortformer" / "reference.json", pkg / "config.json")
    shutil.copy(common.ROOT / "scripts" / "diar-research" / "LICENSE-OpenMDW-1.1.txt", pkg / "LICENSE")
    common.notice(pkg, f"""
Nemotron-3-Diarization, by NVIDIA Corporation.
https://huggingface.co/{HF_ID}
Revision {HF_REVISION}, licensed under OpenMDW-1.1 (LICENSE).""")
    print("packaged", pkg)


if __name__ == "__main__":
    main()
