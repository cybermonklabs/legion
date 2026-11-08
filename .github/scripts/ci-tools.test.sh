#!/usr/bin/env bash
# Tests for `ci-tools.sh`: the primary serves the bytes and the line names it, a primary that does
# not serve sends the fetch to the second source, a primary that serves other bytes than the row
# pins stops the fetch there and the second source is never read, a second source that serves
# other bytes is refused the same way, a refusal of both names both sources, and no refusal of any
# kind leaves a file at the destination. It also runs the repository's own .github/ci-tools.yaml
# through the helper, so a row this file cannot parse fails here rather than in a job.
#
# NO CASE REACHES THE NETWORK. Each case starts an HTTP origin of its own on 127.0.0.1, at a port
# the kernel picks, serving a directory this file builds: `github/…` for the release asset,
# `ghcr/token` and `ghcr/v2/…/blobs/sha256:…` for the package copy, and `npm/@scope/…tgz` for the
# npm package. The case points the helper's four seams (CI_TOOLS_MANIFEST, CI_TOOLS_GITHUB_BASE,
# CI_TOOLS_GHCR_BASE, CI_TOOLS_NPM_BASE) at that origin and at a fixture manifest of five rows:
# a ghcr-mirror row with a primary, a ghcr-mirror row without one (jj's shape, where mise fetches
# the primary bytes), an npm row (Bun's shape), a row whose `primary:` the manifest leaves null,
# and a row whose `second_source` is a word this repository does not write. The origin writes
# every path it is asked for to a log, which is how a case says where a run went and, for the
# mismatch case, where it did not go.
#
# Every run of the helper is `bash -euo pipefail -c 'source …'`, the shell a GitHub `run:` step
# has, because the file under test is sourced into such a shell and carries no `set` of its own.
#
# THE CURL ON THESE CASES' PATH: the helper pins `--retry 7 --retry-all-errors` with
# `--retry-max-time 60`, so each case where a source refuses would sleep up to a minute before
# curl gave up. The cases put a curl of their own first on PATH, a two-line script that runs the
# real curl with `--retry-max-time 1` appended after the helper's own arguments, which curl reads
# as the last word on that option. It bounds the sleep and changes nothing else. A .curlrc cannot
# do it, because curl reads its configuration file before the command line and the helper's own
# bound would win. A CI runner sees neither, so the pinned ladder is what runs there.
#
# Run from anywhere: .github/scripts/ci-tools.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Each digest and each payload below is read inside a command substitution, and bash does not
# carry errexit into one on its own: without this, a value that failed to build would be written
# into the fixture manifest anyway and every case would report on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repository=$(cd "$script_dir/../.." && pwd)
helper="$script_dir/ci-tools.sh"
work=$(mktemp -d)
servers=()
trap 'for server in ${servers[@]+"${servers[@]}"}; do kill "$server" 2>/dev/null || true; done; rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

REGISTRY=ghcr.io/example/fixture/ci
REGISTRY_PATH=example/fixture/ci

manifest="$work/ci-tools.yaml"
runner_temp="$work/runner-temp"
mkdir -p "$runner_temp" "$work/payloads" "$work/dest" "$work/bin"

# The curl the cases run, first on their PATH: the real one with `--retry-max-time 1` after the
# arguments the helper wrote, which is the bound the header above explains.
real_curl=$(command -v curl)
cat > "$work/bin/curl" <<SH
#!/usr/bin/env bash
exec "$real_curl" "\$@" --retry-max-time 1
SH
chmod 0755 "$work/bin/curl"

# The three payloads, one per source kind, and a copy of each with one byte changed. Nothing is
# pulled: each digest below is read off the bytes this file just wrote.
printf 'the bytes a release asset would hold\n' > "$work/payloads/tarred"
printf 'jj, the binary inside a release tarball\n' > "$work/payloads/jjlike"
printf 'the bun binary an install puts on PATH\n' > "$work/payloads/packaged"

