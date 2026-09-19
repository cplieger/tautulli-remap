# check=error=true
FROM golang:1.27.0-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder
# GOTOOLCHAIN=auto: a Renovate dep bump requiring a newer Go downloads that toolchain
# instead of failing the build (org convention, go.md/ci-cd.md); still reproducible
# because go.mod pins the toolchain version. `local` would hard-fail such a build.
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
COPY internal/ ./internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tautulli-remap .
COPY LICENSE NOTICE ./
COPY scripts/collect-licenses.sh scripts/
RUN --mount=type=cache,target=/go/pkg/mod \
    sh scripts/collect-licenses.sh --name tautulli-remap .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

COPY --chmod=755 --from=builder /tautulli-remap /tautulli-remap
COPY --from=builder /out/usr/share/licenses /usr/share/licenses
USER nonroot:nonroot
# The "health" subcommand probes the /tmp/.healthy marker the main
# process maintains. In scheduled mode it is created at startup, refreshed after
# each run, and removed after 3 consecutive failures; in resident-idle mode it
# reflects process liveness (created at startup, present while the process is
# alive). Exits 0 if the marker is present, else 1. See the README
# "Healthcheck" section.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/tautulli-remap", "health"]
ENTRYPOINT ["/tautulli-remap"]
