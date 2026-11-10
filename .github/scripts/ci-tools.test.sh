#!/usr/bin/env bash
# Tests for `ci-tools.sh`: the primary serves the bytes and the line names it, a primary that does
# not serve sends the fetch to the second source, a primary that serves other bytes than the row
# pins stops the fetch there and the second source is never read, a second source that serves
# other bytes is refused the same way, a refusal of both names both sources, and no refusal of any
# kind leaves a file at the destination. A fetch that could not write the file this run gave it
# — curl's exit 23, which is what a runner with no room left answers with — is refused for that
# on either side, in a sentence that names this run and no service, and no second source is read
# after one, while a primary that merely does not answer falls through to the second source as
# it always has. A file curl cannot open at all and one it opens and cannot fill are that same
# exit of curl's and that same refusal, and ci_tools_fetch_primary answers for it as well.
# A row whose second source writes an address of its own is
# served whole or by the member it names, a zip that holds no such member and one that holds it
# and cannot give it back are refused in different words while the tar arm, which can tell
# those two apart no better than tar can, names no cause at all and leaves tar's own words to
# say why, a zip damaged in a way python3 raises no BadZipFile for is refused without a word
# that clears the bytes, a destination this run cannot write is refused by the one pre-check
# both arms share before either reads the archive and no failure of either leaves anything at
# the destination it was given, a row whose primary carries a `primary_digest` is held to it
# before its `primary_member` is taken out, and
# ci_tools_fetch_primary leaves the archive the primary served rather than that member. It also
# runs the repository's own .github/ci-tools.yaml through the helper, so a row this file cannot
# parse fails here rather than in a job.
#
# NO CASE REACHES THE NETWORK. Each case starts an HTTP origin of its own on 127.0.0.1, at a port
# the kernel picks, serving a directory this file builds: `github/…` for the release asset,
# `ghcr/token` and `ghcr/v2/…/blobs/sha256:…` for the package copy, `npm/@scope/…tgz` for the npm
# package, and `url/…` for a second source the row writes a whole address for. The case points
# the helper's five base seams (CI_TOOLS_MANIFEST, CI_TOOLS_GITHUB_BASE, CI_TOOLS_GHCR_BASE,
# CI_TOOLS_NPM_BASE, CI_TOOLS_URL_BASE) at that origin and at a fixture manifest of ten rows:
# a ghcr-mirror row with a primary, a ghcr-mirror row without one (jj's shape, where mise fetches
# the primary bytes), an npm row (Bun's shape), a row whose `primary:` the manifest leaves null,
# a row whose `second_source` is a word this repository does not write, a row whose
# `primary_source` is a word no primary is read with, a pypi row whose primary serves an archive
# with a digest of its own (uv's shape), a pulumi-releases row its second source serves whole
# (the docker provider plugin's shape), a pulumi-releases row whose second source serves a
# .tar.gz the row names a member of, and a row whose primary is a pulumi-releases address and
# whose second source is that same tarball's release asset (the pulumi CLI's shape).
# The origin writes every path it is asked for to a log, which is how a case says where a run
# went and, for the mismatch case, where it did not go.
#
# Every run of the helper is `bash -euo pipefail -c 'source …'`, the shell a GitHub `run:` step
# has, because the file under test is sourced into such a shell and carries no `set` of its own.
#
# THE CURL ON THESE CASES' PATH: the helper pins `--retry 7 --retry-all-errors` with
# `--retry-max-time 120`, so each case where a source refuses would sleep the whole of curl's
# ladder - 127 s of waits, which is what reaching this repository's 120 s floor costs
# (dispatch://INFRA-1545, R1) - before curl gave up. The cases put a curl of their own first on PATH, a two-line script that runs the
# real curl with `--retry-max-time 1` appended after the helper's own arguments, which curl reads
# as the last word on that option. It bounds the sleep and changes nothing else. A .curlrc cannot
# do it, because curl reads its configuration file before the command line and the helper's own
# bound would win. A CI runner sees neither, so the pinned ladder is what runs there.
#
# THE MKTEMP ON THESE CASES' PATH: the write-error cases must name the file a fetch writes into,
# and that file is a mktemp the helper makes under RUNNER_TEMP. Those cases put an mktemp of
# their own beside that curl, inert until a case writes the control file it reads. The control
# file names one row and a path, and the stand-in answers that row's own template — the one
# ci_tools__tempfile writes for it — with a symlink to that path: to a directory, which curl
# cannot open, or to /dev/full, which curl opens and cannot fill. Every other call it is handed,
# in that case and in every other, is the real mktemp.
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

# The mktemp those cases run, beside that curl. The control file holds `<row> <target>`, and the
# answer is one symlink at a fixed path under the runner's temporary directory, where the real
# mktemp's answer would be, so the case knows the name the refusal will carry and
# `temporary_files` reads it: the helper's own `rm -f` takes the symlink away as it would the
# temporary file.
mktemp_control="$work/mktemp-control"
mktemp_answer="$runner_temp/mktemp-answer"
real_mktemp=$(command -v mktemp)
cat > "$work/bin/mktemp" <<SH
#!/usr/bin/env bash
if [ -s "$mktemp_control" ]; then
  read -r row target < "$mktemp_control"
  for argument in "\$@"; do
    case "\$argument" in
      */ci-tools-\$row.XXXXXX)
        ln -sfn "\$target" "$mktemp_answer"
        printf '%s\n' "$mktemp_answer"
        exit 0
        ;;
    esac
  done
fi
exec "$real_mktemp" "\$@"
SH
chmod 0755 "$work/bin/mktemp"

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

