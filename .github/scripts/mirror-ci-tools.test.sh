#!/usr/bin/env bash
# Tests for `mirror-ci-tools.sh`: a row the package holds is held and nothing is pushed, a row it
# holds under no tag gets its manifest pushed, a row it lacks is filled with the layer, the config
# and the manifest in that order and the blob is read back, a fill the package did not keep fails
# naming the row, an asset that serves other bytes than the row pins fails naming both digests and
# pushes nothing, a blob the package held and no longer holds fails naming the row, an asset that
# does not answer leaves the row cold and the run green, a row with a mirror_member pushes and
# checks the member's own digest, an upload location written as a path resolves against the host
# the push went to and not against the path the registry base carries, a row whose second source
# this repository reads from no service stops the run where that read failed, and a run with no
# push credential refuses before it reads anything.
#
# NO CASE REACHES THE NETWORK. Each case starts an HTTP origin of its own on 127.0.0.1, at a port
# the kernel picks. It answers the registry API at its root, as ghcr.io does, and under /ghcr as
# well, and it serves the release host under /github, so CI_TOOLS_GHCR_BASE is that origin and
# CI_TOOLS_GITHUB_BASE is that one path of it.
# One case points the registry base at that /ghcr path instead.
# An upload location the registry writes as a path is absolute from the root of the host, so
# what the run puts in front of it is the scheme and host of the address it pushed to and never
# the path the seam carries, and the two readings differ nowhere else.
# The origin's whole state is one directory: `blobs/sha256:<digest>` is what the package holds,
# `manifests/<tag>` is what a tag resolves to, `assets/<path>` is what the release host serves,
# and `flags/<name>` are the three answers a case asks the origin for — `blob-head-404` for a
# package that keeps no blob, `absolute-location` for an upload location written as a whole URL,
# and `no-query-location` for one written with no query. It appends one line per request to
# `requests.log`, which is what a case reads to say what the run read and wrote.
#
# Each case builds a miniature repository under $work/<case>/repo and symlinks the real script,
# and the fetch helper it sources, into its .github/scripts/, so the script under test is the
# file CI runs: it resolves its root from its own path (`cd "$(dirname "$0")/../.."`), which the
# symlink puts at the fixture root, and it reads the manifest at the default path under that
# root.
#
# THE CURL ON THESE CASES' PATH: the script fetches through .github/scripts/ci-tools.sh, which
# pins `--retry 7 --retry-all-errors` with `--retry-max-time 60`, so the case whose asset does
# not answer would sleep up to a minute. The cases put a curl of their own first on PATH, a
# two-line script that runs the real curl with `--retry-max-time 1` appended after the arguments
# the helper wrote, which curl reads as the last word on that option. It bounds the sleep and
# changes nothing else. A .curlrc cannot do it, because curl reads its configuration file before
# the command line and the helper's own bound would win. A CI runner sees neither, so the pinned
# ladder is what runs there.
#
# Run from anywhere: .github/scripts/mirror-ci-tools.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture and every digest below is read inside a command substitution, and bash does not
# carry errexit into one on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
mirror_script="$script_dir/mirror-ci-tools.sh"
helper_script="$script_dir/ci-tools.sh"
work=$(mktemp -d)
servers=()
trap 'for server in ${servers[@]+"${servers[@]}"}; do kill "$server" 2>/dev/null || true; done; rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

REGISTRY=ghcr.io/example/fixture/ci
REGISTRY_PATH=example/fixture/ci
ACTOR=fixture-actor

runner_temp="$work/runner-temp"
mkdir -p "$runner_temp" "$work/payloads" "$work/bin"

# The curl the cases run, first on their PATH: the real one with `--retry-max-time 1` after the
# arguments the helper wrote, which is the bound the header above explains.
real_curl=$(command -v curl)
cat > "$work/bin/curl" <<SH
#!/usr/bin/env bash
exec "$real_curl" "\$@" --retry-max-time 1
SH
chmod 0755 "$work/bin/curl"

