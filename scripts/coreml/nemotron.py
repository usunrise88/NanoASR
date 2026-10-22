"""Nemotron 3.5 ASR streaming, multilingual, 560 ms: FluidInference's CoreML
build, unchanged, as nemotron-3.5-streaming-multi-coreml.

Nothing is converted here. FluidInference's encoder (int8, 6-bit palettized)
and decoder-joint already run on the Neural Engine, and the Go side drives
them the way their FluidAudio SDK does (internal/asr/coremlasr/nemotron.go).
fetch.sh downloads the files at a pinned revision and checks each; this
packages them with the model's licence and a notice of where they came from.

usage: nemotron.py
"""
import shutil

import common

MODEL_ID = "nemotron-3.5-streaming-multi-coreml"
HF_ID = "FluidInference/Nemotron-3.5-ASR-Streaming-Multilingual-0.6b-CoreML"
HF_REVISION = "1a41b75758b0337ff67db7d5408280aaaf23074e"


def main():
    src = common.WEIGHTS / "nemotron"
    pkg = common.package_dir(MODEL_ID)
    for name in ["encoder.mlmodelc", "decoder_joint.mlmodelc"]:
        shutil.copytree(src / name, pkg / name)
    for name in ["metadata.json", "tokenizer.json"]:
        shutil.copy(src / name, pkg / name)
    shutil.copy(common.ROOT / "scripts" / "diar-research" / "LICENSE-OpenMDW-1.1.txt", pkg / "LICENSE")
    common.notice(pkg, f"""
Nemotron 3.5 ASR Streaming 0.6B (nvidia/nemotron-3.5-asr-streaming-0.6b), in
FluidInference's CoreML build:
https://huggingface.co/{HF_ID}
revision {HF_REVISION}, folder multilingual/560ms,
files unchanged. Licensed under OpenMDW-1.1, a copy of which is in LICENSE.""")
    print("packaged", pkg)


if __name__ == "__main__":
    main()
