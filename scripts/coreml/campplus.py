"""CAM++ (3D-Speaker zh-en common advanced, the checkpoint campplus-sv-zh-en
was exported from) for CoreML: campplus-sv-zh-en-coreml.

  fbank.mlmodelc  sherpa-onnx's Kaldi fbank for this model, frame by frame, on
                  the GPU in fp32: the Neural Engine's fp16 cannot hold the
                  dynamic range of a power spectrum, and the embeddings move
                  by up to 3% when it tries.
                    frames [MAX, 400] -> fbank [MAX, 80]
  net.mlmodelc    the network on the Neural Engine, one function per length
                  t<T>, with the true length given as a mask.
                    fbank [T, 80], mask [T] -> embedding [192]

CAM++ pools over the whole utterance at every layer, so padding is not
harmless as it is in a convolution stack. Here every pooling — the context
mean and 100-frame segment means of each CAM layer, the final statistics — is
taken over the valid frames only, and every convolution that reaches across
time is fed zeros past the end, which is what its own zero padding shows an
unpadded input. The valid frames' outputs are then the unpadded ones. T is a
multiple of 200, so the second stage's 100-frame segments tile it.

Frames are Kaldi's with snip_edges false, samples in [-1, 1]: the Go side cuts
them (internal/tse/coreml_embedder.go).

usage: campplus.py [--onnx 3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx]
  --onnx compares the converted pair, in torch, with sherpa-onnx's model.
"""
import argparse
import math
import shutil

import numpy as np
import torch
import torch.nn.functional as F
import torchaudio
from torch import nn

import common

common.add_path(common.SRC / "3dspeaker")
from speakerlab.models.campplus.DTDNN import CAMPPlus  # noqa: E402

MODEL_ID = "campplus-sv-zh-en-coreml"
BUCKETS = [200, 400, 600, 1000, 2000]  # frames: 2 to 20 s
FRAME, NFFT, NBIN = 400, 512, 257


def bn(m, x):
    return F.batch_norm(x, m.running_mean, m.running_var, m.weight, m.bias, False, 0.0, m.eps)


