import assert from 'node:assert/strict'
import { createHash, randomBytes } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { after, before, test } from 'node:test'
import { chromium, request } from 'playwright'

const baseURL = process.env.REONVR_E2E_URL ?? 'http://reonvr-fresh-app:8080'
const target = new URL(baseURL)
const allowedHosts = ['reonvr-fresh-app', 'reonvr-e2e-app']
let credentials
let configuration
let probeCamera
let storageCamera
let api
let browser
let baseline
let originalSettings
let completedClip
const created = new Map()

async function json(endpoint) {
  const response = await api.get(endpoint)
  assert.equal(response.status(), 200, endpoint)
  return response.json()
}

async function waitForValue(read, ready, timeout = 90000) {
  return new Promise((resolve, reject) => {
    let active = false
    const cleanup = () => { clearInterval(interval); clearTimeout(deadline) }
    const interval = setInterval(async () => {
      if (active) return
      active = true
      try {
        const value = await read()
        if (ready(value)) { cleanup(); resolve(value) }
      } catch (error) {
        cleanup()
        reject(error)
      } finally {
        active = false
      }
    }, 500)
    const deadline = setTimeout(() => { cleanup(); reject(new Error('Timed out waiting for application state')) }, timeout)
  })
}

async function withPage(width, action) {
  const context = await browser.newContext({ viewport: { width, height: 900 } })
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', (error) => errors.push(error.message.replaceAll(credentials.password, '[redacted]')))
  try {
    await page.goto(baseURL)
    await page.getByRole('heading', { name: 'Cameras', exact: true }).waitFor()
    await action(page)
    assert.deepEqual(errors, [], 'No uncaught browser errors')
  } finally {
    await context.close()
  }
}

async function layout(page, name) {
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'No horizontal overflow')
  assert.equal(await page.locator('.navItem').evaluateAll((buttons) => buttons.some((button) => {
    const box = button.getBoundingClientRect()
    return box.left < 0 || box.right > innerWidth + 1 || button.scrollWidth > button.clientWidth + 1
  })), false, 'Navigation labels fit')
  assert.equal(await page.locator('.mediaStage').evaluateAll((elements) => elements.some((element) => {
    const box = element.getBoundingClientRect()
    const parent = element.parentElement.getBoundingClientRect()
    return box.left < parent.left || box.right > parent.right + 1
  })), false, 'Player fits inside its column')
  assert.equal(await page.locator('.mediaStage strong, .mediaStage span').evaluateAll((elements) => elements.some((element) => {
    const box = element.getBoundingClientRect()
    const parent = element.parentElement.getBoundingClientRect()
    return box.left < parent.left || box.right > parent.right + 1 || element.scrollWidth > element.clientWidth + 1
  })), false, 'Player status text fits inside the player')
  assert.equal(await page.evaluate(() => document.body.textContent.includes('Camera/')), false, 'Internal clock rules stay hidden')
  await page.screenshot({ path: fileURLToPath(new URL(`../../runtime/e2e-${name}.png`, import.meta.url)), fullPage: true, animations: 'disabled', mask: [page.locator('input[name=password]')] })
}

function existingDraft(camera, changes = {}) {
  return { ...camera, password: '', ...changes }
}

function cameraDraft(camera, name = `E2E ${camera.id}`) {
  return {
    name, host: camera.host, port: camera.port, channel: camera.channel, timezone: '',
    username: credentials.username, password: credentials.password,
    enabled: true, archiveEnabled: false, archiveStream: 'Main', recordingSource: 'Rtsp',
    recoveryEnabled: false, recordingMode: 'Events', historicalImport: 'NewOnly',
    preEventSeconds: 10, postEventSeconds: 15, eventTypes: ['Motion'],
  }
}