# The npm package's tarball, which holds the binary at the member the row names, one whose
# member is the changed copy, and one healthy tarball that holds no such member at all, which
# is the tar arm's other outcome.
mkdir -p "$work/npm-build/package/bin" "$work/npm-build/package/doc"
cp "$work/payloads/packaged" "$work/npm-build/package/bin/packaged"
tar -czf "$work/payloads/packaged.tgz" -C "$work/npm-build" package/bin/packaged
cp "$work/payloads/packaged.changed" "$work/npm-build/package/bin/packaged"
tar -czf "$work/payloads/packaged.changed.tgz" -C "$work/npm-build" package/bin/packaged
printf 'the package readme, which is no binary\n' > "$work/npm-build/package/doc/README"
tar -czf "$work/payloads/packaged.no-member.tgz" -C "$work/npm-build" package/doc/README

# The uv-shaped payload: one binary, in a release tarball under one member and in a wheel under
# another. The row's own `digest` is of that binary, so the archive the primary serves has a
# digest of its own, which is what a row's `primary_digest` holds.
printf 'the uv-like binary a tarball and a wheel both hold\n' > "$work/payloads/wheeled"
WHEELED=$(digest_of "$work/payloads/wheeled")
mkdir -p "$work/wheel-build/wheeled-7.0.0" "$work/wheel-build/wheeled-7.0.0.data/scripts" \
  "$work/wheel-build/wheeled-7.0.0.dist-info"
cp "$work/payloads/wheeled" "$work/wheel-build/wheeled-7.0.0/wheeled"
cp "$work/payloads/wheeled" "$work/wheel-build/wheeled-7.0.0.data/scripts/wheeled"
printf 'Name: wheeled\n' > "$work/wheel-build/wheeled-7.0.0.dist-info/METADATA"
tar -czf "$work/payloads/wheeled.tar.gz" -C "$work/wheel-build" wheeled-7.0.0/wheeled
# Another archive holding that same member: the bytes the row pins are inside it and it is not
# the archive `primary_digest` names, which is what a case holds the archive's own digest to.
printf 'a release note the pinned tarball does not hold\n' > "$work/wheel-build/wheeled-7.0.0/NOTES"
tar -czf "$work/payloads/wheeled.other.tar.gz" -C "$work/wheel-build" \
  wheeled-7.0.0/wheeled wheeled-7.0.0/NOTES
WHEELED_TARBALL=$(digest_of "$work/payloads/wheeled.tar.gz")
WHEELED_OTHER=$(digest_of "$work/payloads/wheeled.other.tar.gz")

# zip_of <zip> <member…>: those members of $work/wheel-build in a zip, which is the archive a
# wheel is. python3 writes it, as the helper reads it, so the case needs no zip command.
zip_of() {
  local archive=$1
  shift
  PYTHONDONTWRITEBYTECODE=1 python3 - "$archive" "$work/wheel-build" "$@" <<'PY'
import sys
import zipfile

archive, root = sys.argv[1:3]
with zipfile.ZipFile(archive, "w") as held:
    for member in sys.argv[3:]:
        held.write(f"{root}/{member}", member)
PY
}
zip_of "$work/payloads/wheeled.whl" \
  wheeled-7.0.0.data/scripts/wheeled wheeled-7.0.0.dist-info/METADATA
zip_of "$work/payloads/wheeled.no-member.whl" wheeled-7.0.0.dist-info/METADATA

# The same wheel with the last byte of its member's own data changed. zip_of stores its members
# rather than deflating them, so that byte is the member's and nothing else, and python3's
# zipfile refuses it by the CRC-32 the archive's own header holds: an archive that holds the
# member and cannot give it back, which is the failure the case below holds apart from an
# archive that holds no such member at all.
cp "$work/payloads/wheeled.whl" "$work/payloads/wheeled.damaged.whl"
PYTHONDONTWRITEBYTECODE=1 python3 - "$work/payloads/wheeled.damaged.whl" \
  wheeled-7.0.0.data/scripts/wheeled <<'PY'
import sys
import zipfile
from pathlib import Path

archive, member = sys.argv[1:3]
with zipfile.ZipFile(archive) as held, held.open(member) as source:
    payload = source.read()
bytes_of = Path(archive).read_bytes()
last = bytes_of.rindex(payload) + len(payload) - 1
Path(archive).write_bytes(bytes_of[:last] + bytes([bytes_of[last] ^ 0x01]) + bytes_of[last + 1:])
PY

# Two more copies of that wheel, each with one field of the member's central-directory header
# changed, which is what a tampered archive looks like and what no exception list can catch:
# python3 raises neither a BadZipFile nor a zlib.error for either, so both leave the zip arm's
# list of definite damage. Bit 0 of the general purpose bit flag says the member is encrypted,
# which python3 answers with RuntimeError, and a compression method nobody implements it
# answers with NotImplementedError. The cases below read what each is refused with.
cp "$work/payloads/wheeled.whl" "$work/payloads/wheeled.flagged.whl"
cp "$work/payloads/wheeled.whl" "$work/payloads/wheeled.method.whl"
PYTHONDONTWRITEBYTECODE=1 python3 - wheeled-7.0.0.data/scripts/wheeled \
  "$work/payloads/wheeled.flagged.whl" "$work/payloads/wheeled.method.whl" <<'PY'
import struct
import sys
from pathlib import Path

member = sys.argv[1].encode()
flagged, methoded = (Path(name) for name in sys.argv[2:4])


def header_of(body: bytearray) -> int:
    """Where that member's own central-directory header begins, headers being walked in
    order: the last one in the file belongs to whichever member was written last."""
    at = body.find(b"PK\x01\x02")
    while at != -1:
        length, extra, comment = struct.unpack_from("<HHH", body, at + 28)
        if bytes(body[at + 46:at + 46 + length]) == member:
            return at
        at = body.find(b"PK\x01\x02", at + 46 + length + extra + comment)
    raise SystemExit(f"no central-directory header for {member!r}")


def patch(archive: Path, offset: int, value: int) -> None:
    body = bytearray(archive.read_bytes())
    struct.pack_into("<H", body, header_of(body) + offset, value)
    archive.write_bytes(bytes(body))