# digest_of <file>: the sha256 the manifest writes bare.
digest_of() { sha256sum -- "$1" | awk '{ print $1 }'; }

# The two payloads. The `whole` row pushes its asset as it stands, and the `inner` row pushes the
# member its archive holds, so the archive's own digest is one no row names.
printf 'the bytes a release asset would hold\n' > "$work/payloads/whole.tar.gz"
mkdir -p "$work/inner-build"
printf 'the binary inside a release tarball\n' > "$work/inner-build/inner"
tar -czf "$work/payloads/inner.tar.gz" -C "$work/inner-build" ./inner
cp "$work/payloads/whole.tar.gz" "$work/payloads/other.tar.gz"
printf 'X' | dd of="$work/payloads/other.tar.gz" bs=1 seek=0 conv=notrunc status=none

WHOLE=$(digest_of "$work/payloads/whole.tar.gz")
INNER=$(digest_of "$work/inner-build/inner")
OTHER=$(digest_of "$work/payloads/other.tar.gz")
WHOLE_TAG="tool-whole-1.0.0-${WHOLE:0:8}"
INNER_TAG="tool-inner-2.0.0-${INNER:0:8}"
# The two URLs the fixture rows name, written once, and the path the origin serves each asset
# at: the URL with the host the CI_TOOLS_GITHUB_BASE seam stands in for taken off its front.
WHOLE_URL=https://github.com/example/whole/releases/download/v1.0.0/whole.tar.gz
INNER_URL=https://github.com/example/inner/releases/download/v2.0.0/inner.tar.gz
WHOLE_ASSET=${WHOLE_URL#https://github.com/}
INNER_ASSET=${INNER_URL#https://github.com/}
WHOLE_SIZE=$(wc -c < "$work/payloads/whole.tar.gz")
INNER_SIZE=$(wc -c < "$work/inner-build/inner")

# config_digest <layer digest>: the sha256 of the config blob the script builds for that layer.
# Written out here, byte for byte, so a case reads the digest of the document the script must
# push and not the digest of whatever it pushed.
config_digest() {
  printf '{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:%s"]}}' \
    "$1" | sha256sum | awk '{ print $1 }'
}
WHOLE_CONFIG=$(config_digest "$WHOLE")
INNER_CONFIG=$(config_digest "$INNER")

# The origin: the registry API at its root and the release host under /github, with every
# request appended to requests.log. Its whole state is the directory it is given, so a case says
# what the package holds by writing a file, and reads what the run did by reading that log.
cat > "$work/origin.py" <<'PY'
import hashlib
import http.server
import os
import re
import socketserver
import sys
import urllib.parse

state, portfile = sys.argv[1], sys.argv[2]
log = os.path.join(state, "requests.log")

UPLOADS = re.compile(r"^/v2/(?P<repository>.+)/blobs/uploads/$")
UPLOAD = re.compile(r"^/v2/(?P<repository>.+)/blobs/uploads/(?P<handle>[^/]+)$")
BLOB = re.compile(r"^/v2/(?P<repository>.+)/blobs/(?P<digest>sha256:[0-9a-f]{64})$")
MANIFEST = re.compile(r"^/v2/(?P<repository>.+)/manifests/(?P<tag>[^/]+)$")
ASSET = re.compile(r"^/github/(?P<rest>.+)$")

# The one path a case may put the registry API behind, reached through the CI_TOOLS_GHCR_BASE
# seam. ghcr.io answers at the root of its host, and a registry behind a path writes an upload
# location that is absolute from that root, the prefix included, which is what do_POST does.
PREFIX = "/ghcr"


def registry_path(path):
    """The path the registry API reads, and the prefix this origin was asked under."""
    if path == PREFIX or path.startswith(PREFIX + "/"):
        return PREFIX, path[len(PREFIX):]
    return "", path


def flag(name):
    return os.path.exists(os.path.join(state, "flags", name))


class Handler(http.server.BaseHTTPRequestHandler):
    def record(self, line):
        with open(log, "a") as handle:
            handle.write(line + "\n")

    def answer(self, status, body=b"", headers=()):
        self.send_response(status)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def answer_head(self, status, length):
        self.send_response(status)
        self.send_header("Content-Length", str(length))
        self.end_headers()

    def read_body(self):
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def do_GET(self):
        path, _, query = self.path.partition("?")
        _, path = registry_path(path)
        if path == "/token":
            self.record(f"GET token {query}")
            self.answer(200, b'{"token":"fixture-token"}')
            return
        asset = ASSET.match(path)
        if asset:
            self.record(f"GET asset /{asset.group('rest')}")
            file = os.path.join(state, "assets", asset.group("rest"))
            if not os.path.isfile(file):
                self.answer(404, b"no such asset\n")
                return
            with open(file, "rb") as handle:
                self.answer(200, handle.read())
            return
        self.record(f"GET {path}")
        self.answer(404, b"no such path\n")

    def do_HEAD(self):
        path, _, _ = self.path.partition("?")
        _, path = registry_path(path)
        blob = BLOB.match(path)
        if blob:
            digest = blob.group("digest")
            self.record(f"HEAD blob {digest}")
            file = os.path.join(state, "blobs", digest)
            if flag("blob-head-404") or not os.path.isfile(file):
                self.answer_head(404, 0)
                return
            self.answer_head(200, os.path.getsize(file))
            return
        manifest = MANIFEST.match(path)
        if manifest:
            tag = manifest.group("tag")
            self.record(f"HEAD manifest {tag}")
            file = os.path.join(state, "manifests", tag)
            if not os.path.isfile(file):
                self.answer_head(404, 0)
                return
            self.answer_head(200, os.path.getsize(file))
            return
        self.record(f"HEAD {path}")
        self.answer_head(404, 0)

    def do_POST(self):
        path, _, _ = self.path.partition("?")
        prefix, path = registry_path(path)
        self.read_body()
        uploads = UPLOADS.match(path)
        if uploads:
            self.record("POST upload")
            location = f"{prefix}/v2/{uploads.group('repository')}/blobs/uploads/fixture-upload"
            if not flag("no-query-location"):
                location += "?_state=fixture-state"
            if flag("absolute-location"):
                location = f"http://{self.headers.get('Host')}{location}"
            self.answer(202, b"", (("Location", location),))
            return
        self.record(f"POST {path}")
        self.answer(404, b"no such path\n")

    def do_PUT(self):
        path, _, query = self.path.partition("?")
        _, path = registry_path(path)
        body = self.read_body()
        upload = UPLOAD.match(path)
        if upload:
            claimed = urllib.parse.parse_qs(query).get("digest", [""])[0]
            read = "sha256:" + hashlib.sha256(body).hexdigest()
            if claimed != read:
                self.record(f"PUT blob {read} claimed {claimed}")
                self.answer(400, b"the digest and the bytes disagree\n")
                return
            with open(os.path.join(state, "blobs", read), "wb") as handle:
                handle.write(body)
            self.record(f"PUT blob {read}")
            self.answer(201, b"", (("Docker-Content-Digest", read),))
            return
        manifest = MANIFEST.match(path)
        if manifest:
            tag = manifest.group("tag")
            with open(os.path.join(state, "manifests", tag), "wb") as handle:
                handle.write(body)
            self.record(f"PUT manifest {tag}")
            self.answer(201, b"")
            return
        self.record(f"PUT {path}")
        self.answer(404, b"no such path\n")

    def log_message(self, *args):
        pass


socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", 0), Handler) as server:
    with open(portfile, "w") as handle:
        handle.write(str(server.server_address[1]))
    server.serve_forever()
PY

# row <name>: that row of the fixture manifest. The `whole` row pushes its archive as it stands,
# the `inner` row pushes the member of its archive, the `packaged` row is an npm row, which
# needs no copy at all, and the `odd` row names a second source this repository reads from no
# service.
row() {
  case "$1" in
    whole)
      cat <<YAML
  - name: whole
    version: 1.0.0
    digest: $WHOLE
    digest_of: the fixture's release tarball, whole
    primary: $WHOLE_URL
    second_source: ghcr-mirror
    max_time: 7
    mirror_from: $WHOLE_URL
    mirror_member: ""
YAML
      ;;
    inner)
      cat <<YAML
  - name: inner
    version: 2.0.0
    digest: $INNER
    digest_of: the binary inside the fixture's release tarball
    primary: ""
    second_source: ghcr-mirror
    max_time: 7
    mirror_from: $INNER_URL
    mirror_member: ./inner
YAML
      ;;
    packaged)
      cat <<YAML
  - name: packaged
    version: 3.0.0
    digest: $WHOLE
    digest_of: the binary the npm package holds
    primary: ""
    second_source: npm
    max_time: 7
    npm_package: "@fixture/packaged"
    npm_member: package/bin/packaged
