# ReoNvr Application Specification

Status: Approved for implementation
Version: 0.2
Date: 2026-09-11

The existing `ui-mock.html` is a design artifact, not production code.

## 1. Product Goal

ReoNvr is a small, self-hosted Linux application that registers Reolink
cameras, records from either camera storage or a live RTSP stream, and provides
live and recorded playback in a browser.

The camera remains responsible for event detection. In camera-storage mode it
also provides pre/post recording and temporary microSD storage. In RTSP mode,
ReoNvr maintains the local pre-event buffer and recording lifecycle. ReoNvr
provides durable local registration, recording, indexing, playback, retention,
and recovery where the selected source permits it.

## 2. MVP Scope

### Included

- Add, test, edit, enable, disable, and remove a camera registration.
- Store camera registrations locally across container restarts.
- Store camera passwords encrypted and never return them to the browser.
- Detect camera model, firmware, streams, storage, recording settings, and
  supported event categories.
- Record from either completed native camera-storage recordings or RTSP.
- Support event-only and continuous recording with either source when the
  source provides the required media.
- Filter events by motion, person, vehicle, and animal when supported.
- Show a live camera preview.
- Play archived event recordings and seek within a recording.
- Reconcile camera recording history after application or network outages.
- Automatically reclaim local archive space when less than 30 minutes of
  estimated recording capacity remains.
- Run as Linux containers under Docker Desktop during development and Docker
  Compose on the target Linux host.

### Excluded

- Changing camera detection, recording, stream, or SD-card settings.
- Formatting or deleting recordings from camera microSD storage.
- Automatic source switching or hybrid camera-storage/RTSP recording.
- Local object detection, face recognition, or license-plate recognition.
- Reimplementation of the proprietary Baichuan protocol.
- Cloud relay, mobile applications, and multi-user authorization.
- PostgreSQL, MediaMTX, transcoding, and mandatory GPU acceleration.

## 3. Verified Camera Baseline

The initial supported camera is the probed Reolink RLC-810A running firmware
`v3.1.0.956_24022103`.

Verified behavior that controls the design:

- Baichuan v20, HTTPS, RTSP, and ONVIF are reachable.
- Main stream is 3840 x 2160 H.265 at 6144 Kbit/s.
- Sub stream is 640 x 360 H.264 at 256 Kbit/s.
- Native clips have 10 seconds of pre-recording and 15 seconds of
  post-recording.
- Native VOD search returns name, start/end time, byte size, stream, and event
  tags.
- Motion, person, vehicle, and dog/cat are supported.
- Package and cry detection are not supported by this firmware.
- Live event notifications contain category state, but no object bounding
  boxes.
- Hub/NVR historical event command 516 is unsupported. Historical VOD search
  remains the reconciliation source for this camera.

## 4. User Experience

The approved visual direction is represented by `ui-mock.html`. Production UI
may split it into components but should preserve the same compact workflow.

### 4.1 Cameras Screen

Shows every registered camera with:

- Name, model, and host.
- Online, offline, testing, or disabled state.
- Selected archive stream and automatic overwrite policy.
- Recording source and event-only or continuous recording mode.
- Supported and selected event categories.
- Last event and pending download count.
- Playback and edit actions.
- `Not recording` in the archive column when recording is disabled for that
  camera.

Operations:

- `Add camera` opens the empty camera form.
- `Edit` opens the same form with non-secret settings populated.
- `Playback` opens the selected camera in the playback screen.
- Selecting a camera icon opens Playback with that camera selected.
- A persistent application-wide Recording toggle pauses or resumes archive
  production for every camera without changing any per-camera policy.
- Removing a camera requires confirmation and preserves existing archived
  recordings. Destructive archive deletion is outside the MVP.

When global recording is paused, ReoNvr cancels live-event, event-finalization,
camera-storage reconciliation/download, and RTSP recorder workers. Camera
health probes and retention continue running. Queued work and per-camera
settings remain intact and resume when global recording is enabled. The toggle
defaults to enabled during migration and retains its value across restarts.

### 4.2 Add/Edit Camera Screen

Editable fields:

- Display name.
- IP address or LAN hostname.
- Username.
- Password.
- Archive enabled.
- Recording source: `CameraStorage` or `Rtsp`.
- Recording mode: `Events` or `Continuous`.
- Main or sub recording stream.
- Event categories to archive.
- Camera-storage recording automatically imports all matching SD-card media,
  newest first.
