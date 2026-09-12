# Reolink Camera APIs and Linux Native Recording Archive

## Purpose

This document is a self-contained technical handoff for an LLM or engineer
building a Linux application that archives recordings and detection metadata
from Reolink cameras.

The intended product is not an object detector or conventional NVR. Reolink
cameras already perform object detection, pre/post-event buffering, and local
microSD recording. The application should extend that storage onto arbitrary
Linux disks while preserving the camera's native event categories and original
recordings.

This handoff is based on:

- A read-only live probe of an RLC-810A.
- Reolink's Camera HTTP API documentation.
- Reolink's official `reolink-cli` and `reolink-gateway`.
- The `reolink_aio` implementation used by Home Assistant.
- ONVIF specifications and Reolink protocol support documentation.

No credentials are included. Replace placeholders such as `CAMERA_HOST`,
`CAMERA_USER`, and `CAMERA_PASSWORD` with securely supplied values.

## Executive Summary

Reolink has most of the primitives needed for a Linux-native archival
application, but it does not provide a supported Linux equivalent of the
Windows/macOS Reolink Client.

The available primitives include:

- Search recordings stored on camera microSD.
- Download the original camera MP4.
- Retrieve start/end timestamps and file sizes.
- Retrieve native detection tags such as motion, person, vehicle, and animal.
- Receive live detection notifications through a persistent session.
- Resume HTTP downloads using byte ranges.
- Use Linux builds of `reolink-cli` and `reolink-gateway`.

The missing product layer is:

- Automatic ingestion.
- Persistent indexing.
- Reconciliation after outages.
- Retention and disk management.
- Timeline/filter UI.
- Browser-compatible playback.

The best MVP is an event-driven Linux service that:

1. Receives live Reolink notifications.
2. Waits for the camera to finish its post-event recording.
3. Searches a narrow time window for the completed recording.
4. Downloads the original MP4.
5. Stores normalized and raw metadata in SQLite.
6. Reconciles historical camera state periodically to recover missed events.
7. Presents a basic searchable web interface.

## Current Product Situation

As of September 11, 2026:

- Reolink Client is officially available for Windows and macOS.
- Reolink does not provide an equivalent supported Linux desktop client/NVR
  application.
- Linux users are directed to browser access, mobile applications, hardware
  NVRs, or Home Hubs.
- `reolink-cli` and `reolink-gateway` support Linux, macOS, and Windows.
- `reolink-gateway` is a local protocol/session gateway, not an NVR and not a
  recording archive.

The CLI can control cameras, search VOD, download recordings, stream events,
inspect storage, and manage camera recording schedules. It does not
automatically mirror recordings to Linux storage or maintain a durable archive.

## Verified Camera

The live probe used one front-facing Reolink camera:

| Property | Observed value |
|---|---|
| Model | RLC-810A |
| Firmware | `v3.1.0.956_24022103` |
| Hardware | `IPC_523128M8MP` |
| Channels | 1 |
| Storage slots | 1 microSD |
| HTTPS | Enabled |
| RTSP | Enabled on port 554 |
| ONVIF | Enabled on port 8000 |
| Baichuan/media protocol | Enabled on port 9000 |
| HTTP | Disabled |
| RTMP | Disabled |

The camera supported:

- Motion detection.
- Person detection.
- Vehicle detection.
- Animal/dog-cat detection.
- HTTP recording search and download.
- Baichuan recording/event search.
- Current AI and motion state queries.

The camera did not support the HTTP `GetEvents` command on this firmware. This
does not prevent live event delivery through Baichuan or ONVIF.

## Verified microSD Behavior

At probe time:

- Total card capacity was approximately 244 GB.
- Free space was approximately 586 MB.
- Loop overwrite was enabled.
- The camera had approximately 10.6 days of retained event recordings.
- Continuous scheduled recording was disabled.
- Motion, person, vehicle, and animal event schedules were enabled all week.
- Post-recording duration was 15 seconds.
- Pre-recording was enabled.

Observed recording volume:

