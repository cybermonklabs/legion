#!/usr/bin/env bash
# The rig of .github/workflows/ci.yaml's `tool-fallback` and `tool-fallback-arm` jobs, the live
# proof that every tool install of this repository reads the second source .github/ci-tools.yaml
# names when the first source does not serve the bytes, and that no unverified byte is ever
# installed (dispatch://INFRA-1873).
# Those two jobs are this script's only callers, and it is this script's own exercise: every
# verb below either prints a reading a job is judged on or refuses the job itself, so a rig that
# misbehaves is a job that fails, and both jobs run on every change that touches what they prove
# (the `tool_fallback` filter of the `changes` job names this file among them, and the
# `tool_fallback_arm` filter is that list itself).
# A second source nothing exercises rots, and this repository's own principle is that testing
# means booting and running it, so the rig is part of the proof rather than a test beside it.
#
# THE VERBS, in the order the `tool-fallback` job runs them; `tool-fallback-arm` runs
# `start-fault`, `probe` and `installed-bun`.
#
#   origin-start
#       Writes a small python3 HTTP origin to $RUNNER_TEMP/ci-tools-proof/origin.py, starts it on
#       127.0.0.1 at a port the kernel picks, and writes `github=`, `ghcr=` and `npm=` to
#       $GITHUB_OUTPUT, the three base URLs a case hands .github/scripts/ci-tools.sh through its
#       CI_TOOLS_GITHUB_BASE, CI_TOOLS_GHCR_BASE and CI_TOOLS_NPM_BASE seams.
#       The origin serves the three routes those seams reach — a release asset, a ghcr blob with
#       the pull token before it, and an npm tarball — from the payload `stage` wrote, and it
#       decides for every request, from the control file `corrupt` writes, whether to serve the
#       row whole or with its last byte changed.
#       It logs every path it serves, the row it belongs to and whether it corrupted it.
#
#   stage
#       Fetches the correct bytes of every row a case serves, through
#       .github/scripts/ci-tools.sh itself, into $RUNNER_TEMP/ci-tools-proof/payload/.
#       It runs before the fault starts, while the network is whole, because these are the bytes
#       the origin stands in with once no real source answers.
#       It prints each file with its size and its sha256, and refuses the job naming the row
#       when a fetch or a digest does not come out right.
#
#   start-fault --hosts <list>
#       Starts .github/scripts/ci-fault-proxy.py --mode fail over those hosts, waits for its
#       port, and writes `proxy=http://127.0.0.1:<port>` to $GITHUB_OUTPUT, which the job then
#       puts into $GITHUB_ENV as every spelling of the proxy variables.
#       Its log is $RUNNER_TEMP/ci-tools-proof/fault.log.
#
#   probe <faulted host>
#       The pair that reads the fault before any install meets it: one request through the proxy
#       to that host, which the proxy must refuse with HTTP 504, and one to that same name under
#       the reserved `.invalid` suffix (RFC 2606), which the proxy must answer itself with HTTP
#       502, having failed to resolve a name nobody owns and no resolver answers for.
#       Both are printed as they were read, each with the proxy's own log line for it, and the
#       verb fails naming which of the two was wrong.
#       The second reading is what catches a bypass: a request that never reached the proxy
#       leaves no line in its log, whatever carried it past.
#
#   version-answers <row> <printed>
#       Reads what one installed tool printed as its version against the row that pins it.
#       Prints the reading and returns non-zero where the two do not agree, so the step that
#       runs it keeps its own list of the tools that answered wrongly.
#
#   installed-bun <row>
#       The bun on this job's PATH, read against that row: the machine, what `bun --version`
#       answers, the path `command -v bun` gives, the sha256 of that file, and the row's own
#       version and digest.
#       It refuses the job where the version is not the one .bun-version pins or the binary is
#       not that row's bytes, and it is what the arm64 job's two halves are read by.
#
#   corrupt [row…]
#       Writes the control file the origin reads, with those rows or with none, and prints what
#       it wrote.
#       It also marks where both logs stand, so the `report` after it prints the lines of its own
#       case and of no other.
#       Every case calls it, the cases that corrupt nothing included: the control file is one
#       file for the whole job, and a case that did not empty it would read the case before it.
#
#   flip-last-byte <file>
#       Changes the last byte of that file and prints its sha256 afterwards, for the cases whose
#       corrupted payload is a binary on the runner rather than something the origin serves.
#
#   payload-path <row>
#       The file `stage` wrote this row's own pinned bytes to, which is the name the origin
#       serves them under on its ghcr route.
#       A case that must put those bytes on the runner copies them from there, so this rig's own
#       directory layout is named here and in no step of the job.
#       Only a row whose second source is the ghcr copy has such a file: an npm row's payload is
#       the tarball that registry serves, and the binary is a member inside it.
#
#   seed-mise-jj <file>
#       Writes that file as the jj binary inside mise's own install directory for the tool
#       packages/daemon/docker/worker.Dockerfile pins, which is where
#       .github/actions/install-jj reads an installed jj from, and prints the path and the
#       file's sha256.
#       Measured in this issue's pod: with
#       `$MISE_DATA_DIR/installs/github-sjawhar-jj/<version>/jj` in place as a file of mode 0755
#       and nothing else there, `mise install github:sjawhar/jj@<version>` exits 0 without
#       reaching the network — "installed but not activated" is its whole output — and
#       `mise x github:sjawhar/jj@<version> -- sh -c 'command -v jj'` prints that path.
#       The jj-primary case uses it because a live `mise install` inside the fault is the thing
#       that made this job red during an outage: jj's release asset is on the host the fault
#       holds, so a case about the bytes mise installed would spend the outage downloading
#       through a 504, and the job that proves the outage survivable would fail in it.
#
#   report <case> --outcome <the step's outcome> --row <row>
#          [--expect-source <name>] [--expect-untried <name>]
#       Prints the case, the outcome the install step reached, the lines the origin logged for
#       this case and the lines the fault proxy logged for it, and then decides the case.
#       It is this verb, and not the install step, that passes or fails the job: every install
#       step of a case carries `continue-on-error: true`, so the job reads each case rather than
#       stopping at the first install that refuses.
#       Every case of this job is a refusal, so the outcome it holds the step to is `failure`;
#       the row the case is about is given rather than worked out of the case's name.
#       It fails when the step did not fail, when `--expect-source` names a source this case's
#       reading does not, and when `--expect-untried` names a source the origin served for this
#       case's row after all.
#
# HOW `--expect-source` IS READ.
# A composite action writes its refusal to the job's log, and no later step can read that log, so
# the source a refusal named is read from what the origin served instead.
# Each route is one source: /github/ is `github-release`, /ghcr/ is `ghcr-mirror`, /npm/ is
# `npm`, and the origin's log says which route served this case's row.
# A case whose primary is the faulted real host asks the origin's github route for nothing, so
# `--expect-source github-release` is read as the same statement the other way round: the origin
# served neither second source for this row, so the bytes the step refused came from the primary
# path and from nowhere else.
#
# Everything this script writes lives under $RUNNER_TEMP/ci-tools-proof/ (under $TMPDIR off a
# runner), so one directory holds the whole rig and the job's own temporary files are elsewhere.
#
# Run from anywhere; the repository root is derived from this script's own path.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/../.." && pwd)
work="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ci-tools-proof"
payload="$work/payload"
control="$work/corrupt"
origin_log="$work/origin.log"
fault_log="$work/fault.log"
fault_state="$work/fault.state"
mark="$work/mark"

