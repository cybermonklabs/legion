#!/bin/sh
# go-fetch.sh <package>@<version>
#
# The network half of `go install <package>@<version>`, and nothing else: every module the build
# of that package needs, into the module cache, with the checksum database's answer for each, by one
# `go get` in a scratch module under network-install.sh's bound for go. Nothing is compiled here
# (`go get` builds nothing in module mode). The build then runs from that cache with the network
# off, outside network-install.sh, held by its step's own limit:
#
#   GOPROXY="file://$(go env GOMODCACHE)/cache/download" go install <package>@<version>
#
# WHY. A wrapped `go install` timed its compile as a wait on the network: in worker-image run
# 44772738928 gopls's install took 80 s of the go kind's 120 s bound, and its module downloads had
# ended at 6.1 s, so a runner whose CPU is slower fails a healthy install by name and compiles twice
# more. check-ci-filters.sh's rule 22 (.github/scripts/ci_network_installs.py) refuses a go command
# under network-install.sh that compiles, and reads a go command whose GOPROXY is `off` or a
# `file://` proxy as no install. Two other splits do not serve. GOPROXY=off refuses `go install
# <package>@<version>`, which looks the module's latest version up for its deprecation notice
# ("module lookup disabled by GOPROXY=off"); a file proxy answers that from the cache. And `go mod
# download` of the module and of the modules its go.mod requires fetches every zip, but reads no
# checksum database answer for them, which the install asks for: measured on 2026-11-20 with go
# 1.27.3 for gopls v0.23.0, go-licenses v2.0.1 and helm v3.20.2, each from an empty cache, the
# install with no network failed on `verifying module: … Get "https://sum.golang.org/lookup/…"`
# (golang.org/x/telemetry, k8s.io/klog/v2, github.com/moby/term). After `go get` each built with
# no network, GOFLAGS empty and GOSUMDB and GONOSUMDB at go's defaults: no checksum check is off.
#
# It is a POSIX sh script that needs only go and mktemp, so a Dockerfile's RUN runs it on Alpine's
# BusyBox as on Debian, with network-install.sh beside it (the RUN bind-mounts both into
# /usr/local/bin, and a step runs both from .github/scripts).
#
# Its tests: .github/scripts/network-install.test.sh, in ci.yaml's test job.
set -eu

here=$(dirname "$0")
if [ "$#" -ne 1 ] || [ "${1#*@}" = "$1" ]; then
  echo "::error::go-fetch: usage: go-fetch.sh <package>@<version>, and this call gave: $*" >&2
  exit 2
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/go-fetch.XXXXXX")
trap 'rm -rf "$work"' EXIT
cd "$work"
GOWORK=off go mod init example.com/go-fetch 2> /dev/null
GOWORK=off "$here/network-install.sh" go -- go get "$1"