| Date | Recordings | Total bytes | Approximate size |
|---|---:|---:|---:|
| September 9, 2026 | 932 | 28,952,666,566 | 28.95 GB |
| September 10, 2026 | 859 | 21,597,550,810 | 21.60 GB |
| September 11, 2026 through about 10:24 AM | 258 | 6,184,712,593 | 6.18 GB |

The oldest surviving recording began on August 31, 2026 at 6:54 PM.

At the September 10 activity level, this one camera could produce roughly:

- 650 GB per 30-day month.
- 7.9 TB per year.

Actual volume depends heavily on scene activity and detection configuration.

## How Reolink Stores Recordings and Markers

### Video files

The camera stores individual MP4 files on microSD. A typical path is:

```text
Mp4Record/2026-09-10/RecM02_DST20260910_000740_000804_6732828_983366.mp4
```

The file is an ordinary ISO Base Media MP4 containing the camera's encoded
video/audio. A downloaded sample contained no readable person, vehicle, animal,
or motion marker strings.

### Recording index

Reolink maintains a separate recording index. The HTTP `Search` response
includes:

- A monthly day-availability bitmap.
- Recording path/name.
- Start timestamp.
- End timestamp.
- Stream type.
- File size.
- Nominal frame dimensions/rate on firmware that supplies them.

The recording index explains calendar availability and basic timeline ranges.

### Detection markers

Detection metadata is not known to be stored as standard MP4 timed metadata.
It is represented through two proprietary mechanisms:

1. Trigger bit flags encoded in a hexadecimal component of the filename.
2. A separate historical alarm index available through Baichuan on supported
   firmware.

The richer Baichuan index supplies:

- Event type/category.
- Event start time.
- Event end time.
- Associated recording identifier.
- Stream/channel context.

The Reolink desktop client likely combines recording search results with this
alarm index to construct its colored timeline and detection filters.

### Verified filename trigger decoding

For the RLC-810A's `RecM02` filenames, known bits include:

| Meaning | Bit position |
|---|---:|
| Person detection | 17 |
| Face detection | 18 |
| Vehicle detection | 19 |
| Animal detection | 20 |
| Scheduled recording | 23 |
| Generic motion recording | 24 |
| RF recording | 25 |
| Doorbell/visitor recording | 26 |
| Other AI | 27 |

These layouts are proprietary and version-dependent. Do not make filename
decoding the only source of truth. Preserve the raw filename and raw alarm
response so newer layouts can be reprocessed later.

### Verified one-day marker distribution

For all 859 recordings on September 10, the filename flags matched the richer
Baichuan record types:

| Marker combination | Count |
|---|---:|
| Motion | 381 |
| Motion + vehicle | 350 |
| Vehicle | 94 |
| Motion + person + vehicle | 19 |
| Animal | 5 |
| Person | 4 |
| Motion + person | 3 |
| Motion + animal | 2 |
| Motion + person + vehicle + animal | 1 |

## Reolink HTTP API

### Base endpoint

Firmware and documentation vary between:

```text
https://CAMERA_HOST/cgi-bin/api.cgi
https://CAMERA_HOST/api.cgi
```

The probed RLC-810A accepted `https://CAMERA_HOST/cgi-bin/api.cgi`.

Reolink cameras commonly use self-signed TLS certificates. Production software
should support certificate pinning or an explicit per-camera trust policy,
rather than globally disabling TLS validation.

### Authentication

The API supports login tokens. Some read operations also accept URL query
credentials, but credentials should not be placed on command lines, in logs, or
in persisted URLs.

Illustrative login request:

```json
[
  {
    "cmd": "Login",
    "action": 0,
    "param": {
      "User": {
        "userName": "CAMERA_USER",
        "password": "CAMERA_PASSWORD"
      }
    }
  }
]
```

Use the returned token for subsequent operations and explicitly log out when
appropriate.

### Device information

Command:

```text
GetDevInfo
```

Relevant fields include:

```json
{
  "model": "RLC-810A",
  "name": "Camera 1",
  "firmVer": "v3.1.0.956_24022103",
  "hardVer": "IPC_523128M8MP",
  "channelNum": 1,
  "diskNum": 1
}
```