# The rows a case installs, which are the rows `stage` fetches the correct bytes of.
# The two other bun rows are the same binary for another runner (.github/ci-tools.yaml says so),
# and no case installs them, so nothing would serve their payload.
STAGED_ROWS="crane changie mise jj bun-linux-x64"

# RFC 2606's reserved suffix, which `probe` hangs on the faulted host to make its control.
# Nobody owns a name under it and no resolver answers for one, so only the proxy can answer a
# request for it, and a request made around the proxy reads as nothing at all.
RESERVED_SUFFIX=".invalid"

# The bound in seconds each of the probe's two requests gets.
# Both answers are the proxy's own and immediate, so this is spent only where nothing answers at
# all, which is the reading the probe refuses the job for.
PROBE_BOUND=15

# shellcheck source=.github/scripts/ci-tools.sh
. "$root/.github/scripts/ci-tools.sh"

die() {
  echo "::error::ci-tools-proof: $*" >&2
  exit 1
}

note() {
  printf 'ci-tools-proof: %s\n' "$*"
}

# wait_for_file <file> <what it is>: the port file a server writes by rename, waited for rather
# than raced. Thirty seconds, in tenths, which is longer than either server takes to bind.
wait_for_file() {
  local file=$1 what=$2 waited=0
  while [ ! -s "$file" ]; do
    sleep 0.1
    waited=$((waited + 1))
    [ "$waited" -lt 300 ] || die "$what reported no port in 30 s"
  done
}

# ---------------------------------------------------------------- origin-start

