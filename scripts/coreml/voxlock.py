"""Voxlock voice isolation for CoreML, as the three models the Go engine runs
(internal/tse/voxlock/coreml.go): voxlock-int8-coreml.

  enroll.mlmodelc  the author's enrollment graph, on the GPU in fp32: it runs
                   once per voice, and its Kaldi filterbank squares values no
                   fp16 holds
                     enroll [1, 48000] -> enroll_mag [1, 257, 376],
                                          context [1, 512, 298]
  front.mlmodelc   the STFT, the voice map, the subband norms and the fusion
                   with the voice's context, on the GPU in fp16
                     wav [1, (K + 3) 128], enroll_mag, context
                       -> feat [1, 32, 128, K], re, im [1, 257, K]
  back.mlmodelc    the band-split RNN and the masks, on the Neural Engine
                     feat, re, im, h, c [6, 32, 256] -> frames [1, K, 512],
                                                        h_out, c_out

front and back are the author's own streaming Chunk (export_onnx.py in the
voxlock repository) cut in two, with these replaced and nothing else:

  * the STFT: a 512-wide strided convolution has no Neural Engine kernel, so
    the window is cut into frames by reshaping and multiplied by the DFT basis;
  * the twelve LSTMs, written out as matrix products over a fixed number of
    steps: CoreML's lstm op has no Neural Engine kernel either. The input
    projection of every step is one batched product; only the recurrence is
    sequential;
  * the magnitudes and norms that square values fp16 cannot hold squared;
  * the scale: the audio is multiplied by SCALE on the way in and the frames
    divided by it on the way out. The subband norms take an epsilon of 1.2e-7
    and quiet bands a variance of 1e-8, neither of which fp16 holds; scaling
    the input by SCALE and their epsilon by SCALE^2 is the same computation in
    numbers it does. Everything else either normalises or is linear in the
    input.

On the Neural Engine the front's steps cost the extraction 9 dB of agreement
with the fp32 model; on the GPU, in the same fp16, they cost nothing
measurable. Hence the cut.

usage: voxlock.py
"""
import shutil
import sys
import types

import torch
import torch.nn.functional as F
from torch import nn

import common

# WeSpeaker imports its SSL front ends (s3prl, whisper, w2v-BERT) at package
# import; this model uses none of them, and they would pull in a second ML
# stack. Its own front-end registry is all that is needed, and is empty.
_frontend = types.ModuleType("wespeaker.frontend")
_frontend.frontend_class_dict = {}
sys.modules.setdefault("wespeaker.frontend", _frontend)

REPO = common.SRC / "voxlock"
common.add_path(REPO, REPO / "wesep")
import export_onnx as vx  # noqa: E402

MODEL_ID = "voxlock-int8-coreml"
K = 100           # frames per call: 0.8 s
ENROLL = 48000    # enrollment samples: 3 s
HOP, NFFT, NF = 128, 512, 257
SCALE = 64.0


def load_tse():
    import yaml
    from safetensors.torch import load_file
    from wesep.models import get_model

    w = common.WEIGHTS / "voxlock"
    cfg = yaml.safe_load(open(w / "config.yaml"))
    # Nothing to fetch: every weight, the speaker encoder's included, is in
    # the safetensors file.
    cfg["model_args"]["tse_model"]["speaker"]["speaker_model"]["speaker_encoder"]["pretrained"] = None
    tse = get_model(cfg["model"]["tse_model"])(cfg["model_args"]["tse_model"])
    missing, unexpected = tse.load_state_dict(load_file(str(w / "model.safetensors")), strict=False)
    assert not unexpected, unexpected[:5]
    # spk_ft.encoder is a second speaker encoder the frontend builds and the
    # checkpoint leaves untrained: the context feature has its own, which is
    # the one the graphs use.
    assert not [m for m in missing if "num_batches_tracked" not in m and not m.startswith("spk_ft.encoder.")], missing[:5]
    tse.eval()
    tse.spk_ft.context.fbank.dither = 0.0
    tse.requires_grad_(False)
    return tse


def safe_norm(x, dim):
    """||x|| along dim without squaring anything large."""
    m = x.abs().amax(dim=dim, keepdim=True) + 1e-12
    return m * torch.sqrt(((x / m) ** 2).sum(dim=dim, keepdim=True))


def safe_mag(re, im):
    a, b = re.abs(), im.abs()
    m = torch.maximum(a, b)
    r = torch.minimum(a, b) / (m + 6.2e-5)
    return m * torch.sqrt(1.0 + r * r)