### Time and timezone

Command:

```text
GetTime
```

Preserve:

- Camera-local date/time.
- Configured timezone offset.
- DST enablement.
- DST rules.

Search parameters are camera-local naive timestamps. Correct timezone and DST
handling is essential when mapping camera events to UTC database timestamps.

### Storage information

Command:

```text
GetHddInfo
```

Observed item shape:

```json
{
  "capacity": 244447,
  "format": 1,
  "mount": 1,
  "number": 0,
  "size": 586,
  "storageType": 2
}
```

Interpretation:

- `capacity`: total capacity in the camera's reported units.
- `size`: remaining/free capacity in the camera's reported units.
- `format`: filesystem initialized.
- `mount`: storage mounted.
- `storageType`: device-specific storage type.

Mounted and formatted does not prove card health or remaining flash endurance.

### Recording configuration

Preferred command on current firmware:

```text
GetRecV20
```

The older `GetRec` was unsupported on the probed firmware.

Observed configuration included:

- `enable: 1`
- `overwrite: 1`
- `preRec: 1`
- `postRec: "15 Seconds"`
- Event schedules for motion, person, vehicle, and animal.
- No continuous `TIMING` schedule.

### Search recordings

Command:

```text
Search
```

Example request:

```json
[
  {
    "cmd": "Search",
    "action": 0,
    "param": {
      "Search": {
        "channel": 0,
        "onlyStatus": 0,
        "streamType": "main",
        "StartTime": {
          "year": 2026,
          "mon": 9,
          "day": 10,
          "hour": 0,
          "min": 0,
          "sec": 0
        },
        "EndTime": {
          "year": 2026,
          "mon": 9,
          "day": 10,
          "hour": 23,
          "min": 59,
          "sec": 59
        }
      }
    }
  }
]
```

Important response fields:

```json
{
  "SearchResult": {
    "channel": 0,
    "Status": [
      {
        "year": 2026,
        "mon": 9,
        "table": "111111111110000000000000000000"
      }
    ],
    "File": [
      {
        "name": "Mp4Record/2026-09-10/RecM02_DST20260910_000740_000804_6732828_983366.mp4",
        "size": "9974630",
        "type": "main",
        "StartTime": {
          "year": 2026,
          "mon": 9,
          "day": 10,
          "hour": 0,
          "min": 7,
          "sec": 40
        },
        "EndTime": {
          "year": 2026,
          "mon": 9,
          "day": 10,
          "hour": 0,
          "min": 8,
          "sec": 4
        }
      }
    ]
  }
}
```

`Status.table` is a per-day bitmap for the month.

Use `onlyStatus: 1` when only calendar availability is needed.

### Download exact stored recording

Command:

```text
Download
```

Illustrative URL:

```text
https://CAMERA_HOST/cgi-bin/api.cgi?cmd=Download&source=SOURCE_PATH&output=OUTPUT_NAME&token=TOKEN
```

Verified response behavior:

- HTTP 200 for a complete download.
- `Content-Type: application/octet-stream` (misspelled by the tested firmware
  as `apolication/octet-stream`).
- `Content-Length`.
- `Content-Disposition`.
- `ETag`.
- HTTP byte ranges.
- HTTP 206 and `Content-Range` for partial requests.

One verified download:

| Property | Result |
|---|---|
| Expected size | 25,186,873 bytes |
| Downloaded size | 25,186,873 bytes |
| Throughput | Approximately 3.29 MiB/s |
| Integrity | Local SHA-256 successfully computed |

Use `ETag`, expected length, and range requests to implement resumable
downloads.

### Current motion and AI state

Commands:

```text
GetMdState
GetAiState
```

Example `GetAiState` response:

```json
{
  "channel": 0,
  "dog_cat": {
    "alarm_state": 0,
    "support": 1
  },
  "face": {
    "alarm_state": 0,
    "support": 0
  },
  "people": {
    "alarm_state": 0,
    "support": 1
  },
  "vehicle": {
    "alarm_state": 0,
    "support": 1
  }
}
```

