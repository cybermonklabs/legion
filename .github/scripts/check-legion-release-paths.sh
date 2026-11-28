#!/usr/bin/env bash
# release.yaml's version step for the Go `legion` reads only the paths $LEGION_VERSION_ARGS names
# after its `--`: a commit that changes none of them releases no `legion`, and its release archive
# and the tmux daemon's binary keep the code before it. Those paths are written by hand
# (.github/ci-notes/release.md, "The Go legion's version arguments and the command that prints its
# envoy packages"), and a package the binary comes to link through packages/daemon's `replace` on
# ../envoy is one a hand forgets: packages/envoy/programfile, the writer of every program file the
# daemon runs, was linked for a whole pull request before anyone saw it missing.
#
# So this check holds the list to what the release builds legion from. It asks each question a
# program answers for the release of that program, and holds the one rule no program answers:
# how GitHub Actions runs the step that reads the list.
#   * The words of LEGION_VERSION_ARGS: release.yaml's step that runs `release-push.sh version
#     legion-v $LEGION_VERSION_ARGS` expands the variable unquoted, in bash, at the repository's
#     root, so bash splits it on the default IFS and expands each word that matches a path of the
#     checkout. This script reads the value from the workflow's env with a YAML parser and expands
#     it the same way, in bash, at the root, with IFS and globbing as bash starts them. That the
#     step reads it so is the rule it holds, from that step's text and GitHub Actions' scoping: it
#     fails when no step or more than one runs that command; when that step's `run:`, comment
#     lines and blank lines apart, is anything but the command and its `>> "$GITHUB_OUTPUT"`;
#     when a step of its job before it has a `run:`, whatever it runs, since GitHub gives a later
#     step what such a step writes to $GITHUB_ENV or $GITHUB_PATH under any name for them
#     (${GITHUB_ENV}, ${{ github.env }}); when its job's env or its own sets the variable; when
#     the workflow's, the job's or the step's env sets BASH_ENV, ENV, SHELLOPTS or BASHOPTS, the
#     names through which bash or sh reads a startup file or its options from the environment;
#     and when a working directory, or a shell other than bash, is set for it at the step, its
#     job's defaults or the workflow's. It does not read what an action an earlier step uses
#     writes. An IFS or GLOBIGNORE in an env is not refused: measured with bash 5.2.21, neither
#     changes how `bash -e` splits and expands the list.
#   * Which of those words are the paths: release-push.sh's own parse of `version`'s arguments,
#     which `release-push.sh version-paths` runs and prints.
#   * Which files those paths cover: git, which reads them as pathspecs when release-bump.sh runs
#     `git log ... -- <paths>`. `git ls-files -- <paths>` at the root lists the files they cover,
#     and `git ls-files` the files git tracks.
#   * Which targets legion is built for: `release-push.sh legion-targets`, which the build runs.
#   * With which environment and command: release-build-legion.sh, whose modes run what its build
#     runs.
#   * Which files a target's binary is built from: go. `go list -deps -f` over ./cmd/legion, run
#     through release-build-legion.sh list, names for each package the files it compiles or
#     embeds (GoFiles, CgoFiles, CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles, SwigFiles,
#     SwigCXXFiles, SysoFiles and EmbedFiles, in its directory) and its module's go.mod (GoMod),
#     beside which is the module's go.sum. `go list -m`, through release-build-legion.sh modules,
#     names the go.mod of each module the build's go.work uses, linked or not: each one's require
#     lines feed the one version selection the binary is built with. `go env GOWORK`, through
#     release-build-legion.sh env, names the go.work the build reads, beside which is its
#     go.work.sum: in workspace mode its go, godebug, use and replace lines set what the binary
#     builds with.
# A file of those that git does not track is named by no commit, so no path can release a change
# to it, and it is not required.
#
# It fails naming each package directory of this repository with a required file the paths do
# not cover, and each module or workspace file the paths do not cover. It fails, too, when
# release-push.sh legion-targets refuses the legion targets, when go list or go env fails for a
# target, when the go.work the build reads or a module it uses is outside
# the repository, when release.yaml has no LEGION_VERSION_ARGS or its step does not read the list
# as this script does (above), when release-push.sh refuses its words, when git cannot list the
# files, and when go list printed no package of this repository: a check that compared nothing
# would read the same as one that found nothing missing.
#
# Run from anywhere: .github/scripts/check-legion-release-paths.sh
# CI runs it in ci.yaml's lint job, which holds no credential, after setup-go and the go-notices
# restore, which it must not run before, when the go_notices filter matches; its tests run in
# ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# Every listing goes to python in a file, not in a variable of its environment: the legion binary
# links hundreds of packages, and their module-cache paths overran one variable's limit (128 KiB,
# "Argument list too long") on CI's runner.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
release=.github/workflows/release.yaml