class Net(nn.Module):
    """The network over T frames of features, mask marking the valid ones."""

    def __init__(self, ckpt, T):
        super().__init__()
        assert T % 200 == 0
        self.T = T
        net = CAMPPlus(feat_dim=80, embedding_size=192).eval()
        net.load_state_dict(torch.load(ckpt, map_location="cpu"))
        net.requires_grad_(False)
        self.net = net

    def conv2d(self, conv, x, m):
        return conv(x * m)

    def resblock(self, blk, x, m):
        out = F.relu(bn(blk.bn1, self.conv2d(blk.conv1, x, m)))
        out = bn(blk.bn2, self.conv2d(blk.conv2, out, m))
        sc = x
        if len(blk.shortcut) > 0:
            sc = bn(blk.shortcut[1], blk.shortcut[0](x))
        return F.relu(out + sc)

    def nonlinear(self, seq, x):
        for mod in seq:
            x = bn(mod, x) if isinstance(mod, nn.BatchNorm1d) else F.relu(x)
        return x

    def cam(self, cl, x, m, count):
        y = cl.linear_local(x * m)
        Tn = x.shape[-1]
        mean = (x * m).sum(-1, keepdim=True) / count
        seg = (x * m).reshape(1, x.shape[1], Tn // 100, 100).sum(-1)
        segc = m.reshape(1, 1, Tn // 100, 100).sum(-1).clamp(min=1.0)
        seg = (seg / segc).unsqueeze(-1).expand(1, x.shape[1], Tn // 100, 100).reshape(1, x.shape[1], Tn)
        ctx = F.relu(cl.linear1(mean + seg))
        return y * torch.sigmoid(cl.linear2(ctx))

    def forward(self, fb, mask):
        T = self.T
        count = mask.sum()
        fb = (fb - (fb * mask[:, None]).sum(0, keepdim=True) / count) * mask[:, None]
        h = self.net.head
        mt = mask.view(1, 1, 1, T)
        x = fb.t().reshape(1, 1, 80, T)
        x = F.relu(bn(h.bn1, self.conv2d(h.conv1, x, mt)))
        for layer in list(h.layer1) + list(h.layer2):
            x = self.resblock(layer, x, mt)
        x = F.relu(bn(h.bn2, self.conv2d(h.conv2, x, mt)))
        x = x.reshape(1, h.out_channels, T)

        xv = self.net.xvector
        m1 = mask.view(1, 1, T)
        x = self.nonlinear(xv.tdnn.nonlinear, xv.tdnn.linear(x * m1))   # stride 2
        m2 = mask[::2].view(1, 1, T // 2)
        c2 = m2.sum()
        for name in ["block1", "transit1", "block2", "transit2", "block3", "transit3"]:
            mod = getattr(xv, name)
            if name.startswith("block"):
                for layer in mod:
                    y = layer.linear1(self.nonlinear(layer.nonlinear1, x))
                    y = self.cam(layer.cam_layer, self.nonlinear(layer.nonlinear2, y), m2, c2)
                    x = torch.cat([x, y], dim=1)
            else:
                x = mod.linear(self.nonlinear(mod.nonlinear, x))
        x = self.nonlinear(xv.out_nonlinear, x)
        mean = (x * m2).sum(-1) / c2
        var = (((x - mean[..., None]) * m2) ** 2).sum(-1) / (c2 - 1)
        stats = torch.cat([mean, torch.sqrt(var)], dim=-1)               # [1, 2C]
        d = xv.dense
        e = d.linear(stats.unsqueeze(-1)).squeeze(-1)
        return self.nonlinear(d.nonlinear, e)[0]


class Fbank(nn.Module):
    """torchaudio's kaldi.fbank with sherpa-onnx's settings for this model
    (no dither, Povey window, 20 Hz to Nyquist - 400 Hz, float epsilon floor),
    over frames already cut."""

    def __init__(self):
        super().__init__()
        n = torch.arange(FRAME, dtype=torch.float64)
        win = (0.5 - 0.5 * torch.cos(2 * math.pi * n / (FRAME - 1))) ** 0.85
        k = torch.arange(NBIN, dtype=torch.float64)
        ang = 2 * math.pi * n[:, None] * k[None, :] / NFFT
        self.register_buffer("cos", (torch.cos(ang) * win[:, None]).float())
        self.register_buffer("sin", (-torch.sin(ang) * win[:, None]).float())
        mel, _ = torchaudio.compliance.kaldi.get_mel_banks(80, NFFT, 16000.0, 20.0, -400.0, 100.0, -500.0, 1.0)
        self.register_buffer("mel", F.pad(mel, (0, 1)).t().contiguous())
        self.eps = float(torch.finfo(torch.float32).eps)

    def forward(self, frames):
        x = frames - frames.mean(dim=1, keepdim=True)
        x = torch.cat([x[:, :1] * (1 - 0.97), x[:, 1:] - 0.97 * x[:, :-1]], dim=1)
        re, im = x @ self.cos, x @ self.sin
        return torch.log(torch.clamp((re * re + im * im) @ self.mel, min=self.eps))


def frames_for(x):
    """Kaldi's frames with snip_edges false: centred on every 10 ms, the edges
    reflected."""
    n = len(x)
    T = (n + 80) // 160
    idx = np.arange(T)[:, None] * 160 - 120 + np.arange(FRAME)[None, :]
    while (idx < 0).any() or (idx >= n).any():
        idx = np.where(idx < 0, -idx - 1, idx)
        idx = np.where(idx >= n, 2 * n - 1 - idx, idx)
    return x[idx].astype(np.float32)


def kaldi(x):
    return torchaudio.compliance.kaldi.fbank(torch.from_numpy(x)[None], num_mel_bins=80, snip_edges=False,
                                             dither=0.0, energy_floor=0.0, high_freq=-400, low_freq=20)


def check_onnx(ckpt, onnx_path, fb):
    """Masked network over the padded features vs sherpa-onnx's model over the
    exact ones, by cosine."""
    import onnxruntime as ort

    sess = ort.InferenceSession(onnx_path)
    torch.manual_seed(0)
    for T in BUCKETS:
        net = Net(ckpt, T).eval()
        worst = 1.0
        for sec in (0.6, T / 200 * 0.97, T / 100 * 0.45, T / 100 - 0.01):
            x = (torch.randn(int(16000 * sec)) * 0.1).numpy()
            f = frames_for(x)
            if len(f) > T:
                continue
            feats = np.zeros((T, 80), np.float32)
            with torch.no_grad():
                feats[:len(f)] = fb(torch.from_numpy(f)).numpy()
            mk = np.zeros(T, np.float32)
            mk[:len(f)] = 1
            with torch.no_grad():
                got = net(torch.from_numpy(feats), torch.from_numpy(mk)).numpy()
            ref_fb = kaldi(x)
            ref = sess.run(None, {"x": (ref_fb - ref_fb.mean(0, keepdim=True))[None].numpy()})[0][0]
            worst = min(worst, float(got @ ref / np.linalg.norm(got) / np.linalg.norm(ref)))
        print(f"t{T}: masked network vs sherpa-onnx's model, worst cosine {worst:.6f}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--onnx", metavar="MODEL.onnx")
    args = ap.parse_args()
    ckpt = str(common.WEIGHTS / "campplus" / "campplus_cn_en_common.pt")
    build = common.BUILD / "campplus"

    fb = Fbank().eval()
    x = (torch.randn(16000 * 3) * 0.1).numpy()
    with torch.no_grad():
        d = float((fb(torch.from_numpy(frames_for(x))) - kaldi(x)).abs().max())
    print(f"fbank vs torchaudio kaldi.fbank: max abs diff {d:.2e}")
    assert d < 1e-3, d
    if args.onnx:
        check_onnx(ckpt, args.onnx, fb)

    most = max(BUCKETS)
    ex = torch.zeros(most, FRAME)
    with torch.no_grad():
        tr = torch.jit.trace(fb, (ex,))
    fbank = common.save(common.convert(tr, [("frames", ex)], ["fbank"], precision="fp32"), build / "fbank.mlpackage")

    funcs = []
    for T in BUCKETS:
        ex = (torch.zeros(T, 80), torch.ones(T))
        with torch.no_grad():
            tr = torch.jit.trace(Net(ckpt, T).eval(), ex)
        funcs.append((f"t{T}", common.save(common.convert(tr, list(zip(["fbank", "mask"], ex)), ["embedding"]),
                                           build / f"net_t{T}.mlpackage")))
    common.merge(funcs, build / "net.mlpackage", default="t400")

    pkg = common.package_dir(MODEL_ID)
    common.compile_into(fbank, pkg / "fbank.mlmodelc")
    common.compile_into(build / "net.mlpackage", pkg / "net.mlmodelc")
    shutil.copy(common.SRC / "3dspeaker" / "LICENSE", pkg / "LICENSE")
    common.notice(pkg, """
CAM++ speaker embedding, zh-en common advanced, from the 3D-Speaker project:
https://www.modelscope.cn/models/iic/speech_campplus_sv_zh_en_16k-common_advanced
(campplus_cn_en_common.pt), with the network definition of
https://github.com/modelscope/3D-Speaker at revision
065629c313eaf1a01c65c640c46d77e61e9607b4. Apache License 2.0, a copy of which
is in LICENSE.""")
    print("packaged", pkg)


if __name__ == "__main__":
    main()