These are current state queries, not a durable historical event API.

### `GetEvents`

The probed RLC-810A returned `not support` for HTTP `GetEvents`.

Do not assume that an unsupported `GetEvents` command means the camera cannot
produce live notifications. Baichuan and ONVIF event mechanisms remain
available.

## Baichuan Protocol

Baichuan is Reolink's proprietary protocol, normally exposed on TCP port 9000.
It is used by Reolink applications and supported by `reolink-gateway` and
`reolink_aio`.

Do not reimplement Baichuan for the MVP. Use an existing maintained adapter and
isolate it behind an application interface.

### Live notifications

A persistent Baichuan session can receive live events such as:

- Motion start/end.
- Person.
- Vehicle.
- Animal/dog-cat.
- Visitor/doorbell.
- Package on supported devices.
- Other model-specific smart detections.

`reolink-gateway events stream` exposes this functionality.

Live notifications should be persisted immediately because the gateway event
buffer is not a durable database.

### Historical alarm search

`reolink_aio` uses Baichuan command IDs:

- 272: open historical recording/alarm search.
- 273: retrieve search results.
- 274: close the search handle.

Returned alarm records can include:

- `fileName`
- `alarmType`
- `startTime`
- `endTime`

Known categories include:

- `md`
- `pir`
- `io`
- `people`
- `face`
- `vehicle`
- `dog_cat`
- `visitor`
- `package`
- `cry`
- `crossline`
- `intrusion`
- `loitering`
- forgotten-item/legacy categories
- taken-item/loss categories

Support varies by model and firmware.

### Why historical search still matters

Live notifications can be missed during:

- Application restart.
- Gateway restart.
- Camera reboot.
- Network outage.
- Host outage.

The application must reconcile historical state after reconnecting. Live
notifications optimize latency; historical search guarantees eventual
completeness while the data remains on microSD.

## Official Reolink CLI and Gateway

Repository:

```text
https://github.com/reolink/reolink-cli
```

As observed on September 11, 2026, the latest release was v0.19.0, published
September 9, 2026.

Supported release platforms included:

- Linux x86-64.
- Linux x86-64 musl.
- Linux ARM64.
- Linux ARM64 musl.
- Linux ARMv6.
- Linux ARMv7.
- macOS ARM64.
- Windows x86-64.

### Installation

Official Linux installer:

```bash
curl -fsSL https://raw.githubusercontent.com/reolink/reolink-cli/main/install.sh | sh
```

For production deployment, pin a version and verify the published checksum
rather than installing an unpinned latest build.

### Initialize and start gateway

```bash
reolink-cli config init
reolink-cli gateway start --addr 127.0.0.1:9000
export REOLINK_GATEWAY_ADDR=127.0.0.1:9000
```

The gateway maintains camera sessions and exposes the protocol operations used
by the CLI. It is not a recording server.

### Add a camera securely

```bash
reolink-cli device add front \
  --host CAMERA_HOST:9000 \
  --user CAMERA_USER \
  --password-stdin
```

Do not put passwords directly in command-line arguments. They can be exposed
through process listings, logs, or shell history.

### Inspect camera

```bash
reolink-cli --camera front login
reolink-cli --camera front info
reolink-cli --camera front capabilities
reolink-cli --camera front storage status
reolink-cli --camera front record config get
```

### Search VOD

```bash
reolink-cli --camera front vod search \
  --from 2026-09-10T00:00:00 \
  --to 2026-09-10T23:59:59 \
  --stream main \
  --limit 0
```

The richer CLI/Baichuan result can include:

```json
{
  "channel": 0,
  "startTime": "2026-09-11T08:59:33",
  "endTime": "2026-09-11T09:00:03",
  "fileSize": 25186873,
  "name": "0120260911085933",
  "recordType": "md,vehicle",
  "streamType": "mainStream"
}
```

Supported record filters include:

