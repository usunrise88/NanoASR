"""The VAD NanoASR ships — Silero v4, k2-fsa's 16 kHz export (silero_vad.onnx,
the catalog's silero-vad-v5) — rebuilt from that file's weights for the Neural
Engine, N windows per call: silero-vad-v5-coreml.

  inputs   frames [N, 8, 256]  each 512-sample window reflect-padded by 96 on
                               both sides and cut into its 8 STFT frames (hop
                               64), audio scaled by SCALE; laid out by the host
           h, c   [2, 64]      the two LSTM layers' state
  outputs  prob   [N]
           h_out, c_out [2, 64]

fp16 cannot hold |X|^2 of quiet audio nor 2^20 |X| of loud audio, which is what
the graph computes; both are rewritten so every intermediate stays in range:

  |X|                = m sqrt(1 + (n/m)^2),   m, n = max, min(|re|, |im|),
  log(1 + 2^20 |X|)  = 12 ln 2 + log(|X_s| + 2^-12),   X_s = STFT(SCALE x)

with SCALE = 256 (the identity holds to within 2^-20/|X|, far below fp16).

usage: silero.py [--check AUDIO.wav]
  --check compares the converted model's probabilities over the first ten
  minutes of a 16 kHz recording with onnxruntime's.
"""
import argparse
import math
import shutil

import numpy as np
import onnx
import torch
import torch.nn.functional as F
from onnx import numpy_helper
from torch import nn

import common

MODEL_ID = "silero-vad-v5-coreml"
N = 128  # windows per call: 4.1 s
SCALE = 256.0
WIN, PAD, FRAME, HOP, NFR = 512, 96, 256, 64, 8


def P(a):
    return nn.Parameter(torch.from_numpy(np.array(a, dtype=np.float32)), requires_grad=False)


class SileroV4(nn.Module):
    def __init__(self, onnx_path, n):
        super().__init__()
        w = {i.name: numpy_helper.to_array(i) for i in onnx.load(onnx_path).graph.initializer}
        self.n = n
        basis = w["feature_extractor.forward_basis_buffer"][:, 0, :]          # [258, 256]
        self.basis = P(basis.T)                                               # [256, 258]
        self.filt = P(w["adaptive_normalization.filter_"])                    # [1, 1, 7]
        self.w = nn.ParameterDict({k.replace(".", "_").replace(":", "_"): P(v) for k, v in w.items()
                                   if k.startswith(("first_layer", "encoder", "decoder", "onnx::Conv"))})
        # ONNX LSTM gates are i, o, f, c; keep [4H, in] matrices.
        self.lstm = []
        for wi, ri, bi in [("onnx::LSTM_398", "onnx::LSTM_399", "onnx::LSTM_400"),
                           ("onnx::LSTM_418", "onnx::LSTM_419", "onnx::LSTM_420")]:
            b = w[bi][0]
            self.lstm.append((w[wi][0], w[ri][0], b[:256] + b[256:]))
        self.lw = nn.ParameterList([P(x) for t in self.lstm for x in t])

    def c(self, name):
        return self.w[name.replace(".", "_").replace(":", "_")]

    def block(self, x, prefix, proj):
        dw_w, dw_b = self.c(prefix + ".dw_conv.0.weight"), self.c(prefix + ".dw_conv.0.bias")
        y = F.relu(F.conv1d(x, dw_w, dw_b, padding=2, groups=x.shape[1]))
        y = F.conv1d(y, self.c(prefix + ".pw_conv.0.weight"), self.c(prefix + ".pw_conv.0.bias"))
        s = F.conv1d(x, self.c(prefix + ".proj.weight"), self.c(prefix + ".proj.bias")) if proj else x
        return F.relu(y + s)

    def forward(self, frames, h, c):
        N = self.n
        x = frames.reshape(N * NFR, FRAME) @ self.basis                      # [N*8, 258]
        re, im = x[:, :129], x[:, 129:]
        a, b = re.abs(), im.abs()
        m = torch.maximum(a, b)
        # Guarded in the denominator only, so a silent bin is exactly zero.
        r = torch.minimum(a, b) / (m + 6.2e-5)
        mag_s = m * torch.sqrt(1.0 + r * r)                                   # |X| of the scaled audio
        mag = (mag_s * (1.0 / SCALE)).reshape(N, NFR, 129).transpose(1, 2)    # [N, 129, 8]
        spect = (12 * math.log(2) + torch.log(mag_s + 2.0 ** -12)).reshape(N, NFR, 129).transpose(1, 2)
        mean = spect.mean(dim=1, keepdim=True)                                # [N, 1, 8]
        left = torch.flip(mean[:, :, 1:4], dims=[2])
        right = torch.flip(mean[:, :, -4:-1], dims=[2])
        smooth = F.conv1d(torch.cat([left, mean, right], dim=2), self.filt)   # [N, 1, 8]
        norm = spect - smooth.mean(dim=2, keepdim=True)
        x = torch.cat([mag, norm], dim=1)                                     # [N, 258, 8]

        x = self.block(x, "first_layer.0", True)
        x = F.relu(F.conv1d(x, self.c("onnx::Conv_349"), self.c("onnx::Conv_350"), stride=2))
        x = self.block(x, "encoder.3.0", True)
        x = F.relu(F.conv1d(x, self.c("onnx::Conv_352"), self.c("onnx::Conv_353"), stride=2))
        x = self.block(x, "encoder.7.0", False)
        x = F.relu(F.conv1d(x, self.c("onnx::Conv_355"), self.c("onnx::Conv_356"), stride=2))
        x = self.block(x, "encoder.11.0", True)
        x = F.relu(F.conv1d(x, self.c("onnx::Conv_358"), self.c("onnx::Conv_359")))
        x = x[:, :, 0]                                                        # [N, 64]

        # The LSTM written out: CoreML's lstm op has no Neural Engine kernel.
        H = 64
        hs = [h[0:1], h[1:2]]
        cs = [c[0:1], c[1:2]]
        Wi0, Ri0, B0, Wi1, Ri1, B1 = self.lw
        xi = x @ Wi0.t() + B0                                                 # layer 0 input part, batched
        outs = []
        for j in range(N):
            for layer, (Wi, Ri, B) in enumerate([(Wi0, Ri0, B0), (Wi1, Ri1, B1)]):
                g = (xi[j:j + 1] if layer == 0 else hs[0] @ Wi.t() + B) + hs[layer] @ Ri.t()
                i_, o_, f_, c_ = g[:, :H], g[:, H:2 * H], g[:, 2 * H:3 * H], g[:, 3 * H:]
                cs[layer] = torch.sigmoid(f_) * cs[layer] + torch.sigmoid(i_) * torch.tanh(c_)
                hs[layer] = torch.sigmoid(o_) * torch.tanh(cs[layer])
            outs.append(hs[1])
        y = torch.cat(outs, dim=0)                                            # [N, 64]
        prob = torch.sigmoid(F.relu(y) @ self.c("decoder.decoder.1.weight")[:, :, 0].t()
                             + self.c("decoder.decoder.1.bias"))[:, 0]
        return prob, torch.cat(hs, dim=0), torch.cat(cs, dim=0)