# +8 is the general purpose bit flag, whose bit 0 is the encrypted flag; +10 is the
# compression method, and 99 is no method python3 has.
body = bytearray(flagged.read_bytes())
patch(flagged, 8, struct.unpack_from("<H", body, header_of(body) + 8)[0] | 0x0001)
patch(methoded, 10, 99)
PY

# The plugin tarball a second source serves whole, and a tarball whose member is the row's bytes,
# which is the member rule on an archive that is no wheel.
mkdir -p "$work/plugin-build" "$work/tarmember-build"
printf 'the provider plugin binary, archive and all\n' > "$work/plugin-build/pulumi-resource-plugin"
tar -czf "$work/payloads/plugin.tar.gz" -C "$work/plugin-build" pulumi-resource-plugin
PLUGIN=$(digest_of "$work/payloads/plugin.tar.gz")
cp "$work/payloads/jjlike" "$work/tarmember-build/jjlike"
tar -czf "$work/payloads/tarmembered.tar.gz" -C "$work/tarmember-build" ./jjlike
# That same tarball with its last byte changed, which is one byte of the gzip framing `tar -xz`
# reads: a damaged stream, the tar arm's other way of not giving a member back. The origin of
# .github/scripts/ci-tools-proof.sh changes the same byte of a tarball for the same reason.
cp "$work/payloads/tarmembered.tar.gz" "$work/payloads/tarmembered.damaged.tar.gz"
PYTHONDONTWRITEBYTECODE=1 python3 - "$work/payloads/tarmembered.damaged.tar.gz" <<'PY'
import sys
from pathlib import Path

archive = Path(sys.argv[1])
body = archive.read_bytes()
archive.write_bytes(body[:-1] + bytes([body[-1] ^ 0x01]))
PY

# The clause both tar outcomes are refused in, written once because that is what the two cases
# below claim: `tar -xzO` answers an archive that holds no such member, a damaged stream and a
# destination that filled up part way through the write with the same exit, so the refusal
# names the member and the archive, names no cause at all, and leaves tar's own words on
# stderr to say why.
TAR_WHY='(tar exit 2); what tar printed on stderr above says why'

# The sentence a write error of this run is refused in, written once because that is what the
# four cases below claim: the stand-in mktemp hands one fetch the file the sentence names, curl
# cannot write it, and the refusal names this run and that file and no service at all.
WRITE_WHY="this run could not write $mktemp_answer (curl exit 23), which says nothing about any source or the bytes it serves — refusing"

# The CLI tarball: one archive two services serve, which is the row whose primary is a service
# of its own and whose second source is the release asset.
mkdir -p "$work/cli-build"
printf 'the CLI two services serve the same bytes of\n' > "$work/cli-build/clitool"
tar -czf "$work/payloads/clitool.tar.gz" -C "$work/cli-build" clitool
CLITOOL=$(digest_of "$work/payloads/clitool.tar.gz")

ASSET_PATH=github/example/tarred/releases/download/v1.0.0/tarred.tar.gz
WHEELED_ASSET_PATH=github/example/wheeled/releases/download/v7.0.0/wheeled.tar.gz
PLUGIN_ASSET_PATH=github/example/plugin/releases/download/v8.0.0/plugin-v8.0.0-linux-amd64.tar.gz
# The three paths the `url/…` subtree serves, which are the paths of the addresses the rows below
# write: the CI_TOOLS_URL_BASE seam keeps a second_url's path and replaces its scheme and host.
WHEEL_PATH=url/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl
PLUGIN_URL_PATH=url/plugins/plugin-v8.0.0-linux-amd64.tar.gz
TARMEMBER_URL_PATH=url/plugins/tarmembered-v9.0.0-linux-amd64.tar.gz
CLITOOL_URL_PATH=url/releases/sdk/clitool-v10.0.0-linux-x64.tar.gz
CLITOOL_ASSET_PATH=github/example/clitool/releases/download/v10.0.0/clitool-v10.0.0-linux-x64.tar.gz
cat > "$manifest" <<YAML
registry: $REGISTRY

