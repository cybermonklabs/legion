#!/usr/bin/env bash
# The rig of .github/workflows/ci.yaml's `tool-fallback` and `tool-fallback-arm` jobs, the live
# proof that every tool install of this repository reads the second source .github/ci-tools.yaml
# names when the first source does not serve the bytes, and that no unverified byte is ever
# installed (dispatch://INFRA-1873).
# Those two jobs are this script's only callers, and it is this script's own exercise: every
# verb below either prints a reading a job is judged on or refuses the job itself, so a rig that
# misbehaves is a job that fails, and both jobs run on every change that touches what they prove
# (the `tool_fallback` filter of the `changes` job names this file among them, and that one
# filter selects both jobs).
# A second source nothing exercises rots, and this repository's own principle is that testing
# means booting and running it, so the rig is part of the proof rather than a test beside it.
#
# THE VERBS, in the order the `tool-fallback` job runs them; `tool-fallback-arm` runs
# `start-fault`, `probe`, `installed-bun` and `stop-fault`.
#
#   origin-start
#       Writes a small python3 HTTP origin to $RUNNER_TEMP/ci-tools-proof/origin.py, starts it on
#       127.0.0.1 at a port the kernel picks, and writes `github=`, `ghcr=`, `npm=` and `url=` to
#       $GITHUB_OUTPUT, the four base URLs a case hands .github/scripts/ci-tools.sh through its
#       CI_TOOLS_GITHUB_BASE, CI_TOOLS_GHCR_BASE, CI_TOOLS_NPM_BASE and CI_TOOLS_URL_BASE seams.
#       The origin serves the four routes those seams reach — a release asset, a ghcr blob with
#       the pull token before it, an npm tarball, and the address a `pypi` or `pulumi-releases`
#       side of a row writes, whose scheme and host that last seam replaces — from the payload
#       `stage` wrote, and it decides for every request, from the control file `corrupt` writes,
#       whether to serve the row whole or with one byte changed.
#       It logs every path it serves, the row it belongs to and whether it corrupted it.
#
#   stage
#       Fetches the correct bytes of every row a case serves, through
#       .github/scripts/ci-tools.sh itself, into $RUNNER_TEMP/ci-tools-proof/payload/, under
#       every name the origin serves that row under: a row's own bytes reach one name as they
#       are, an npm tarball and a PyPI wheel are packed around them, and for a row whose own
#       `digest` is of a member — uv's is — the name the row's primary URL serves is staged
#       with ci_tools_fetch_primary and the member is taken out of that one staged archive,
#       because the primary serves the archive and not that member.
#       It runs before the fault starts, while the network is whole, because these are the bytes
#       the origin stands in with once no real source answers.
#       It prints each file with its size and its sha256, and refuses the job naming the row
#       when a fetch, a digest or a repack does not come out right.
#
#   shorten-ladder
#       Writes a stand-in curl to $RUNNER_TEMP/ci-tools-proof/bin/curl and puts that directory
#       on this job's PATH through $GITHUB_PATH, so every step after it runs that one rather
#       than the runner's.
#       The `tool-fallback` job calls it with the fault about to come up, and not before its
#       own honest fetches: `stage` and that job's jj-primary case each fetch a host the job
#       faults, and each must succeed on the pinned ladder, so the stand-in goes on PATH
#       after them and in front of the fault.
#       The stand-in runs the real curl with the arguments it was handed, and appends
#       `--retry-max-time 1` to them where the request is one this job arranged to fail: a host
#       FAULT_HOSTS holds, the dead address DEAD_ADDRESS below, or a dead proxy. curl reads the
#       last word on that option, so the appended bound wins over the helper's own
#       `--retry-max-time 60`, and nothing else of the request changes.
#       Every other fetch keeps the pinned ladder, which is what rides out a flap of npm, PyPI,
#       get.pulumi.com or the local origin, each of which this job reaches through the fault
#       proxy and none of which it faults.
#       It takes no argument, as `stage` does, and it refuses the job where no curl is on PATH
#       for the stand-in to run.
#       It logs one line per fetch to $RUNNER_TEMP/ci-tools-proof/curl.log — the seconds it
#       took, the decision, the host, curl's exit and the name CI_TOOLS_PROOF_FULL_LADDER
#       carries — and of the arguments only the host of the request, never the rest of its URL
#       and never another argument, because .github/scripts/ci-tools.sh hands curl its
#       credentials as arguments.
#       It exists because this job spent 1952 s of its 1994 s inside retry ladders the proof
#       does not read: a case here is a refusal this rig arranged, and a refusal arranged is a
#       ladder nobody reads (dispatch://INFRA-2621). One case keeps the ladder, and
#       `ladder-report` below is the reading of it.
#
#   start-fault
#       Starts .github/scripts/ci-fault-proxy.py --mode fail over FAULT_HOSTS below, the release
#       hosts both jobs fault, which is written there and in no step of either job, waits for its
#       port, and writes the four spellings of the two proxy variables and the two emptied bypass
#       lists to $GITHUB_ENV, printing each line as it wrote it, so a job routes its later steps
#       through the fault by running this verb and nothing else.
#       It takes no argument, as `stop-fault` does.
#       The proxy's own address goes to $RUNNER_TEMP/ci-tools-proof/fault.state, which is where
#       `probe` and `installed-bun --expect-refused` read it from; neither job gives this step an
#       id, so a step output would have no reader.
#       Its log is $RUNNER_TEMP/ci-tools-proof/fault.log.
#
#   stop-fault
#       The same four proxy variables, emptied in $GITHUB_ENV and printed. The steps after it
#       must not go through a proxy that stops with the job: the ci-ok-report step every job
#       ends with uploads an artifact.
#       The two bypass lists stay as `start-fault` wrote them, because with no proxy set
#       nothing reads them.
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
#       It also marks where the proxy's log stands when it ends, which is where this job's own
#       installs begin: `installed-bun --expect-refused` reads the refusals after that mark, so
#       it reads an install's 504 and never this probe's.
#
#   version-answers <row> <printed>
#       Reads what one installed tool printed as its version against the row that pins it.
#       Prints the reading and returns non-zero where the two do not agree, so the step that
#       runs it keeps its own list of the tools that answered wrongly.
#
#   refusals <host>
#       How many requests to that host the fault proxy refused since the probe marked its log,
#       and the lines themselves, printed beside the readings of the installs that succeeded:
#       the 504s that sent each of those installs to its second source.
#       It refuses the job where there were none, because a job whose installs all passed with
#       the fault refusing nothing is a job that reached its primaries after all.
#       `installed-bun --expect-refused` takes that same reading of that same log, and both go
#       through the one function that makes it, so neither can count a refusal the other does
#       not.
#
#   installed-bun <row> [--expect-refused <host>]
#       The bun on this job's PATH, read against that row: the machine, what `bun --version`
#       answers, the path `command -v bun` gives, the row's own version, and the digest check
#       ci_tools_verify makes of that file against that row, which is the one digest reader of
#       this repository, so the rig holds a binary to a row the way every action does.
#       It refuses the job where the version is not the one .bun-version pins or the binary is
#       not that row's bytes, and it is what the arm64 job's two halves are read by.
#       It takes the bunx reading `installed-bunx` makes after the digest and before
#       `--expect-refused`, since every call of it is taken after an install.
#       `--expect-refused <host>` adds the reading that says which source served those bytes:
#       the fault proxy must have refused a request to that host, after the probe marked its
#       log, with the 504 that sent the install to its second source. Neither the version nor
#       the digest can say it, since a row's two sources serve one binary by design and read
#       the same whichever answered, and no later step can read the `ci-tools:` line the
#       install wrote, since a composite action writes it to the job's log.
#
#   installed-bunx
#       The bunx on this job's PATH, read beside the bun on it: that a bunx is there at all,
#       that `bunx --version` answers the version .bun-version pins, and that it runs the same
#       bytes as that bun, which an install already held to its row's digest.
#       Bun's npm package holds the binary alone, so a Bun that second source served leaves no
#       bunx behind and .github/actions/setup-bun links one again; an install that left none
#       answers every version reading of `bun` and fails at the first `bunx` of a later job,
#       which is ci.yaml's lint, test, ci-caches and docs-site jobs and
#       .github/workflows/dispatch-browser.yaml.
#       `installed-bun` takes the same reading, so the arm64 job takes it from there; the x64
#       job, which reads its four faulted head installs tool by tool with `version-answers`,
#       runs this verb.
#
#   corrupt [row…]
#       Writes the control file the origin reads, with those rows or with none, and prints what
#       it wrote.
#       It also marks where both logs stand, so the `report` after it prints the lines of its own
#       case and of no other, and the second it ran in, so that `report` reads how long the
#       install between the two took.
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
#       the tarball that registry serves and a pypi row's is the wheel it serves, and the pinned
#       bytes are a member inside it.
#
#   seed-mise-jj <file>
#       Writes that file as the jj binary inside mise's own install directory for the tool
#       packages/daemon/docker/worker.Dockerfile pins, which is where
#       .github/actions/install-jj reads an installed jj from, and prints the path and the
#       file's sha256. That directory is derived from the `ARG JJ_TOOL` spec rather than
#       written here, so an ARG that moves to another fork moves the seed with it.
#       Measured in this issue's pod: with
#       `$MISE_DATA_DIR/installs/<the spec's own directory>/<version>/jj` in place as a file of
#       mode 0755 and nothing else there, `mise install github:sjawhar/jj@<version>` exits 0
#       having downloaded nothing — "installed but not activated" is its whole output — and
#       `mise x github:sjawhar/jj@<version> -- sh -c 'command -v jj'` prints that path.
#       It is no offline install: measured in CI at run 42507815176, that `mise install`
#       resolves the tool's version list over api.github.com even when the directory already
#       holds the tool, so the case that seeds it runs before the fault rather than inside it.
#       The jj-primary case uses it because a live `mise install` inside the fault is the thing
#       that made this job red during an outage: jj's release asset is on the host the fault
#       holds, so a case about the bytes mise installed would spend the outage downloading
#       through a 504, and the job that proves the outage survivable would fail in it.
#
#   install-bound
#       The seconds no timed install may reach, printed and nothing else.
#       INSTALL_BOUND below is the one place that number is written, and the `tool-fallback`
#       job's control over its eight installs with nowhere to go reads it through this verb, so
#       the bound the rig holds the sixteen corrupted cases to and the bound that step holds
#       those eight installs to cannot drift apart.
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
#       It fails when the step did not fail, when the install took INSTALL_BOUND seconds or more
#       — the reading that tells a step the runner ended at its `timeout-minutes` from a step
#       that refused the payload, since both leave `outcome=failure` behind them — when
#       `--expect-source` names a source this case's reading does not, and when
#       `--expect-untried` names a source the origin served for this case's row after all.
#
#   ladder-report
#       Every fetch the stand-in above measured, printed as the host, the decision, curl's
#       exit, the seconds it took and the name that fetch carried, and then the reading that
#       holds the one measurement this job keeps.
#       Every run that reads the log through, whether that reading passes or refuses, also
#       prints the count of the fetches that carried CI_TOOLS_PROOF_FULL_LADDER, beside the
#       LADDER_RECORDS below that bounds it. The two refusals that stop before the count is
#       taken are the absent log and the line of another shape, each of which ends the verb
#       where it stands.
#       It takes no argument, as `shorten-ladder` does.
#       It refuses the job where the log is absent or empty, which is what a stand-in that
#       never reached PATH leaves behind, so that refusal is also the reading that says the
#       appended bound applied on the runner at all; where no fetch carried
#       CI_TOOLS_PROOF_FULL_LADDER, since one case must keep the pinned ladder; where more than
#       one name did, since the rig keeps one measurement and not several; where the longest
#       fetch carrying that name is outside LADDER_FLOOR..LADDER_CEILING below, the floor being
#       the reading that catches a kept case which stopped driving the real ladder; where more
#       fetches carried that variable, whatever name they carried it under, than the
#       LADDER_RECORDS below, the reading that catches it set wider than the one step's `env:`
#       which carries it, since at job scope and at workflow scope the stand-in shortens
#       nothing; where a fetch the stand-in left on the pinned ladder spent one and then
#       failed, which is a fault shape its rule did not recognise; where a fetch the stand-in
#       shortened exited 0, which is a fetch this job means to succeed and not one it arranged
#       to fail; and where a line of the log is not the five-field line the stand-in writes.
#       A fetch that spent a ladder and served the bytes is the ladder doing the work it is
#       kept for, and it is printed as a warning that refuses nothing.
#
# HOW `--expect-source` IS READ.
# A composite action writes its refusal to the job's log, and no later step can read that log, so
# the source a refusal named is read from what the origin served instead.
# Each route is one source: /github/ is `github-release`, /ghcr/ is `ghcr-mirror`, /npm/ is
# `npm`, and /url/ is `pypi` and `pulumi-releases` alike — one seam for both, and the row says
# which of the two words its own address is written in — and the origin's log says which route
# served this case's row.
# A case whose primary is the faulted real host asks the origin's github route for nothing, so
# `--expect-source github-release` is read as the same statement the other way round: the origin
# served this row on none of its other three routes, so the bytes the step refused came from the
# primary path and from nowhere else.
# The pulumi-cli row is the one row whose second source is a release asset, and a case of it the
# origin did serve on /github/ is read as that route having served, which is the reading the
# same branch makes.
# A Bun already installed on the runner reads the same way, though setup-bun names it
# `existing-install` rather than `github-release`: this rig reads the route and not the word,
# so the word a refusal printed is read in the job's own log and not by this script.
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
curl_log="$work/curl.log"
stand_in_bin="$work/bin"
stand_in="$stand_in_bin/curl"

