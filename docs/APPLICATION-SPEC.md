# ReoNvr Functional Specification

Status: Implemented; review fixes and validation limits are listed in section 12

This document defines the functional contract and known validation limits,
not a roadmap. Camera protocol observations and experiments are in
[REOLINK-CAMERA-API-LEARNINGS.md](REOLINK-CAMERA-API-LEARNINGS.md).

## 1. Purpose and boundaries

ReoNvr is a single-instance, self-hosted Reolink archive. It registers cameras,
records RTSP streams with optional camera-storage recovery, indexes event
metadata, serves local playback, and reclaims local storage when necessary.

ReoNvr must:

- Preserve native H.264/H.265 video and available audio without transcoding.
- Store RTSP capture timestamps in host UTC; convert camera-local storage
  timestamps to UTC using the camera-reported clock rules.
- Keep registration, credentials, recording state, and local media across
  container recreation.
- Keep camera passwords out of command arguments, logs, URLs, and API output.
- Continue operating unaffected cameras when one camera or worker fails.

ReoNvr must not:

- Change a camera's detection, recording, stream, or storage configuration.
- Format camera storage or delete camera recordings.
- Replace primary RTSP capture with camera-storage polling.
- Perform local object detection, face recognition, or license-plate analysis.
- Expose the Reolink gateway directly to the host network.
- Transcode media for browser compatibility.

Live browser preview, multi-user authentication, cloud relay, and mobile
applications are not implemented.

## 2. Recording policies

RTSP is the primary source for every camera. Each camera selects a mode and
may enable camera-storage recovery independently:

| Mode | Primary capture | Optional recovery |
|---|---|---|
| `Events` | Stream-copy into a rolling buffer and publish selected event windows | Recover matching historical event clips from monitored intervals |
| `Continuous` | Persist all stream-copy segments | Recover clips overlapping missing verified local coverage |

The selected `Main` or `Sub` stream controls the recording profile. Event mode
requires at least one selected event category. Continuous mode records all
available video and uses selected categories as searchable markers.

### 2.1 Camera-storage recovery

- `RecoveryEnabled` requires mounted, formatted storage and a successful
  selected-stream recording-search probe. RTSP-only capture does not require
  either capability.
- Durable recording-intent periods snapshot stream, mode, event categories,
  and event buffers while global recording and the camera's archive/recovery
  policies are enabled. Open periods survive process restarts. Policy changes
  close the old period and start a new one.
- Recovery starts at activation, not at the beginning of camera history.
- A per-camera worker runs every minute. Each pass scans at most four pending
  intent periods, advances each by at most 30 minutes, and checks at most four
  unresolved intervals. Recent footage remains provisional for two minutes in
  Continuous mode or ten minutes in Events mode.
- Continuous scans subtract verified coverage from expected recording periods.
  Packet timestamps and keyframes establish usable ranges, with a two-second
  continuity/alignment tolerance. Successful download or file duration alone
  is not proof of complete coverage.
- Event scans also inspect historical recording tags during quiet intervals;
  absence of an event clip is not automatically a continuous-recording gap.
  If capture was unobserved and no matching camera clip exists, the interval
  stays `Unknown`. Matching recovered event clips and observed intervals with
  no matching clips have distinct terminal outcomes.
- VOD searches extend two seconds around each interval boundary. Results must
  match the saved stream and overlap missing coverage; event recovery also
  filters selected categories. Files ending less than 30 seconds ago remain
  provisional.
- Eligible native recordings are queued and downloaded newest first.
- Repeated searches are idempotent by camera ID and native recording name.
- Downloads use native camera media and never remove the source from the SD
  card.
- A file is downloaded to a `.partial` path, hashed with SHA-256, flushed, and
  inspected before atomic rename on the archive filesystem. Verified coverage
  is persisted before the recording is marked complete.
- A transfer smaller than the camera's indexed size fails verification. A
  larger transfer is accepted because observed camera size metadata can be
  stale.
- Recordings overwritten on the camera before download become `Expired` and
  are not retried indefinitely.
- Native clips are retained whole and may overlap RTSP boundaries. Recovery
  does not trim, transcode, or seamlessly splice the playback timeline.
- Recovery requires an existing camera schedule and retained footage covering
  the interval. Camera and host clocks must be synchronized; timezone discovery
  is not clock-skew correction.
- Unresolved states include `Pending`, `Downloading`, `Partial`, `Unavailable`,
  `Unknown`, and `SearchFailed`. Terminal outcomes are `Recovered`,
  `RecoveredEvents` and `CheckedEvents`. Legacy `Suppressed` intervals are
  eligible for rechecking rather than treated as proof of complete coverage.

### 2.2 RTSP

