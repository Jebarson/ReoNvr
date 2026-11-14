$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
  docker compose version
  if ($LASTEXITCODE -ne 0) { throw 'Docker Compose v2 is required' }
    $manifest = @(Get-Content -LiteralPath checksums.sha256)
    if ($manifest.Count -eq 0) { throw 'Release checksum manifest is empty. Download and extract the ZIP again.' }
    foreach ($line in $manifest) {
    $digest, $name = $line -split '  ', 2
    if (-not $name -or (Get-FileHash -LiteralPath $name -Algorithm SHA256).Hash.ToLowerInvariant() -ne $digest) {
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
  docker load --input images.tar
  if ($LASTEXITCODE -ne 0) { throw 'Docker image import failed' }
  docker compose up -d --wait --wait-timeout 120
  if ($LASTEXITCODE -ne 0) { throw 'Startup failed. Run docker compose ps and docker compose logs.' }
  Write-Host 'ReoNvr is running. Open http://localhost:8080 (or the port in .env).'
} finally {
  Pop-Location
}