# The rows a case installs, which are the rows `stage` fetches the correct bytes of.
# The two other bun rows are the same binary for another runner (.github/ci-tools.yaml says so),
# and no case installs them, so nothing would serve their payload.
STAGED_ROWS="crane changie mise jj bun-linux-x64 elan uv pulumi-docker pulumi-cli"

# The hosts both jobs fault, written here and in no step of .github/workflows/ci.yaml:
# github.com, where a release download starts, and the two githubusercontent.com hosts it ends
# at, rather than the whole githubusercontent.com domain.
# Measured on 2026-11-08: a release asset redirects to release-assets.githubusercontent.com and
# a ghcr.io blob redirects to pkg-containers.githubusercontent.com, so faulting the domain takes
# the second source down with the first and the proof reads nothing (run 42484733737, where
# crane's copy answered curl exit 22 behind that fault). objects.githubusercontent.com is the
# host release downloads ended at before, and the issue's own check names it.
# A release host that moves again is one edit here. What catches the move is the second-source
# cases of the `tool-fallback` job: each of them fails when the origin served nothing for its
# row, which is what a primary that answered after all leaves behind.
FAULT_HOSTS="github.com,release-assets.githubusercontent.com,objects.githubusercontent.com"

# The address .github/workflows/ci.yaml's `tool-fallback` job writes at every seam and every
# dead proxy that stands for a second source that cannot be reached: the CI_TOOLS_GHCR_BASE,
# CI_TOOLS_NPM_BASE and CI_TOOLS_URL_BASE of its controls, and the http_proxy and https_proxy
# of its base-pulumi control, each of them `http://127.0.0.1:1`.
# The rig holds a copy of the address the job writes, so a fetch the job meant to fail is
# recognised here whichever of those seams carried it. Where the two copies drift apart, the
# fetch keeps the pinned ladder and fails, and `ladder-report` refuses the job for it.
DEAD_ADDRESS="127.0.0.1:1"

# The window in seconds the one case that keeps the pinned ladder must measure inside.
# The pinned flags are `--retry 7 --retry-all-errors --retry-max-time 60` against a source that
# refuses at once, whose back-off is 1+2+4+8+16+32 = 63 s; job 129874475871, whose stand-in
# measured every fetch of the job, read that case at 63 s. The floor is those 63 s less the
# headroom a slow runner needs, and the ceiling is the worst the bound allows: a minute in
# which a new attempt may still start, and one last attempt of the row's own `max_time` of
# 60 s, with headroom on top.
# Job 129743677408 ran without the stand-in, so it measured no fetch at all and can say
# nothing about how long a fetch takes. Job 129874475871 did measure them: of its 36 fetches
# that kept the pinned ladder, the npm fetch of the 107 MB Bun tarball reads 0 s, the PyPI
# wheel 0 s and the package blobs 0 s or 1 s, and the only one above 1 s is the kept case at
# 63 s. A reading of about 30 s for that tarball is a step duration and no fetch: setup-bun
# measured 38 s, 40 s and 34 s in that job, and that step is the Node action and the unpack
# around the fetch.
# So the floor stands about 44 s above the largest honest fetch measured, and not about 15 s.
# A fetch that merely rode out a flap of its source reads as the warning `ladder-report` prints
# below and not as a refusal.
LADDER_FLOOR=45
LADDER_CEILING=150

# How many fetches of one run may carry CI_TOOLS_PROOF_FULL_LADDER, read by `ladder-report`
# over the log the stand-in wrote.
# That variable is set at exactly one step's `env:` in .github/workflows/ci.yaml, the head's
# install-crane step, so one install keeps the pinned ladder. That one install makes three
# fetches through the stand-in: the release asset behind the fault, which
# ci_tools__fetch_release reads (.github/scripts/ci-tools.sh:1006), and then the pull token and
# the blob of the copy the `crane` row names as its second source, which ci_tools__fetch_ghcr
# asks for one after the other (.github/scripts/ci-tools.sh:770, 775). Merge_group run
# 42975857631, job 130374882879 (`tool-fallback`, success, head 9300f28b1c), read those three
# as records 18 to 20 of its listing, at 63 s, 0 s and 1 s.
# A retry adds no record, because ci_tools_curl is one curl process with `--retry 7` and the
# stand-in writes one line for each time it is called; a redirect is followed inside that same
# process, so it adds none either. Of the five second-source words only `ghcr-mirror` costs two
# requests, so another second source for that row can only lower this count, and a primary that
# answers after all lowers it to one.
# What moves this number is a change in this tree: a ghcr path that reads a manifest before it
# asks for the blob, or a second attempt inside ci_tools_fetch. The ladder-report step prints
# the count it read beside this bound, so the reading that moves it is in the log of the job
# that holds it.
LADDER_RECORDS=3

# RFC 2606's reserved suffix, which `probe` hangs on the faulted host to make its control.
# Nobody owns a name under it and no resolver answers for one, so only the proxy can answer a
# request for it, and a request made around the proxy reads as nothing at all.
RESERVED_SUFFIX=".invalid"

# The bound in seconds each of the probe's two requests gets.
# Both answers are the proxy's own and immediate, so this is spent only where nothing answers at
# all, which is the reading the probe refuses the job for.
PROBE_BOUND=15

