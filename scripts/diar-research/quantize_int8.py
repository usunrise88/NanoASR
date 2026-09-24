"""Quantize step.onnx to int8 for the opt-in nemotron-3-diarization-int8 entry.

Dynamic quantization, weights only, one scale per output channel: the variant
that kept every speaker decision of the fp32 graph on the reference clip and
the same DER on the 16-minute dialogue (1.4%), at 0.65 of the step time on a
CPU with VNNI. Activations stay float, so the embedding graph is not touched.

    venv/bin/python quantize_int8.py --src models/step.onnx --out models-int8/step.onnx
"""

import argparse

from onnxruntime.quantization import QuantType, quantize_dynamic


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", required=True)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    quantize_dynamic(args.src, args.out, weight_type=QuantType.QInt8, per_channel=True, reduce_range=False)


if __name__ == "__main__":
    main()
