export type EventType = 'Motion' | 'Person' | 'Vehicle' | 'Animal'
export type ArchiveStream = 'Main' | 'Sub'
export type RecordingSource = 'CameraStorage' | 'Rtsp'
export type RecordingMode = 'Events' | 'Continuous'
export type HistoricalImport = 'NewOnly' | 'Last24Hours' | 'Last48Hours' | 'Last7Days' | 'AllAvailable'

export interface CameraCapabilities {
  mainStream: string
  subStream: string
  mainBitrateKbps: number
  subBitrateKbps: number
  preRecordSeconds: number
  postRecordSeconds: number
  storage: string
  storageAvailable: boolean
  rtspAvailable: boolean
  continuousAvailable: boolean
  supportedEventTypes: EventType[]
}

export interface Camera {
  id: string
  name: string
  host: string
  port: number
  channel: number
  timezone: string
  username: string
  enabled: boolean
  archiveEnabled: boolean
  archiveStream: ArchiveStream
  recordingSource: RecordingSource
  recordingMode: RecordingMode
  historicalImport: HistoricalImport
  preEventSeconds: number
  postEventSeconds: number
  eventTypes: EventType[]
  model?: string
  firmware?: string
  capabilities?: CameraCapabilities
  connectionStatus: 'Online' | 'Offline' | 'Degraded' | 'Testing'
  credentialConfigured: boolean
  lastTestedAtUtc?: string
  lastEventAtUtc?: string
  recordingCount: number
  pendingDownloadCount: number
}

export interface CameraDraft {
  name: string
  host: string
  port: number
  channel: number
  timezone: string
  username: string
  password: string
  enabled: boolean
  archiveEnabled: boolean
  archiveStream: ArchiveStream
  recordingSource: RecordingSource
  recordingMode: RecordingMode
  historicalImport: HistoricalImport
  preEventSeconds: number
  postEventSeconds: number
  eventTypes: EventType[]
}

export interface CameraTestResult extends CameraCapabilities {
  testId: string
  model: string
  firmware: string
  timezone: string
  timezoneOffsetSeconds: number
  snapshotDataUrl?: string
}

export interface RecordingControl {
  enabled: boolean
  updatedAtUtc: string
}

interface ApiErrorBody {
  message?: string
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...init?.headers },
  })

  if (!response.ok) {
    let body: ApiErrorBody = {}
    try {
      body = await response.json() as ApiErrorBody
    } catch {
      body = {}
    }
    throw new Error(body.message || `Request failed with status ${response.status}.`)
  }

  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}

export function listCameras() {
  return request<Camera[]>('/api/cameras')
}

export function getRecordingControl() {
  return request<RecordingControl>('/api/system/recording')
}

export function updateRecordingControl(enabled: boolean) {
  return request<RecordingControl>('/api/system/recording', { method: 'PUT', body: JSON.stringify({ enabled }) })
}

export function testCamera(camera: CameraDraft, cameraId?: string) {
  const path = cameraId ? `/api/cameras/${cameraId}/test` : '/api/cameras/test'
  return request<CameraTestResult>(path, { method: 'POST', body: JSON.stringify(camera) })
}

export function createCamera(camera: CameraDraft, testId: string) {
  return request<Camera>('/api/cameras', { method: 'POST', body: JSON.stringify({ ...camera, testId }) })
}

export function updateCamera(cameraId: string, camera: CameraDraft, testId?: string) {
  return request<Camera>(`/api/cameras/${cameraId}`, { method: 'PUT', body: JSON.stringify({ ...camera, testId }) })
}

export function deleteCamera(cameraId: string) {
  return request<void>(`/api/cameras/${cameraId}`, { method: 'DELETE' })
}

export type RecordingState = 'Discovered' | 'Queued' | 'Recording' | 'Downloading' | 'Verifying' | 'Complete' | 'RetryWait' | 'Failed' | 'Expired' | 'Deleted'

export interface Recording {
  id: string
  cameraId: string
  nativeName: string
  source: RecordingSource
  stream: ArchiveStream
  codec?: string
  startAtUtc: string
  endAtUtc: string
  sizeBytes: number
  eventTypes: EventType[]
  state: RecordingState
  discoveredAtUtc: string
  updatedAtUtc: string
}

export function listRecordings(cameraId: string) {
  return request<Recording[]>(`/api/cameras/${cameraId}/recordings`)
}

export function recordingMediaUrl(recordingId: string) {
  return `/api/recordings/${encodeURIComponent(recordingId)}/media`
}

export function recordingOriginalUrl(recordingId: string) {
  return `/api/recordings/${encodeURIComponent(recordingId)}/original`
}