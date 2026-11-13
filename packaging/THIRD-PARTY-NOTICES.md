# Third-Party Software

The Docker images include Reolink CLI/gateway 0.19.0, FFmpeg and Debian packages,
the Go dependencies declared in go.mod/go.sum, and frontend dependencies declared
in web/package-lock.json. Their respective licenses apply independently.

- Reolink CLI source and license: https://github.com/reolink/reolink-cli/tree/v0.19.0
- FFmpeg licensing and corresponding source: https://ffmpeg.org/legal.html
- Debian FFmpeg source: https://sources.debian.org/src/ffmpeg/
- Debian package copyright and license texts are retained in the images under
  /usr/share/doc/*/copyright and /usr/share/common-licenses.

Native camera media is not transcoded. No camera credentials, test configuration,
user database, encryption key, or recordings are included in release bundles.