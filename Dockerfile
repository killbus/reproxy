# reproxy — single build path for humans and CI.
#
#   docker build -t reproxy .
#   docker build --build-arg VERSION=v0.4.0 -t reproxy:v0.4.0 .
#
# VERSION is injected into the binary via -ldflags and reported by
# `reproxy --version` (default "dev" when unstamped).

# --- build stage ---------------------------------------------------------
# golang:<major.minor> must match go.mod (go 1.25.1). Patch-level variants
# of the builder image are interchangeable; major.minor is the contract.
FROM golang:1.25 AS builder

ARG VERSION=dev

WORKDIR /src

# Copy the module (go.mod has zero dependencies — no layer-caching dance
# is warranted; the source arrives in one COPY).
COPY . .

# CGO_ENABLED=0: static binary, the only kind a distroless runtime can run.
# -trimpath: no absolute paths of the build host in the binary.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/reproxy \
    ./cmd/reproxy

# --- runtime stage -------------------------------------------------------
# distroless static, NOT scratch: reproxy performs outbound HTTPS with
# certificate validation; a bare scratch image has no CA roots and fails
# the first https upstream. distroless static ships the CA bundle.
#
# static-debian13:nonroot is the maintained spelling (the distroless
# README marks bare `distroless/static` aliases "deprecated and no longer
# updated"; the legacy tag still resolves to the same index — digest
# f2ea2709 at implementation time — but a frozen alias is how CA-bundle
# freshness silently goes stale). The `nonroot` tag runs as UID 65532.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /out/reproxy /reproxy

# Explicit for documentation (the base already defaults to this user):
# the container never runs as root.
USER nonroot

EXPOSE 8080

ENTRYPOINT ["/reproxy"]
