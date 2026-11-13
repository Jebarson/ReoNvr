#!/bin/sh
set -eu
cd "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
docker compose version >/dev/null
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -c checksums.sha256
else
  shasum -a 256 -c checksums.sha256
fi
architecture=$(docker info --format '{{.Architecture}}')
case "$architecture" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  *) printf 'Unsupported Docker architecture: %s\n' "$architecture" >&2; exit 1 ;;
esac
if ! grep -q "\"architecture\": \"$architecture\"" release.json; then
  printf 'Download the release ZIP matching the Docker server architecture.\n' >&2
  exit 1
fi
docker load --input images.tar
docker compose up -d --wait --wait-timeout 120
printf 'ReoNvr is running. Open http://localhost:8080 (or the port in .env).\n'