#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# 烟测始终读取无凭据模板，不能继承真实账号、模型路径或部署密钥。
env_file=deploy/server.env.example
docker info >/dev/null

project="intercom-smoke-${RANDOM}-$$"
tmp_dir="$(mktemp -d)"
compose=(docker compose --project-name "$project" --env-file "$env_file" -f deploy/compose.yaml)
sim_pid=""

cleanup() {
  result=$?
  if [[ $result -ne 0 ]]; then
    "${compose[@]}" logs --no-color server janus >&2 || true
    if [[ -n "$sim_pid" ]]; then
      kill "$sim_pid" 2>/dev/null || true
    fi
  fi
  if ! "${compose[@]}" down --volumes --remove-orphans; then
    echo "Smoke project cleanup failed: $project" >&2
    result=1
  fi
  rm -rf "$tmp_dir"
  exit "$result"
}
trap cleanup EXIT

# 测试身份仅在本次 Compose 进程环境内覆盖模板值，并禁用外部 Linx 连接。
export DEVICE_TOKENS='{"smoke-device":"smoke-token"}'
export LINX_DEVICE_TOKENS='{}'
export MCP_API_TOKEN='intercom-local-smoke-only'
export LINX_WS_URL=''
export JANUS_API_SECRET=''
export JANUS_ADMIN_KEY=''
export KWS_ENCODER='' KWS_DECODER='' KWS_JOINER='' KWS_TOKENS='' KWS_KEYWORDS_FILE=''
export INTERCOM_HTTP_PORT="${INTERCOM_SMOKE_HTTP_PORT:-18080}"
export JANUS_HTTP_PORT="${JANUS_SMOKE_HTTP_PORT:-18088}"
export JANUS_WS_PORT="${JANUS_SMOKE_WS_PORT:-18188}"

python3 - "$tmp_dir/input.wav" <<'PY'
import math
import struct
import sys
import wave

with wave.open(sys.argv[1], "wb") as output:
    output.setnchannels(1)
    output.setsampwidth(2)
    output.setframerate(16000)
    samples = (int(500 * math.sin(2 * math.pi * 440 * i / 16000)) for i in range(16000))
    output.writeframes(b"".join(struct.pack("<h", sample) for sample in samples))
PY

"${compose[@]}" up --build -d
for attempt in $(seq 1 45); do
  if curl -fsS "http://127.0.0.1:${INTERCOM_HTTP_PORT}/healthz" >/dev/null; then
    break
  fi
  if [[ "$attempt" == 45 ]]; then
    echo "intercom server did not become healthy" >&2
    exit 1
  fi
  sleep 1
done

(cd server && go run ./cmd/intercom-sim \
  -server "ws://127.0.0.1:${INTERCOM_HTTP_PORT}/device/ws" \
  -device-id smoke-device -token smoke-token \
  -audio-file "$tmp_dir/input.wav" -reconnect-after 300ms \
  -output-file "$tmp_dir/downlink.pcm") >"$tmp_dir/simulator.log" 2>&1 &
sim_pid=$!
for attempt in $(seq 1 45); do
  if ! kill -0 "$sim_pid" 2>/dev/null; then
    wait "$sim_pid"
    sim_pid=""
    break
  fi
  if [[ "$attempt" == 45 ]]; then
    echo "Linux simulator did not finish" >&2
    exit 1
  fi
  sleep 1
done

grep -q 'simulator audio input complete' "$tmp_dir/simulator.log"
"${compose[@]}" logs --no-color server >"$tmp_dir/server.log"
grep -q 'device session resumed; stale queues cleared' "$tmp_dir/server.log"
"${compose[@]}" stop --timeout 10 server
"${compose[@]}" logs --no-color server >"$tmp_dir/server.log"
grep -q 'intercom server stopped' "$tmp_dir/server.log"
echo "Compose smoke checks passed: health, authenticated simulator audio, reconnect, graceful shutdown."
