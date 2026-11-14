$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
docker run --rm -i --tmpfs /root/.config/gh:rw,noexec,nosuid,size=1m -v "${root}:/src:ro" -w /src alpine:3.24.4 sh -c 'set -eu
apk add --no-cache github-cli >/dev/null
gh auth login --hostname github.com --git-protocol https --web --skip-ssh-key
gh api --method PUT repos/Jebarson/ReoNvr/branches/main/protection --input .github/branch-protection.json'
if ($LASTEXITCODE -ne 0) { throw 'GitHub could not enable protection. A private repository requires an eligible GitHub plan.' }