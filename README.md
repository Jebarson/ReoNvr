# ReoNvr

ReoNvr is a self-hosted Reolink recording archive. It records native RTSP
streams, optionally recovers missed recordings from camera storage, indexes
camera detections, and provides local browser playback without transcoding.

ReoNvr reads camera configuration and media. It never changes camera recording
schedules, formats camera storage, or deletes files from a camera.

## Recording modes

| Mode | Live recording | Optional camera-storage recovery |
|---|---|---|
| Events | Retain stream-copy segments around selected camera events | Recover matching native event clips from monitored intervals |
| Continuous | Retain every stream-copy segment | Recover native clips overlapping missing local coverage |

RTSP is always the primary source. **Recover missed recordings from camera
storage** is available after a connection test confirms usable storage and
recording search. It does not import the camera's entire history or change its
recording schedule. Recovery requires the camera to have recorded and retained
the missing footage. Keep the camera and ReoNvr host clocks synchronized.

Public Playback and Events views use local archive media, not camera-only
history. Recordings appear after their local media is complete. Event visibility
requires verified video coverage, not just recording time bounds. Older files
without inspected coverage remain playable but do not establish event backing.

## Prerequisites

- Docker Engine with Docker Compose v2, or Docker Desktop on Windows/macOS.
- A Reolink camera reachable from the Docker host on its Baichuan and selected
	media ports.
- A camera account allowed to read device information, events, recordings, and
	RTSP streams.
- A writable host directory with enough space for the archive.
- A modern browser. Native main-stream recordings are commonly H.265/HEVC;
	Chrome works on the tested Windows host. Microsoft Edge requires the HEVC
	Video Extensions package when Windows does not already provide an HEVC
	decoder.

## Installation

1. Clone the repository and enter it:

	 ```sh
	 git clone https://github.com/Jebarson/ReoNvr.git
	 cd ReoNvr
	 git switch initial-setup
	 ```

2. Optionally choose a host recording directory:

	 ```sh
	 cp .env.example .env
	 ```

	 Edit `REONVR_RECORDINGS_PATH` in `.env`. The default is
	 `./runtime/archive`. Create a custom directory before starting Docker.

3. Build and start the application:

	 ```sh
	 docker compose up -d --build
	 ```

4. Confirm the app and gateway are healthy and the discovery relay is running:

	 ```sh
	 docker compose ps
	 ```

5. Open <http://localhost:8080>.

Runtime data is stored in bind-mounted directories and survives container
rebuilds. The gateway is private to the Compose network. The app publishes TCP
`8080` for the UI and UDP `3000` for discovery replies. The bundled relay listens
on UDP `2000` in the Docker host's network namespace; on Docker Desktop this is
the Linux VM, not a Windows installation or a Docker Desktop settings change.

## First use

1. Open **Cameras** and select **Detect Automatically**, then **Add camera**
	 beside a detected device. For manual entry, select the toolbar's **Add camera**.
2. Review the camera name and LAN address, then enter the username and password.
3. Choose the stream quality and select **Test connection**. ReoNvr discovers
	 the camera model, timezone, streams, storage/search support, and event
	 categories without changing the camera. Changing stream quality requires
	 another test.
4. Choose Events or Continuous and event categories. Optionally enable
	 **Recover missed recordings from camera storage**, then save.
5. Open **Settings** to choose the recording subfolder and organization:
	 - Camera, then day
	 - Day, then camera
	 - Camera only
	 - Day only
6. Enable the application-wide **Recording** switch when ingestion should run.

Clock warnings are advisory: you can keep the camera's current settings and save
it for live RTSP recording. Custom rules alone do not trigger a warning; the
camera's reported current time must differ by more than one minute after
applying its clock rules. ReoNvr uses the camera's reported offset and
daylight-saving rules for camera-storage timestamps, even without a matching
named timezone. If those rules cannot be verified, only camera-storage recovery
is unavailable; reviewing or changing the camera settings in the Reolink app is
your choice. ReoNvr never changes them automatically.

The Settings folder is relative to the mounted `/archive` root. To move the
host archive, change `REONVR_RECORDINGS_PATH` and recreate the app container.
Folder and layout changes apply only to new media; indexed older files retain
their existing paths.

## Daily operation

- **Cameras** shows connectivity, RTSP mode, local media count, and recovery
	status. Select the recovery status to inspect intervals and unresolved time.
