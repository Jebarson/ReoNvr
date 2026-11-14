FROM --platform=$BUILDPLATFORM node:26-alpine AS web-build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS server-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY webui/ ./webui/
RUN rm -rf webui/dist
COPY --from=web-build /web/dist ./webui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/reonavr ./cmd/reonavr

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS reolink-cli
ARG TARGETARCH
ARG REOLINK_VERSION=0.19.0
RUN case "$TARGETARCH" in \
            amd64) arch=x86_64; checksum=a0424c0ce00ed95467162ec267824769e3e6d7d83d81f84df00dba34310b4dc2 ;; \
            arm64) arch=arm64; checksum=b24c503690c1e30da047d0268d0380457526d4fb5e43ce23ad6f33b69ad91d42 ;; \
            *) echo "Unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
        esac \
        && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "https://github.com/reolink/reolink-cli/releases/download/v${REOLINK_VERSION}/reolink-cli-${REOLINK_VERSION}-external-linux-${arch}.tar.gz" -o /tmp/reolink.tar.gz \
    && echo "${checksum}  /tmp/reolink.tar.gz" | sha256sum -c - \
    && mkdir -p /opt/reolink \
    && tar -xzf /tmp/reolink.tar.gz -C /opt/reolink --strip-components=1 \
    && test -x /opt/reolink/bin/reolink-cli \
    && test -x /opt/reolink/bin/reolink-gateway \
    && test -s /opt/reolink/EULA.txt \
    && test -s /opt/reolink/THIRD-PARTY-LICENSES.txt \
    && mv /tmp/reolink.tar.gz /opt/reolink/release.tar.gz

FROM scratch AS licensing-artifacts
COPY --from=reolink-cli /opt/reolink/release.tar.gz /reolink/release.tar.gz
COPY --from=reolink-cli /opt/reolink/EULA.txt /reolink/EULA.txt
COPY --from=reolink-cli /opt/reolink/THIRD-PARTY-LICENSES.txt /reolink/THIRD-PARTY-LICENSES.txt

FROM server-build AS media-tests
COPY tools/gotest/ ./tools/gotest/
RUN go build -o /tmp/gotest ./tools/gotest \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH /tmp/gotest -c -o /out/archive.test ./internal/archive

FROM scratch AS test-artifacts
COPY --from=media-tests /out/archive.test /archive.test

FROM debian:bookworm-slim
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="ReoNvr" \
      org.opencontainers.image.source="https://github.com/Jebarson/ReoNvr" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 reonavr \
    && useradd --uid 10001 --gid reonavr --home-dir /var/lib/reonavr --create-home reonavr \
    && mkdir -p /data /archive /run/secrets \
    && chown -R reonavr:reonavr /data /archive /run/secrets /var/lib/reonavr
COPY --from=server-build /out/reonavr /usr/local/bin/reonavr
COPY --from=reolink-cli /opt/reolink/bin/reolink-cli /usr/local/bin/reolink-cli
COPY --from=reolink-cli /opt/reolink/bin/reolink-gateway /usr/local/bin/reolink-gateway
COPY --from=reolink-cli /opt/reolink/release.tar.gz /opt/reolink/EULA.txt /opt/reolink/THIRD-PARTY-LICENSES.txt /usr/share/reolink-cli/
USER reonavr
ENV HOME=/var/lib/reonavr
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 CMD curl -fsS http://127.0.0.1:8080/api/health/live || exit 1
ENTRYPOINT ["/usr/local/bin/reonavr"]