- `manual`
- `sched`
- `io`
- `md`
- `people`
- `vehicle`
- `face`
- `dog_cat`
- `visitor`
- `other`
- `package`

### Download VOD

```bash
reolink-cli --camera front vod download \
  RECORDING_NAME \
  --file /archive/front/recording.mp4
```

Multiple recording names can be downloaded in one invocation to reuse the
device connection.

The CLI also supports camera-cut time-range downloads on compatible devices.
Current versions output timestamp-preserving MPEG-TS for these cuts. Whole-file
download by recording name returns the original stored MP4.

### Stream events

```bash
reolink-cli --camera front events stream \
  --types motion,people,vehicle,dog_cat
```

Normalized event records may resemble:

```json
{
  "timestamp": 1776765505,
  "channel": 0,
  "device": "CAMERA_HOST:9000",
  "eventType": "vehicle",
  "data": {
    "status": "MD",
    "aiType": "vehicle",
    "aiTypes": [
      "vehicle"
    ]
  }
}
```

The event stream is an immediate notification source, not permanent history.

## ONVIF

The cameras advertise ONVIF support and the tested camera had ONVIF enabled on
port 8000.

Useful ONVIF capabilities may include:

- Device discovery.
- Live stream configuration.
- PullPoint event subscription.
- Standardized motion/event messages.

Do not assume that generic ONVIF support includes ONVIF Profile G historical
recording search. Profile G conformance is model- and firmware-specific.

For this application:

- Baichuan is likely the richest Reolink-native event source.
- ONVIF can be an optional standardized fallback for live events.
- HTTP `Search`/`Download` remains the most direct documented recording-file
  interface.

## Recommended MVP Architecture

```text
                           +-------------------------+
                           | Reolink camera          |
                           | detection + microSD MP4 |
                           +------------+------------+
                                        |
                  live notifications    | historical VOD/event search
                                        |
                           +------------v------------+
                           | reolink-gateway adapter |
                           +------------+------------+
                                        |
                           +------------v------------+
                           | Linux archive service   |
                           |                         |
                           | - event processor       |
                           | - reconciliation        |
                           | - download manager      |
                           | - retention worker      |
                           +------+------------+-----+
                                  |            |
                        +---------v--+      +--v----------------+
                        | SQLite DB |      | Native MP4 archive |
                        +---------+-+      +-------------------+
                                  |
                           +------v------+
                           | Web API/UI  |
                           +-------------+
```

### Normal event flow

1. Maintain a persistent gateway session.
2. Receive a live event notification.
3. Insert/update a pending event in SQLite.
4. Wait for event end and the configured post-record interval.
5. Add a safety delay, initially 10-20 seconds.
6. Search a narrow camera-local time range around the event.
7. Match the finalized recording by time and trigger type.
8. Download to a `.partial` file.
9. Resume if interrupted.
10. Verify expected length and, when available, `ETag`.
11. Atomically rename to the final filename.
12. Mark the database recording as complete.

### Recovery flow

At startup or reconnect:

1. Read the last successful historical-search watermark.
2. Search from a safe overlap before that watermark.
3. Upsert all returned recordings/events.
4. Queue missing files.
5. Advance the watermark only after the result has been persisted.

During normal operation:

- Run an hourly short-window reconciliation.
- Run a daily 24-48 hour reconciliation.
- Never rely exclusively on live events.

### Why not continuously record RTSP for the MVP?

Mirroring completed camera recordings preserves:

- The camera's own pre-event buffer.
- The camera's post-event buffer.
- The camera's exact clip boundaries.
- The camera's detection classifications.
- The original stored MP4.
- SD card as an independent outage cache.

It also avoids:

- A continuously running FFmpeg process.
- Duplicate recording logic.
- Additional segmentation decisions.
- Reconstructing event-to-video relationships.
- GPU or object-detection requirements.

Direct RTSP continuous recording can be added later if 24/7 footage is needed.

## Suggested Persistence Model

SQLite in WAL mode is sufficient for approximately five cameras and one
ingestion service.

### `cameras`

Suggested fields:

