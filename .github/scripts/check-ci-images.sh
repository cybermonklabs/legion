#!/usr/bin/env bash
# A GitHub-hosted runner shares one anonymous pull quota with every other runner on its address,
# so a CI job that fetches an image from a third-party registry fails for a reason the change
# under test did not cause. Every image a CI job of this repository fetches is therefore one
# image in one package this repository publishes, ghcr.io/cybermonklabs/legion/ci, and every
# reference names it by digest. .github/ci-images.yaml holds one row per image, and a row is
# one of two kinds. A row carrying `upstream:` is a copy of a third-party image:
# .github/scripts/mirror-ci-images.sh makes it with `crane copy`, which keeps the manifest
# digest. A row carrying `built:` is an image this repository builds itself, from the
# Dockerfile that key names, and .github/workflows/browser-image.yaml builds and pushes it. A
# row carries exactly one of the two keys, because each of them says where the row's bytes came
# from. ci.yaml's ci-images job runs the copy script and then the package mode of this check,
# in that order. That job is the one copy path, and it runs on every event ci.yaml runs on: a
# pull request, a merge group and a dispatch. This check holds the two sides to each other.
#
# THE TWO MODES. This check takes one mode argument, and runs that half alone. A call that names
# no mode, or that names more than one, fails naming both.
#
#   tree     every refusal that reads the files of this tree alone. It makes no network read, so
#            ci.yaml's lint job runs it, and lint runs on every non-draft run. A change that
#            names a third-party registry again can touch any file of the tree, and an image
#            filter names few of those files, so this half must read every change.
#   package  the two refusals that read the package, over the network. ci.yaml's ci-images job
#            runs them, after the copy of that same job. The ci_images filter selects that job
#            and names every file that decides what the copy and these reads do:
#            .github/ci-images.yaml, the two scripts, .github/scripts/ci_images.py (the
#            reference grammar both read a row with), .github/docker/browser.Dockerfile (rule
#            10 hashes it), .github/actions/install-crane and ci.yaml itself. The package can
#            also change under a run that touches none of them, and a re-run reads it again.
#
# Both modes read .github/ci-images.yaml, because the package mode asks ghcr.io about each row's
# digest, and both read the Dockerfile a `built` row names: the tree mode to see that this tree
# holds it and that it is the file the build workflow builds, the package mode to hash it. The
# package mode leaves the shape of a row to the tree mode, which runs on every change.
#
# It fails, naming the file, the line and the remedy, on:
#
# IN TREE MODE:
#
#   1. a row this check cannot read: a row that is not a mapping, a row carrying both
#      `upstream` and `built` or neither of them, a missing or empty `upstream`, `built`, `tag`
#      or `digest`, a `built` naming a file this tree does not hold, or a digest that is not
#      `sha256:` and 64 hex characters. A tag is required because an untagged manifest is a
#      package version nothing names, and a retention policy may delete a digest a job pins.
#   2. two rows with the same digest, two rows with the same tag, or a tag that is not the one
#      its own row gives it. The row is what says which image a digest is — which upstream it
#      copies and which version that upstream carries, or which Dockerfile of this repository
#      builds it — so one digest names one row and one row names one tag. The digest in the tag
#      is what makes one tag name one digest: a row that moves to a new digest takes a new tag
#      with it, the digest it leaves keeps the tag it had, and no copy moves a tag off a digest
#      a consumer pins. The tag an `upstream` row must carry is the upstream repository's last
#      path element, then the upstream tag when the row names one, then the first eight hex
#      characters of the digest. The tag a `built` row must carry is its Dockerfile's name
#      without the `.Dockerfile` suffix, then those same eight characters, so
#      .github/docker/browser.Dockerfile at sha256:1a2b3c4d… is browser-1a2b3c4d. This check
#      reads the whole string, not the eight characters alone.
#   3. a version this repository pins elsewhere that no row carries: no `oven/bun` row at the
#      version `.bun-version` locks, with or without the `-slim` suffix, or no `library/golang`
#      row at `ARG GO_VERSION` of packages/daemon/docker/worker.Dockerfile. A row for another
#      version is admitted, so raising Bun or Go is two commits: the new version's row first,
#      which ci.yaml's ci-images job copies, then the pin and every FROM. One commit holding
#      both would let a consumer job pull the new digest while that same run's copy is still
#      landing it. Rule 7 below is what keeps a Go FROM on the row the ARG names. This rule and
#      rule 7 read a row's repository with Docker Hub's implicit `library/` written out, so
#      `docker.io/golang:1.27.1-alpine` and `docker.io/library/golang:1.27.1-alpine` are one
#      repository to both of them.
#   4. a reference in CI's reach that names a registry other than ghcr.io/cybermonklabs/legion/ci,
#   5. a reference in CI's reach that is not pinned by digest (`@sha256:` and 64 hex characters):
#      a tag is a name the registry can move, so only a digest names the bytes a job ran,
#   6. a reference in CI's reach whose digest no row of .github/ci-images.yaml holds,
#   7. a reference in CI's reach whose row copies library/golang at a version other than
#      ARG GO_VERSION of packages/daemon/docker/worker.Dockerfile. Rule 3 asks for one row at
#      that version and admits a row beside it, so without this rule a FROM could name the
#      other row: that stage would build with the other Go, and the worker's toolchain stage
#      downloads the ARG's. A bump's second commit moves the ARG and every FROM together, so it
#      passes. .github/scripts/check-bun-version.sh holds a Bun FROM to .bun-version this way.
#   8. a `built` row naming a Dockerfile other than .github/docker/browser.Dockerfile.
#      .github/workflows/browser-image.yaml builds that one file and no other, so a row built
#      from anything else is a row nothing builds and nothing pushes: no image of the package
#      was ever pushed from it, and rule 10 below would hold the row to the hash of a build
#      recipe no build ever ran. Rule 10 does refuse such a row, in the package mode and over
#      the network, including one naming another file of .github/docker, because the recipe
#      hash folds in the Dockerfile's own path and not only the files of the directory it
#      sits in (.github/scripts/ci_images.py's recipe_sha256): without that path two rows
#      naming two Dockerfiles of one directory would hash alike, and the second would pass
#      rule 10 on the first one's published label. This rule is that same refusal made in
#      lint, which reads the files of this tree alone and runs on every change, so a row
#      nothing builds is refused where it is written rather than in the one job that reads
#      the package. A second image this repository builds is a second build workflow, and
#      this rule and rule 10 would then name the Dockerfile each of them builds.
#
# IN PACKAGE MODE:
#
#   9. a row whose digest the package does not hold: the manifest is read with an anonymous
#      `HEAD` against ghcr.io. A `built` row is in the package like any other, because the
#      build pushes it there. The message of an `upstream` row names .github/ci-images.yaml and
#      says that ci.yaml's ci-images job copies a row before this mode reads the package, so
#      the usual remedy is to merge the row and let that job run. The message of a `built` row
#      names its Dockerfile and .github/workflows/browser-image.yaml instead: that build runs
#      in the push that adds the Dockerfile, and the row carrying the digest it pushed lands in
#      a later push.
#  10. a `built` row whose published image was built from another build recipe than the one
#      this tree holds. THE RECIPE IS NOT THE DOCKERFILE ALONE: it is the row's own Dockerfile
#      path, every file of the build context directory that Dockerfile sits in, and
#      .github/workflows/browser-image.yaml itself, whose `labels:`, `outputs:`, `platforms:`,
#      `provenance:`, build-args and action pin decide the bytes a build pushes as surely as a
#      `RUN` line does. One function hashes it, .github/scripts/ci_images.py's recipe_sha256;
#      that workflow stamps the hash into the image it pushes, as the label
#      legion.browser-image.source-sha256, and this rule reads the label back out of the image
#      at the row's own digest and holds it equal to the hash of the recipe this tree holds.
#      Without it an edit to the recipe merges green and changes nothing CI runs: the row still
#      names a digest the package holds, so rule 9 passes, the tag still matches that digest,
#      so rule 2 passes, and every `shard` and `alone` leg keeps running the image built before
#      the edit. The tree mode cannot make this comparison, because the label is on the
#      published image and only a read of the registry can see it.
#      The remedy is the build: let .github/workflows/browser-image.yaml push the image this
#      recipe makes, then move the row's digest onto what it pushed.
#      What this rule does NOT prove is where the published bytes came from: the label is
#      written by the run that published, and on a pull request that run is the pull request's
#      own copy of that workflow. It holds a row to an image some run stamped this recipe's
#      hash into, which catches a row left behind at an old digest; the attestation's own
#      certificate is what names the commit a build ran at, and AGENTS.md says what a reviewer
#      reads by hand when a `built` row's digest moves.
#
# CI'S REACH is every .github/workflows/*.yaml, every .github/actions/*/action.yml, and each
# other file a CI job runs that names an image. The first two kinds come from those two globs;
# REACH below is the explicit list of the other files, each entry a file and never a directory.
# A REACH entry that is gone fails this check rather than silently checking nothing. A
# .github/workflows/*.yml, or a .github/actions/*/action.yaml, fails too: this check reads the
# two spellings the repository uses, and a third would be in no list.
#
# A REFERENCE in those files is one of:
#   * a job's `services:` or `container:` image, read from the parsed YAML, never grepped;
#   * a buildx `driver-opts` `image=` value;
#   * a `FROM` argument, continuations joined, with each `ARG`'s default substituted into it.
#     Docker builds `FROM` the default when no `--build-arg` overrides it, so the default is the
#     image the build fetches. A `FROM` naming an earlier stage of the same Dockerfile is not a
#     reference. A `FROM` on a variable the Dockerfile declares with no default anywhere is not
#     one either: whoever runs the build passes that value, so no image can be read here;
#   * the image a `docker pull`, `docker run` or `docker create` fetches, in a `run:` script or a
#     shell file. Every literal reference the command holds is read. A command holding none must
#     name a variable this check can follow: one the same file assigns a literal reference to, or
#     one IMAGE_FROM lists with where its value comes from and why that source is already checked.
#     A command naming neither is refused, because an image this check cannot read is an image no
#     row has to hold;
#   * a string of a Go or a TypeScript source, or a shell assignment, whose value reads as a
#     reference. Every string, not only a `const`: nats-stream.test.ts writes its image as one
#     argument of a `docker("run", …)` call. reads_as_reference says which values are read as
#     one and which written-alike things (a Go import path, a TypeScript module specifier, a
#     host and port) are not.
# A line this check cannot read is a refusal that names it, never a silent pass.
#
# THE EXCEPTION LIST. Besides the reach, this check sweeps every text file of the repository and
# fails on any file that names mirror.gcr.io, the registry CI used before, unless EXCEPTIONS
# names that file. Each entry carries exactly one ground: `no CI job runs it`, or `a CI job runs
# it, and it fetches nothing`. An entry on a file that no longer names mirror.gcr.io fails too:
# a ground that holds nothing any more is a ground nobody reads again. An entry naming a file
# the tree does not hold admits nothing, and is left alone. The sweep walks the tree with
# workflow_paths.walk, which skips .git, .jj, node_modules, .venv and vendor. This sweep skips
# two more things itself: .legion (Legion's per-issue run data, which a working tree may still
# carry) and this check's own two files, whose text names the registry to find it.
#
# Files are discovered by walking the tree, not by asking git, so an unsnapshotted change cannot
# read green locally.
#
# CHECKS 9 AND 10 AND THE NETWORK. The package mode's read of the package is one anonymous token
# request (`GET https://ghcr.io/token?scope=repository:cybermonklabs/legion/ci:pull&service=ghcr.io`)
# and one `HEAD https://ghcr.io/v2/cybermonklabs/legion/ci/manifests/<digest>` per row, with an
# `Accept` naming the four index and manifest media types. Rule 10 adds, for each `built` row
# the package holds, one `crane config --platform linux/amd64
# ghcr.io/cybermonklabs/legion/ci@<digest>`, which answers the image config a label is written
# in. That is the read .github/scripts/mirror-ci-images.sh already makes for a row's Go
# version, with the crane .github/actions/install-crane puts on PATH in ci.yaml's ci-images
# job, before this mode runs there; one reader of an image config, so the two cannot disagree
# about which bytes of an index they read. The platform is named because the build pushes
# linux/amd64 alone, and a read that answered for whatever entry an index happened to carry
# would hold a row to the label of an image this repository never built. A read that fails for
# any reason — no crane on PATH, a refusal from the registry, an image carrying no label — is a
# refusal naming what failed, never a pass.
# CI_IMAGES_MANIFEST_HEAD is the seam both reads hang on: when it is set, this check runs
# `"$CI_IMAGES_MANIFEST_HEAD" <digest>` once per row instead of reaching ghcr.io or running
# crane, and reads what the command prints on stdout — the HTTP status code of the HEAD (200 =
# the package holds the digest, 404 = it does not), and after it the image's
# legion.browser-image.source-sha256 label. A stand-in that prints a status alone answers for
# the manifest and not for a label, and rule 10 refuses a `built` row answered that way rather
# than passing it over: a seam that could turn a rule off is a seam that hides what the rule
# would have said. check-ci-images.test.sh sets the seam to a small script of its own, which
# prints the label its rule 10 cases drive the comparison with, and mirror-ci-images.test.sh
# drives rule 9 with it over the repository's own rows, so no case of either test reaches the
# network.
#
# Run from anywhere: .github/scripts/check-ci-images.sh tree
#                    .github/scripts/check-ci-images.sh package
# CI runs the tree mode in ci.yaml's lint job, and the package mode in ci.yaml's ci-images job,
# after that job's copy. Its tests run in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The mode is required, and names what it runs. A call with no mode, or with more than one word,
# fails here: a caller that says neither cannot be given one half of this check by default.
case "$#:${1:-}" in
  1:tree | 1:package) ;;
  *)
    echo "::error file=.github/scripts/check-ci-images.sh::this check takes one mode: \`tree\` reads the files of this tree and makes no network read, and \`package\` reads ghcr.io/cybermonklabs/legion/ci. Run \`.github/scripts/check-ci-images.sh tree\` or \`.github/scripts/check-ci-images.sh package\`." >&2
    exit 1
    ;;
