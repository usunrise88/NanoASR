"""Produce int8 variants of step.onnx with different accuracy/speed trade-offs.

per_channel gives each weight column its own scale instead of one scale for the
whole tensor, which is where most of the accuracy of a dynamically quantized
transformer is won or lost. reduce_range keeps weights in 7 bits, which avoids
saturation in the int8 accumulate path on CPUs without VNNI — this one is a
2.8 GHz Xeon, so it is not a given.
"""
import sys
from onnxruntime.quantization import quantize_dynamic, QuantType

src = "models/step.onnx"
variants = [
    ("models/q_perchan.onnx",        dict(per_channel=True,  reduce_range=False)),
    ("models/q_perchan_reduce.onnx", dict(per_channel=True,  reduce_range=True)),
    ("models/q_pertensor_reduce.onnx", dict(per_channel=False, reduce_range=True)),
]
for out, kw in variants:
    print(f"   {out}: {kw}", flush=True)
    quantize_dynamic(src, out, weight_type=QuantType.QInt8, **kw)
    import os
    print(f"     -> {os.path.getsize(out)/1e6:.0f} MB", flush=True)
