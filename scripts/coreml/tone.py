"""T-one (t-tech/T-one) for CoreML, one 300 ms chunk per call, with the
authors' own export wrapper: t-one-ctc-ru-coreml.

  model.mlmodelc  functions len0, len10, len20 and len30, one per value the
                  attention cache length takes
    inputs   signal [1, 2400, 1]  8 kHz samples in [-1, 1]
             s_pre [1, 80], s_mhsa [1, 2, 30, 384], s_conv [1, 16, 384, 30],
             s_sub1 [1, 1, 10, 64], s_sub2 [1, 32, 8, 44], s_red [1, 384, 1]
    outputs  logprobs [1, 10, 35] and the six states, suffixed _out
  tokens.txt      sherpa-onnx's token list for this model

The ONNX export carries its state as one fused vector of 219,729 values with
the cache length among them. Here:

  * the state is its six tensors: a single axis that long is past what the
    Neural Engine takes, and the whole graph fell back to the CPU over it;
  * the cache length only ever takes four values — 0, 10, 20, then 30 for
    good — so it is baked into one function per value instead of computed:
    the masks it drives become constants and the graph keeps no integer
    arithmetic. The caller picks the function by how many chunks the stream
    has decoded since it started;
  * the filterbank, the RMS norms and the convolutions wider than the Neural
    Engine takes are rewritten in operations it has, in numbers fp16 holds.

The Neural Engine still plans this graph out, although it supports every op
in it; the Go side runs it on the GPU.

usage: tone.py
"""
import json
import math
import shutil
import sys
import types

import torch

import common

REPO = common.SRC / "T-one"
common.add_path(REPO)
# The package's __init__ pulls in its beam-search decoder (pyctcdecode,
# kenlm), which the export path never touches: register the package without
# running it, and stub the one name export.py imports from cloudpathlib.
_pkg = types.ModuleType("tone")
_pkg.__path__ = [str(REPO / "tone")]
sys.modules.setdefault("tone", _pkg)
_cp = types.ModuleType("cloudpathlib")
_cp.AnyPath = object
sys.modules.setdefault("cloudpathlib", _cp)

from tone.nn.modules.submodules import RMSNorm  # noqa: E402
from tone.scripts.export import ModelToExport  # noqa: E402
from tone.training import model_wrapper  # noqa: E402

MODEL_ID = "t-one-ctc-ru-coreml"
CACHE_LENS = (0, 10, 20, 30)
STATE_NAMES = ["s_pre", "s_mhsa", "s_conv", "s_sub1", "s_sub2", "s_red"]


def _load(path):
    """ToneForCTC from a checkpoint directory, built on the CPU and filled
    from its safetensors: transformers 5's from_pretrained initialises on the
    meta device, which this model's buffers do not survive."""
    from safetensors.torch import load_file

    cfg = model_wrapper.ToneConfig(**json.load(open(f"{path}/config.json")))
    m = model_wrapper.ToneForCTC(cfg)
    missing, unexpected = m.load_state_dict(load_file(f"{path}/model.safetensors"), strict=False)
    assert not unexpected and not missing, (missing[:5], unexpected[:5])
    return m.eval()


model_wrapper.ToneForCTC.from_pretrained = staticmethod(_load)