tools:
  - name: tarred
    version: 1.0.0
    digest: $TARRED
    digest_of: the fixture's release tarball, whole
    primary_source: github-release
    primary: https://github.com/example/tarred/releases/download/v1.0.0/tarred.tar.gz
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: ghcr-mirror
    max_time: 7
    mirror_from: https://github.com/example/tarred/releases/download/v1.0.0/tarred.tar.gz
    mirror_member: ""
  - name: jjlike
    version: 2.0.0
    digest: $JJLIKE
    digest_of: the binary inside the fixture's release tarball
    primary_source: github-release
    primary: ""
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: ghcr-mirror
    max_time: 7
    mirror_from: https://github.com/example/jjlike/releases/download/v2.0.0/jjlike.tar.gz
    mirror_member: ./jjlike
  - name: packaged
    version: 3.0.0
    digest: $PACKAGED
    digest_of: the binary the package holds
    primary_source: github-release
    primary: ""
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: npm
    max_time: 7
    npm_package: "@fixture/packaged"
    npm_member: package/bin/packaged
  # A row whose primary: the manifest leaves null, which is how a reader finds Python's own
  # None where an empty string belongs. Its bytes are the tarred row's, so the copy the origin
  # already holds serves it.
  - name: nullprimary
    version: 4.0.0
    digest: $TARRED
    digest_of: the fixture's release tarball, whole
    primary_source: github-release
    primary:
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
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
    primary_source: github-release
    primary: ""
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: s3
    max_time: 7
  # The pulumi CLI's shape: the primary is a service of its own and the second source is the
  # release asset of the same tarball, so each side is a word and a URL of its own.
  - name: clitool
    version: 10.0.0
    digest: $CLITOOL
    digest_of: the CLI tarball, whole
    primary_source: pulumi-releases
    primary: https://get.example.com/releases/sdk/clitool-v10.0.0-linux-x64.tar.gz
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: github-release
    second_url: https://github.com/example/clitool/releases/download/v10.0.0/clitool-v10.0.0-linux-x64.tar.gz
    second_member: ""
    max_time: 7
  # A row whose primary_source is a word this file reads as a second source and as no row's
  # primary, which is refused by the side it is written on.
  - name: oddprimary
    version: 11.0.0
    digest: $TARRED
    digest_of: the fixture's release tarball, whole
    primary_source: ghcr-mirror
    primary: https://github.com/example/oddprimary/releases/download/v11.0.0/oddprimary.tar.gz
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: ghcr-mirror
    max_time: 7
  # uv's shape: the row's digest is of the binary, the primary serves a tarball holding it
  # under one member, and PyPI serves a wheel holding it under another.
  - name: wheeled
    version: 7.0.0
    digest: $WHEELED
    digest_of: the binary the fixture's release tarball and its wheel both hold
    primary_source: github-release
    primary: https://github.com/example/wheeled/releases/download/v7.0.0/wheeled.tar.gz
    primary_digest: $WHEELED_TARBALL
    primary_digest_of: the release tarball wheeled-7.0.0.tar.gz, whole
    primary_member: wheeled-7.0.0/wheeled
    second_source: pypi
    second_url: https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl
    second_member: wheeled-7.0.0.data/scripts/wheeled
    max_time: 7
  # The provider plugin's shape: one service serves the whole archive the row pins, at an
  # address of its own.
  - name: plugin
    version: 8.0.0
    digest: $PLUGIN
    digest_of: the plugin tarball, whole
    primary_source: github-release
    primary: https://github.com/example/plugin/releases/download/v8.0.0/plugin-v8.0.0-linux-amd64.tar.gz
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: pulumi-releases
    second_url: https://releases.example.com/plugins/plugin-v8.0.0-linux-amd64.tar.gz
    second_member: ""
    max_time: 7
  # A row whose second source serves a .tar.gz it names a member of, which is the member rule on
  # an archive that is no wheel. A third party fetches its primary bytes, as jj's does.
  - name: tarmembered
    version: 9.0.0
    digest: $JJLIKE
    digest_of: the binary inside the tarball the second source serves
    primary_source: github-release
    primary: ""
    primary_digest: ""
    primary_digest_of: ""
    primary_member: ""
    second_source: pulumi-releases
    second_url: https://releases.example.com/plugins/tarmembered-v9.0.0-linux-amd64.tar.gz
    second_member: ./jjlike
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
# its port read. Sets `root`, `log` and `base`, which the four base seams are built from. Each
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
  mkdir -p "$root/$(dirname "$ASSET_PATH")" "$root/$(dirname "$WHEELED_ASSET_PATH")" \
    "$root/$(dirname "$PLUGIN_ASSET_PATH")" "$root/$(dirname "$CLITOOL_ASSET_PATH")" \
    "$root/$(dirname "$WHEEL_PATH")" "$root/$(dirname "$PLUGIN_URL_PATH")" \
    "$root/$(dirname "$CLITOOL_URL_PATH")" "$root/ghcr/v2/$REGISTRY_PATH/blobs" \
    "$root/npm/@fixture/packaged/-"
  : > "$log"
  cp "$work/payloads/tarred" "$root/$ASSET_PATH"
  printf '{"token":"fixture-token"}\n' > "$root/ghcr/token"
  cp "$work/payloads/tarred" "$root/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED"
  cp "$work/payloads/jjlike" "$root/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$JJLIKE"
  cp "$work/payloads/wheeled.tar.gz" "$root/$WHEELED_ASSET_PATH"
  cp "$work/payloads/plugin.tar.gz" "$root/$PLUGIN_ASSET_PATH"
  cp "$work/payloads/clitool.tar.gz" "$root/$CLITOOL_ASSET_PATH"
  cp "$work/payloads/wheeled.whl" "$root/$WHEEL_PATH"
  cp "$work/payloads/plugin.tar.gz" "$root/$PLUGIN_URL_PATH"
  cp "$work/payloads/tarmembered.tar.gz" "$root/$TARMEMBER_URL_PATH"
  cp "$work/payloads/clitool.tar.gz" "$root/$CLITOOL_URL_PATH"
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
  out=$(env CI_TOOLS_MANIFEST="$manifest" \
    CI_TOOLS_GITHUB_BASE="$base/github" \
    CI_TOOLS_GHCR_BASE="$base/ghcr" \
    CI_TOOLS_NPM_BASE="$base/npm" \
    CI_TOOLS_URL_BASE="$base/url" \
    PATH="$work/bin:$PATH" \
    RUNNER_TEMP="$runner_temp" \
    bash -euo pipefail -c 'source "$1"; "$2" "${@:3}"' bash "$helper" "$@" 2>&1)
  status=$?
  set -e
}

