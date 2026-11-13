param(
  [string]$Config = (Join-Path $PSScriptRoot '../../runtime/test-config.json'),
  [switch]$SkipLive
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$configPath = if ($SkipLive) { $null } else { (Resolve-Path $Config).Path }
$guard = 'reonvr-e2e-pause-guard'
$guardStarted = $false
$pauseConfirmed = $false
$pauseNow = @'
try {
  const response = await fetch('http://reonvr-e2e-pause-guard:8099/pause', { method: 'POST', signal: AbortSignal.timeout(10000) });
  if (response.ok && (await response.json()).enabled === false) process.exit(0);
} catch {}
const response = await fetch('http://reonvr-fresh-app:8080/api/system/recording', {
  method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: false }), signal: AbortSignal.timeout(10000)
});
if (!response.ok || (await response.json()).enabled !== false) throw new Error('Could not confirm recording pause');
'@

Push-Location $root
try {
  if (-not $SkipLive) {
    docker run --rm -v "${root}:/src" -v reonvr-e2e-tools:/out -v reonvr-go-mod:/go/pkg/mod -v reonvr-go-build:/root/.cache/go-build -w /src golang:1.27-alpine go build -o /out/credentials ./web/e2e/credentials
    if ($LASTEXITCODE -ne 0) { throw 'Configuration helper build failed' }
    docker run --rm -e REONVR_E2E_CREDENTIALS=1 -e REONVR_TEST_CONFIG=/run/test-config.json -v reonvr-e2e-tools:/tools:ro -v "${configPath}:/run/test-config.json:ro" --entrypoint /tools/credentials reonvr-fresh-app:snapshot > $null
    if ($LASTEXITCODE -ne 0) { throw 'Fill in the ignored local test configuration before running live acceptance' }
  }
  docker run --rm -v "${root}:/src" -v reonvr-go-mod:/go/pkg/mod -v reonvr-go-build:/root/.cache/go-build -w /src golang:1.27-alpine go test ./... -count=1
  if ($LASTEXITCODE -ne 0) { throw 'Backend regression tests failed' }
  docker run --rm -v "${root}:/src" -v reonvr-playwright-modules:/src/web/node_modules -w /src/web mcr.microsoft.com/playwright:v1.65.0-noble npm ci
  if ($LASTEXITCODE -ne 0) { throw 'Containerized frontend dependency installation failed' }
  docker run --rm -v "${root}:/src" -v reonvr-playwright-modules:/src/web/node_modules -w /src/web mcr.microsoft.com/playwright:v1.65.0-noble npm run lint
  if ($LASTEXITCODE -ne 0) { throw 'Frontend lint failed' }
  docker run --rm -v "${root}:/src" -v reonvr-playwright-modules:/src/web/node_modules -w /src/web mcr.microsoft.com/playwright:v1.65.0-noble npm run build
  if ($LASTEXITCODE -ne 0) { throw 'Frontend build failed' }
  docker run --rm -v "${root}:/src:ro" -w /src/web node:26-alpine node --test e2e/pause-guard.test.mjs
  if ($LASTEXITCODE -ne 0) { throw 'Independent pause guard tests failed' }
  docker run --rm -v "${root}:/src" -v reonvr-e2e-tools:/out -v reonvr-go-mod:/go/pkg/mod -v reonvr-go-build:/root/.cache/go-build -w /src golang:1.27-alpine go test -c -o /out/archive.test ./internal/archive
  if ($LASTEXITCODE -ne 0) { throw 'Native-media test compilation failed' }
  docker run --rm -v reonvr-e2e-tools:/tools:ro --entrypoint /tools/archive.test reonvr-fresh-app:snapshot '-test.run=TestClockAlignedSegmentsUseRelativeMediaTime|TestMediaInspectionWithRealFFmpeg' '-test.v=true'
  if ($LASTEXITCODE -ne 0) { throw 'FFmpeg-backed media tests failed' }
  if ($SkipLive) { return }
  docker run --rm -v "${root}:/src" -v reonvr-e2e-tools:/out -v reonvr-go-mod:/go/pkg/mod -v reonvr-go-build:/root/.cache/go-build -w /src golang:1.27-alpine go test -c -o /out/reolink.test ./internal/reolink
  if ($LASTEXITCODE -ne 0) { throw 'Live-camera test compilation failed' }
  docker run --rm -e REONVR_E2E_CREDENTIALS=1 -e REONVR_TEST_CONFIG=/run/test-config.json -v reonvr-e2e-tools:/tools:ro -v "${configPath}:/run/test-config.json:ro" --entrypoint /tools/credentials reonvr-fresh-app:snapshot |
    docker run --rm -i --network reonvr-fresh_default -e REONVR_E2E_LIVE=1 -v reonvr-e2e-tools:/tools:ro --entrypoint /tools/reolink.test reonvr-fresh-app:snapshot '-test.run=TestLiveCameraStorage' '-test.v=true'
  if ($LASTEXITCODE -ne 0) { throw 'Read-only live clock and native SD tests failed' }
  docker run --rm -d --name $guard --network reonvr-fresh_default -v "${root}:/src:ro" node:26-alpine node /src/web/e2e/pause-guard.mjs | Out-Null
  if ($LASTEXITCODE -ne 0) { throw 'Independent recording pause guard could not start' }
  $guardStarted = $true
  docker run --rm -e REONVR_E2E_CREDENTIALS=1 -e REONVR_TEST_CONFIG=/run/test-config.json -v reonvr-e2e-tools:/tools:ro -v "${configPath}:/run/test-config.json:ro" --entrypoint /tools/credentials reonvr-fresh-app:snapshot |
    docker run --rm -i --network reonvr-fresh_default -e REONVR_E2E_ALLOW_MUTATIONS=1 -e REONVR_E2E_GUARD_URL=http://reonvr-e2e-pause-guard:8099 -e REONVR_E2E_URL=http://reonvr-fresh-app:8080 -e REONVR_TEST_CONFIG=/run/test-config.json -v "${configPath}:/run/test-config.json:ro" -v "${root}:/src" -v reonvr-playwright-modules:/src/web/node_modules -w /src/web mcr.microsoft.com/playwright:v1.65.0-noble npm run test:e2e:full
  if ($LASTEXITCODE -ne 0) { throw 'Full acceptance tests failed; recording pause and cleanup are being verified' }
} finally {
  try {
    if ($guardStarted) {
      docker run --rm --network reonvr-fresh_default node:26-alpine node --input-type=module -e $pauseNow
      if ($LASTEXITCODE -ne 0) { throw "Could not confirm recording pause; $guard is left running to retry. Check the fresh deployment immediately" }
      $pauseConfirmed = $true
    }
  } finally {
    if ($guardStarted -and $pauseConfirmed) { docker rm -f $guard 2>$null | Out-Null }
    Pop-Location
  }
}