before(async () => {
  assert.equal(process.env.REONVR_E2E_ALLOW_MUTATIONS, '1', 'Explicit acceptance-test permission is required')
  assert.ok(allowedHosts.includes(target.hostname), 'Use a fresh/disposable container, never the original deployment')
  try {
    configuration = JSON.parse(readFileSync(0, 'utf8'))
  } catch {
    throw new Error('Supply validated local test configuration over stdin through run.ps1')
  }
  credentials = configuration
  assert.ok(credentials?.username && credentials?.password, 'Secure test credential handoff is required')
  assert.ok(Array.isArray(configuration.cameras) && configuration.cameras.length >= 2, 'Configure at least two test cameras')
  probeCamera = configuration.cameras.find((camera) => camera.id === configuration.probeCamera)
  storageCamera = configuration.cameras.find((camera) => camera.id === configuration.storageCamera)
  assert.ok(probeCamera && storageCamera, 'Configure probeCamera and storageCamera IDs')
  assert.ok(configuration.cameras.every((camera) => camera.channel === 0), 'Browser registration currently supports channel zero; live Go tests also support configured NVR channels')
  api = await request.newContext({ baseURL, timeout: 30000 })
  assert.equal((await api.get('/api/health/ready')).status(), 200)
  assert.equal((await (await api.get('/api/system/recording')).json()).enabled, false)
  baseline = await (await api.get('/api/cameras')).json()
  assert.equal(baseline.length, 0, 'Full live acceptance requires an empty fresh instance to avoid changing operator cameras')
  originalSettings = await json('/api/system/settings')
  browser = await chromium.launch({ channel: process.env.REONVR_E2E_CHANNEL ?? 'chromium', headless: true })
})

after(async () => {
  await browser?.close()
  if (api) {
    await api.put('/api/system/recording', { data: { enabled: false } })
    for (const id of created.keys()) {
      assert.equal((await api.delete(`/api/cameras/${id}`)).status(), 204, 'Clean up only test-created cameras')
    }
    if (originalSettings) {
      assert.equal((await api.put('/api/system/settings', { data: originalSettings })).status(), 200)
    }
    assert.equal((await (await api.get('/api/system/recording')).json()).enabled, false)
    const cameras = await (await api.get('/api/cameras')).json()
    assert.deepEqual(cameras.map((camera) => camera.id).sort(), baseline.map((camera) => camera.id).sort())
    await api.dispose()
  }
  credentials = undefined
  configuration = undefined
})

test('live health, workers and operational issues are readable while paused', async () => {
  for (const endpoint of ['/api/health/live', '/api/health/ready', '/api/system/workers', '/api/system/issues', '/api/system/settings']) {
    assert.equal((await api.get(endpoint)).status(), 200, endpoint)
  }
})

test('configured camera authentication, native streams, clock rules and snapshot', { timeout: 45000 }, async () => {
  const response = await api.post('/api/cameras/test', { data: cameraDraft(probeCamera) })
  assert.equal(response.status(), 200, 'Configured probe camera connection test must succeed')
  const result = await response.json()
  assert.ok(result.model)
  assert.equal(result.rtspAvailable, true)
  assert.ok(result.mainStream)
  assert.ok(result.testId)
  assert.ok(result.timezone)
  assert.match(result.snapshotDataUrl, /^data:image\/jpeg;base64,/)
  assert.equal(JSON.stringify(result).includes(credentials.password), false, 'API must not expose the password')
})

