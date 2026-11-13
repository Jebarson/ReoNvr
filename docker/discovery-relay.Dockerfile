FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/discovery-relay/ ./cmd/discovery-relay/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/discovery-relay ./cmd/discovery-relay

FROM scratch
COPY --from=build /out/discovery-relay /discovery-relay
USER 10001:10001
ENTRYPOINT ["/discovery-relay"]