# corrupt <file> <destination>: those bytes with the first one changed, which is the smallest
# difference a digest must catch.
corrupt() {
  local from=$1 to=$2
  cp -- "$from" "$to"
  printf 'X' | dd of="$to" bs=1 seek=0 conv=notrunc status=none
}
corrupt "$work/payloads/tarred" "$work/payloads/tarred.changed"
corrupt "$work/payloads/jjlike" "$work/payloads/jjlike.changed"
corrupt "$work/payloads/packaged" "$work/payloads/packaged.changed"

# digest_of <file>: the sha256 the manifest writes bare.
digest_of() { sha256sum -- "$1" | awk '{ print $1 }'; }

TARRED=$(digest_of "$work/payloads/tarred")
JJLIKE=$(digest_of "$work/payloads/jjlike")
PACKAGED=$(digest_of "$work/payloads/packaged")
TARRED_CHANGED=$(digest_of "$work/payloads/tarred.changed")
JJLIKE_CHANGED=$(digest_of "$work/payloads/jjlike.changed")
PACKAGED_CHANGED=$(digest_of "$work/payloads/packaged.changed")

# The npm package's tarball, which holds the binary at the member the row names, and one whose
# member is the changed copy.
mkdir -p "$work/npm-build/package/bin"
cp "$work/payloads/packaged" "$work/npm-build/package/bin/packaged"
tar -czf "$work/payloads/packaged.tgz" -C "$work/npm-build" package/bin/packaged
cp "$work/payloads/packaged.changed" "$work/npm-build/package/bin/packaged"
tar -czf "$work/payloads/packaged.changed.tgz" -C "$work/npm-build" package/bin/packaged

ASSET_PATH=github/example/tarred/releases/download/v1.0.0/tarred.tar.gz
cat > "$manifest" <<YAML
registry: $REGISTRY

tools:
  - name: tarred
    version: 1.0.0
    digest: $TARRED
    digest_of: the fixture's release tarball, whole
    primary: https://github.com/example/tarred/releases/download/v1.0.0/tarred.tar.gz
    second_source: ghcr-mirror
    max_time: 7
    mirror_from: https://github.com/example/tarred/releases/download/v1.0.0/tarred.tar.gz
    mirror_member: ""
  - name: jjlike
    version: 2.0.0
    digest: $JJLIKE
    digest_of: the binary inside the fixture's release tarball
    primary: ""
    second_source: ghcr-mirror
    max_time: 7
    mirror_from: https://github.com/example/jjlike/releases/download/v2.0.0/jjlike.tar.gz
    mirror_member: ./jjlike
  - name: packaged
    version: 3.0.0
    digest: $PACKAGED
    digest_of: the binary the package holds
    primary: ""
    second_source: npm
    max_time: 7
    npm_package: "@fixture/packaged"
    npm_member: package/bin/packaged
  # A row whose `primary:` the manifest leaves null, which is how a reader finds Python's own
  # `None` where an empty string belongs. Its bytes are the tarred row's, so the copy the origin
  # already holds serves it.
  - name: nullprimary
    version: 4.0.0
    digest: $TARRED
    digest_of: the fixture's release tarball, whole
    primary:
    second_source: ghcr-mirror
    max_time: 7
    mirror_from: https://github.com/example/nullprimary/releases/download/v4.0.0/nullprimary.tar.gz
    mirror_member: ""
  # A row whose second source is a word this repository does not write, which no reader of a
  # source word may print.
  - name: oddsource
    version: 5.0.0
    digest: $TARRED
    digest_of: the fixture's release tarball, whole
    primary: ""
    second_source: s3
    max_time: 7
YAML

# The origin: a directory server that writes every path it is asked for, served or not, to a log.
# A case reads that log to say which sources a run went to.
cat > "$work/origin.py" <<'PY'
import http.server
import socketserver
import sys

root, portfile, logfile = sys.argv[1:4]