esac
export CI_IMAGES_MODE="$1"

# The sweep's walk is workflow_paths.py's and the reference grammar is ci_images.py's, both
# beside this script in the repository it checks, which PYTHONPATH names.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import NamedTuple

import yaml

from ci_images import (
    BUILT,
    DIGEST,
    UPSTREAM,
    recipe_sha256,
    row_kind,
    row_tag,
    split_reference,
    upstream_parts,
)


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where it was written. The lines are attributes, not keys: a
    document is never searched for a key the loader invented."""

    line: int
    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.line = node.start_mark.line + 1
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)


def line_of(value: object) -> int:
    return getattr(value, "line", 0)


def key_line(mapping: object, name: str) -> int:
    return getattr(mapping, "lines", {}).get(name, line_of(mapping))


# ---------------------------------------------------------------- data
# The half this run checks. The wrapper above admits `tree` and `package` alone, and the rules
# below ask for the package half by name.
PACKAGE = "package"
MODE = os.environ["CI_IMAGES_MODE"]

REGISTRY = "ghcr.io/cybermonklabs/legion/ci"
REPOSITORY = "cybermonklabs/legion/ci"
CI_IMAGES = Path(".github/ci-images.yaml")
MIRROR_SCRIPT = ".github/scripts/mirror-ci-images.sh"
MIRROR_JOB = "ci.yaml's ci-images job"
# The workflow that publishes a row of the second kind. The two kinds themselves, and which one
# a row is, are .github/scripts/ci_images.py's UPSTREAM, BUILT and row_kind, which every reader
# of a row shares: a row carrying `upstream:` is a copy MIRROR_SCRIPT makes, and a row carrying
# `built:` is an image this repository builds from the Dockerfile that key names, which nothing
# copies.
BUILD_WORKFLOW = ".github/workflows/browser-image.yaml"
# The one Dockerfile BUILD_WORKFLOW builds, and the label that build writes the hash of its
# whole build recipe into — that Dockerfile's own path, the context directory it sits in, and
# BUILD_WORKFLOW itself (ci_images.py's recipe_sha256). Rule 8 holds every `built` row to that
# file, because a row naming another is a row no build pushes, which the hash says too: the
# path is folded into it, so a row naming another file of the context directory hashes to
# something other than the label the published image carries. Rule 10 reads that label back
# out of the image the row's digest names, and so is the only half of this check that can tell
# an edited recipe from the bytes the package serves.
BUILT_DOCKERFILE = ".github/docker/browser.Dockerfile"
SOURCE_LABEL = "legion.browser-image.source-sha256"
# How rule 10 reads an image's label: `crane config` answers the config a label is written in,
# and the platform is named because this repository's build pushes linux/amd64 alone.
# MIRROR_SCRIPT reads a row's Go version with this same call, and CRANE_ACTION is the step that
# puts crane on PATH in MIRROR_JOB, which runs the copy and then this mode.
CRANE_CONFIG = ("crane", "config", "--platform", "linux/amd64")
CRANE_ACTION = ".github/actions/install-crane"
BUN_PIN = Path(".bun-version")
WORKER_DOCKERFILE = Path("packages/daemon/docker/worker.Dockerfile")
# The two upstream repositories a pin of this repository names: rule 3 asks for a row at each
# pin, and rule 7 holds every reference on a GOLANG_REPOSITORY row to ARG GO_VERSION. Both are
# written as upstream_parts gives a repository, with Docker Hub's implicit `library/` spelled
# out.
BUN_REPOSITORY = "oven/bun"
GOLANG_REPOSITORY = "library/golang"
MIRROR = "mirror.gcr.io"
# The directories workflow_paths.walk skips (.git, .jj, node_modules, .venv, vendor) are every
# directory this sweep skips but one, and this set holds that one.
SWEEP_SKIP_DIRS = {".legion"}
SELF = {".github/scripts/check-ci-images.sh", ".github/scripts/check-ci-images.test.sh"}
ACCEPT = ",".join(
    (
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    )
)

# Every file a CI job runs that names a container image. Each entry is a file: a directory would
# grow a file this check never read.
REACH = (
    ".github/docker/browser.Dockerfile",
    "packages/envoy/docker/Dockerfile",
    "packages/envoy/infra/host/heldportwrapper/Dockerfile",
    "packages/daemon/docker/worker.Dockerfile",
    "packages/daemon/internal/testnats/nats.go",
    "packages/envoy/internal/testnats/nats.go",
    "packages/envoy/internal/testpg/testpg.go",
    "packages/envoy/internal/tests3/seaweedfs.go",
    "packages/envoy-client/src/__tests__/nats-server.ts",
    "scripts/e2e/lib/nats-stream.test.ts",
    "packages/pi-legion/scripts/skill-scenarios/rig.sh",
    "scripts/e2e/stage1-skeleton.sh",
)

# A docker command whose image is a variable, and where that variable's value comes from. The
# key is (file, variable name); the value is why the image it carries is checked elsewhere.
IMAGE_FROM = {
    (".github/actions/pull-test-images/action.yml", "image"): (
        "each image is read from a Go string constant the caller's `consts:` input names "
        "(packages/daemon/internal/testnats/nats.go, packages/envoy/internal/testnats/nats.go, "
        "packages/envoy/internal/testpg/testpg.go, packages/envoy/internal/tests3/seaweedfs.go), "
        "or comes from the `images:` input, which ci.yaml's envoy-smoke job fills with the FROM "
        "arguments of packages/envoy/docker/Dockerfile; every one of those files is in the reach "
        "above, and this check reads the references in them"
    ),
    (".github/workflows/ci.yaml", "ENVOY_SMOKE_IMAGE"): (
        "the local tag the envoy-smoke job's own build loaded (ci.yaml sets it to "
        "legion-envoy-smoke:<sha>); no registry holds it, so the run fetches nothing"
    ),
}

NO_CI_JOB = "no CI job runs it"
FETCHES_NOTHING = "a CI job runs it, and it fetches nothing"

# Every file outside the reach that may name mirror.gcr.io, and the one ground that admits it.
# `no CI job runs it`: the devbox live proofs and their libraries, the compose files, the
# walkthrough recorder, the corpus copy a developer restores on a devbox, and documentation.
# `a CI job runs it, and it fetches nothing`: a script a CI job runs whose mirror.gcr.io text no
# job ever hands to a registry — dev-postgres.test.sh, which the envoy-go job runs, installs a
# fake `docker` on PATH; the load test's cluster copy (packages/envoy/loadtest/cluster/), whose
# cluster.test.sh the envoy-go job runs with a fake `kubectl` on PATH, sources cluster.sh,
# templates nats.yaml and runs render-nats.sh --pin-from, which takes each digest from the
# committed nats.yaml; the images are pulled only by a cluster an operator points cluster.sh at.
EXCEPTIONS = {
    "AGENTS.md": NO_CI_JOB,
    "docs/kubernetes.md": NO_CI_JOB,
    "docs/plans/2026-10-14-dispatch-gcp-plan.md": NO_CI_JOB,
    "docs/plans/2026-10-14-envoy-infra-plan.md": NO_CI_JOB,
    "docs/solutions/github/an-image-pull-that-fails-in-ci-is-this-repositorys-own-package-read-the-failure-do-not-re-run-pr2512.md": NO_CI_JOB,
    "docs/site/media/walkthroughs/legion-operator/record-casts.sh": NO_CI_JOB,
    "docs/solutions/testing/a-docker-run-line-is-proven-from-a-pod-with-no-container-runtime-as-a-two-container-job-whose-client-ships-in-the-image-pr2498.md": NO_CI_JOB,
    "packages/envoy/deploy/compose/dispatch.acceptance.compose.yml": NO_CI_JOB,
    "packages/envoy/deploy/compose/single-node.compose.yml": NO_CI_JOB,
    "packages/envoy/scripts/corpus-copy.sh": NO_CI_JOB,
    "scripts/e2e/README.md": NO_CI_JOB,
    "scripts/e2e/burstable-kind.sh": NO_CI_JOB,
    "scripts/e2e/claude-backend-sandbox.sh": NO_CI_JOB,
    "scripts/e2e/claude-backend-tmux.sh": NO_CI_JOB,
    "scripts/e2e/controller-start-tmux.sh": NO_CI_JOB,
    "scripts/e2e/controller-sweep-kind.sh": NO_CI_JOB,
    "scripts/e2e/dispatch-user-turns.sh": NO_CI_JOB,
    "scripts/e2e/lib/bridge-tunnel.sh": NO_CI_JOB,
    "scripts/e2e/lib/kind-sandbox.sh": NO_CI_JOB,
    "scripts/e2e/parked-decision-devbox.sh": NO_CI_JOB,
    "scripts/e2e/share-volumes-kind.sh": NO_CI_JOB,
    "scripts/e2e/stage2-tmux-supervision.sh": NO_CI_JOB,
    "scripts/e2e/stage3-devbox-workflow.sh": NO_CI_JOB,
    "scripts/e2e/stage4a-sandbox-runtime.sh": NO_CI_JOB,
    "scripts/e2e/stage4b-sandbox-tree.sh": NO_CI_JOB,
    "scripts/e2e/verifiers-staging-token.sh": NO_CI_JOB,
    "packages/daemon/internal/testpg/testdb/testdb.go": FETCHES_NOTHING,
    "packages/envoy/scripts/dev-postgres.sh": FETCHES_NOTHING,
    "packages/envoy/scripts/dev-postgres.test.sh": FETCHES_NOTHING,
    "packages/envoy/loadtest/cluster/cluster.sh": FETCHES_NOTHING,
    "packages/envoy/loadtest/cluster/cluster.test.sh": FETCHES_NOTHING,
    "packages/envoy/loadtest/cluster/manifests/nats.yaml": FETCHES_NOTHING,
    "packages/envoy/loadtest/cluster/render-nats.sh": FETCHES_NOTHING,
}

# ---------------------------------------------------------------- reference grammar
# The grammar of a reference, and the two readers of one, are ci_images.py's, imported above:
# mirror-ci-images.sh reads a row's upstream with the same two.
VARIABLE = re.compile(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?")
ASSIGNMENT = re.compile(r"^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(\S*)")
STRING = re.compile(r"\"([^\"\n\\]*)\"|'([^'\n\\]*)'|`([^`\n\\]*)`")
GO_VERSION_ARG = re.compile(r"^ARG\s+GO_VERSION=(\S+)\s*$")
FETCH_VERBS = {"pull", "run", "create"}

problems: list[str] = []
used_image_from: set[tuple[str, str]] = set()


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines: a block scalar in a workflow would
    otherwise reach the Actions log as its own `::stop-commands::` line."""
    return " ".join(str(value).split())


