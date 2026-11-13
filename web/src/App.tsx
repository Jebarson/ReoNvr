import { useEffect, useRef, useState, type FormEvent } from 'react'
import {
  Activity,
  ArrowLeft,
  Archive,
  Camera as CameraIcon,
  CheckCircle2,
  CircleAlert,
	Clock3,
  Download,
  Eye,
  EyeOff,
  HardDrive,
	ListFilter,
  Pencil,
  Plus,
  RefreshCw,
  Radar,
  Save,
  Server,
	Settings as SettingsIcon,
  ShieldCheck,
  Trash2,
  Video,
  X,
} from 'lucide-react'
import {
  createCamera,
  deleteCamera,
  discoverCameras,
	getApplicationSettings,
  getRecordingControl,
  listCameras,
	listRecoveryGaps,
	listRecordings,
  recordingMediaUrl,
  recordingOriginalUrl,
  testCamera,
  updateCamera,
	updateApplicationSettings,
  updateRecordingControl,
	type ApplicationSettings,
  type ArchiveStream,
	type ArchiveLayout,
  type Camera,
  type CameraDraft,
  type CameraTestResult,
  type DiscoveredCamera,
  type EventType,
	type Recording,
  type RecordingControl,
  type RecoveryGap,
} from './api'
import './App.css'

type Screen = 'Cameras' | 'CameraForm' | 'Playback' | 'Settings' | 'Recovery'

const archiveLayoutLabels: Record<ArchiveLayout, { label: string, example: string }> = {
  CameraDate: { label: 'Camera, then day', example: 'Front-Camera-a35b97be/2026/09/12/recording.mp4' },
  DateCamera: { label: 'Day, then camera', example: '2026/09/12/Front-Camera-a35b97be/recording.mp4' },
  Camera: { label: 'Camera only', example: 'Front-Camera-a35b97be/recording.mp4' },
  Date: { label: 'Day only', example: '2026/09/12/Front-Camera-a35b97be-recording.mp4' },
}

const eventLabels: Record<EventType, string> = {
  Motion: 'Motion',
  Person: 'Person',
  Vehicle: 'Vehicle',
  Animal: 'Animal',
}

const emptyDraft: CameraDraft = {
  name: '',
  host: '',
  port: 9000,
  channel: 0,
  timezone: '',
  username: '',
  password: '',
  enabled: true,
  archiveEnabled: true,
  archiveStream: 'Main',
  recordingSource: 'Rtsp',
  recoveryEnabled: false,
  recordingMode: 'Events',
  historicalImport: 'NewOnly',
  preEventSeconds: 10,
  postEventSeconds: 15,
  eventTypes: ['Motion', 'Person', 'Vehicle', 'Animal'],
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : 'An unexpected error occurred.'
}

