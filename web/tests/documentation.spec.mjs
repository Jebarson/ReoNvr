import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, mkdtemp, readFile, rm, stat } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { extname, resolve, sep } from 'node:path'
import test from 'node:test'
import { chromium } from 'playwright'
import { parseSync, Visitor } from 'rolldown/utils'
import { captureMaskedScreenshot, liveCameraScreenshotSelector } from './screenshot.mjs'

const maskedSelector = '.cameraName small, .discoveryList small, input[name=host], input[name=username], input[name=password], .cameraSnapshot, img, video, canvas'
const timestamp = '2026-01-15T11:30:00Z'
const capabilities = {
  mainStream: '3840x2160 H.265', subStream: '640x360 H.264',
  mainBitrateKbps: 8192, subBitrateKbps: 512,
  preRecordSeconds: 5, postRecordSeconds: 15, storage: 'Camera storage available',
  storageAvailable: true, storagePlaybackAvailable: true, rtspAvailable: true,
  continuousAvailable: true, supportedEventTypes: ['Motion', 'Person', 'Vehicle', 'Animal'],
}
const cameras = [
  { id: 'demo-entrance', name: 'Front entrance', host: '192.0.2.10', recordingMode: 'Events', recordingCount: 128 },
  { id: 'demo-driveway', name: 'Driveway', host: '192.0.2.20', recordingMode: 'Continuous', recordingCount: 64 },
].map((camera) => ({
  port: 9000, channel: 0, timezone: 'UTC', username: '', enabled: true,
  archiveEnabled: true, archiveStream: 'Main', recordingSource: 'Rtsp',
  recoveryEnabled: true, recoveryStatus: 'Monitoring', recoveryPendingCount: 0,
  recoveryMissingSeconds: 0, historicalImport: 'NewOnly', preEventSeconds: 5,
  postEventSeconds: 15, eventTypes: ['Person', 'Vehicle'], model: 'Demo Reolink',
  firmware: 'Demo', capabilities, connectionStatus: 'Online', credentialConfigured: true,
  lastTestedAtUtc: timestamp, pendingDownloadCount: 0, ...camera,
}))

async function captureMasked(page, output, name, minimumMasks) {
  await page.evaluate(async () => { await document.fonts.ready; window.scrollTo(0, 0) })
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false)
  const mask = page.locator(maskedSelector)
  const regions = await mask.evaluateAll((elements) => elements.map((element) => {
    const bounds = element.getBoundingClientRect()
    return { x: bounds.x + scrollX, y: bounds.y + scrollY, width: bounds.width, height: bounds.height }
  }).filter((bounds) => bounds.width > 0 && bounds.height > 0))
  assert.ok(regions.length >= minimumMasks, 'Expected sensitive areas must be present before capture')
  await captureMaskedScreenshot(page, {
    path: resolve(output, `${name}.png`), fullPage: true, animations: 'disabled',
    mask: [mask], maskColor: '#202020',
  })
}

test('discovery scan cleanup matches its Promise shape', async () => {
  const source = await readFile(new URL('./recovery.spec.mjs', import.meta.url), 'utf8')
  const syntax = parseSync('recovery.spec.mjs', source)
  const promises = new Set()
  new Visitor({
    VariableDeclarator(node) {
      if (node.init?.type === 'NewExpression' && node.init.callee.name === 'Promise') promises.add(node.id.name)
    },
    MemberExpression(node) {
      if (promises.has(node.object.name)) {
        assert.ok(!['resolve', 'promise'].includes(node.property.name), 'Promise cleanup must use its captured resolver')
      }
    },
  }).visit(syntax.program)
})

test('recovery screenshots cannot bypass verified masking', async () => {
  for (const path of ['./recovery.spec.mjs', '../e2e/recovery.spec.mjs']) {
    let source
    try {
      source = await readFile(new URL(path, import.meta.url), 'utf8')
    } catch (error) {
      if (path.startsWith('../e2e/') && error.code === 'ENOENT') continue
      throw error
    }
    const syntax = parseSync(path, source)
    assert.deepEqual(syntax.errors, [])
    let verifiedCaptures = 0
    let screenshotCalls = 0
    new Visitor({
      CallExpression(node) {
        const callee = node.callee
        assert.ok(!(callee.type === 'MemberExpression' && (callee.property.name === 'screenshot' || callee.property.value === 'screenshot')), `${path} contains an unverified screenshot call`)
        if (callee.type === 'Identifier' && callee.name === 'captureMaskedScreenshot') verifiedCaptures += 1
        if (callee.type === 'Identifier' && callee.name === 'screenshot') {
          assert.equal(node.arguments[0]?.name, 'page')
          screenshotCalls += 1
        }
      },
    }).visit(syntax.program)
    assert.equal(verifiedCaptures, 1, `${path} must use the verified capture helper`)
    assert.equal(screenshotCalls, 6, `${path} must mask every recovery capture`)
  }
})