class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=root, **kwargs)

    def do_GET(self):
        with open(logfile, "a") as log:
            log.write(self.path + "\n")
        super().do_GET()

    def log_message(self, *args):
        pass


socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", 0), Handler) as server:
    with open(portfile, "w") as handle:
        handle.write(str(server.server_address[1]))
    server.serve_forever()
PY

# origin <name>: this case's origin, holding the bytes every row pins, with its server running and
# its port read. Sets `root`, `log` and `base`, which the three base seams are built from. Each
# case names its own origin: two cases sharing a name would share a directory, so a repeated name
# is refused here rather than surfacing as one case seeing another's edits.
origin() {
  local name=$1 waited=0
  root="$work/origins/$name/root"
  log="$work/origins/$name/requests.log"
  if [ -e "$work/origins/$name" ]; then
    echo "  FAIL: the origin name '$name' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/$(dirname "$ASSET_PATH")" "$root/ghcr/v2/$REGISTRY_PATH/blobs" \
    "$root/npm/@fixture/packaged/-"
  : > "$log"
  cp "$work/payloads/tarred" "$root/$ASSET_PATH"
  printf '{"token":"fixture-token"}\n' > "$root/ghcr/token"
  cp "$work/payloads/tarred" "$root/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED"
  cp "$work/payloads/jjlike" "$root/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$JJLIKE"
  cp "$work/payloads/packaged.tgz" "$root/npm/@fixture/packaged/-/packaged-3.0.0.tgz"
  python3 "$work/origin.py" "$root" "$work/origins/$name/port" "$log" &
  servers+=("$!")
  while [ ! -s "$work/origins/$name/port" ]; do
    if [ "$waited" -gt 100 ]; then
      echo "  FAIL: the origin '$name' did not write its port" >&2
      return 1
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  base="http://127.0.0.1:$(cat "$work/origins/$name/port")"
}

# run <function> <argument…>: the helper, sourced into a shell with errexit, nounset and pipefail
# on, as a GitHub `run:` step has. Sets `out` to the combined output and `status` to the exit
# code. Not a subshell, so both survive the call.
run() {
  set +e
  out=$(CI_TOOLS_MANIFEST="$manifest" \
    CI_TOOLS_GITHUB_BASE="$base/github" \
    CI_TOOLS_GHCR_BASE="$base/ghcr" \
    CI_TOOLS_NPM_BASE="$base/npm" \
    PATH="$work/bin:$PATH" \
    RUNNER_TEMP="$runner_temp" \
    bash -euo pipefail -c 'source "$1"; "$2" "${@:3}"' bash "$helper" "$@" 2>&1)
  status=$?
  set -e
}

# run_real <function> <argument…>: the same, against the repository's own manifest with
# CI_TOOLS_MANIFEST unset, so the default resolved from ${BASH_SOURCE[0]} is what answers.
run_real() {
  set +e
  out=$(env -u CI_TOOLS_MANIFEST RUNNER_TEMP="$runner_temp" \
    bash -euo pipefail -c 'source "$1"; "$2" "${@:3}"' bash "$helper" "$@" 2>&1)
  status=$?
  set -e
}

# asked_for <log> <prefix>: true when the origin was asked for a path under that prefix.
asked_for() { grep -q "^$2" "$1" && echo true || echo false; }

# nothing_at <path>: true when nothing is there, which is what every refusal leaves at the
# destination.
nothing_at() { [ -e "$1" ] && echo false || echo true; }

# temporary_files: what the helper left under RUNNER_TEMP, which is nothing on every path.
temporary_files() { find "$runner_temp" -mindepth 1 | sort; }

echo "case: github-release — the primary serves the bytes"
origin primary-serves
dest="$work/dest/primary-serves"
run ci_tools_fetch tarred "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$TARRED")"
check "names the source that served them" \
  "$(contains "$out" "^ci-tools: tarred: github-release served the bytes, sha256 $TARRED$")"
