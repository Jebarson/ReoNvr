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