# The seconds no timed install may reach, read by `report` over the mark `corrupt` wrote. The
# twenty-four installs whose control accepts a failure are the timed ones; the head's own eight,
# which no control lets fail, are not checked against it.
# Every install of a case carries `continue-on-error: true` and `timeout-minutes: 11`, and a
# step the runner ends at that bound leaves `outcome=failure` behind it, which is the outcome
# `report` holds a case to, so a hung install would otherwise pass as a refusal.
# A step held to its bound takes about 660 to 672 s. The longest single install measured over
# 2422 executions between 2026-11-09T19:38Z and 2026-11-10T10:51Z, before the ladders stopped
# sleeping (dispatch://INFRA-2621), is 326 s (`install-jj` on slow runs), so this leaves 274 s
# over the slowest healthy install there has been and at least 60 s under the fastest a held
# step can be, and it reads no ladder's length. Since the ladders stopped sleeping, the slowest
# timed install is far shorter: 42 s, the Bun second-source case, in job 130289341628.
# It is the one wall-clock bound `report` and the control step judge an install by, and this line
# is the one place it is written. Two other bounds are not it: `timeout-minutes: 11`, which the
# runner enforces on the step rather than reading after it, and `PROBE_BOUND` above, which
# bounds the probe's two requests. The `tool-fallback` job holds its eight
# installs with nowhere to go to the same seconds, and its control step reads them from here
# through the `install-bound` verb rather than writing a second number that nothing keeps equal
# to this one.
INSTALL_BOUND=600

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
"""The four sources .github/scripts/ci-tools.sh reads, served from one local origin.

Routes, each the one a seam of that helper reaches:

  GET /github/<any path>/<file>                  the payload of that file name
  GET /ghcr/token?service=...&scope=...          {"token":"proof"}
  GET /ghcr/v2/<path>/blobs/sha256:<digest>      the payload of the row that digest pins
  GET /npm/<package>/-/<file>.tgz                the payload of that file name
  GET /url/<the path of the row's own address>   the payload of that file name

Every request is decided by the control file, read again each time: a row named in it is served
with one byte changed, and a row not named in it is served whole. Each request is logged
with the row it belongs to and whether it was corrupted.
"""

import http.server
import json
import os
import re
import socketserver
import struct
import sys
import zipfile
from pathlib import Path

import yaml

manifest, payload, control, logfile, portfile = (Path(argument) for argument in sys.argv[1:6])

document = yaml.safe_load(manifest.read_text()) or {}
ROWS = document.get("tools") or []
GHCR_BLOB = re.compile(r"^/ghcr/v2/(?P<path>.+)/blobs/sha256:(?P<digest>[0-9a-f]{64})$")
NPM_TARBALL = re.compile(r"^/npm/(?P<package>.+)/-/(?P<file>[^/]+\.tgz)$")
# The size of one read of a payload, which is the size of one write to the socket.
CHUNK = 64 * 1024


# The source words each route is reached by, which is what decides the row a request is about:
# the seam a case sets points the helper here, and the word the row writes on that side says
# which of its two addresses it then asks for.
# This program is started with the manifest and nothing else, so these two sets are its own
# and not the shell's `route_of`. The places of this tree that decide a source word are listed
# at ci_tools__source_fetch in .github/scripts/ci-tools.sh, these two sets among them, and a
# new word moves all of them together.
GITHUB_WORDS = {"github-release"}
URL_WORDS = {"pypi", "pulumi-releases"}


def base_name(url):
    """The file name at the end of that address, and "" where the row wrote no address."""
    return str(url or "").rsplit("/", 1)[-1]


def served_names(row, words):
    """The names this row's bytes are served under at the addresses written in one of those
    words: its `primary` where `primary_source` is one of them, and its `second_url` where
    `second_source` is.
    Both sides are read, because a word is no longer a side: `github-release` is the pulumi-cli
    row's second source, and the two pulumi rows write one file name at both of their
    addresses, which is one name on two routes."""
    names = set()
    if row.get("primary_source") in words:
        names.add(base_name(row.get("primary")))
    if row.get("second_source") in words:
        names.add(base_name(row.get("second_url")))
    return names - {""}


def github_names(row):
    """The names the /github/ route serves this row under, which is the CI_TOOLS_GITHUB_BASE
    seam's: whichever of its two addresses is written `github-release`, and its `mirror_from`,
    the release asset .github/scripts/mirror-ci-tools.sh fills a ghcr-mirror row's copy from."""
    return served_names(row, GITHUB_WORDS) | ({base_name(row.get("mirror_from"))} - {""})


def url_names(row):
    """The names the /url/ route serves this row under, which is the CI_TOOLS_URL_BASE seam's:
    whichever of its two addresses is written `pypi` or `pulumi-releases`. That seam keeps a
    URL's path and replaces its scheme and host, so what arrives here is the file name at the
    end of that path."""
    return served_names(row, URL_WORDS)


def resolve(path):
    """The row this request is about and the payload file it is served from, or (None, None)."""
    for prefix, names_of in (("/github/", github_names), ("/url/", url_names)):
        if path.startswith(prefix):
            name = path.split("?", 1)[0].rsplit("/", 1)[-1]
            for row in ROWS:
                if name in names_of(row):
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
    """The rows this request must serve with one byte changed, read again for every
    request: one origin serves every case of the job, and a case says what it wants here."""
    if not control.exists():
        return set()
    return set(control.read_text().split())


def member_data_end(file, member):
    """Where that member's own stored bytes end inside that zip, or None where the archive
    holds no such member. The local header is read for its two length fields, because the extra
    field a writer puts there is not the one the central directory carries.
    The archive is read from the open file rather than from a body in memory, because a payload
    is never held whole here."""
    try:
        with zipfile.ZipFile(file) as held:
            info = held.getinfo(member)
        file.seek(info.header_offset)
        head = file.read(30)
        name_length, extra_length = struct.unpack("<HH", head[26:30])
        return info.header_offset + 30 + name_length + extra_length + info.compress_size
    except (KeyError, OSError, struct.error, zipfile.BadZipFile):
        return None


def corrupted_offset(row, name, file, size):
    """The one byte a corrupted case changes, as an offset into the payload file: the smallest
    edit that moves the sha256 of what the reader of these bytes is held to, and None for a file
    of no bytes, which has no byte to change.
    For most names it is the last byte of the file, which is what the row's digest is of, and
    for a gzipped tarball it is part of the framing `tar -xz` reads, so the member read refuses
    it before any digest does.
    A zip is the exception, measured on 2026-11-09 against this rig's own wheel: its last byte
    is inside the end-of-directory record, which a reader taking a member out never looks at,
    so a wheel changed there hands the pinned binary back whole and the case installs it. The
    byte changed is the last one of the member's own stored data instead, which is what the
    row's `digest` answers for, and python3's zipfile then hands over no member at all, because
    the CRC the archive carries for it no longer matches what it holds.
    """
    if size == 0:
        return None
    if name.endswith((".whl", ".zip")):
        end = member_data_end(file, str(row.get("second_member") or ""))
        if end is not None:
            return end - 1
    return size - 1


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
        corrupt = row["name"] in corrupted_rows()
        with open(file, "rb") as handle:
            size = os.fstat(handle.fileno()).st_size
            flip = corrupted_offset(row, name, handle, size) if corrupt else None
            handle.seek(0)
            self.serve(handle, size, flip, row["name"], "yes" if corrupt else "no")

    def answer(self, status, body, row, corrupt):
        """One of the small answers this program holds in memory: the ghcr token and the two
        refusals."""
        self.log_served(status, len(body), row, corrupt)
        self.send_headers(status, len(body))
        self.wfile.write(body)

    def serve(self, file, size, flip, row, corrupt):
        """That payload, copied from the file to the socket a chunk at a time and never held
        whole, with the one byte a corrupted case changes flipped in the chunk that carries it.
        It is read this way because the largest payload this manifest pins is 119816773 bytes:
        one request for it held those bytes twice, the body read whole and the corrupting
        path's copy of it, while the fault proxy in front of this origin relays 64 KiB at a
        time."""
        self.log_served(200, size, row, corrupt)
        self.send_headers(200, size)
        sent = 0
        while sent < size:
            chunk = file.read(min(CHUNK, size - sent))
            if not chunk:
                break
            if flip is not None and sent <= flip < sent + len(chunk):
                chunk = bytearray(chunk)
                chunk[flip - sent] ^= 0x01
            self.wfile.write(chunk)
            sent += len(chunk)

    def log_served(self, status, count, row, corrupt):
        with open(logfile, "a") as log:
            log.write(f"GET {self.path} row={row} corrupt={corrupt} status={status} bytes={count}\n")

    def send_headers(self, status, count):
        self.send_response(status)
        self.send_header("Content-Length", str(count))
        self.send_header("Content-Type", "application/octet-stream")
        self.end_headers()

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
      echo "url=$base/url"
    } >> "$GITHUB_OUTPUT"
  fi
  note "the origin serves $base/github, $base/ghcr, $base/npm and $base/url, and logs to $origin_log"
}

# ---------------------------------------------------------------- stage