def _fp16_safe_forward(self, waveform):
    """FilterbankFeatures._forward in operations fp16 holds: framing by
    reshape rather than a 160-wide strided convolution, which the Neural
    Engine lacks; log-power as 2 log m + log(1 + r^2) rather than squares;
    each mel band as a log-sum-exp over its bins; and the 2^-24 guard added in
    the log domain, since fp16 cannot hold it."""
    hop = self.hop_length
    B, T = waveform.shape
    blocks = waveform.reshape(B, T // hop, hop)
    frames = torch.cat([blocks[:, :-1], blocks[:, 1:]], dim=2)                 # [B, F, 160]
    basis = self.forward_basis[:, 0, :].t()                                     # [160, 162]
    x = torch.matmul(frames, basis)
    nb = basis.shape[1] // 2
    a, b = x[..., :nb].abs(), x[..., nb:].abs()
    m = torch.clamp(torch.maximum(a, b), min=6.2e-5)
    r = torch.minimum(a, b) / m
    logp = 2.0 * torch.log(m) + torch.log(1.0 + r * r)                         # [B, F, 81]
    g = torch.matmul(logp, self._sel).reshape(B, logp.shape[1], self._nmel, self._k) + self._logw
    mx = g.amax(dim=-1, keepdim=True)
    lse = mx[..., 0] + torch.log(torch.exp(g - mx).sum(dim=-1))                # [B, F, 64]
    guard = math.log(self.LOG_ZERO_GUARD_VALUE)
    hi = torch.clamp(lse, min=guard)
    lo = torch.clamp(lse, max=guard)
    out = hi + torch.log(1.0 + torch.exp(lo - hi))
    return out.transpose(1, 2).to(waveform.dtype)


def make_fp16_safe(fe):
    fb = fe.filterbanks.t()                                                     # [81, 64]
    nz = [torch.nonzero(fb[:, j] > 0)[:, 0] for j in range(fb.shape[1])]
    K = max(len(z) for z in nz)
    sel = torch.zeros(fb.shape[0], fb.shape[1] * K)
    logw = torch.full((fb.shape[1], K), -30000.0)
    for j, z in enumerate(nz):
        for s, b in enumerate(z.tolist()):
            sel[b, j * K + s] = 1.0
            logw[j, s] = math.log(fb[b, j].item())
    fe.register_buffer("_sel", sel, persistent=False)
    fe.register_buffer("_logw", logw, persistent=False)
    fe._nmel, fe._k = fb.shape[1], K
    fe._forward = types.MethodType(_fp16_safe_forward, fe)


def _rmsnorm_forward(self, x):
    """RMSNorm without reduce_l2_norm, which the Neural Engine lacks, and
    without squaring activations fp16 cannot hold squared: the norm of x is
    max|x| times the norm of x / max|x|."""
    m = x.abs().amax(dim=-1, keepdim=True) + 1e-12
    norm = m * torch.sqrt(((x / m) ** 2).sum(dim=-1, keepdim=True))
    return self.weight * (x / (norm * self.d ** -0.5 + self.eps))


class SplitConv2d(torch.nn.Module):
    """A Conv2d whose kernel is wider than the Neural Engine takes, as two
    convolutions over the two halves of the kernel, the second on the input
    shifted by the first half's width. Stride 1 and no padding on that axis."""

    def __init__(self, conv, split):
        super().__init__()
        assert conv.stride[1] == 1 and conv.padding == (0, 0)
        w = conv.weight.detach()
        self.split, self.kw = split, w.shape[3]
        self.a = torch.nn.Conv2d(conv.in_channels, conv.out_channels, (w.shape[2], split), stride=conv.stride, bias=True)
        self.b = torch.nn.Conv2d(conv.in_channels, conv.out_channels, (w.shape[2], self.kw - split), stride=conv.stride, bias=False)
        self.a.weight.data, self.b.weight.data = w[..., :split].clone(), w[..., split:].clone()
        self.a.bias.data = conv.bias.detach().clone()
        self.requires_grad_(False)

    def forward(self, x):
        n = x.shape[3] - self.kw + 1
        return self.a(x)[..., :n] + self.b(x[..., self.split:])[..., :n]


class SplitConv1d(torch.nn.Module):
    """A Conv1d with a kernel longer than `most`, as a sum of convolutions over
    consecutive pieces of the kernel, each on the input shifted by its offset.
    Stride 1 and no padding, which is how T-one's causal convolutions call it
    (they pad the state on the left themselves)."""

    def __init__(self, conv, most):
        super().__init__()
        assert conv.stride == (1,) and conv.padding == (0,) and conv.dilation == (1,)
        w = conv.weight.detach()
        self.k = w.shape[2]
        self.parts = torch.nn.ModuleList()
        self.offsets = []
        for off in range(0, self.k, most):
            width = min(most, self.k - off)
            c = torch.nn.Conv1d(conv.in_channels, conv.out_channels, width, groups=conv.groups,
                                bias=off == 0 and conv.bias is not None)
            c.weight.data = w[..., off:off + width].clone()
            if c.bias is not None:
                c.bias.data = conv.bias.detach().clone()
            self.parts.append(c)
            self.offsets.append(off)
        self.requires_grad_(False)

    def forward(self, x):
        n = x.shape[2] - self.k + 1
        out = None
        for off, c in zip(self.offsets, self.parts):
            y = c(x[..., off:])[..., :n]
            out = y if out is None else out + y
        return out


def make_ane_friendly(model):
    for mod in model.modules():
        if isinstance(mod, RMSNorm):
            mod.forward = types.MethodType(_rmsnorm_forward, mod)
    for mod in model.modules():
        for name, child in list(mod.named_children()):
            if isinstance(child, torch.nn.Conv2d) and child.kernel_size[1] > 16:
                setattr(mod, name, SplitConv2d(child, 11))
            elif isinstance(child, torch.nn.Conv1d) and child.kernel_size[0] > 11:
                setattr(mod, name, SplitConv1d(child, 11))


class Tone(ModelToExport):
    """The authors' ModelToExport at one cache length, the state as six
    tensors, the signal as float in [-1, 1] and no autocast: the conversion
    picks the precision."""

    def __init__(self, path, cache_len):
        super().__init__(path, 300)
        self.cache_len = cache_len
        make_fp16_safe(self._model.preprocessor)
        make_ane_friendly(self._model)

    def state_shapes(self, B):
        # The first of the fused state's parts is the cache length.
        return [(B, *shape) for shape in self._state_shape[1:]]

    def forward(self, signal, s_pre, s_mhsa, s_conv, s_sub1, s_sub2, s_red):
        B = signal.shape[0]
        mhsa_len = torch.full((B, 1), self.cache_len, dtype=torch.int32)
        # forward_for_export takes int16-scale samples and divides them by
        # 32767 itself.
        res, pre, mhsa, conv, _, sub1, sub2, red = self._model.forward_for_export(
            signal * 32767.0, s_pre, s_mhsa, s_conv, mhsa_len, s_sub1, s_sub2, s_red)
        return res, pre, mhsa, conv, sub1, sub2, red


def main():
    weights = common.WEIGHTS / "tone"
    build = common.BUILD / "tone"
    funcs = []
    for L in CACHE_LENS:
        m = Tone(str(weights), L).eval()
        ex = (torch.zeros(1, 2400, 1), *[torch.zeros(*sh) for sh in m.state_shapes(1)])
        with torch.no_grad():
            # The model keeps per-call scratch on its layers, which the trace
            # check mistakes for nondeterminism; every call refills it from
            # the inputs.
            tr = torch.jit.trace(m, ex, check_trace=False)
        ml = common.convert(tr, list(zip(["signal"] + STATE_NAMES, ex)), ["logprobs"] + [s + "_out" for s in STATE_NAMES])
        funcs.append((f"len{L}", common.save(ml, build / f"len{L}.mlpackage")))
    common.merge(funcs, build / "model.mlpackage", default="len30")

    pkg = common.package_dir(MODEL_ID)
    common.compile_into(build / "model.mlpackage", pkg / "model.mlmodelc")
    # sherpa-onnx's tokens.txt for this model: the vocabulary in its own
    # order, with the CTC blank and the word separator spelled its way.
    vocab = json.load(open(weights / "vocab.json"))
    spell = {"[PAD]": "<blk>", "|": " "}
    (pkg / "tokens.txt").write_text("".join(f"{spell.get(k, k)} {v}\n" for k, v in vocab.items()))
    shutil.copy(REPO / "LICENSE", pkg / "LICENSE")
    common.notice(pkg, """
T-one, by T-Software DC: https://huggingface.co/t-tech/T-one, weights at
revision 106f3b0b32a9e107eb613312e4ebc61ff3d53926, exported with the code of
https://github.com/voicekit-team/T-one at revision
3c5b6c015038173840e62cea99e10cdb1c759116. Apache License 2.0 (LICENSE).""")
    print("packaged", pkg)


if __name__ == "__main__":
    main()
