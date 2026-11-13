import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { chromium, request } from 'playwright'

const baseURL = process.env.REONVR_E2E_URL ?? 'http://localhost:8080'
const defaults = JSON.parse(readFileSync(new URL('../../internal/testconfig/config.example.json', import.meta.url), 'utf8'))
const configuration = process.env.REONVR_TEST_CONFIG ? JSON.parse(readFileSync(process.env.REONVR_TEST_CONFIG, 'utf8')) : defaults
const discoveryHost = configuration.fixtures?.hosts?.[2] ?? defaults.fixtures.hosts[2]
let browser
let api
let cameras

before(async () => {
  api = await request.newContext({ baseURL })
  assert.equal((await (await api.get('/api/system/recording')).json()).enabled, false, 'Run browser validation with recording paused')
  cameras = await (await api.get('/api/cameras')).json()
  assert.ok(cameras.length > 0, 'A registered camera is required for policy persistence checks')
  browser = await chromium.launch({ channel: process.env.REONVR_E2E_CHANNEL ?? 'chrome', headless: true })
})

after(async () => {
  if (api) assert.equal((await (await api.get('/api/system/recording')).json()).enabled, false)
  await browser?.close()
  await api?.dispose()
})

function draft(camera, recoveryEnabled = camera.recoveryEnabled) {
  return {
    name: camera.name, host: camera.host, port: camera.port, channel: camera.channel,
    timezone: camera.timezone, username: camera.username, password: '', enabled: camera.enabled,
    archiveEnabled: camera.archiveEnabled, archiveStream: camera.archiveStream,
    recordingSource: 'Rtsp', recordingMode: camera.recordingMode, historicalImport: 'NewOnly',
    recoveryEnabled, preEventSeconds: camera.preEventSeconds, postEventSeconds: camera.postEventSeconds,
    eventTypes: camera.eventTypes,
  }
}

async function checkLayout(page) {
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth), false, 'Page must not overflow horizontally')
  const clippedNavigation = await page.locator('.navItem').evaluateAll((buttons) => buttons.some((button) => {
    const bounds = button.getBoundingClientRect()
    return bounds.left < 0 || bounds.right > window.innerWidth + 1 || button.scrollWidth > button.clientWidth + 1
  }))
  assert.equal(clippedNavigation, false, 'Navigation labels must fit within the viewport')
}

function screenshot(name) {
  return fileURLToPath(new URL(`../../runtime/${name}.png`, import.meta.url))
}

test('real automatic detection is read-only and available beside Add camera', { timeout: 30000 }, async () => {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const page = await context.newPage()
  try {
    await page.goto(baseURL)
    const detection = page.getByRole('button', { name: 'Detect Automatically', exact: true })
    await detection.waitFor()
    const responsePromise = page.waitForResponse((response) => response.url().endsWith('/api/cameras/discover') && response.request().method() === 'POST')
    await detection.click()
    const response = await responsePromise
    assert.equal(response.status(), 200, await response.text())
    const devices = await response.json()
    assert.ok(Array.isArray(devices))
    if (devices.length === 0) await page.getByText('No cameras replied', { exact: true }).waitFor()
    assert.equal(await detection.isEnabled(), true)
    const add = page.locator('.pageActions').getByRole('button', { name: 'Add camera', exact: true })
    const detectBox = await detection.boundingBox()
    const addBox = await add.boundingBox()
    assert.equal(detectBox.y, addBox.y, 'Detection and manual add must share a toolbar row on desktop')
    const current = await (await api.get('/api/cameras')).json()
    assert.equal(current.length, cameras.length)
    assert.equal(current.reduce((total, camera) => total + camera.recordingCount, 0), cameras.reduce((total, camera) => total + camera.recordingCount, 0))
    await checkLayout(page)
    await page.screenshot({ path: screenshot('detection-live-desktop'), fullPage: true })
  } finally {
    await context.close()
  }
})