test('real discovery and configured cameras register through the browser', { timeout: 180000 }, async () => {
  await withPage(1440, async (page) => {
    await page.getByText('No cameras registered', { exact: true }).waitFor()
    const scan = page.waitForResponse((response) => response.url().endsWith('/api/cameras/discover'))
    await page.getByRole('button', { name: 'Detect Automatically', exact: true }).click()
    const discovery = await scan
    assert.equal(discovery.status(), 200)
    const devices = await discovery.json()
    assert.deepEqual(configuration.cameras.filter((camera) => !devices.some((device) => device.host === camera.host)).map((camera) => camera.id), [], 'All configured cameras are discoverable')
    await page.getByRole('button', { name: 'Dismiss detection results', exact: true }).click()
    for (const configured of configuration.cameras) {
      await page.getByRole('button', { name: 'Add camera', exact: true }).first().click()
      assert.equal(await page.getByRole('button', { name: 'Save camera', exact: true }).isDisabled(), true)
      await page.getByLabel('Name', { exact: true }).fill(`E2E ${configured.id}`)
      await page.getByLabel('IP address or hostname', { exact: true }).fill(configured.host)
      await page.getByLabel('Baichuan port', { exact: true }).fill(String(configured.port))
      await page.getByLabel('Username', { exact: true }).fill(credentials.username)
      await page.locator('input[name=password]').fill(credentials.password)
      const tested = page.waitForResponse((response) => response.url().endsWith('/api/cameras/test'))
      await page.getByRole('button', { name: 'Test connection', exact: true }).click()
      const response = await tested
      assert.equal(response.status(), 200, `Live test ${configured.id}`)
      const capabilities = await response.json()
      assert.equal(capabilities.rtspAvailable, true)
      assert.equal(JSON.stringify(capabilities).includes(credentials.password), false)
      await page.locator('.cameraSnapshot img').waitFor()
      assert.equal(await page.locator('.cameraSnapshot img').evaluate((image) => image.complete && image.naturalWidth > 0), true)
      await page.getByRole('checkbox', { name: /^Record this camera/ }).uncheck()
      const saved = page.waitForResponse((result) => result.url().endsWith('/api/cameras') && result.request().method() === 'POST')
      await page.getByRole('button', { name: 'Save camera', exact: true }).click()
      const savedResponse = await saved
      assert.equal(savedResponse.status(), 201)
      const camera = await savedResponse.json()
      created.set(camera.id, camera)
      assert.equal(camera.recordingSource, 'Rtsp')
      assert.equal(camera.credentialConfigured, true)
      await page.getByRole('heading', { name: 'Cameras', exact: true }).waitFor()
    }
    await page.reload()
    assert.equal((await json('/api/cameras')).length, configuration.cameras.length)
    await layout(page, 'registered-desktop')
  })
})

test('edit policy, retest stream changes, recovery, enable and disable persist', { timeout: 90000 }, async () => {
  const camera = [...created.values()].find((item) => item.host === storageCamera.host)
  assert.ok(camera)
  await withPage(1440, async (page) => {
    const row = () => page.getByRole('row').filter({ has: page.getByText(camera.name, { exact: true }) })
    await row().getByTitle('Disable camera', { exact: true }).click()
    await row().getByTitle('Enable camera', { exact: true }).waitFor()
    assert.equal((await json(`/api/cameras/${camera.id}`)).enabled, false)
    await row().getByTitle('Enable camera', { exact: true }).click()
    await row().getByTitle('Disable camera', { exact: true }).waitFor()
    await row().getByTitle('Edit camera', { exact: true }).click()
    assert.equal(await page.locator('input[name=password]').inputValue(), '')
    await page.getByRole('checkbox', { name: /^Record this camera/ }).check()
    await page.getByRole('radio', { name: /^Sub stream/ }).click()
    await page.getByText('Test the connection to read camera capabilities.', { exact: true }).waitFor()
    const tested = page.waitForResponse((response) => response.url().endsWith(`/api/cameras/${camera.id}/test`))
    await page.getByRole('button', { name: 'Test connection', exact: true }).click()
    assert.equal((await tested).status(), 200)
    await page.getByRole('radio', { name: /^Continuous/ }).check()
    const recovery = page.getByRole('checkbox', { name: /^Recover missed recordings/ })
    assert.equal(await recovery.isEnabled(), true)
    await recovery.check()
    const saved = page.waitForResponse((response) => response.url().endsWith(`/api/cameras/${camera.id}`) && response.request().method() === 'PUT')
    await page.getByRole('button', { name: 'Save camera', exact: true }).click()
    assert.equal((await saved).status(), 200)
    const current = await json(`/api/cameras/${camera.id}`)
    assert.equal(current.archiveStream, 'Sub')
    assert.equal(current.recordingMode, 'Continuous')
    assert.equal(current.recoveryEnabled, true)
    await page.reload()
    await row().getByRole('button', { name: 'Recovery paused', exact: true }).click()
    await page.getByRole('heading', { name: 'Recovery', exact: true }).waitFor()
    await page.getByText('No recovery intervals', { exact: true }).waitFor()
    await layout(page, 'recovery-empty-desktop')
    assert.equal((await api.put(`/api/cameras/${camera.id}`, { data: existingDraft(current, { archiveEnabled: false, recoveryEnabled: false }) })).status(), 200)
  })
})