# payload_names <row>: every file name the origin serves this row's bytes under, one per line.
# A row writes a source word on each of its two sides, and the word says where those bytes are
# served and so under what name: a release asset and a service's own address under the file name
# at the end of that address, a ghcr blob under the digest its URL names, and an npm package
# under the tarball name the registry serves it as.
# The second source's word is read here and the primary's is not, because a row's `primary` is
# one URL whichever of the words it is written in, and the name is the end of it either way.
payload_names() {
  local row=$1 primary second route mirror_from package version second_url
  primary=$(ci_tools_field "$row" primary)
  # The word and the refusal of one no fetch of that side answers to are both the helper's:
  # ci_tools_source_name reads the row's `second_source` through the same list the fetch reads
  # it through and refuses it in its own `::error::ci-tools:` line, which names the five. The
  # refusal is made here rather than inside the pipeline below, where an exit would end a
  # subshell alone, and `stage` reads these names into a variable for the same reason.
  second=$(ci_tools_source_name "$row" second) ||
    die "$row: the helper reads no second source for this row, so the origin cannot serve one"
  # Which fields the names come from is a property of the route and not of the word, and
  # route_of below answers that for this shell: /github/ and /url/ alike mean the address the
  # row writes out in `second_url`.
  route=$(route_of "$second" "payload_names")
  {
    [ -z "$primary" ] || printf '%s\n' "${primary##*/}"
    case $route in
      /ghcr/)
        mirror_from=$(ci_tools_field "$row" mirror_from)
        [ -z "$mirror_from" ] || printf '%s\n' "${mirror_from##*/}"
        printf 'sha256:%s\n' "$(ci_tools_digest "$row")"
        ;;
      /npm/)
        package=$(ci_tools_field "$row" npm_package)
        version=$(ci_tools_field "$row" version)
        printf '%s-%s.tgz\n' "${package##*/}" "$version"
        ;;
      *)
        second_url=$(ci_tools_field "$row" second_url)
        [ -z "$second_url" ] || printf '%s\n' "${second_url##*/}"
        ;;
    esac
    # A row whose `primary` is its `mirror_from`, and a row whose two addresses end in one file
    # name — the two pulumi rows, which Pulumi serves byte for byte at both — names one file
    # twice, and the origin serves one file: the name is printed once, in the order the names
    # were written.
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

# pack_zip <row> <binary> <archive>: the binary as PyPI serves it, a zip whose one member is the
# row's `second_member`, so the helper's own zipfile read of it takes out the same bytes it takes
# out of the wheel. python3's zipfile writes it, because python3's zipfile is what reads it.
# The member is stored rather than deflated, for the reason pack_npm compresses at gzip's
# fastest level: the archive is served over 127.0.0.1 to a reader that takes either, and the
# seconds are on the proof job's critical path.
pack_zip() {
  local row=$1 binary=$2 archive=$3 member
  member=$(ci_tools_field "$row" second_member)
  rm -f -- "$archive"
  PYTHONDONTWRITEBYTECODE=1 python3 -c '
import sys
import zipfile

archive, member, binary = sys.argv[1:4]
with zipfile.ZipFile(archive, "w", zipfile.ZIP_STORED) as held:
    held.write(binary, member)
' "$archive" "$member" "$binary"
}

# packed_back <row> <archive> <member> <what that archive is called>: the round trip of what
# this rig packed — that member taken back out and held to the row's own pinned digest. The
# origin serves this archive and an install reads the member out of it, so a pack that does
# not give the pinned bytes back is this rig's own defect and must not read as the install's,
# which is why it dies rather than returning.
# The member comes out with the helper's own ci_tools_extract, the one place this repository
# takes a member out of an archive, and the archive's own path stands in for the address it
# would have come from: ci_tools_extract reads the shape off that name, and the names this rig
# packs under are the names the origin serves, so a `.tgz` goes to `tar -xzO` and a `.whl` to
# python3's zipfile exactly as an install's would.
# What comes out is held to the row's digest with ci_tools_verify, as stage_member below holds
# the member it takes out: ci_tools__hold behind that call is this repository's one digest
# comparison, so this rig writes no second reading of what a match is.
packed_back() {
  local row=$1 archive=$2 member=$3 what=$4 back report status=0
  back="$work/round-trip-$row"
  rm -f -- "$back"
  report=$(ci_tools_extract "$archive" "$member" "$back" "$archive") || status=$?
  [ "$status" -eq 0 ] || die "$row: the $what this rig packed did not give $member back: $report"
  # The label is this rig, because this rig packed the archive the member came out of. No
  # service served these bytes, so naming one — the row's own second source, say — would put
  # a service that served nothing into the line, and a mismatch would read as that service
  # serving bytes this row does not pin.
  ci_tools_verify "$row" "$back" "the $what this rig packed" || status=$?
  rm -f -- "$back"
  [ "$status" -eq 0 ] ||
    die "$row: the $what this rig packed does not give the bytes this row pins back"
}

# stage_member <row> <archive> <member> <file>: the row's own pinned bytes out of the archive
# its primary served, which ci_tools_fetch_primary has already staged and held to the row's
# `primary_digest`.
# The member is taken out with the helper's own ci_tools_extract, so this rig reads an archive
# the one way an install does, and it is held to the row's `digest` with ci_tools_verify, which
# is the check ci_tools_fetch makes of the member it fetches.
# Prints what happened and returns non-zero where either refuses, so `stage` appends the row to
# its failures and goes on.
stage_member() {
  local row=$1 archive=$2 member=$3 file=$4 report status=0
  report=$(ci_tools_extract "$archive" "$member" "$file" "$(ci_tools_field "$row" primary)") || status=$?
  if [ "$status" -ne 0 ]; then
    echo "::error::ci-tools-proof: $row: $report" >&2
    return 1
  fi
  ci_tools_verify "$row" "$file" "$(ci_tools_source_name "$row" primary)"
}

stage() {
  [ "$#" -eq 0 ] || die "stage takes no argument, and this call gave $#"
  mkdir -p "$payload"
  local row file name names member primary_name failures=""
  for row in $STAGED_ROWS; do
    note "staging $row"
    # The name the row's primary URL serves its bytes under, and the member of what it serves
    # the row's own `digest` is of, which only a row whose primary serves an archive around the
    # pinned bytes names.
    primary_name=$(ci_tools_field "$row" primary)
    primary_name=${primary_name##*/}
    member=$(ci_tools_field "$row" primary_member)
    # Every name the origin serves this row under, read before the loop below rather than from
    # a process substitution feeding it: a `die` inside that subshell ends the subshell alone,
    # and this script would stage nothing for the row and walk on. Read into a variable, the
    # refusal is this script's own exit under its `set -euo pipefail`.
    names=$(payload_names "$row")
    file="$work/staged-$row"
    rm -f "$file"
    if [ -n "$member" ]; then
      # The bytes the PRIMARY URL serves, staged under the name that URL ends in and held to
      # the row's `primary_digest`, and the row's own pinned bytes taken out of that staged
      # archive: one download for the two files, and a case whose primary meets the origin
      # reads exactly these bytes. Fetching the row's bytes with ci_tools_fetch and then the
      # archive with ci_tools_fetch_primary downloaded uv's 16682639-byte archive twice, and
      # uv is the one row that names a `primary_member`.
      # Both digests are still read: ci_tools_fetch_primary holds the archive to
      # `primary_digest` before it places it, and stage_member holds the member to the row's
      # own `digest`.
      ci_tools_fetch_primary "$row" "$payload/$primary_name" &&
        stage_member "$row" "$payload/$primary_name" "$member" "$file" ||
        {
          failures="$failures $row"
          continue
        }
    else
      # The row's own path, which the helper decides: a row with a primary is tried there
      # first, and a row whose primary bytes a third party fetches falls straight through to
      # the second source. What it leaves is the bytes the row's `digest` pins.
      ci_tools_fetch "$row" "$file" || {
        failures="$failures $row"
        continue
      }
    fi
    while read -r name; do
      # The primary's own name is the archive staged above, which no destination of
      # ci_tools_fetch ever holds and which this loop therefore neither fetches nor packs.
      if [ -z "$member" ] || [ "$name" != "$primary_name" ]; then
        case $name in
          *.tgz)
            pack_npm "$row" "$file" "$payload/$name"
            packed_back "$row" "$payload/$name" "$(ci_tools_field "$row" npm_member)" tarball
            ;;
          *.whl | *.zip)
            pack_zip "$row" "$file" "$payload/$name"
            packed_back "$row" "$payload/$name" "$(ci_tools_field "$row" second_member)" wheel
            ;;
          # A hard link and not a copy, for pack_npm's reason: both files are under this rig's
          # own directory and no payload file is ever written to, so the staged file and every
          # name the origin serves it under are one set of bytes — the CLI's 119816773 and the
          # plugin's 24184600 among them — rather than a copy each.
          *) cp -fl -- "$file" "$payload/$name" ;;
        esac
      fi
      describe "$payload/$name"
    done <<<"$names"
    rm -f "$file"
  done
  [ -z "$failures" ] ||
    die "no correct bytes for$failures, so the origin cannot stand in for their source; the copy of every ghcr-mirror row is filled by .github/scripts/mirror-ci-tools.sh in the ci-images job this job needs"
  note "the payload the origin serves is $payload"
}

# ---------------------------------------------------------------- shorten-ladder