class LSTM(nn.Module):
    """nn.LSTM, one layer, batch first, unrolled over a fixed length."""

    def __init__(self, rnn: nn.LSTM, steps: int):
        super().__init__()
        self.steps = steps
        self.H = rnn.hidden_size
        self.bi = rnn.bidirectional
        dirs = ["", "_reverse"] if self.bi else [""]
        self.w_ih = nn.ParameterList([nn.Parameter(getattr(rnn, f"weight_ih_l0{d}").detach().clone(), requires_grad=False) for d in dirs])
        self.w_hh = nn.ParameterList([nn.Parameter(getattr(rnn, f"weight_hh_l0{d}").detach().clone(), requires_grad=False) for d in dirs])
        self.b = nn.ParameterList([nn.Parameter((getattr(rnn, f"bias_ih_l0{d}") + getattr(rnn, f"bias_hh_l0{d}")).detach().clone(),
                                                requires_grad=False) for d in dirs])

    def run(self, x, d, h, c, reverse):
        H = self.H
        xi = torch.matmul(x, self.w_ih[d].t()) + self.b[d]          # [B, T, 4H], all steps at once
        outs = [None] * self.steps
        order = range(self.steps - 1, -1, -1) if reverse else range(self.steps)
        for t in order:
            g = xi[:, t] + torch.matmul(h, self.w_hh[d].t())
            i, f, gg, o = g[:, :H], g[:, H:2 * H], g[:, 2 * H:3 * H], g[:, 3 * H:]
            c = torch.sigmoid(f) * c + torch.sigmoid(i) * torch.tanh(gg)
            h = torch.sigmoid(o) * torch.tanh(c)
            outs[t] = h
        return torch.stack(outs, dim=1), h, c

    def forward(self, x, state=None):
        B = x.shape[0]
        if state is None:
            z = torch.zeros(B, self.H)
            h0, c0 = [z] * len(self.w_ih), [z] * len(self.w_ih)
        else:
            h0, c0 = [state[0][0]], [state[1][0]]
        y, hN, cN = self.run(x, 0, h0[0], c0[0], False)
        if not self.bi:
            return y, (hN[None], cN[None])
        yr, hr, cr = self.run(x, 1, h0[1], c0[1], True)
        return torch.cat([y, yr], dim=-1), (torch.stack([hN, hr]), torch.stack([cN, cr]))


class Split(vx.Chunk):
    """The author's Chunk with the replacements above, as its two halves."""

    def __init__(self, tse):
        super().__init__(tse)
        cr, ci = vx.dft_basis(NFFT, NFFT, torch.hann_window(NFFT))     # [257, 512] each
        self.register_buffer("basis", torch.cat([cr, ci]).t().contiguous())  # [512, 514]
        nb = tse.sep_model.nband
        for seq in tse.sep_model.subband_norm.BN:
            seq[0].eps = seq[0].eps * SCALE * SCALE
        for blk in tse.sep_model.separator.separation:
            blk.band_rnn.rnn = LSTM(blk.band_rnn.rnn, K)
            blk.band_comm.rnn = LSTM(blk.band_comm.rnn, nb)
        tf = tse.spk_ft.tfmap

        def tfmap(enroll, mix):
            mix_n = mix / safe_norm(mix, 1).clamp(min=1e-12)
            enr_n = enroll / safe_norm(enroll, 1).clamp(min=1e-12)
            w = F.softmax(torch.matmul(mix_n.transpose(1, 2), enr_n), dim=-1)
            m = torch.matmul(w, enr_n.transpose(1, 2)).transpose(1, 2)
            m = m / safe_norm(m, 1)
            # sum(mix * m) runs past fp16 on a loud frame; mix is scaled first.
            s = mix.abs().amax(dim=1, keepdim=True) + 1e-12
            return (torch.sum((mix / s) * m, dim=1, keepdim=True) * s) * m
        tf.compute = tfmap

    @staticmethod
    def group_norm(gn, x):
        """GroupNorm with one group as a layer norm over (channels, bands): the
        same function, and the op the Neural Engine runs natively. Spelled as
        GroupNorm, CoreML reduces 4096 values per frame in fp16."""
        y = F.layer_norm(x, x.shape[1:], eps=gn.eps)
        return y * gn.weight[:, None] + gn.bias[:, None]

    def stft(self, wav):
        blocks = wav.reshape(K + 3, HOP)
        frames = torch.cat([blocks[0:K], blocks[1:K + 1], blocks[2:K + 2], blocks[3:K + 3]], dim=1)  # [K, 512]
        x = torch.matmul(frames, self.basis)                            # [K, 514]
        return x[:, :NF].t()[None], x[:, NF:].t()[None]                  # [1, 257, K] each

    def front(self, re, im, enroll_mag, context):
        """Spectrum to the separator's input features, [1, 32, 128, K]."""
        t, sep = self.t, self.t.sep_model
        mag = safe_mag(re, im)
        spec = torch.stack([re, im], 1)
        tfmap = t.spk_ft.tfmap.compute(enroll_mag, mag)
        spec = torch.cat([spec, tfmap[:, None]], 1)
        sub, i0 = [], 0
        for bw in self.bw:
            sub.append(spec[:, :, i0: i0 + bw]); i0 += bw
        feat = torch.stack([bn(sub[i].reshape(1, self.bw[i] * 3, K)) for i, bn in enumerate(sep.subband_norm.BN)], 1)
        return t.spk_ft.context.post(feat, context)

    def back(self, feat, re, im, h, c):
        """The separator and the masks: features to output frames and state."""
        sep = self.t.sep_model
        sre, sim, i0 = [], [], 0
        for bw in self.bw:
            sre.append(re[:, i0: i0 + bw]); sim.append(im[:, i0: i0 + bw]); i0 += bw
        nb, fd = self.nband, feat.shape[2]
        x = feat.reshape(1, nb * fd, K)
        hs, cs = [], []
        for j, blk in enumerate(sep.separator.separation):
            r = blk.band_rnn
            inp = x.reshape(nb, fd, K)
            o, (hj, cj) = r.rnn(r.norm(inp).transpose(1, 2), (h[j: j + 1], c[j: j + 1]))
            hs.append(hj); cs.append(cj)
            o = r.proj(o)
            band = (inp + o.transpose(1, 2)).reshape(1, nb, fd, K)
            band = band.permute(0, 3, 2, 1).reshape(K, fd, nb)
            bc = blk.band_comm
            ro, _ = bc.rnn(self.group_norm(bc.norm, band).transpose(1, 2))
            band = band + bc.proj(ro).transpose(1, 2)
            x = band.reshape(1, K, fd, nb).permute(0, 3, 2, 1).reshape(1, nb * fd, K)
        out = x.reshape(1, nb, fd, K)
        er, ei = [], []
        for i, mf in enumerate(sep.band_masker.mask):
            # (gate or value, real or imaginary, bin, frame): the original's
            # view minus its batch and speaker axes, which CoreML's rank
            # limit of 5 does not leave room for.
            o = mf(out[:, i]).reshape(2, 2, self.bw[i], K)
            m = o[0] * torch.sigmoid(o[1])
            mr, mi = m[0:1], m[1:2]
            er.append(sre[i] * mr - sim[i] * mi); ei.append(sre[i] * mi + sim[i] * mr)
        frames = self.istft(torch.cat(er, 1), torch.cat(ei, 1)) * (1.0 / SCALE)
        return frames, torch.cat(hs), torch.cat(cs)