def fatal(message: str) -> None:
    """Nothing downstream can be judged without the rows, so stop here."""
    print(f"::error file={CI_IMAGES}::{message}", file=sys.stderr)
    raise SystemExit(1)


def reads_as_reference(text: str) -> bool:
    """A value this check reads as an image reference where its position does not already say it
    is one: a Go or TypeScript string, a shell assignment, a word of a docker command. Three
    shapes are read as one, and the reason each needs a rule of its own is that other things are
    written the same way:
      * anything carrying a digest. Nothing else is;
      * a registry or a namespace and a tag (`mirror.gcr.io/library/nats:2.10`, `oven/bun:1.4.4`).
        A Go import path (`github.com/owner/module`) is shaped like the untagged form, so a tag
        or a digest is required here;
      * one name and a tag (`nats:2.10`), where the tag starts with a digit or is `latest` and
        the name is not an address. A TypeScript module specifier (`node:crypto`, `bun:test`)
        and a host and port (`127.0.0.1:4222`) are written the same way, and the tag, which
        names a release, is what tells them apart."""
    parts = split_reference(text)
    if not parts:
        return False
    host, path, tag, digest = parts
    if digest:
        return True
    if not tag:
        return False
    if host or "/" in path:
        return True
    return (tag[0].isdigit() or tag == "latest") and not re.fullmatch(r"[0-9.]+", path)


