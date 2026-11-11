#!/usr/bin/env bash
# The leak guard's detection engine is gitleaks, and this repository names its version twice.
# packages/daemon/go.mod requires github.com/zricethezav/gitleaks/v8, the library
# packages/daemon/internal/leakguard links and so the engine behind every call site the `legion`
# binary reaches: a pod's pre-push hook, `legion gh`, `legion leak-check` and the daemon itself.
# .github/scripts/release-push.sh names the same version in the release-asset URL it fetches
# (leak_engine_url), because the release lane may run no built binary of this repository and so
# has to fetch a gitleaks of its own to read the same rule file with.
#
# If one moves and the other does not, the release lane scans with one engine and every other
# write of this repository with another, and a line one version refuses and the other passes is
# first met at a release. So this check fails until the two name one gitleaks: raising the engine
# is the `go get` and the URL — with leak_engine_sha256 taken from that release's own
# gitleaks_X.Y.Z_checksums.txt — together.
#
# Run from anywhere: .github/scripts/check-gitleaks-version.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

GOMOD = Path("packages/daemon/go.mod")
RELEASE = Path(".github/scripts/release-push.sh")
# The shape leak_engine_url must take, written in parts rather than as one URL: a whole release
# asset URL here would be a fetch to .github/scripts/check-ci-tools.sh, whose rule 3 reads every
# such URL under .github/ and holds it to a row of .github/ci-tools.yaml. This file fetches
# nothing; it only says what the fetch in release-push.sh must look like.
ASSET = "https://github.com/gitleaks/gitleaks/releases/" + "download/vX.Y.Z/gitleaks_X.Y.Z_linux_x64.tar.gz"


def fatal(path: Path, message: str) -> None:
    print(f"::error file={path}::{message}", file=sys.stderr)
    raise SystemExit(1)


def read(path: Path) -> str:
    try:
        return path.read_bytes().decode("utf-8")
    except FileNotFoundError:
        fatal(path, f"{path} is missing")
    except UnicodeDecodeError as error:
        fatal(path, f"{path} is not readable as text: {error.reason}")


# The module requirement, in a require block or on a `require` line of its own, and whether or
# not go's tidy has marked it indirect: the whole line is one requirement, so a comment after the
# version is the only thing that may follow it.
requires = [
    (number, match.group(2))
    for number, line in enumerate(read(GOMOD).splitlines(), start=1)
    if (match := re.fullmatch(r"\s*(require\s+)?github\.com/zricethezav/gitleaks/v8\s+(\S+)\s*(//.*)?", line))
]
if len(requires) != 1:
    fatal(
        GOMOD,
        f"{GOMOD} must require github.com/zricethezav/gitleaks/v8 exactly once, as vX.Y.Z; "
        f"it does {len(requires)} times",
    )
module_line, required = requires[0]
module = re.fullmatch(r"v(\d+\.\d+\.\d+)", required)
if not module:
    fatal(
        GOMOD,
        f"{GOMOD}:{module_line}: github.com/zricethezav/gitleaks/v8 {required} is no exact "
        f"gitleaks release; write vX.Y.Z",
    )

# The release lane's own engine, named by the asset it fetches. A comment naming the version is
# not an assignment, so only the `leak_engine_url=` line counts.
pins = [
    (number, match.group(1))
    for number, line in enumerate(read(RELEASE).splitlines(), start=1)
    if (match := re.fullmatch(r"\s*leak_engine_url=(\S*)\s*", line))
]
if len(pins) != 1:
    fatal(
        RELEASE,
        f"{RELEASE} must set leak_engine_url exactly once, to {ASSET}; it does {len(pins)} times",
    )
asset_line, url = pins[0]
asset = re.fullmatch(
    r"https://github\.com/gitleaks/gitleaks/releases/download/v(\d+\.\d+\.\d+)"
    r"/gitleaks_(\d+\.\d+\.\d+)_linux_x64\.tar\.gz",
    url,
)
if not asset:
    fatal(RELEASE, f"{RELEASE}:{asset_line}: leak_engine_url={url} is no exact gitleaks release asset; write {ASSET}")
tag, file_name = asset.group(1), asset.group(2)
if tag != file_name:
    fatal(
        RELEASE,
        f"{RELEASE}:{asset_line}: leak_engine_url={url} fetches the file of gitleaks {file_name} "
        f"from the release of gitleaks {tag}; one release's tag and file name are the same version",
    )

if tag != module.group(1):
    fatal(
        RELEASE,
        f"{RELEASE}:{asset_line} fetches gitleaks {tag}, and {GOMOD}:{module_line} requires "
        f"gitleaks {module.group(1)}: move both to one version, with leak_engine_sha256 taken "
        f"from that release's own gitleaks_{module.group(1)}_checksums.txt",
    )
print(f"the release lane fetches gitleaks {tag}, the version {GOMOD} requires for the leak guard")
PY