class Front(Split):
    def forward(self, wav, enroll_mag, context):
        re, im = self.stft(wav * SCALE)
        return self.front(re, im, enroll_mag, context), re, im


class Back(Split):
    def forward(self, feat, re, im, h, c):
        return self.back(feat, re, im, h, c)


def main():
    build = common.BUILD / "voxlock"
    en = vx.Enroll(load_tse()).eval()
    with torch.no_grad():
        mag, ctx = en(torch.randn(1, ENROLL) * 0.1)
        tr = torch.jit.trace(en, (torch.zeros(1, ENROLL),))
    enroll = common.save(common.convert(tr, [("enroll", torch.zeros(1, ENROLL))], ["enroll_mag", "context"], precision="fp32"),
                         build / "enroll.mlpackage")
    parts = {}
    # Each half from its own copy of the model: Split rewrites it in place.
    for name, mod, ex, ins, outs in [
        ("front", Front(load_tse()), (torch.zeros(1, (K + 3) * HOP), torch.zeros(*mag.shape), torch.zeros(*ctx.shape)),
         ["wav", "enroll_mag", "context"], ["feat", "re", "im"]),
        ("back", Back(load_tse()), (torch.zeros(1, 32, 128, K), torch.zeros(1, NF, K), torch.zeros(1, NF, K),
                                    torch.zeros(6, 32, 256), torch.zeros(6, 32, 256)),
         ["feat", "re", "im", "h", "c"], ["frames", "h_out", "c_out"]),
    ]:
        with torch.no_grad():
            tr = torch.jit.trace(mod.eval(), ex)
        parts[name] = common.save(common.convert(tr, list(zip(ins, ex)), outs), build / f"{name}.mlpackage")

    pkg = common.package_dir(MODEL_ID)
    common.compile_into(enroll, pkg / "enroll.mlmodelc")
    common.compile_into(parts["front"], pkg / "front.mlmodelc")
    common.compile_into(parts["back"], pkg / "back.mlmodelc")
    shutil.copy(REPO / "LICENSE", pkg / "LICENSE")
    common.notice(pkg, (REPO / "NOTICE").read_text().strip() + """

The models here are converted from Voxlock's fp32 weights
(https://huggingface.co/arbazsiddiqui/voxlock, model.safetensors at revision
a50f4e1d109301d4b6f61dec57ebb1a61e7d8f03) with the code of
https://github.com/arbazsiddiqui/voxlock at revision
08ed37cbb79f449ca6b499bc23fb14138b207bfd. Apache License 2.0, a copy of
which is in LICENSE.""")
    print("packaged", pkg)


if __name__ == "__main__":
    main()