def unquote(word: str) -> str:
    for quote in ('"', "'"):
        if len(word) > 1 and word.startswith(quote) and word.endswith(quote):
            return word[1:-1]
    return word


def read_text(path: Path) -> str | None:
    """A file's text, or None where it is not decodable as UTF-8."""
    try:
        return path.read_bytes().decode("utf-8")
    except UnicodeDecodeError:
        return None


def locate(lines: list[str], start: int, needle: str) -> int:
    """The first line at or after `start` (1-based) holding `needle`, else `start`. A problem in
    a `run:` script is reported where the reader has to edit, not at the step's first line."""
    for number in range(max(start, 1), len(lines) + 1):
        if needle in lines[number - 1]:
            return number
    return start


# ---------------------------------------------------------------- the rows
if not CI_IMAGES.is_file():
    fatal(f"{CI_IMAGES} is missing: it is the one place that says which image a digest is")
try:
    document = yaml.load(CI_IMAGES.read_text(), Loader=LineLoader)
except yaml.YAMLError as error:
    fatal(f"{CI_IMAGES} is not valid YAML: {one_line(error)}")
if not isinstance(document, dict):
    fatal(f"{CI_IMAGES} is not a mapping of `registry:` and `images:`")
if document.get("registry") != REGISTRY:
    fatal(
        f"{CI_IMAGES}:{key_line(document, 'registry')}: registry is "
        f"{one_line(document.get('registry'))}, and every consumer names {REGISTRY}. Rewrite the "
        f"row, or change this check and every reference with it."
    )
rows = document.get("images")
if not isinstance(rows, list) or not rows:
    fatal(f"{CI_IMAGES}:{key_line(document, 'images')}: images is empty, so nothing is checked")


class Row(NamedTuple):
    """One row of .github/ci-images.yaml this check could read: the line it is written on, and
    the four things the rules below read it for. `kind` is UPSTREAM or BUILT, and `source` is
    what that kind says the row's bytes came from: the upstream reference for an UPSTREAM row,
    and the Dockerfile this repository builds the image from for a BUILT one."""

    line: int
    kind: str
    source: str
    tag: str
    digest: str


readable: list[Row] = []
for row in rows:
    line = line_of(row) or key_line(document, "images")
    if not isinstance(row, dict):
        problems.append(
            f"{CI_IMAGES}:{line}: this row is not a mapping of upstream or built, tag, digest"
        )
        continue
    kind_and_source = row_kind(row)
    # Which kind a row is, is row_kind's; WHY a row is neither is this rule's, and the four
    # shapes it refuses are the four row_kind answers None for. The keys are read again here
    # for the message alone.
    if kind_and_source is None:
        upstream, built = row.get("upstream"), row.get("built")
        if upstream is not None and built is not None:
            problems.append(
                f"{CI_IMAGES}:{key_line(row, 'built')}: this row carries both `upstream:` and "
                f"`built:`, and a row is one or the other: a copy {MIRROR_SCRIPT} makes, or an "
                f"image this repository builds. A row with both says two different things about "
                f"where its bytes came from. Delete the key that is not this row's."
            )
        elif upstream is None and built is None:
            problems.append(
                f"{CI_IMAGES}:{line}: this row carries neither `upstream:` nor `built:`, and a "
                f"row is one or the other: `upstream:` is the image {MIRROR_SCRIPT} copies "
                f"from, and `built:` is the Dockerfile this repository builds the image from. "
                f"Give it one of the two."
            )
        elif built is not None:
            problems.append(
                f"{CI_IMAGES}:{key_line(row, 'built')}: this row's built is {one_line(built)}, "
                f"and it names the Dockerfile this repository builds the image from."
            )
        else:
            problems.append(
                f"{CI_IMAGES}:{key_line(row, 'upstream')}: this row has no upstream, and "
                f"{MIRROR_SCRIPT} copies from it"
            )
        continue
    kind, source = kind_and_source
    tag, digest = row.get("tag"), row.get("digest")
    if kind == BUILT and not Path(source).is_file():
        problems.append(
            f"{CI_IMAGES}:{key_line(row, 'built')}: this row is built from "
            f"{one_line(source)}, and this tree holds no such file, so nothing builds the "
            f"row. Name the Dockerfile {BUILD_WORKFLOW} builds, or delete the row."
        )
        continue
    if not isinstance(tag, str) or not tag.strip():
        rule = (
            "the upstream repository's last path element, then the upstream tag when this row "
            "names one, then the first eight hex characters of the digest"
            if kind == UPSTREAM
            else "the Dockerfile's name without the .Dockerfile suffix, then the first eight "
            "hex characters of the digest"
        )
        problems.append(
            f"{CI_IMAGES}:{key_line(row, 'tag')}: the row for {one_line(source)} has no tag. "
            f"An untagged manifest is a package version nothing names, and a retention policy "
            f"may delete a digest a job pins. Give it one: {rule}."
        )
        continue
    if not isinstance(digest, str) or not DIGEST.fullmatch(digest.strip()):
        read_it = (
            f"Read it with `crane digest {one_line(source)}`."
            if kind == UPSTREAM
            else f"It is the digest {BUILD_WORKFLOW} pushed when it built {one_line(source)}."
        )
        problems.append(
            f"{CI_IMAGES}:{key_line(row, 'digest')}: the row for {one_line(source)} has digest "
            f"{one_line(digest)}, which is not sha256: and 64 hex characters. {read_it}"
        )
        continue
    readable.append(Row(line, kind, source, tag.strip(), digest.strip()))


