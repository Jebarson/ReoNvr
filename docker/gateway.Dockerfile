FROM --platform=$BUILDPLATFORM debian:trixie-slim AS reolink-cli
ARG TARGETARCH
ARG REOLINK_VERSION=0.21.0
RUN case "$TARGETARCH" in \
            amd64) arch=x86_64; checksum=5ffab9501ead8bdaaee229c60676faa76727817224aa7d275bacdbc860640908 ;; \
            arm64) arch=arm64; checksum=7e542777f88c8fc47c69d1de8738241dae3c179acea253c07bfcde1b25506402 ;; \
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

FROM debian:trixie-slim
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="ReoNvr Gateway" \
    org.opencontainers.image.version=$VERSION \
    org.opencontainers.image.revision=$REVISION
RUN groupadd --gid 10001 reonavr \
    && useradd --uid 10001 --gid reonavr --home-dir /var/lib/reolink --create-home reonavr \
    && mkdir -p /var/lib/reolink/cache /var/lib/reolink/config \
    && chown -R reonavr:reonavr /var/lib/reolink
COPY --from=reolink-cli /opt/reolink/bin/reolink-cli /usr/local/bin/reolink-cli
COPY --from=reolink-cli /opt/reolink/bin/reolink-gateway /usr/local/bin/reolink-gateway
COPY --from=reolink-cli /opt/reolink/release.tar.gz /opt/reolink/EULA.txt /opt/reolink/THIRD-PARTY-LICENSES.txt /usr/share/reolink-cli/
USER reonavr
ENV HOME=/var/lib/reolink \
    XDG_CACHE_HOME=/var/lib/reolink/cache \
    XDG_CONFIG_HOME=/var/lib/reolink/config
EXPOSE 9000
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["reolink-cli", "--gateway-addr", "127.0.0.1:9000", "gateway", "status"]
ENTRYPOINT ["reolink-gateway", "--addr", "0.0.0.0:9000"]