```text
id
name
host
channel
timezone
enabled
last_event_at_utc
last_search_watermark_utc
last_success_at_utc
created_at_utc
updated_at_utc
```

Credentials should not be stored directly in this table. Use:

- Container secrets.
- Environment-file secrets with restrictive permissions.
- A platform secret store.
- An external credential provider.

### `recordings`

Suggested fields:

```text
id
camera_id
native_name
native_source_path
stream_type
start_at_utc
end_at_utc
camera_local_start
camera_local_end
expected_bytes
etag
record_type_raw
final_path
download_state
downloaded_bytes
sha256
retry_count
last_error
raw_search_json
created_at_utc
updated_at_utc
```

Recommended unique key:

```text
(camera_id, native_name, expected_bytes)
```

If firmware produces unstable names, add a fallback uniqueness key based on:

```text
(camera_id, stream_type, start_at_utc, end_at_utc, expected_bytes)
```

### `recording_tags`

Suggested fields:

```text
recording_id
tag
source
```

Examples:

```text
motion
person
vehicle
animal
visitor
package
scheduled
```

`source` should identify:

- Baichuan historical alarm.
- Filename flags.
- Live notification.
- HTTP search inference.

### `events`

Suggested fields:

```text
id
camera_id
event_type
started_at_utc
ended_at_utc
status
raw_payload_json
matched_recording_id
created_at_utc
updated_at_utc
```

### `ingestion_runs`

Suggested fields:

```text
id
camera_id
run_type
window_start_utc
window_end_utc
started_at_utc
completed_at_utc
records_seen
records_inserted
records_updated
files_queued
status
error
```

This table makes reconciliation observable and debuggable.

## File Layout

Suggested archive layout:

```text
/archive/
  front/
    2026/
      09/
        10/
          20260910T000740-20260910T000804-md.mp4
          20260910T000740-20260910T000804-md.json
```

The sidecar should contain:

```json
{
  "schemaVersion": 1,
  "camera": "front",
  "channel": 0,
  "nativeName": "NATIVE_RECORDING_NAME",
  "nativeSourcePath": "Mp4Record/...",
  "streamType": "main",
  "startTimeUtc": "2026-09-10T07:07:40Z",
  "endTimeUtc": "2026-09-10T07:08:04Z",
  "cameraLocalStart": "2026-09-10T00:07:40",
  "cameraLocalEnd": "2026-09-10T00:08:04",
  "recordTypes": [
    "motion"
  ],
  "expectedBytes": 9974630,
  "etag": "CAMERA_ETAG",
  "sha256": "LOCAL_SHA256",
  "raw": {
    "search": {},
    "historicalAlarm": {}
  }
}
```

Do not make sidecars the only metadata store. SQLite supports efficient
searching; sidecars make each file independently portable.

## Download Reliability Requirements

Use this state machine:

```text
discovered
queued
downloading
verifying
complete
retry_wait
failed
deleted
```

Required behaviors:

- Download into `.partial`.
- Persist downloaded byte count.
- Resume with HTTP `Range`.
- Validate `Content-Range`.
- Validate total expected length.
- Validate `ETag` when stable and available.
- Optionally calculate SHA-256 after completion.
- Flush file data before final rename when durability is important.
- Rename atomically within the same filesystem.
- Never expose a partial file as complete.
- Back off retries with jitter.
- Distinguish transient network errors from permanent not-found/overwritten
  recordings.

If the recording has already been overwritten on microSD:

- Mark it as missed/expired.
- Keep its metadata.
- Do not repeatedly retry forever.
- Surface the gap in health status.

## Matching Notifications to Recordings

The live notification may not contain the final microSD filename.

Use:

- Camera/channel.
- Event start/end.
- Detection type.
- A configurable pre-event tolerance.
- A configurable post-event tolerance.

Initial matching approach:

1. Search from event start minus 30 seconds.
2. Search through event end plus post-recording duration plus 30 seconds.
3. Score candidates by temporal overlap.
4. Prefer candidates containing the same native trigger type.
5. Allow one recording to contain multiple overlapping event categories.
6. Persist ambiguous matches for later reconciliation rather than silently
   choosing a poor match.