test('live screenshot masks hide camera inputs, selectors and unconfigured discoveries', { timeout: 30000 }, async () => {
  const browser = await chromium.launch({ channel: 'chromium', headless: true })
  try {
    const page = await browser.newPage({ viewport: { width: 800, height: 600 } })
    await page.setContent('<label>Name<input value="private-camera" required></label><input name="host" value="192.0.2.55"><input name="username" value="private-user"><input name="password" value="private-password"><label>Camera<select><option>private-camera</option></select></label><section aria-label="Detected cameras"><p>Unconfigured camera 192.0.2.99</p></section>')
    assert.equal(await page.getByLabel('Name', { exact: true }).getAttribute('name'), null, 'Mirror the actual unnamed camera Name input')
    const controls = page.locator(liveCameraScreenshotSelector)
    assert.equal(await controls.count(), 5, 'Every private form and camera select control must be masked')
    await captureMaskedScreenshot(page, { fullPage: true, mask: [controls, page.getByRole('region', { name: 'Detected cameras', exact: true })] })
    const temporary = await mkdtemp(resolve(tmpdir(), 'reonvr-privacy-'))
    try {
      const accepted = resolve(temporary, 'fresh', 'nested', 'capture.png')
      const image = await captureMaskedScreenshot(page, { path: accepted, fullPage: true, mask: [controls] })
      assert.deepEqual(await readFile(accepted), image)
      const output = resolve(temporary, 'rejected', 'capture.png')
      const screenshot = page.screenshot.bind(page)
      page.screenshot = (options) => screenshot({ ...options, mask: [] })
      await assert.rejects(captureMaskedScreenshot(page, { path: output, fullPage: true, mask: [controls] }), /solid opaque pixels/)
      await assert.rejects(readFile(output), { code: 'ENOENT' })
      await assert.rejects(stat(resolve(temporary, 'rejected')), { code: 'ENOENT' })
    } finally {
      await rm(temporary, { recursive: true, force: true })
    }
  } finally {
    await browser.close()
  }
})

test('manual screenshots use demo data and opaque address, credential and media masks', { timeout: 60000 }, async () => {
  const buildRoot = resolve('dist')
  const contentTypes = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' }
  const server = createServer(async (request, response) => {
    const pathname = new URL(request.url, 'http://localhost').pathname
    const file = resolve(buildRoot, `.${pathname === '/' ? '/index.html' : pathname}`)
    if (!file.startsWith(buildRoot + sep)) { response.writeHead(403).end(); return }
    try {
      const body = await readFile(file)
      response.writeHead(200, { 'Content-Type': contentTypes[extname(file)] ?? 'application/octet-stream' }).end(body)
    } catch { response.writeHead(404).end() }
  })
  await new Promise((ready) => server.listen(0, '127.0.0.1', ready))
  let browser
  try {
    browser = await chromium.launch({ channel: 'chromium', headless: true })
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: 'en-US', timezoneId: 'UTC', serviceWorkers: 'block' })
    const page = await context.newPage()
    const snapshotDataUrl = await page.evaluate(() => {
      const canvas = document.createElement('canvas')
      canvas.width = 640
      canvas.height = 360
      canvas.getContext('2d').fillRect(0, 0, 640, 360)
      return canvas.toDataURL()
    })
    const fixtures = new Map([
      ['GET /api/cameras', cameras],
      ['GET /api/system/recording', { enabled: false, updatedAtUtc: timestamp }],
      ['GET /api/system/settings', { archiveRoot: '/archive', recordingFolder: '.', archiveLayout: 'CameraDate', updatedAtUtc: timestamp }],
      ['POST /api/cameras/discover', [
        { name: 'Front entrance', host: '192.0.2.10', port: 9000, registered: true },
        { name: 'Driveway', host: '192.0.2.20', port: 9000, registered: true },
        { name: 'Garden', host: '192.0.2.30', port: 9000, registered: false },
      ]],
      ['POST /api/cameras/test', { ...capabilities, testId: 'demo-test', model: 'Demo Reolink', firmware: 'Demo', timezone: 'UTC', timezoneOffsetSeconds: 0, snapshotDataUrl }],
    ])
    const origin = `http://127.0.0.1:${server.address().port}`
    const unexpected = []
    const errors = []
    page.on('pageerror', (error) => errors.push(error.message))
    await context.route('**/*', async (route) => {
      const url = new URL(route.request().url())
      if (url.origin !== origin) { unexpected.push('External request blocked'); await route.abort(); return }
      if (!url.pathname.startsWith('/api/')) { await route.continue(); return }
      const key = `${route.request().method()} ${url.pathname}`
      if (!fixtures.has(key)) { unexpected.push(key); await route.abort(); return }
      await route.fulfill({ json: fixtures.get(key) })
    })
    const output = resolve(process.env.REONVR_DOCS_OUTPUT ?? '../runtime/reports/documentation')
    await mkdir(output, { recursive: true })
    await page.goto(origin)
    await page.getByText('Front entrance', { exact: true }).waitFor()
    assert.equal(await page.getByRole('switch', { name: 'Record all cameras' }).isChecked(), false)
    await captureMasked(page, output, 'cameras', 2)
    await page.getByRole('button', { name: 'Detect Automatically', exact: true }).click()
    await page.getByRole('button', { name: 'Add Garden', exact: true }).waitFor()
    await captureMasked(page, output, 'automatic-detection', 5)
    await page.getByRole('button', { name: 'Add Garden', exact: true }).click()
    assert.equal(await page.locator('input[name=host]').inputValue(), '192.0.2.30')
    assert.equal(await page.locator('input[name=username]').inputValue(), '')
    assert.equal(await page.locator('input[name=password]').inputValue(), '')
    await page.locator('input[name=username]').fill('demo-user')
    await page.getByRole('button', { name: 'Test connection', exact: true }).click()
    await page.getByText('Connected', { exact: true }).waitFor()
    await page.getByRole('img').waitFor()
    await captureMasked(page, output, 'camera-setup', 5)
    assert.deepEqual(unexpected, [])
    assert.deepEqual(errors, [])
  } finally {
    await browser?.close()
    await new Promise((closed) => server.close(closed))
  }
})