- RTSP pre-event and post-event durations for event recording.

Read-only fields discovered from the camera:

- Model and firmware.
- Camera storage timezone, including base UTC offset and daylight-saving rules.
- Main and sub stream details.
- Supported event categories.
- Pre-record and post-record duration.
- Camera-storage recording and RTSP availability.

Camera microSD capacity and schedule details are not shown in the detected
camera summary. They are camera-local capabilities used only to determine
whether camera-storage source and continuous camera-storage mode are available;
they must not be presented as ReoNvr archive capacity.

Behavior:

- `Test connection` validates credentials and reads capabilities without
  persisting the form.
- The camera-reported timezone is authoritative and cannot be entered or
  overridden in the browser.
- Camera-local timestamps are converted to UTC at ingestion. Database and API
  timestamps are always UTC; browser timezone conversion is presentation-only.
- A successful test displays detected values and enables save.
- Turning off `Archive enabled` hides all recording quality, source, mode,
  import, event, duration, and overwrite controls. Their values remain in the
  draft and are restored if recording is turned back on before saving.
- Saving a new camera requires a successful test for the current host,
  username, and password.
- Editing a camera never displays its stored password. A blank password keeps
  the existing password.
- Changing host, username, or password requires another successful test.
- Camera names must be unique. Host plus channel must also be unique.
- Unsupported event categories are disabled rather than silently accepted.
- `CameraStorage` is disabled when mounted/formatted storage and native VOD
  search are unavailable.
- `CameraStorage` plus `Continuous` is disabled unless the camera already
  produces continuous recordings. ReoNvr never changes the camera schedule.
- `Rtsp` is disabled when the selected stream cannot be opened.
- Changing recording source or mode restarts only the affected camera workers.
- In `Events` mode, selected categories determine which recordings are kept.
- In `Continuous` mode, all video is kept and selected categories determine
  which event markers are indexed.

### 4.3 Playback Screen

The screen contains a camera selector, live/recorded mode switch, player,
event list, date selection, and event-category filters.

Live mode:

- Starts an on-demand H.264 substream preview when the screen is opened.
- Stops the preview shortly after the final viewer disconnects.
- Does not save the live stream as a recording.

Recorded mode:

- Lists locally archived clips for the selected day.
- Selecting an event loads its recording and seeks to the event start.
- Shows every native category attached to the clip.
- Supports HTTP byte ranges for seeking.
- Offers original-file download.

The original H.264 or H.265 codec is preserved without transcoding. Browser
codec support is the user's responsibility. When playback is unsupported,
ReoNvr reports the codec/container limitation and still offers the original
file.

### 4.4 Application Settings Screen

Archive storage is configured once for the application, not per camera. The
Settings screen provides:

- One archive location shared by all cameras. The location must be beneath an
  administrator-configured writable storage root.
- A folder layout choice of `CameraName/Date/Hour` or `CameraName/Date`.
- The selected location's filesystem capacity, used bytes, free bytes, and
  calculated 30-minute reserve.
- The current automatic-overwrite state and any storage health issue.

Camera names are sanitized before use as directory names. Changing the archive
location or folder layout affects new recordings only; existing indexed media
retains its stored path and remains playable while its original location is
available. The Settings screen reports archive destination capacity only, never
camera microSD capacity.

## 5. Camera Registration Workflow

### Test

1. Validate the host as a LAN address or hostname resolving to a private
   address.
2. Send credentials to the adapter through standard input. Never put a
   password in process arguments.
3. Use the pinned official Reolink CLI and loopback-only gateway to read:
  device info, timezone and daylight-saving rules, capabilities, storage,
  core recording settings, and streams.
4. Apply a 15-second timeout and return a normalized, redacted result.
5. Keep the successful test fingerprint in server memory for five minutes.

### Save

1. Verify the submitted settings match a current successful test fingerprint.
2. Encrypt the password with AES-256-GCM using the application master key.
3. In one SQLite transaction, insert or update camera metadata, archive policy,
   event selections, and encrypted credential material.
4. Never store the clear-text password, CLI token, or authenticated URL.
5. Start or restart the `CameraLifecycleWorker` after the transaction commits.
6. Start workers for the configured recording source and mode.
7. In camera-storage mode, trigger an overlapping VOD reconciliation for all
  available matching recordings.

