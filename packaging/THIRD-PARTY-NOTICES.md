# Third-Party Software

The Docker images include Reolink CLI/gateway 0.19.0, FFmpeg and Debian packages,
and the application's Go and frontend dependencies. Their respective licenses
apply independently.

The ZIP includes `THIRD-PARTY-LICENSES.tar` for offline inspection. It contains
the complete upstream Reolink archive and legal texts, Go dependency license
artifacts and a CSV report, a Go module/version inventory, frontend dependency
metadata and license texts, and the corresponding Go/npm manifests. The frontend
inventory includes build-tool dependencies as well as runtime dependencies.
The same release gate generates these documents for each architecture.

- Reolink CLI/gateway binaries are proprietary, not Apache-licensed source code.
  The upstream repository's Apache license covers its documentation and plugin
  components. The complete, unmodified upstream archive, `EULA.txt`, and
  `THIRD-PARTY-LICENSES.txt` are retained in both runtime images under
  `/usr/share/reolink-cli/`.
- Review the bundled Reolink EULA before using or redistributing these images.
  Section 3 permits no-charge redistribution of the complete, unmodified archive
  with its unaltered notices; commercial bundling requires Reolink's prior written
  consent. Do not remove notices, charge for the Software, or assume unrestricted
  redistribution rights. Upstream: https://github.com/reolink/reolink-cli/tree/v0.19.0
- FFmpeg licensing and corresponding source: https://ffmpeg.org/legal.html
- Debian FFmpeg source: https://sources.debian.org/src/ffmpeg/
- Debian package copyright and license texts are retained in the images under
  /usr/share/doc/*/copyright and /usr/share/common-licenses.

Native camera media is not transcoded. No camera credentials, test configuration,
user database, encryption key, or recordings are included in release bundles.