#!/usr/bin/env bash
#
# Fetch what the converters in this directory start from — the authors' source
# trees and weights, each at a pinned revision and checked against its sha256 —
# into $NANOASR_COREML_WORK (default .cache/coreml). Files already there with
# the right checksum are kept, so a second run downloads nothing.
#
# Nemotron-3-Diarization is not fetched here: sortformer.py loads it through
# transformers at the revision scripts/diar-research pins, into the Hugging
# Face cache.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="${NANOASR_COREML_WORK:-$HERE/../../.cache/coreml}"
mkdir -p "$WORK"
WORK="$(cd "$WORK" && pwd)"
SRC="$WORK/src"
W="$WORK/weights"

sha() { shasum -a 256 "$1" | cut -d' ' -f1; }

# get URL DEST SHA256
get() {
  local url="$1" dest="$2" want="$3"
  if [[ -f "$dest" && "$(sha "$dest")" == "$want" ]]; then
    return
  fi
  mkdir -p "$(dirname "$dest")"
  curl -sSfL --retry 3 -o "$dest.part" "$url"
  local got
  got="$(sha "$dest.part")"
  if [[ "$got" != "$want" ]]; then
    rm -f "$dest.part"
    echo "$url: sha256 $got, want $want" >&2
    exit 1
  fi
  mv "$dest.part" "$dest"
  echo "fetched $dest"
}

# clone URL COMMIT DEST: that one commit, nothing else.
clone() {
  local url="$1" rev="$2" dest="$3"
  if [[ "$(git -C "$dest" rev-parse HEAD 2>/dev/null)" == "$rev" ]]; then
    return
  fi
  rm -rf "$dest"
  git init -q "$dest"
  git -C "$dest" fetch -q --depth 1 "$url" "$rev"
  git -C "$dest" -c advice.detachedHead=false checkout -q FETCH_HEAD
  echo "cloned $url@$rev"
}

# -- GigaAM v3 ----------------------------------------------------------------
clone https://github.com/salute-developers/GigaAM 7447938d791c4f3e643386ee22c33777004293a5 "$SRC/GigaAM"
GIGA=https://cdn.chatwm.opensmodel.sberdevices.ru/GigaAM
get "$GIGA/v3_e2e_rnnt.ckpt" "$W/gigaam/v3_e2e_rnnt.ckpt" \
  f60c62fe45902d967000770a12f260de7c5ac4d4b8e5e852f1df8c548e3958b5
get "$GIGA/v3_e2e_rnnt_tokenizer.model" "$W/gigaam/v3_e2e_rnnt_tokenizer.model" \
  828c12c991019eef952a960661f25a92d6ad279591e2ea466b4aeddf1d20a18a
get "$GIGA/v3_e2e_ctc.ckpt" "$W/gigaam/v3_e2e_ctc.ckpt" \
  73e27ec8f904e486ab063d5bb7c6ab701838b7e5a967ee79b71ccc5aefde146f
get "$GIGA/v3_e2e_ctc_tokenizer.model" "$W/gigaam/v3_e2e_ctc_tokenizer.model" \
  0b9a1960898fbfdf5424ab852ea17445eb3da960fba23e977ff100eb0054fbc8

# -- Silero VAD v4, the file silero-vad-v5 in the catalog is -------------------
get https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx "$W/silero/silero_vad.onnx" \
  9e2449e1087496d8d4caba907f23e0bd3f78d91fa552479bb9c23ac09cbb1fd6
get https://raw.githubusercontent.com/snakers4/silero-vad/915dd3d639b8333a52e001af095f87c5b7f1e0ac/LICENSE "$W/silero/LICENSE" \
  2e63e9a38b6e8fc0c7bc37ce174caca1862870856c6daf5697cfb785e925520b

# -- Voxlock -------------------------------------------------------------------
clone https://github.com/arbazsiddiqui/voxlock 08ed37cbb79f449ca6b499bc23fb14138b207bfd "$SRC/voxlock"
VOX=https://huggingface.co/arbazsiddiqui/voxlock/resolve/a50f4e1d109301d4b6f61dec57ebb1a61e7d8f03
get "$VOX/config.yaml" "$W/voxlock/config.yaml" \
  786230c34b7f40f4a23eb1dd791d917010e74443a7e8125eee7e9f14fae0224b