# run_with_mktemp <row> <target> <function> <argument…>: that same run with the stand-in mktemp
# answering that row's temporary file with a symlink to <target>. The control file is written for
# this call and removed after it, so every other case runs with the real mktemp.
run_with_mktemp() {
  printf '%s %s\n' "$1" "$2" > "$mktemp_control"
  run "${@:3}"
  rm -f -- "$mktemp_control"
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

echo "case: github-release — the primary's fetch cannot write the file this run gave it"
# curl exits 23 where it could not write the file it was handed, which is a failure of this run
# and of nothing else: a runner with no room left is the live shape of it. So the refusal names
# this run and that file, says nothing of any source or the bytes it serves, and the fetch stops
# there — the second source cannot serve a file this runner will not let a fetch write, and
# reading it would cost a second full retry ladder for that answer.
# The stand-in mktemp is what lets a case name that file: it hands this row's fetch a symlink to
# a directory, which curl cannot open at any attempt of its ladder.
origin primary-write-error
dest="$work/dest/primary-write-error"
run_with_mktemp tarred "$work/dest" ci_tools_fetch tarred "$dest"
check "fails" "$(is "$status" 1)"
check "names this run and the file, and claims nothing of any source" \
  "$(contains "$out" "^::error::ci-tools: tarred: $WRITE_WHY$")"
check "and says of no source that it did not serve the bytes" \
  "$(is "$(contains "$out" 'did not serve the bytes')" false)"
check "curl asked the origin for the release asset" "$(is "$(asked_for "$log" "/github/")" true)"
check "and the second source was never read" "$(is "$(asked_for "$log" "/ghcr/")" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: github-release — ci_tools_fetch_primary cannot write the file this run gave it"
# The fetch that reads the primary alone has a write error arm of its own, and this is what
# drives it: the same symlink, the same exit 23, and a refusal that names this run and leaves
# the archive the primary serves at no destination.
origin primary-only-write-error
dest="$work/dest/primary-only-write-error"
run_with_mktemp tarred "$work/dest" ci_tools_fetch_primary tarred "$dest"
check "fails" "$(is "$status" 1)"
check "is refused in the words the primary's write error is refused in" \
  "$(contains "$out" "^::error::ci-tools: tarred: $WRITE_WHY$")"
check "and says of no source that it did not serve the bytes" \
  "$(is "$(contains "$out" 'did not serve the bytes')" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: github-release — the second source's own fetch cannot write it either"
# The same exit on the other side, refused in the same words: ci_tools_fetch_fallback reads the
# second source alone, so this is that source's own fetch failing to write, and what is known is
# again this run and not the service whose bytes were never written down.
origin second-write-error
dest="$work/dest/second-write-error"
run_with_mktemp clitool "$work/dest" ci_tools_fetch_fallback clitool "$dest"
check "fails" "$(is "$status" 1)"
check "is refused in the words the primary's write error is refused in" \
  "$(contains "$out" "^::error::ci-tools: clitool: $WRITE_WHY$")"
check "and says of no source that it served nothing" \
  "$(is "$(contains "$out" 'no source served the bytes')" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: github-release — a destination that fills part way through the write"
# The shape a runner actually meets: a destination this run can open and cannot fill. /dev/full
# is that exactly — every write to it is ENOSPC, which is what a runner with no room left gives
# a fetch part way through the write — so curl opens it, writes into it, and answers with the
# same exit 23 it answers a file it could never open with. It is refused as this run's fault in
# the one sentence, and the second source is still not read.
origin enospc-mid-write
dest="$work/dest/enospc-mid-write"
run_with_mktemp tarred /dev/full ci_tools_fetch tarred "$dest"
check "fails" "$(is "$status" 1)"
check "is refused in the words every write error of this run is refused in" \
  "$(contains "$out" "^::error::ci-tools: tarred: $WRITE_WHY$")"
check "the release asset was asked for" \
  "$(is "$(asked_for "$log" "/github/")" true)"
check "and the second source was never read" "$(is "$(asked_for "$log" "/ghcr/")" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: github-release — a primary that does not serve the bytes still reads the second source"
# The control for the four cases above: the fall-through is narrowed to curl's write error and
# not removed. This primary answers 404 — curl exit 22, which says something about that address
# and nothing about this run — so the copy is read and serves the row, exactly as before that
# one exit was told apart.
origin primary-404-falls-through
rm -- "$root/$ASSET_PATH"
dest="$work/dest/primary-404-falls-through"
run ci_tools_fetch tarred "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$TARRED")"
check "says the primary did not serve them, and goes on" \
  "$(contains "$out" '^ci-tools: tarred: github-release did not serve the bytes (curl exit 22); trying the second source$')"
check "the second source served them" \
  "$(contains "$out" "^ci-tools: tarred: ghcr-mirror served the bytes, sha256 $TARRED$")"
check "the blob the digest names was read" \
  "$(is "$(asked_for "$log" "/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED")" true)"
check "and no write error of this run was claimed" \
  "$(is "$(contains "$out" 'this run could not write')" false)"
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
  "$(contains "$out" "^::error::ci-tools: crane: $manifest holds no row of that name (it holds: clitool, jjlike, nullprimary, oddprimary, oddsource, packaged, plugin, tarmembered, tarred, wheeled)$")"
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
check "a word no second source is read from fails" "$(is "$status" 1)"
check "naming the row, the word and the five" \
  "$(contains "$out" "^::error::ci-tools: oddsource: the manifest's second_source is 's3', and this file reads github-release, ghcr-mirror, npm, pypi and pulumi-releases$")"
# Each side is the row's own word and neither is the other's default: this row's primary is a
# service of its own, and `github-release` is its second source, which is a word a row may now
# write on either side.
run ci_tools_source_name clitool primary
check "a primary that is no release asset is the row's own word" "$(is "$out" "pulumi-releases")"
run ci_tools_source_name clitool second
check "and a second source that is one says so" "$(is "$out" "github-release")"
run ci_tools_source_name wheeled second
check "a pypi row's second source is its own word" "$(is "$out" "pypi")"
run ci_tools_source_name plugin second
check "and a pulumi-releases row's is its own" "$(is "$out" "pulumi-releases")"
# One list decides the words of both sides, so a word this file reads as a second source and as
# no row's primary is refused by the side it was written on, here and where the bytes are read.
run ci_tools_source_name oddprimary primary
check "a word no primary is read with fails" "$(is "$status" 1)"
check "naming the row, the field and the words a primary is written in" \
  "$(contains "$out" "^::error::ci-tools: oddprimary: the manifest's primary_source is 'ghcr-mirror', and this file reads github-release, pypi and pulumi-releases as a primary$")"
run ci_tools_fetch oddprimary "$work/dest/oddprimary"
check "and the fetch of that row fails" "$(is "$status" 1)"
check "with the same sentence" \
  "$(contains "$out" "^::error::ci-tools: oddprimary: the manifest's primary_source is 'ghcr-mirror', and this file reads github-release, pypi and pulumi-releases as a primary$")"
check "and nothing is at the destination" "$(nothing_at "$work/dest/oddprimary")"
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