function localDateKey(value: string) {
  const date = new Date(value)
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

function formatBytes(bytes: number) {
  if (bytes <= 0) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function formatDuration(start: string, end: string) {
  const seconds = Math.max(0, Math.round((new Date(end).getTime() - new Date(start).getTime()) / 1000))
  if (seconds < 60) return `${seconds}s`
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
}

function unavailableMediaCopy(recording: Recording) {
  switch (recording.state) {
    case 'Discovered':
      return ['Metadata only', 'This historical recording is indexed but is not queued under the current import policy.']
    case 'Downloading':
    case 'Verifying':
      return ['Archiving native media', 'The original recording is being transferred and verified.']
    case 'Failed':
      return ['Native media unavailable', 'The transfer could not be completed; its metadata remains indexed.']
    case 'Expired':
    case 'Deleted':
      return ['Local copy removed', 'The recording metadata remains available after retention removed its media.']
    default:
      return ['Native media pending', 'The original recording is queued for secure local transfer.']
  }
}

function App() {
  const formRef = useRef<HTMLFormElement>(null)
  const [screen, setScreen] = useState<Screen>('Cameras')
  const [cameras, setCameras] = useState<Camera[]>([])
  const [recordingControl, setRecordingControl] = useState<RecordingControl | null>(null)
  const [recordingControlSaving, setRecordingControlSaving] = useState(false)
  const [applicationSettings, setApplicationSettings] = useState<ApplicationSettings | null>(null)
  const [settingsDraft, setSettingsDraft] = useState<Pick<ApplicationSettings, 'recordingFolder' | 'archiveLayout'>>({ recordingFolder: '.', archiveLayout: 'CameraDate' })
  const [settingsSaving, setSettingsSaving] = useState(false)
  const [settingsError, setSettingsError] = useState('')
  const [loading, setLoading] = useState(true)
  const [pageError, setPageError] = useState('')
  const [editing, setEditing] = useState<Camera | null>(null)
  const [draft, setDraft] = useState<CameraDraft>(emptyDraft)
  const [testedValues, setTestedValues] = useState('')
  const [testResult, setTestResult] = useState<CameraTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  const [saving, setSaving] = useState(false)
  const [showPassword, setShowPassword] = useState(false)
  const [formError, setFormError] = useState('')
  const [notice, setNotice] = useState('')
  const [selectedCameraId, setSelectedCameraId] = useState('')
  const [recordings, setRecordings] = useState<Recording[]>([])
  const [recordingsLoading, setRecordingsLoading] = useState(false)
  const [recordingsError, setRecordingsError] = useState('')
  const [recordingDate, setRecordingDate] = useState('')
  const [eventFilters, setEventFilters] = useState<EventType[]>([])
  const [selectedRecordingId, setSelectedRecordingId] = useState('')
	const [failedMediaId, setFailedMediaId] = useState('')
  const [recoveryGaps, setRecoveryGaps] = useState<RecoveryGap[]>([])
  const [recoveryLoading, setRecoveryLoading] = useState(false)
  const [recoveryError, setRecoveryError] = useState('')
  const [discoveryVisible, setDiscoveryVisible] = useState(false)
  const [detecting, setDetecting] = useState(false)
  const [discoveredCameras, setDiscoveredCameras] = useState<DiscoveredCamera[]>([])
  const [discoveryError, setDiscoveryError] = useState('')
  const discoveryController = useRef<AbortController | null>(null)

  useEffect(() => () => {
    discoveryController.current?.abort()
    discoveryController.current = null
  }, [])

  async function handleDiscover() {
    if (discoveryController.current) return
    const controller = new AbortController()
    discoveryController.current = controller
    setDiscoveryVisible(true)
    setDetecting(true)
    setDiscoveryError('')
    setDiscoveredCameras([])
    try {
      const devices = await discoverCameras(controller.signal)
      if (!controller.signal.aborted) setDiscoveredCameras(devices)
    } catch (error) {
      if (!controller.signal.aborted) setDiscoveryError(errorMessage(error))
    } finally {
      if (discoveryController.current === controller) {
        discoveryController.current = null
        setDetecting(false)
      }
    }
  }

  function dismissDiscovery() {
    discoveryController.current?.abort()
    discoveryController.current = null
    setDetecting(false)
    setDiscoveryVisible(false)
  }

  useEffect(() => {
    if (screen !== 'Recovery' || !selectedCameraId) return
    let active = true
    async function refresh() {
      try {
        const gaps = await listRecoveryGaps(selectedCameraId)
        if (active) { setRecoveryGaps(gaps); setRecoveryError('') }
      } catch (error) {
        if (active) setRecoveryError(errorMessage(error))
      } finally {
        if (active) setRecoveryLoading(false)
      }
    }
    void refresh()
    const timer = window.setInterval(() => void refresh(), 10000)
    return () => { active = false; window.clearInterval(timer) }
  }, [screen, selectedCameraId])

  async function refreshCameras() {
    setLoading(true)
    setPageError('')
    try {
      setCameras(await listCameras())
    } catch (error) {
      setPageError(errorMessage(error))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    let active = true
    Promise.all([listCameras(), getRecordingControl(), getApplicationSettings()])
      .then(([items, control, settings]) => {
        if (active) {
          setCameras(items)
          setRecordingControl(control)
		  setApplicationSettings(settings)
		  setSettingsDraft({ recordingFolder: settings.recordingFolder, archiveLayout: settings.archiveLayout })
        }
      })
      .catch((error: unknown) => {
        if (active) setPageError(errorMessage(error))
      })
      .finally(() => {
        if (active) setLoading(false)
      })
    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    if (!cameras.some((camera) => camera.enabled && camera.connectionStatus === 'Testing')) return
    const timer = window.setTimeout(() => {
      listCameras()
        .then((items) => {
          setCameras(items)
          setPageError('')
        })
        .catch((error: unknown) => setPageError(errorMessage(error)))
    }, 1000)
    return () => window.clearTimeout(timer)
  }, [cameras])

  useEffect(() => {
    if (!recordingControl?.enabled) return
    const timer = window.setInterval(() => {
      listCameras()
        .then(setCameras)
        .catch((error: unknown) => setPageError(errorMessage(error)))
    }, 15_000)
    return () => window.clearInterval(timer)
  }, [recordingControl?.enabled])

  useEffect(() => {
    if (!recordingControl?.enabled || screen !== 'Playback' || !selectedCameraId) return
    const timer = window.setInterval(() => {
      listRecordings(selectedCameraId)
        .then((items) => {
          setRecordings(items)
          setSelectedRecordingId((current) => items.some((item) => item.id === current) ? current : (items[0]?.id ?? ''))
        })
        .catch((error: unknown) => setRecordingsError(errorMessage(error)))
    }, 15_000)
    return () => window.clearInterval(timer)
  }, [recordingControl?.enabled, screen, selectedCameraId])

  function connectionFingerprint(value: CameraDraft) {
  return JSON.stringify([value.host.trim(), value.port, value.channel, value.archiveStream, value.username, value.password])
  }

  function openAdd() {
    dismissDiscovery()
    setEditing(null)
    setDraft({ ...emptyDraft, eventTypes: [...emptyDraft.eventTypes] })
    setTestResult(null)
    setTestedValues('')
    setFormError('')
    setShowPassword(false)
    setScreen('CameraForm')
  }

  function openDiscovered(device: DiscoveredCamera) {
    openAdd()
    setDraft((current) => ({ ...current, name: device.name, host: device.host, port: device.port }))
  }

  function openEdit(camera: Camera) {
    const nextDraft: CameraDraft = {
      name: camera.name,
      host: camera.host,
      port: camera.port,
      channel: camera.channel,
      timezone: camera.timezone,
      username: camera.username,
      password: '',
      enabled: camera.enabled,
      archiveEnabled: camera.archiveEnabled,
      archiveStream: camera.archiveStream,
      recordingSource: 'Rtsp',
      recoveryEnabled: camera.recoveryEnabled,
      recordingMode: camera.recordingMode,
      historicalImport: 'NewOnly',
      preEventSeconds: camera.preEventSeconds,
      postEventSeconds: camera.postEventSeconds,
      eventTypes: [...camera.eventTypes],
    }
    setEditing(camera)
    setDraft(nextDraft)
    setTestResult(camera.capabilities ? {
      testId: '',
      model: camera.model ?? 'Unknown',
      firmware: camera.firmware ?? 'Unknown',
      timezone: camera.timezone,
      timezoneOffsetSeconds: 0,
      ...camera.capabilities,
    } : null)
    setTestedValues('')
    setFormError('')
    setShowPassword(false)
    setScreen('CameraForm')
  }

  function updateDraft<K extends keyof CameraDraft>(key: K, value: CameraDraft[K]) {
    setDraft((current) => ({ ...current, [key]: value }))
    if (['host', 'port', 'channel', 'archiveStream', 'username', 'password'].includes(key)) {
      setTestResult(null)
      setTestedValues('')
    }
  }

  function toggleEvent(eventType: EventType) {
    setDraft((current) => ({
      ...current,
      eventTypes: current.eventTypes.includes(eventType)
        ? current.eventTypes.filter((item) => item !== eventType)
        : [...current.eventTypes, eventType],
    }))
  }

  async function handleTest() {
    const fields = formRef.current ? new FormData(formRef.current) : null
    const submittedDraft: CameraDraft = fields ? {
      ...draft,
      host: String(fields.get('host') ?? ''),
      port: Number(fields.get('port')),
      username: String(fields.get('username') ?? ''),
      password: String(fields.get('password') ?? ''),
    } : draft
    setTesting(true)
    setFormError('')
    try {
      const result = await testCamera(submittedDraft, editing?.id)
      setTestResult(result)
      setTestedValues(connectionFingerprint(submittedDraft))
      setDraft({
        ...submittedDraft,
        timezone: result.timezone,
        recordingSource: 'Rtsp',
        eventTypes: submittedDraft.eventTypes.filter((eventType) => result.supportedEventTypes.includes(eventType)),
      })
      setNotice(`Connected to ${result.model}`)
    } catch (error) {
      setTestResult(null)
      setTestedValues('')
      setFormError(errorMessage(error))
    } finally {
      setTesting(false)
    }
  }

  async function handleSave(event: FormEvent) {
    event.preventDefault()
    setFormError('')
    const changedConnection = !editing
      || draft.host !== editing.host
      || draft.port !== editing.port
      || draft.channel !== editing.channel
      || draft.archiveStream !== editing.archiveStream
      || draft.username !== editing.username
      || draft.password.length > 0

    if (changedConnection && (!testResult?.testId || testedValues !== connectionFingerprint(draft))) {
      setFormError('Test this connection before saving it.')
      return
    }

    setSaving(true)
    try {
      if (editing) {
        await updateCamera(editing.id, draft, testResult?.testId)
        setNotice('Camera updated')
      } else {
        await createCamera(draft, testResult!.testId)
        setNotice('Camera registered')
      }
      setScreen('Cameras')
      await refreshCameras()
    } catch (error) {
      setFormError(errorMessage(error))
    } finally {
      setSaving(false)
    }
  }

  async function handleToggle(camera: Camera) {
    try {
      await updateCamera(camera.id, { ...camera, password: '', enabled: !camera.enabled })
      setNotice(camera.enabled ? 'Camera disabled' : 'Camera enabled')
      await refreshCameras()
    } catch (error) {
      setPageError(errorMessage(error))
    }
  }

  async function handleRecordingControl(enabled: boolean) {
    setRecordingControlSaving(true)
    setPageError('')
    try {
      setRecordingControl(await updateRecordingControl(enabled))
      setNotice(enabled ? 'Recording resumed' : 'Recording paused')
    } catch (error) {
      setPageError(errorMessage(error))
    } finally {
      setRecordingControlSaving(false)
    }
  }

  async function handleApplicationSettings(event: FormEvent) {
    event.preventDefault()
    setSettingsSaving(true)
    setSettingsError('')
    try {
      const settings = await updateApplicationSettings(settingsDraft)
      setApplicationSettings(settings)
      setSettingsDraft({ recordingFolder: settings.recordingFolder, archiveLayout: settings.archiveLayout })
      setNotice('Storage settings saved')
    } catch (error) {
      setSettingsError(errorMessage(error))
    } finally {
      setSettingsSaving(false)
    }
  }

  async function handleDelete(camera: Camera) {
    if (!window.confirm(`Remove ${camera.name}? Existing archive ownership will be preserved.`)) return
    try {
      await deleteCamera(camera.id)
      setNotice('Camera removed')
      await refreshCameras()
    } catch (error) {
      setPageError(errorMessage(error))
    }
  }

  async function refreshRecordings(cameraId: string, resetDate = false) {
    setRecordingsLoading(true)
    setRecordingsError('')
    try {
      const items = await listRecordings(cameraId)
      setRecordings(items)
      setSelectedRecordingId((current) => items.some((item) => item.id === current) ? current : (items[0]?.id ?? ''))
      if (resetDate) setRecordingDate(items[0] ? localDateKey(items[0].startAtUtc) : '')
    } catch (error) {
      setRecordingsError(errorMessage(error))
    } finally {
      setRecordingsLoading(false)
    }
  }

  function openPlayback(requestedCameraId?: string) {
    const cameraId = requestedCameraId && cameras.some((camera) => camera.id === requestedCameraId)
      ? requestedCameraId
      : cameras.some((camera) => camera.id === selectedCameraId)
      ? selectedCameraId
      : (cameras.find((camera) => camera.recordingCount > 0) ?? cameras[0])?.id ?? ''
    setSelectedCameraId(cameraId)
    setRecordingDate(localDateKey(new Date().toISOString()))
    setScreen('Playback')
    if (cameraId) void refreshRecordings(cameraId)
  }

  function selectPlaybackCamera(cameraId: string) {
    setSelectedCameraId(cameraId)
    setEventFilters([])
    void refreshRecordings(cameraId)
  }

  const connectionIsCurrent = testedValues === connectionFingerprint(draft)
  const supportedEvents: EventType[] = testResult?.supportedEventTypes ?? []
  const sourceAvailable = !draft.archiveEnabled || Boolean(testResult?.rtspAvailable)
  const storageReady = Boolean(testResult?.storageAvailable && testResult?.storagePlaybackAvailable)
  const recoveryAvailable = !draft.archiveEnabled || !draft.recoveryEnabled || storageReady
  const eventSelectionValid = !draft.archiveEnabled || draft.recordingMode === 'Continuous' || draft.eventTypes.length > 0
  const recordingPolicyValid = sourceAvailable && recoveryAvailable && eventSelectionValid
  const filteredRecordings = recordings.filter((recording) =>
    (!recordingDate || localDateKey(recording.startAtUtc) === recordingDate)
    && (eventFilters.length === 0 || recording.eventTypes.some((eventType) => eventFilters.includes(eventType))))
  const selectedRecording = filteredRecordings.find((recording) => recording.id === selectedRecordingId) ?? filteredRecordings[0]
  const selectedCamera = cameras.find((camera) => camera.id === selectedCameraId)
  const mediaError = selectedRecording?.id === failedMediaId
  const [unavailableMediaHeading, unavailableMediaDescription] = selectedRecording
    ? unavailableMediaCopy(selectedRecording)
    : ['', '']

  function togglePlaybackEvent(eventType: EventType) {
    setEventFilters((current) => current.includes(eventType)
      ? current.filter((item) => item !== eventType)
      : [...current, eventType])
  }

  return (
    <div className="appShell">
      <aside className="sidebar">
        <div className="brand"><span className="brandMark"><Archive /></span>ReoNvr</div>
        <div className="navLabel">Workspace</div>
        <button className={`navItem ${screen === 'Cameras' || screen === 'CameraForm' || screen === 'Recovery' ? 'active' : ''}`} onClick={() => setScreen('Cameras')}><CameraIcon /> Cameras</button>
        <button className={`navItem ${screen === 'Playback' ? 'active' : ''}`} onClick={() => openPlayback()}><Video /> Playback</button>
        <button className={`navItem ${screen === 'Settings' ? 'active' : ''}`} onClick={() => setScreen('Settings')}><SettingsIcon /> Settings</button>
        <div className="systemState">
          <div><span className={`statusDot ${pageError ? 'warning' : ''}`} />{pageError ? 'Service needs attention' : 'Archive service ready'}</div>
          <div><ShieldCheck /> Credentials encrypted locally</div>
        </div>
      </aside>

      <div className="workspace">
        <header className="topbar">
      <strong>{screen === 'Cameras' ? 'Cameras' : screen === 'Playback' ? 'Playback' : screen === 'Settings' ? 'Settings' : screen === 'Recovery' ? 'Recovery' : editing ? 'Edit camera' : 'Add camera'}</strong>
          <span><HardDrive /> Automatic 30 minute reserve</span>
        </header>

        <main>
		  {screen === 'Cameras' ? (
            <section className="screen">
              <div className="pageHeading">
                <div><h1>Cameras</h1><p>Connections and archive policies</p></div>
                <div className="pageActions">
                  <div className={`masterRecording ${recordingControl?.enabled === false ? 'paused' : ''}`}>
                    <span><strong>Recording</strong><small>{recordingControl ? (recordingControl.enabled ? 'On' : 'Paused') : 'Loading'}</small></span>
                    <button
                      type="button"
                      className="recordingSwitch"
                      role="switch"
                      aria-label="Record all cameras"
                      aria-checked={recordingControl?.enabled ?? false}
                      disabled={!recordingControl || recordingControlSaving}
                      onClick={() => void handleRecordingControl(!recordingControl?.enabled)}
                    ><span className="recordingSwitchThumb" /></button>
                  </div>
                  <button className="button detectButton" disabled={detecting} aria-busy={detecting} onClick={() => void handleDiscover()}>{detecting ? <RefreshCw className="spin" /> : <Radar />}<span>Detect Automatically</span></button>
                  <button className="button primary" onClick={openAdd}><Plus /> Add camera</button>
                </div>
              </div>

              {pageError && <div className="alert error"><CircleAlert /> <span>{pageError}</span><button className="button" onClick={() => void refreshCameras()}><RefreshCw /> Retry</button></div>}
              {notice && <div className="alert success"><CheckCircle2 /> {notice}</div>}

              {discoveryVisible && <section className="discoverySection" aria-labelledby="discovery-heading" aria-busy={detecting}>
                <div className="discoveryHeading"><h2 id="discovery-heading">Detected cameras</h2><button className="iconButton" title="Dismiss detection results" aria-label="Dismiss detection results" onClick={dismissDiscovery}><X /></button></div>
                {detecting ? <div className="discoveryMessage" role="status"><RefreshCw className="spin" /><span>Scanning local network...</span></div> : discoveryError ? <div className="alert error" role="alert"><CircleAlert /><span>{discoveryError}</span><button className="button" onClick={() => void handleDiscover()}><RefreshCw /> Retry</button></div> : discoveredCameras.length === 0 ? <div className="discoveryMessage" role="status"><Radar /><div><strong>No cameras replied</strong><p>LAN broadcasts may be blocked by Docker networking, VLANs, or a firewall. Manual address entry is still available.</p></div></div> : <ul className="discoveryList">
                  {discoveredCameras.map((device) => <li key={`${device.host}:${device.port}`}><div><strong>{device.name || 'Reolink camera'}</strong><small>{device.host.includes(':') ? `[${device.host}]` : device.host}:{device.port}</small></div>{device.registered || cameras.some((camera) => camera.host === device.host && camera.port === device.port) ? <span className="discoveryRegistered"><CheckCircle2 /> Already added</span> : <button className="button" aria-label={`Add ${device.name || device.host}`} onClick={() => openDiscovered(device)}><Plus /> Add camera</button>}</li>)}
                </ul>}
              </section>}

              <div className="summaryStrip panel">
                <div><span>Configured</span><strong>{cameras.length}</strong></div>
                <div><span>Enabled</span><strong>{cameras.filter((camera) => camera.enabled).length}</strong></div>
                <div><span>Online</span><strong>{cameras.filter((camera) => camera.connectionStatus === 'Online').length}</strong></div>
              </div>

              <div className="panel cameraPanel">
                {loading ? (
                  <div className="empty"><RefreshCw className="spin" /><h2>Loading cameras</h2></div>
                ) : cameras.length === 0 ? (
                  <div className="empty"><span className="emptyIcon"><CameraIcon /></span><h2>No cameras registered</h2><p>Connect the first Reolink camera to begin.</p><button className="button primary" onClick={openAdd}><Plus /> Add camera</button></div>
                ) : (
                  <div className="tableScroll"><table><thead><tr><th>Camera</th><th>Status</th><th>Archive</th><th>Events</th><th>Last tested</th><th>Actions</th></tr></thead><tbody>
                    {cameras.map((camera) => <tr key={camera.id}>
                      <td><div className="cameraName"><button className="cameraGlyph" title={`Open ${camera.name} playback`} onClick={() => openPlayback(camera.id)}><Video /></button><div><strong>{camera.name}</strong><small>{camera.model ?? 'Reolink camera'} · {camera.host}</small></div></div></td>
                      <td data-label="Status"><button className={`statusBadge ${camera.connectionStatus.toLowerCase()}`} onClick={() => void handleToggle(camera)} title={camera.enabled ? 'Disable camera' : 'Enable camera'}>{camera.enabled ? camera.connectionStatus : 'Disabled'}</button></td>
                      <td data-label="Archive">{camera.archiveEnabled ? <><strong>RTSP · {camera.recordingMode}</strong><small>{camera.archiveStream} stream · {camera.recordingCount.toLocaleString()} local</small>{camera.recoveryEnabled && <button className="recoveryStatus" onClick={() => { setSelectedCameraId(camera.id); setRecoveryGaps([]); setRecoveryLoading(true); setScreen('Recovery') }}><RefreshCw />{camera.recoveryStatus === 'StorageUnavailable' ? 'Recovery: storage unavailable' : camera.recoveryStatus === 'SearchUnavailable' ? 'Recovery: search unavailable' : camera.recoveryPendingCount > 0 ? `Recovery: ${camera.recoveryPendingCount} pending` : recordingControl?.enabled === false ? 'Recovery paused' : 'Recovery monitoring'}</button>}</> : <><strong className="notRecording">Not recording</strong><small>Archive disabled</small></>}</td>
                      <td data-label="Events"><div className="tagRow">{camera.eventTypes.map((eventType) => <span className="tag" key={eventType}>{eventLabels[eventType]}</span>)}</div></td>
                      <td data-label="Last tested">{camera.lastTestedAtUtc ? new Date(camera.lastTestedAtUtc).toLocaleString() : 'Not tested'}</td>
                      <td data-label="Actions"><div className="rowActions"><button className="iconButton" title="Edit camera" onClick={() => openEdit(camera)}><Pencil /></button><button className="iconButton danger" title="Remove camera" onClick={() => void handleDelete(camera)}><Trash2 /></button></div></td>
                    </tr>)}
                  </tbody></table></div>
                )}
              </div>
            </section>
      ) : screen === 'Playback' ? (
      <section className="screen">
        <div className="pageHeading">
        <div><h1>Playback</h1><p>Recordings available in the local archive</p></div>
        <button className="button" disabled={!selectedCameraId || recordingsLoading} onClick={() => {
          if (!selectedCameraId) return
          setRecordingsLoading(true)
          void refreshRecordings(selectedCameraId)
        }}><RefreshCw className={recordingsLoading ? 'spin' : ''} /> Refresh</button>
        </div>

        {recordingsError && <div className="alert error"><CircleAlert /> {recordingsError}</div>}

        <div className="panel playbackToolbar">
        <label>Camera<select value={selectedCameraId} onChange={(event) => selectPlaybackCamera(event.target.value)}>{cameras.map((camera) => <option key={camera.id} value={camera.id}>{camera.name}</option>)}</select></label>
        <label>Date<input type="date" value={recordingDate} onChange={(event) => setRecordingDate(event.target.value)} /></label>
        <div className="eventFilter"><span><ListFilter /> Event type</span><div className="filterButtons"><button className={eventFilters.length === 0 ? 'active' : ''} onClick={() => setEventFilters([])}>All</button>{(Object.keys(eventLabels) as EventType[]).map((eventType) => <button key={eventType} className={eventFilters.includes(eventType) ? 'active' : ''} aria-pressed={eventFilters.includes(eventType)} onClick={() => togglePlaybackEvent(eventType)}>{eventLabels[eventType]}</button>)}</div></div>
        </div>

        <div className="playbackSummary">
        <span><strong>{filteredRecordings.length.toLocaleString()}</strong> recordings</span>
        <span><strong>{formatBytes(filteredRecordings.reduce((total, recording) => total + recording.sizeBytes, 0))}</strong> stored locally</span>
        </div>

        <div className="panel playbackWorkspace">
        <div className="recordingList">
          {recordingsLoading ? <div className="playbackEmpty"><RefreshCw className="spin" /> Loading recordings</div> : filteredRecordings.length === 0 ? <div className="playbackEmpty"><Video /> No local recordings for this date</div> : filteredRecordings.map((recording) => <button key={recording.id} className={`recordingRow ${selectedRecording?.id === recording.id ? 'selected' : ''}`} onClick={() => setSelectedRecordingId(recording.id)}>
          <span className="recordingTime">{new Date(recording.startAtUtc).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })}</span>
          <span className="recordingMeta"><strong>{recording.eventTypes.length > 0 ? recording.eventTypes.map((eventType) => eventLabels[eventType]).join(' + ') : recording.source === 'Rtsp' ? 'Continuous' : 'Camera recording'}</strong><small>{formatDuration(recording.startAtUtc, recording.endAtUtc)} · {formatBytes(recording.sizeBytes)} · {recording.source === 'Rtsp' ? 'RTSP' : 'Camera storage'}</small></span>
          <span className={`archiveState ${recording.state.toLowerCase()}`}>{recording.state}</span>
          </button>)}
        </div>
        <div className="recordingDetail">
          {selectedRecording ? <>
          <div className="detailHeading"><div><span>{selectedCamera?.name}</span><h2>{new Date(selectedRecording.startAtUtc).toLocaleString()}</h2></div><div className="detailActions"><span className={`archiveState ${selectedRecording.state.toLowerCase()}`}>{selectedRecording.state}</span>{selectedRecording.state === 'Complete' && <a className="button" href={recordingOriginalUrl(selectedRecording.id)}><Download /> Original</a>}</div></div>
          <div className="mediaStage">{selectedRecording.state === 'Complete' && !mediaError ? <video key={selectedRecording.id} controls preload="metadata" src={recordingMediaUrl(selectedRecording.id)} onError={() => setFailedMediaId(selectedRecording.id)} /> : <><Video /><strong>{mediaError ? 'Browser cannot play this native codec' : unavailableMediaHeading}</strong><span>{mediaError ? 'The original recording remains available without conversion.' : unavailableMediaDescription}</span>{mediaError && <a className="button mediaDownload" href={recordingOriginalUrl(selectedRecording.id)}><Download /> Original</a>}</>}</div>
          <dl className="recordingFacts"><div><dt>Duration</dt><dd>{formatDuration(selectedRecording.startAtUtc, selectedRecording.endAtUtc)}</dd></div><div><dt>Size</dt><dd>{formatBytes(selectedRecording.sizeBytes)}</dd></div><div><dt>Stream</dt><dd>{selectedRecording.stream}</dd></div><div><dt>Events</dt><dd>{selectedRecording.eventTypes.join(', ') || 'Unclassified'}</dd></div></dl>
          </> : <div className="playbackEmpty"><Clock3 /> No recording selected</div>}
        </div>
        </div>
      </section>
      ) : screen === 'Recovery' ? (
      <section className="screen">
        <div className="pageHeading"><div><h1>Recovery</h1><p>{selectedCamera?.name}</p></div><button className="button" onClick={() => setScreen('Cameras')}><ArrowLeft /> Cameras</button></div>
        {recordingControl?.enabled === false && <div className="alert"><Clock3 /> Recording and recovery paused</div>}
        {recoveryError && <div className="alert error"><CircleAlert /> {recoveryError}</div>}
        <div className="panel tableScroll"><table className="recoveryTable" role="table" aria-label="Recovery intervals"><thead role="rowgroup"><tr role="row"><th role="columnheader">Interval</th><th role="columnheader">Stream</th><th role="columnheader">Mode</th><th role="columnheader">Status</th><th role="columnheader">Unresolved</th><th role="columnheader">Last checked</th></tr></thead><tbody role="rowgroup">
          {recoveryGaps.map((gap) => <tr role="row" key={gap.id}><td role="cell" data-label="Interval">{new Date(gap.startAtUtc).toLocaleString()}<small>{new Date(gap.endAtUtc).toLocaleString()}</small></td><td role="cell" data-label="Stream">{gap.stream}</td><td role="cell" data-label="Mode">{gap.mode}</td><td role="cell" data-label="Status">{({ RecoveredEvents: 'Event clips recovered', CheckedEvents: 'No matching event clips', SearchFailed: 'Search failed' } as Record<string, string>)[gap.status] ?? gap.status}</td><td role="cell" data-label="Unresolved">{Math.ceil(gap.missingSeconds)}s</td><td role="cell" data-label="Last checked">{gap.lastCheckedAtUtc ? new Date(gap.lastCheckedAtUtc).toLocaleString() : 'Not checked'}</td></tr>)}
        </tbody></table>{recoveryLoading ? <div className="empty"><RefreshCw className="spin" /> Loading recovery intervals</div> : recoveryGaps.length === 0 && !recoveryError ? <div className="empty"><ShieldCheck /><h2>No recovery intervals</h2></div> : null}</div>
      </section>
      ) : screen === 'Settings' ? (
    <section className="screen">
      <div className="pageHeading"><div><h1>Settings</h1><p>Storage location and archive organization</p></div></div>
      {notice && <div className="alert success"><CheckCircle2 /> {notice}</div>}
      {settingsError && <div className="alert error"><CircleAlert /> {settingsError}</div>}
      <form className="panel settingsPanel" onSubmit={(event) => void handleApplicationSettings(event)}>
      <div className="sectionTitle"><HardDrive /><h2>Recording storage</h2></div>
      <div className="settingsGrid">
        <label>Mounted archive root<input value={applicationSettings?.archiveRoot ?? ''} readOnly /></label>
        <label>Recording folder<input value={settingsDraft.recordingFolder} onChange={(event) => setSettingsDraft((current) => ({ ...current, recordingFolder: event.target.value }))} placeholder="." required /></label>
        <label>Folder organization<select value={settingsDraft.archiveLayout} onChange={(event) => setSettingsDraft((current) => ({ ...current, archiveLayout: event.target.value as ArchiveLayout }))}>{(Object.keys(archiveLayoutLabels) as ArchiveLayout[]).map((layout) => <option key={layout} value={layout}>{archiveLayoutLabels[layout].label}</option>)}</select></label>
      </div>
      <div className="settingsPreview"><span>New recording path</span><code>{applicationSettings?.archiveRoot ?? '/archive'}/{settingsDraft.recordingFolder === '.' ? '' : `${settingsDraft.recordingFolder.replaceAll('\\', '/').replace(/^\/+|\/+$/g, '')}/`}{archiveLayoutLabels[settingsDraft.archiveLayout].example}</code></div>
      <div className="policyNote"><Archive /><div><strong>Existing recordings stay in place</strong><p>Folder changes apply to newly archived media. Playback and retention continue using each recording's stored path.</p></div></div>
      <div className="formActions"><button className="button primary" disabled={settingsSaving || !applicationSettings} type="submit"><Save /> {settingsSaving ? 'Saving' : 'Save settings'}</button></div>
      </form>
    </section>
      ) : (
            <section className="screen">
              <div className="pageHeading"><div><h1>{editing ? 'Edit camera' : 'Add camera'}</h1><p>{editing ? `${editing.name} · ${editing.model ?? 'Camera'}` : 'Register a Reolink camera on your local network'}</p></div>{testResult?.testId && connectionIsCurrent && <span className="connectedBadge"><CheckCircle2 /> Connected</span>}</div>
              <form ref={formRef} onSubmit={(event) => void handleSave(event)}>
                {testResult?.clockWarning && <div className="alert" role="status"><CircleAlert /><span>{testResult.clockWarning}</span></div>}
                <div className="formLayout">
                  <div className="panel">
                    <div className="formSection">
                      <div className="sectionTitle"><Server /><h2>Connection</h2></div>
                      <div className="fieldGrid">
                        <label>Name<input value={draft.name} onChange={(event) => updateDraft('name', event.target.value)} required /></label>
                        <label>IP address or hostname<input name="host" value={draft.host} onChange={(event) => updateDraft('host', event.target.value)} required /></label>
                        <label>Username<input name="username" value={draft.username} onChange={(event) => updateDraft('username', event.target.value)} autoComplete="username" required /></label>
                        <label>Password<div className="passwordField"><input name="password" type={showPassword ? 'text' : 'password'} value={draft.password} onChange={(event) => updateDraft('password', event.target.value)} placeholder={editing ? 'Leave blank to keep current password' : 'Optional'} autoComplete="new-password" /><button type="button" title={showPassword ? 'Hide password' : 'Show password'} onClick={() => setShowPassword((value) => !value)}>{showPassword ? <EyeOff /> : <Eye />}</button></div></label>
                        <label>Baichuan port<input name="port" type="number" min="1" max="65535" value={draft.port} onChange={(event) => updateDraft('port', Number(event.target.value))} required /></label>
                      </div>
                      <button className="button testButton" type="button" disabled={testing || !draft.host || !draft.username} onClick={() => void handleTest()}>{testing ? <RefreshCw className="spin" /> : <Activity />} {testing ? 'Testing connection' : 'Test connection'}</button>
                    </div>

                    <div className="formSection">
                      <div className="sectionTitle"><Archive /><h2>Archive policy</h2></div>
                      <label className="toggleRow"><span><strong>Record this camera</strong><small>Retain native video in the local archive</small></span><input type="checkbox" checked={draft.archiveEnabled} onChange={(event) => updateDraft('archiveEnabled', event.target.checked)} /></label>
                      {draft.archiveEnabled && <>{testResult && <>
                        <div className="fieldBlock"><span className="fieldLabel">Recording quality</span><div className="choiceGrid">{(['Main', 'Sub'] as ArchiveStream[]).map((stream) => <label className={`choice ${draft.archiveStream === stream ? 'selected' : ''}`} key={stream}><input type="radio" checked={draft.archiveStream === stream} onChange={() => updateDraft('archiveStream', stream)} /><span><strong>{stream} stream</strong><small>{stream === 'Main' ? 'Original highest quality' : 'Lower bandwidth profile'}</small></span></label>)}</div></div>
                        {!testResult.rtspAvailable && <div className="alert error"><CircleAlert /> Selected RTSP stream unavailable</div>}
                        <div className="fieldBlock"><span className="fieldLabel">Recording mode</span><div className="choiceGrid">
                          <label className={`choice ${draft.recordingMode === 'Events' ? 'selected' : ''}`}><input type="radio" checked={draft.recordingMode === 'Events'} onChange={() => updateDraft('recordingMode', 'Events')} /><span><strong>Events</strong><small>Keep selected event windows</small></span></label>
                          <label className={`choice ${draft.recordingMode === 'Continuous' ? 'selected' : ''}`}><input type="radio" checked={draft.recordingMode === 'Continuous'} onChange={() => updateDraft('recordingMode', 'Continuous')} /><span><strong>Continuous</strong><small>Keep every video segment</small></span></label>
                        </div></div>
                        {draft.recordingMode === 'Events' && <div className="fieldBlock durationGrid"><label>Pre-event seconds<input type="number" min="0" max="300" value={draft.preEventSeconds} onChange={(event) => updateDraft('preEventSeconds', Number(event.target.value))} /></label><label>Post-event seconds<input type="number" min="0" max="300" value={draft.postEventSeconds} onChange={(event) => updateDraft('postEventSeconds', Number(event.target.value))} /></label></div>}
                        <div className="fieldBlock"><span className="fieldLabel">Event types</span><div className="choiceGrid">{(Object.keys(eventLabels) as EventType[]).map((eventType) => { const supported = supportedEvents.includes(eventType); return <label className={`choice ${draft.eventTypes.includes(eventType) ? 'selected' : ''} ${!supported ? 'disabled' : ''}`} key={eventType}><input type="checkbox" checked={draft.eventTypes.includes(eventType)} disabled={!supported} onChange={() => toggleEvent(eventType)} /><span><strong>{eventLabels[eventType]}</strong><small>{supported ? (draft.recordingMode === 'Events' ? 'Retain matching video' : 'Index event markers') : 'Not supported by camera'}</small></span></label> })}</div></div>
                        <label className="toggleRow recoveryToggle"><span><strong>Recover missed recordings from camera storage</strong><small>{!testResult.storageAvailable ? 'Mounted, formatted camera storage unavailable' : !testResult.storagePlaybackAvailable ? 'Camera recording search unavailable' : testResult.storage}</small></span><input type="checkbox" checked={draft.recoveryEnabled} disabled={!storageReady && !draft.recoveryEnabled} onChange={(event) => updateDraft('recoveryEnabled', event.target.checked)} /></label>
                        {draft.recoveryEnabled && draft.recordingMode === 'Continuous' && !testResult.continuousAvailable && <div className="alert"><CircleAlert /> No continuous SD recording schedule detected. Recovery may be incomplete.</div>}
                      </>}
                      <div className="policyNote"><HardDrive /><div><strong>Automatic overwrite</strong><p>When free archive capacity drops below 30 minutes, the oldest completed clips are removed until the reserve is restored. Active and partial downloads are protected.</p></div></div>
                      </>}
                    </div>
                  </div>

                  <aside className="panel detectedPanel">
                    <div className="sectionTitle"><ShieldCheck /><h2>Detected camera</h2></div>
                    {testResult ? <>{testResult.snapshotDataUrl && <div className="cameraSnapshot"><img src={testResult.snapshotDataUrl} alt={`Snapshot from ${testResult.model}`} /></div>}<dl><div><dt>Model</dt><dd>{testResult.model}</dd></div><div><dt>Firmware</dt><dd>{testResult.firmware}</dd></div><div><dt>Time zone</dt><dd>{cameraTimezoneLabel(testResult)}</dd></div><div><dt>Main stream</dt><dd>{testResult.mainStream}</dd></div><div><dt>Sub stream</dt><dd>{testResult.subStream}</dd></div><div><dt>Pre-record</dt><dd>{testResult.preRecordSeconds} seconds</dd></div><div><dt>Post-record</dt><dd>{testResult.postRecordSeconds} seconds</dd></div><div><dt>RTSP</dt><dd>{testResult.rtspAvailable ? `${draft.archiveStream} ready` : 'Unavailable'}</dd></div></dl></> : <div className="notTested"><Activity /><p>Test the connection to read camera capabilities.</p></div>}
                  </aside>
                </div>
                {formError && <div className="alert error formAlert"><CircleAlert /> {formError}</div>}
                <div className="formActions"><button className="button" type="button" onClick={() => setScreen('Cameras')}>Cancel</button><button className="button primary" disabled={saving || !recordingPolicyValid} type="submit"><Save /> {saving ? 'Saving' : 'Save camera'}</button></div>
              </form>
            </section>
          )}
        </main>
      </div>
    </div>
  )
}

function cameraTimezoneLabel(result: CameraTestResult) {
  if (result.timezone === 'Camera/Unverified') return 'Unverified'
  if (!result.timezone.startsWith('Camera/')) return result.timezone || 'Unknown'
  const offset = result.timezoneOffsetSeconds
  if (!result.testId || !Number.isInteger(offset) || Math.abs(offset) > 14 * 3600) return 'Custom camera clock'
  const absolute = Math.abs(offset)
  const hours = String(Math.floor(absolute / 3600)).padStart(2, '0')
  const minutes = String(Math.floor(absolute % 3600 / 60)).padStart(2, '0')
  const seconds = absolute % 60
  return `UTC${offset < 0 ? '-' : '+'}${hours}:${minutes}${seconds ? `:${String(seconds).padStart(2, '0')}` : ''} (standard offset)`
}

export default App
