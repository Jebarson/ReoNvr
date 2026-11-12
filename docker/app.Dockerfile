FROM node:26-alpine AS web-build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS server-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY webui/ ./webui/
RUN rm -rf webui/dist
COPY --from=web-build /web/dist ./webui/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/reonavr ./cmd/reonavr

FROM debian:bookworm-slim AS reolink-cli
ARG REOLINK_VERSION=0.19.0
ARG REOLINK_SHA256=a0424c0ce00ed95467162ec267824769e3e6d7d83d81f84df00dba34310b4dc2
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "https://github.com/reolink/reolink-cli/releases/download/v${REOLINK_VERSION}/reolink-cli-${REOLINK_VERSION}-external-linux-x86_64.tar.gz" -o /tmp/reolink.tar.gz \
    && echo "${REOLINK_SHA256}  /tmp/reolink.tar.gz" | sha256sum -c - \
    && mkdir -p /opt/reolink \
    && tar -xzf /tmp/reolink.tar.gz -C /opt/reolink --strip-components=1 \
    && test -x /opt/reolink/bin/reolink-cli \
    && test -x /opt/reolink/bin/reolink-gateway \
    && rm /tmp/reolink.tar.gz

FROM debian:bookworm-slim
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
USER reonavr
ENV HOME=/var/lib/reonavr
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 CMD curl -fsS http://127.0.0.1:8080/api/health/live || exit 1
ENTRYPOINT ["/usr/local/bin/reonavr"]