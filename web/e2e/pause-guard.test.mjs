import assert from 'node:assert/strict'
import { once } from 'node:events'
import { createServer } from 'node:http'
import { test } from 'node:test'
import { armPauseGuard, createPauseGuard } from './pause-guard.mjs'

async function listen(server, context) {
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  context.after(() => new Promise((resolve) => server.close(resolve)))
  return `http://127.0.0.1:${server.address().port}`
}

test('slow registration cannot consume the capture lease and expired guards keep retrying pause', async (context) => {
  let now = 0
  let attempts = 0
  const guard = createPauseGuard({ recordingURL: 'http://test.invalid/recording', now: () => now, request: async (_url, options) => {
    assert.equal(options.method, 'PUT')
    assert.deepEqual(JSON.parse(options.body), { enabled: false })
    attempts++
    if (attempts === 1) throw new Error('Application temporarily unavailable')
    return Response.json({ enabled: attempts === 2 })
  } })
  const url = await listen(guard.server, context)
  now += 900000
  await guard.tick()
  assert.equal(attempts, 0)
  await armPauseGuard(url)
  now += 299999
  await guard.tick()
  assert.equal(attempts, 0)
  now++
  assert.equal(await guard.tick(), false)
  assert.equal(await guard.tick(), false, 'An HTTP success without paused state is not confirmation')
  assert.equal(await guard.tick(), true)
  assert.equal(await guard.tick(), true, 'Keep enforcing pause until the orchestrator removes the guard')
  assert.equal(attempts, 4)
  await assert.rejects(armPauseGuard(url), /could not be armed/)
})

test('failed immediate cleanup leaves the guard armed for later retries', async (context) => {
  let available = false
  const guard = createPauseGuard({ recordingURL: 'http://test.invalid/recording', request: async () => {
    if (!available) throw new Error('Application temporarily unavailable')
    return Response.json({ enabled: false })
  } })
  const url = await listen(guard.server, context)
  assert.equal((await fetch(`${url}/pause`, { method: 'POST' })).status, 503)
  const status = await (await fetch(`${url}/status`)).json()
  assert.equal(status.armed, true)
  assert.equal(status.expired, true)
  available = true
  assert.equal(await guard.tick(), true)
  const response = await fetch(`${url}/pause`, { method: 'POST' })
  assert.equal(response.status, 200)
  assert.deepEqual(await response.json(), { enabled: false })
})

test('recording refuses an unverified guard instead of trusting an environment flag', async (context) => {
  const server = createServer((incoming, response) => {
    response.writeHead(incoming.url === '/arm' ? 201 : 200, { 'Content-Type': 'application/json' })
    response.end(JSON.stringify({ armed: false, expired: true, remainingMilliseconds: 0 }))
  })
  const url = await listen(server, context)
  await assert.rejects(armPauseGuard(url), /not protecting/)
})