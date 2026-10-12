# SPDX-License-Identifier: AGPL-3.0-only
# SPDX-FileCopyrightText: 2026 Martin Buhr and contributors
#
# The standalone tracker: one static binary on a small Alpine base, run as
# a non-root user. Build for both architectures with:
#
#   docker buildx build --platform linux/amd64,linux/arm64 .
#
# The build stage runs on the build machine's own architecture and
# cross-compiles, so no emulation is needed.

FROM --platform=$BUILDPLATFORM golang:1.25.6-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w \
        -X github.com/lonelycode/corruption-tracker/version.Version=$VERSION \
        -X github.com/lonelycode/corruption-tracker/version.Commit=$COMMIT" \
      -o /out/corruption-tracker ./cmd/corruption-tracker

FROM alpine:3.22
# tzdata: the job schedule's time zone. postgresql16-client: pg_dump for
# `import --replace` backups, matching the compose stack's Postgres 16.
RUN apk add --no-cache ca-certificates tzdata postgresql16-client \
 && adduser -D -H -u 10001 tracker \
 && mkdir /data && chown tracker:tracker /data
COPY --from=build /out/corruption-tracker /usr/local/bin/corruption-tracker
COPY LICENSE /licenses/LICENSE
LABEL org.opencontainers.image.title="Corruption & Fraud Tracker" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/lonelycode/corruption-tracker"
USER tracker
# /data holds the key that encrypts stored secrets, and `import --replace`
# writes its backups under it (./backups): keep it on a volume.
WORKDIR /data
ENV TRACKER_DATA_DIR=/data \
    TRACKER_LISTEN=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=30s \
  CMD wget -qO /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["corruption-tracker"]
CMD ["serve"]