### Delete

1. Stop new work for the camera.
2. Cancel its pending jobs safely.
3. Delete its encrypted credential and active registration.
4. Preserve archived files and a non-secret retired-camera identity so existing
   recordings remain playable and are not reimported as a new camera.

## 6. Architecture

```text
Browser
  |
  v
ReoNvr application
  - Go HTTP API and background workers
  - Embedded React static UI
  - SQLite repository
  - Credential encryption service
  - Reolink adapter
  - Playback and retention services
  |
  +--> reolink-gateway on the private Compose network
  |      |
  |      +--> Camera Baichuan port 9000
  |
  +--> Camera HTTPS/RTSP endpoints
  |
  +--> Local archive filesystem
```

### Components

- **Web/API:** serves the SPA, camera APIs, event queries, recordings, and
  health endpoints.
- **Reolink adapter:** isolates official CLI/gateway JSON and all
  firmware-specific behavior.
- **RTSP recorder:** stream-copies the selected camera stream without changing
  H.264/H.265, and maintains bounded event buffers or continuous segments.
- **Playback service:** serves archived ranges and original media without
  transcoding.

### Background Worker Inventory

Every background worker loop is owned by the root `WorkerSupervisor`. Workers
do not share a failure-cancellation context. Failure in one worker, camera,
recording, or payload must not stop unrelated work.

| Worker | Scope | Responsibility | First milestone |
|---|---|---|---|
| `WorkerSupervisor` | Application | Starts, observes, and restarts every worker loop; catches and reports worker panics | 1 |
| `BootstrapWorker` | Application | Retries database, key, migration, and gateway initialization while the diagnostic API stays available | 1 |
| `GatewayHealthWorker` | Application | Probes the loopback gateway, reports degradation, and waits for Docker to restart an unhealthy gateway | 1 |
| `CameraLifecycleWorker` | Per camera | Loads registration, decrypts credentials only when needed, connects, refreshes capabilities, and maintains camera status | 1 |
| `DatabaseMaintenanceWorker` | Application | Runs WAL checkpoints, integrity checks, and scheduled local backups without blocking normal reads | 1 |
| `LiveEventWorker` | Per camera | Maintains the Baichuan event subscription and durably records normalized and raw events | 2 |
| `EventFinalizationWorker` | Application | Waits for event end plus post-record delay, then queues a focused recording search | 2 |
| `ReconciliationWorker` | Per camera | Runs startup, reconnect, hourly, and daily overlapping VOD searches and advances durable watermarks | 2 |
| `DownloadWorker` | Bounded pool | Claims downloads, safely restarts partial transfers, streams bytes, and checkpoints progress | 2 |
| `RtspRecorderWorker` | Per camera | Stream-copies the selected RTSP profile into a bounded event ring or durable continuous segments | 2 |
| `VerificationWorker` | Bounded pool | Verifies length and ETag, hashes files, atomically finalizes media, and writes sidecars | 2 |
| `RetentionWorker` | Archive filesystem | Maintains the 30-minute reserve by deleting oldest eligible completed recordings | 2 |
| `ThumbnailWorker` | Bounded pool | Produces event thumbnails after recording verification | 3 |
| `LivePreviewWorker` | Per active camera viewer group | Starts and stops an on-demand H.264 live preview; never records it | 3 |
| `PlaybackCacheCleanupWorker` | Application | Removes expired live fragments and abandoned playback sessions | 3 |
| `HealthAggregationWorker` | Application | Rolls worker and camera state into API/UI health summaries and resolves recovered issues | 1 |

No other unlisted recurring process may be added without updating this table.

### Common Worker Lifecycle

Durable worker stages use PascalCase values:

```text
Initializing -> Starting -> Running -> Waiting
                    |          |
                    v          v
                 Degraded -> Backoff -> Starting
                    |
                    v
                  Paused -> Starting

Running -> Stopping -> Stopped
```

- `Initializing`: load configuration, cursor, pending work, and prior failure
  state.
- `Starting`: acquire dependencies or establish a camera/gateway session.
- `Running`: execute one bounded unit of work.
- `Waiting`: remain healthy while waiting for a timer, event, or queue item.
- `Degraded`: report a failure while preserving enough state to retry.
- `Backoff`: wait with capped exponential backoff and jitter.
- `Paused`: dependency or capacity is unavailable; probe periodically and
  resume automatically.