# write_stand_in <the real curl>: the stand-in curl, in the directory the whole rig writes to.
# The real curl is resolved by the caller and written in here as an absolute path, because the
# stand-in is first on PATH and a `command -v curl` of its own would find itself.
# The four values this rig decides are written in at the top of it for the same reason: the
# stand-in takes the hosts and the address from this one file, and nobody writes a second copy
# of them by hand.
# The four values go through `%q`, so each reads back as the one word it is, whatever it holds;
# written between double quotes instead, a value carrying a dollar or a backtick would be
# expanded each time the stand-in runs. Both heredocs are quoted, so the body is plain shell
# with no escape in it.
write_stand_in() {
  local real_curl=$1
  {
    cat <<'SH'
#!/usr/bin/env bash
# Written by the shorten-ladder verb of .github/scripts/ci-tools-proof.sh, which states what
# this claims and what it leaves untouched.
# It is deliberately not the two-line stand-in .github/scripts/ci-tools.test.sh and
# .github/scripts/mirror-ci-tools.test.sh each write. Those two shorten every fetch, because no
# case of theirs needs a real ladder. This one shortens only a fetch the job arranged to fail,
# so that the pinned ladder still rides out a flap of npm, PyPI, get.pulumi.com or the local
# origin, each of which this job reaches through the fault proxy and none of which it faults.
#
# errexit is left off on purpose: this script must write its line and then exit with the status
# the real curl exited with, whatever that status is.
set -uo pipefail

SH
    printf '%s=%q\n' \
      real_curl "$real_curl" \
      curl_log "$curl_log" \
      fault_hosts "$FAULT_HOSTS" \
      dead_address "$DEAD_ADDRESS"
    cat <<'SH'

# The request is the first argument that starts with http:// or https://, and its authority is
# the text up to the first /, ? or #, with any user@ before that removed.
# Of the arguments, only the host of that URL reaches the log: never its path, query, fragment
# or user, and no other argument. This helper is handed credentials as arguments — -u for a
# ghcr token, an Authorization header for a blob — and .github/scripts/ci-tools.sh passes them
# that way exactly so that they reach no log line.
url=""
for argument in "$@"; do
  case $argument in
    http://* | https://*)
      url=$argument
      break
      ;;
  esac
done
authority=${url#*://}
authority=${authority%%[/?#]*}
authority=${authority##*@}
# A bracketed IPv6 literal carries colons inside its brackets, so a cut at the first colon
# would leave `[` alone in the host field. The first arm keeps such a literal whole, with its
# brackets, and drops the port after it.
case $authority in
  "["*"]"*) host=${authority%%]*}] ;;
  *) host=${authority%%:*} ;;
esac
# An authority that is only a port, as in http://:443/ or http://u@:80/, leaves the host
# empty, and the ladder-report verb refuses a line that holds an empty host=, so an empty host
# is written as none.
[ -n "$host" ] || host=none

# faulted <host>: the fault proxy's own rule for a host it holds, which is exact or dot-suffix
# (.github/scripts/ci-fault-proxy.py lines 64 to 67 hold that same rule): github.com holds
# github.com and x.github.com, and it does not hold github.com.invalid, the reserved control
# name the probe verb asks for.
faulted() {
  local host=${1,,} held
  local IFS=,
  for held in $fault_hosts; do
    case $host in
      "$held" | *".$held") return 0 ;;
    esac
  done
  return 1
}

# dead_proxy: whether a proxy variable carries this request to the dead address, which is the
# shape the base-pulumi control of the job writes where the others write a seam.
dead_proxy() {
  local value
  for value in "${http_proxy:-}" "${https_proxy:-}" "${HTTP_PROXY:-}" "${HTTPS_PROXY:-}"; do
    value=${value%/}
    [ "${value#*://}" != "$dead_address" ] || return 0
  done
  return 1
}

# CI_TOOLS_PROOF_FULL_LADDER is the way back to the pinned ladder, and the one case that
# measures that ladder is the case that sets it.
decision=full
if [ -z "${CI_TOOLS_PROOF_FULL_LADDER:-}" ] &&
  { faulted "$host" || [ "$authority" = "$dead_address" ] || dead_proxy; }; then
  decision=shortened
fi

started=$(printf '%(%s)T' -1)
status=0
if [ "$decision" = shortened ]; then
  # curl reads the last word on --retry-max-time, so this bound wins over the helper's own
  # --retry-max-time 60. It is appended after the arguments and changes nothing else of them.
  "$real_curl" "$@" --retry-max-time 1 || status=$?
else
  "$real_curl" "$@" || status=$?
fi
ended=$(printf '%(%s)T' -1)
# Both of those times are readings of the wall clock. A clock stepped back during the fetch
# would make this difference negative, and the ladder-report verb refuses a line that holds a
# negative seconds=, so the difference is held at zero or above here.
elapsed=$((ended - started))
[ "$elapsed" -ge 0 ] || elapsed=0

# The one line per fetch the ladder-report verb reads, and the whole of what this script writes
# of its own: curl's stdout and stderr pass through untouched, because the probe verb parses
# what curl printed.
printf 'seconds=%s decision=%s host=%s exit=%s full-ladder=%s\n' \
  "$elapsed" "$decision" "$host" "$status" \
  "${CI_TOOLS_PROOF_FULL_LADDER:-none}" >> "$curl_log"
exit "$status"
SH
  } > "$stand_in"
  chmod 0755 "$stand_in"
}

