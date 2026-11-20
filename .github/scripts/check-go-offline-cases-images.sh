# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rule 6 (go_offline_images.py): each go command an image build of a
# required job runs reads the job's module cache alone. A bare go RUN in a Dockerfile a required
# job builds is refused at its line, naming the Dockerfile, the stage and the job; one that lacks
# any one of GOPROXY=off, GOFLAGS=-mod=readonly, GOTOOLCHAIN=local and the mount of a named context
# the build hands is refused naming what it lacks; the mounted offline RUN passes, and so does a
# file:// proxy at the mounted directory; a stage the named context replaces is not built, and is
# refused when the build hands no such context; a Dockerfile only a workflow outside the required
# set builds is not read; a reason above the RUN passes it, named; a build in a run: step, an exec
# form RUN and a repository script the RUN runs that starts go are read; a Dockerfile no tree holds
# is refused. Sourced by check-go-offline.test.sh, whose fixtures, run_check and counters they use;
# not run on its own.

# image_fixture <name> <build-contexts line or ''>: a fixture whose ci.yaml's job builds
# docker/Dockerfile with docker/build-push-action, handing the named contexts given; the
# Dockerfile is this harness's stdin. Echoes its root.
image_fixture() {
  local root
  root=$(fixture "$1")
  {
    printf '      - name: Build the image\n        uses: docker/build-push-action@v6\n        with:\n'
    printf '          context: .\n          file: docker/Dockerfile\n'
    [ -z "$2" ] || printf '          build-contexts: %s\n' "$2"
  } >>"$root/.github/workflows/ci.yaml"
  put "$root" docker/Dockerfile
  echo "$root"
}

CONTEXT='gomodcache=${{ steps.gomodcache.outputs.dir }}'
OFFLINE_ENV='ENV GOPROXY=off GOFLAGS=-mod=readonly GOTOOLCHAIN=local'
MOUNT='RUN --mount=type=bind,from=gomodcache,target=/go/pkg/mod/cache/download,rw \'

# image_refused <path> <line> <what it lacks>: the checks of a go RUN refused at <path>:<line>.
image_refused() {
  check "fails" "$(is "$status" 1)"
  check "names $1:$2, the stage and the job" \
    "$(contains "$out" "$1:$2: a go command of stage .* of $1, which job lint of \.github/workflows/ci\.yaml (step \"Build the image\") builds, runs without .*$3")"
  check "annotates the line" "$(contains "$out" "::error file=$1,line=$2::")"
}

echo "case: a bare go RUN in a Dockerfile a required job builds is refused at its line"
root=$(image_fixture image-bare "" <<'EOF'
FROM golang AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app ./cmd/app
EOF
)
run_check "$root"
image_refused docker/Dockerfile 4 'GOPROXY=off (it is unset or unknown); GOFLAGS=-mod=readonly (it is unset or unknown); GOTOOLCHAIN=local (it is unset or unknown); a --mount=type=bind,from=<a named context the build hands>,target=/go/pkg/mod'
check "refuses it alone" "$(contains "$out" '^1 problems over the ')"

echo "case: the mounted offline RUN passes, counted as reading the job's module cache"
root=$(image_fixture image-mounted "$CONTEXT" <<EOF
FROM scratch AS gomodcache
FROM golang AS build
WORKDIR /src
$OFFLINE_ENV
COPY . .
$MOUNT
    CGO_ENABLED=0 go build -o /out/app ./cmd/app
EOF
)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts it" "$(contains "$out" '1 image builds of 1 required workflows read (1 Dockerfiles, 1 go commands in the stages they build): 1 read the job.s module cache alone')"

echo "case: each of the four, left out, is refused by name"
for lack in GOPROXY GOFLAGS GOTOOLCHAIN mount; do
  env_line=$OFFLINE_ENV mount_line=$MOUNT
  case $lack in
    GOPROXY) env_line='ENV GOPROXY=https://proxy.golang.org GOFLAGS=-mod=readonly GOTOOLCHAIN=local'; want='GOPROXY=off (it is https://proxy.golang.org)' ;;
    GOFLAGS) env_line='ENV GOPROXY=off GOTOOLCHAIN=local'; want='GOFLAGS=-mod=readonly (it is unset or unknown)' ;;
    GOTOOLCHAIN) env_line='ENV GOPROXY=off GOFLAGS=-mod=readonly'; want='GOTOOLCHAIN=local (it is unset or unknown)' ;;
    mount) mount_line='RUN \'; want='a --mount=type=bind,from=<a named context the build hands>,target=/go/pkg/mod' ;;
  esac
  root=$(image_fixture "image-lacks-$lack" "$CONTEXT" <<EOF
FROM scratch AS gomodcache
FROM golang AS build
$env_line
$mount_line
    go build ./...
EOF
)
  run_check "$root"
  image_refused docker/Dockerfile 5 "$want"