test('all storage layouts persist and unsafe paths are rejected', { timeout: 45000 }, async () => {
  await withPage(1440, async (page) => {
    await page.getByRole('button', { name: 'Settings', exact: true }).click()
    for (const archiveLayout of ['CameraDate', 'DateCamera', 'Camera', 'Date']) {
      await page.getByLabel('Recording folder', { exact: true }).fill('e2e-acceptance')
      await page.getByLabel('Folder organization').selectOption(archiveLayout)
      const saved = page.waitForResponse((response) => response.url().endsWith('/api/system/settings') && response.request().method() === 'PUT')
      await page.getByRole('button', { name: 'Save settings', exact: true }).click()
      assert.equal((await saved).status(), 200)
      assert.equal((await json('/api/system/settings')).archiveLayout, archiveLayout)
    }
    await page.getByLabel('Recording folder', { exact: true }).fill('../outside')
    const rejected = page.waitForResponse((response) => response.url().endsWith('/api/system/settings') && response.request().method() === 'PUT')
    await page.getByRole('button', { name: 'Save settings', exact: true }).click()
    assert.equal((await rejected).status(), 400)
    assert.equal((await json('/api/system/settings')).recordingFolder, 'e2e-acceptance')
  })
})

for (const width of [1440, 768, 390, 320]) {
  test(`responsive camera, playback, settings and edit views at ${width}px`, { timeout: 45000 }, async () => {
    await withPage(width, async (page) => {
      await page.getByText(`E2E ${probeCamera.id}`, { exact: true }).waitFor()
      await layout(page, `cameras-${width}`)
      await page.getByRole('button', { name: 'Playback', exact: true }).click()
      await page.getByText('No local recordings for this date', { exact: true }).waitFor()
      await layout(page, `playback-empty-${width}`)
      await page.getByRole('button', { name: 'Settings', exact: true }).click()
      await layout(page, `settings-${width}`)
      await page.getByRole('button', { name: 'Cameras', exact: true }).first().click()
      await page.getByTitle('Edit camera', { exact: true }).first().click()
      await layout(page, `edit-${width}`)
      const password = page.locator('input[name=password]')
      await page.getByTitle('Show password', { exact: true }).click()
      assert.equal(await password.getAttribute('type'), 'text')
      await page.getByTitle('Hide password', { exact: true }).click()
      assert.equal(await password.getAttribute('type'), 'password')
      await page.getByRole('button', { name: 'Cancel', exact: true }).click()
    })
  })
}

test('invalid API requests cannot bypass testing, leak credentials or escape storage', async () => {
  const withoutTest = await api.post('/api/cameras', { data: cameraDraft(probeCamera) })
  assert.ok(withoutTest.status() >= 400 && withoutTest.status() < 500)
  assert.equal((await withoutTest.text()).includes(credentials.password), false)
  const publicHost = await api.post('/api/cameras/test', { data: cameraDraft({ ...probeCamera, host: '8.8.8.8' }) })
  assert.equal(publicHost.status(), 400)
  assert.equal((await api.get('/api/cameras/nonexistent')).status(), 404)
  assert.equal((await api.get('/api/recordings/nonexistent/media')).status(), 404)
  const malformed = await api.put('/api/system/recording', { data: {} })
  assert.equal(malformed.status(), 400)
  assert.equal((await json('/api/system/recording')).enabled, false)
})