def verdict(what: str, summary: str) -> None:
    """Prints the problems this mode found, and its own summary line. Each mode ends here."""
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        print(f"::error::{len(problems)} problem(s) {what}", file=sys.stderr)
        raise SystemExit(1)
    print(summary)
    raise SystemExit(0)


# ---------------------------------------------------------------- rules 9 and 10: the package
class PackageRead(NamedTuple):
    """What this mode read about one row's digest. `status` is the HTTP status of the manifest
    HEAD rule 9 reads. `label` is the SOURCE_LABEL the image at that digest carries, which rule
    10 holds the row's build recipe to, and `trouble` says why a read that was made gave no
    label. For a `built` row the package holds, one of the two is always set: a read that gave
    no label is a read rule 10 refuses on, never one it passes over."""

    status: str
    label: str
    trouble: str


def source_label(digest: str) -> tuple[str, str]:
    """The SOURCE_LABEL the image at one digest carries, and, where it gives none, the clause
    that says why. An image's labels are written in its config, and `crane config` is what
    answers one: the read MIRROR_SCRIPT already makes for a row's Go version, under the crane
    CRANE_ACTION puts on PATH in the job that runs both. The platform is named because an
    index names one manifest per platform and this repository's build pushes linux/amd64
    alone, so reading whatever entry an index happens to carry would hold a row to the label
    of an image no build of this repository pushed."""
    reference = f"{REGISTRY}@{digest}"
    call = " ".join((*CRANE_CONFIG, reference))
    try:
        done = subprocess.run(
            [*CRANE_CONFIG, reference], capture_output=True, text=True, check=False, timeout=120
        )
    except Exception as error:  # noqa: BLE001 - every failure to read the package is a refusal
        return "", (
            f"`{call}` would not run ({type(error).__name__}: {one_line(error)}), and "
            f"{CRANE_ACTION} is what puts crane on PATH before this mode runs"
        )
    if done.returncode != 0:
        return "", (
            f"`{call}` exited {done.returncode} "
            f"({one_line(done.stderr) or 'saying nothing on stderr'})"
        )
    try:
        config = json.loads(done.stdout)
    except ValueError as error:
        return "", f"`{call}` answered what is not an image config ({one_line(error)})"
    labels = (config.get("config") or {}).get("Labels") if isinstance(config, dict) else None
    carried = (labels or {}).get(SOURCE_LABEL, "")
    if not carried:
        return "", f"the image at that digest carries no {SOURCE_LABEL} label"
    return one_line(carried), ""


def package_reads(rows: list[Row]) -> dict[str, PackageRead]:
    """What the package says about each row, by digest. CI_IMAGES_MANIFEST_HEAD stands in for
    both reads, so a test drives this mode without the network."""
    command = os.environ.get("CI_IMAGES_MANIFEST_HEAD", "")
    if command:
        out = {}
        for row in rows:
            done = subprocess.run(
                [command, row.digest], capture_output=True, text=True, check=False
            )
            # The stand-in prints the status, and after it the label the image at that digest
            # carries. A stand-in that prints a status alone has answered for the manifest and
            # not for the label, and that is trouble rather than silence: this seam stands in
            # for the whole of the package read, so a half-answer it passed over would turn
            # rule 10 off and no run would say so.
            words = done.stdout.split()
            label = words[1] if len(words) > 1 else ""
            out[row.digest] = PackageRead(
                words[0] if words else "no status",
                label,
                ""
                if label
                else (
                    f"the CI_IMAGES_MANIFEST_HEAD stand-in {one_line(command)} printed a "
                    f"status and no {SOURCE_LABEL} label after it, so it answered for the "
                    f"manifest and not for the image's label. Have it print that label after "
                    f"the status, or unset CI_IMAGES_MANIFEST_HEAD and let this check read the "
                    f"package itself"
                ),
            )
        return out
    try:
        with urllib.request.urlopen(
            f"https://ghcr.io/token?scope=repository:{REPOSITORY}:pull&service=ghcr.io",
            timeout=30,
        ) as answer:
            token = json.load(answer)["token"]
    except Exception as error:  # noqa: BLE001 - every failure to read the package is a refusal
        problems.append(
            f"{CI_IMAGES}:1: ghcr.io would not issue a pull token for {REPOSITORY} "
            f"({type(error).__name__}: {one_line(error)}), so this check cannot tell which rows "
            f"the package holds."
        )
        return {}
    out = {}
    for row in rows:
        request = urllib.request.Request(
            f"https://ghcr.io/v2/{REPOSITORY}/manifests/{row.digest}", method="HEAD"
        )
        request.add_header("Authorization", f"Bearer {token}")
        request.add_header("Accept", ACCEPT)
        try:
            with urllib.request.urlopen(request, timeout=30) as answer:
                status = str(answer.status)
        except urllib.error.HTTPError as error:
            status = str(error.code)
        except Exception as error:  # noqa: BLE001
            status = f"{type(error).__name__}: {one_line(error)}"
        # The label is read for a `built` row alone, and only where the package holds the
        # digest: rule 9 already names a digest the package lacks, and an image that is not
        # there has no config to read.
        label, trouble = "", ""
        if row.kind == BUILT and status == "200":
            label, trouble = source_label(row.digest)
        out[row.digest] = PackageRead(status, label, trouble)
    return out


if MODE == PACKAGE:
    # Rule 1, a row's own shape, is the tree mode's, and that mode reads every change. A row
    # this mode cannot read is one it asks ghcr.io nothing about, so this mode names the rows
    # the package lacks, and the built rows the package holds at other bytes than this tree's,
    # and nothing else.
    problems = []
    held = 0
    compared = 0
    if readable:
        reads = package_reads(readable)
        for line, kind, source, tag, digest in readable:
            read = reads.get(digest)
            if read is None:
                continue
            if read.status == "200":
                held += 1
            elif kind == UPSTREAM:
                problems.append(
                    f"{CI_IMAGES}:{line}: {REGISTRY}@{digest} ({tag}, the copy of "
                    f"{one_line(source)}) is not in the package: a manifest HEAD answered "
                    f"{read.status}. {MIRROR_JOB} copies a row of {CI_IMAGES} before this mode "
                    f"reads the package, so merge this row, or re-run that job, before a job "
                    f"pins the digest."
                )
            else:
                # A built row is in the package like any other, because the build pushes it
                # there; nothing copies it, so the remedy names the build and not the mirror.
                problems.append(
                    f"{CI_IMAGES}:{line}: {REGISTRY}@{digest} ({tag}, the image "
                    f"{BUILD_WORKFLOW} builds from {one_line(source)}) is not in the package: a "
                    f"manifest HEAD answered {read.status}. That build runs in the push that "
                    f"adds {one_line(source)}, and the row carrying the digest it pushed lands "
                    f"in a later push, so let that build run, or re-run it, before a job pins "
                    f"the digest."
                )
            # Rule 10: the published image carries the hash of the build recipe the row's
            # Dockerfile belongs to, because the build writes it there. Every `built` row the
            # package holds is compared, and a read that gave no label is refused below on its
            # `trouble`: a row this rule read nothing for is a row nothing held to a recipe.
            if kind != BUILT or read.status != "200":
                continue
            # Half of that recipe is BUILD_WORKFLOW's own text, so a tree without it is a tree
            # this rule cannot hash a row against, and saying so beats a traceback.
            if not Path(BUILD_WORKFLOW).is_file():
                problems.append(
                    f"{CI_IMAGES}:{line}: this tree holds no {BUILD_WORKFLOW}, and that "
                    f"workflow's own text is half of the build recipe this row's published "
                    f"image is held to, so there is nothing to compare its {SOURCE_LABEL} "
                    f"label with. Restore that workflow, or delete the row nothing builds."
                )
                continue
            recipe = Path(source).parent.as_posix()
            tree_sha256 = recipe_sha256(source, BUILD_WORKFLOW)
            if read.label == tree_sha256:
                compared += 1
                continue
            carried = read.trouble or (
                f"the image at that digest carries sha256 {read.label} in its {SOURCE_LABEL} "
                f"label"
            )
            problems.append(
                f"{CI_IMAGES}:{line}: {REGISTRY}@{digest} ({tag}) is not the image this tree's "
                f"build recipe makes: the name {one_line(source)}, every file of {recipe} and "
                f"{BUILD_WORKFLOW} hash to sha256 {tree_sha256} as this tree holds them, and "
                f"{carried}. "
                f"{BUILD_WORKFLOW} writes that recipe's own sha256 into the image it pushes, "
                f"as that label, so the two differ when a file of the recipe changed and no "
                f"build pushed the change: every job pinning this row would keep running the "
                f"image built before it. Let that build push the image this recipe makes, then "
                f"move this row's digest onto what it pushed."
            )
    verdict(
        f"between {CI_IMAGES} and the package",
        f"{CI_IMAGES} holds {len(readable)} row(s), {held} of them in the package, "
        f"{compared} built row(s) at the build recipe this tree holds",
    )


