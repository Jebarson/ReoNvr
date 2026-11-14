#!/bin/sh
set -eu
cd "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
docker compose version >/dev/null
if ! awk '
  BEGIN {
    count = split("compose.yaml start.sh start.ps1 images.tar images.txt release.json THIRD-PARTY-LICENSES.tar", assets, " ")
    for (assetIndex = 1; assetIndex <= count; assetIndex++) expected[assets[assetIndex]] = 1
  }
  {
    digest = substr($0, 1, 64)
    name = substr($0, 67)
    if (length(digest) != 64 || digest ~ /[^0-9a-f]/ || substr($0, 65, 2) != "  " || !(name in expected) || seen[name]++) exit 1
  }
  END { for (name in expected) if (seen[name] != 1) exit 1 }
' checksums.sha256; then
  printf 'Release checksum manifest is incomplete or invalid. Extract the ZIP again.\n' >&2
  exit 1
fi
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
if ! awk -v architecture="$architecture" '
  BEGIN { expected["reonvr/app"] = 1; expected["reonvr/gateway"] = 1; expected["reonvr/discovery-relay"] = 1 }
  {
    separator = index($0, ":")
    name = substr($0, 1, separator - 1)
    tag = substr($0, separator + 1)
    if (!(name in expected) || seen[name]++ || tag !~ /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?-(amd64|arm64)$/ || substr(tag, length(tag) - length(architecture) + 1) != architecture) exit 1
    if (NR == 1) version = tag
    else if (tag != version) exit 1
  }
  END { if (NR != 3) exit 1 }
' images.txt; then
  printf 'Invalid image metadata or wrong Docker server architecture.\n' >&2
  exit 1
fi
resolved_images=$(docker compose --file compose.yaml --env-file .env --profile '*' config --images)
if [ "$(printf '%s\n' "$resolved_images" | LC_ALL=C sort)" != "$(LC_ALL=C sort images.txt)" ]; then
  printf 'Compose images do not match this release. Check REONVR_VERSION and REONVR_ARCH in .env and your environment.\n' >&2
  exit 1
fi
docker load --input images.tar
docker compose --file compose.yaml --env-file .env up -d --wait --wait-timeout 120
printf 'ReoNvr is running. Open http://localhost:8080 (or the port in .env).\n'