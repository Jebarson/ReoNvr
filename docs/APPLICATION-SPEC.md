# ReoNvr Functional Specification

Status: Implemented

This document defines the current application behavior. It is a functional
contract, not a roadmap. Camera protocol observations and experiments are in
[REOLINK-CAMERA-API-LEARNINGS.md](REOLINK-CAMERA-API-LEARNINGS.md).

## 1. Purpose and boundaries

ReoNvr is a single-instance, self-hosted Reolink archive. It registers cameras,
copies native camera-storage recordings or records RTSP streams, indexes event
metadata, serves local playback, and reclaims local storage when necessary.

ReoNvr must:

- Preserve native H.264/H.265 video and available audio without transcoding.
- Store timestamps in UTC after applying the camera-reported timezone.
- Keep registration, credentials, recording state, and local media across
  container recreation.
- Keep camera passwords out of command arguments, logs, URLs, and API output.
- Continue operating unaffected cameras when one camera or worker fails.

ReoNvr must not:

- Change a camera's detection, recording, stream, or storage configuration.
- Format camera storage or delete camera recordings.
- Automatically switch between Camera Storage and RTSP.
- Perform local object detection, face recognition, or license-plate analysis.
- Expose the Reolink gateway directly to the host network.
- Transcode media for browser compatibility.

Live browser preview, multi-user authentication, cloud relay, and mobile
applications are not implemented.

## 2. Recording policies

Recording source and mode are independent settings on each camera:

| Source | Events | Continuous |
|---|---|---|
| `CameraStorage` | Import native SD-card clips matching enabled event categories | Import all native SD-card clips |
| `Rtsp` | Stream-copy RTSP into a rolling buffer and publish event windows | Persist all stream-copy segments |

The selected `Main` or `Sub` stream controls the recording profile. Event mode
requires at least one selected event category. Continuous mode records all
available video and uses selected categories as searchable markers.

### 2.1 Camera Storage

- Reconciliation searches camera VOD history using overlapping time windows.
- Historical import is automatically `AllAvailable` for Camera Storage.
- Eligible recordings are queued and downloaded newest first.
- Repeated searches are idempotent by camera ID and native recording name.
- Existing eligible `Discovered` rows are promoted to `Queued`.
- Downloads use native camera media and never remove the source from the SD
  card.
- A file is downloaded to a `.partial` path, hashed with SHA-256, flushed, and
  atomically renamed on the archive filesystem.
- A transfer smaller than the camera's indexed size fails verification. A
  larger transfer is accepted because observed camera size metadata can be
  stale.
- Recordings overwritten on the camera before download become `Expired` and
  are not retried indefinitely.

### 2.2 RTSP

- FFmpeg uses stream copy; no video or audio re-encoding is allowed.
- Continuous mode publishes bounded segments to the local archive.
- Event mode keeps a rolling pre-event buffer and retains segments intersecting
  selected event windows through the configured post-event duration.
- Overlapping event windows may share retained media and event tags.
- RTSP media cannot be recovered for periods when ReoNvr, the network, or the
  camera stream was unavailable.

### 2.3 Global recording control

The persisted application-wide Recording switch controls archive production.
Turning it off removes event finalization, event subscription, reconciliation,
download, and RTSP recorder workers. Camera health, database maintenance,
retention, and health aggregation continue. Camera policies and queued work
remain intact and resume when recording is enabled.

The migration default is enabled. Operators may leave it disabled indefinitely
without modifying any camera setting.

## 3. Camera registration

A camera has a display name, LAN host, Baichuan port and channel, enabled and
archive-enabled state, stream, source, mode, event categories, and RTSP event
buffer durations.

Before a camera can be created or connection-sensitive fields can be changed,
ReoNvr tests the supplied credentials and discovers model, firmware, timezone,
streams, storage support, and event categories. The successful test is held as
a short-lived server-side fingerprint. Camera names and active host/channel
pairs must be unique.

Editing never returns or displays the saved password. An empty password retains
the existing credential. Removing a camera retires its registration and stops
its workers while preserving local recordings under that camera ID.