get "$VOX/model.safetensors" "$W/voxlock/model.safetensors" \
  c90815e7cdd1a859029d2a7e8e9b30616b23d3abb183cbd38e4f79daab98ac42

# -- CAM++ (3D-Speaker, zh-en common advanced) ---------------------------------
# Only the two files that define the network, laid out as the package they
# import each other from.
DS=https://raw.githubusercontent.com/modelscope/3D-Speaker/065629c313eaf1a01c65c640c46d77e61e9607b4
CAMP="$SRC/3dspeaker/speakerlab/models/campplus"
get "$DS/speakerlab/models/campplus/DTDNN.py" "$CAMP/DTDNN.py" \
  52272359361ef9f03a8b4198f50139301ce712833e777396940a5b437177cd34
get "$DS/speakerlab/models/campplus/layers.py" "$CAMP/layers.py" \
  b3d28ad94d14b30fcd20c96d1d83516d4e44de733959460b358e90d97476adb8
get "$DS/LICENSE" "$SRC/3dspeaker/LICENSE" \
  c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4
touch "$SRC/3dspeaker/speakerlab/__init__.py" "$SRC/3dspeaker/speakerlab/models/__init__.py" "$CAMP/__init__.py"
get https://www.modelscope.cn/models/iic/speech_campplus_sv_zh_en_16k-common_advanced/resolve/master/campplus_cn_en_common.pt \
  "$W/campplus/campplus_cn_en_common.pt" 92f29b94e6948786a26778c9e302525d185bb08c8b9f5252ed98776902840199

# -- T-one ---------------------------------------------------------------------
clone https://github.com/voicekit-team/T-one 3c5b6c015038173840e62cea99e10cdb1c759116 "$SRC/T-one"
TONE=https://huggingface.co/t-tech/T-one/resolve/106f3b0b32a9e107eb613312e4ebc61ff3d53926
get "$TONE/config.json" "$W/tone/config.json" 8a6272e21a05301afac2686f6066554151f9cd85bd0d817297339e8c3dc778a6
get "$TONE/model.safetensors" "$W/tone/model.safetensors" 60f42e72314cd132a25a770913a6c55ebd44fb1a665d0e2371267551eab08007
get "$TONE/vocab.json" "$W/tone/vocab.json" 9b5c1255eb3d2e2bcd3021fd9edb606fef735c3a3fc9841e56f3014829ea25a6

# -- Nemotron 3.5 ASR streaming: FluidInference's CoreML build, unchanged -------
NEMO=https://huggingface.co/FluidInference/Nemotron-3.5-ASR-Streaming-Multilingual-0.6b-CoreML/resolve/1a41b75758b0337ff67db7d5408280aaaf23074e/multilingual/560ms
while read -r want f; do
  get "$NEMO/$f" "$W/nemotron/$f" "$want"
done <<'FILES'
5a09e646e9cc1b204f6c25b414a819c51b006587a425f66f3220cf1ef0645113 metadata.json
fb70c8fcb6472cda2bdb799b156a8941e72762344a7c636d3b1275f1d53c4a6b tokenizer.json
269d7188428298be3d0b2803c0e42bf6d44f7b4a9371030ae690beac443afbfa encoder.mlmodelc/coremldata.bin
990af9875f58889a98113a536801b7c121bed1eac1aa8421138c50018108d622 encoder.mlmodelc/model.mil
0e994440ed6c936ac375d375eaab16eb445881ff2d9bc62384de991b411e234e encoder.mlmodelc/weights/weight.bin
3409887ed9eadc8689bd4c64754d95e4119152dc0b9ee1650664d2a6a689c783 encoder.mlmodelc/analytics/coremldata.bin
ddd2d71cd9d13cc7b1d6f7632ab99f482b5f3f07a726bbc0673802c9dd6dcf71 decoder_joint.mlmodelc/coremldata.bin
62144df8c1928d571c1df508a576f2d574a6bd6b1f2dba7f3f15cf9605805d6a decoder_joint.mlmodelc/model.mil
01f21eb747fbc53bd0ed7efebea1bf0aa655ebf2816f21d0bb6554c9b7fcfc0b decoder_joint.mlmodelc/weights/weight.bin
8a8e98a54ed1f16c3d5125816a002b991e167d620beb8fcc557f26d9a1c092f8 decoder_joint.mlmodelc/analytics/coremldata.bin
FILES

echo "sources and weights are in $WORK"
