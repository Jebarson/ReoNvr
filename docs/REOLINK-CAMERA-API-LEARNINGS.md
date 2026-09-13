# Reolink Camera API Learnings

This document records camera and protocol observations used by ReoNvr. It is
research context, not a second application specification. Current product
behavior is defined in [APPLICATION-SPEC.md](APPLICATION-SPEC.md).

No credentials are included. Examples use placeholders and must be run with
credentials supplied through secure input.

## Verified camera baseline

The live probe used one RLC-810A running firmware
`v3.1.0.956_24022103` with hardware `IPC_523128M8MP`.

| Property | Observed value |
|---|---|
| Channels | 1 |
| Storage | 1 microSD slot |
| HTTPS | Enabled |
| RTSP | Enabled on port 554 |
| ONVIF | Enabled on port 8000 |
| Baichuan | Enabled on port 9000 |
| Main stream | 3840 x 2160 H.265, 6144 Kbit/s |
| Sub stream | 640 x 360 H.264, 256 Kbit/s |
| Pre-recording | Enabled, observed 10 seconds |
| Post-recording | 15 seconds |

The camera reported motion, person, vehicle, and animal/dog-cat detection.
Face, package, and cry support vary by model and firmware and were unavailable
on the tested camera.

HTTP recording search/download, Baichuan VOD search, live event streaming,
RTSP, and current AI/motion state queries worked. HTTP `GetEvents` and
Baichuan historical alarm command 516 were unsupported by this firmware.

## Verified microSD behavior

At probe time, the approximately 244 GB card had loop overwrite enabled and
retained about 10.6 days of event recordings. Continuous scheduled camera
recording was disabled. Motion, person, vehicle, and animal schedules were
enabled all week.

One observed full day contained 859 recordings totaling 21.60 GB. Storage
consumption therefore depends strongly on scene activity and camera policy;
camera capacity must not be presented as local archive capacity.

ReoNvr treats the camera as read-only. It does not change schedules, enable
continuous recording, alter codecs, format storage, or delete camera media.

## Native recordings and markers

The HTTP recording index can return native files such as:

```text
Mp4Record/2026-09-10/RecM02_DST20260910_000740_000804_6732828_983366.mp4
```

The stored file is an ISO Base Media MP4 containing camera-encoded video and
audio. Detection metadata is not known to be standard timed MP4 metadata. The
camera exposes detection information through recording-index flags and, on
some firmware, a separate Baichuan alarm index.

Known `RecM02` filename bit positions on the tested camera were:

| Meaning | Bit |
|---|---:|
| Person | 17 |
| Face | 18 |
| Vehicle | 19 |
| Animal | 20 |
| Scheduled recording | 23 |
| Generic motion | 24 |
| RF recording | 25 |
| Doorbell/visitor | 26 |
| Other AI | 27 |

These layouts are proprietary and version-dependent. ReoNvr preserves native
names and raw redacted metadata so future parsing changes do not require
changing the original media.

The VOD path used by the official CLI may instead expose an extensionless
identifier such as `0120260911085933`. Tested payloads with this naming pattern
still contain a valid MP4 `ftyp` signature. ReoNvr recognizes that signature,
serves `video/mp4`, and adds `.mp4` to generated paths and download filenames.

## HTTP camera API

Firmware varies between these endpoints:

```text
https://CAMERA_HOST/cgi-bin/api.cgi
https://CAMERA_HOST/api.cgi
```

The tested RLC-810A accepted `/cgi-bin/api.cgi`. Cameras commonly use
self-signed certificates. Credentials must not be placed in command arguments,
logs, or persisted query URLs.

Useful commands include:

| Command | Purpose |
|---|---|
| `Login` | Create a short-lived camera token |
| `GetDevInfo` | Model, firmware, hardware, channels, and storage slots |
| `GetTime` | Camera-local time, UTC offset, and DST rules |
| `GetHddInfo` | Camera storage mount, format, and capacity state |
| `GetRecV20` | Current recording/overwrite/pre/post policy |
| `Search` | Recording day availability and file metadata |
| `Download` | Exact native file transfer |
| `GetMdState` | Current motion state |
| `GetAiState` | Current supported AI categories and state |

### Time handling

Camera search parameters are local naive timestamps. ReoNvr uses `GetTime` and
the discovered DST rules to convert recording and event timestamps to UTC.
Database and API timestamps remain UTC; browser-local conversion is only a
presentation concern.

### Recording search

`Search` accepts channel, stream, and local start/end structures. The result
can include a monthly day bitmap plus each recording's name, size, stream,
start, and end. Availability and metadata differ across firmware.

Camera Storage reconciliation uses overlapping searches and a persisted UTC
watermark. Results are upserted by camera and native name, then eligible rows
are queued newest first. Repeated discovery must not duplicate media.

### Exact download

The tested `Download` response supplied `Content-Length`,
`Content-Disposition`, `ETag`, byte-range support, and `206 Content-Range`.
Its content type was a misspelled variant of `application/octet-stream`.

Although the camera protocol supports range requests, the current ReoNvr
downloader restarts a stale partial transfer rather than resuming it. It writes
to `.partial`, checks the minimum expected length, computes SHA-256, flushes,
and atomically renames on the same filesystem. The minimum-length check permits
larger transfers because tested camera size metadata can be stale.

