import assert from 'node:assert/strict'
import { mkdir } from 'node:fs/promises'
import test from 'node:test'
import { chromium, request } from 'playwright'

test('downloaded deployment is healthy, usable, responsive, and paused', { timeout: 90000 }, async () => {
  assert.ok(process.env.REONVR_TEST_URL, 'Set REONVR_TEST_URL to an isolated empty test deployment')
  const api = await request.newContext({ baseURL: process.env.REONVR_TEST_URL, timeout: 10000 })
  let browser
  let emptyConfirmed = false
  try {
    const cameras = await api.get('/api/cameras')
    assert.equal(cameras.status(), 200)
    assert.deepEqual(await cameras.json(), [], 'Only an empty deployment may be smoke-tested')
    emptyConfirmed = true
    const pause = await api.put('/api/system/recording', { data: { enabled: false } })
    assert.equal(pause.status(), 200)
    assert.equal((await pause.json()).enabled, false)
    assert.equal((await api.get('/api/health/ready')).status(), 200)
    browser = await chromium.launch({ channel: 'chromium', headless: true })
    await mkdir('../runtime/reports', { recursive: true })
    for (const width of [1440, 390, 320]) {
      const context = await browser.newContext({ viewport: { width, height: 900 } })
      const page = await context.newPage()
      const errors = []
      page.on('pageerror', (error) => errors.push(error.message))
      await page.goto(process.env.REONVR_TEST_URL)
      await page.getByRole('heading', { name: 'Cameras', exact: true }).waitFor()
      assert.equal(await page.getByRole('switch', { name: 'Record all cameras', exact: true }).isChecked(), false)
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false)
      await page.getByRole('button', { name: 'Add camera', exact: true }).first().click()
      assert.ok((await page.locator('input[name=username]').inputValue()) === '', 'Username must default to blank')
      assert.ok((await page.locator('input[name=password]').inputValue()) === '', 'Password must default to blank')
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false)
      await page.screenshot({ path: `../runtime/reports/smoke-${width}.png`, fullPage: true, mask: [page.locator('input[name=password]')] })
      assert.deepEqual(errors, [])
      await context.close()
    }
    const workers = await (await api.get('/api/system/workers')).json()
    assert.ok(workers.some((worker) => worker.workerName === 'RecordingIntentWorker'))
    assert.equal(workers.filter((worker) => worker.stage !== 'Stopped').filter((worker) => /RtspRecorder|LiveEvent|Recovery|Download/.test(worker.workerName)).length, 0)
  } finally {
    await browser?.close()
    if (emptyConfirmed) {
      const response = await api.put('/api/system/recording', { data: { enabled: false } })
      assert.equal(response.status(), 200)
      assert.equal((await response.json()).enabled, false)
    }
    await api.dispose()
  }
})