done

echo "case: a mount of a context the build does not hand, or at another directory, is no mount of the cache"
root=$(image_fixture image-mount-unhanded "" <<EOF
FROM scratch AS gomodcache
FROM golang AS build
$OFFLINE_ENV
$MOUNT
    go build ./...
EOF
)
run_check "$root"
image_refused docker/Dockerfile 5 'a --mount=type=bind'
root=$(image_fixture image-mount-elsewhere "$CONTEXT" <<EOF
FROM golang AS build
$OFFLINE_ENV
RUN --mount=type=bind,from=gomodcache,target=/tmp/mod,rw go build ./...
EOF
)
run_check "$root"
image_refused docker/Dockerfile 3 'a --mount=type=bind'

echo "case: a GOMODCACHE the stage sets moves the directory the mount must reach"
root=$(image_fixture image-gomodcache "$CONTEXT" <<EOF
FROM golang AS build
$OFFLINE_ENV
ENV GOMODCACHE=/gomodcache
RUN --mount=type=bind,from=gomodcache,target=/gomodcache/cache/download,rw go build ./...
EOF
)
run_check "$root"
check "a mount at GOMODCACHE's cache/download: exits 0" "$(is "$status" 0)"

echo "case: a file:// proxy at the mounted directory is offline"
root=$(image_fixture image-file-proxy "$CONTEXT" <<EOF
FROM golang AS build
$OFFLINE_ENV
$MOUNT
    GOPROXY=file:///go/pkg/mod/cache/download go install example.com/tool@v1.0.0
EOF
)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
root=$(image_fixture image-file-proxy-elsewhere "$CONTEXT" <<EOF
FROM golang AS build
$OFFLINE_ENV
$MOUNT
    GOPROXY=file:///srv/proxy go install example.com/tool@v1.0.0
EOF
)
run_check "$root"
image_refused docker/Dockerfile 4 'GOPROXY=off (it is file:///srv/proxy)'

echo "case: an export, an env -u and a prefix in the RUN are read after the stage's ENV"
root=$(image_fixture image-export-online "$CONTEXT" <<EOF
FROM golang AS build
$OFFLINE_ENV
$MOUNT
    export GOPROXY=https://proxy.golang.org && go mod download
EOF
)
run_check "$root"
image_refused docker/Dockerfile 4 'GOPROXY=off (it is https://proxy.golang.org)'
root=$(image_fixture image-env-u "$CONTEXT" <<EOF
FROM golang AS build
$OFFLINE_ENV
$MOUNT
    env -u GOFLAGS go build ./...
EOF
)
run_check "$root"
image_refused docker/Dockerfile 4 'GOFLAGS=-mod=readonly (it is unset or unknown)'

echo "case: the stage a named context replaces is not built; with no such context it is, and refused"
cat >"$work/online-stage.Dockerfile" <<EOF
FROM golang AS gomodcache-online
ENV GOPROXY=https://proxy.golang.org
RUN go mod download
FROM scratch AS gomodcache
COPY --from=gomodcache-online /go/pkg/mod/cache/download/ /
FROM golang AS build
$OFFLINE_ENV
$MOUNT
    go build ./...
EOF
root=$(image_fixture image-replaced "$CONTEXT" <"$work/online-stage.Dockerfile")
run_check "$root"
check "with the context: exits 0" "$(is "$status" 0)"
check "with the context: the online stage's go command is not counted" "$(contains "$out" '(1 Dockerfiles, 1 go commands in the stages they build)')"
root=$(image_fixture image-not-replaced "" <"$work/online-stage.Dockerfile")
run_check "$root"
image_refused docker/Dockerfile 3 'GOPROXY=off (it is https://proxy.golang.org)'
check "and the build stage's, whose mount reads no handed context" \
  "$(contains "$out" 'docker/Dockerfile:9: a go command of stage build')"

echo "case: a stage the target reaches is read, and one it does not is not"
root=$(image_fixture image-target "" <<'EOF'
FROM golang AS tools
RUN go install example.com/tool@v1.0.0
FROM golang AS other
RUN go build ./...
FROM alpine AS final
COPY --from=tools /go/bin/tool /usr/local/bin/tool
EOF
)
run_check "$root"
image_refused docker/Dockerfile 2 'GOPROXY=off'
check "the stage no build reaches is not read" "$(is "$(contains "$out" 'docker/Dockerfile:4:')" false)"