echo "case: the address of the package"
# One owner of that address: ci_tools_registry_url builds it from the CI_TOOLS_GHCR_BASE seam
# and the manifest's own `registry:`, ci_tools_blob_url is that address and the row's digest,
# and .github/scripts/mirror-ci-tools.sh pushes to it, so the filler of a copy and every reader
# of one address one package. These cases read the manifest and no source, so the base is a
# port nothing listens on.
base="http://127.0.0.1:1"
run ci_tools_registry
check "the package is the manifest's own registry:" "$(is "$out" "$REGISTRY")"
run ci_tools_registry_url
check "its address is the seam and that package's path" \
  "$(is "$out" "$base/ghcr/v2/$REGISTRY_PATH")"
run ci_tools_blob_url tarred
check "a row's blob is that address and the row's digest" \
  "$(is "$out" "$base/ghcr/v2/$REGISTRY_PATH/blobs/sha256:$TARRED")"
run ci_tools_blob_url crane
check "the blob of a row the manifest does not hold fails" "$(is "$status" 1)"
check "naming that row" \
  "$(contains "$out" "^::error::ci-tools: crane: $manifest holds no row of that name")"
run ci_tools_registry_url tarred
check "an argument to the address fails" "$(is "$status" 1)"
check "saying it takes none" \
  "$(contains "$out" "^::error::ci-tools: ci_tools_registry_url takes no argument, and this call gave 1$")"

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

echo "case: pulumi-releases — the second source serves the archive whole"
origin url-whole
rm -- "$root/$PLUGIN_ASSET_PATH"
dest="$work/dest/url-whole"
run ci_tools_fetch plugin "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$PLUGIN")"
check "says the primary did not serve them" \
  "$(contains "$out" '^ci-tools: plugin: github-release did not serve the bytes (curl exit 22); trying the second source$')"
check "names the source that did" \
  "$(contains "$out" "^ci-tools: plugin: pulumi-releases served the bytes, sha256 $PLUGIN$")"
check "read the address the row writes, under the seam" \
  "$(is "$(asked_for "$log" "/$PLUGIN_URL_PATH")" true)"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: pypi — the member of a .whl is the row's bytes"
origin url-wheel
rm -- "$root/$WHEELED_ASSET_PATH"
dest="$work/dest/url-wheel"
run ci_tools_fetch wheeled "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the member's bytes" "$(is "$(digest_of "$dest")" "$WHEELED")"
check "names the source that served them" \
  "$(contains "$out" "^ci-tools: wheeled: pypi served the bytes, sha256 $WHEELED$")"
check "read the wheel" "$(is "$(asked_for "$log" "/$WHEEL_PATH")" true)"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: pulumi-releases — the member of a .tar.gz is the row's bytes"
# The same member rule on an archive that is no wheel, which tar reads as the npm fetch does.
origin url-tar-member
dest="$work/dest/url-tar-member"
run ci_tools_fetch tarmembered "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the member's bytes" "$(is "$(digest_of "$dest")" "$JJLIKE")"
check "names the source that served them" \
  "$(contains "$out" "^ci-tools: tarmembered: pulumi-releases served the bytes, sha256 $JJLIKE$")"
check "read the address the row writes" "$(is "$(asked_for "$log" "/$TARMEMBER_URL_PATH")" true)"
check "and read no release asset, this row having no primary" \
  "$(is "$(asked_for "$log" "/github/")" false)"
check "and left no temporary file" "$(is "$(temporary_files)" "")"

echo "case: github-release — the primary carries a digest of its own and a member"
# uv's shape: the archive is held to `primary_digest`, the row's own `digest` is of the member
# inside it, and the member is what reaches the destination.
origin primary-member
dest="$work/dest/primary-member"
run ci_tools_fetch wheeled "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the member, not the archive" \
  "$(is "$(digest_of "$dest")" "$WHEELED")"
check "the line names the row's own digest" \
  "$(contains "$out" "^ci-tools: wheeled: github-release served the bytes, sha256 $WHEELED$")"
check "the release asset was read" "$(is "$(asked_for "$log" "/$WHEELED_ASSET_PATH")" true)"
check "and no second source was" "$(is "$(asked_for "$log" "/url/")" false)"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: github-release — the primary serves another archive than its primary_digest pins"
# That archive holds the member the row pins and is not the archive the row pins, which is what
# says the archive's own digest is checked and not the member's alone. A mismatch is a
# supply-chain signal, so the second source is not read after one.
origin primary-digest-changed
cp "$work/payloads/wheeled.other.tar.gz" "$root/$WHEELED_ASSET_PATH"
dest="$work/dest/primary-digest-changed"
run ci_tools_fetch wheeled "$dest"
check "fails" "$(is "$status" 1)"
check "names the source, what that digest is of and both digests" \
  "$(contains "$out" "^::error::ci-tools: wheeled: the bytes from github-release are not the pinned ones for the release tarball wheeled-7.0.0.tar.gz, whole (pinned sha256 $WHEELED_TARBALL, read sha256 $WHEELED_OTHER) — refusing$")"
check "the second source was not read" "$(is "$(asked_for "$log" "/url/")" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: pypi — the archive holds no such member"
# An archive without the member is a refusal naming the member and the archive, and not an empty
# file the digest check would refuse for the wrong reason. The code python3 exits with is one of
# the helper's own two, and it is what tells this failure from the case below.
origin url-member-missing
rm -- "$root/$WHEELED_ASSET_PATH"
cp "$work/payloads/wheeled.no-member.whl" "$root/$WHEEL_PATH"
dest="$work/dest/url-member-missing"
run ci_tools_fetch wheeled "$dest"
check "fails" "$(is "$status" 1)"
check "names both sources, the member and the archive" \
  "$(contains "$out" '^::error::ci-tools: wheeled: no source served the bytes; github-release: curl exit 22; pypi: the archive at https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl holds no wheeled-7.0.0.data/scripts/wheeled (python3 exit 3)$')"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: pypi — the archive holds the member and its bytes are damaged"
