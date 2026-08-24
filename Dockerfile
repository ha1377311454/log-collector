# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/log-collector ./cmd/log-collector

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="log-collector" \
      org.opencontainers.image.description="YAML-driven log collector with OTLP/HTTP export" \
      org.opencontainers.image.source="https://github.com/ha1377311454/log-collector" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/log-collector /usr/local/bin/log-collector
COPY LICENSE /LICENSE

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/log-collector"]
CMD ["-config", "/etc/log-collector/config.yaml"]