- `Stopping`: stop accepting work, checkpoint, release leases, and close
  handles after an explicit shutdown signal.
- `Stopped`: no work is active. This is reached only after explicit shutdown or
  camera disable/removal.

Each work cycle follows these stages:

1. Restore its durable cursor or claim one work item with an expiring lease.
2. Validate current dependencies without changing camera configuration.
3. Perform one bounded, timeout-controlled operation.
4. Persist output and the next cursor atomically.
5. Report success and return to `Waiting`.
6. On failure, classify and report the error, release or extend the lease,
   calculate the next attempt, and continue through `Degraded` or `Backoff`.

### Pipeline Stages

- Worker supervision: `Discover`, `Start`, `Observe`, `Report`, `Restart`,
  `Drain`.
- Bootstrap: `ValidateConfig`, `LoadMasterKey`, `OpenDatabase`, `Migrate`,
  `VerifyGateway`, `Ready`.
- Gateway health: `Probe`, `Classify`, `RecordStatus`, `Wait`, `ProbeAgain`.
- Camera lifecycle: `LoadRegistration`, `LoadCredential`, `Connect`,
  `RefreshCapabilities`, `Online`, `Monitor`, `Reconnect`.
- Database maintenance: `CheckpointWal`, `IntegrityCheck`, `Backup`,
  `VerifyBackup`, `PruneBackups`, `Wait`.
- Live events: `Subscribe`, `Receive`, `Normalize`, `Persist`, `Checkpoint`,
  `Reconnect`.
- Reconciliation: `PlanWindow`, `Search`, `Normalize`, `Upsert`,
  `QueueDownloads`, `CommitWatermark`.
- Event finalization: `WaitForEnd`, `WaitForCameraFinalize`, `Search`, `Match`,
  `QueueDownload`.
- Download: `Claim`, `CheckCapacity`, `OpenOrRestart`, `Transfer`, `Checkpoint`,
  `QueueVerification`.
- RTSP recording: `Connect`, `StreamCopy`, `RotateSegment`, `RetainOrExpire`,
  `Checkpoint`, `Reconnect`.
- Verification: `CheckLength`, `CheckEtag`, `Hash`, `AtomicRename`,
  `WriteSidecar`, `Complete`.
- Retention: `Measure`, `CalculateReserve`, `SelectOldest`, `DeleteArtifacts`,
  `WriteTombstone`, `Remeasure`.
- Thumbnail generation: `Claim`, `OpenRecording`, `Extract`, `Verify`,
  `AtomicPublish`, `Complete`.
- Live preview: `Authorize`, `Connect`, `Publish`, `Heartbeat`, `Drain`,
  `Cleanup`.
- Playback cache cleanup: `Scan`, `CheckSessionLease`, `DeleteExpired`,
  `Reclaim`, `Wait`.
- Health aggregation: `Collect`, `Aggregate`, `PersistIssues`,
  `ResolveRecoveredIssues`, `Publish`, `Wait`.

### Recovery Contract

- Operational errors never call process exit and never cancel sibling workers.
- Every worker entry point has a panic boundary. A panic records its stack as a
  redacted operational issue, marks the worker `Degraded`, and is restarted by
  `WorkerSupervisor` after backoff.
- HTTP handlers have recovery middleware that reports the error and returns a
  request ID without stopping the server.
- Transient failures retry from 1 second up to 15 minutes with jitter.
- Authentication and invalid-configuration failures become `Paused`; they are
  retried every 15 minutes and immediately after configuration changes.
- Unsupported capabilities are recorded and skipped until firmware or camera
  capabilities change. They do not generate rapid retries.
- A malformed event or recording is quarantined as an operational issue while
  the worker continues with the next item.
- Work leases expire, so another worker instance can recover abandoned work
  after a crash or container restart.
- Queue operations and reconciliation are idempotent. Replaying a work item is
  expected and must not create duplicate rows or files.
- If SQLite is temporarily locked or storage is unavailable, workers pause and
  retry. The diagnostic health surface remains available where possible.
- Docker services use `restart: unless-stopped`. Process-level failures outside
  application control, including OOM termination or runtime corruption, are
  recovered by Docker and then by durable worker checkpoints.
