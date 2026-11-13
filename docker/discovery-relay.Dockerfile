FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/discovery-relay/ ./cmd/discovery-relay/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discovery-relay ./cmd/discovery-relay

FROM scratch
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="ReoNvr Discovery Relay" \
	org.opencontainers.image.version=$VERSION \
	org.opencontainers.image.revision=$REVISION
COPY --from=build /out/discovery-relay /discovery-relay
USER 10001:10001
ENTRYPOINT ["/discovery-relay"]