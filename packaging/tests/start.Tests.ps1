param([ValidateSet('amd64', 'arm64')][string]$Architecture = 'amd64')

$ErrorActionPreference = 'Stop'
$source = Join-Path $PSScriptRoot '..'
$assets = @('compose.yaml', 'start.sh', 'start.ps1', 'images.tar', 'images.txt', 'release.json', 'THIRD-PARTY-LICENSES.tar')
$global:StartupTestArchitecture = $Architecture
$expectedImages = @('app', 'gateway', 'discovery-relay') | ForEach-Object { "reonvr/${_}:v1.2.3-$Architecture" }
function global:docker {
  $global:StartupTestDockerCalls.Add(($args -join ' '))
  $global:LASTEXITCODE = 0
  if ($args[0] -eq 'info') { $global:StartupTestArchitecture }
  if ($args[0] -eq 'compose' -and $args -contains '--images') {
    if ($global:StartupTestFailConfig) { $global:LASTEXITCODE = 1 } else { $global:StartupTestImages }
  }
}
$cases = @('valid', 'empty', 'malformed', 'tampered', 'partial', 'duplicate', 'unknown', 'stale-version', 'wrong-image-architecture', 'unexpected-image', 'missing-image', 'failed-config')
$cases += $assets | ForEach-Object { "missing-$_" }
$unixMock = 'docker() { printf "%s\n" "$*" >> docker-calls.log; case "$1" in info) cat architecture.txt;; compose) case "$*" in *--images*) if test -f fail-config; then return 1; fi; cat resolved-images.txt;; esac;; esac; }; . ./start.sh'
$originalDirectory = Get-Location
foreach ($case in $cases) {
  $fixture = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
  try {
    New-Item -ItemType Directory -Path $fixture | Out-Null
    foreach ($name in @('start.ps1', 'start.sh', 'compose.yaml')) {
      Copy-Item -LiteralPath (Join-Path $source $name) -Destination (Join-Path $fixture $name)
    }
    $shell = Join-Path $fixture 'start.sh'
    [IO.File]::WriteAllText($shell, [IO.File]::ReadAllText($shell).Replace("`r`n", "`n"))
    $images = Join-Path $fixture 'images.tar'
    [IO.File]::WriteAllText($images, 'demo images')
    [IO.File]::WriteAllText((Join-Path $fixture 'THIRD-PARTY-LICENSES.tar'), 'demo licenses')
    [IO.File]::WriteAllText((Join-Path $fixture 'images.txt'), ($expectedImages -join "`n") + "`n")
    @{ version = 'v1.2.3'; architecture = $Architecture; images = $expectedImages } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $fixture 'release.json')
    [IO.File]::WriteAllText((Join-Path $fixture '.env'), "REONVR_VERSION=v1.2.3`nREONVR_ARCH=$Architecture`n")
    [IO.File]::WriteAllText((Join-Path $fixture 'architecture.txt'), "$Architecture`n")
    $manifest = @($assets | ForEach-Object {
      $digest = (Get-FileHash -LiteralPath (Join-Path $fixture $_) -Algorithm SHA256).Hash.ToLowerInvariant()
      "$digest  $_"
    })
    switch ($case) {
      'empty' { $manifest = @() }
      'malformed' { $manifest[0] = 'invalid' }
      'partial' { $manifest = @($manifest[0]) }
      'duplicate' { $manifest[0] = $manifest[1] }
      'unknown' { $manifest[0] = ('a' * 64) + '  unexpected.txt' }
    }
    if ($case.StartsWith('missing-') -and $case -ne 'missing-image') {
      $missing = $case.Substring('missing-'.Length)
      $manifest = @($manifest | Where-Object { -not $_.EndsWith("  $missing") })
    }
    [IO.File]::WriteAllText((Join-Path $fixture 'checksums.sha256'), ($manifest -join "`n") + "`n")
    if ($case -eq 'tampered') { [IO.File]::WriteAllText($images, 'changed images') }
    $global:StartupTestImages = switch ($case) {
      'stale-version' { $expectedImages -replace 'v1.2.3', 'v0.9.0' }
      'wrong-image-architecture' { $other = if ($Architecture -eq 'amd64') { 'arm64' } else { 'amd64' }; $expectedImages -replace $Architecture, $other }
      'unexpected-image' { @($expectedImages) + 'unexpected/image:v1.2.3-amd64' }
      'missing-image' { $expectedImages[0..1] }
      default { $expectedImages }
    }
    [IO.File]::WriteAllText((Join-Path $fixture 'resolved-images.txt'), ($global:StartupTestImages -join "`n") + "`n")
    $global:StartupTestFailConfig = $case -eq 'failed-config'
    if ($case -eq 'failed-config') { [IO.File]::WriteAllText((Join-Path $fixture 'fail-config'), '') }
    $global:StartupTestDockerCalls = [Collections.Generic.List[string]]::new()
    $rejected = $false
    try { & (Join-Path $fixture 'start.ps1') 6>$null } catch { $rejected = $true }
    if ($rejected -eq ($case -eq 'valid')) { throw "Unexpected startup result for $case manifest" }
    $loaded = $global:StartupTestDockerCalls.Contains('load --input images.tar')
    $started = $global:StartupTestDockerCalls.Contains('compose --file compose.yaml --env-file .env up -d --wait --wait-timeout 120')
    if ($case -eq 'valid') {
      if (-not $loaded -or -not $started) { throw 'Valid manifest did not reach mocked startup' }
    } elseif ($loaded -or $started) { throw 'Invalid manifest reached mocked image import or startup' }
    if ($env:OS -ne 'Windows_NT') {
      Set-Location $fixture
      $unixOutput = @(& /bin/sh -c $unixMock ./start.sh 2>&1)
      $unixRejected = $LASTEXITCODE -ne 0
      Set-Location $originalDirectory
      if ($unixRejected -eq ($case -eq 'valid')) { throw "Unexpected Unix startup result for $case" }
      $calls = @(Get-Content -LiteralPath (Join-Path $fixture 'docker-calls.log') -ErrorAction SilentlyContinue)
      if ($case -eq 'valid' -and (-not ($calls -contains 'load --input images.tar') -or -not ($calls -contains 'compose --file compose.yaml --env-file .env up -d --wait --wait-timeout 120'))) {
        throw 'Valid Unix fixture did not reach mocked startup'
      }
      if ($case -ne 'valid' -and ($calls -contains 'load --input images.tar' -or $calls -contains 'compose --file compose.yaml --env-file .env up -d --wait --wait-timeout 120')) {
        throw "Invalid Unix fixture reached image import or startup: $case"
      }
    }
  } finally {
    Set-Location $originalDirectory
    Remove-Item -LiteralPath $fixture -Recurse -Force
  }
}
Remove-Item Function:\docker
Write-Output "Startup checks passed all $($cases.Count) fixtures for both scripts."