def frames_for(x):
    """The windows sherpa-onnx runs, laid out as model input (unscaled)."""
    n = len(x) // WIN
    out = np.zeros((n, NFR, FRAME), dtype=np.float32)
    for k in range(n):
        w = np.pad(x[k * WIN:(k + 1) * WIN], PAD, mode="reflect")
        for f in range(NFR):
            out[k, f] = w[f * HOP:f * HOP + FRAME]
    return out


def check(onnx_path, package, wav):
    import coremltools as ct
    import onnxruntime as ort
    import soundfile as sf

    x, sr = sf.read(wav, dtype="float32")
    assert sr == 16000 and x.ndim == 1, "a 16 kHz mono recording"
    x = x[: 16000 * 600]
    sess = ort.InferenceSession(onnx_path)
    h = np.zeros((2, 1, 64), np.float32)
    c = np.zeros((2, 1, 64), np.float32)
    ref = []
    for k in range(len(x) // WIN):
        p, h, c = sess.run(None, {"x": x[k * WIN:(k + 1) * WIN][None], "h": h, "c": c})
        ref.append(p.item())
    ref = np.array(ref)
    fr = frames_for(x)
    mm = ct.models.MLModel(str(package), compute_units=ct.ComputeUnit.CPU_AND_NE)
    hh, cc = np.zeros((2, 64), np.float16), np.zeros((2, 64), np.float16)
    got = []
    for k in range(0, len(fr) - N + 1, N):
        o = mm.predict({"frames": (fr[k:k + N] * SCALE).astype(np.float16), "h": hh, "c": cc})
        got.extend(o["prob"].astype(np.float32).tolist())
        hh, cc = o["h_out"], o["c_out"]
    got = np.array(got)
    d = np.abs(got - ref[:len(got)])
    print(f"CoreML vs onnxruntime over {len(got)} windows: mean {d.mean():.4f} max {d.max():.3f}, "
          f"decision flips {np.mean((got > 0.5) != (ref[:len(got)] > 0.5)):.3%}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", metavar="AUDIO.wav")
    args = ap.parse_args()
    onnx_path = str(common.WEIGHTS / "silero" / "silero_vad.onnx")
    m = SileroV4(onnx_path, N).eval()
    ex = (torch.zeros(N, NFR, FRAME), torch.zeros(2, 64), torch.zeros(2, 64))
    with torch.no_grad():
        traced = torch.jit.trace(m, ex)
    ml = common.convert(traced, list(zip(["frames", "h", "c"], ex)), ["prob", "h_out", "c_out"])
    package = common.save(ml, common.BUILD / "silero" / "silero.mlpackage")
    if args.check:
        check(onnx_path, package, args.check)

    pkg = common.package_dir(MODEL_ID)
    common.compile_into(package, pkg / "silero.mlmodelc")
    shutil.copy(common.WEIGHTS / "silero" / "LICENSE", pkg / "LICENSE")
    common.notice(pkg, """
Silero VAD v4 (https://github.com/snakers4/silero-vad), by the Silero Team,
rebuilt from the weights of k2-fsa's export of it (silero_vad.onnx from the
sherpa-onnx asr-models release). MIT License, a copy of which is in LICENSE.""")
    print("packaged", pkg)


if __name__ == "__main__":
    main()