## 4. Events and playback

Live camera notifications are normalized and persisted while recording is
enabled. Event finalization searches for and associates matching media.

The public archive has a strict local-media contract:

- Recording lists return only `Complete` rows with a local archive path.
- Event lists return only events backed by completed local media.
- Pending camera metadata is not presented as playable history.
- Local retention removes media and its events from public archive views
  together.

Playback supports HTTP byte ranges and original-file download. Media type is
determined from the file extension and file signature. Reolink MP4 files with
no native extension are served as `video/mp4`; original downloads advertise an
`.mp4` filename without renaming already archived media.

Native H.265/HEVC playback depends on browser and operating-system decoder
support. The original file remains downloadable when in-browser playback is
unsupported.

## 5. Application storage settings

Storage configuration is split across deployment and application settings:

- `REONVR_RECORDINGS_PATH` selects the host directory bind-mounted by Compose.
- The container sees that mount as the fixed archive root `/archive`.
- `RecordingFolder` selects a relative subfolder beneath the archive root.
- `ArchiveLayout` controls paths created for future recordings.

The Settings API and UI expose the mounted root as read-only. `RecordingFolder`
must be relative, must not traverse above the root, and is normalized to `/`.
Saving settings creates the target directory, resolves symbolic links, verifies
that the result remains below the archive root, and performs a temporary
write/delete probe.

Supported layouts are:

| Value | Relative path shape |
|---|---|
| `CameraDate` | `folder/Camera-shortid/YYYY/MM/DD/file` |
| `DateCamera` | `folder/YYYY/MM/DD/Camera-shortid/file` |
| `Camera` | `folder/Camera-shortid/file` |
| `Date` | `folder/YYYY/MM/DD/Camera-shortid-file` |

Camera directory names are sanitized and include the first eight ID characters
to reduce collisions. Date components use the recording's UTC start date.
Extensionless Camera Storage MP4 files receive `.mp4` in newly generated local
paths. Changing settings never moves or renames existing recordings; their
persisted paths remain valid.

## 6. Retention

Retention applies only to completed files in the local archive. It never
deletes camera SD-card content.

Before new media is published and once per minute, ReoNvr calculates a
30-minute free-space reserve from enabled cameras and their selected streams:

$$
\text{reserve bytes} = \frac{\sum \text{selected stream bits per second}
\times 1800}{8}
$$

When free space is below the reserve, the oldest eligible completed local
recording is deleted first until capacity is restored. The database retains
enough state to prevent Camera Storage reconciliation from re-importing a
retained tombstone. Active, incomplete, and partial recordings are ineligible.

## 7. Architecture and workers

```text
Browser -> Go HTTP API -> SQLite
                       -> local archive
                       -> Reolink adapter -> private gateway -> camera
                       -> FFmpeg/FFprobe -> camera RTSP
```

The Go server embeds the React application. SQLite uses WAL mode, foreign keys,
a busy timeout, and forward-only migrations. A root supervisor isolates worker
failures, persists status, applies capped backoff, and restarts recurring work.

Implemented workers:

| Worker | Scope | Responsibility |
|---|---|---|
| `BootstrapWorker` | Application | Publishes bootstrap lifecycle state |
| `GatewayHealthWorker` | Application | Probes the private Reolink gateway |
| `DatabaseMaintenanceWorker` | Application | Runs SQLite maintenance |
| `RetentionWorker` | Application | Enforces the free-space reserve |
| `HealthAggregationWorker` | Application | Publishes aggregate lifecycle state |
| `CameraLifecycleWorker` | Camera | Probes enabled camera registrations |
| `LiveEventWorker` | Camera | Receives and stores selected event notifications |
| `EventFinalizationWorker` | Application | Associates completed events with recordings |
| `ReconciliationWorker` | Camera | Discovers Camera Storage recordings |
| `DownloadWorker` | Camera | Downloads the next queued native recording |
| `RtspRecorderWorker` | Camera | Records the configured RTSP policy |