check "read the release asset" "$(is "$(asked_for "$log" "/github/")" true)"
check "and read no second source" "$(is "$(asked_for "$log" "/ghcr/")" false)"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: ghcr-mirror — the primary refuses and the copy serves the bytes"
origin primary-refuses
rm -- "$root/$ASSET_PATH"
dest="$work/dest/primary-refuses"
run ci_tools_fetch tarred "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$TARRED")"
check "says the primary did not serve them" \
  "$(contains "$out" '^ci-tools: tarred: github-release did not serve the bytes (curl exit 22); trying the second source$')"
check "names the source that did" \
  "$(contains "$out" "^ci-tools: tarred: ghcr-mirror served the bytes, sha256 $TARRED$")"
check "asked the package for an anonymous pull token" \
  "$(is "$(asked_for "$log" "/ghcr/token?service=ghcr.io&scope=repository:$REGISTRY_PATH:pull")" true)"
check "and for the blob the digest names" \
  "$(is "$(asked_for "$log" "/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED")" true)"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: github-release — the primary serves one byte changed"
# A digest mismatch is a supply-chain signal, not an outage: the second source is never read
# after one, because the question it would answer is already answered.
origin primary-changed
cp "$work/payloads/tarred.changed" "$root/$ASSET_PATH"
dest="$work/dest/primary-changed"
run ci_tools_fetch tarred "$dest"
check "fails" "$(is "$status" 1)"
check "names the source and both digests" \
  "$(contains "$out" "^::error::ci-tools: tarred: the bytes from github-release are not the pinned ones (pinned sha256 $TARRED, read sha256 $TARRED_CHANGED) — refusing$")"
check "the second source was not read" "$(is "$(asked_for "$log" "/ghcr/")" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: ghcr-mirror — the copy serves one byte changed"
origin mirror-changed
rm -- "$root/$ASSET_PATH"
cp "$work/payloads/tarred.changed" "$root/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED"
dest="$work/dest/mirror-changed"
run ci_tools_fetch tarred "$dest"
check "fails" "$(is "$status" 1)"
check "names the source and both digests" \
  "$(contains "$out" "^::error::ci-tools: tarred: the bytes from ghcr-mirror are not the pinned ones (pinned sha256 $TARRED, read sha256 $TARRED_CHANGED) — refusing$")"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: both sources refuse"
origin both-refuse
rm -- "$root/$ASSET_PATH" "$root/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED"
dest="$work/dest/both-refuse"
run ci_tools_fetch tarred "$dest"
check "fails" "$(is "$status" 1)"
check "names both sources and what each did" \
  "$(contains "$out" '^::error::ci-tools: tarred: no source served the bytes; github-release: curl exit 22; ghcr-mirror: the blob request failed (curl exit 22)$')"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: ghcr-mirror — a row whose primary bytes a third party fetches"
# jj's shape: mise fetches the primary bytes, so there is no primary here to try. Either entry
# goes straight to the copy, and neither reads a release asset.
origin third-party-primary
dest="$work/dest/third-party-primary"
run ci_tools_fetch_fallback jjlike "$dest"
check "the fallback exits 0" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$JJLIKE")"
check "names the source that served them" \
  "$(contains "$out" "^ci-tools: jjlike: ghcr-mirror served the bytes, sha256 $JJLIKE$")"
dest="$work/dest/third-party-primary-fetch"
run ci_tools_fetch jjlike "$dest"
check "ci_tools_fetch of the same row exits 0" "$(is "$status" 0)"
check "naming the same source" \
  "$(contains "$out" "^ci-tools: jjlike: ghcr-mirror served the bytes, sha256 $JJLIKE$")"
check "and neither read a release asset" "$(is "$(asked_for "$log" "/github/")" false)"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: ghcr-mirror — that row's copy serves one byte changed"
origin third-party-changed
cp "$work/payloads/jjlike.changed" "$root/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$JJLIKE"
dest="$work/dest/third-party-changed"
run ci_tools_fetch_fallback jjlike "$dest"
check "fails" "$(is "$status" 1)"
check "names the source and both digests" \
  "$(contains "$out" "^::error::ci-tools: jjlike: the bytes from ghcr-mirror are not the pinned ones (pinned sha256 $JJLIKE, read sha256 $JJLIKE_CHANGED) — refusing$")"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: npm — the package serves the bytes"