YAML
      ;;
    odd)
      cat <<YAML
  - name: odd
    version: 4.0.0
    digest: $WHOLE
    digest_of: the fixture's release tarball, whole
    primary: ""
    second_source: s3
    max_time: 7
YAML
      ;;
    *)
      echo "  FAIL: this test has no row named '$1'" >&2
      return 1
      ;;
  esac
}

# fixture <case> <row…>: this case's miniature repository and its origin, with the release host
# serving every named row's asset and the package holding nothing. Sets `root`, `state`, `log`
# and `base`. Each case names its own fixture: two cases sharing a name would share a tree and an
# origin, so a repeated name is refused here rather than surfacing as the `ln -s` below failing.
fixture() {
  local name=$1 waited=0 which
  shift
  root="$work/$name/repo"
  state="$work/$name/state"
  log="$state/requests.log"
  if [ -e "$work/$name" ]; then
    echo "  FAIL: the fixture name '$name' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$state/blobs" "$state/manifests" "$state/flags" \
    "$state/assets/$(dirname "$WHOLE_ASSET")" "$state/assets/$(dirname "$INNER_ASSET")"
  ln -s "$mirror_script" "$root/.github/scripts/mirror-ci-tools.sh"
  # The helper that script sources, beside it, as it is beside it in the repository.
  ln -s "$helper_script" "$root/.github/scripts/ci-tools.sh"
  : > "$log"
  printf 'registry: %s\n\ntools:\n' "$REGISTRY" > "$root/.github/ci-tools.yaml"
  for which in "$@"; do
    row "$which" >> "$root/.github/ci-tools.yaml"
    case "$which" in
      whole)
        cp "$work/payloads/whole.tar.gz" "$state/assets/$WHOLE_ASSET"
        ;;
      inner)
        cp "$work/payloads/inner.tar.gz" "$state/assets/$INNER_ASSET"
        ;;
    esac
  done
  python3 "$work/origin.py" "$state" "$work/$name/port" &
  servers+=("$!")
  while [ ! -s "$work/$name/port" ]; do
    if [ "$waited" -gt 100 ]; then
      echo "  FAIL: the origin of '$name' did not write its port" >&2
      return 1
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  base="http://127.0.0.1:$(cat "$work/$name/port")"
}