Workers expose `Starting`, `Running`, `Waiting`, `Degraded`, `Backoff`,
`Paused`, `Stopping`, and `Stopped` lifecycle states where applicable. A panic
or operational error is contained to the worker, recorded without credentials,
and retried according to the supervisor policy.

## 8. Persistence and security

Default container paths:

| Data | Container path | Default host path |
|---|---|---|
| SQLite | `/data/reonavr.db` | `./runtime/data/reonavr.db` |
| Master key | `/run/secrets/master.key` | `./runtime/secrets/master.key` |
| Archive | `/archive` | `${REONVR_RECORDINGS_PATH:-./runtime/archive}` |
| RTSP buffer | `/archive/.rtsp-buffer` | Beneath the archive mount |

The application creates a random 32-byte master key when absent. Camera
passwords are independently encrypted with AES-256-GCM and authenticated with
camera-specific associated data. Clear-text passwords are passed to Reolink
child processes through standard input and are never stored in SQLite.

The database and master key must be backed up and restored together. API
responses and logs must not contain passwords, ciphertext, nonces, tokens, or
authenticated camera URLs.

## 9. Data model

Database identifiers use PascalCase. The current schema contains:

- `SchemaMigrations`: applied forward-only schema versions.
- `Cameras`: registration, capabilities, recording policy, and lifecycle data.
- `CameraCredentials`: encrypted password material and non-secret username.
- `CameraEventTypes`: supported and selected categories.
- `WorkerStates`: current recurring-worker lifecycle and error state.
- `OperationalIssues`: deduplicated active and resolved failures.
- `Recordings`: native identity, source, stream, UTC range, state, path, size,
  codec, ETag, and SHA-256 data.
- `RecordingTags`: normalized categories attached to recordings.
- `Events`: normalized event intervals and redacted raw payload data.
- `IngestionRuns`: reconciliation windows and outcomes.
- `ApplicationSettings`: singleton global recording control, recording folder,
  archive layout, and update time.

Migration 7 adds `RecordingFolder` with default `.` and `ArchiveLayout` with
default `CameraDate`.

## 10. HTTP API

All application APIs return JSON except media bodies and embedded frontend
assets. Errors have stable codes and user-safe messages.

```text
GET    /api/health/live
GET    /api/health/ready
GET    /api/system/workers
GET    /api/system/issues
GET    /api/system/recording
PUT    /api/system/recording
GET    /api/system/settings
PUT    /api/system/settings

GET    /api/cameras
POST   /api/cameras/test
POST   /api/cameras
GET    /api/cameras/{id}
PUT    /api/cameras/{id}
DELETE /api/cameras/{id}
POST   /api/cameras/{id}/test
GET    /api/cameras/{id}/status
GET    /api/cameras/{id}/recordings
GET    /api/cameras/{id}/events

GET    /api/recordings/{id}
GET    /api/recordings/{id}/media
GET    /api/recordings/{id}/original
```

`PUT /api/system/settings` accepts `recordingFolder` and `archiveLayout` after
filesystem and enum validation. `PUT /api/system/recording` persists the global
enabled state and applies it immediately.

## 11. Deployment contract

Docker Compose builds the app and pinned gateway images, starts them with
`restart: unless-stopped`, exposes only app port `8080`, and waits for gateway
health before starting the app. Runtime data uses bind mounts so rebuilding or
recreating containers does not erase it.

The supported update path is `docker compose up -d --build`. Deployment must
not change Docker Desktop's engine, data root, WSL distribution, or storage
backend.

## 12. Validation contract

Changes should pass:

- `go test ./...`
- `npm run lint` and `npm run build` in `web/`
- `docker compose config --quiet`
- API tests for camera, recording control, settings, event, and media behavior
- Migration/reopen tests against a real SQLite file
- Archive path containment and layout tests
- Browser checks at desktop and mobile widths

Live-camera acceptance must be explicitly enabled. Tests must not alter camera
configuration, enable recording without operator intent, delete camera media,
or transcode archived recordings.