# shorten_ladder: the stand-in curl of the paragraph above, on this job's PATH.
shorten_ladder() {
  [ "$#" -eq 0 ] || die "shorten-ladder takes no argument, and this call gave $#"
  local real_curl version search
  # The real curl is resolved with this rig's own directory taken off PATH, and written into
  # the stand-in as an absolute path. A second run of this verb — a job that runs it again, or
  # a shell that already has that directory on PATH — would otherwise find the stand-in the
  # first run left there and write a script that runs itself until the shell gives up.
  search=":$PATH:"
  search=${search//":$stand_in_bin:"/:}
  search=${search#:}
  search=${search%:}
  real_curl=$(PATH="$search" command -v curl) ||
    die "shorten-ladder: no curl is on this job's PATH, so there is none for the stand-in to run"
  mkdir -p "$stand_in_bin"
  : > "$curl_log"
  write_stand_in "$real_curl"
  # Off a runner there is no $GITHUB_PATH, and the line is printed and written nowhere, which
  # is what fault_env does with the proxy variables.
  [ -z "${GITHUB_PATH:-}" ] || printf '%s\n' "$stand_in_bin" >> "$GITHUB_PATH"
  version=$("$real_curl" --version)
  note "the stand-in curl every step after this one runs, which shortens only a fetch this job arranged to fail"
  printf '  %s\n' "$stand_in"
  note "the line it adds to this job's PATH:"
  printf '  %s\n' "$stand_in_bin"
  echo "  the curl it runs is $real_curl: ${version%%$'\n'*}"
  echo "  what it measures goes to $curl_log, which the ladder-report verb reads"
}

# ---------------------------------------------------------------- start-fault

# fault_env <proxy>: every variable a client reads before it decides where to send a request,
# written to $GITHUB_ENV and printed as it was written.
# The four spellings are the whole of what a client reads, and the two bypass lists are emptied
# with a proxy set because either one holding a star sends every host past it. A cleared proxy
# leaves those two alone: with nothing to go around, nothing reads them.
# It goes through $GITHUB_ENV rather than a step `env:` map because GitHub refuses a map that
# holds two spellings of one name, and the workflow would then not start at all.
# Off a runner there is no $GITHUB_ENV, and the lines are printed and written nowhere.
fault_env() {
  local proxy=$1 line
  local -a lines=(
    "http_proxy=$proxy" "https_proxy=$proxy" "HTTP_PROXY=$proxy" "HTTPS_PROXY=$proxy"
  )
  [ -z "$proxy" ] || lines+=("no_proxy=" "NO_PROXY=")
  for line in "${lines[@]}"; do
    [ -z "${GITHUB_ENV:-}" ] || printf '%s\n' "$line" >> "$GITHUB_ENV"
    printf '  %s\n' "$line"
  done
}

# start_fault: the proxy over FAULT_HOSTS, and every variable that routes this job's later steps
# through it. The address it bound goes to the fault state file, the one thing the verbs after it
# need of it: `probe` makes its two requests through that proxy and `installed-bun
# --expect-refused` reads the log of the same one.
start_fault() {
  [ "$#" -eq 0 ] || die "start-fault takes no argument, and this call gave $#"
  mkdir -p "$work"
  local port_file="$work/fault.port"
  rm -f "$port_file" "$fault_log"
  : > "$fault_log"
  python3 "$root/.github/scripts/ci-fault-proxy.py" --mode fail --hosts "$FAULT_HOSTS" \
    --port 0 --port-file "$port_file" > "$fault_log" 2>&1 &
  wait_for_file "$port_file" "the fault proxy"
  local proxy
  proxy="http://127.0.0.1:$(cat "$port_file")"
  echo "proxy=$proxy" > "$fault_state"
  note "the fault proxy is $proxy, answering HTTP 504 for $FAULT_HOSTS and passing every other host on"
  note "every later step of this job goes through it:"
  fault_env "$proxy"
}

# ---------------------------------------------------------------- stop-fault

# stop_fault: the four proxy variables emptied, which is where a job stops going through the
# fault. The proxy process itself ends with the job; what must end before that is every later
# step reading it, since the ci-ok-report step each job ends with uploads an artifact and a
# proxy about to go would carry that upload.
stop_fault() {
  [ "$#" -eq 0 ] || die "stop-fault takes no argument, and this call gave $#"
  note "no later step of this job goes through the fault proxy:"
  fault_env ""
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
  # Where the proxy's log stands now, which is where this job's own installs begin: a later
  # `installed-bun --expect-refused` reads the refusals after this mark, so it reads an
  # install's 504 and never this probe's, whose request names the same host.
  printf 'probed=%s\n' "$(wc -l < "$fault_log")" >> "$fault_state"
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

# ---------------------------------------------------------------- refusals

# proxy_refusals <host>: the fault proxy's own lines refusing a request to that host since the
# probe marked its log, one per line and none at all where it refused nothing.
# The proxy writes `ci-fault-proxy: 504 <verb> <target>`, where the target is `<host>:<port>`
# for the CONNECT an https request makes and the whole URL for a plain one, so the host is read
# off the target and compared rather than searched for in the line.
# The probe's mark is where the reading starts, because the probe's own request names the same
# host: a count from the top of the log would read that one request as an install's refusal.
proxy_refusals() {
  local host=$1 probed
  [ -f "$fault_state" ] ||
    die "a refusal is a line of the fault proxy's own log, and this job started no fault proxy"
  probed=$(sed -n 's/^probed=//p' "$fault_state" | tail -n 1)
  [ -n "$probed" ] ||
    die "the refusals are read after the probe marked the proxy's log, and this job ran no probe under this fault"
  awk -v skip="$probed" -v host="$host" '
    NR > skip && $2 == "504" {
      target = $4
      sub(/^https?:\/\//, "", target)
      sub(/[:\/].*$/, "", target)
      if (target == host) print
    }' "$fault_log"
}

# refusals_read <host>: that reading printed — how many there were and which lines they are —
# and non-zero where there were none.
# It is the one implementation of the reading: the `refusals` verb is this and nothing else, and
# `installed-bun --expect-refused` takes the same reading of the same log, so the two cannot
# count a refusal differently or print it in two shapes.
# What an empty reading means is left to the caller, which is the one thing the two do not
# share: for a bun it says nothing proves the second source served it, and for the verb it says
# no install of the job met the fault at all.
refusals_read() {
  local host=$1 lines count=0
  lines=$(proxy_refusals "$host")
  [ -z "$lines" ] || count=$(wc -l <<<"$lines")
  echo "  what the fault proxy refused for $host since the probe: $count"
  if [ -z "$lines" ]; then
    echo "    nothing at all"
    return 1
  fi
  sed 's/^/    /' <<<"$lines"
}

# refusals <host>: that count and those lines, printed beside the readings of the installs this
# job made — the 504s that sent each of them to its second source — and a refusal of the job
# where the proxy refused nothing.
refusals() {
  [ "$#" -eq 1 ] || die "refusals takes the faulted host and nothing else, and this call gave $#"
  note "what the fault proxy refused for $1 since the probe, beside what this job installed"
  refusals_read "$1" ||
    die "the fault proxy refused no request to $1 after the probe, so no install read above met the fault at all: each of them would read exactly as it does on a runner where $1 answered, and that it did not is the whole statement of this job"
}

# ---------------------------------------------------------------- installed-bun

# installed_bun <row> [--expect-refused <host>]: the bun on this job's PATH read against that
# row, which is how a job on a runner architecture this repository proves nothing else on says
# which Bun it got.
# It prints the machine, what `bun --version` answers, the path `command -v bun` gives, the
# row's own version and digest, and what ci_tools_verify reads that file as, and it refuses the
# job where the version is not the one .bun-version pins or the binary is not that row's bytes.
# The digest reading is ci_tools_verify's and not one of this rig's own: that function is this
# repository's one digest check, it writes the `ci-tools:` line that names what was held to the
# row, and a second comparison here is a second rule that can disagree with it.
#
# The row a reader wants is the one .github/actions/setup-bun chose from `uname -m`, and no step
# can read that out of the action: a composite action's step outputs are its own. It is read
# from the binary instead. That action held what it installed to the row it chose, this verb
# holds the same file to the row named here, and one file has one sha256, so two passing
# readings name one row — the three bun rows of .github/ci-tools.yaml do not share the aarch64
# row's digest.
#
# `--expect-refused <host>` is the other half of a reading taken under the fault: which source
# served those bytes. The version and the digest cannot say it, because a row's release asset
# and its npm package are one binary by design and read the same whichever answered. The fault
# proxy's own log can: a 504 for that host, logged after the probe marked the log, is the
# refusal that sent this install to its second source. It is the reading the `refusals` verb
# prints, taken through the one function the two go through.
#
# The bunx beside that bun is read as well, by the function the `installed-bunx` verb is made
# of: the digest above holds the bun alone, and the pair is what a later step runs.
installed_bun() {
  [ "$#" -ge 1 ] ||
    die "installed-bun takes the row this runner's bun must be, and this call gave none"
  local row=$1 refused=""
  shift
  while [ "$#" -gt 0 ]; do
    case $1 in
      --expect-refused)
        refused=$2
        shift 2
        ;;
      *) die "installed-bun: unknown argument '$1'" ;;
    esac
  done
  local bun digest version pinned printed problems=0
  digest=$(ci_tools_digest "$row")
  version=$(ci_tools_field "$row" version)
  pinned=$(tr -d '[:space:]' < "$root/.bun-version")
  bun=$(command -v bun) || bun=""
  [ -n "$bun" ] ||
    die "installed-bun: no bun is on this job's PATH, so neither source of the $row row installed one"
  printed=$("$bun" --version)

  note "the bun this $(uname -m) runner holds, read against the $row row"
  echo "  bun --version answers $printed, and .bun-version pins $pinned"
  echo "  command -v bun gives $bun"
  echo "  the $row row pins version $version and sha256 $digest"
  if [ "$printed" != "$pinned" ]; then
    echo "::error::ci-tools-proof: the bun at $bun answers $printed, and .bun-version pins $pinned, so this runner installed another Bun than the one this repository locks." >&2
    problems=$((problems + 1))
  fi
  ci_tools_verify "$row" "$bun" "the bun on this $(uname -m) runner's PATH" ||
    problems=$((problems + 1))
  # The name the install must have left beside that bun, read by `bunx_reading` below, which is
  # also the whole of the `installed-bunx` verb: every call of this verb is taken after an
  # install, and an install that left no runnable bunx is what that reading exists to catch.
  bunx_reading "$bun" "$pinned" ||
    problems=$((problems + 1))
  if [ -n "$refused" ]; then
    # The reading the `refusals` verb is made of, taken here through the same function: which
    # requests to that host the fault proxy refused since the probe. What an empty one means is
    # this verb's own sentence, because it is about this row's two sources.
    refusals_read "$refused" || {
      echo "::error::ci-tools-proof: the fault proxy refused no request to $refused after the probe, so nothing here says the second source served this bun: the two sources of the $row row are one binary with one digest by design, and a run where $refused answered after all reads exactly as this one does." >&2
      problems=$((problems + 1))
    }
  fi
  [ "$problems" -eq 0 ] || exit 1
  echo "  the bun on this runner is the $row row's pinned binary"
}

# ---------------------------------------------------------------- installed-bunx

# bunx_reading <bun> <pinned>: the bunx on this job's PATH, read beside that bun and against the
# version .bun-version pins, which is the reading an install of Bun is held to after the digest
# one.
# Bun's npm package holds the binary alone, so a Bun its second source served arrives with no
# bunx beside it and .github/actions/setup-bun links one again after the fetch; an install that
# left none is a job whose every reading of bun passes and whose first bunx fails, which is the
# lint, test, ci-caches and docs-site jobs of .github/workflows/ci.yaml and the merge-reports
# step of .github/workflows/dispatch-browser.yaml — during the very outage this proof exists to
# survive.
# A version alone would catch neither half of it: a bunx that is not there answers no version
# at all rather than a wrong one, and a bunx that resolves to some other Bun answers one of its
# own, so this reads three things — that a bunx is on this job's PATH, that it answers
# the pinned version, and that it runs the same bytes as the bun beside it, which an install
# already held to its row's digest.
# The two are compared by their bytes and never by their names: a bunx is a symbolic link to
# the bun beside it, so the two paths differ by design, and sha256sum follows a link, which
# makes each of these readings the bytes that name runs rather than the name itself.
# A bunx that is a copy of that bun, or a link to it from somewhere else, reads as the same
# bytes and passes, which is the statement this reading makes.
# It prints what it read and returns non-zero rather than refusing the job itself, so that
# `installed-bun` can finish its other readings before it refuses; `installed-bunx` refuses at
# once on that return.
bunx_reading() {
  [ "$#" -eq 2 ] ||
    die "bunx_reading takes the bun this job holds and the version .bun-version pins, and this call gave $#"
  local bun=$1 pinned=$2 bunx printed bun_bytes bunx_bytes problems=0
  # A bunx whose link target is gone is not executable, so `command -v` finds nothing for it,
  # which is the reading this branch makes: a name no later step can run is a name that is not
  # there.
  bunx=$(command -v bunx) || bunx=""
  if [ -z "$bunx" ]; then
    echo "::error::ci-tools-proof: no bunx is on this job's PATH, and the bun at $bun is, so this install left the binary with no runnable bunx beside it: every version reading of that bun passes, and the first bunx a later step runs finds nothing." >&2
    return 1
  fi
  printed=$("$bunx" --version) || printed="nothing, since it did not run"
  bunx_bytes=$(sha256sum -- "$bunx" | cut -d ' ' -f 1) || bunx_bytes="nothing this run could read"
  bun_bytes=$(sha256sum -- "$bun" | cut -d ' ' -f 1) || bun_bytes="nothing this run could read"
  echo "  bunx --version answers $printed, and .bun-version pins $pinned"
  echo "  command -v bunx gives $bunx, which resolves to $(readlink -f -- "$bunx")"
  echo "  that bunx runs sha256 $bunx_bytes, and the bun at $bun runs $bun_bytes"
  if [ "$printed" != "$pinned" ]; then
    echo "::error::ci-tools-proof: the bunx at $bunx answers $printed, and .bun-version pins $pinned, so every later step that runs bunx runs another Bun than the one this repository locks." >&2
    problems=$((problems + 1))
  fi
  if [ "$bunx_bytes" != "$bun_bytes" ]; then
    echo "::error::ci-tools-proof: the bunx at $bunx runs sha256 $bunx_bytes and the bun at $bun runs $bun_bytes, so this job's bunx is not the binary a row's digest was read against: that digest reading holds the bun alone, and a bunx resolving to anything else is a Bun nothing here answers for." >&2
    problems=$((problems + 1))
  fi
  [ "$problems" -eq 0 ] || return 1
  echo "  the bunx beside that bun runs its bytes and answers the version it is pinned to"
}

# installed_bunx: the reading above for a job that names no row here. The `tool-fallback` job
# reads its four head installs under the fault tool by tool with `version-answers`, each one's
# digest having been held by the action that installed it, so the reading left for the Bun of
# that list is the name beside it; `installed-bun`, which the arm64 job reads its two halves
# with, takes the same reading between its digest reading and `--expect-refused`.
installed_bunx() {
  [ "$#" -eq 0 ] || die "installed-bunx takes no argument, and this call gave $#"
  local bun pinned
  bun=$(command -v bun) || bun=""
  [ -n "$bun" ] ||
    die "installed-bunx: no bun is on this job's PATH, so this job installed none to read a bunx beside"
  pinned=$(tr -d '[:space:]' < "$root/.bun-version")
  note "the bunx beside the bun this $(uname -m) runner holds"
  bunx_reading "$bun" "$pinned" || exit 1
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
  # case's, and when this case began, so that `report` reads how long its install took.
  # Every case calls this verb, so every case has a mark of its own.
  {
    printf 'origin=%s\n' "$([ -f "$origin_log" ] && wc -l < "$origin_log" || echo 0)"
    printf 'fault=%s\n' "$([ -f "$fault_log" ] && wc -l < "$fault_log" || echo 0)"
    printf 'started=%s\n' "$(date +%s)"
  } > "$mark"
  if [ "$#" -eq 0 ]; then
    note "the origin now serves every row whole"
  else
    note "the origin now serves $* with one byte changed, and every other row whole"
  fi
}

# ---------------------------------------------------------------- flip-last-byte

# flip_last_byte <file>: that file with its last byte changed, and the sha256 it has afterwards.
# It is the origin's own corruption rule for a file nothing takes a member out of (write_origin
# above), applied to a file that stays on the runner rather than to one the origin serves: the
# smallest edit that moves the sha256 of the whole file, and the one that leaves an archive's own
# framing alone until the digest is read.
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
# directory, `mise install github:sjawhar/jj@<version>` exits 0 having downloaded nothing —
# "installed but not activated" is its whole output — and
# `mise x github:sjawhar/jj@<version> -- sh -c 'command -v jj'` prints that path.
# It is no offline install: measured in CI at run 42507815176, that `mise install` resolves the
# tool's version list over api.github.com even when the directory already holds the tool, which
# is why the case that seeds it runs before the fault rather than inside it.
# The jj-primary case is about the bytes mise installed and not about mise's fetch, and a live
# `mise install` inside the fault is what made this job red during an outage: jj's release asset
# is on the host the fault holds, so that install would spend the outage downloading through a
# 504 and the job that exists to prove the outage survivable would fail in it.
# Seeding the directory takes the download out of the case and leaves the reading it is about
# where it was.
seed_mise_jj() {
  [ "$#" -eq 1 ] || die "seed-mise-jj takes the file to seed mise's install with, and this call gave $#"
  local file=$1 dockerfile="$root/packages/daemon/docker/worker.Dockerfile" jj_tool spec version dir
  [ -f "$file" ] || die "seed-mise-jj: there is no file at '$file' to seed mise's install with"
  jj_tool=$(sed -n 's/^ARG JJ_TOOL=//p' "$dockerfile")
  [ -n "$jj_tool" ] || die "seed-mise-jj: $dockerfile names no ARG JJ_TOOL"
  version=${jj_tool##*@}
  [ "$version" != "$jj_tool" ] ||
    die "seed-mise-jj: the ARG JJ_TOOL '$jj_tool' names no version after an @, so this rig cannot name mise's install directory for it"
  # mise's own directory for a tool is that tool's spec with its `:` and its `/` written as `-`,
  # and it is derived here rather than written out: an ARG that moved to another fork would
  # otherwise leave this rig seeding a directory mise never reads, and the case would download
  # the real jj and fail as "did not refuse", naming the wrong cause.
  spec=${jj_tool%@*}
  spec=${spec//:/-}
  spec=${spec//\//-}
  dir="${MISE_DATA_DIR:-$HOME/.local/share/mise}/installs/$spec/$version"
  # The directory holds this one file and nothing else, which is what the measurement above was
  # taken on: a half-written install an earlier `mise install` left behind is not that reading.
  rm -rf "$dir"
  mkdir -p "$dir"
  install -m 0755 -- "$file" "$dir/jj"
  note "mise's install of $jj_tool is now $dir/jj"
  sha256sum -- "$dir/jj"
}

# ---------------------------------------------------------------- install-bound

# install_bound: the bound above, printed and nothing else, for the one reader outside this
# script: the step of the `tool-fallback` job that holds the eight installs with nowhere to go
# to the same seconds `report` holds a case's install to.
install_bound() {
  printf '%s\n' "$INSTALL_BOUND"
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

# route_of <source> <the caller a refusal names>: the origin route one source serves, read from
# the one word that source has — the name a refusal of .github/scripts/ci-tools.sh carries,
# which is also the manifest's own `second_source` word.
# It is the one place in this shell that turns a source word into a route: `payload_names`
# above decides which fields a row's second-source names come from by the route this answers,
# so nothing in this shell holds a second mapping of them.
# The places of this tree that decide a source word are listed at ci_tools__source_fetch in
# .github/scripts/ci-tools.sh — this function and the origin's own GITHUB_WORDS and URL_WORDS
# among them, the origin being a program of its own, started with the manifest and no word of
# this shell. That comment is where a new word is read off; this one keeps no count.
route_of() {
  case $1 in
    github-release) printf '/github/\n' ;;
    ghcr-mirror) printf '/ghcr/\n' ;;
    npm) printf '/npm/\n' ;;
    pypi | pulumi-releases) printf '/url/\n' ;;
    *) die "$2: '$1' is no source this rig serves (github-release, ghcr-mirror, npm, pypi or pulumi-releases)" ;;
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

  # How long this case's install took, read from the mark its own `corrupt` wrote immediately
  # before it. The time is read here rather than given as an argument, so that the reading can
  # never drift away from the step it measures.
  # A mark whose started= line is missing or unreadable stops the rig: a case read with no clock
  # is exactly the case this clock exists to catch, so it may not pass quietly.
  local started elapsed
  started=$(sed -n 's/^started=//p' "$mark")
  case $started in
    '' | *[!0-9]*)
      die "report: the mark of case '$name' holds '${started:-no started= line}', so this rig cannot read how long that install took, and that reading is the one thing that tells an install the runner ended at its timeout-minutes from an install that refused the payload"
      ;;
  esac
  elapsed=$(($(date +%s) - started))

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
  echo "  the install of this case took ${elapsed}s, against the ${INSTALL_BOUND}s no timed install may reach"

  if [ "$outcome" != "$wanted" ]; then
    echo "::error::ci-tools-proof: case $name: the install step's outcome is $outcome, and this case is the proof that the install reaches $wanted: read the step's own log above for what it did instead." >&2
    problems=$((problems + 1))
  fi
  if [ "$elapsed" -ge "$INSTALL_BOUND" ]; then
    echo "::error::ci-tools-proof: case $name: the install took ${elapsed}s, which reaches the ${INSTALL_BOUND}s no timed install may, so the runner ended that step at its own timeout-minutes rather than the step refusing the payload: a step the runner ends leaves outcome=failure behind it, which the reading above accepts, so the wall time is the only reading that tells the two apart; read this job's step record (gh api repos/<owner>/<repo>/actions/jobs/<id> --jq .steps[]) for the step that ran to its bound, and that step's log for 'The action has timed out.'" >&2
    problems=$((problems + 1))
  fi
  if [ -n "$expect_source" ]; then
    local route
    route=$(route_of "$expect_source" report)
    if [ "$route" = /github/ ]; then
      # The primary of this case is the faulted real host or a binary a third party installed, so
      # the origin's github route serves nothing for it. What says the bytes came from the
      # primary path is then that no second source served this row.
      # A case whose second source is itself the release asset — the pulumi-cli row's is — the
      # origin did serve on this route, and that is the first reading below rather than this one.
      if ! origin_served /github/ "$row" &&
        { origin_served /ghcr/ "$row" || origin_served /npm/ "$row" || origin_served /url/ "$row"; }; then
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
    untried=$(route_of "$expect_untried" report)
    if origin_served "$untried" "$row"; then
      echo "::error::ci-tools-proof: case $name: the origin served $row on its $untried route, and this case is the rule that it must not: a digest mismatch is a supply-chain signal and never a reason to read another source." >&2
      problems=$((problems + 1))
    fi
  fi

  [ "$problems" -eq 0 ] || exit 1
  echo "  the case reads as it must"
}

