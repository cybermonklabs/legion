# The image of single-node.compose.yml's one-shot `buckets` service: `envoy-infra`, which the
# published envoy image does not carry, built from this checkout. Build context is the repository
# root, read through buckets.Dockerfile.dockerignore beside this file, which admits the two Go
# modules of packages/envoy (their module files and every Go source but tests) and nothing else:
#
#   docker build -f packages/envoy/deploy/compose/buckets.Dockerfile -t envoy-buckets:local .

# Go version must match packages/envoy/infra/go.mod. The listener image builds on the same image
# (packages/envoy/docker/Dockerfile).
# ghcr.io/cybermonklabs/legion/ci, the copy of golang:1.27.3-alpine (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build

# A `|` between proxy entries makes a go command fall through to the next source after any error,
# where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
# go.sum holds each module's hash either way, so this is a second path, not a weaker check
# (sjawhar/legion#2512).
ENV GOPROXY=https://proxy.golang.org|direct

# The command builds in the workspace of packages/envoy/infra/go.work, which binds the envoy module
# beside it, as the envoy-infra release builds it (.github/workflows/release-envoy-infra.yaml). The
# whole of both modules' Go sources is copied, not the packages the command imports today, so a new
# import of the command never leaves this build short of a directory. The two caches stay on the
# builder, so an `up` after a source change rebuilds only what changed.
WORKDIR /src/packages/envoy/infra
COPY packages/envoy /src/packages/envoy
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/envoy-infra ./cmd/envoy-infra

# The listener image's runtime (packages/envoy/docker/Dockerfile).
# ghcr.io/cybermonklabs/legion/ci, the copy of debian:bookworm-slim (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3

LABEL org.opencontainers.image.source=https://github.com/cybermonklabs/legion

COPY --from=build /out/envoy-infra /usr/local/bin/envoy-infra

# nobody: the scan reads each process's /proc/<pid>/comm, which every user may read, and the
# command needs no other privilege.
USER 65534:65534

ENTRYPOINT ["envoy-infra"]