test('existing discovery and recovery regressions run against temporary cameras', { timeout: 120000 }, async () => {
  const originals = []
  try {
    for (const id of created.keys()) {
      const camera = await json(`/api/cameras/${id}`)
      originals.push(camera)
      assert.equal((await api.put(`/api/cameras/${id}`, { data: existingDraft(camera, { archiveEnabled: true }) })).status(), 200)
    }
    const output = execFileSync(process.execPath, ['--test', '--test-concurrency=1', 'e2e/recovery.spec.mjs'], {
      env: { ...process.env, REONVR_E2E_URL: baseURL, REONVR_E2E_CHANNEL: 'chromium' },
      encoding: 'utf8', timeout: 100000,
    })
    console.log(output.replaceAll(credentials.password, '[redacted]'))
  } finally {
    for (const camera of originals) {
      assert.equal((await api.put(`/api/cameras/${camera.id}`, { data: existingDraft(camera) })).status(), 200)
    }
  }
})

test('camera removal cancel and confirm affect only the selected test camera', { timeout: 30000 }, async () => {
  const camera = [...created.values()].find((item) => item.host !== storageCamera.host)
  await withPage(1440, async (page) => {
    const row = () => page.getByRole('row').filter({ has: page.getByText(camera.name, { exact: true }) })
    page.once('dialog', (dialog) => dialog.dismiss())
    await row().getByTitle('Remove camera', { exact: true }).click()
    assert.equal((await api.get(`/api/cameras/${camera.id}`)).status(), 200)
    page.once('dialog', (dialog) => dialog.accept())
    const removed = page.waitForResponse((response) => response.url().endsWith(`/api/cameras/${camera.id}`) && response.request().method() === 'DELETE')
    await row().getByTitle('Remove camera', { exact: true }).click()
    assert.equal((await removed).status(), 204)
    created.delete(camera.id)
    await row().waitFor({ state: 'detached' })
    assert.equal((await api.get(`/api/cameras/${camera.id}`)).status(), 404)
  })
})

