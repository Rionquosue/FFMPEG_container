#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
image="${1:-ffmpeg-service:test}"
suffix="$$"
network="ffmpeg-smoke-$suffix"
container="ffmpeg-smoke-$suffix"
scratch="$(mktemp -d)"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT INT TERM
python3 - "$scratch" <<'PY'
import json, secrets, sys
from pathlib import Path
p=Path(sys.argv[1])/"keys.json"
p.write_text(json.dumps({"consumers":[{"id":"test","keys":[secrets.token_urlsafe(32)]}]}))
p.chmod(0o644)
PY
ffmpeg -v error -y -f lavfi -i color=c=blue:s=64x64:r=10 -f lavfi -i sine=frequency=440 \
  -t 1 -threads 1 -filter_threads 1 -c:v libx264 -pix_fmt yuv420p -c:a aac "$scratch/input.mp4"
docker pull python:3.12-alpine >/dev/null
docker network create --internal "$network" >/dev/null
docker run -d --name "$container" --network "$network" --network-alias ffmpeg \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true --pids-limit 128 \
  --memory 2g --memory-swap 2g --cpus 2 \
  --tmpfs /work:rw,noexec,nosuid,nodev,size=1073741824,uid=10001,gid=10001,mode=0700 \
  -v "$scratch/keys.json:/run/secrets/api_keys:ro" "$image" >/dev/null
# Readiness includes real secret/sibling read-denial and subprocess network tests.
ready=0
for n in $(seq 1 30); do
  if docker exec "$container" /usr/local/bin/ffmpeg-api healthcheck; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then docker logs "$container"; exit 1; fi
docker run --rm --network "$network" --read-only --cap-drop ALL \
  -v "$PWD/scripts:/scripts:ro" -v "$scratch:/fixtures:ro" \
  python:3.12-alpine python /scripts/smoke-client.py
docker exec "$container" sh -c 'test "$(id -u)" = 10001'
docker logs "$container"
