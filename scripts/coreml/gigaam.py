"""GigaAM v3 with punctuation for the Neural Engine, both heads:
gigaam-v3-rnnt-punct-ru-coreml and gigaam-v3-ctc-punct-ru-coreml.

  encoder.mlmodelc  one call per window of T feature frames into which the
                    host has packed several speech segments (Packed below),
                    returning the joiner's projection of every encoder frame
                    (RNN-T) or its log-probabilities (CTC)
  step.mlmodelc     RNN-T only: the prediction network and joiner, one decoder
                    step for B lanes over a window of W encoder frames (Step
                    below). CTC needs no second model: the host collapses the
                    best token of each frame.
  rope.bin          float32 cos then sin, [T2, 48] each: the rotary tables a
                    packed segment restarts from
  tokens.txt        sherpa-onnx's token list for the model
  config.json       the layout the Go side packs to (internal/asr/coremlasr)

usage: gigaam.py [--head rnnt|ctc]   (both without --head)
"""
import argparse
import json
import math
import shutil

import numpy as np
import torch
import torch.nn.functional as F
from torch import nn

import common

common.add_path(common.SRC / "GigaAM")
import gigaam  # noqa: E402
import sentencepiece as spm  # noqa: E402

# head -> GigaAM's name for the model, the catalog entry, the checkpoint
HEADS = {
    "rnnt": ("e2e_rnnt", "gigaam-v3-rnnt-punct-ru-coreml", "v3_e2e_rnnt"),
    "ctc": ("e2e_ctc", "gigaam-v3-ctc-punct-ru-coreml", "v3_e2e_ctc"),
}
T = 2048      # feature frames per window: 20.5 s
LANES = 8     # segments the decoder steps at once
WINDOW = 16   # encoder frames each step scores
GAP = 8       # feature frames between packed segments
N_FFT, HOP, N_MELS = 320, 160, 64
SCALE = 256.0
BINS = N_FFT // 2 + 1


class LogMel(nn.Module):
    """GigaAM's log-mel front end in operations that keep their range in fp16.

    torchaudio computes log(clamp(mel(|STFT|^2), 1e-9)). Squaring in fp16
    loses everything below ~6e-5 and overflows above 65504, so the power is
    never formed:

      log|X|^2 = 2 log m + log(1 + (n/m)^2),  m = max(|re|, |im|), n = min(...)

    and the mel sum is taken in the log domain as a logsumexp over each
    filter's bins, gathered by a 0/1 matrix (one non-zero per column, so exact
    in fp16). The audio is scaled by SCALE on the way in so the floor of m
    sits far below the reference's clamp, and the scale is subtracted again in
    the log domain.

    window and fb are the checkpoint's own: GigaAM stores both in fp16, and
    its reference pipeline computes features with those rounded values, whose
    spectral leakage sets the floor of the quiet bins. Exact ones differ from
    it by up to a nat there."""

    def __init__(self, window, fb):
        super().__init__()
        win = window.float()
        n = torch.arange(N_FFT, dtype=torch.float64)
        k = torch.arange(BINS, dtype=torch.float64)
        ang = 2 * math.pi * n[:, None] * k[None, :] / N_FFT
        self.register_buffer("cos", (torch.cos(ang) * win[:, None].double()).float())   # [320, 161]
        self.register_buffer("sin", (-torch.sin(ang) * win[:, None].double()).float())
        fb = fb.float()
        # fb [161, 64]: gather each filter's non-zero bins into K slots.
        nz = [torch.nonzero(fb[:, j] > 0)[:, 0] for j in range(N_MELS)]
        K = max(len(z) for z in nz)
        self.K = K
        sel = torch.zeros(BINS, N_MELS * K)
        logw = torch.full((N_MELS, K), -30000.0)
        for j, z in enumerate(nz):
            for s, b in enumerate(z.tolist()):
                sel[b, j * K + s] = 1.0
                logw[j, s] = math.log(fb[b, j].item())
        self.register_buffer("sel", sel)
        self.register_buffer("logw", logw)
        self.log_floor = math.log(1e-9)
        self.log_scale2 = 2 * math.log(SCALE)

    def forward(self, audio):
        """audio [1, 160 (T + 1)] already scaled by SCALE -> log-mel [1, 64, T]."""
        halves = audio.reshape(-1, HOP)                       # [T+1, 160]
        frames = torch.cat([halves[:-1], halves[1:]], dim=1)  # [T, 320]
        re = frames @ self.cos
        im = frames @ self.sin
        a, b = re.abs(), im.abs()
        m = torch.maximum(a, b)
        n = torch.minimum(a, b)
        m = torch.clamp(m, min=6.2e-5)
        r = n / m
        logp = 2.0 * torch.log(m) + torch.log(1.0 + r * r) - self.log_scale2   # [T, 161]
        g = (logp @ self.sel).reshape(-1, N_MELS, self.K) + self.logw            # [T, 64, K]
        mx = g.amax(dim=-1, keepdim=True)
        lse = mx[..., 0] + torch.log(torch.exp(g - mx).sum(dim=-1))              # [T, 64]
        lse = torch.clamp(lse, min=self.log_floor)
        return lse.transpose(0, 1).unsqueeze(0)                                    # [1, 64, T]