# ---------------------------------------------------------------- ladder-report

ladder_report() {
  [ "$#" -eq 0 ] || die "ladder-report takes no argument, and this call gave $#"
  # The stand-in writes a line for every fetch it is handed, so a log that is not there or
  # holds nothing is a stand-in that never reached this job's PATH. This refusal is therefore
  # also the reading that says the appended bound applied on the runner at all.
  [ -s "$curl_log" ] ||
    die "ladder-report: there is no fetch at all in $curl_log, so the stand-in curl the shorten-ladder step above writes never ran: read that step for the directory it adds to this job's PATH, because a job that runs the runner's own curl rides the pinned ladder for every one of its refusals"
  local problems=0 names="" longest=0 bypass_records=0 unrecognised="" rode_out="" served=""
  local kept measured who
  local seconds decision host status bypass line
  note "every fetch the stand-in curl of the shorten-ladder step measured"
  while IFS= read -r line || [ -n "$line" ]; do
    # Every line is matched whole against the shape write_stand_in's printf writes, and a line
    # of any other shape refuses the job. The match holds seconds to six digits and exit to
    # three, so each [ … -le … ], [ … -lt … ], [ … -gt … ] and [ … -eq … ] below reads a number
    # bash can hold; a value past that limit makes such a test fail with `integer expression
    # expected` and read false, which would pass the job with no refusal. Each key carries its
    # `=`, and the `$` anchor ends the line at the fifth field.
    # The `|| [ -n "$line" ]` of the loop header reads a last line that has no newline, which a
    # plain `read` drops.
    [[ $line =~ ^seconds=([0-9]{1,6})\ decision=(full|shortened)\ host=([^ ]+)\ exit=([0-9]{1,3})\ full-ladder=([^ ]+)$ ]] ||
      die "ladder-report: this line of $curl_log is not the five-field line write_stand_in's printf writes (seconds= decision= host= exit= full-ladder=, each value without a space): $line"
    seconds=${BASH_REMATCH[1]} decision=${BASH_REMATCH[2]} host=${BASH_REMATCH[3]} status=${BASH_REMATCH[4]} bypass=${BASH_REMATCH[5]}
    printf '  %s: %s, curl exit %s, %ss, full-ladder %s\n' "$host" "$decision" "$status" "$seconds" "$bypass"
    if [ "$bypass" != none ]; then
      bypass_records=$((bypass_records + 1))
      case " $names " in
        *" $bypass "*) ;;
        *) names="$names $bypass" ;;
      esac
      [ "$seconds" -le "$longest" ] || longest=$seconds
    elif [ "$decision" = full ] && [ "$seconds" -ge "$LADDER_FLOOR" ]; then
      # curl's exit status is what tells the two apart. A fetch that spent a whole ladder and
      # failed is a refusal this job arranged whose shape the stand-in's rule did not
      # recognise, and that sleep is what this change exists to take out. A fetch that spent as
      # long and served the bytes rode out a flap of its source, which is the work the pinned
      # ladder is kept for, so refusing that one would be the opposite of what this change is
      # for.
      if [ "$status" -eq 0 ]; then
        rode_out="$rode_out $host"
      else
        unrecognised="$unrecognised $host"
      fi
    elif [ "$decision" = shortened ] && [ "$status" -eq 0 ]; then
      # A fetch this job arranged to fail cannot serve the bytes: the fault proxy refuses every
      # request for a host it holds, the dead address answers none, and a dead proxy carries
      # none. So a shortened fetch that exited 0 is a fetch this job meant to succeed, and
      # shortening it took the retries that ride out a flap of that source off a required job,
      # which is the opposite of what this rig claims below.
      served="$served $host"
    fi
  done < "$curl_log"
  kept=${names# }

  for who in $rode_out; do
    echo "::warning::ci-tools-proof: the fetch of $who stayed on the pinned ladder, took at least $LADDER_FLOOR s and served the bytes, so it rode out a flap of that source or it is one of this job's large downloads; that is the work this ladder is kept for, and this reading refuses nothing for it"
  done
  measured="$longest s"
  [ -n "$kept" ] || measured="no fetch at all"
  echo "  the case that keeps the pinned ladder must measure inside $LADDER_FLOOR..$LADDER_CEILING s, and it measured $measured"
  echo "  $bypass_records of those fetches carried CI_TOOLS_PROOF_FULL_LADDER, and this rig holds that count to $LADDER_RECORDS"

  if [ -z "$kept" ]; then
    echo "::error::ci-tools-proof: no fetch of this job carried CI_TOOLS_PROOF_FULL_LADDER, so no case kept the pinned ladder and this job measured that ladder nowhere: one case must carry that variable, and the window above is read of that one case." >&2
    problems=$((problems + 1))
  elif [ "$(wc -w <<<"$kept")" -gt 1 ]; then
    echo "::error::ci-tools-proof: these names kept the pinned ladder: $kept. This rig keeps one measurement of it and not several, so one case carries CI_TOOLS_PROOF_FULL_LADDER and every other case is shortened." >&2
    problems=$((problems + 1))
  elif [ "$longest" -lt "$LADDER_FLOOR" ]; then
    echo "::error::ci-tools-proof: the case that kept the pinned ladder ($kept) measured $longest s, under the floor of $LADDER_FLOOR s, so it stopped driving the real ladder: its fetch now ends before the back-off of 1+2+4+8+16+32 s does, and nothing in this job measures the ladder an outage is ridden out on." >&2
    problems=$((problems + 1))
  elif [ "$longest" -gt "$LADDER_CEILING" ]; then
    echo "::error::ci-tools-proof: the case that kept the pinned ladder ($kept) measured $longest s, over the ceiling of $LADDER_CEILING s, which is more than a minute of new attempts and one last attempt of a row's max_time of 60 s: read that fetch's flags against the ladder ci_tools_curl pins, because under them a source that refuses at once costs 63 s and one that hangs about 124 s, so a reading over this ceiling is a ladder those flags no longer bound." >&2
    problems=$((problems + 1))
  fi
  # This reading stands on its own and is no further arm of the chain above: a job whose kept
  # measurement is also out of window would otherwise take one of those arms and say nothing
  # of the records at all.
  if [ "$bypass_records" -gt "$LADDER_RECORDS" ]; then
    echo "::error::ci-tools-proof: $bypass_records fetches of this job carried CI_TOOLS_PROOF_FULL_LADDER, over the $LADDER_RECORDS this rig holds that count to, and the names they carried it under are: $kept. The variable belongs at one step's env: in .github/workflows/ci.yaml and nowhere else, so a second env: that carries it is what to read that file for. At job scope or at workflow scope the stand-in shortens nothing, and this job then rides a full ladder for every refusal it arranges, which is the sleep this rig exists to take out. Where instead the one kept case now makes more requests than it did, measure them again and move LADDER_RECORDS at the top of this script, which is the one place that number is written." >&2
    problems=$((problems + 1))
  fi
  if [ -n "$unrecognised" ]; then
    echo "::error::ci-tools-proof: the fetch of$unrecognised stayed on the pinned ladder, took at least $LADDER_FLOOR s and failed, so this job arranged a refusal whose shape the stand-in curl does not recognise and slept a whole ladder for it: read the seam that carried it against FAULT_HOSTS and DEAD_ADDRESS at the top of this script." >&2
    problems=$((problems + 1))
  fi
  if [ -n "$served" ]; then
    echo "::error::ci-tools-proof: the fetch of$served was shortened and then served the bytes, so it is no refusal this job arranged: the shortening took the pinned ladder off a fetch this job means to succeed, and a flap of that source now reddens this job where the ladder rode it out. Read the point the shorten-ladder step runs at against the fetches this job makes before its fault is up." >&2
    problems=$((problems + 1))
  fi

  [ "$problems" -eq 0 ] || exit 1
  echo "  every fetch this job shortened is one it arranged to fail, and the one case that kept the pinned ladder measured it inside the window"
}