# Rule 2's tag is .github/scripts/ci_images.py's row_tag, the rule the build writes a tag with
# and this check holds a row to: one function, so no row can carry a tag the build gives it and
# this check refuses.


by_digest: dict[str, Row] = {}
by_tag: dict[str, Row] = {}
for row in readable:
    line, kind, source, tag, digest = row
    earlier = by_digest.get(digest)
    if earlier is not None:
        problems.append(
            f"{CI_IMAGES}:{line}: {digest} is also the digest of the row for "
            f"{one_line(earlier.source)} on line {earlier.line}. The row is what says which "
            f"image a digest is, so one digest names one row: delete one of them."
        )
    else:
        by_digest[digest] = row
    earlier = by_tag.get(tag)
    if earlier is not None:
        problems.append(
            f"{CI_IMAGES}:{line}: tag {tag} is also the tag of the row for "
            f"{one_line(earlier.source)} on line {earlier.line}, and the second copy would move "
            f"the tag off the first row's digest. Give one of them another tag."
        )
    else:
        by_tag[tag] = row
    wanted = row_tag(kind, source, digest)
    if tag != wanted:
        gives = "upstream" if kind == UPSTREAM else "Dockerfile"
        problems.append(
            f"{CI_IMAGES}:{line}: tag {tag} is not {wanted}, the tag this row's {gives} and "
            f"digest give it. The last eight characters are what keeps one tag on one digest. A "
            f"tag without them is a tag another digest of the same image can take, and the copy "
            f"that takes it moves the tag off the digest consumers pin. The rest names the image "
            f"a reader of the package sees. Name this row {wanted}."
        )


# Rule 8: a `built` row names the one Dockerfile BUILD_WORKFLOW builds. Rule 1 has already held
# `built` to a file this tree holds; this rule holds it to the file that workflow builds,
# because a row built from any other file is a row no build pushes — so rule 10, which reads
# the published image's recipe hash back out of the registry, would hold the row to a recipe
# no build ever ran, and refuse it there. That holds for a file of BUILT_DOCKERFILE's own
# directory as much as for one anywhere else, because recipe_sha256 folds the Dockerfile's
# path in beside the files of the context: were it the files alone, two rows naming two
# Dockerfiles of .github/docker would hash alike and rule 10 would pass the second on the
# first one's label. This rule is that refusal made in lint: the tree alone says which file
# BUILD_WORKFLOW builds, so the row is refused on every change rather than in the one job that
# reads the package. The path is compared as a path, which is how BUILD_WORKFLOW reads the
# same key when it looks its own row up.
for row in readable:
    if row.kind == BUILT and Path(row.source) != Path(BUILT_DOCKERFILE):
        problems.append(
            f"{CI_IMAGES}:{row.line}: this row is built from {one_line(row.source)}, and "
            f"{BUILD_WORKFLOW} builds {BUILT_DOCKERFILE} and no other file, so nothing builds "
            f"this row: no build ever pushed an image from {one_line(row.source)}, and rule 10 "
            f"refuses this row in the package mode, where it holds the published image to the "
            f"hash of a recipe no build ever ran — the hash folds in the Dockerfile's own "
            f"path, so a row naming another file of {BUILT_DOCKERFILE}'s own directory is "
            f"refused there too. This is that refusal in lint, which reads "
            f"this tree alone. Name {BUILT_DOCKERFILE}, or give {BUILD_WORKFLOW} a build for "
            f"{one_line(row.source)} and this check the rule that holds the row it pushes to "
            f"it."
        )


# Rule 3: a version this repository pins elsewhere that no row carries. One row carrying the pin
# is enough, and a row for another version is admitted, because a bump lands its row in the
# commit before the pin: ci.yaml's ci-images job copies the row, and the commit that moves the
# pin and the FROMs then pulls a digest the package already holds. Nothing loosens by admitting
# the extra row. Rule 6 refuses a FROM whose digest no row holds, rule 7 refuses a FROM on a
# library/golang row of any Go but the pinned one, and .github/scripts/check-bun-version.sh
# refuses a FROM on a row of any Bun but the pinned one.
if not BUN_PIN.is_file():
    fatal(f"{BUN_PIN} is missing: the oven/bun rows of {CI_IMAGES} carry the version it pins")
bun_pin = BUN_PIN.read_text().strip()
worker_text = read_text(WORKER_DOCKERFILE)
if worker_text is None:
    fatal(
        f"{WORKER_DOCKERFILE} is missing or is not readable as text: the library/golang row of "
        f"{CI_IMAGES} carries the version its ARG GO_VERSION pins"
    )
go_pin = ""
for text in worker_text.splitlines():
    found = GO_VERSION_ARG.match(text)
    if found:
        go_pin = found.group(1)
if not go_pin:
    fatal(f"{WORKER_DOCKERFILE} declares no ARG GO_VERSION=<version>, which a golang row copies")

carried_by: dict[str, list[str]] = {BUN_REPOSITORY: [], GOLANG_REPOSITORY: []}
for _, kind, source, _, _ in readable:
    # A built row's source is a Dockerfile of this repository and no upstream repository at
    # all, so it is neither the oven/bun row nor the library/golang row this rule asks for.
    if kind != UPSTREAM:
        continue
    _, repository, carried, _ = upstream_parts(source)
    if repository in carried_by:
        carried_by[repository].append(carried)
if not any(carried in {bun_pin, f"{bun_pin}-slim"} for carried in carried_by[BUN_REPOSITORY]):
    problems.append(
        f"{CI_IMAGES}:{key_line(document, 'images')}: no row copies oven/bun:{bun_pin}, and "
        f"{BUN_PIN} locks {bun_pin}. Add that row (the `-slim` suffix is a row of its own) and "
        f"name its digest, and {MIRROR_JOB} publishes it. Raising Bun is two commits: this row "
        f"first, then {BUN_PIN}, the ARG and every FROM."
    )
if not any(carried.partition("-")[0] == go_pin for carried in carried_by[GOLANG_REPOSITORY]):
    problems.append(
        f"{CI_IMAGES}:{key_line(document, 'images')}: no row copies library/golang at the "
        f"version {WORKER_DOCKERFILE} pins ARG GO_VERSION={go_pin}. Add the row "
        f"golang:{go_pin}-alpine and name its digest, and {MIRROR_JOB} publishes it. Raising Go "
        f"is two commits: this row first, then the ARG and every FROM."
    )