# One byte of the member's own data changed, which python3's zipfile meets as a CRC-32 the
# archive's own header no longer matches. Nothing unverified is installed either way, and the
# two are not the same thing to say: this is what the ARCHIVE raised, which is a signal about
# the bytes that address served, and the refusal says so and quotes python3's own last line
# rather than calling the member absent, as the case above is.
# The exception's name is all this case reads of that quote. The message beside it is CPython's
# wording of a CRC failure, and a release that rewords it would redden a case that pinned it
# while saying nothing about this helper.
origin url-member-damaged
rm -- "$root/$WHEELED_ASSET_PATH"
cp "$work/payloads/wheeled.damaged.whl" "$root/$WHEEL_PATH"
dest="$work/dest/url-member-damaged"
run ci_tools_fetch wheeled "$dest"
check "fails" "$(is "$status" 1)"
check "refuses it as an integrity failure, quoting what python3 said" \
  "$(contains "$out" "^::error::ci-tools: wheeled: no source served the bytes; github-release: curl exit 22; pypi: the archive at https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl did not give up wheeled-7.0.0.data/scripts/wheeled, and not because it holds no such member: zipfile.BadZipFile: .* What that address served is no readable archive of the bytes this row pins, which is a signal about those bytes (python3 exit 4)$")"
check "and does not call the member absent" \
  "$(is "$(contains "$out" "holds no wheeled-7.0.0.data/scripts/wheeled")" false)"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"
# That same archive through ci_tools_extract itself, which is where the destination is written.
# Every fetch above hands it a temporary file of its own, so what a caller that names its own
# destination is left with is read here, on the function this file's header calls public.
base="http://127.0.0.1:1"
dest="$work/dest/extract-damaged"
run ci_tools_extract "$work/payloads/wheeled.damaged.whl" wheeled-7.0.0.data/scripts/wheeled \
  "$dest" https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl
check "ci_tools_extract called directly fails too" "$(is "$status" 1)"
check "and leaves nothing at the destination it was given" "$(nothing_at "$dest")"

echo "case: npm — the package tarball holds no such member"
# The first of the tar arm's two outcomes: a healthy tarball the member is not in. The arm
# cannot tell it from the case after it, so the sentence names the member and the archive,
# names no cause at all, and leaves tar's own words on stderr to say why.
origin npm-member-missing
cp "$work/payloads/packaged.no-member.tgz" "$root/npm/@fixture/packaged/-/packaged-3.0.0.tgz"
dest="$work/dest/npm-member-missing"
run ci_tools_fetch_fallback packaged "$dest"
check "fails" "$(is "$status" 1)"
check "names the source, the member and the archive, and names no cause" \
  "$(contains "$out" "^::error::ci-tools: packaged: no source served the bytes; npm: the archive at $base/npm/@fixture/packaged/-/packaged-3.0.0.tgz did not give up package/bin/packaged $TAR_WHY\$")"
check "and tar's own words are in the log" "$(contains "$out" '^tar: ')"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: pulumi-releases — the .tar.gz holds the member and its bytes are damaged"
# The other tar outcome: the tarball does hold the member and one changed byte of its gzip
# framing means no reader reaches it. The refusal is the sentence of the case above, word for
# word, which is this arm's statement that it names no cause it cannot prove.
origin url-tar-damaged
cp "$work/payloads/tarmembered.damaged.tar.gz" "$root/$TARMEMBER_URL_PATH"
dest="$work/dest/url-tar-damaged"
run ci_tools_fetch tarmembered "$dest"
check "fails" "$(is "$status" 1)"
check "is refused in the sentence the absent member above is refused in" \
  "$(contains "$out" "^::error::ci-tools: tarmembered: no source served the bytes; pulumi-releases: the archive at https://releases.example.com/plugins/tarmembered-v9.0.0-linux-amd64.tar.gz did not give up ./jjlike $TAR_WHY\$")"
check "and tar's own words are in the log" "$(contains "$out" '^tar: ')"
check "nothing is at the destination" "$(nothing_at "$dest")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"
# That same tarball through ci_tools_extract itself, as the damaged wheel above is read: the
# fetches hand this function a temporary file of their own and remove it themselves, so what
# a caller that names its own destination is left with when tar fails is pinned here and
# nowhere else.
base="http://127.0.0.1:1"
dest="$work/dest/extract-tar-damaged"
run ci_tools_extract "$work/payloads/tarmembered.damaged.tar.gz" ./jjlike \
  "$dest" https://releases.example.com/plugins/tarmembered-v9.0.0-linux-amd64.tar.gz
check "ci_tools_extract called directly fails too" "$(is "$status" 1)"
check "and leaves nothing at the destination it was given" "$(nothing_at "$dest")"

echo "case: ci_tools_extract — a destination this run cannot write, by either arm"
# The one failure of this run either arm can prove, and the one thing proved before a byte of
# an archive is read: the pre-check above the case opens the destination, so both arms refuse
# it in the one sentence, before the wheel below reaches python3 and before the tarball below
# reaches tar. The destination here is under a directory this file never makes.
# ci_tools_extract takes no row, so neither call reads the manifest, and neither reads a
# source: the base is a port nothing listens on.
base="http://127.0.0.1:1"
dest="$work/dest/no-such-directory/extract-oserror"
run ci_tools_extract "$work/payloads/wheeled.whl" wheeled-7.0.0.data/scripts/wheeled \
  "$dest" https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl
check "the zip arm fails" "$(is "$status" 1)"
check "saying this run cannot write the destination, and claiming nothing of the archive" \
  "$(contains "$out" "^this run cannot write $dest, so https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl was never read for wheeled-7.0.0.data/scripts/wheeled, and nothing here is a statement about the bytes that address served$")"
check "and gives no exit of a python3 it never ran" \
  "$(is "$(contains "$out" "python3 exit")" false)"
check "and does not call the member absent" \
  "$(is "$(contains "$out" "holds no wheeled-7.0.0.data/scripts/wheeled")" false)"
check "and nothing is at the destination" "$(nothing_at "$dest")"
dest="$work/dest/no-such-directory/extract-oserror-tar"
run ci_tools_extract "$work/payloads/tarmembered.tar.gz" ./jjlike \
  "$dest" https://releases.example.com/plugins/tarmembered-v9.0.0-linux-amd64.tar.gz