origin npm-serves
dest="$work/dest/npm-serves"
run ci_tools_fetch packaged "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the member's bytes" "$(is "$(digest_of "$dest")" "$PACKAGED")"
check "names the source that served them" \
  "$(contains "$out" "^ci-tools: packaged: npm served the bytes, sha256 $PACKAGED$")"
check "read the package tarball" \
  "$(is "$(asked_for "$log" "/npm/@fixture/packaged/-/packaged-3.0.0.tgz")" true)"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: npm — the package serves one byte changed"
origin npm-changed
cp "$work/payloads/packaged.changed.tgz" "$root/npm/@fixture/packaged/-/packaged-3.0.0.tgz"
dest="$work/dest/npm-changed"
run ci_tools_fetch_fallback packaged "$dest"
check "fails" "$(is "$status" 1)"
check "names the source and both digests" \
  "$(contains "$out" "^::error::ci-tools: packaged: the bytes from npm are not the pinned ones (pinned sha256 $PACKAGED, read sha256 $PACKAGED_CHANGED) — refusing$")"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: npm — the registry refuses"
# The row has no primary, so the refusal names the one source this row has.
origin npm-refuses
rm -- "$root/npm/@fixture/packaged/-/packaged-3.0.0.tgz"
dest="$work/dest/npm-refuses"
run ci_tools_fetch packaged "$dest"
check "fails" "$(is "$status" 1)"
check "names the source and what it did" \
  "$(contains "$out" '^::error::ci-tools: packaged: no source served the bytes; npm: the package tarball request failed (curl exit 22)$')"
check "and names no release asset it never had" "$(is "$(contains "$out" 'github-release')" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: ci_tools_digest and ci_tools_field"
# These cases read the manifest and no source. The base they are run with points at a port
# nothing listens on, which is the statement that no source is read here.
base="http://127.0.0.1:1"
run ci_tools_digest tarred
check "ci_tools_digest prints the row's digest" "$(is "$out" "$TARRED")"
check "and exits 0" "$(is "$status" 0)"
run ci_tools_field packaged npm_package
check "ci_tools_field prints the field asked for" "$(is "$out" "@fixture/packaged")"
# jjlike's primary is empty: a third party fetches that row's primary bytes.
run ci_tools_field jjlike primary
check "an empty field is an empty line, not a failure" "$(is "$status" 0)"
check "and prints nothing" "$(is "$out" "")"
run ci_tools_digest crane
check "an unknown row fails" "$(is "$status" 1)"
check "naming the row and what the manifest holds" \
  "$(contains "$out" "^::error::ci-tools: crane: $manifest holds no row of that name (it holds: jjlike, nullprimary, oddsource, packaged, tarred)$")"
run ci_tools_field tarred npm_package
check "a field this row does not carry fails" "$(is "$status" 1)"
check "naming the row and the field" \
  "$(contains "$out" "^::error::ci-tools: tarred: this row of the manifest carries no field 'npm_package'")"

echo "case: a field the manifest leaves null"
# `primary:` with no value is a row whose primary bytes a third party fetches, and it reads as
# an empty string. Python's own `None` would reach the caller as the word None: the fetch would
# take that word for a URL and spend the whole retry ladder on it before it read the second
# source, and the filler would ask tar for a member of that name.
origin null-primary
dest="$work/dest/null-primary"
run ci_tools_field nullprimary primary
check "the field is empty" "$(is "$out" "")"
check "and reading it exits 0" "$(is "$status" 0)"
run ci_tools_fetch nullprimary "$dest"
check "the fetch exits 0" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$TARRED")"
check "and the line names the second source" \
  "$(contains "$out" "^ci-tools: nullprimary: ghcr-mirror served the bytes, sha256 $TARRED$")"