- FFmpeg uses stream copy; no video or audio re-encoding is allowed.
- Continuous mode publishes approximately one-minute, clock-aligned segments
  to the local archive; cuts wait for native keyframes. Event buffering uses
  two-second segment targets, also constrained by native keyframes.
- RTSP files are muxed into Matroska without changing their codecs. UTC
  wall-clock timestamps identify capture coverage in the index, while each
  file uses relative presentation timestamps for browser playback and seeking.
- Event mode keeps a rolling pre-event buffer and retains segments intersecting
  selected event windows through the configured post-event duration.
- Overlapping event windows may share retained media and event tags.
- Active event windows publish checkpoints at approximately one-minute
  intervals. Capture cancellation or failure flushes valid buffered event and
  post-event footage with a bounded shutdown context; it cannot invent footage
  that was never captured. Native boundary fragments may overlap checkpoints.
- Capture coverage is persisted separately from retained event media, so quiet
  observed video is distinguishable from an unobserved capture interval.
- Optional recovery can fill unavailable capture intervals only when matching
  native footage remains on the camera. Incomplete recovery remains visible.

### 2.3 Global recording control

The persisted application-wide Recording switch controls archive production.
Turning it off removes event subscription, recovery, download, and RTSP
recorder workers. Camera health, database maintenance,
retention, and health aggregation continue. Camera policies and queued work
remain intact and resume when recording is enabled.

Each recording-control update serializes persistence and its lifecycle hook,
so a delayed resume cannot overtake a newer pause.

Pausing closes open recording-intent periods in the same transaction as the
global switch update. Resuming starts new periods; deliberate pauses are not
backfilled. Abrupt process downtime leaves an open period available for recovery.

The migration default is enabled. Operators may leave it disabled indefinitely
without modifying any camera setting.

Worker status retains historical rows after workers stop. A `Stopped` row is
not active ingestion and does not mean recording resumed.

## 3. Camera registration

**Detect Automatically** appears beside **Add camera** and starts an on-demand,
read-only network scan. It does not run at startup, authenticate to cameras,
register devices, or change recording state. A scan requests three seconds of
discovery time within an eight-second process deadline. Only private or
link-local numeric IP endpoints with valid TCP ports are returned, deduplicated
by address and port; matching registered endpoints are marked already added.

Results prefill only the name, address, and port in the normal add form. The
operator must still supply credentials and pass the connection test before
saving. The UI supports loading, empty, failure/retry, and dismiss states.
Docker broadcast isolation, VLANs, firewalls, and sleeping devices can prevent
replies; manual address entry remains available.

The standard Compose deployment includes a discovery relay and publishes app
UDP `3000` for fixed-port native replies. The relay listens on UDP `2000` in the
Docker host network namespace and forwards only the exact four-byte native
probe from private, link-local, or loopback senders on source port `3000`. It
forwards at most once per second, accepts at most 32 private/link-local IPv4
targets, and never handles credentials or interprets camera reply payloads.
The existing CLI and API still decode, validate, and deduplicate replies.

Default targets combine private IPv4 interface broadcasts with a short list of
common home-network broadcasts for Docker Desktop. This is best-effort support
for common networks, not automatic knowledge of every host LAN. The optional
`REONVR_DISCOVERY_BROADCASTS` comma-separated override replaces both sets of
defaults. There is no background probing without an eligible discovery request.

A camera has a display name, LAN host, Baichuan port and channel, enabled and
archive-enabled state, stream, mode, event categories, RTSP event buffer
durations, and optional recovery policy. New saves normalize the legacy source
and history fields to `Rtsp` and `NewOnly`; recording-level source fields still
identify each file's provenance.

Before a camera can be created or connection-sensitive fields can be changed,
ReoNvr tests the supplied credentials and discovers model, firmware, timezone,
streams, storage support, and event categories. When storage is available, a
one-minute search probes selected-stream playback support independently of
RTSP readiness. The successful test is held as a short-lived server-side
fingerprint that includes the stream. Stream changes require a fresh test.
Camera names and active host/channel pairs must be unique.

Clock discovery is nonblocking for RTSP registration. A matching named timezone
is used when available; otherwise verified camera offset/DST rules are stored
as `Camera/<base64url-encoded JSON>` in the existing `Timezone` field. The UI
shows a friendly standard offset or custom-clock label, never that internal
encoding. Unreadable rules use `Camera/Unverified`; this disables camera-storage
recovery rather than guessing UTC, but does not prevent RTSP-only registration.

For custom clock rules, the connection test also reads the camera's current
date/time. It compares that value with the host's request interval localized
using those rules, allowing one minute of difference. Only an observed mismatch
produces the advisory:

> Your camera's time doesn't match your machine. ReoNvr will use the camera's clock.