test('bounded RTSP recording, pause, original download, byte ranges, playback and seeking', { timeout: 180000 }, async () => {
  assert.equal(process.env.REONVR_E2E_RECORDING_GUARD, '1', 'Run through run.ps1 so an independent pause guard is active')
  const camera = [...created.values()].find((item) => item.host === storageCamera.host)
  const current = await json(`/api/cameras/${camera.id}`)
  assert.equal((await api.put(`/api/cameras/${camera.id}`, { data: existingDraft(current, { archiveEnabled: true, recoveryEnabled: false, recordingMode: 'Continuous' }) })).status(), 200)
  let clip
  try {
    await withPage(1440, async (page) => {
      const resumed = page.waitForResponse((response) => response.url().endsWith('/api/system/recording') && response.request().method() === 'PUT')
      await page.getByRole('switch', { name: 'Record all cameras', exact: true }).click()
      assert.equal((await resumed).status(), 200)
      assert.equal((await json('/api/system/recording')).enabled, true)
      const usable = (item) => item.state === 'Complete' && Date.parse(item.endAtUtc) - Date.parse(item.startAtUtc) >= 50000
      const recordings = await waitForValue(() => json(`/api/cameras/${camera.id}/recordings`), (items) => items.some(usable), 150000)
      clip = recordings.find(usable)
      completedClip = clip
      assert.equal(clip.source, 'Rtsp')
      assert.equal(clip.stream, 'Sub')
      assert.ok(clip.sizeBytes > 0)
      const paused = page.waitForResponse((response) => response.url().endsWith('/api/system/recording') && response.request().method() === 'PUT')
      await page.getByRole('switch', { name: 'Record all cameras', exact: true }).click()
      assert.equal((await paused).status(), 200)
      await waitForValue(() => json('/api/system/workers'), (workers) => !workers.some((worker) => worker.stage !== 'Stopped' && /RtspRecorder|LiveEvent|Recovery|Download/.test(worker.workerName)), 15000)
      assert.equal((await json('/api/system/recording')).enabled, false)
      await page.getByTitle(`Open ${camera.name} playback`, { exact: true }).click()
      await page.getByLabel('Date', { exact: true }).fill('')
      const video = page.locator('video')
      await video.waitFor()
      for (let index = 0; index < await page.locator('.recordingRow').count(); index++) {
        await page.locator('.recordingRow').nth(index).click()
        if ((await video.getAttribute('src')).includes(clip.id)) break
      }
      assert.equal((await video.getAttribute('src')).includes(clip.id), true, 'Select the verified clip, not a short pause-flush segment')
      await page.waitForFunction(() => {
        const element = document.querySelector('video')
        return element && element.readyState >= 1 && Number.isFinite(element.duration) && element.duration > 0
      }, undefined, { timeout: 20000 })
      await video.evaluate((element) => { element.muted = true; return element.play() })
      await page.waitForFunction(() => document.querySelector('video')?.currentTime > 0.2, undefined, { timeout: 10000 })
      const seekTarget = await video.evaluate((element) => { element.pause(); return Math.min(1, element.duration / 2) })
      await video.evaluate((element, target) => { element.currentTime = target }, seekTarget)
      await page.waitForFunction((target) => {
        const element = document.querySelector('video')
        return element && !element.seeking && Math.abs(element.currentTime - target) < 0.2
      }, seekTarget, { timeout: 10000 })
      await layout(page, 'playback-live-desktop')
    })
    const ranged = await api.get(`/api/recordings/${clip.id}/media`, { headers: { Range: 'bytes=0-127' } })
    assert.equal(ranged.status(), 206)
    assert.equal((await ranged.body()).length, 128)
    assert.match(ranged.headers()['content-range'], /^bytes 0-127\//)
    const original = await api.get(`/api/recordings/${clip.id}/original`)
    assert.equal(original.status(), 200)
    assert.match(original.headers()['content-disposition'], /^attachment;/)
    const media = await api.get(`/api/recordings/${clip.id}/media`)
    assert.equal(media.status(), 200)
    assert.equal(createHash('sha256').update(await original.body()).digest('hex'), createHash('sha256').update(await media.body()).digest('hex'), 'Original download preserves native file bytes')
    assert.equal((await api.get(`/api/recordings/${clip.id}/media`, { headers: { Range: 'bytes=999999999999-' } })).status(), 416)
    assert.equal((await json(`/api/recordings/${clip.id}`)).state, 'Complete')
  } finally {
    await api.put('/api/system/recording', { data: { enabled: false } })
    await api.put(`/api/cameras/${camera.id}`, { data: existingDraft(current) })
  }
})

for (const width of [1440, 390, 320]) {
  test(`playback states, filters, codec fallback, connection errors and clock warnings at ${width}px`, { timeout: 60000 }, async () => {
    assert.ok(completedClip)
    await withPage(width, async (page) => {
      const fixture = { ...completedClip, id: 'fixture-playback', eventTypes: ['Person'], startAtUtc: new Date().toISOString() }
      let state = 'Pending'
      let multiple = false
      await page.route((url) => /\/api\/cameras\/[^/]+\/recordings$/.test(url.pathname), (route) => route.fulfill({ json: multiple
        ? [{ ...fixture, state }, { ...fixture, id: 'fixture-motion', state, eventTypes: ['Motion'] }]
        : [{ ...fixture, state }],
      }))
      await page.route('**/api/recordings/fixture-*/media*', (route) => route.fulfill({ status: 200, contentType: 'video/mp4', body: 'unsupported-native-codec-fixture' }))
      await page.getByRole('button', { name: 'Playback', exact: true }).click()
      await page.getByLabel('Date', { exact: true }).fill('')
      const states = [
        ['Pending', 'Native media pending'], ['Discovered', 'Metadata only'],
        ['Downloading', 'Archiving native media'], ['Verifying', 'Archiving native media'],
        ['Failed', 'Native media unavailable'], ['Expired', 'Local copy removed'], ['Deleted', 'Local copy removed'],
      ]
      for (const [nextState, heading] of states) {
        state = nextState
        const refreshed = page.waitForResponse((response) => /\/api\/cameras\/[^/]+\/recordings$/.test(new URL(response.url()).pathname))
        await page.getByRole('button', { name: 'Refresh', exact: true }).click()
        await refreshed
        await page.locator('.mediaStage').getByText(heading, { exact: true }).waitFor()
        await page.locator('.detailActions').getByText(state, { exact: true }).waitFor()
        assert.equal(await page.locator('video').count(), 0)
      }
      state = 'Complete'
      multiple = true
      await page.getByRole('button', { name: 'Refresh', exact: true }).click()
      await page.getByText('Browser cannot play this native codec', { exact: true }).waitFor()
      assert.equal(await page.locator('.recordingRow').count(), 2)
      await page.locator('.filterButtons').getByRole('button', { name: 'Person', exact: true }).click()
      assert.equal(await page.locator('.recordingRow').count(), 1)
      await page.locator('.filterButtons').getByRole('button', { name: 'All', exact: true }).click()
      assert.equal(await page.locator('.recordingRow').count(), 2)
      await page.getByLabel('Date', { exact: true }).fill('1999-01-01')
      await page.getByText('No local recordings for this date', { exact: true }).waitFor()
      await page.getByLabel('Date', { exact: true }).fill('')
      await layout(page, `playback-fallback-${width}`)
      await page.getByRole('button', { name: 'Cameras', exact: true }).first().click()
      await page.getByRole('button', { name: 'Add camera', exact: true }).first().click()
      await page.getByLabel('Name', { exact: true }).fill('E2E error fixture')
      await page.getByLabel('IP address or hostname', { exact: true }).fill(probeCamera.host)
      await page.getByLabel('Username', { exact: true }).fill(randomBytes(12).toString('hex'))
      await page.locator('input[name=password]').fill(randomBytes(24).toString('hex'))
      let failure = { status: 502, message: 'Unable to connect to the camera. Verify its address, Baichuan port, and credentials.' }
      let clockWarning
      await page.route('**/api/cameras/test', (route) => failure
        ? route.fulfill({ status: failure.status, json: { code: 'FixtureError', message: failure.message } })
        : route.fulfill({ json: { testId: 'fixture', model: 'Doorbell', firmware: 'test', timezone: 'Camera/fixture', timezoneOffsetSeconds: -28800,
          clockWarning, mainStream: '2560x1920', subStream: '640x480', rtspAvailable: true, storageAvailable: true, storagePlaybackAvailable: true,
          preRecordSeconds: 10, postRecordSeconds: 15, supportedEventTypes: ['Motion', 'Person'],
        } }))
      for (const status of [502, 504, 409]) {
        failure = { status, message: `Camera test fixture ${status}. Check the address and retry.` }
        await page.getByRole('button', { name: 'Test connection', exact: true }).click()
        await page.getByText(failure.message, { exact: true }).waitFor()
        assert.equal(await page.getByRole('button', { name: 'Save camera', exact: true }).isDisabled(), true)
      }
      failure = null
      clockWarning = "Your camera's time doesn't match your machine. ReoNvr will use the camera's clock."
      await page.getByRole('button', { name: 'Test connection', exact: true }).click()
      await page.getByRole('status').filter({ hasText: clockWarning }).waitFor()
      assert.equal(await page.getByRole('button', { name: 'Save camera', exact: true }).isEnabled(), true)
      await layout(page, `clock-warning-${width}`)
      clockWarning = undefined
      await page.getByRole('button', { name: 'Test connection', exact: true }).click()
      await page.locator('.alert[role=status]').waitFor({ state: 'detached' })
      assert.equal(await page.getByRole('button', { name: 'Save camera', exact: true }).isEnabled(), true)
    })
  })
}