- **Detect Automatically** is available beside **Add camera** at any time.
	It only scans for devices; selecting one prefills its name, address, and port.
	Credentials and a successful connection test are still required before saving.
- **Playback** lists only completed local recordings and supports date/event
	filters, seeking, and original-file download.
- **Settings** controls the recording subfolder and folder layout.
- The global **Recording** switch pauses recovery, downloads, event ingestion,
	and RTSP recording without changing camera policies. Deliberately paused
	intervals are not scheduled for later catch-up.
- Camera health, database maintenance, and local retention continue while
	recording is paused. Pause is not a storage freeze; worker status can retain
	`Stopped` history after ingestion ends.
- Retention removes the oldest completed local recordings when free space
	falls below the calculated 30-minute reserve. It never deletes camera media.

Camera-storage downloads preserve their camera codec and container. RTSP capture
copies the native streams into Matroska without transcoding; Continuous mode
targets one-minute segments, with cuts at available keyframes. **Original**
downloads the exact archived file. Extensionless native Reolink MP4 files are
served and downloaded with an `.mp4` filename.

Recovery compares saved recording intent with verified media coverage using a
two-second tolerance. A downloaded file alone does not establish full recovery.
Partial coverage remains unresolved; an unobserved event interval with no
matching camera history stays **Unknown**, not recovered. Recent footage is
left provisional for two minutes in Continuous mode or ten minutes in Events
mode. Recovery keeps whole native clips, which can overlap live recordings;
Playback remains a recording list, not a seamlessly stitched timeline.

Upgrading preserves completed local media. Existing camera-storage policies
become RTSP-primary with recovery enabled from the migration boundary, without
automatically importing older camera history. Legacy queued or in-progress
camera-storage transfers become metadata-only and are not resumed by that old
queue. Review camera recovery policies after upgrading; old pending files are
not guaranteed to be recovered, and no camera SD media is removed.

## Reliability and limits

Recording-control updates apply in order. Events mode periodically publishes
long events and flushes valid buffered footage on pause or stream interruption.
RTSP publication journals reconcile unfinished media/index/coverage commits at
startup or the next camera-worker retry. Replays verify the native file hash
and do not resurrect intentionally retained tombstones. Retention leaves other
queued clips in the same recovery interval eligible for download.

A forced process/host shutdown can still lose the unjournaled active tail.
Missing or corrupt journaled media is reported rather than marked recovered.
Unindexed files from versions before publication journaling are not discovered
automatically. Physical outages, power loss, full disks, and destructive
retention drills remain separate from the deterministic regression tests.