# ---------------------------------------------------------------- CI's reach
workflows = sorted(Path(".github/workflows").glob("*.yaml"))
actions = sorted(Path(".github/actions").glob("*/action.yml"))
for stray in sorted(Path(".github/workflows").glob("*.yml")) + sorted(
    Path(".github/actions").glob("*/action.yaml")
):
    problems.append(
        f"{stray}:1: CI's reach is .github/workflows/*.yaml and .github/actions/*/action.yml, so "
        f"this file is in no list and its images are checked by nothing. Rename it, or widen the "
        f"two globs in this check."
    )
reach: list[Path] = list(workflows) + list(actions)
for name in REACH:
    path = Path(name)
    if not path.is_file():
        problems.append(
            f"{name}:1: CI's reach names this file, and it is gone. Delete its line from REACH in "
            f"this check, or restore the file."
        )
        continue
    reach.append(path)
reach_names = {str(path) for path in reach}

references: list[tuple[str, int, str, str]] = []  # file, line, text, how


def note(path: Path, line: int, text: str, how: str) -> None:
    references.append((str(path), line, text, how))


def judge(path: str, line: int, text: str, how: str) -> None:
    """Rules 4, 5, 6 and 7: one reference, read against the rows."""
    parts = split_reference(text)
    if parts is None:
        problems.append(
            f"{path}:{line}: {how} is {one_line(text)}, which this check cannot read as an image "
            f"reference. Write {REGISTRY}@<digest>, naming a digest {CI_IMAGES} holds."
        )
        return
    host, repository, tag, digest = parts
    named = f"{host}/{repository}" if host else repository
    if named != REGISTRY:
        problems.append(
            f"{path}:{line}: {how} names {one_line(named)}, and every image a CI job fetches is "
            f"a copy in {REGISTRY}. Add a row for it to {CI_IMAGES}, which {MIRROR_JOB} "
            f"copies, and name that row's digest here."
        )
        return
    if not digest:
        problems.append(
            f"{path}:{line}: {how} is {one_line(text)}, which names "
            f"{'tag ' + tag if tag else 'no digest'}. A tag is a name the registry can move, so "
            f"name the digest instead: {REGISTRY}@<digest> from {CI_IMAGES}."
        )
        return
    row = by_digest.get(digest)
    if row is None:
        problems.append(
            f"{path}:{line}: {how} names a digest no row of {CI_IMAGES} holds, so nothing copies "
            f"it into the package. Add its row to that file, and {MIRROR_JOB} publishes it."
        )
        return
    # Rule 7. Rule 3 admits a library/golang row beside the one the ARG names, which is what a
    # bump's first commit lands, so the row a Go FROM names is read here against that ARG. A
    # built row is an image this repository builds and never a copy of library/golang, so it
    # carries no upstream tag to read a Go version out of.
    if row.kind != UPSTREAM:
        return
    _, row_repository, carried, _ = upstream_parts(row.source)
    if row_repository == GOLANG_REPOSITORY and carried.partition("-")[0] != go_pin:
        version = one_line(carried) if carried else "no version this check can read"
        problems.append(
            f"{path}:{line}: {how} names the row for {one_line(row.source)} on line {row.line} of "
            f"{CI_IMAGES}, which copies Go {version}, and {WORKER_DOCKERFILE} pins "
            f"ARG GO_VERSION={go_pin}. A stage that builds with another Go is not the Go the "
            f"worker's toolchain stage downloads, so name the row at {go_pin} here. Raising Go "
            f"is two commits: that row first, then the ARG and every FROM together."
        )


def shell_lines(text: str, first: int) -> list[tuple[int, str]]:
    """(line, command) for each logical line of a shell script: backslash continuations joined,
    a comment that starts a word dropped. `first` is the file line the text starts on."""
    out: list[tuple[int, str]] = []
    number = first
    pending = ""
    start = first
    for raw in text.splitlines():
        if not pending:
            start = number
        body = raw
        continued = False
        if body.endswith("\\") and not body.endswith("\\\\"):
            body, continued = body[:-1], True
        pending = f"{pending} {body}" if pending else body
        if not continued:
            out.append((start, pending))
            pending = ""
        number += 1
    if pending:
        out.append((start, pending))
    return [(line, strip_comment(command)) for line, command in out]


def strip_comment(command: str) -> str:
    quote = ""
    for index, character in enumerate(command):
        if quote:
            if character == quote:
                quote = ""
        elif character in "\"'":
            quote = character
        elif character == "#" and (index == 0 or command[index - 1].isspace()):
            return command[:index]
    return command


def fetches(words: list[str]) -> bool:
    """A command that fetches an image from a registry: a `docker` whose subcommand, the first
    word after it that is not an option, is `pull`, `run` or `create`. `docker buildx
    imagetools create` writes an index and `docker image inspect` reads the local daemon, so
    neither is one; what `docker buildx build` pulls is the FROM of its Dockerfile, which this
    check reads where that Dockerfile is in the reach. A global option that takes a separate
    value (`docker --context two run …`) would shift the subcommand, and no command in the reach
    has one."""
    for index, word in enumerate(words):
        if word != "docker" and not word.endswith("/docker"):
            continue
        for following in words[index + 1 :]:
            if following.startswith("-"):
                continue
            if following in FETCH_VERBS:
                return True
            break
    return False