for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }, { width: 320, height: 740 }]) {
  test(`automatic detection results, prefill, empty and retry at ${viewport.width}px`, { timeout: 45000 }, async () => {
    const context = await browser.newContext({ viewport })
    const page = await context.newPage()
    const errors = []
    const writes = []
    const firstScan = Promise.withResolvers()
    let attempts = 0
    page.on('pageerror', (error) => errors.push(error.message))
    page.on('request', (request) => {
      if (request.method() !== 'GET' && !request.url().endsWith('/api/cameras/discover')) writes.push(request.url())
    })
    await page.route((url) => url.pathname === '/api/cameras', (route) => route.fulfill({ json: [cameras[0]] }))
    await page.route((url) => url.pathname === '/api/cameras/discover', async (route) => {
      attempts += 1
      if (attempts === 1) {
        await firstScan.promise
        await route.fulfill({ json: [
          { host: cameras[0].host, port: cameras[0].port, name: 'Existing camera', registered: true },
          { host: discoveryHost, port: 9001, name: 'Garden camera', registered: false },
        ] })
      } else if (attempts === 3) {
        await route.fulfill({ status: 502, json: { code: 'DiscoveryFailed', message: 'Camera detection failed. Check local network access and try again.' } })
      } else {
        await route.fulfill({ json: [] })
      }
    })
    try {
      await page.goto(baseURL)
      const detect = page.getByRole('button', { name: 'Detect Automatically', exact: true })
      await detect.click()
      await page.getByRole('status').filter({ hasText: 'Scanning local network...' }).waitFor()
      assert.equal(await detect.isDisabled(), true)
      firstScan.resolve()
      const results = page.getByRole('region', { name: 'Detected cameras', exact: true })
      await results.getByText('Garden camera', { exact: true }).waitFor()
      assert.equal(await results.getByText('Already added', { exact: true }).count(), 1)
      assert.equal(await results.getByRole('button', { name: 'Add Existing camera', exact: true }).count(), 0)
      await checkLayout(page)
      await page.screenshot({ path: screenshot(`detection-results-${viewport.width}`), fullPage: true })
      await results.getByRole('button', { name: 'Add Garden camera', exact: true }).click()
      assert.equal(await page.getByLabel('Name', { exact: true }).inputValue(), 'Garden camera')
      assert.equal(await page.getByLabel('IP address or hostname', { exact: true }).inputValue(), discoveryHost)
      assert.equal(await page.getByLabel('Baichuan port', { exact: true }).inputValue(), '9001')
      assert.equal(await page.locator('input[name="password"]').inputValue(), '')
      assert.equal(await page.getByRole('button', { name: 'Save camera', exact: true }).isDisabled(), true)
      await page.getByRole('button', { name: 'Cancel', exact: true }).click()
      await detect.click()
      await page.getByText('No cameras replied', { exact: true }).waitFor()
      await checkLayout(page)
      await page.screenshot({ path: screenshot(`detection-empty-${viewport.width}`), fullPage: true })
      await detect.click()
      await page.getByRole('alert').filter({ hasText: 'Camera detection failed.' }).waitFor()
      await results.getByRole('button', { name: 'Retry', exact: true }).click()
      await page.getByText('No cameras replied', { exact: true }).waitFor()
      assert.equal(attempts, 4)
      await page.getByRole('button', { name: 'Dismiss detection results', exact: true }).click()
      assert.equal(await results.count(), 0)
      assert.equal(await detect.isEnabled(), true)
      assert.deepEqual(writes, [])
      assert.deepEqual(errors, [])
    } finally {
      firstScan.resolve()
      await context.close()
    }
  })
}