check "nothing is said of a primary this row has not" \
  "$(is "$(contains "$out" 'github-release')" false)"
check "no release asset was asked for" "$(is "$(asked_for "$log" "/github/")" false)"
check "and the blob the digest names was" \
  "$(is "$(asked_for "$log" "/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED")" true)"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: ci_tools_source_name"
# The one reader of a source word: .github/actions/install-jj and .github/actions/setup-bun take
# both of theirs from here, so a row that moved to another second source cannot leave an action
# naming a service that served no byte of what it installed.
# These cases read the manifest and no source, so the base is a port nothing listens on.
base="http://127.0.0.1:1"
run ci_tools_source_name tarred primary
check "the primary of a row that holds one is github-release" "$(is "$out" "github-release")"
run ci_tools_source_name packaged primary
check "and so is the primary of a row a third party fetches" "$(is "$out" "github-release")"
run ci_tools_source_name tarred second
check "the second source is the row's own word" "$(is "$out" "ghcr-mirror")"
run ci_tools_source_name packaged second
check "for an npm row too" "$(is "$out" "npm")"
run ci_tools_source_name oddsource second
check "a word that is none of the three fails" "$(is "$status" 1)"
check "naming the row, the word and the three" \
  "$(contains "$out" "^::error::ci-tools: oddsource: the manifest's second_source is 's3', and the sources this repository names are github-release, ghcr-mirror and npm$")"
run ci_tools_source_name tarred both
check "a mode that is neither primary nor second fails" "$(is "$status" 1)"
check "naming what the call asked for" \
  "$(contains "$out" "^::error::ci-tools: ci_tools_source_name reads the primary or the second source of a row, and this call asked for 'both'$")"
run ci_tools_source_name tarred
check "one argument fails" "$(is "$status" 1)"
check "saying what it takes" \
  "$(contains "$out" "^::error::ci-tools: ci_tools_source_name takes a row and primary or second, and this call gave 1$")"
run ci_tools_source_name crane primary
check "a row the manifest does not hold fails" "$(is "$status" 1)"
check "naming that row" \
  "$(contains "$out" "^::error::ci-tools: crane: $manifest holds no row of that name")"

echo "case: ci_tools_verify holds a file this run did not fetch to its row"
run ci_tools_verify tarred "$work/payloads/tarred" ghcr-mirror
check "a file whose digest matches passes" "$(is "$status" 0)"
check "and the line names the source" \
  "$(contains "$out" "^ci-tools: tarred: ghcr-mirror served the bytes, sha256 $TARRED$")"
check "and the file is left where it is" \
  "$(is "$(digest_of "$work/payloads/tarred")" "$TARRED")"
run ci_tools_verify tarred "$work/payloads/tarred.changed" github-release
check "a file whose digest does not match fails" "$(is "$status" 1)"
check "naming the source and both digests" \
  "$(contains "$out" "^::error::ci-tools: tarred: the bytes from github-release are not the pinned ones (pinned sha256 $TARRED, read sha256 $TARRED_CHANGED) — refusing$")"

echo "case: the repository's own .github/ci-tools.yaml"
# The default manifest seam, and every row of the real file, read off that file rather than
# listed here, so a row added to the manifest is exercised: a row this helper cannot read fails
# here rather than in the job that installs the tool.
real_rows=$(CI_TOOLS_FILE="$repository/.github/ci-tools.yaml" PYTHONDONTWRITEBYTECODE=1 python3 - <<'PY'
import os

import yaml

with open(os.environ["CI_TOOLS_FILE"]) as handle:
    document = yaml.safe_load(handle)
print("\n".join(row["name"] for row in document["tools"]))
PY
)
for row in $real_rows; do
  run_real ci_tools_digest "$row"
  check "$row prints a 64-character lower-case hex digest" \
    "$(contains "$out" '^[0-9a-f]\{64\}$')"
done

summary "ci-tools.sh"
