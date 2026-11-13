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
    && rm /tmp/reolink.tar.gz

FROM debian:bookworm-slim
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
USER reonavr
ENV HOME=/var/lib/reolink \
    XDG_CACHE_HOME=/var/lib/reolink/cache \
    XDG_CONFIG_HOME=/var/lib/reolink/config
EXPOSE 9000
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["reolink-cli", "--gateway-addr", "127.0.0.1:9000", "gateway", "status"]
ENTRYPOINT ["reolink-gateway", "--addr", "0.0.0.0:9000"]