verb=${1:-}
[ "$#" -ge 1 ] ||
  die "usage: $(basename "$0") <origin-start|stage|shorten-ladder|start-fault|stop-fault|probe|version-answers|refusals|installed-bun|installed-bunx|corrupt|flip-last-byte|payload-path|seed-mise-jj|install-bound|report|ladder-report> [argument…]"
shift
case $verb in
  origin-start) origin_start "$@" ;;
  stage) stage "$@" ;;
  shorten-ladder) shorten_ladder "$@" ;;
  start-fault) start_fault "$@" ;;
  stop-fault) stop_fault "$@" ;;
  probe) probe "$@" ;;
  version-answers) version_answers "$@" ;;
  refusals) refusals "$@" ;;
  installed-bun) installed_bun "$@" ;;
  installed-bunx) installed_bunx "$@" ;;
  corrupt) corrupt "$@" ;;
  flip-last-byte) flip_last_byte "$@" ;;
  payload-path) payload_path "$@" ;;
  seed-mise-jj) seed_mise_jj "$@" ;;
  install-bound) install_bound "$@" ;;
  report) report "$@" ;;
  ladder-report) ladder_report "$@" ;;
  *) die "'$verb' is no verb of this rig (origin-start, stage, shorten-ladder, start-fault, stop-fault, probe, version-answers, refusals, installed-bun, installed-bunx, corrupt, flip-last-byte, payload-path, seed-mise-jj, install-bound, report, ladder-report)" ;;
esac
