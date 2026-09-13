# ReoNvr

ReoNvr is a self-hosted Reolink recording archive. It imports native files
from camera storage or records native RTSP streams, indexes camera detections,
and provides local browser playback without transcoding.

ReoNvr reads camera configuration and media. It never changes camera recording
schedules, formats camera storage, or deletes files from a camera.

## Recording modes

| Source | Events | Continuous |
|---|---|---|
| Camera storage | Import matching SD-card recordings, newest first | Import every recording produced by the camera |
| RTSP | Retain stream-copy segments around selected camera events | Retain every stream-copy segment |

Public Playback and Events views are local-only. A recording appears after its
media is complete in the local archive, and an event appears only when local
video covers it.

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

4. Confirm both services are healthy:

	 ```sh
	 docker compose ps
	 ```

5. Open <http://localhost:8080>.

Runtime data is stored in bind-mounted directories and survives container
rebuilds. The gateway is private to the Compose network; only port `8080` is
published.

## First use

1. Open **Cameras** and select **Add camera**.
2. Enter the camera name, LAN address, username, and password.
3. Select **Test connection**. ReoNvr discovers the camera model, timezone,
	 streams, storage support, and event categories without changing the camera.
4. Choose Camera storage or RTSP, Events or Continuous, the stream quality,
	 and event categories, then save.
5. Open **Settings** to choose the recording subfolder and organization:
	 - Camera, then day
	 - Day, then camera
	 - Camera only
	 - Day only
6. Enable the application-wide **Recording** switch when ingestion should run.

The Settings folder is relative to the mounted `/archive` root. To move the
host archive, change `REONVR_RECORDINGS_PATH` and recreate the app container.
Folder and layout changes apply only to new media; indexed older files retain
their existing paths.

## Daily operation

- **Cameras** shows connectivity, source/mode, local media count, and pending
	Camera Storage downloads.
- **Playback** lists only completed local recordings and supports date/event
	filters, seeking, and original-file download.
- **Settings** controls the recording subfolder and folder layout.
- The global **Recording** switch pauses reconciliation, downloads, event
	ingestion, and RTSP recording without changing camera policies.
- Retention removes the oldest completed local recordings when free space
	falls below the calculated 30-minute reserve. It never deletes camera media.

Original files preserve their camera codec and container. Extensionless native
Reolink MP4 files are served and downloaded with an `.mp4` filename.

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

## Backup

Pause recording in the UI, then back up these items together:

- `runtime/data/reonavr.db` plus its WAL files when present
- `runtime/secrets/master.key`
- The directory configured by `REONVR_RECORDINGS_PATH`

The database and master key are a pair. Losing the key makes saved camera
passwords unrecoverable.

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

## Development

Backend tests:

```sh
go test ./...
```

Frontend checks:

```sh
cd web
npm ci
npm run lint
npm run build
```

The container build embeds the frontend into the Go server. See
[docs/APPLICATION-SPEC.md](docs/APPLICATION-SPEC.md) for the implemented
functional contract and
[docs/REOLINK-CAMERA-API-LEARNINGS.md](docs/REOLINK-CAMERA-API-LEARNINGS.md)
for camera integration details.