Custom rules alone, a matching time, or unreadable current time do not produce
that warning. It requires no acknowledgement and does not block saving. No
camera time settings are changed; timezone conversion is not skew correction.

Editing never returns or displays the saved password. An empty password retains
the existing credential. Removing a camera retires its registration and stops
its workers while preserving local recordings under that camera ID.

## 4. Events and playback

Live camera notifications are normalized and persisted while recording is
enabled. RTSP publication associates matching event windows with local media;
recovery uses historical recording tags for missed native event clips.

The public archive has a strict local-media contract:

- Recording lists return only `Complete` rows with a local archive path.
- Event lists return only events backed by completed local media.
- Event backing requires overlap with verified `RecordingCoverage`, not merely
  a recording's bounding interval. Legacy files without inspected coverage can
  still be played but do not establish event backing until verified.
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

Verified coverage for retained tombstones and recording-level final-state checks
prevent re-importing intentionally removed footage. Retention does not suppress
unrelated missing coverage or pending downloads in the same interval.

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
| `RecoveryWorker` | Camera | Compares durable intent with coverage and searches bounded camera-history intervals |
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

Migration 8 preserves completed local media and converts camera-storage-primary
policies to RTSP-primary with recovery beginning at the migration boundary.
Legacy camera-storage rows in `Queued`, `Downloading`, `Verifying`, or
`RetryWait` become `Discovered` metadata rather than continuing the old import
queue. This does not remove source SD files or completed archive files, and
does not guarantee those old pending transfers will later be recovered.

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
- `RecoveryPeriods`: durable expected recording periods and scan cursors.
- `RecordingCoverage`: verified UTC ranges backed by local media or retention
  tombstones.
- `CaptureCoverage`: observed RTSP ranges, including unretained quiet video.
- `RecoveryGaps`: persisted interval outcomes and unresolved durations.
- `RecoveryGapRecordings`: native recordings associated with recovery intervals.

Migration 7 adds `RecordingFolder` with default `.` and `ArchiveLayout` with
default `CameraDate`.

Migration 8 adds the recovery policy and tables. Existing cameras become
`Rtsp`/`NewOnly`; previous `CameraStorage` cameras have recovery enabled from
the migration boundary. Unfinished legacy camera downloads return to discovery
until a recovery interval needs them. Completed recordings and paths remain
unchanged.

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
POST   /api/cameras/discover
POST   /api/cameras/test
POST   /api/cameras
GET    /api/cameras/{id}
PUT    /api/cameras/{id}
DELETE /api/cameras/{id}
POST   /api/cameras/{id}/test
GET    /api/cameras/{id}/status
GET    /api/cameras/{id}/recordings
GET    /api/cameras/{id}/events
GET    /api/cameras/{id}/recovery