## Baichuan protocol

Baichuan is Reolink's proprietary protocol, normally on TCP port 9000. ReoNvr
does not reimplement it; the pinned official gateway and CLI are isolated
behind an adapter.

### Live events

`reolink-gateway events stream` can deliver motion, person, vehicle, animal,
visitor, package, and model-specific event state. Support is capability-driven.
The stream is immediate notification, not durable history, so ReoNvr persists
normalized events before attempting media association.

### VOD and historical events

The official CLI's Baichuan VOD search returns recording ID, local start/end,
size, stream, and record types such as `md`, `people`, `vehicle`, and
`dog_cat`. Historical alarm capabilities vary. On the tested firmware, VOD
search and filename flags are the reliable reconciliation sources.

Live notifications can be missed during app, gateway, camera, network, or host
outages. Overlapping VOD reconciliation recovers recordings that remain on the
SD card.

## Official CLI and gateway

ReoNvr currently pins Reolink CLI/gateway v0.19.0 in the container build and
verifies the release checksum. The gateway is a session/protocol bridge, not
an NVR, and is reachable only on the private Compose network.

Representative commands:

```bash
reolink-cli device add front \
  --host CAMERA_HOST:9000 \
  --user CAMERA_USER \
  --password-stdin

reolink-cli --camera front info
reolink-cli --camera front capabilities
reolink-cli --camera front vod search \
  --from 2026-09-10T00:00:00 \
  --to 2026-09-10T23:59:59 \
  --stream main \
  --limit 0
reolink-cli --camera front vod download RECORDING_NAME \
  --file /archive/recording.mp4
reolink-cli --camera front events stream \
  --types motion,people,vehicle,dog_cat
```

ReoNvr gives every adapter operation a timeout and supplies passwords through
standard input. It never exposes the gateway port or persists gateway tokens.

## ONVIF

The tested camera advertised ONVIF on port 8000. ONVIF can provide device
discovery, stream configuration, and PullPoint event subscriptions, but generic
ONVIF support does not imply Profile G historical recording search.

ReoNvr currently uses the official Reolink CLI/gateway rather than ONVIF for
camera operations. HTTP or Baichuan recording search remains necessary for the
tested model.

## Current archive behavior

Camera Storage and RTSP use the same path planner. The operator chooses one of
four layouts:

```text
CameraDate: folder/Camera-shortid/YYYY/MM/DD/file
DateCamera: folder/YYYY/MM/DD/Camera-shortid/file
Camera:     folder/Camera-shortid/file
Date:       folder/YYYY/MM/DD/Camera-shortid-file
```

The host archive directory is configured by `REONVR_RECORDINGS_PATH` and
mounted as `/archive`. The in-app recording folder is a validated relative
path beneath that mount. Existing indexed files are not moved when settings
change.

SQLite is the metadata source of truth. Camera Storage and RTSP publication
also write portable JSON sidecars beside media; retention treats each sidecar
as part of the recording artifact set. The current implementation does not
generate thumbnails. Public lists expose only complete local media and
media-backed events.

## Browser playback

Tested main-stream MP4 files contain 4K H.265/HEVC video and AAC audio. Browser
support depends on both browser and operating-system codecs. Chrome played the
tested files on the Windows development host; Edge produced audio-only playback
because the Microsoft HEVC decoder was not installed.

ReoNvr preserves the original media, serves HTTP ranges, and provides original
download. It does not currently generate H.264 previews, thumbnails, remuxed
copies, or transcoded derivatives.

## Security and reliability constraints

- Use `--password-stdin`; never put passwords on command lines.
- Redact credentials, tokens, authenticated URLs, and sensitive raw payloads.
- Keep the gateway private and expose only the ReoNvr HTTP port.
- Prefer a camera account with only the read capabilities needed by the
  adapter, where firmware permits it.
- Treat an event notification as provisional until a finalized recording is
  discovered.
- Expect files to disappear when camera loop overwrite reaches them.
- Keep reconciliation windows comfortably inside the shortest observed SD-card
  retention period.
- Never expose format, delete, schedule-change, or stream-change operations.

## Important caveats

- Baichuan and filename layouts are proprietary and may change.
- Detection and historical-search support differ by model and firmware.
- HTTP commands and response shapes differ across firmware generations.
- Camera-local time and DST require explicit handling.
- Self-signed camera certificates require a deliberate trust policy.
- ONVIF support does not guarantee Profile G support.
- Native HEVC support is not portable across browsers.

## References

- Reolink CLI: <https://github.com/reolink/reolink-cli>
- Reolink CLI releases: <https://github.com/reolink/reolink-cli/releases>
- Camera HTTP API Guide v8:
  <https://cdn.reolink.com/shop/public/media/1764830053906-5Uwr.pdf>
- Reolink protocol support matrix:
  <https://support.reolink.com/articles/900000617826-Which-Reolink-Products-Support-CGI-RTSP-ONVIF/>
- Reolink Client platform support:
  <https://support.reolink.com/articles/360012466173-Introduction-to-Reolink-Client-Versions/>
- `reolink_aio`: <https://github.com/starkillerOG/reolink_aio>
- Home Assistant Reolink integration:
  <https://github.com/home-assistant/core/tree/dev/homeassistant/components/reolink>
- ONVIF Profile G: <https://www.onvif.org/profiles/profile-g/>