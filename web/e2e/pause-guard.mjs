import { createServer } from 'node:http'
import { pathToFileURL } from 'node:url'

export function createPauseGuard({ recordingURL, now = Date.now, request = fetch, leaseMilliseconds = 300000 }) {
  let deadline = null
  let inFlight
  const status = () => ({ armed: deadline !== null, expired: deadline !== null && now() >= deadline, remainingMilliseconds: deadline === null ? 0 : Math.max(0, deadline - now()) })
  const tick = async () => {
    if (!status().expired) return false
    if (inFlight) return inFlight
    inFlight = (async () => {
      try {
        const response = await request(recordingURL, {
          method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: false }), signal: AbortSignal.timeout(5000),
        })
        return response.ok && (await response.json()).enabled === false
      } catch {
        return false
      }
    })()
    try { return await inFlight } finally { inFlight = undefined }
  }
  const server = createServer(async (incoming, response) => {
    const send = (code, body) => { response.writeHead(code, { 'Content-Type': 'application/json' }); response.end(JSON.stringify(body)) }
    if (incoming.method === 'GET' && incoming.url === '/status') return send(200, status())
    if (incoming.method === 'POST' && incoming.url === '/arm') {
      if (deadline !== null) return send(409, status())
      deadline = now() + leaseMilliseconds
      return send(201, status())
    }
    if (incoming.method === 'POST' && incoming.url === '/pause') {
      deadline = now()
      const paused = await tick()
      return send(paused ? 200 : 503, { enabled: paused ? false : null })
    }
    send(404, {})
  })
  const timer = setInterval(() => { void tick() }, 1000)
  server.once('close', () => clearInterval(timer))
  return { server, tick }
}

export async function armPauseGuard(url) {
  const armed = await fetch(new URL('/arm', url), { method: 'POST', signal: AbortSignal.timeout(5000) })
  if (armed.status !== 201) throw new Error('Independent pause guard could not be armed')
  const response = await fetch(new URL('/status', url), { signal: AbortSignal.timeout(5000) })
  const status = await response.json()
  if (!response.ok || !status.armed || status.expired || status.remainingMilliseconds < 180000 || status.remainingMilliseconds > 300000) {
    throw new Error('Independent pause guard is not protecting the recording window')
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  createPauseGuard({ recordingURL: 'http://reonvr-fresh-app:8080/api/system/recording' }).server.listen(8099, '0.0.0.0')
}