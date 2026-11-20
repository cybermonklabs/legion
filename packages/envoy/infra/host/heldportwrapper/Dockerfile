# The image the envoy-infra live tests run (host/live_test.go, pulumitest.HeldPortImage): the smoke
# image BASE names, with this directory's wrapper as its envoy-listener and the real listener moved
# to /usr/local/libexec/envoy-listener, where the wrapper executes it on the socket the test holds.
# The user, entrypoint, healthcheck and curl stay the smoke image's. Build context is the repository
# root, read through Dockerfile.dockerignore beside this file:
#
#   docker buildx build --load --build-arg BASE=legion-envoy-smoke:local \
#     -t legion-envoy-smoke-heldport:local -f packages/envoy/infra/host/heldportwrapper/Dockerfile .
#
# The wrapper's go build reads a module cache alone: the stage `gomodcache`, the download directory
# of a module cache (its .info, .mod and .zip files, which go extracts what it builds from), mounted
# where go reads it, with the module proxy off, so a module that directory lacks fails the build at
# once, naming it, and nothing is downloaded while the image builds. ci.yaml's envoy-smoke job hands
# the build its own under that name (`build-contexts: gomodcache=<its GOMODCACHE>/cache/download`),
# which its fetch step filled before (.github/go-fetch/envoy-smoke.txt), so the two stages that make
# the stage below are not built there. A build that hands none, as the command above, builds them:
# they download the workspace's modules online, in a layer only a change to a module file rebuilds.
ARG BASE

FROM ${BASE} AS base

# Go version must match packages/envoy/infra/go.mod. The smoke image builds on the same image, so
# the envoy-smoke job has pulled it already.
# ghcr.io/cybermonklabs/legion/ci, the copy of golang:1.27.3-alpine (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS gomodcache-online

WORKDIR /src/packages/envoy/infra

# A `|` between proxy entries makes a go command fall through to the next source after any error,
# where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
# go.sum holds each module's hash either way, so this is a second path, not a weaker check
# (sjawhar/legion#2512).
ENV GOPROXY=https://proxy.golang.org|direct

# The module files of packages/envoy/infra/go.work's two modules alone, so a source change downloads
# nothing again.
COPY packages/envoy/go.mod packages/envoy/go.sum /src/packages/envoy/
COPY packages/envoy/infra/go.work packages/envoy/infra/go.work.sum packages/envoy/infra/go.mod packages/envoy/infra/go.sum ./
RUN go mod download

# The download directory the wrapper's build mounts, at the root, as the named context a CI job
# hands in its place holds it.
FROM scratch AS gomodcache
COPY --from=gomodcache-online /go/pkg/mod/cache/download/ /

# ghcr.io/cybermonklabs/legion/ci, the copy of golang:1.27.3-alpine (.github/ci-images.yaml)
FROM ghcr.io/cybermonklabs/legion/ci@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS wrapper

WORKDIR /src/packages/envoy/infra

# The module cache alone: no proxy, go.mod read only, and this image's own Go, never a toolchain a
# go line names (.github/scripts/go_offline_images.py holds every go RUN of a required job's image
# build to these and to the mount of the job's module cache).
ENV GOPROXY=off GOFLAGS=-mod=readonly GOTOOLCHAIN=local

# The module builds in the workspace of packages/envoy/infra/go.work, as the envoy-infra-go job
# builds it, which settles the two modules' requirements. Both modules are copied whole, so a new
# import of the wrapper builds with no edit to this file; Dockerfile.dockerignore beside this file
# leaves out what no build compiles. The mount is read-write so go may write its lock files there;
# what it writes is dropped with the mount.
COPY packages/envoy /src/packages/envoy
RUN --mount=type=bind,from=gomodcache,target=/go/pkg/mod/cache/download,rw \
    CGO_ENABLED=0 go build -o /out/envoy-listener-heldport ./host/heldportwrapper

FROM ${BASE}

COPY --from=base /usr/local/bin/envoy-listener /usr/local/libexec/envoy-listener
COPY --from=wrapper --chmod=0755 /out/envoy-listener-heldport /usr/local/bin/envoy-listener
