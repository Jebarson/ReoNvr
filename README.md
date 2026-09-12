# ReoNvr

ReoNvr is a self-hosted Reolink video archive. It can copy native recordings
from camera storage or record live RTSP streams without transcoding.

## Features

- Camera-storage event and continuous recording import
- RTSP event buffering and continuous recording
- Native H.264/H.265 preservation with FFmpeg stream copy
- Automatic newest-first import of matching camera-storage recordings
- UTC persistence with camera timezone and DST discovery
- Local AES-256-GCM credential encryption
- Playback, original-file download, retention, and capacity protection
- Persistent global recording pause

ReoNvr only reads camera configuration and media. It does not change camera
settings.

## Requirements

- Docker Engine with Docker Compose
- Reolink cameras reachable from the Docker host
- Enough local storage for the configured archive policy

## Start

```sh
docker compose up -d --build
```

Open <http://localhost:8080>, add a camera, test the connection, and select an
archive source and policy. Runtime databases, recordings, credentials, logs,
and backups stay under `runtime/` and are excluded from Git and Docker build
contexts.

Stop the service with:

```sh
docker compose down
```

## Development

Run backend tests:

```sh
go test ./...
```

Run frontend checks:

```sh
cd web
npm ci
npm run lint
npm run build
```

The container build runs the frontend build and embeds its output into the Go
server. See [APPLICATION-SPEC.md](APPLICATION-SPEC.md) for behavior and data
contracts and [REOLINK-CAMERA-API-LEARNINGS.md](REOLINK-CAMERA-API-LEARNINGS.md)
for integration notes.

## Security

Never commit `runtime/`, database files, archive media, master keys, environment
files, or backups. Camera passwords are encrypted at rest and are passed to the
Reolink tools through standard input rather than command-line arguments.