def sub_len(t):
    return (t - 1) // 2 + 1


def frames_of(n_samples):
    return (n_samples - 320) // HOP + 1 if n_samples >= 320 else 0


class Packed(nn.Module):
    """The encoder over a window of T feature frames' worth of audio into which
    the host has packed several speech segments, returning per encoder frame
    the joiner's projection of the encoder output (RNN-T) or the CTC
    log-probabilities. Segments do not see each other:

      * each starts at a feature frame divisible by 4, so its subsampled
        frames land on the grid a standalone run would give it, and is
        followed by at least GAP empty frames, wider than any convolution's
        reach;
      * m0, m1 and m2 zero everything outside the segments at the feature,
        first and second subsampling resolutions, which is what zero padding
        does to a standalone run;
      * attention is block-diagonal (amask);
      * cos and sin restart at every segment. GigaAM rotates its attention
        input before the projections, so positions are absolute rather than
        relative and a packed segment must see the positions it would see
        alone.

    Inputs   audio [1, 160 (T + 1)]  samples x SCALE
             m0 [1, 1, T], m1 [1, 1, T1], m2 [1, 1, T2]
             amask [1, 1, T2, T2]   0 within a segment, -1e4 across
             cos, sin [1, T2, 1, 48]
    Output   out [1, T2, D]  D = joiner width (RNN-T) or vocabulary (CTC)"""

    def __init__(self, model, t_feat, head):
        super().__init__()
        ms = model.preprocessor.featurizer[0]
        self.fe = LogMel(ms.spectrogram.window, ms.mel_scale.fb)
        self.enc = model.encoder
        self.head = head
        if head == "rnnt":
            self.proj = model.head.joint.enc
        else:
            self.ctc = model.head.decoder_layers   # a 1x1 Conv1d to the vocabulary
        a = self.enc.layers[0].self_attn
        self.h, self.d_k = a.h, a.d_k
        self.t2 = sub_len(sub_len(t_feat))

    def forward(self, audio, m0, m1, m2, amask, cos, sin):
        x = self.fe(audio) * m0                               # [1, 64, T]
        convs = [m for m in self.enc.pre_encode.conv if isinstance(m, nn.Conv1d)]
        x = F.relu(convs[0](x)) * m1
        x = F.relu(convs[1](x)) * m2
        x = x.transpose(1, 2)                                 # [1, T2, 768]
        for L in self.enc.layers:
            x = self.layer(L, x, m2, amask, cos, sin)
        if self.head == "rnnt":
            return self.proj(x)
        return torch.log_softmax(self.ctc(x.transpose(1, 2)).transpose(1, 2), dim=-1)

    def layer(self, L, x, keep, amask, cos, sin):
        r = x + 0.5 * L.feed_forward1(L.norm_feed_forward1(x))
        r = r + self.attn(L.self_attn, L.norm_self_att(r), amask, cos, sin)
        y = L.norm_conv(r)
        c = L.conv
        y = F.glu(c.pointwise_conv1(y.transpose(1, 2)), dim=1) * keep
        y = c.depthwise_conv(y)
        y = c.batch_norm(y.transpose(1, 2)).transpose(1, 2)
        y = c.pointwise_conv2(F.silu(y)).transpose(1, 2)
        r = r + y
        r = r + 0.5 * L.feed_forward2(L.norm_feed_forward2(r))
        return L.norm_out(r)

    def attn(self, a, x, amask, cos, sin):
        b, t, h, d = 1, self.t2, self.h, self.d_k
        xh = x.view(b, t, h, d)
        rot = torch.cat([-xh[..., d // 2:], xh[..., : d // 2]], dim=-1)
        xr = (xh * cos + rot * sin).reshape(b, t, h * d)
        q = a.linear_q(xr).view(b, t, h, d).transpose(1, 2)
        k = a.linear_k(xr).view(b, t, h, d).transpose(1, 2)
        v = a.linear_v(x).view(b, t, h, d).transpose(1, 2)
        s = torch.matmul(q, k.transpose(-2, -1)) * (1.0 / math.sqrt(d)) + amask
        o = torch.matmul(torch.softmax(s, dim=-1), v).transpose(1, 2).reshape(b, t, h * d)
        return a.linear_out(o)


def rope_tables(model, n):
    enc = model.encoder
    if not hasattr(enc.pos_enc, "pe"):
        enc.pos_enc.extend_pe(enc.pos_emb_max_len, torch.device("cpu"))
    pe = enc.pos_enc.pe
    half = pe.shape[0] // 2
    return pe[:n, 0, 0].numpy(), pe[half:half + n, 0, 0].numpy()   # [n, 48] each


def pack(segments, t_feat, cos_tab, sin_tab):
    """Lay segments (float32 sample arrays) into one window, as the Go side
    does (pack.go). Returns its inputs and, per segment, (index, first
    encoder frame, encoder frames)."""
    t1, t2 = sub_len(t_feat), sub_len(sub_len(t_feat))
    d = cos_tab.shape[1]
    audio = np.zeros(HOP * (t_feat + 1), dtype=np.float32)
    m0 = np.zeros(t_feat, dtype=np.float32)
    m1 = np.zeros(t1, dtype=np.float32)
    m2 = np.zeros(t2, dtype=np.float32)
    amask = np.full((t2, t2), -1e4, dtype=np.float32)
    cos = np.zeros((t2, d), dtype=np.float32)
    sin = np.zeros((t2, d), dtype=np.float32)
    places, off = [], 0
    for i, s in enumerate(segments):
        L = frames_of(len(s))
        assert 0 < L and off + L <= t_feat - GAP
        n = HOP * (L + 1)
        audio[HOP * off: HOP * off + n] = s[:n]
        m0[off:off + L] = 1
        a1, l1 = off // 2, sub_len(L)
        m1[a1:a1 + l1] = 1
        a2, l2 = off // 4, sub_len(sub_len(L))
        m2[a2:a2 + l2] = 1
        amask[a2:a2 + l2, a2:a2 + l2] = 0
        cos[a2:a2 + l2] = cos_tab[:l2]
        sin[a2:a2 + l2] = sin_tab[:l2]
        places.append((i, a2, l2))
        off = (off + L + GAP + 3) // 4 * 4
    inputs = {
        "audio": (audio * SCALE)[None],
        "m0": m0.reshape(1, 1, -1), "m1": m1.reshape(1, 1, -1), "m2": m2.reshape(1, 1, -1),
        "amask": amask.reshape(1, 1, t2, t2),
        "cos": cos.reshape(1, t2, 1, d), "sin": sin.reshape(1, t2, 1, d),
    }
    return inputs, places


class Step(nn.Module):
    """GigaAM's RNN-T prediction network and joiner as one static step.

    One call advances the decoder by the token just emitted and scores a
    window of W encoder frames against the new state, for B independent
    lanes (segments) at once:

      inputs   onehot [B, V]    the token just emitted (blank at the start)
               h, c   [B, H]    LSTM state
               enc    [B, W, J] joiner-projected encoder frames of the window
      outputs  h_out, c_out [B, H]  the new state
               best   [B, W]   argmax token per frame
               best_lp[B, W]   its log-probability

    Greedy RNN-T keeps the decoder state fixed until a token is emitted, so
    the first non-blank frame of the window is the next emission: the host
    loop is a scan, and the Neural Engine never sees data-dependent control
    flow."""

    def __init__(self, head):
        super().__init__()
        d, j = head.decoder, head.joint
        self.embed = nn.Parameter(d.embed.weight.detach().clone())          # [V, H]
        self.w_ih = nn.Parameter(d.lstm.weight_ih_l0.detach().clone())      # [4H, H]
        self.w_hh = nn.Parameter(d.lstm.weight_hh_l0.detach().clone())
        self.b = nn.Parameter((d.lstm.bias_ih_l0 + d.lstm.bias_hh_l0).detach().clone())
        self.pred = j.pred
        self.out = j.joint_net[1]
        self.hid = d.pred_hidden
        self.register_buffer("ids", torch.arange(self.embed.shape[0], dtype=torch.float32))

    def forward(self, onehot, h, c, enc):
        x = onehot @ self.embed
        g = x @ self.w_ih.t() + h @ self.w_hh.t() + self.b
        H = self.hid
        i, f, gg, o = g[:, :H], g[:, H:2 * H], g[:, 2 * H:3 * H], g[:, 3 * H:]
        c = torch.sigmoid(f) * c + torch.sigmoid(i) * torch.tanh(gg)
        h = torch.sigmoid(o) * torch.tanh(c)
        z = torch.relu(enc + self.pred(h).unsqueeze(1))                    # [B, W, J]
        lp = torch.log_softmax(self.out(z), dim=-1)                        # [B, W, V]
        best_lp = lp.amax(dim=-1, keepdim=True)
        # argmax has no Neural Engine kernel: mark the maxima and take the
        # highest index among them. Ids below 2048 are exact in fp16.
        best = ((lp >= best_lp).to(lp.dtype) * self.ids).amax(dim=-1)
        return h, c, best, best_lp[..., 0]


def check_packing(model, net, cos_tab, sin_tab):
    """Three segments of different lengths in one window must each come out
    as they do alone."""
    torch.manual_seed(0)
    segs = [(torch.randn(n) * 0.05).numpy() for n in (16000 * 3 + 123, 16000 * 7 + 5, 16000 * 2 + 999)]
    inputs, places = pack(segs, T, cos_tab, sin_tab)
    worst = 0.0
    with torch.no_grad():
        got = net(*[torch.from_numpy(inputs[k]) for k in ["audio", "m0", "m1", "m2", "amask", "cos", "sin"]])[0]
        for i, a2, l2 in places:
            x = torch.from_numpy(segs[i])[None]
            f, fl = model.preprocessor(x, torch.tensor([x.shape[1]]))
            e, el = model.encoder(f, fl)                      # [1, 768, T2]
            if net.head == "rnnt":
                ref = model.head.joint.enc(e.transpose(1, 2)[0])
            else:
                ref = model.head.decoder_layers(e).transpose(1, 2).log_softmax(-1)[0]
            assert el.item() == l2, (el.item(), l2)
            worst = max(worst, (got[a2:a2 + l2] - ref).abs().max().item())
    print(f"packed vs standalone, torch fp32: max abs diff {worst:.2e}")
    assert worst < 1e-3, worst
    return inputs


def build(head):
    name, model_id, ckpt = HEADS[head]
    weights = common.WEIGHTS / "gigaam"
    build = common.BUILD / f"gigaam-{head}"
    model = gigaam.load_model(name, fp16_encoder=False, device="cpu", download_root=str(weights)).eval()
    model.requires_grad_(False)
    t2 = sub_len(sub_len(T))
    cos_tab, sin_tab = rope_tables(model, t2)

    net = Packed(model, T, head).eval()
    inputs = check_packing(model, net, cos_tab, sin_tab)
    names = ["audio", "m0", "m1", "m2", "amask", "cos", "sin"]
    ex = tuple(torch.from_numpy(inputs[k]) for k in names)
    with torch.no_grad():
        traced = torch.jit.trace(net, ex)
        dims = net(*ex).shape[-1]
    enc = common.save(common.convert(traced, list(zip(names, ex)), ["out"]), build / "encoder.mlpackage")

    pkg = common.package_dir(model_id)
    common.compile_into(enc, pkg / "encoder.mlmodelc")
    if head == "rnnt":
        step = Step(model.head).eval()
        V, H = step.embed.shape[0], step.hid
        ex = (torch.zeros(LANES, V), torch.zeros(LANES, H), torch.zeros(LANES, H), torch.zeros(LANES, WINDOW, dims))
        with torch.no_grad():
            traced = torch.jit.trace(step, ex)
        stp = common.save(common.convert(traced, list(zip(["onehot", "h", "c", "enc"], ex)), ["h_out", "c_out", "best", "best_lp"]),
                          build / "step.mlpackage")
        common.compile_into(stp, pkg / "step.mlmodelc")
    np.concatenate([cos_tab.ravel(), sin_tab.ravel()]).astype("<f4").tofile(pkg / "rope.bin")
    # sherpa-onnx's tokens.txt for these models: the tokenizer's pieces, then the blank.
    sp = spm.SentencePieceProcessor(model_file=str(weights / f"{ckpt}_tokenizer.model"))
    n = sp.get_piece_size()
    (pkg / "tokens.txt").write_text("".join(f"{sp.id_to_piece(i)} {i}\n" for i in range(n)) + f"<blk> {n}\n")
    json.dump({"head": head, "sample_rate": 16000, "window_frames": T, "gap_frames": GAP, "hop": HOP,
               "frame_length": N_FFT, "audio_scale": SCALE, "frame_shift_seconds": 0.04, "blank_id": n,
               "max_symbols_per_frame": 10, "rope_dim": cos_tab.shape[1], "encoder_output": "out",
               "encoder_dims": dims}, open(pkg / "config.json", "w"), indent=1)
    shutil.copy(common.SRC / "GigaAM" / "LICENSE", pkg / "LICENSE")
    common.notice(pkg, f"""
GigaAM v3, end-to-end {"RNN-T" if head == "rnnt" else "CTC"} with punctuation ({ckpt}), by the
GigaChat Team: https://github.com/salute-developers/GigaAM, code at revision
7447938d791c4f3e643386ee22c33777004293a5, weights {ckpt}.ckpt. MIT License, a
copy of which is in LICENSE.""")
    print("packaged", pkg)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--head", choices=sorted(HEADS))
    args = ap.parse_args()
    for head in [args.head] if args.head else ["rnnt", "ctc"]:
        build(head)


if __name__ == "__main__":
    main()