- Only an explicit user stop, camera disable/removal, or host shutdown places a
  healthy worker in `Stopped`.

## 7. Docker Deployment Contract

All Docker project configuration will remain inside this repository:

```text
compose.yaml
.env.example
docker/
  app.Dockerfile
  gateway.Dockerfile
config/
  reonavr.example.yaml
runtime/                 # ignored by Git
  data/                  # SQLite and application state
  archive/               # default development archive
  secrets/               # generated master key
  playback-cache/        # disposable live fragments
  rtsp-buffer/           # bounded pre-event stream-copy segments
```

Rules:

- Do not change Docker Desktop settings, Docker's data root, image location,
  WSL distribution, or storage backend.
- Use the existing Docker engine exactly as configured by Docker Desktop.
- Build Linux images with Docker Compose from repository Dockerfiles.
- Pin the Reolink CLI/gateway version and verify its published checksum during
  the image build.
- Bind the gateway only to its private Compose network. Do not publish the
  gateway port to the host.
- Publish only the ReoNvr HTTP port, default `8080`.
- Use bind mounts under `./runtime` by default for transparent local testing.
- Permit `REONVR_ARCHIVE_PATH` to select a different host archive directory
  without moving Docker images.
- Run containers as non-root and use read-only root filesystems where
  practical.
- Add health checks and graceful shutdown.

## 8. Local Persistence and Secrets

### SQLite

Database path: `/var/lib/reonavr/reonavr.db`
Development host path: `./runtime/data/reonavr.db`

SQLite settings:

- WAL journal mode.
- Foreign keys enabled.
- Busy timeout configured.
- Versioned, forward-only migrations.
- One application instance owns the database in the MVP.

### Master Key

- On first setup, generate a random 32-byte key outside the container image.
- Mount it read-only at `/run/secrets/reonavr_master_key`.
- Development location is `./runtime/secrets/master.key` and is ignored by Git.
- On Linux deployment, require owner-only file permissions.
- Losing the key makes stored camera passwords unrecoverable; backup guidance
  must treat the database and key as a pair.
- Rotating the key re-encrypts credentials transactionally and records a key
  version.

### Credential Rules

- Encrypt each password independently with AES-256-GCM and a random nonce.
- Bind ciphertext authentication to camera ID and credential version as
  associated data.
- Never log request bodies from credential endpoints.
- Never return password, ciphertext, nonce, or token through the API.
- Redact usernames, hosts, native filenames, and raw camera payloads from debug
  exports unless explicitly requested by an administrator.
- Prefer `--password-stdin` for CLI operations. If a pinned CLI command has a
  confirmed stdin defect, inject the secret only into that child process
  environment and remove it immediately after process creation.

The Windows Credential Manager entry used for probing is development-only. It
will not be copied into an image, Compose file, environment file, or Linux
runtime.

## 9. Data Model

### Database Naming Convention

- Every table, column, index, and migration-created database identifier uses
  PascalCase.
- Database identifiers contain no underscores.
- SQL values are not identifiers, but durable enum values also use PascalCase
  for consistency.
- Migrations and schema tests must reject newly introduced identifiers that do
  not follow this convention.

### `Cameras`

```text
Id TEXT PRIMARY KEY
Name TEXT NOT NULL UNIQUE
Host TEXT NOT NULL
Port INTEGER NOT NULL DEFAULT 9000
Channel INTEGER NOT NULL DEFAULT 0
Timezone TEXT NOT NULL
Enabled INTEGER NOT NULL DEFAULT 1
ArchiveEnabled INTEGER NOT NULL DEFAULT 1
ArchiveStream TEXT NOT NULL CHECK (ArchiveStream IN ('Main', 'Sub'))
RecordingSource TEXT NOT NULL CHECK (RecordingSource IN ('CameraStorage', 'Rtsp'))
RecordingMode TEXT NOT NULL CHECK (RecordingMode IN ('Events', 'Continuous'))
HistoricalImport TEXT NOT NULL CHECK (HistoricalImport IN ('NewOnly', 'Last24Hours', 'Last48Hours', 'Last7Days', 'AllAvailable'))
PreEventSeconds INTEGER NOT NULL DEFAULT 10
PostEventSeconds INTEGER NOT NULL DEFAULT 15
Model TEXT
Firmware TEXT
CapabilitiesJson TEXT
LastTestedAtUtc TEXT
LastEventAtUtc TEXT
LastSearchWatermarkAtUtc TEXT
LastSuccessAtUtc TEXT
CreatedAtUtc TEXT NOT NULL
UpdatedAtUtc TEXT NOT NULL
RetiredAtUtc TEXT
UNIQUE(Host, Channel)
```

