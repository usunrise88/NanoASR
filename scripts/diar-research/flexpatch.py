"""Make NeMo's TransformerEncoder traceable by replacing FlexAttention with SDPA.

The production Nemotron-3-Diarization encoder computes attention through
torch.nn.attention.flex_attention. create_block_mask runs vmap over the mask
function, which neither ONNX exporter can trace — that is the whole blocker.

For this model the substitution is exact rather than approximate, and the reason
is in NeMo's own comment: under ``rope`` the rotation is applied to Q and K
directly and ``score_mod`` stays None, so FlexAttention is being used for one
thing only — the padding mask. What is left is

    softmax(QK^T / sqrt(d) masked by mask_mod) V

which is scaled_dot_product_attention with a boolean mask. The patch therefore
refuses to run whenever score_mod is not None, rather than silently computing
something else.

Two seams, no copied code:
  * create_block_mask -> evaluate mask_mod on broadcast index grids and return a
    plain (B, 1, Q, KV) bool tensor. mask_mod is elementwise by contract, so
    calling it on broadcast tensors is what vmap was doing, minus the tracing
    problem.
  * _get_flex_attention -> hand back an SDPA call that consumes that tensor.
"""

import torch
import torch.nn.functional as F

from nemo.collections.asr.modules import transformer_encoder as _te
from nemo.collections.asr.modules import transformer_encoder_utils as _teu

_original = {}


def _dense_block_mask(mask_mod, B, H, Q_LEN, KV_LEN, device=None, **kwargs):
    """Evaluate mask_mod over the index grid instead of vmapping it."""
    b = torch.arange(B, device=device).view(B, 1, 1, 1)
    h = torch.arange(max(H, 1), device=device).view(1, max(H, 1), 1, 1)
    q_idx = torch.arange(Q_LEN, device=device).view(1, 1, Q_LEN, 1)
    kv_idx = torch.arange(KV_LEN, device=device).view(1, 1, 1, KV_LEN)
    mask = mask_mod(b, h, q_idx, kv_idx)
    return mask.expand(B, max(H, 1), Q_LEN, KV_LEN)


def _sdpa(q, k, v, block_mask=None, score_mod=None):
    if score_mod is not None:
        raise NotImplementedError(
            "this patch only replaces FlexAttention where score_mod is None "
            "(the rope path); rel_pos folds positions into the score and would "
            "need its own equivalent"
        )
    return F.scaled_dot_product_attention(q, k, v, attn_mask=block_mask)


def _get_sdpa(_x):
    return _sdpa


def apply():
    """Swap both seams. Idempotent."""
    if _original:
        return
    _original["create_block_mask"] = _te.create_block_mask
    _original["_get_flex_attention"] = _teu._get_flex_attention
    _te.create_block_mask = _dense_block_mask
    _teu._get_flex_attention = _get_sdpa


def revert():
    if not _original:
        return
    _te.create_block_mask = _original.pop("create_block_mask")
    _teu._get_flex_attention = _original.pop("_get_flex_attention")
    _original.clear()