## Browser Playback Constraint

RLC-810A main recordings are commonly 4K H.265/HEVC.

Browser support for HEVC is inconsistent across operating systems and browsers.
The application should:

- Always preserve the original native MP4.
- Use JPEG thumbnails or the H.264 substream for previews.
- Detect browser playback capability.
- Offer direct download/open-in-native-player.
- Add on-demand remux/transcode only when needed.

Do not transcode every archived recording by default. It would add substantial
CPU/GPU load and storage overhead.

## Security Requirements

- Keep the gateway bound to loopback unless LAN access is explicitly required.
- Do not expose camera credentials through query logs.
- Do not put passwords on command lines.
- Redact tokens and credentials from structured logs.
- Use a read-only camera account if Reolink capabilities permit the required
  search/download/event operations.
- Treat raw API responses as potentially sensitive.
- Protect the archive UI with authentication if exposed beyond a trusted LAN.
- Use least-privilege container mounts.
- Do not expose microSD format or destructive camera operations in the MVP.
- Use pinned CLI versions and verified checksums.

## Operational Health Metrics

Per camera, expose:

- Connection state.
- Last live notification.
- Last successful historical search.
- Reconciliation watermark.
- Oldest currently searchable camera recording.
- Approximate SD retention window.
- Number of queued downloads.
- Number of failed downloads.
- Oldest pending download.
- Archive bytes per day.
- Archive disk free space.
- Last camera clock offset measurement.

Global metrics:

- Ingestion lag.
- Download throughput.
- Retry rate.
- Missing/expired recording count.
- Database size.
- Archive growth rate.

## Recommended MVP Scope

### Include

- One RLC-810A initially.
- Linux container deployment.
- Persistent gateway session.
- Live event ingestion.
- Historical reconciliation.
- Native MP4 download.
- SQLite metadata.
- JSON sidecars.
- Resumable downloads.
- Configurable retention.
- Minimal web calendar/event list.
- Filters for motion, person, vehicle, and animal.
- Direct playback when supported and file download otherwise.
- Health page.

### Exclude

- New object detection.
- GPU inference.
- Face recognition.
- License-plate recognition.
- Continuous RTSP recording.
- Reimplementation of Baichuan.
- Mobile applications.
- Cloud relay.
- Multi-user permissions beyond basic authentication.
- Full parity with Reolink Client.
- Camera configuration writes.
- SD-card formatting.

## MVP Acceptance Criteria

The MVP is successful when:

1. A new camera event appears in the archive within approximately two minutes.
2. Its native detection tags are searchable.
3. Its downloaded MP4 matches the expected byte count.
4. Interrupted downloads resume rather than restart.
5. Restarting the service creates no duplicate database rows or files.
6. A service/network outage is automatically caught up from microSD.
7. Retention deletes both file and metadata consistently.
8. Failed or overwritten recordings are visible as gaps.
9. Credentials and tokens do not appear in logs.
10. The archive remains usable without the Reolink desktop client.

## Technology Guidance

No Python is required.

Good implementation options:

### .NET

Strengths:

- Strong background-service model.
- Mature ASP.NET Core web API/UI stack.
- Excellent SQLite support.
- Straightforward Linux containers.
- Strong typing and robust HTTP streaming.

Potential structure:

```text
ReolinkArchive.Domain
ReolinkArchive.Infrastructure
ReolinkArchive.Worker
ReolinkArchive.Web
ReolinkArchive.Tests
```

### Go

Strengths:

- Small static Linux binaries.
- Simple deployment.
- Good concurrency and streaming I/O.
- Low resource use.

Either is suitable. The CLI/gateway integration should be hidden behind an
interface so the rest of the application does not depend directly on command
output details.

Example abstraction:

```text
IReolinkAdapter
  SubscribeEvents(camera)
  SearchRecordings(camera, start, end)
  SearchHistoricalEvents(camera, start, end)
  OpenRecording(camera, recording, offset)
  GetStorageStatus(camera)
  GetCameraTime(camera)
```