# write_origin: the origin program, in the directory the whole rig writes to.
# It reads .github/ci-tools.yaml once, at startup, to learn which row each route is about: the
# manifest cannot change while the job runs, and the one thing that does change between cases —
# which rows are served corrupted — is the control file, read on every request.
write_origin() {
  cat > "$work/origin.py" <<'PY'
"""The three sources .github/scripts/ci-tools.sh reads, served from one local origin.

Routes, each the one a seam of that helper reaches:

  GET /github/<any path>/<file>                  the payload of that file name
  GET /ghcr/token?service=...&scope=...          {"token":"proof"}
  GET /ghcr/v2/<path>/blobs/sha256:<digest>      the payload of the row that digest pins
  GET /npm/<package>/-/<file>.tgz                the payload of that file name

Every request is decided by the control file, read again each time: a row named in it is served
with its last byte changed, and a row not named in it is served whole. Each request is logged
with the row it belongs to and whether it was corrupted.
"""

import http.server
import json
import os
import re
import socketserver
import sys
from pathlib import Path

import yaml

manifest, payload, control, logfile, portfile = (Path(argument) for argument in sys.argv[1:6])

document = yaml.safe_load(manifest.read_text()) or {}
ROWS = document.get("tools") or []
GHCR_BLOB = re.compile(r"^/ghcr/v2/(?P<path>.+)/blobs/sha256:(?P<digest>[0-9a-f]{64})$")
NPM_TARBALL = re.compile(r"^/npm/(?P<package>.+)/-/(?P<file>[^/]+\.tgz)$")


def basenames(row):
    """The file names a release asset of this row is served under: its primary's and its
    mirror_from's, which are one name where the row holds both and they agree."""
    return {str(row.get(field) or "").rsplit("/", 1)[-1] for field in ("primary", "mirror_from")} - {""}


def resolve(path):
    """The row this request is about and the payload file it is served from, or (None, None)."""
    if path.startswith("/github/"):
        name = path.split("?", 1)[0].rsplit("/", 1)[-1]
        for row in ROWS:
            if name in basenames(row):
                return row, name
        return None, None
    blob = GHCR_BLOB.match(path.split("?", 1)[0])
    if blob:
        for row in ROWS:
            if row.get("digest") == blob.group("digest"):
                return row, "sha256:" + blob.group("digest")
        return None, None
    tarball = NPM_TARBALL.match(path.split("?", 1)[0])
    if tarball:
        for row in ROWS:
            if row.get("npm_package") == tarball.group("package"):
                return row, tarball.group("file")
    return None, None


def corrupted_rows():
    """The rows this request must serve with their last byte changed, read again for every
    request: one origin serves every case of the job, and a case says what it wants here."""
    if not control.exists():
        return set()
    return set(control.read_text().split())


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/ghcr/token"):
            self.answer(200, json.dumps({"token": "proof"}).encode() + b"\n", "-", "no")
            return
        row, name = resolve(self.path)
        if row is None:
            self.answer(404, b"ci-tools-proof: no row of the manifest is served at this path\n", "-", "no")
            return
        file = payload / name
        if not file.is_file():
            self.answer(404, b"ci-tools-proof: the rig staged no payload for this row\n", row["name"], "no")
            return
        body = file.read_bytes()
        corrupt = row["name"] in corrupted_rows()
        if corrupt:
            # The last byte, changed: the smallest edit that moves the sha256 of the whole file,
            # and the one that leaves an archive's own framing alone until the digest is read.
            body = body[:-1] + bytes([body[-1] ^ 0x01])
        self.answer(200, body, row["name"], "yes" if corrupt else "no")

    def answer(self, status, body, row, corrupt):
        with open(logfile, "a") as log:
            log.write(f"GET {self.path} row={row} corrupt={corrupt} status={status} bytes={len(body)}\n")
        self.send_response(status)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Content-Type", "application/octet-stream")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        """The log above is the one this rig reads, one line per request and in its own words."""


class Origin(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


with Origin(("127.0.0.1", 0), Handler) as server:
    port = server.server_address[1]
    temporary = f"{portfile}.{os.getpid()}.tmp"
    with open(temporary, "w") as handle:
        handle.write(f"{port}\n")
    os.replace(temporary, portfile)
    server.serve_forever()
PY
}

origin_start() {
  [ "$#" -eq 0 ] || die "origin-start takes no argument, and this call gave $#"
  mkdir -p "$payload"
  : > "$control"
  write_origin
  local port_file="$work/origin.port"
  rm -f "$port_file"
  python3 "$work/origin.py" "$root/.github/ci-tools.yaml" "$payload" "$control" "$origin_log" "$port_file" \
    > "$work/origin.stderr" 2>&1 &
  wait_for_file "$port_file" "the origin"
  local base
  base="http://127.0.0.1:$(cat "$port_file")"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    {
      echo "github=$base/github"
      echo "ghcr=$base/ghcr"
      echo "npm=$base/npm"
    } >> "$GITHUB_OUTPUT"
  fi
  note "the origin serves $base/github, $base/ghcr and $base/npm, and logs to $origin_log"
}

# ---------------------------------------------------------------- stage

# payload_names <row>: every file name the origin serves this row's bytes under, one per line.
# A release asset is served under its own file name, a ghcr blob under the digest its URL names,
# and an npm package under the tarball name the registry serves it as.
payload_names() {
  local row=$1 primary mirror_from second package version
  primary=$(ci_tools_field "$row" primary)
  second=$(ci_tools_field "$row" second_source)
  {
    [ -z "$primary" ] || printf '%s\n' "${primary##*/}"
    if [ "$second" = ghcr-mirror ]; then
      mirror_from=$(ci_tools_field "$row" mirror_from)
      [ -z "$mirror_from" ] || printf '%s\n' "${mirror_from##*/}"
      printf 'sha256:%s\n' "$(ci_tools_digest "$row")"
    else
      package=$(ci_tools_field "$row" npm_package)
      version=$(ci_tools_field "$row" version)
      printf '%s-%s.tgz\n' "${package##*/}" "$version"
    fi
    # A row whose `primary` is its `mirror_from` names one file twice, and the origin serves one
    # file: the name is printed once, in the order the names were written.
  } | awk '!seen[$0]++'
}

# describe <file>: the one line `stage` prints per payload, which is what a reader checks the
# origin against.
describe() {
  local file=$1 size read_back
  size=$(wc -c < "$file")
  read_back=$(sha256sum -- "$file")
  printf '  %s: %s bytes, sha256 %s\n' "${file##*/}" "$size" "${read_back%% *}"
}

# pack_npm <row> <binary> <tarball>: the binary as the npm registry serves it, a gzipped tar whose
# one member is the row's `npm_member`, so the helper's own `tar -xzO` of it reads the same bytes
# it reads from the registry.
# The member is a hard link and the compression is gzip's fastest level: both files are under
# this rig's own directory, and the tarball is served over 127.0.0.1 to a reader that takes any
# level. Measured here on the 107 MB Bun binary: 0.3 s to copy it and 9.6 s to compress it at
# the default level, against 4.4 s at -1, all of it on the proof job's critical path.
pack_npm() {
  local row=$1 binary=$2 tarball=$3 member staging
  member=$(ci_tools_field "$row" npm_member)
  staging="$work/pack-$row"
  rm -rf "$staging"
  mkdir -p "$staging/$(dirname "$member")"
  ln -- "$binary" "$staging/$member"
  tar --use-compress-program='gzip -1' -cf "$tarball" -C "$staging" "$member"
  rm -rf "$staging"
}

stage() {
  [ "$#" -eq 0 ] || die "stage takes no argument, and this call gave $#"
  mkdir -p "$payload"
  local row digest file name read_back failures=""
  for row in $STAGED_ROWS; do
    note "staging $row"
    digest=$(ci_tools_digest "$row")
    file="$work/staged-$row"
    rm -f "$file"
    # The row's own path, which the helper decides: a row with a primary is tried there first,
    # and a row whose primary bytes a third party fetches falls straight through to the second
    # source.
    ci_tools_fetch "$row" "$file" || { failures="$failures $row"; continue; }
    while read -r name; do
      case $name in
        *.tgz)
          pack_npm "$row" "$file" "$payload/$name"
          # The round trip, because the origin serves this tarball and the helper reads the
          # member out of it: a pack that did not come back as the pinned bytes is this rig's
          # own defect and must not read as the install's.
          read_back=$(tar -xzOf "$payload/$name" "$(ci_tools_field "$row" npm_member)" | sha256sum)
          [ "${read_back%% *}" = "$digest" ] ||
            die "$row: the tarball this rig packed holds ${read_back%% *}, not the pinned $digest"
          ;;
        *) cp -- "$file" "$payload/$name" ;;
      esac
      describe "$payload/$name"
    done < <(payload_names "$row")
    rm -f "$file"
  done
  [ -z "$failures" ] ||
    die "no correct bytes for$failures, so the origin cannot stand in for their source; the copy of every ghcr-mirror row is filled by .github/scripts/mirror-ci-tools.sh in the ci-images job this job needs"
  note "the payload the origin serves is $payload"
}

# ---------------------------------------------------------------- start-fault

start_fault() {
  local hosts=""
  while [ "$#" -gt 0 ]; do
    case $1 in
      --hosts)
        hosts=$2
        shift 2
        ;;
      *) die "start-fault: unknown argument '$1'" ;;
    esac
  done
  [ -n "$hosts" ] || die "start-fault: --hosts is required"
  mkdir -p "$work"
  local port_file="$work/fault.port"
  rm -f "$port_file" "$fault_log"
  : > "$fault_log"
  python3 "$root/.github/scripts/ci-fault-proxy.py" --mode fail --hosts "$hosts" \
    --port 0 --port-file "$port_file" > "$fault_log" 2>&1 &
  wait_for_file "$port_file" "the fault proxy"
  local proxy
  proxy="http://127.0.0.1:$(cat "$port_file")"
  {
    echo "proxy=$proxy"
    echo "hosts=$hosts"
  } > "$fault_state"
  [ -z "${GITHUB_OUTPUT:-}" ] || echo "proxy=$proxy" >> "$GITHUB_OUTPUT"
  note "the fault proxy is $proxy, answering HTTP 504 for $hosts and passing every other host on"
}

# ---------------------------------------------------------------- probe

# through_proxy <url>: one request made the way an install makes its own, over https and with
# the proxy variables and nothing else, printed as `<status> <seconds>`.
# The status is the proxy's own answer, read from whichever field of curl carries it: a refused
# CONNECT leaves `%{http_code}` at 000 and puts the proxy's status in `%{http_connect}`
# (measured with curl 8.14.1: `--proxy <this proxy> https://github.com/` reads http_code 000 and
# http_connect 504), while a plain request carries it in `%{http_code}`.
# A request nothing answered at all reads 000 in both, which is the reading that says it never
# reached the proxy.
# Its failure is the reading rather than a fault of this script, so curl's own exit status is
# consumed here.
through_proxy() {
  local url=$1 proxy reading code connect seconds
  proxy=$(sed -n 's/^proxy=//p' "$fault_state")
  reading=$(http_proxy="$proxy" https_proxy="$proxy" HTTP_PROXY="$proxy" HTTPS_PROXY="$proxy" \
    no_proxy="" NO_PROXY="" curl -sS --max-time "$PROBE_BOUND" \
    -o /dev/null -w '%{http_code} %{http_connect} %{time_total}' "$url" 2>/dev/null || true)
  reading=${reading:-000 000 0}
  read -r code connect seconds <<<"$reading"
  [ "$code" != 000 ] || code=$connect
  printf '%s %s\n' "${code:-000}" "${seconds:-0}"
}

# proxy_line_after <line count>: the line the proxy wrote for the request just made, or nothing
# where it wrote none.
# The line lands a moment after the answer it belongs to, so it is waited for rather than missed;
# a request that never reached the proxy leaves none however long this waits.
proxy_line_after() {
  local skip=$1 waited=0 line=""
  while [ -z "$line" ] && [ "$waited" -lt 50 ]; do
    line=$(awk -v skip="$skip" 'NR > skip { print; exit }' "$fault_log")
    [ -n "$line" ] || sleep 0.1
    waited=$((waited + 1))
  done
  printf '%s\n' "$line"
}

# what_it_read <status> <seconds>: one reading, in the words a person reading the run counts it in.
what_it_read() {
  if [ "$1" = 000 ]; then
    printf 'no answer at all, after %ss' "$2"
  else
    printf 'HTTP %s, after %ss' "$1" "$2"
  fi
}

probe() {
  [ "$#" -eq 1 ] || die "probe takes the faulted host and nothing else, and this call gave $#"
  [ -f "$fault_state" ] || die "probe: no fault proxy was started"
  local faulted=$1 control_host="$1$RESERVED_SUFFIX" problems=0
  local before reading code seconds line

  note "the probe through the fault proxy, before any install meets it"

  before=$(wc -l < "$fault_log")
  reading=$(through_proxy "https://$faulted/")
  code=${reading%% *}
  seconds=${reading##* }
  line=$(proxy_line_after "$before")
  echo "  the faulted host $faulted read $(what_it_read "$code" "$seconds")"
  echo "  the proxy's own line for it: ${line:-none at all}"
  if [ "$code" != 504 ]; then
    echo "::error::ci-tools-proof: the faulted reading was wrong. This proxy answers HTTP 504 for $faulted, so the request must be refused with one; it read $(what_it_read "$code" "$seconds"). A faulted host that answers is the proxy bypassed: read no_proxy and NO_PROXY in this job's environment, since either one set to a star sends every host past the proxy." >&2
    problems=$((problems + 1))
  fi
  if [ -z "$line" ]; then
    echo "::error::ci-tools-proof: the faulted reading was wrong: the proxy logged no line for the request to $faulted, so that request was never made through it at all." >&2
    problems=$((problems + 1))
  fi

  before=$(wc -l < "$fault_log")
  reading=$(through_proxy "https://$control_host/")
  code=${reading%% *}
  seconds=${reading##* }
  line=$(proxy_line_after "$before")
  echo "  the unfaulted host $control_host read $(what_it_read "$code" "$seconds")"
  echo "  the proxy's own line for it: ${line:-none at all}"
  if [ "$code" != 502 ]; then
    echo "::error::ci-tools-proof: the unfaulted reading was wrong. $control_host is off this proxy's --hosts, so the proxy must answer it itself — HTTP 502, the answer it gives for a name nothing resolves — rather than fault it; it read $(what_it_read "$code" "$seconds"). Nothing answering at all is the request going nowhere near the proxy, and a 504 is the fault reaching a host it was never given." >&2
    problems=$((problems + 1))
  fi
  if [ -z "$line" ]; then
    echo "::error::ci-tools-proof: the unfaulted reading was wrong: the proxy logged no line for the request to $control_host, so that request was never made through it at all." >&2
    problems=$((problems + 1))
  fi

  [ "$problems" -eq 0 ] || exit 1
  echo "  the pair reads as it must, so every install below meets the fault rather than the host"
}

# ---------------------------------------------------------------- version-answers

# version_answers <row> <printed>: what one tool printed as its version, read against the row
# that pins it. The job runs it once per installed tool, before the corrupted cases and again
# after them.
# A fork's binary prints the version upstream released, where the row's version is the fork's
# own tag with a suffix on it (docs/solutions/daemon/omp-pin-bump-behavioral-proof.md:79
# measures that for the Oh My Pi fork this repository pins the same way), so a reading is held
# to the row's version or to the part of it before that suffix.
version_answers() {
  [ "$#" -eq 2 ] ||
    die "version-answers takes a row and what the tool printed, and this call gave $#"
  local row=$1 printed=$2 version
  version=$(ci_tools_field "$row" version)
  echo "$row answers '$printed', and its row pins $version"
  case $printed in
    *"$version"* | *"${version%%-*}"*) return 0 ;;
  esac
  return 1
}

# ---------------------------------------------------------------- installed-bun

# installed_bun <row>: the bun on this job's PATH read against that row, which is how a job on a
# runner architecture this repository proves nothing else on says which Bun it got.
# It prints the machine, what `bun --version` answers, the path `command -v bun` gives, the
# sha256 of that file, and the row's own version and digest, and it refuses the job where the
# version is not the one .bun-version pins or the binary is not that row's bytes.
#
# The row a reader wants is the one .github/actions/setup-bun chose from `uname -m`, and no step
# can read that out of the action: a composite action's step outputs are its own. It is read
# from the binary instead. That action held what it installed to the row it chose, this verb
# holds the same file to the row named here, and one file has one sha256, so two passing
# readings name one row — the three bun rows of .github/ci-tools.yaml do not share the aarch64
# row's digest.
installed_bun() {
  [ "$#" -eq 1 ] || die "installed-bun takes the row this runner's bun must be, and this call gave $#"
  local row=$1 bun digest version pinned printed read_back problems=0
  digest=$(ci_tools_digest "$row")
  version=$(ci_tools_field "$row" version)
  pinned=$(tr -d '[:space:]' < "$root/.bun-version")
  bun=$(command -v bun) || bun=""
  [ -n "$bun" ] ||
    die "installed-bun: no bun is on this job's PATH, so neither source of the $row row installed one"
  printed=$("$bun" --version)
  read_back=$(sha256sum -- "$bun")
  read_back=${read_back%% *}

  note "the bun this $(uname -m) runner holds, read against the $row row"
  echo "  bun --version answers $printed, and .bun-version pins $pinned"
  echo "  command -v bun gives $bun, whose sha256 is $read_back"
  echo "  the $row row pins version $version and sha256 $digest"
  if [ "$printed" != "$pinned" ]; then
    echo "::error::ci-tools-proof: the bun at $bun answers $printed, and .bun-version pins $pinned, so this runner installed another Bun than the one this repository locks." >&2
    problems=$((problems + 1))
  fi
  if [ "$read_back" != "$digest" ]; then
    echo "::error::ci-tools-proof: the bun at $bun is sha256 $read_back, and the $row row pins $digest, so the bytes on this runner are not the ones that row names." >&2
    problems=$((problems + 1))
  fi
  [ "$problems" -eq 0 ] || exit 1
  echo "  the bun on this runner is the $row row's pinned binary"
}

# ---------------------------------------------------------------- corrupt

corrupt() {
  mkdir -p "$work"
  local row
  for row in "$@"; do
    ci_tools_field "$row" digest > /dev/null ||
      die "corrupt: '$row' is no row of .github/ci-tools.yaml"
  done
  printf '%s\n' "$*" > "$control"
  # Where both logs stand now, so the `report` of this case prints its own lines and no earlier
  # case's. Every case calls this verb, so every case has a mark of its own.
  {
    printf 'origin=%s\n' "$([ -f "$origin_log" ] && wc -l < "$origin_log" || echo 0)"
    printf 'fault=%s\n' "$([ -f "$fault_log" ] && wc -l < "$fault_log" || echo 0)"
  } > "$mark"
  if [ "$#" -eq 0 ]; then
    note "the origin now serves every row whole"
  else
    note "the origin now serves $* with the last byte changed, and every other row whole"
  fi
}

# ---------------------------------------------------------------- flip-last-byte

# flip_last_byte <file>: that file with its last byte changed, and the sha256 it has afterwards.
# It is the origin's own corruption rule (write_origin above) applied to a file on the runner
# rather than to a body in memory: the smallest edit that moves the sha256 of the whole file,
# and the one that leaves an archive's own framing alone until the digest is read.
# The cases whose corrupted bytes are a binary on the runner rather than a body the origin
# serves use it.
flip_last_byte() {
  [ "$#" -eq 1 ] || die "flip-last-byte takes a file, and this call gave $#"
  python3 - "$1" <<'PY'
import sys

with open(sys.argv[1], "r+b") as handle:
    handle.seek(-1, 2)
    last = handle.read(1)
    handle.seek(-1, 2)
    handle.write(bytes([last[0] ^ 0x01]))
PY
  sha256sum "$1"
}

# ---------------------------------------------------------------- payload-path

# payload_path <row>: the file `stage` wrote this row's own pinned bytes to, which is the name
# the origin serves them under on its ghcr route (payload_names above writes both names).
# A case that must put those bytes on the runner copies them from there, so this rig's own
# directory layout is named here and in no step of .github/workflows/ci.yaml.
payload_path() {
  [ "$#" -eq 1 ] || die "payload-path takes a row, and this call gave $#"
  local row=$1 second
  second=$(ci_tools_field "$row" second_source)
  [ "$second" = ghcr-mirror ] ||
    die "payload-path: no payload holds the pinned bytes of '$row' on their own; its second source is '${second:-nothing}', whose payload is the archive that service serves"
  printf '%s/sha256:%s\n' "$payload" "$(ci_tools_digest "$row")"
}

# ---------------------------------------------------------------- seed-mise-jj

# seed_mise_jj <file>: that file as the jj binary inside mise's own install directory for the
# tool packages/daemon/docker/worker.Dockerfile pins, which is where .github/actions/install-jj
# reads an installed jj from.
# Measured in this issue's pod: with that one file in place as mode 0755 and nothing else in the
# directory, `mise install github:sjawhar/jj@<version>` exits 0 without reaching the network —
# "installed but not activated" is its whole output — and
# `mise x github:sjawhar/jj@<version> -- sh -c 'command -v jj'` prints that path.
# The jj-primary case is about the bytes mise installed and not about mise's fetch, and a live
# `mise install` inside the fault is what made this job red during an outage: jj's release asset
# is on the host the fault holds, so that install would spend the outage downloading through a
# 504 and the job that exists to prove the outage survivable would fail in it.
# Seeding the directory takes the download out of the case and leaves the reading it is about
# where it was.
seed_mise_jj() {
  [ "$#" -eq 1 ] || die "seed-mise-jj takes the file to seed mise's install with, and this call gave $#"
  local file=$1 dockerfile="$root/packages/daemon/docker/worker.Dockerfile" jj_tool version dir
  [ -f "$file" ] || die "seed-mise-jj: there is no file at '$file' to seed mise's install with"
  jj_tool=$(sed -n 's/^ARG JJ_TOOL=//p' "$dockerfile")
  [ -n "$jj_tool" ] || die "seed-mise-jj: $dockerfile names no ARG JJ_TOOL"
  version=${jj_tool##*@}
  [ "$version" != "$jj_tool" ] ||
    die "seed-mise-jj: the ARG JJ_TOOL '$jj_tool' names no version after an @, so this rig cannot name mise's install directory for it"
  dir="${MISE_DATA_DIR:-$HOME/.local/share/mise}/installs/github-sjawhar-jj/$version"
  # The directory holds this one file and nothing else, which is what the measurement above was
  # taken on: a half-written install an earlier `mise install` left behind is not that reading.
  rm -rf "$dir"
  mkdir -p "$dir"
  install -m 0755 -- "$file" "$dir/jj"
  note "mise's install of $jj_tool is now $dir/jj"
  sha256sum -- "$dir/jj"
}

# ---------------------------------------------------------------- report

# case_lines <log> <key>: the lines that log gained since this case's `corrupt` marked it.
case_lines() {
  local log=$1 key=$2 skip
  skip=$(sed -n "s/^$key=//p" "$mark")
  [ -f "$log" ] || return 0
  awk -v skip="${skip:-0}" 'NR > skip' "$log"
}

# origin_served <route> <row>: whether the origin served that row's bytes on that route for this
# case, which is what says a source answered.
# The lines are read into a variable first: `grep -q` ends at its first match, and a pipeline
# into it under pipefail can answer with the writer's broken pipe rather than with grep's match.
origin_served() {
  local lines
  lines=$(case_lines "$origin_log" origin)
  grep -qE "^GET $1[^ ]* row=$2 corrupt=(yes|no) status=200" <<<"$lines"
}

# route_of <source>: the origin route one source serves, read from the one word that source has
# — the name a refusal of .github/scripts/ci-tools.sh carries, which is also the manifest's own
# `second_source` word.
route_of() {
  case $1 in
    github-release) printf '/github/\n' ;;
    ghcr-mirror) printf '/ghcr/\n' ;;
    npm) printf '/npm/\n' ;;
    *) die "report: '$1' is no source this rig serves (github-release, ghcr-mirror or npm)" ;;
  esac
}

report() {
  [ "$#" -ge 1 ] || die "report takes a case name and its expectations"
  local name=$1 outcome="" row="" expect_source="" expect_untried=""
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --outcome)
        outcome=$2
        shift 2
        ;;
      --row)
        row=$2
        shift 2
        ;;
      --expect-source)
        expect_source=$2
        shift 2
        ;;
      --expect-untried)
        expect_untried=$2
        shift 2
        ;;
      *) die "report: unknown argument '$1'" ;;
    esac
  done
  [ -n "$outcome" ] || die "report: --outcome is required"
  [ -n "$row" ] || die "report: --row is required"
  ci_tools_field "$row" digest > /dev/null ||
    die "report: '$row' is no row of .github/ci-tools.yaml"
  [ -f "$mark" ] ||
    die "report: the case '$name' called no corrupt verb, so this rig cannot tell its lines from another case's"
  # Every case of this job is a refusal, so this is the one outcome a case is held to.
  local wanted=failure problems=0

  local served asked
  served=$(case_lines "$origin_log" origin)
  asked=$(case_lines "$fault_log" fault)
  note "case $name, on row $row: the install step's outcome was $outcome, and this case is the proof that it refuses"
  echo "  what the origin served for this case:"
  if [ -n "$served" ]; then
    sed 's/^/    /' <<<"$served"
  else
    echo "    nothing: no source of this case was the origin"
  fi
  echo "  what the fault proxy logged for this case:"
  if [ -n "$asked" ]; then
    sed 's/^/    /' <<<"$asked"
  else
    echo "    nothing: this case asked no faulted host"
  fi

  if [ "$outcome" != "$wanted" ]; then
    echo "::error::ci-tools-proof: case $name: the install step's outcome is $outcome, and this case is the proof that the install reaches $wanted: read the step's own log above for what it did instead." >&2
    problems=$((problems + 1))
  fi
  if [ -n "$expect_source" ]; then
    local route
    route=$(route_of "$expect_source")
    if [ "$route" = /github/ ]; then
      # The primary of this case is the faulted real host or a binary a third party installed, so
      # the origin's github route serves nothing for it. What says the bytes came from the
      # primary path is then that neither second source served this row.
      if ! origin_served /github/ "$row" && { origin_served /ghcr/ "$row" || origin_served /npm/ "$row"; }; then
        echo "::error::ci-tools-proof: case $name: this case is the refusal of the bytes $expect_source served, and the origin served a second source for $row above, so the step read another source than the one this case is about." >&2
        problems=$((problems + 1))
      fi
    elif ! origin_served "$route" "$row"; then
      echo "::error::ci-tools-proof: case $name: this case is the refusal of the bytes $expect_source served, and the origin served nothing for $row on its $route route, so the step never read that source." >&2
      problems=$((problems + 1))
    fi
  fi
  if [ -n "$expect_untried" ]; then
    local untried
    untried=$(route_of "$expect_untried")
    if origin_served "$untried" "$row"; then
      echo "::error::ci-tools-proof: case $name: the origin served $row on its $untried route, and this case is the rule that it must not: a digest mismatch is a supply-chain signal and never a reason to read another source." >&2
      problems=$((problems + 1))
    fi
  fi

  [ "$problems" -eq 0 ] || exit 1
  echo "  the case reads as it must"
}

verb=${1:-}
[ "$#" -ge 1 ] ||
  die "usage: $(basename "$0") <origin-start|stage|start-fault|probe|version-answers|installed-bun|corrupt|flip-last-byte|payload-path|seed-mise-jj|report> [argument…]"
shift
case $verb in
  origin-start) origin_start "$@" ;;
  stage) stage "$@" ;;
  start-fault) start_fault "$@" ;;
  probe) probe "$@" ;;
  version-answers) version_answers "$@" ;;
  installed-bun) installed_bun "$@" ;;
  corrupt) corrupt "$@" ;;
  flip-last-byte) flip_last_byte "$@" ;;
  payload-path) payload_path "$@" ;;
  seed-mise-jj) seed_mise_jj "$@" ;;
  report) report "$@" ;;
  *) die "'$verb' is no verb of this rig (origin-start, stage, start-fault, probe, version-answers, installed-bun, corrupt, flip-last-byte, payload-path, seed-mise-jj, report)" ;;
esac