test('real camera policy persists without losing recordings or enabling recording', { timeout: 90000 }, async () => {
  const original = cameras.find((camera) => camera.capabilities?.storageAvailable && camera.capabilities?.rtspAvailable)
  assert.ok(original, 'A camera with storage and RTSP is required')
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', (error) => errors.push(error.message))
  page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()) })
  let changed = false
  try {
    await page.goto(baseURL)
    const row = page.getByRole('row').filter({ has: page.getByText(original.name, { exact: true }) })
    await row.getByTitle('Edit camera', { exact: true }).click()
    const tested = page.waitForResponse((response) => response.url().endsWith(`/api/cameras/${original.id}/test`) && response.request().method() === 'POST')
    await page.getByRole('button', { name: 'Test connection', exact: true }).click()
    const testResponse = await tested
    assert.equal(testResponse.status(), 200)
    assert.equal((await testResponse.json()).storagePlaybackAvailable, true)
    const recovery = page.getByRole('checkbox', { name: /^Recover missed recordings/ })
    await recovery.waitFor()
    assert.equal(await page.getByText('Recording source', { exact: true }).count(), 0)
    assert.equal(await page.getByRole('radio', { name: /^Continuous/ }).isEnabled(), true)
    await recovery.setChecked(!original.recoveryEnabled)
    const saved = page.waitForResponse((response) => response.url().endsWith(`/api/cameras/${original.id}`) && response.request().method() === 'PUT')
    await page.getByRole('button', { name: 'Save camera', exact: true }).click()
    const savedResponse = await saved
    assert.equal(savedResponse.status(), 200, await savedResponse.text())
    changed = true
    const savedCamera = await savedResponse.json()
    assert.equal(savedCamera.recordingSource, 'Rtsp')
    assert.equal(savedCamera.recoveryEnabled, !original.recoveryEnabled)
    await page.getByRole('heading', { name: 'Cameras', exact: true }).waitFor()
    await page.reload()
    const updated = (await (await api.get('/api/cameras')).json()).find((camera) => camera.id === original.id)
    assert.equal(updated.recordingCount, original.recordingCount)
    await page.getByRole('row').filter({ has: page.getByText(original.name, { exact: true }) }).getByTitle('Edit camera', { exact: true }).click()
    assert.equal(await page.getByRole('checkbox', { name: /^Recover missed recordings/ }).isChecked(), !original.recoveryEnabled)
    await checkLayout(page)
    await page.screenshot({ path: screenshot('recovery-settings-desktop'), fullPage: true })
    assert.deepEqual(errors, [])
  } finally {
    if (changed) {
      const restored = await api.put(`/api/cameras/${original.id}`, { data: draft(original) })
      assert.equal(restored.status(), 200, await restored.text())
    }
    await context.close()
  }
})

for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
  test(`unavailable storage and recovery details at ${viewport.width}px`, { timeout: 45000 }, async () => {
    const context = await browser.newContext({ viewport })
    const page = await context.newPage()
    const errors = []
    page.on('pageerror', (error) => errors.push(error.message))
    const camera = {
      ...cameras[0], recoveryEnabled: true, recoveryStatus: 'Pending', recoveryPendingCount: 2,
      capabilities: { ...cameras[0].capabilities, rtspAvailable: true, storageAvailable: false, storagePlaybackAvailable: false },
    }
    await page.route((url) => url.pathname === '/api/cameras', (route) => route.fulfill({ json: [camera] }))
    const start = '2026-11-11T10:00:00Z'
    await page.route((url) => url.pathname === `/api/cameras/${camera.id}/recovery`, (route) => route.fulfill({ json: [
      { id: 'partial', cameraId: camera.id, stream: 'Main', mode: 'Continuous', startAtUtc: start, endAtUtc: '2026-11-11T10:03:00Z', status: 'Partial', missingSeconds: 37, lastCheckedAtUtc: start },
      { id: 'unknown', cameraId: camera.id, stream: 'Main', mode: 'Events', startAtUtc: start, endAtUtc: '2026-11-11T10:04:00Z', status: 'Unknown', missingSeconds: 240 },
      { id: 'events', cameraId: camera.id, stream: 'Main', mode: 'Events', startAtUtc: start, endAtUtc: '2026-11-11T10:05:00Z', status: 'RecoveredEvents', missingSeconds: 0 },
    ] }))
    try {
      await page.goto(baseURL)
      await page.getByRole('button', { name: 'Recovery: 2 pending', exact: true }).click()
      await page.getByRole('cell', { name: /^(Status )?Partial$/ }).waitFor()
      assert.equal(await page.getByRole('cell', { name: /^(Unresolved )?37s$/ }).count(), 1)
      assert.equal(await page.getByRole('cell', { name: /^(Status )?Unknown$/ }).count(), 1)
      assert.equal(await page.getByRole('cell', { name: /^(Status )?Event clips recovered$/ }).count(), 1)
      await checkLayout(page)
      await page.screenshot({ path: screenshot(`recovery-history-${viewport.width}`), fullPage: true })
      await page.getByRole('main').getByRole('button', { name: 'Cameras', exact: true }).click()
      await page.getByTitle('Edit camera', { exact: true }).click()
      const recovery = page.getByRole('checkbox', { name: /^Recover missed recordings/ })
      await recovery.uncheck()
      assert.equal(await recovery.isDisabled(), true)
      assert.equal(await page.getByRole('radio', { name: /^Continuous/ }).isEnabled(), true)
      await checkLayout(page)
      await page.screenshot({ path: screenshot(`recovery-unavailable-${viewport.width}`), fullPage: true })
      assert.deepEqual(errors, [])
    } finally {
      await context.close()
    }
  })
}