echo "case: a Dockerfile only a workflow outside the required set builds is not read"
root=$(fixture image-nightly)
put "$root" docker/Dockerfile <<'EOF'
FROM golang
RUN go build ./...
EOF
nightly "$root" <<'EOF'
      - uses: docker/build-push-action@v6
        with:
          file: docker/Dockerfile
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads no build" "$(contains "$out" '0 image builds of 1 required workflows read')"

echo "case: a go-offline reason within five lines above the RUN passes it, named; six lines up does not"
root=$(image_fixture image-reasoned "" <<'EOF'
FROM golang AS build
# go-offline: only a pull request that changes this image's files builds it.
RUN go build ./...
EOF
)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "names it" "$(contains "$out" 'RUNs carry a reason: docker/Dockerfile:3')"
root=$(image_fixture image-reason-far "" <<'EOF'
FROM golang AS build
# go-offline: only a pull request that changes this image's files builds it.
#
#
#
#
#
RUN go build ./...
EOF
)
run_check "$root"
image_refused docker/Dockerfile 8 'GOPROXY=off'

echo "case: a docker buildx build in a run: step is read, with its --build-context"
root=$(fixture image-run-step)
put "$root" docker/Dockerfile <<EOF
FROM golang AS build
$OFFLINE_ENV
$MOUNT
    go build ./...
EOF
printf '      - name: Build the image\n        run: docker buildx build --load --build-context gomodcache=%s -f docker/Dockerfile .\n' \
  '"$RUNNER_TEMP/mod"' >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "with the context: exits 0" "$(is "$status" 0)"
check "reads the build" "$(contains "$out" '1 image builds of 1 required workflows read')"
sed -i 's|--build-context gomodcache="$RUNNER_TEMP/mod" ||' "$root/.github/workflows/ci.yaml"
run_check "$root"
image_refused docker/Dockerfile 4 'a --mount=type=bind'

echo "case: an exec form RUN is read as an argv"
root=$(image_fixture image-exec "" <<'EOF'
FROM golang AS build
RUN ["go", "build", "./..."]
EOF
)
run_check "$root"
image_refused docker/Dockerfile 2 'GOPROXY=off'

echo "case: a repository script the RUN runs that starts go is a go command of the RUN"
root=$(image_fixture image-script "" <<'EOF'
FROM golang AS build
COPY scripts/notices.sh /usr/local/bin/
RUN notices.sh --install
EOF
)
printf '#!/bin/sh\ngo install example.com/tool@v1.0.0\n' | put "$root" scripts/notices.sh
run_check "$root"
image_refused docker/Dockerfile 3 'GOPROXY=off'
printf '#!/bin/sh\necho notices\n' >"$root/scripts/notices.sh"
run_check "$root"
check "a script that starts no go is none: exits 0" "$(is "$status" 0)"

echo "case: a Dockerfile the build names and no tree holds is refused"
root=$(fixture image-absent)
printf '      - name: Build the image\n        uses: docker/build-push-action@v6\n        with:\n          file: docker/Absent.Dockerfile\n' \
  >>"$root/.github/workflows/ci.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" 'builds docker/Absent.Dockerfile, which is not in this tree')"

echo "case: the rule run alone gives UNKNOWN for no ci.yaml and for required workflows with no step"
# The go check meets rule 4's UNKNOWN first in both trees, so the rule's own CLI is read here.
root=$(fixture image-unknown-noci)
rm "$root/.github/workflows/ci.yaml"
set +e
out=$(PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 "$script_dir/go_offline_images.py" "$root" 2>&1)
status=$?
set -e
check "no ci.yaml: exits 2" "$(is "$status" 2)"
check "no ci.yaml: says so" "$(contains "$out" '^UNKNOWN: no \.github/workflows/ci\.yaml here')"
root=$(fixture image-unknown-nosteps)
printf 'name: CI\non: pull_request\njobs:\n  lint:\n    runs-on: ubuntu-24.04\n    steps: []\n' >"$root/.github/workflows/ci.yaml"
set +e
out=$(PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$script_dir" python3 "$script_dir/go_offline_images.py" "$root" 2>&1)
status=$?
set -e
check "no step: exits 2" "$(is "$status" 2)"
check "no step: says so" "$(contains "$out" '^UNKNOWN: read 1 required workflows and no step of them')"