See [docs/APPLICATION-SPEC.md](docs/APPLICATION-SPEC.md#12-review-fixes-and-validation-limits)
for the addressed review cases and remaining validation limits. Stop the app
container for consistent filesystem backups; recording pause alone leaves
retention and database maintenance running.

## Updating and stopping

Update and rebuild:

```sh
git pull
docker compose up -d --build
```

Stop containers without deleting data:

```sh
docker compose down
```

Do not add `-v`; ReoNvr uses bind mounts, but destructive volume commands are
not part of the supported update workflow.

## Fresh setup replica

An independent fresh-install replica runs at <http://localhost:8081> using
`compose.fresh.yaml`. Its app and gateway use local snapshot image tags, a
separate Docker network, and separate named volumes for the database, encryption
key, archive, and gateway state. It does not copy cameras, credentials, settings,
or media from the installation on port `8080`. Port `8081` is bound only to the
local machine.

Start the prepared replica:

```sh
docker compose -f compose.fresh.yaml up -d --wait
```

The fresh stack includes the discovery relay and owns host UDP `3000`. Only one
installation or discovery client can own that fixed reply port at a time. The
original running instance can remain online, but do not recreate it with the
updated main Compose file while the fresh instance owns the port.

Recording has been left paused for setup review. Add cameras through the normal
UI when ready. Changes and camera registrations in this replica persist in its
own volumes across restarts.

Stop only the replica, preserving its data:

```sh
docker compose -f compose.fresh.yaml down
```

## Acceptance testing

The Docker-only Windows runner is `web/e2e/run.ps1`. It runs the Go suite,
frontend lint/build, runtime FFmpeg checks, read-only live SD checks, and
Playwright UX tests. No host Go, npm, Node, or browser installation is needed.

Live acceptance is an opt-in **local camera lab profile**, not a generic CI smoke test:

- Start the fresh stack with current snapshot images. Its visible camera list
	must be empty and global recording paused. Never point it at the original app.
- Use `internal/testconfig/config.example.json` as the template for the ignored
	`runtime/test-config.json`. The example has blank camera addresses, usernames,
	and passwords. Fill in the local file yourself; never commit or publish it.
- Configure at least two reachable cameras with unique IDs. `probeCamera` selects
	the clock/snapshot probe and `storageCamera` selects the SD/RTSP test camera.
	Set each camera's host, Baichuan port, and channel for the current network.
	Browser registration currently requires channel zero; the Go live check also
	accepts configured NVR channels. All live cameras share the configured username
	and password. No particular camera model, subnet, or fixed camera count is required.
- The helper validates the local file and pipes it directly to test containers
	over stdin. It no longer reads the original database or encryption key. Never
	run the helper standalone, capture its stdout, or put passwords in arguments,
	environment variables, traces, or HAR recordings. The plaintext local file is
	read-only mounted, excluded from Git/Docker builds, and must be kept private.
- The fresh image must include the code under test. The runner does not rebuild
	or redeploy either installation automatically.

From the repository root in PowerShell:

```powershell
docker build -f docker/app.Dockerfile -t reonvr-fresh-app:snapshot .
docker compose -f compose.fresh.yaml up -d --wait
.\web\e2e\run.ps1
```

Use `-Config <path>` to select another private local JSON file. Run
`.\web\e2e\run.ps1 -SkipLive` for Go, frontend, and FFmpeg checks without a
configuration file, camera credentials, camera access, or recording mutations.
Blank live credentials fail before contacting the fresh app or any camera.
Unit tests load synthetic addresses from the example's `fixtures` section and
generate blank fixture usernames/passwords in memory rather than embedding them
in source. `REONVR_TEST_CONFIG` can select an alternative fixture configuration
for individual containerized Go or recovery-browser runs.

The runner installs pinned frontend dependencies into a named Docker volume.
It registers temporary cameras through the UI, checks all storage layouts,
records the configured storage camera's native substream to verify a full continuous
segment, then checks playback, seeking, original-download byte identity, and
HTTP range handling. Screenshots cover desktop, tablet, and 390/320-pixel mobile
layouts and are written under ignored `runtime/e2e-*.png`, with password inputs
masked. Screenshots can contain private camera images; do not publish them.

The browser suite restores local settings, removes its temporary cameras, and
pauses recording. An independent container also pauses recording after five
minutes, and the runner's finalizer requests pause again. Native test recordings
are intentionally retained in the isolated fresh archive after camera removal;
this also exercises archive ownership preservation. The original installation
is not mutated. Camera settings and camera SD media are never changed or deleted.

Only application assets under `web/public` or `web/src/assets` and intentional
manual illustrations under `docs/images` belong in Git. Test screenshots,
camera snapshots, recordings, reports, and private configuration remain ignored.

Live coverage includes discovery/authentication/snapshots for configured cameras,
camera CRUD/policies, actual clock parsing, native SD search/download/codec
inspection, bounded RTSP recording, and browser playback. Backend tests cover
recovery/retention algorithms, leases, worker lifecycle, validation, and credential
safety. Mocked browser responses exercise unavailable/pending/failed media,
unsupported-codec fallback, filters, connection failures, and optional clock
warnings without causing real camera faults.

This does not certify physical outage recovery, actual AI-event triggering,
SD removal, full-disk behavior, destructive retention, long-running load, or
every browser/host platform. Those need separate controlled drills; the suite
does not simulate a real outage by changing camera settings or disrupting the LAN.

## Backup

Pausing recording does not stop retention or database maintenance. For a
consistent filesystem backup, first pause recording in the UI, then stop the
app container before copying data:

```sh
docker compose stop app
```

Back up these items together while the app remains stopped:

- The complete `runtime/data` directory, including SQLite WAL files when present
- `runtime/secrets/master.key`
- The directory configured by `REONVR_RECORDINGS_PATH`

The database and master key are a pair. Losing the key makes saved camera
passwords unrecoverable. Keep backup keys private. Restart with
`docker compose start app`; the persisted recording pause remains in effect.

To restore, stop the app and preserve the current data before replacing
anything. Restore the database directory, matching key, and archive from the
same backup generation to their configured mounts, preserving write permissions
for container user `10001`. Do not mix restored database files with unrelated
WAL files. Start the app, confirm readiness, check camera registrations and
playback, and leave recording paused until the restored state has been reviewed.
For the fresh replica, use its separate named volumes and
`docker compose -f compose.fresh.yaml` commands; do not restore over the original
installation or assume its bind-mount paths apply to the replica.

## Security

- Camera passwords are encrypted locally with AES-256-GCM.
- Passwords are supplied to Reolink tools through standard input, not argv.
- Passwords, ciphertext, and authenticated media URLs are not returned by the
	API or written to logs.
- Keep port `8080` on a trusted LAN or place ReoNvr behind an authenticated
	reverse proxy. Built-in multi-user authentication is not implemented.
- Never commit `.env`, `runtime/`, database files, archive media, or master
	keys.

## Troubleshooting

Check service health and recent logs:

```sh
docker compose ps
docker compose logs --tail 200 app gateway
```

If a native H.265 recording plays audio but shows no video, use a browser with
HEVC support or install the operating system's HEVC decoder. ReoNvr deliberately
does not transcode archived media.

Automatic detection includes a credential-free relay for common home networks.
It uses private IPv4 interface broadcasts and these common `/24` broadcast
addresses by default: `10.0.0.255`, `10.0.1.255`, and `192.168.{0,1,4,8,31,50,68,86,100,178,254}.255`.
The relay forwards only the native four-byte discovery probe, at most once per
second; it does not scan address ranges, authenticate, or change camera settings.

For other networks, set `REONVR_DISCOVERY_BROADCASTS` in `.env` to a
comma-separated list such as `192.168.20.255,10.20.255.255`, then recreate the
relay with `docker compose up -d discovery-relay`. This replaces the defaults;
up to 32 private or link-local IPv4 destinations are accepted. Use the fresh
Compose file instead when testing the replica.

UDP `3000` must be available to the app and permitted by the host firewall. If
Docker reports that the port is allocated, stop the other discovery receiver
before starting this stack. VLAN isolation, unusual subnets, firewalls, and
sleeping cameras can still prevent replies. Manual entry remains available.

## Development

Build and test in containers; no host Go, Node.js, browser, or camera CLI
installation is needed. The container build uses Go 1.27 and Node.js 26 LTS.
Run the following commands from the repository root in PowerShell.

Backend tests:

```powershell
docker run --rm -v "${PWD}:/src" -v reonvr-go-mod:/go/pkg/mod -v reonvr-go-build:/root/.cache/go-build -w /src golang:1.27-alpine go test ./... -count=1
```

Install frontend test dependencies into a Docker volume, then lint and build:

```powershell
docker run --rm -v "${PWD}:/src" -v reonvr-playwright-modules:/src/web/node_modules -w /src/web mcr.microsoft.com/playwright:v1.65.0-noble npm ci
docker run --rm -v "${PWD}:/src" -v reonvr-playwright-modules:/src/web/node_modules -w /src/web mcr.microsoft.com/playwright:v1.65.0-noble npm run lint
docker compose build app
```

Browser acceptance tests use the deployed app, an already registered
storage-capable camera, and the test container's bundled Chromium:

```powershell
docker compose up -d --wait app
docker run --rm --network reonvr-private -e REONVR_E2E_URL=http://host.docker.internal:8080 -e REONVR_E2E_CHANNEL=chromium -v "${PWD}:/src" -v reonvr-playwright-modules:/src/web/node_modules -w /src/web mcr.microsoft.com/playwright:v1.65.0-noble npm run test:e2e
```

Recording must already be paused. The tests temporarily change and restore the
camera's local recovery policy, leave recording paused, and check desktop and
mobile discovery and recovery states. The real discovery check does not save
devices; simulated replies exercise prefill, already-added, empty, and retry
states. `host.docker.internal` reaches the published app port on Docker Desktop;
`REONVR_E2E_URL` can target another paused deployment. Screenshots are written
beneath the ignored `runtime/` directory.

The container build embeds the frontend into the Go server. See
[docs/APPLICATION-SPEC.md](docs/APPLICATION-SPEC.md) for the implemented
functional contract and
[docs/REOLINK-CAMERA-API-LEARNINGS.md](docs/REOLINK-CAMERA-API-LEARNINGS.md)
for camera integration details.