# run_mirror [environment assignment…]: the script under test against this case's tree and
# origin. Sets `out` to the combined output and `status` to the exit code. Not a subshell, so
# both survive the call.
run_mirror() {
  set +e
  out=$(env \
    CI_TOOLS_GHCR_BASE="$base" \
    CI_TOOLS_GITHUB_BASE="$base/github" \
    GITHUB_ACTOR="$ACTOR" \
    GITHUB_TOKEN=fixture-password \
    PATH="$work/bin:$PATH" \
    RUNNER_TEMP="$runner_temp" \
    "$@" "$root/.github/scripts/mirror-ci-tools.sh" 2>&1)
  status=$?
  set -e
}

# requests <kind>: the lines of this case's log that start with that word, joined by spaces, so a
# case states the order of what the run wrote as one string.
requests() { grep "^$1" "$log" | tr '\n' ' ' || true; }

# temporary_files: what the script left under RUNNER_TEMP, which is nothing on every path.
temporary_files() { find "$runner_temp" -mindepth 1 | sort; }

echo "case: a row the package holds"
fixture holds-the-row whole
cp "$work/payloads/whole.tar.gz" "$state/blobs/sha256:$WHOLE"
printf 'a manifest\n' > "$state/manifests/$WHOLE_TAG"
run_mirror
check "exits 0" "$(is "$status" 0)"
check "holds the row" "$(contains "$out" "^held whole sha256:$WHOLE ($WHOLE_TAG)$")"
check "counts it" "$(contains "$out" '^1 ghcr-mirror row(s) held, 0 filled, 0 cold, and 0 row(s) that need no copy$')"
check "asked for a pull,push token" \
  "$(contains "$(cat "$log")" "^GET token service=ghcr.io&scope=repository:$REGISTRY_PATH:pull,push$")"