## Testing Strategy

### Unit tests

- Timestamp and DST conversion.
- Filename trigger decoding.
- Event-to-recording matching.
- Unique-key/idempotency behavior.
- Retry state transitions.
- Retention calculations.
- CLI JSON parsing.

### Integration tests

- Fake CLI/gateway process with captured JSON fixtures.
- HTTP server supporting `Range`, `ETag`, disconnects, and truncated responses.
- SQLite restart/recovery.
- Atomic finalization.
- Duplicate reconciliation windows.

### Live acceptance tests

- Trigger person, vehicle, animal, and generic motion events.
- Disconnect network during download.
- Restart the service while events occur.
- Reboot the camera.
- Stop gateway and restart it.
- Verify catch-up from historical index.
- Verify no duplicates.
- Verify an intentionally partial download resumes.

Avoid making live camera access mandatory for the ordinary automated test
suite.

## Important Caveats

- Baichuan is proprietary and may change.
- Filename layouts are version-dependent.
- Preserve raw responses for future reprocessing.
- Detection types differ across models.
- HTTP API availability differs across firmware.
- Some cameras expose different download behavior.
- Camera-local time and DST must be handled carefully.
- An event notification does not guarantee the MP4 is finalized yet.
- A completed MP4 can still disappear later when microSD loop overwrite reaches
  it.
- Reconciliation frequency must stay comfortably inside the shortest observed
  SD retention window.
- ONVIF support does not imply ONVIF Profile G recording-search support.
- Reolink Client behavior is not an official stable API contract.

## Source References

### Reolink

- Official Reolink CLI:
  `https://github.com/reolink/reolink-cli`
- Reolink CLI releases:
  `https://github.com/reolink/reolink-cli/releases`
- Camera HTTP API Guide v8:
  `https://cdn.reolink.com/shop/public/media/1764830053906-5Uwr.pdf`
- RLC-810A:
  `https://reolink.com/product/rlc-810a/`
- Protocol support matrix:
  `https://support.reolink.com/articles/900000617826-Which-Reolink-Products-Support-CGI-RTSP-ONVIF/`
- Reolink Client platform support:
  `https://support.reolink.com/articles/360012466173-Introduction-to-Reolink-Client-Versions/`
- Reolink playback filtering:
  `https://support.reolink.com/articles/15785632026009-How-to-Filter-out-Motion-Alarm-triggered-Videos-from-Continuous-Recordings-When-Playing-back-via-Reolink-App-or-Client/`

### Open-source implementations

- `reolink_aio`:
  `https://github.com/starkillerOG/reolink_aio`
- Home Assistant Reolink integration:
  `https://github.com/home-assistant/core/tree/dev/homeassistant/components/reolink`

### ONVIF

- Profile G:
  `https://www.onvif.org/profiles/profile-g/`
- Recording Search Service:
  `https://www.onvif.org/specs/srv/rsrch/ONVIF-RecordingSearch-Service-Spec.pdf`
- Conformant products:
  `https://www.onvif.org/conformant-products/`

## Suggested Prompt for Another LLM

```text
Use the attached Reolink technical handoff as the authoritative project
context. Design or implement a Linux-native archive service that mirrors
completed native Reolink microSD event recordings and their detection metadata
to local disks.

Do not add object detection or continuously recapture RTSP for the MVP. Use
Reolink's existing detection, recording, event stream, historical VOD search,
and native download capabilities. Treat live notifications as a low-latency
signal and historical reconciliation as the source of eventual completeness.

Preserve raw metadata, use SQLite for normalized indexing, write downloads to
.partial files, support byte-range resume, verify expected length and ETag, and
atomically finalize files. Keep credentials out of logs and command-line
arguments. Isolate reolink-cli/reolink-gateway behind an adapter because
Baichuan and filename layouts are proprietary.

Start with one RLC-810A and provide a minimal event timeline with motion,
person, vehicle, and animal filters. Clearly identify any design decision that
would make the application difficult to migrate or expand.
```