# The targets the release builds legion for, one <os>/<arch> a line.
if ! .github/scripts/release-push.sh legion-targets >"$work/targets" 2>"$work/errors"; then
  echo "::error file=.github/go-release-targets.json::could not read the legion targets of .github/go-release-targets.json (release-push.sh legion-targets), so the packages the legion binary links are unknown:" >&2
  cat "$work/errors" >&2
  exit 1
fi
mapfile -t targets <"$work/targets"

# One line for each package go list prints that is not of the standard library: its directory,
# its module's go.mod, and each file it compiles or embeds, separated by tabs. Every target's
# listing goes to the one file, and the go.work each target's build reads (a path, or empty for
# none found, or off for GOWORK=off) to another.
format='{{if not .Standard}}{{.Dir}}{{"\t"}}{{with .Module}}{{.GoMod}}{{end}}'
for field in GoFiles CgoFiles CFiles CXXFiles MFiles HFiles FFiles SFiles SwigFiles SwigCXXFiles SysoFiles EmbedFiles; do
  format+="{{range .$field}}{{\"\\t\"}}{{.}}{{end}}"
done
format+='{{"\n"}}{{end}}'
: >"$work/listed"
: >"$work/gowork"
: >"$work/modules"
for target in "${targets[@]}"; do
  if ! .github/scripts/release-build-legion.sh list "$target" -deps -f "$format" </dev/null >>"$work/listed" 2>"$work/errors"; then
    echo "::error file=packages/daemon/cmd/legion::go list -deps ./cmd/legion failed in packages/daemon, so the packages the legion binary links are unknown (target $target):" >&2
    cat "$work/errors" >&2
    exit 1
  fi
  if ! .github/scripts/release-build-legion.sh modules "$target" </dev/null >>"$work/modules" 2>"$work/errors"; then
    echo "::error file=packages/daemon/go.mod::go list -m failed in packages/daemon, so the modules the legion build selects versions with are unknown (target $target):" >&2
    cat "$work/errors" >&2
    exit 1
  fi
  if ! .github/scripts/release-build-legion.sh env "$target" GOWORK </dev/null >>"$work/gowork" 2>"$work/errors"; then
    echo "::error file=packages/daemon/go.mod::go env GOWORK failed in packages/daemon, so the go.work the legion build reads is unknown (target $target):" >&2
    cat "$work/errors" >&2
    exit 1
  fi
done