GET    /api/recordings/{id}
GET    /api/recordings/{id}/media
GET    /api/recordings/{id}/original
```

`PUT /api/system/settings` accepts `recordingFolder` and `archiveLayout` after
filesystem and enum validation. `PUT /api/system/recording` persists the global
enabled state and applies it immediately.

Clock discovery is advisory and does not block successful authentication,
registration, or live RTSP recording, which uses the NVR's UTC clock. Connection
tests return `clockWarning` only when a custom clock's reported current time
differs from the NVR's current time, converted using the camera's rules, by more
than one minute. The read-only `system-general` query is timed to allow for
network delay. Custom rules alone, matching times, and unreadable current times
do not trigger a mismatch warning. Its text is: "Your camera's time doesn't match
your machine. ReoNvr will use the camera's clock." Users can save without changing
or acknowledging those settings. No camera settings are changed automatically.

Verified custom rules are persisted with the camera and used to convert
camera-storage timestamps without requiring an IANA timezone name. An unreadable
clock is explicitly marked unverified, never guessed as UTC, and disables
camera-storage recovery rather than live recording. Custom-rule searches reject
ambiguous repeated local timestamps during a daylight-saving rollback and dates
outside the supported 1900-2200 conversion range. Camera-command timeouts and
other connection failures retain actionable, sanitized messages without raw
process output or credentials.

Camera responses expose recovery readiness, unresolved count, and missing
seconds. `GET /api/cameras/{id}/recovery` returns the latest 100 persisted
intervals, including terminal outcomes. Missing cameras return 404.

`POST /api/cameras/discover` accepts no credentials and returns an array of
`host`, `port`, `name`, and `registered` fields. An empty scan returns `[]`.
Only one scan may run at a time: overlapping requests return 409 with
`Retry-After: 3`. Unavailable discovery returns 501; execution or parsing failures
return 502 with a generic safe message, never raw device or process diagnostics.

## 11. Deployment contract

Docker Compose builds the app, pinned gateway, and minimal non-root discovery
relay images, starts them with `restart: unless-stopped`, publishes app TCP
`8080` and UDP `3000`, and waits for gateway health before starting the app.
The gateway remains private. The relay uses the Docker host network namespace,
has a read-only root filesystem, drops all capabilities, and cannot gain new
privileges. This does not require changing Docker Desktop's platform settings.
Runtime data uses bind mounts so rebuilding or recreating containers does not
erase it.

Only one discovery receiver can own host UDP `3000`. The fresh replica publishes
TCP `8081` on loopback and uses independent state volumes; when it owns discovery
UDP `3000`, another installation must not publish the same port. The relay is
stateless and has no access to either installation's secrets, database, or media.

The supported update path is `docker compose up -d --build`. Deployment must
not change Docker Desktop's engine, data root, WSL distribution, or storage
backend.

## 12. Review fixes and validation limits

The five findings from the 2026-11-12 independent review are addressed with
focused regression coverage:

1. Recording-control persistence and lifecycle application are serialized;
   a blocked-resume/competing-pause test checks ordering.
2. Interrupted event/post-event windows flush captured native fragments, and
   long open events publish checkpoints. Tests cover pause, stream failure,
   post-roll interruption, and checkpoint timing.
3. Retention preserves other queued recovery clips and missing seconds.
   Tombstones retain verified coverage; old blanket-suppressed gaps are
   rechecked. Real SQLite tests verify these cases and continued eligibility.
4. RTSP publication writes and flushes a journal before renaming media.
   Startup and camera-worker retries reconcile it before removing abandoned
   fragments. Replay checks archive containment, file size and SHA-256,
   restores sidecar/index/coverage, is idempotent after indexing, and never
   resurrects retention tombstones. Journaled publication sources survive
   cleanup. Tests inject failures before rename, after rename, and after
   indexing, and reject escaped paths, symlinks, and corrupted media. See
   [rtsp_publication.go](../internal/archive/rtsp_publication.go).
5. Public event queries require verified media overlap. SQLite tests cover
   uninspected recordings and events entirely inside discontinuous-video holes.

This does not certify physical power-loss durability, real camera/network
outage recovery, full-disk behavior, or destructive storage drills. A hard
process failure can still lose the unjournaled tail of the active capture;
event checkpoints bound the accumulated window rather than guaranteeing every
uncut packet. Corrupted or missing journaled media produces an error instead
of fabricated coverage. Historical orphan files created before journaling are
not automatically discovered; the journal protects new publications.

## 13. Validation contract

Changes should pass:

- `go test ./...`
- `npm run lint` and `npm run build` in `web/`
- `docker compose config --quiet`
- API tests for camera, recording control, settings, event, and media behavior
- Migration/reopen tests against a real SQLite file
- Archive path containment and layout tests
- Browser checks at desktop and mobile widths
- `npm run test:e2e` in the Playwright container against a paused deployment
  with a registered storage-capable camera and bundled Chromium

Execute builds and tests only in containers. The README documents PowerShell
commands with isolated dependency/cache volumes; no host tool installation is
required. Live camera targets and shared credentials come from an ignored local
JSON configuration based on `internal/testconfig/config.example.json`; checked-in
examples keep live addresses and credentials blank. Tests must not read saved
credentials or the encryption key from the original installation. Blank live
credentials fail before network access. Unit tests use configured synthetic
addresses and generate blank fixture credentials in memory. Camera/test images
must not be committed; only actual application/manual image assets belong in Git.
Discovery checks cover read-only live scans, normalized safe endpoints,
concurrent requests, simulated result selection, empty/error/retry states, and
responsive layouts down to 320 pixels.

Relay tests cover default/interface targets, explicit overrides, deduplication,
invalid/public targets, the destination limit, exact probe/source filtering,
rate limiting, real loopback UDP forwarding, and cancellation. The packaged
relay has also been checked against real cameras on Windows Docker Desktop
through the API and browser without changing platform settings or registering
devices. Other host platforms and unusual network topologies remain unverified.

Deterministic recovery tests cover real SQLite persistence, matching-stream
selection, partial downloads, repeated searches, and event uncertainty. Media
tests inspect packet discontinuities when FFmpeg is installed. The opt-in
`TestRtspLiveMediaIntegration` uses a copied database and isolated temporary
archive for a bounded live capture; it requires `REONVR_INTEGRATION_DATABASE`
and `REONVR_INTEGRATION_KEY`. These checks do not substitute for physically
inducing and recovering a live-camera outage.

Live-camera acceptance must be explicitly enabled. Tests must not alter camera
configuration, enable recording without operator intent, delete camera media,
or transcode archived recordings.