### `CameraCredentials`

```text
CameraId TEXT PRIMARY KEY REFERENCES Cameras(Id)
Username TEXT NOT NULL
PasswordCiphertext BLOB NOT NULL
PasswordNonce BLOB NOT NULL
KeyVersion INTEGER NOT NULL
UpdatedAtUtc TEXT NOT NULL
```

### `CameraEventTypes`

```text
CameraId TEXT REFERENCES Cameras(Id)
EventType TEXT
Enabled INTEGER NOT NULL
Supported INTEGER NOT NULL
PRIMARY KEY(CameraId, EventType)
```

### `WorkerStates`

```text
WorkerName TEXT NOT NULL
ScopeKey TEXT NOT NULL
Stage TEXT NOT NULL
LastHeartbeatAtUtc TEXT
LastSuccessAtUtc TEXT
ConsecutiveFailures INTEGER NOT NULL DEFAULT 0
NextAttemptAtUtc TEXT
LastErrorCode TEXT
UpdatedAtUtc TEXT NOT NULL
PRIMARY KEY(WorkerName, ScopeKey)
```

### `WorkItems`

```text
Id TEXT PRIMARY KEY
Kind TEXT NOT NULL
CameraId TEXT REFERENCES Cameras(Id)
DeduplicationKey TEXT NOT NULL UNIQUE
Stage TEXT NOT NULL
Attempts INTEGER NOT NULL DEFAULT 0
NextAttemptAtUtc TEXT
LeaseOwner TEXT
LeaseExpiresAtUtc TEXT
PayloadJson TEXT NOT NULL
LastErrorCode TEXT
CreatedAtUtc TEXT NOT NULL
UpdatedAtUtc TEXT NOT NULL
```

### `OperationalIssues`

```text
Id TEXT PRIMARY KEY
Severity TEXT NOT NULL
Source TEXT NOT NULL
CameraId TEXT REFERENCES Cameras(Id)
WorkItemId TEXT REFERENCES WorkItems(Id)
ErrorCode TEXT NOT NULL
Message TEXT NOT NULL
FirstSeenAtUtc TEXT NOT NULL
LastSeenAtUtc TEXT NOT NULL
OccurrenceCount INTEGER NOT NULL DEFAULT 1
ResolvedAtUtc TEXT
```

### Archive Tables

`Recordings` stores source (`CameraStorage` or `Rtsp`), original codec when
known, native identity, time range, byte count, local path, verification data,
and the durable state. `RecordingTags` stores normalized event categories and
their source. `Events` stores normalized and raw live notifications.
`IngestionRuns` stores bounded reconciliation history. Every identifier follows
the PascalCase convention.

## 10. HTTP API

All responses use JSON. Errors use a stable code, user-safe message, and
optional field errors. Secret values are never echoed.

```text
GET    /api/health/live
GET    /api/health/ready
GET    /api/system/workers
GET    /api/system/issues
GET    /api/cameras
POST   /api/cameras/test
POST   /api/cameras
GET    /api/cameras/{id}
PUT    /api/cameras/{id}
DELETE /api/cameras/{id}
POST   /api/cameras/{id}/test
GET    /api/cameras/{id}/status

GET    /api/cameras/{id}/events
GET    /api/recordings/{id}
GET    /api/recordings/{id}/media
GET    /api/recordings/{id}/original
GET    /api/cameras/{id}/live/playlist.m3u8
```

`POST /api/cameras/test` accepts an unsaved candidate configuration. A test
response contains normalized device details, supported categories, and an
opaque short-lived test ID.

`POST /api/cameras` requires that test ID. The response contains
`credentialConfigured: true`, never the password.

`PUT /api/cameras/{id}` uses these password semantics:

- Missing or empty password: retain the stored password.
- Non-empty password: replace it after a successful test.
- The literal masked UI placeholder is never accepted as a password.