# LEGION_VERSION_ARGS as release.yaml's workflow env holds it, and the step that reads it, which
# must read it as this script does (the header's first point).
if ! arguments=$(python3 - "$release" <<'PY'
import sys
from pathlib import Path

import yaml

RELEASE = Path(sys.argv[1])
COMMAND = ".github/scripts/release-push.sh version legion-v $LEGION_VERSION_ARGS"
LINE = f'{COMMAND} >> "$GITHUB_OUTPUT"'
# The names through which bash or sh reads a startup file or its options from the environment.
STARTUP = ("BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS")


def fatal(message: str) -> None:
    print(f"::error file={RELEASE}::{message}", file=sys.stderr)
    raise SystemExit(1)


try:
    document = yaml.safe_load(RELEASE.read_text(encoding="utf-8"))
except FileNotFoundError:
    fatal(f"{RELEASE} is missing")
document = document or {}
arguments = (document.get("env") or {}).get("LEGION_VERSION_ARGS")
if not isinstance(arguments, str):
    fatal(f"{RELEASE} has no env LEGION_VERSION_ARGS, the paths whose change releases the Go legion")
found = [(name, job or {}, index) for name, job in (document.get("jobs") or {}).items()
         for index, step in enumerate((job or {}).get("steps") or []) if COMMAND in str(step.get("run", ""))]
if len(found) != 1:
    fatal(f"{RELEASE} has {len(found)} steps that run `{COMMAND}`, want one: this check expands the list as that step's bash does")
name, job, index = found[0]
step = job["steps"][index]


def commands(step: dict) -> list:
    """The lines of step's run: that bash runs: each one stripped, with blank lines and comment
    lines left out."""
    lines = (line.strip() for line in str(step.get("run", "")).splitlines())
    return [line for line in lines if line and not line.startswith("#")]


def run_default(scope: dict, key: str):
    return ((scope.get("defaults") or {}).get("run") or {}).get(key)


def label(step: dict, at: int) -> str:
    return str(step.get("name") or step.get("id") or step.get("uses") or f"number {at + 1}")


problems = []
if (run := commands(step)) != [LINE]:
    problems.append(f"runs {' and '.join(f'`{line}`' for line in run)}, not `{LINE}` alone")
problems += [f'comes after the step "{label(earlier, at)}", which runs a command'
             for at, earlier in enumerate(job["steps"][:index]) if "run" in earlier]
envs = (("the workflow's env", document.get("env")), ("its job's env", job.get("env")), ("its env", step.get("env")))
problems += [f"sets LEGION_VERSION_ARGS in {where}" for where, env in envs[1:]
             if isinstance(env, dict) and "LEGION_VERSION_ARGS" in env]
problems += [f"sets {variable} in {where}" for where, env in envs if isinstance(env, dict)
             for variable in STARTUP if variable in env]
problems += [f"sets {where} ({value})" for where, value in (
    ("its working-directory", step.get("working-directory")),
    ("its job's default working-directory", run_default(job, "working-directory")),
    ("the workflow's default working-directory", run_default(document, "working-directory")))
    if value not in (None, "", ".")]
shell = step.get("shell") or run_default(job, "shell") or run_default(document, "shell")
if shell not in (None, "bash"):
    problems.append(f"runs in {shell}")
if problems:
    fatal(f"{RELEASE}'s legion version step (job {name}) {'; '.join(problems)}, so it reads another list than the one "
          "this check expands from the workflow's env, in bash, at the root")
sys.stdout.write(arguments)
PY
); then
  exit 1
fi

# The paths release-push.sh's version reads from the words release.yaml's step gives it: the
# variable unquoted, so split and expanded here as there.
# shellcheck disable=SC2086 # $arguments is the argument list, split and expanded on purpose
if ! .github/scripts/release-push.sh version-paths legion-v $arguments >"$work/paths" 2>"$work/errors"; then
  echo "::error file=$release::release-push.sh refused $release's LEGION_VERSION_ARGS, as the release's version step would, so the paths whose change releases the Go legion are unknown:" >&2
  cat "$work/errors" >&2
  exit 1
fi
mapfile -d '' -t paths <"$work/paths"

# The files the paths cover, and the files git tracks, each ended by a NUL. With no path at all,
# git ls-files lists every file, as git log with none reads every commit.
if ! git ls-files -z -- "${paths[@]}" >"$work/covered" 2>"$work/errors" ||
  ! git ls-files -z >"$work/tracked" 2>>"$work/errors"; then
  echo "::error::git ls-files failed at the repository's root, so the files LEGION_VERSION_ARGS covers are unknown:" >&2
  cat "$work/errors" >&2
  exit 1
fi

python3 - "$work/listed" "$work/gowork" "$work/tracked" "$work/covered" "$work/modules" "${#targets[@]}" <<'PY'
import sys
from pathlib import Path, PurePosixPath