check "read the blob and the tag" \
  "$(is "$(requests HEAD)" "HEAD blob sha256:$WHOLE HEAD manifest $WHOLE_TAG ")"
check "pushed nothing" "$(is "$(requests PUT)$(requests POST)" "")"
check "read no release asset" "$(is "$(requests 'GET asset')" "")"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: a row the package holds whose tag is missing"
# An untagged blob is a package version nothing names, and a retention policy may take it, so the
# tag is pushed over the blob that is already there.
fixture holds-the-row-untagged whole
cp "$work/payloads/whole.tar.gz" "$state/blobs/sha256:$WHOLE"
run_mirror
check "exits 0" "$(is "$status" 0)"
check "holds the row" "$(contains "$out" "^held whole sha256:$WHOLE ($WHOLE_TAG)$")"
check "pushed the config and the manifest, and no layer" \
  "$(is "$(requests PUT)" "PUT blob sha256:$WHOLE_CONFIG PUT manifest $WHOLE_TAG ")"
check "the manifest names the layer the package already held" \
  "$(contains "$(cat "$state/manifests/$WHOLE_TAG")" "\"digest\":\"sha256:$WHOLE\",\"size\":$WHOLE_SIZE")"
check "read no release asset" "$(is "$(requests 'GET asset')" "")"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: a row the package lacks"
fixture fills-the-row whole
run_mirror
check "exits 0" "$(is "$status" 0)"
check "fills the row" "$(contains "$out" "^filled whole sha256:$WHOLE ($WHOLE_TAG)$")"
check "counts it" "$(contains "$out" '^0 ghcr-mirror row(s) held, 1 filled, 0 cold, and 0 row(s) that need no copy$')"
check "read the release asset" \
  "$(is "$(requests 'GET asset')" "GET asset /$WHOLE_ASSET ")"
check "pushed the layer, then the config, then the manifest" \
  "$(is "$(requests PUT)" "PUT blob sha256:$WHOLE PUT blob sha256:$WHOLE_CONFIG PUT manifest $WHOLE_TAG ")"
check "started one upload per blob" "$(is "$(requests POST)" "POST upload POST upload ")"
check "the package now holds the whole archive, which is what this row pushes" \
  "$(is "$(wc -c < "$state/blobs/sha256:$WHOLE")" "$WHOLE_SIZE")"
check "the manifest names the archive layer media type" \
  "$(contains "$(cat "$state/manifests/$WHOLE_TAG")" '"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip"')"
check "and the asset's own file name" \
  "$(contains "$(cat "$state/manifests/$WHOLE_TAG")" '"org.opencontainers.image.title":"whole.tar.gz"')"
check "and the config blob it pushed" \
  "$(contains "$(cat "$state/manifests/$WHOLE_TAG")" "\"digest\":\"sha256:$WHOLE_CONFIG\"")"
check "read the blob back after the push" \
  "$(is "$(requests HEAD)" "HEAD blob sha256:$WHOLE HEAD manifest $WHOLE_TAG HEAD blob sha256:$WHOLE ")"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: a fill the package did not keep"
