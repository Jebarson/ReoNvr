$ErrorActionPreference = 'Stop'
$source = Join-Path $PSScriptRoot '../start.ps1'
function global:docker {
  $global:StartupTestDockerCalls.Add(($args -join ' '))
  $global:LASTEXITCODE = 0
  if ($args[0] -eq 'info') { 'amd64' }
}
foreach ($case in @('empty', 'malformed', 'tampered', 'valid')) {
  $fixture = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
  try {
    New-Item -ItemType Directory -Path $fixture | Out-Null
    Copy-Item -LiteralPath $source -Destination (Join-Path $fixture 'start.ps1')
    $images = Join-Path $fixture 'images.tar'
    [IO.File]::WriteAllText($images, 'demo images')
    $digest = (Get-FileHash -LiteralPath $images -Algorithm SHA256).Hash.ToLowerInvariant()
    $manifest = switch ($case) {
      'empty' { '' }
      'malformed' { 'invalid' }
      default { "$digest  images.tar`n" }
    }
    [IO.File]::WriteAllText((Join-Path $fixture 'checksums.sha256'), $manifest)
    if ($case -eq 'tampered') { [IO.File]::WriteAllText($images, 'changed images') }
    @{ architecture = 'amd64' } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $fixture 'release.json')
    $global:StartupTestDockerCalls = [Collections.Generic.List[string]]::new()
    $rejected = $false
    try { & (Join-Path $fixture 'start.ps1') 6>$null } catch { $rejected = $true }
    if ($rejected -eq ($case -eq 'valid')) { throw "Unexpected startup result for $case manifest" }
    $loaded = $global:StartupTestDockerCalls.Contains('load --input images.tar')
    $started = $global:StartupTestDockerCalls.Contains('compose up -d --wait --wait-timeout 120')
    if ($case -eq 'valid') {
      if (-not $loaded -or -not $started) { throw 'Valid manifest did not reach mocked startup' }
    } elseif ($loaded -or $started) { throw 'Invalid manifest reached mocked image import or startup' }
  } finally {
    Remove-Item -LiteralPath $fixture -Recurse -Force
  }
}
Remove-Item Function:\docker
Write-Output 'Windows startup checksum checks passed all four mocked fixtures'