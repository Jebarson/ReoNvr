$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
  docker compose version
  if ($LASTEXITCODE -ne 0) { throw 'Docker Compose v2 is required' }
  $assets = @('compose.yaml', 'start.sh', 'start.ps1', 'images.tar', 'images.txt', 'release.json', 'THIRD-PARTY-LICENSES.tar')
  $manifest = @(Get-Content -LiteralPath checksums.sha256)
  if ($manifest.Count -ne $assets.Count) { throw 'Release checksum manifest is incomplete. Extract the ZIP again.' }
  $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
  foreach ($line in $manifest) {
    if ($line -cnotmatch '^([0-9a-f]{64})  ([A-Za-z0-9.-]+)$') { throw 'Invalid release checksum entry' }
    $name = $Matches[2]
    if ($assets -cnotcontains $name -or -not $seen.Add($name)) { throw 'Unexpected or duplicate release checksum entry' }
  }
  foreach ($line in $manifest) {
    $digest, $name = $line -split '  ', 2
    if ((Get-FileHash -LiteralPath $name -Algorithm SHA256).Hash.ToLowerInvariant() -ne $digest) {
      throw 'Release checksum verification failed. Download and extract the ZIP again.'
    }
  }
  $architecture = docker info --format '{{.Architecture}}'
  if ($LASTEXITCODE -ne 0) { throw 'Docker is not running' }
  $architecture = switch ($architecture) {
    'x86_64' { 'amd64' }
    'amd64' { 'amd64' }
    'aarch64' { 'arm64' }
    'arm64' { 'arm64' }
    default { throw 'Unsupported Docker server architecture' }
  }
  $release = Get-Content release.json -Raw | ConvertFrom-Json
  if ($release.architecture -ne $architecture) { throw 'Download the ZIP matching the Docker server architecture' }
  $expectedImages = @('app', 'gateway', 'discovery-relay') | ForEach-Object { "reonvr/${_}:$($release.version)-$architecture" }
  $expectedImages = @($expectedImages | Sort-Object -CaseSensitive)
  $listedImages = @(Get-Content -LiteralPath images.txt | Sort-Object -CaseSensitive)
  if (($listedImages -join "`n") -cne ($expectedImages -join "`n")) { throw 'Image manifest does not match release metadata' }
  $resolvedImages = @(docker compose --file compose.yaml --env-file .env --profile '*' config --images)
  if ($LASTEXITCODE -ne 0) { throw 'Unable to resolve release Compose configuration' }
  $resolvedImages = @($resolvedImages | Sort-Object -CaseSensitive)
  if (($resolvedImages -join "`n") -cne ($expectedImages -join "`n")) { throw 'Compose images do not match this release. Check REONVR_VERSION and REONVR_ARCH in .env and your environment.' }
  docker load --input images.tar
  if ($LASTEXITCODE -ne 0) { throw 'Docker image import failed' }
  docker compose --file compose.yaml --env-file .env up -d --wait --wait-timeout 120
  if ($LASTEXITCODE -ne 0) { throw 'Startup failed. Run docker compose ps and docker compose logs.' }
  Write-Host 'ReoNvr is running. Open http://localhost:8080 (or the port in .env).'
} finally {
  Pop-Location
}