# The asset answered and the pushes were accepted, and the blob is not there. That is a defect of
# the fill and never an outage, so it reddens the run.
fixture fill-does-not-land whole
: > "$state/flags/blob-head-404"
run_mirror
check "fails" "$(is "$status" 1)"
check "names the row and the read that answered" \
  "$(contains "$out" "::error file=.github/ci-tools.yaml::whole: this run pushed the blob, the config and the manifest, and a HEAD of $base/v2/$REGISTRY_PATH/blobs/sha256:$WHOLE then answered 404")"
check "says it is a defect of the fill" \
  "$(contains "$out" 'this is a defect of the fill and not an outage')"
check "says nothing was filled" \
  "$(is "$(contains "$out" '^filled ')" false)"

echo "case: an asset that serves bytes of another digest"
fixture asset-changed whole
cp "$work/payloads/other.tar.gz" "$state/assets/$WHOLE_ASSET"
run_mirror
check "fails" "$(is "$status" 1)"
# The digest check is .github/scripts/ci-tools.sh's, the one every install of this copy runs,
# so the refusal naming both digests is that file's and the URL that served the bytes is this
# script's.
check "the helper names the row and both digests" \
  "$(contains "$out" "::error::ci-tools: whole: the bytes from github-release are not the pinned ones (pinned sha256 $WHOLE, read sha256 $OTHER) — refusing")"
check "and this script names the URL that served them" \
  "$(contains "$out" "::error file=.github/ci-tools.yaml::whole: $WHOLE_URL served those bytes")"
check "says nothing is pushed" "$(contains "$out" 'so nothing is pushed')"
check "and pushes nothing" "$(is "$(requests PUT)$(requests POST)" "")"

echo "case: a blob the package held and no longer holds"
# The tag resolves and the blob is gone, which is the garbage collection that tag exists to stop.
# A run that filled it again would hide it.
fixture blob-collected whole
printf 'a manifest\n' > "$state/manifests/$WHOLE_TAG"
run_mirror
check "fails" "$(is "$status" 1)"
check "names the row and both reads" \
  "$(contains "$out" "::error file=.github/ci-tools.yaml::whole: the package resolves $WHOLE_TAG and holds no blob sha256:$WHOLE")"
check "says what that is" "$(contains "$out" 'That is the garbage collection the tag exists to stop')"
check "read no release asset" "$(is "$(requests 'GET asset')" "")"
check "and pushes nothing" "$(is "$(requests PUT)$(requests POST)" "")"

echo "case: an asset that does not answer and a blob that is absent"
# The one case that stays green: the asset is down, the blob is absent, and nothing can be done.
# A cold copy must not redden ci-ok during the outage the copy exists to survive.
fixture asset-is-down whole
rm -- "$state/assets/$WHOLE_ASSET"
run_mirror
check "exits 0" "$(is "$status" 0)"
check "leaves the row cold" \
  "$(contains "$out" "^cold whole sha256:$WHOLE — $WHOLE_URL did not answer (curl exit 22)$")"
check "warns beside it" \
  "$(contains "$out" "^::warning::mirror-ci-tools: whole has no second source yet, because $WHOLE_URL did not answer this run")"
check "says each install keeps the source it has" \
  "$(contains "$out" 'Each install of this tool keeps the single source it has today')"
check "counts it" "$(contains "$out" '^0 ghcr-mirror row(s) held, 0 filled, 1 cold, and 0 row(s) that need no copy$')"
check "pushes nothing" "$(is "$(requests PUT)$(requests POST)" "")"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: a row with a mirror_member"
# The member is what every install is checked against, so the member is what the copy holds: the
# archive's own digest is a digest no row names. This case also hands the run an upload location
# written as a whole URL and with no query, which is the other half of what the registry API
# allows there.
fixture member-of-the-archive inner
: > "$state/flags/absolute-location"
: > "$state/flags/no-query-location"
run_mirror
check "exits 0" "$(is "$status" 0)"
check "fills the row" "$(contains "$out" "^filled inner sha256:$INNER ($INNER_TAG)$")"
check "pushed the member and not the archive" \
  "$(is "$(requests PUT)" "PUT blob sha256:$INNER PUT blob sha256:$INNER_CONFIG PUT manifest $INNER_TAG ")"