check "the tar arm fails" "$(is "$status" 1)"
check "in the same sentence, which is the one pre-check both arms share" \
  "$(contains "$out" "^this run cannot write $dest, so https://releases.example.com/plugins/tarmembered-v9.0.0-linux-amd64.tar.gz was never read for ./jjlike, and nothing here is a statement about the bytes that address served$")"
check "and gives no exit of a tar it never ran" "$(is "$(contains "$out" "tar exit")" false)"
check "and nothing is at the destination" "$(nothing_at "$dest")"

echo "case: ci_tools_extract — a tampered zip python3 raises no BadZipFile for"
# Archive damage no exception list catches, which is why no sentence of this arm may clear the
# bytes: one flipped bit of the member's central-directory flag word reaches python3 as a
# RuntimeError about an encrypted member, and a compression method nobody implements reaches
# it as a NotImplementedError. Both are the archive, not this run, and both exit 1, so what
# they are refused with must rule neither in nor out — a run that said "the archive is not
# what failed" here would be telling its reader that tampered bytes are fine.
# The exception's name is all these cases read of that quote, for the reason the CRC case
# above gives: the message beside it is CPython's wording.
for damage in flagged:RuntimeError method:NotImplementedError; do
  dest="$work/dest/extract-${damage%%:*}"
  run ci_tools_extract "$work/payloads/wheeled.${damage%%:*}.whl" \
    wheeled-7.0.0.data/scripts/wheeled "$dest" \
    https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl
  check "the ${damage%%:*} wheel fails" "$(is "$status" 1)"
  check "quoting the ${damage#*:} python3 raised and clearing nothing" \
    "$(contains "$out" "^the archive at https://files.example.org/packages/py3/w/wheeled/wheeled-7.0.0-py3-none-any.whl did not give up wheeled-7.0.0.data/scripts/wheeled: ${damage#*:}: .* python3 raised that, and this helper cannot tell from it whether the archive or this run is what failed — damage reaches python3 as an OSError, a RuntimeError, a NotImplementedError or a UnicodeDecodeError, and so does a destination that filled up part way through the write — so these bytes are not cleared (python3 exit 1)$")"
  check "and never says the archive is not what failed" \
    "$(is "$(contains "$out" "the archive is not what failed")" false)"
  check "and never calls itself no signal about those bytes" \
    "$(is "$(contains "$out" "no signal about the bytes")" false)"
  check "and does not call the member absent" \
    "$(is "$(contains "$out" "holds no wheeled-7.0.0.data/scripts/wheeled")" false)"
  check "and nothing is at the destination" "$(nothing_at "$dest")"
done

echo "case: ci_tools_fetch_primary places the archive the primary served"
# What .github/scripts/ci-tools-proof.sh needs: a row whose `digest` is of a member is staged as
# the archive that URL serves, held to the row's `primary_digest`.
origin fetch-primary
dest="$work/dest/fetch-primary"
run ci_tools_fetch_primary wheeled "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the archive" "$(is "$(digest_of "$dest")" "$WHEELED_TARBALL")"
check "with the member still inside it" \
  "$(is "$(tar -xzOf "$dest" wheeled-7.0.0/wheeled | sha256sum | awk '{ print $1 }')" "$WHEELED")"
check "and the line names the primary's own digest" \
  "$(contains "$out" "^ci-tools: wheeled: github-release served the bytes, sha256 $WHEELED_TARBALL$")"
check "and no second source was read" "$(is "$(asked_for "$log" "/url/")" false)"
dest="$work/dest/fetch-primary-plain"
run ci_tools_fetch_primary tarred "$dest"
check "a row that names no primary_digest is held to its own" "$(is "$status" 0)"
check "and the destination holds those bytes" "$(is "$(digest_of "$dest")" "$TARRED")"
run ci_tools_fetch_primary jjlike "$work/dest/fetch-primary-third-party"
check "a row whose primary bytes a third party fetches fails" "$(is "$status" 1)"
check "saying this file has no primary URL for it" \
  "$(contains "$out" "^::error::ci-tools: jjlike: a third party fetches this row's primary bytes, so this file has no primary URL to read them from — refusing$")"
check "and nothing is at that destination" \
  "$(nothing_at "$work/dest/fetch-primary-third-party")"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

echo "case: a pulumi-releases primary and a github-release second source"
# The pulumi CLI's shape: each side is a word and a URL of its own, read through its own seam.
origin two-sided
dest="$work/dest/two-sided"
run ci_tools_fetch clitool "$dest"
check "exits 0" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$CLITOOL")"
check "the line names the primary's own word" \
  "$(contains "$out" "^ci-tools: clitool: pulumi-releases served the bytes, sha256 $CLITOOL$")"
check "the primary's address was read" "$(is "$(asked_for "$log" "/$CLITOOL_URL_PATH")" true)"
check "and the release asset was not" "$(is "$(asked_for "$log" "/$CLITOOL_ASSET_PATH")" false)"
origin two-sided-primary-refuses
rm -- "$root/$CLITOOL_URL_PATH"
dest="$work/dest/two-sided-second"
run ci_tools_fetch clitool "$dest"
check "the fetch exits 0 where the primary did not serve" "$(is "$status" 0)"
check "the destination holds the bytes" "$(is "$(digest_of "$dest")" "$CLITOOL")"
check "the line names the primary that did not serve them" \
  "$(contains "$out" '^ci-tools: clitool: pulumi-releases did not serve the bytes (curl exit 22); trying the second source$')"
check "and the release asset that did" \
  "$(contains "$out" "^ci-tools: clitool: github-release served the bytes, sha256 $CLITOOL$")"
check "which was read through the github seam" \
  "$(is "$(asked_for "$log" "/$CLITOOL_ASSET_PATH")" true)"
check "and no temporary file is left" "$(is "$(temporary_files)" "")"

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