def scan_shell(path: Path, text: str, first: int, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over shell text: every assignment that reads as a reference, and the
    image of every docker fetch."""
    commands = shell_lines(text, first)
    assigned: dict[str, str] = {}
    for _, command in commands:
        found = ASSIGNMENT.match(command.strip())
        if found:
            assigned[found.group(1)] = unquote(found.group(2))
    for line, command in commands:
        words = command.split()
        found = ASSIGNMENT.match(command.strip())
        if found and reads_as_reference(unquote(found.group(2))):
            value = unquote(found.group(2))
            note(path, locate(lines, line, value), value, f"{found.group(1)}=")
        if not fetches(words):
            continue
        literals = [
            unquote(word) for word in words if reads_as_reference(unquote(word))
        ]
        for value in literals:
            note(path, locate(lines, line, value), value, "the image this docker command fetches")
        if literals:
            continue
        names = set(VARIABLE.findall(command))
        followed = any(reads_as_reference(assigned.get(name, "")) for name in names)
        for name in sorted(names):
            if (str(path), name) in IMAGE_FROM:
                used_image_from.add((str(path), name))
                followed = True
        if not followed:
            problems.append(
                f"{path}:{line}: this docker command fetches an image this check cannot read "
                f"({one_line(command)}). Name the image, or add the variable that holds it to "
                f"IMAGE_FROM in this check, saying where its value comes from."
            )


def expand(text: str, defaults: dict[str, str]) -> str:
    """`text` with each ARG default substituted for its variable. The substitution repeats,
    because a default can name another ARG. The bound stops a default that names itself."""
    for _ in range(10):
        grown = VARIABLE.sub(lambda found: defaults.get(found.group(1), found.group(0)), text)
        if grown == text:
            break
        text = grown
    return text


def scan_dockerfile(path: Path, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over a Dockerfile: every FROM, read with the ARG defaults substituted
    into it. A FROM naming an earlier stage of the same Dockerfile is not a reference, and
    neither is a FROM on a variable the Dockerfile declares with no default anywhere."""
    defaults: dict[str, str] = {}
    declared: set[str] = set()
    stages: set[str] = set()
    for line, command in shell_lines("\n".join(lines), 1):
        words = command.split()
        if not words:
            continue
        verb = words[0].upper()
        if verb == "ARG" and len(words) > 1:
            name, equals, value = words[1].partition("=")
            declared.add(name)
            # A stage re-declares an ARG by name to carry the global default into that stage, so
            # a re-declaration with no `=` keeps the default the first declaration gave.
            if equals:
                defaults[name] = unquote(value)
            continue
        if verb != "FROM":
            continue
        rest = [word for word in words[1:] if not word.startswith("--")]
        if not rest:
            problems.append(
                f"{path}:{line}: this FROM names no image. Write {REGISTRY}@<digest>, naming a "
                f"digest {CI_IMAGES} holds."
            )
            continue
        image = rest[0]
        if len(rest) > 2 and rest[1].upper() == "AS":
            stages.add(rest[2].lower())
        if image.lower() in stages:
            continue
        if VARIABLE.search(image):
            resolved = expand(image, defaults)
            left = sorted(set(VARIABLE.findall(resolved)))
            unknown = [name for name in left if name not in declared]
            if unknown:
                problems.append(
                    f"{path}:{line}: this FROM reads {one_line(image)}, whose "
                    f"{', '.join(unknown)} this Dockerfile does not declare as an ARG, "
                    f"so the image it builds on cannot be read here."
                )
                continue
            if left:
                # Each variable left is one the Dockerfile declares with no default anywhere.
                # Whoever runs the build passes it with --build-arg, so this FROM names no image
                # to read (packages/envoy/infra/host/heldportwrapper/Dockerfile is that case).
                continue
            if resolved.lower() in stages:
                continue
            note(path, line, resolved, f"this FROM, which the ARG defaults make {resolved},")
            continue
        note(path, line, image, "this FROM")


def scan_source(path: Path, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over a Go or TypeScript source: every string literal whose value reads
    as a reference. Every spelling, not only a `const`: scripts/e2e/lib/nats-stream.test.ts
    writes its image as one argument of a `docker("run", …)` call."""
    for number, text in enumerate(lines, start=1):
        for found in STRING.finditer(text):
            value = next(group for group in found.groups() if group is not None)
            if reads_as_reference(value):
                note(path, number, value, "this string")


def scan_workflow(path: Path, lines: list[str]) -> None:
    """Rules 4, 5 and 6 over a workflow or a composite action: each job's runner and service
    images, each buildx driver-opts image=, and every run: script."""
    try:
        parsed = yaml.load("\n".join(lines), Loader=LineLoader)
    except yaml.YAMLError as error:
        problems.append(f"{path}:1: this file is not valid YAML: {one_line(error)}")
        return
    if not isinstance(parsed, dict):
        return
    holders = []
    jobs = parsed.get("jobs")
    if isinstance(jobs, dict):
        holders.extend(job for job in jobs.values() if isinstance(job, dict))
    runs = parsed.get("runs")
    if isinstance(runs, dict):
        holders.append(runs)
    for holder in holders:
        images: list[tuple[int, object, str]] = []
        container = holder.get("container")
        how = "this job's container image"
        if isinstance(container, dict):
            images.append((key_line(container, "image"), container.get("image"), how))
        elif container is not None:
            images.append((key_line(holder, "container"), container, how))
        services = holder.get("services")
        if isinstance(services, dict):
            for name, service in services.items():
                how = f"the {name} service's image"
                if isinstance(service, dict):
                    images.append((key_line(service, "image"), service.get("image"), how))
                else:
                    images.append((key_line(services, name), service, how))
        for line, image, how in images:
            if not isinstance(image, str) or "${{" in image:
                problems.append(
                    f"{path}:{line}: {how} is {one_line(image)}, which this check cannot read. "
                    f"Write {REGISTRY}@<digest>, naming a digest {CI_IMAGES} holds."
                )
                continue
            note(path, line, image, how)
        for step in holder.get("steps", []) or []:
            if not isinstance(step, dict):
                continue
            with_ = step.get("with")
            if isinstance(with_, dict):
                options = with_.get("driver-opts")
                values = options if isinstance(options, list) else str(options or "").splitlines()
                for option in values:
                    option = str(option).strip()
                    if option.startswith("image="):
                        value = option[len("image=") :]
                        note(
                            path,
                            locate(lines, key_line(with_, "driver-opts"), value),
                            value,
                            "this buildx driver's image",
                        )
            script = step.get("run")
            if isinstance(script, str):
                scan_shell(path, script, key_line(step, "run"), lines)


for path in reach:
    text = read_text(path)
    if text is None:
        problems.append(f"{path}:1: this file is in CI's reach and is not readable as text")
        continue
    lines = text.splitlines()
    if path.suffix in {".yaml", ".yml"}:
        scan_workflow(path, lines)
    elif path.name == "Dockerfile" or path.name.endswith(".Dockerfile"):
        scan_dockerfile(path, lines)
    elif path.suffix in {".go", ".ts"}:
        scan_source(path, lines)
    elif path.suffix == ".sh":
        scan_shell(path, text, 1, lines)
    else:
        problems.append(
            f"{path}:1: CI's reach names this file, and this check reads no references out of a "
            f"{path.suffix or 'suffixless'} file. Teach it how, or drop the file from REACH."
        )

for path, line, text, how in references:
    judge(path, line, text, how)

for key, why in sorted(IMAGE_FROM.items()):
    if key not in used_image_from:
        problems.append(
            f"{key[0]}:1: IMAGE_FROM names ${key[1]} here, and no docker command in that file "
            f"fetches it any more ({one_line(why)}). Delete the entry."
        )

# ---------------------------------------------------------------- the mirror.gcr.io sweep
# The walk is workflow_paths.walk, the one check-image-trigger-paths.sh reads the same tree
# with. It is imported here rather than beside this check's other imports, because the package
# mode ends above and this sweep is the one half that walks the tree.
from workflow_paths import walk  # noqa: E402

swept = 0
naming_mirror: set[str] = set()
for name in walk():
    path = Path(name)
    if SWEEP_SKIP_DIRS.intersection(path.parts[:-1]) or name in SELF:
        continue
    text = read_text(path)
    if text is None:
        continue
    swept += 1
    if MIRROR not in text:
        continue
    naming_mirror.add(name)
    line = locate(text.splitlines(), 1, MIRROR)
    if name in reach_names:
        problems.append(
            f"{name}:{line}: a CI job runs this file and it names {MIRROR}, the registry CI "
            f"fetched from before. Name {REGISTRY}@<digest> instead, from a row of {CI_IMAGES}."
        )
    elif name not in EXCEPTIONS:
        problems.append(
            f"{name}:{line}: this file names {MIRROR} and is in neither CI's reach nor the "
            f"exception list. Repoint it at a row of {CI_IMAGES}, or add it to EXCEPTIONS in "
            f"this check under `{NO_CI_JOB}` or `{FETCHES_NOTHING}`."
        )

# An entry admits mirror.gcr.io in one file. An entry naming a file the tree does not hold
# admits nothing, so it is left alone; an entry on a file that no longer names mirror.gcr.io is
# a ground nothing holds any more, and fails here.
for name, ground in sorted(EXCEPTIONS.items()):
    if name in reach_names:
        problems.append(
            f"{name}:1: this file is in CI's reach and in the exception list, which admits "
            f"{MIRROR} in it. Delete one of the two entries."
        )
    elif Path(name).is_file() and name not in naming_mirror:
        problems.append(
            f"{name}:1: the exception list admits {MIRROR} here on the ground that {ground}, and "
            f"the file does not name {MIRROR} any more. Delete its line from EXCEPTIONS."
        )

verdict(
    f"between {CI_IMAGES} and the images CI fetches",
    f"{CI_IMAGES} holds {len(readable)} row(s); {len(references)} reference(s) in {len(reach)} "
    f"file(s) of CI's reach name one of those rows; of {swept} text file(s) swept, the "
    f"{len(naming_mirror)} that name {MIRROR} are all in the exception list",
)
PY