check "the package holds the member's own bytes and not the archive's" \
  "$(is "$(wc -c < "$state/blobs/sha256:$INNER")" "$INNER_SIZE")"
check "the manifest names the member's size" \
  "$(contains "$(cat "$state/manifests/$INNER_TAG")" "\"digest\":\"sha256:$INNER\",\"size\":$INNER_SIZE")"
check "and its media type, which is no archive" \
  "$(contains "$(cat "$state/manifests/$INNER_TAG")" '"mediaType":"application/octet-stream"')"
check "and the member's own name" \
  "$(contains "$(cat "$state/manifests/$INNER_TAG")" '"org.opencontainers.image.title":"inner"')"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: an upload location written as a path, with the package behind a path of its own"
# What the run puts in front of such a location is the scheme and host of the address it pushed
# to, which is what a path location is absolute from.
# Read off the CI_TOOLS_GHCR_BASE seam instead, it would ask this origin for
# /ghcr/ghcr/v2/..., which it serves nothing at, and the row would not be filled.
fixture location-behind-a-path whole
run_mirror CI_TOOLS_GHCR_BASE="$base/ghcr"
check "exits 0" "$(is "$status" 0)"
check "fills the row" "$(contains "$out" "^filled whole sha256:$WHOLE ($WHOLE_TAG)$")"
check "started one upload per blob" "$(is "$(requests POST)" "POST upload POST upload ")"
check "and both PUTs reached the package" \
  "$(is "$(requests PUT)" "PUT blob sha256:$WHOLE PUT blob sha256:$WHOLE_CONFIG PUT manifest $WHOLE_TAG ")"
check "the package holds the blob the run pushed" \
  "$(is "$(wc -c < "$state/blobs/sha256:$WHOLE")" "$WHOLE_SIZE")"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: an npm row"
fixture npm-row packaged
run_mirror
check "exits 0" "$(is "$status" 0)"
check "says the row needs no copy" \
  "$(contains "$out" '^none packaged needs no copy: its second source is npm, which serves the bytes itself$')"
check "counts it" "$(contains "$out" '^0 ghcr-mirror row(s) held, 0 filled, 0 cold, and 1 row(s) that need no copy$')"
check "and reads nothing for it" "$(is "$(requests HEAD)$(requests 'GET asset')" "")"

echo "case: a run with no GITHUB_TOKEN"
fixture no-push-credential whole
run_mirror GITHUB_TOKEN=
check "fails" "$(is "$status" 1)"
check "refuses in one sentence" \
  "$(is "$out" '::error file=.github/ci-tools.yaml::this run has no GITHUB_ACTOR and GITHUB_TOKEN to mint a push token from, and it writes to the package ci.yaml'"'"'s ci-images job logs in to, so it reads nothing and fills nothing.')"
check "and reads nothing at all" "$(is "$(cat "$log")" "")"

echo "case: a row whose second source this repository reads from no service"
# Each row's word is read inside the loop, where a read that fails stops the run. Read outside
# it, in the list of a `for`, that failure would be no failure at all under errexit, and every
# row of the manifest would be reported as one that needs no copy.
fixture second-source-unknown whole odd
cp "$work/payloads/whole.tar.gz" "$state/blobs/sha256:$WHOLE"
printf 'a manifest\n' > "$state/manifests/$WHOLE_TAG"
run_mirror
check "fails" "$(is "$status" 1)"
check "naming the row and the word" \
  "$(contains "$out" "^::error::ci-tools: odd: the manifest's second_source is 's3', and this file reads ghcr-mirror and npm\$")"
check "stops at that row, after the row before it was read" \
  "$(contains "$out" "^held whole sha256:$WHOLE ($WHOLE_TAG)\$")"
check "says of no row that it needs no copy" "$(is "$(contains "$out" 'needs no copy')" false)"
check "and prints no summary line" "$(is "$(contains "$out" 'row(s) held')" false)"

summary "mirror-ci-tools.sh"