## 11. Recording and Reconciliation

Recording source and recording mode are independent:

| Source | Events | Continuous |
|---|---|---|
| `CameraStorage` | Download native clips matching selected event categories | Mirror every recording already produced by the camera |
| `Rtsp` | Keep a rolling stream-copy buffer and finalize selected event windows | Persist all stream-copy segments |

No mode transcodes video. Main/sub selection controls the source profile.

### Camera Storage

- Subscribe to live category notifications for low-latency event discovery.
- Persist notifications immediately.
- Wait for event end, camera post-record duration, and a 15-second safety delay.
- Search a narrow VOD window and match by camera, time overlap, and native tags.
- Download the exact recording by native name.
- Run an hourly overlapping recent reconciliation plus bounded, durable
  newest-to-oldest historical backfill windows.
- Use unique recording identities to make every reconciliation idempotent.
- Preserve raw search/event payloads for future firmware decoding changes.
- Camera-storage reconciliation automatically queues every matching recording
  available on the SD card. Existing matching `Discovered` rows are promoted
  when Camera Storage is selected.
- Download queued camera-storage recordings newest-first so recent footage is
  available before older backfill.

### RTSP

- Keep the selected stream connected while recording is enabled.
- Use stream copy; preserve H.264/H.265 and audio without re-encoding.
- In event mode, keep a bounded rolling buffer at least as long as the
  configured pre-event duration.
- Coalesce overlapping selected events into one recording and union their tags.
- Finalize after the configured post-event duration.
- In continuous mode, atomically publish bounded segments and attach selected
  event markers without filtering video.
- RTSP recordings cannot be recovered for periods when ReoNvr, the network, or
  the camera stream was unavailable.
- The UI warns that RTSP recording continuously consumes network and disk;
  codec conversion, if added outside this MVP, may require a high-performance
  CPU or supported GPU.

Download states:

```text
Discovered -> Queued -> Downloading -> Verifying -> Complete
                        |                 |
                        +-> RetryWait <---+
                                 |
                                 +-> Failed or Expired
```

Files are written as `.partial`; retries replace the incomplete transfer.
Completed transfers must be at least as large as the camera's indexed byte
count, are SHA-256 hashed and flushed, and are atomically renamed on the same
filesystem. A larger transfer is accepted because camera size metadata can be
stale.

RTSP fragments are written under the configured buffer root and promoted to
the archive with an atomic rename on the same filesystem. Sidecars identify
the recording source, original codec, event tags, time range, byte count, and
SHA-256 hash.

## 12. Automatic Overwrite Policy

This is a required, non-optional MVP behavior for the local archive. It never
deletes media from the camera microSD card.

### Capacity Calculation

For each enabled camera, use the configured bitrate of its selected archive
stream. The global 30-minute reserve is:

```text
reserve_bytes = sum(selected_stream_bits_per_second) * 1800 / 8
```

If a camera does not report bitrate, use the greater of its rolling seven-day
observed byte rate or a conservative default for that stream resolution.

### Cleanup

- Check free archive bytes before each download or RTSP segment publication and
  at least once per minute.
- If free bytes are below `reserve_bytes`, delete the oldest eligible completed
  recording first.
- Continue oldest-first deletion until free bytes are at least
  `reserve_bytes`.
- Delete the native file, playback derivative, thumbnail, and sidecar as one
  logical operation.
- Retain a minimal database tombstone so reconciliation does not download the
  deleted recording again.
- Never select recordings in `downloading`, `verifying`, or `.partial` state.
- Never delete outside the configured archive root.
- Serialize cleanup so only one retention run can delete files at a time.
- If the reserve cannot be restored, pause new downloads and show a storage
  health error. Resume automatically when space becomes available.

For the verified camera's 6144 Kbit/s main stream, the initial reserve is about
1.38 GB. The value is recalculated when cameras or stream selections change.

## 13. Failure and Status Behavior

- Every error creates or updates a deduplicated `OperationalIssues` record and
  emits a redacted structured log with a request or work-item correlation ID.
- Recovery of the underlying condition resolves the issue automatically while
  retaining its occurrence count and timestamps.
- Bad credentials: show `Authentication failed`; do not reveal camera details.
- Host unreachable: keep registration offline and retry with exponential
  backoff.