LISTED, GOWORK, TRACKED, COVERED, MODULES = (Path(arg) for arg in sys.argv[1:6])
TARGETS = int(sys.argv[6])
RELEASE = ".github/workflows/release.yaml"
ROOT = Path.cwd().resolve()


def fatal(message: str) -> None:
    print(f"::error file={RELEASE}::{message}", file=sys.stderr)
    raise SystemExit(1)


def text(path: Path) -> str:
    return path.read_bytes().decode("utf-8", "surrogateescape")


def ours(directory: str):
    """directory relative to the repository's root, or None for one outside it."""
    try:
        return Path(directory).resolve().relative_to(ROOT).as_posix()
    except ValueError:
        return None


def file_of(path: str):
    """path, a file, relative to the repository's root, or None for one outside it: its directory
    resolved, its own name kept, as git names it."""
    path = Path(path)
    directory = ours(str(path.parent))
    return None if directory is None else PurePosixPath(directory, path.name).as_posix()


# Each package of this repository, with the files of it the build reads, and the go.mod of each
# module they belong to.
packages, modules = {}, set()
for line in text(LISTED).splitlines():
    if not line.strip():
        continue
    directory, gomod, *files = line.split("\t")
    if (package := ours(directory)) is None:
        continue
    packages.setdefault(package, set()).update(PurePosixPath(package, name).as_posix() for name in files)
    if gomod and (module := file_of(gomod)) is not None:
        modules.add(module)
if not packages:
    fatal("go list -deps ./cmd/legion in packages/daemon printed no package of this repository, so this check compared nothing")
# The go.mod of each module the build's go.work uses, a package of it linked or not.
for gomod in sorted(set(text(MODULES).splitlines()) - {""}):
    if (module := file_of(gomod)) is None:
        fatal(f"the module {gomod} the legion build's go.work uses is outside the repository, so no path of "
              "LEGION_VERSION_ARGS can name its go.mod and a change to it alone would release no legion")
    modules.add(module)

tracked = set(text(TRACKED).split("\0")) - {""}
covered = set(text(COVERED).split("\0")) - {""}

missing = sorted(f"{package}/" for package, files in packages.items()
                 if any(path in tracked and path not in covered for path in files))
for gomod in sorted(modules):
    for path in (gomod, PurePosixPath(gomod).with_name("go.sum").as_posix()):
        if path in tracked and path not in covered:
            missing.append(path)
# The go.work the build reads binds the modules: in workspace mode its go, godebug, use and
# replace lines set what the binary builds with, and its go.work.sum holds the sums.
workspace = []
for gowork in sorted(set(text(GOWORK).splitlines()) - {"", "off"}):
    if (path := file_of(gowork)) is None:
        fatal(f"the go.work the legion build reads, {gowork}, is outside the repository, so no path of LEGION_VERSION_ARGS "
              "can name it and a change to it alone would release no legion")
    workspace += [name for name in (path, f"{path}.sum") if name in tracked and name not in workspace]
missing += [path for path in workspace if path not in covered]
if missing:
    fatal(
        f"{RELEASE}'s LEGION_VERSION_ARGS names no path that covers {len(missing)} of what the legion binary is built "
        f"from, so a change to it alone would release no legion: "
        + ", ".join(missing)
        + ". Add each after the `--` (.github/ci-notes/release.md, \"The Go legion's version arguments and the command "
        "that prints its envoy packages\")."
    )
bound = f", and {' and '.join(workspace)}, which {'bind' if len(workspace) > 1 else 'binds'} them" if workspace else ""
print(
    f"{RELEASE}'s LEGION_VERSION_ARGS covers every tracked file that the {len(packages)} packages of this repository "
    f"go list -deps ./cmd/legion prints for the {TARGETS} legion targets of .github/go-release-targets.json compile or "
    f"embed, and the go.mod and go.sum of the {len(modules)} modules the build uses{bound}"
)
PY
