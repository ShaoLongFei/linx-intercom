#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
model_name="sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01"
model_dir="$repo_root/deploy/models/kws/$model_name"
model_repo="https://www.modelscope.cn/models/pkufool/$model_name/resolve"
weights_revision="f360c3af05b218887669bf03d69d63042a1cd4e6"
tokens_revision="c7f531779e2324b104e58899d2d7727ea98e0b19"

if ! command -v sherpa-onnx-cli >/dev/null 2>&1 ||
   ! python3 -c 'import click, pypinyin, sentencepiece' >/dev/null 2>&1; then
    echo "sherpa-onnx-cli and its text2token dependencies are required; install with: python3 -m pip install sherpa-onnx click sentencepiece pypinyin" >&2
    exit 1
fi
if [[ -e "$model_dir" ]]; then
    echo "model directory already exists; refusing to overwrite: $model_dir" >&2
    exit 1
fi

temp_root="${TMPDIR:-$HOME/tmp}"
mkdir -p "$temp_root"
temp_dir="$(mktemp -d "$temp_root/linx-intercom-kws.XXXXXX")"
trap 'rm -rf "$temp_dir"' EXIT

download_model_file() {
    local filename="$1" revision="$2" checksum="$3"
    curl -fL --retry 3 --connect-timeout 15 \
        "$model_repo/$revision/$filename" -o "$temp_dir/$filename"
    printf '%s  %s\n' "$checksum" "$temp_dir/$filename" | shasum -a 256 -c -
}

download_model_file \
    encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx "$weights_revision" \
    017af32f2c0138f931d05fbc009ee864295e910aff304f77d2f563815fc834fb
download_model_file \
    decoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx "$weights_revision" \
    fe53b8d6a07bc5373d1770649025a5a985c8fb3dab70323386a1b73aacba0546
download_model_file \
    joiner-epoch-12-avg-2-chunk-16-left-64.int8.onnx "$weights_revision" \
    431de10b554f134ef8af320feea2db337e641290449a3d3f6cb6e5f5fd2c9c3d
download_model_file \
    tokens.txt "$tokens_revision" \
    72316508d9119696145abc6f1f8cdc46287535c34e5ce7e595f845cb1499cf2e

printf '%s\n' '灵矽灵矽 @灵矽灵矽' > "$temp_dir/keywords_raw.txt"
sherpa-onnx-cli text2token \
    --tokens "$temp_dir/tokens.txt" \
    --tokens-type ppinyin \
    "$temp_dir/keywords_raw.txt" "$temp_dir/keywords.txt"

test -s "$temp_dir/keywords.txt"
mkdir -p "$repo_root/deploy/models/kws"
mkdir "$model_dir"
mv "$temp_dir"/* "$model_dir/"

echo "Wake-word model prepared at: $model_dir"
echo "Add these paths to deploy/.env:"
printf 'KWS_ENCODER=/opt/kws/%s/encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx\n' "$model_name"
printf 'KWS_DECODER=/opt/kws/%s/decoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx\n' "$model_name"
printf 'KWS_JOINER=/opt/kws/%s/joiner-epoch-12-avg-2-chunk-16-left-64.int8.onnx\n' "$model_name"
printf 'KWS_TOKENS=/opt/kws/%s/tokens.txt\n' "$model_name"
printf 'KWS_KEYWORDS_FILE=/opt/kws/%s/keywords.txt\n' "$model_name"