- Unsupported command: mark only that capability unavailable.
- Camera storage full: report it but do not modify the camera.
- Archive storage low: run automatic overwrite and expose current action.
- Recording overwritten before download: mark expired and surface the gap.
- Gateway unavailable: restart through Compose health policy and reconcile
  after recovery.
- Application restart: recover SQLite state, partial downloads, and workers
  without duplicate camera or recording rows.
- Database initialization failure: keep the HTTP diagnostic endpoint alive,
  report `BootstrapWorker` as degraded, and retry without terminating.
- Worker panic: contain it at that worker boundary, report it, and restart only
  that worker.
- Unrecoverable process termination: rely on Docker `restart: unless-stopped`,
  then restore all workers from durable stages, cursors, and expired leases.

## 14. Observability

Structured logs must include operation, camera ID, duration, and result while
excluding credentials, tokens, authenticated URLs, and raw request bodies.

Minimum health data:

- Application and gateway readiness.
- Every worker's scope, lifecycle stage, heartbeat, consecutive failure count,
  and next attempt time.
- Camera connection state and last successful probe.
- Last event and reconciliation watermark.
- Pending, active, failed, and expired download counts.
- Archive free bytes and calculated 30-minute reserve bytes.
- Current overwrite activity and oldest eligible recording.
- Active and resolved operational issues.

Health endpoints distinguish liveness from readiness:

- Liveness reports whether the process and supervisor loop can respond. A
  degraded camera or dependency does not fail liveness or cause restart loops.
- Readiness reports whether the API can currently perform normal operations and
  may be degraded while recovery continues.

## 15. Implementation Milestones

### Milestone 1: Local Camera Registration

This is the first implementation slice after approval.

- Create the Go service, embedded React UI, and local Docker Compose stack.
- Add migrations for `Cameras`, `CameraCredentials`, `CameraEventTypes`,
  `WorkerStates`, and `OperationalIssues`.
- Generate/load the local master key and implement credential encryption.
- Package the pinned Reolink CLI/gateway with checksum verification.
- Implement camera test, create, list, read, update, disable, and remove APIs.
- Connect the Cameras and Add/Edit screens to those APIs.
- Persist and recover camera registrations across container restarts.
- Do not implement recording downloads in this milestone.

Milestone 1 acceptance criteria:

1. `docker compose up --build` starts Linux containers without changing Docker
   Desktop configuration.
2. A camera can be tested and registered from the UI.
3. Restarting or recreating the application container preserves registration.
4. Editing non-secret settings retains the existing password.
5. Replacing credentials requires a successful connection test.
6. The database and API contain no clear-text password.
7. Process arguments and logs contain no password or camera token.
8. A removed camera no longer starts workers, while prior archive ownership is
   preserved.
9. Restarting the app or gateway recovers all enabled camera workers without
  manual action.
10. A camera, gateway, database-lock, malformed-response, or worker-panic test
   is reported and does not terminate the API or unrelated workers.

### Milestone 2: Recording and Event Archive

- Configurable `CameraStorage` or `Rtsp` recording source.
- Configurable `Events` or `Continuous` recording mode.
- Live event ingestion, VOD reconciliation, native download, RTSP stream-copy,
  sidecars, verification, recovery where available, and automatic overwrite.

### Milestone 3: Playback

- Event timeline, filters, live preview, native playback, explicit unsupported
  codec feedback, and original-file download.

## 16. Test Strategy

- Unit tests for validation, encryption, password-retention semantics, unique
  camera constraints, reserve calculation, and cleanup eligibility.
- Schema tests reject any database identifier that is not PascalCase or that
  contains an underscore.
- Worker lifecycle tests cover every stage, panic recovery, capped backoff,
  lease expiry, idempotent replay, pause/resume, and graceful shutdown.
- API tests with a fake Reolink adapter for every camera operation.
- Migration and restart tests against a real SQLite file.
- Container tests verifying non-root execution, health checks, persistence,
  and no published gateway port.
- Log/process inspection proving secrets are absent.
- Live acceptance against the RLC-810A only when explicitly enabled.
- Destructive camera commands are not present in the adapter interface.

## 17. Approval Gate

Version 0.2 approval authorizes the Milestone 2 recording-source and
recording-mode implementation described above. Camera settings remain
read-only; implementation must not change camera recording schedules, codecs,
streams, or storage configuration.