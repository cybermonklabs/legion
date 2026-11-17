"""The release lines check-legion-version-inputs.sh holds to what go says their builds read, and
what it asks go: one listing per program, target and cgo setting each release builds.
check-legion-version-inputs.sh runs these listings, and ci_job_reads.py turns the same records into
the Runs of the ci-filters gate, so the go commands that check runs and the ones the filter check
accounts for are one plan, kept here.

  legion_listings(targets)     ./cmd/legion in packages/daemon for each os/arch of
                               .github/go-release-targets.json's `legion` list, under
                               CGO_ENABLED=0, as release.yaml's Build legion step compiles it: a
                               file one target's build constraints alone compile can import a
                               package no other target links.
  legion_envoy_listings(arches, client_targets)
                               the archives release-envoy-listener.yaml compiles on the runner, in
                               packages/envoy: envoy-listener for linux and each arch of the
                               `envoy` list under CGO_ENABLED=0 and 1 both, since the build job
                               leaves cgo to the runner; agent-secrets and agent-secrets-helper for
                               the same under CGO_ENABLED=0; and agent-secrets for each os/arch of
                               .github/agent-secrets-client-targets.json under CGO_ENABLED=0.
  VERSION_STEPS                each step that hands a line's paths to release-push.sh, with the
                               variables that hold those paths, the run: it must be, character for
                               character after YAML has read it, its env:, key for key, its if:,
                               and its job's runs-on:.
                               Exact text is the rule because shell that reads like the command may
                               not run it: an escaped `\\$`, a `${{ }}` line GitHub renders into an
                               assignment, or any added line can keep the paths from
                               release-push.sh, and a reader of shell finds one more such form
                               each time. The runner is pinned because a self-hosted one runs its
                               own hooks and environment before the job's first step. The if: is
                               pinned because the decision is the step's only product: an `if:
                               false` throws away what the step computed, and the release
                               downstream is skipped all the same.
  JOB_OUTPUTS                  the job outputs that carry a decision on: each build job's version,
                               skip and notes, and publish's version and tag, as written here. A
                               job output `skip: "true"`, `notes: 'false'`, or publish's `tag:
                               ''`, throws the decision away as an `if: false` does.
  STEPS_BEFORE_VERSION_STEPS   every step that may run before one of those in its job: an action
                               at its pin with its with:, or a run: text, and in each case its
                               env:, all exactly. Any other step is code this check does not read,
                               and code can write $GITHUB_ENV or $GITHUB_PATH for every later step
                               however it is spelled, or through a helper it calls; so the jobs
                               are pinned up to and including the version step, and an edit of one
                               of these steps in those jobs is an edit here too.
  WORKFLOW_ENV_KEYS            the variables each workflow's env: may set: every one reaches the
                               version step, and one that a shell, the dynamic loader, git or gh
                               reads (BASH_ENV, PATH, LD_PRELOAD, GIT_DIR, GH_REPO and more) can
                               change what the step hands release-push.sh or what that script
                               reads. A list of what may be set closes that class; a list of what
                               may not would name its members one review at a time.
  JOB_KEYS                     the keys a job holding one of VERSION_STEPS may set: those the three
                               jobs set today. Another key can run code or set the environment
                               before the step - services: (a container with a writable host
                               mount and a health-check command can replace host tools such as
                               git), container:, env:, defaults:, a reusable workflow's uses: -
                               so the class is closed with a list of what may be set, as
                               WORKFLOW_ENV_KEYS closes the variables'.
  DECISION_READERS             the if:s, job and step, of the two release workflows that read a
                               checked version step's decision, in job order. A reader is an if:
                               that names, as a dotted reference in any case and with any
                               whitespace around a `.`, one of JOB_OUTPUTS' outputs through
                               needs.<job>.outputs, the version step's own outputs within its job,
                               or an earlier reader step's outcome or outputs in the same job (the
                               publish chains read steps.<id>.outcome one after another). Listed
                               with them are the own if: (position 0) of the four decision jobs,
                               of every job JOB_NEEDS names and of worker_image_release, and pair,
                               whose output legion_pair reads. Each entry is the if: exactly, for
                               the step by its id:, else its name:, else its position; the check
                               refuses an entry's if: changed or gone, entries out of this order,
                               an unlisted step an entry's if: reads, a reader no entry names,
                               continue-on-error: on an entry's step, and two steps of one name
                               in an entry's job. An if: of the two workflows that reaches the
                               needs or steps context other than by dotted names, or that reads a
                               JOB_OUTPUTS job's or checked step's outputs whole rather than one
                               output by name, is refused, so the reader rule sees each reference
                               in it. It does not see the decision passed on through the env
                               context or through another step's output, env: or run:.
  TRIGGERS, PUSH_KEYS, PUSH_BRANCHES
                               each workflow's on: events, the keys of its push trigger and the
                               branches it runs on: a push to main is what releases, so another
                               filter skips every release with a green run. The check holds the
                               push paths to cover each path a version step reads.

Pure: each caller reads the target lists its own way and hands them in, and keeps its own
annotations and its own handling of a failure.
"""

from __future__ import annotations

from typing import NamedTuple

DAEMON = "packages/daemon"
ENVOY = "packages/envoy"
RELEASE = ".github/workflows/release.yaml"
LISTENER = ".github/workflows/release-envoy-listener.yaml"
LEGION_ARGS = "LEGION_VERSION_ARGS"
LEGION_BLOCK = "LEGION_GO_INPUTS"
ENVOY_ARGS = "LEGION_ENVOY_VERSION_ARGS"
CHECKOUT = "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"
LOGIN = "docker/login-action@dbcb813823bdd20940b903addbd779551569679f"

WORKFLOW_ENV_KEYS = {
    RELEASE: ("CLAUDE_ENVOY_VERSION_ARGS", "GOPROXY", LEGION_BLOCK, LEGION_ARGS, "PI_ENVOY_VERSION_ARGS",
              "PI_LEGION_VERSION_ARGS"),
    LISTENER: ("GOPROXY", ENVOY_ARGS),
}

# Each workflow's on: events, the keys of its push trigger, and the branches a push runs it on. A
# push to main is what releases: another branch filter, a branch exclusion, or the push event taken
# away releases nothing with a green run, and no release step changes. The paths a push filters on
# are held by the check to cover whole each path a version step reads, which needs no table here:
# the paths are the version arguments and the generated block.
TRIGGERS = {
    RELEASE: ("push", "workflow_dispatch"),
    LISTENER: ("push", "workflow_dispatch"),
}
PUSH_KEYS = ("branches", "paths")
PUSH_BRANCHES = ("main",)

JOB_KEYS = ("cache-mode", "concurrency", "if", "needs", "outputs", "permissions", "runs-on", "steps",
            "timeout-minutes")


# The GitHub-hosted runner each checked job runs on: its image is GitHub's, with no hook or
# environment of anyone else's before the job's first step.
RUNNER = "ubuntu-24.04"


# The job outputs a build job hands its version step's decision on with: what the publish steps
# read as needs.<job>.outputs.version and .skip, and .notes, which decides whether the release
# body gets its changelog.
DECISION = {
    "version": "${{ steps.version.outputs.version }}",
    "skip": "${{ steps.version.outputs.skip }}",
    "notes": "${{ steps.notes.outputs.notes }}",
}
# publish's: the version and tag of the legion release it made, which worker_image_release reads
# to tag the worker image and name it in the release body.
PUBLISHED = {
    "version": "${{ steps.legion_release.outputs.version }}",
    "tag": "${{ steps.legion_release.outputs.tag }}",
}
# Each job output that carries a decision on, as written here; another value throws it away (an
# output `skip: "true"`, or publish's `tag: ''`, which skips worker_image_release). The job's other
# outputs carry other steps' results and stay free. An if: that reads one of these is a reader.
JOB_OUTPUTS = {
    (RELEASE, "legion_build"): DECISION,
    (RELEASE, "publish"): PUBLISHED,
    (LISTENER, "release_build"): DECISION,
}


# What each job that carries a checked decision needs, order aside (a string need is a list of
# one): a need taken away lets the job run without the decision it reads, or start before it, and
# a need added on a job that skips on main skips the decision job with it. The jobs those needs
# name, which a decision job's own if: reads (release_publish's reads build, image_index and
# targets) or which run before it, are pinned the same way, own if: and needs:, since one of
# them skipped on main skips the decision job too. worker_image_release carries publish's release
# on, so its chain is here as well: GitHub's needs context holds a job's direct needs alone, so
# publish taken out of its needs leaves needs.publish.result empty and its pinned if: false.
JOB_NEEDS = {
    (RELEASE, "legion_build"): (),
    (RELEASE, "publish"): ("claude_envoy_build", "legion_build", "pi_envoy_build", "pi_legion_build"),
    (RELEASE, "pi_envoy_build"): (),
    (RELEASE, "pi_legion_build"): (),
    (RELEASE, "claude_envoy_build"): (),
    (RELEASE, "worker_image"): ("legion_build",),
    (RELEASE, "worker_image_release"): ("publish", "worker_image"),
    (LISTENER, "release_build"): (),
    (LISTENER, "release_publish"): ("build", "image_index", "release_build", "targets"),
    (LISTENER, "targets"): (),
    (LISTENER, "build"): ("targets",),
    (LISTENER, "docker"): ("build", "targets"),
    (LISTENER, "image_index"): ("docker", "targets"),
}


class Reader(NamedTuple):
    """An if: that reads a decision, and what holds it: a step by its id:, else its name:, else its
    1-based position among its job's steps; position 0 is the job's own if:, and None there is a
    job with no if: of its own."""

    if_: str | None
    id: str | None = None
    name: str | None = None
    position: int | None = None


BUILT = "steps.version.outputs.skip != 'true'"
CONFIRMED = "steps.confirm.outputs.release == 'true'"
ON_MAIN = "github.ref == 'refs/heads/main'"
# The four jobs that compute or carry out a checked decision lead their entries with their own
# if: (position 0): a job `if: false` skips every reader in it at once.
DECISION_READERS = {
    (RELEASE, "legion_build"): (
        Reader(ON_MAIN, position=0),
        Reader(BUILT, id="setup-go"),
        Reader(BUILT, position=5),
        Reader(BUILT, name="Build legion"),
        Reader(BUILT, name="Smoke test"),
        Reader(BUILT, position=8),
        Reader(BUILT, position=9),
        Reader(BUILT, id="notes"),
        Reader(f"{BUILT} && steps.notes.outputs.notes == 'true'", position=11),
    ),
    (RELEASE, "publish"): (
        Reader(f"always() && {ON_MAIN}", position=0),
        # Not itself a reader of the decision, but legion_pair's if: reads its ok output: an `if:
        # false` here skips the chain as surely, so it is listed with the readers.
        Reader("${{ !cancelled() }}", id="pair"),
        Reader("${{ !cancelled() && steps.pair.outputs.ok == 'true' && needs.legion_build.result == 'success' && "
               "needs.legion_build.outputs.skip != 'true' }}", id="legion_pair"),
        Reader("${{ !cancelled() && steps.legion_pair.outcome == 'success' }}", id="legion_download"),
        Reader("${{ !cancelled() && steps.legion_download.outcome == 'success' }}", id="legion_assets"),
        Reader("${{ !cancelled() && steps.legion_assets.outcome == 'success' }}", id="legion_tag"),
        Reader("${{ !cancelled() && steps.legion_tag.outcome == 'success' }}", id="legion_attest"),
        Reader("${{ !cancelled() && steps.legion_attest.outcome == 'success' }}", id="legion_release"),
        Reader("${{ !cancelled() && steps.legion_release.outcome == 'success' && "
               "needs.legion_build.outputs.notes == 'true' }}", id="legion_notes"),
        Reader("${{ !cancelled() && steps.legion_notes.outcome == 'success' }}",
               name="legion: Write the changelog into the release body"),
    ),
    (LISTENER, "release_build"): (
        Reader(ON_MAIN, position=0),
        Reader(BUILT, position=3),
        Reader(BUILT, id="notes"),
        Reader(f"{BUILT} && steps.notes.outputs.notes == 'true'", position=5),
        Reader(BUILT, position=6),
        Reader(BUILT, name="Build agent-secrets binaries"),
        Reader(BUILT, name="Check the stamped tag"),
        Reader(BUILT, position=9),
    ),
    (LISTENER, "release_publish"): (
        Reader(f"always() && {ON_MAIN} && needs.build.result == 'success' && needs.image_index.result == 'success' && "
               "needs.targets.result == 'success'", position=0),
        Reader(CONFIRMED, position=6),
        Reader(CONFIRMED, position=7),
        Reader(CONFIRMED, id="assets"),
        Reader(CONFIRMED, name="Tag release"),
        Reader(CONFIRMED, position=10),
        Reader(CONFIRMED, id="release"),
        Reader("steps.release.outcome == 'success' && needs.release_build.outputs.notes == 'true'", id="notes"),
        Reader("steps.notes.outcome == 'success'", name="Write the changelog into the release body"),
    ),
    # The jobs JOB_NEEDS names beyond the four: their own if: alone, None where they have none.
    (RELEASE, "pi_envoy_build"): (Reader(ON_MAIN, position=0),),
    (RELEASE, "pi_legion_build"): (Reader(ON_MAIN, position=0),),
    (RELEASE, "claude_envoy_build"): (Reader(ON_MAIN, position=0),),
    (LISTENER, "targets"): (Reader(None, position=0),),
    (LISTENER, "build"): (Reader(None, position=0),),
    (LISTENER, "docker"): (Reader(None, position=0),),
    (LISTENER, "image_index"): (Reader(None, position=0),),
    # The jobs that carry publish's release on: worker_image_release reads publish's tag output,
    # and the worker image is tagged with the legion version, and named in the release body, only
    # when its own if: reads the tag; worker_image is a job it needs.
    (RELEASE, "worker_image"): (
        Reader("always() && (needs.legion_build.result == 'success' || needs.legion_build.result == 'skipped')",
               position=0),
    ),
    (RELEASE, "worker_image_release"): (
        Reader("always() && needs.worker_image.result == 'success' && (needs.publish.result == 'success' || "
               "needs.publish.result == 'failure') && needs.publish.outputs.tag != ''", position=0),
    ),
}


class VersionStep(NamedTuple):
    """A step that hands a line's paths to release-push.sh: where it is, the workflow-level
    variables that hold the paths, its whole run: text, its env: and its if: (None for a step with
    none), and its job's runs-on:. The job outputs that carry its decision on are JOB_OUTPUTS'."""

    workflow: str
    job: str
    step_id: str
    variables: tuple[str, ...]
    run: str
    env: dict[str, str] | None = None
    runs_on: str = RUNNER
    if_: str | None = None


VERSION_STEPS = (
    VersionStep(RELEASE, "legion_build", "version", (LEGION_ARGS, LEGION_BLOCK), (
        "# shellcheck disable=SC2086 # the two are the argument list, split on purpose\n"
        '.github/scripts/release-push.sh version legion-v $LEGION_VERSION_ARGS $LEGION_GO_INPUTS >> "$GITHUB_OUTPUT"\n'
    )),
    VersionStep(LISTENER, "release_build", "version", (ENVOY_ARGS,), (
        "# shellcheck disable=SC2086 # $LEGION_ENVOY_VERSION_ARGS is the argument list, split on purpose\n"
        '.github/scripts/release-push.sh version legion-envoy-v $LEGION_ENVOY_VERSION_ARGS >> "$GITHUB_OUTPUT"\n'
    )),
    # The confirm step's decision, release=true, stays in its own job, whose later steps read it.
    VersionStep(LISTENER, "release_publish", "confirm", (ENVOY_ARGS,), (
        "status=0\n"
        "# shellcheck disable=SC2086 # $LEGION_ENVOY_VERSION_ARGS is the argument list, split on purpose\n"
        '.github/scripts/release-push.sh version legion-envoy-v --expect "$VERSION" $LEGION_ENVOY_VERSION_ARGS'
        " >/dev/null || status=$?\n"
        "case $status in\n"
        '  0) echo "release=true" >> "$GITHUB_OUTPUT" ;;\n'
        "  3) ;;\n"
        '  4) .github/scripts/release-push.sh restart release-envoy-listener.yaml "$LOST_RACES" ;;\n'
        '  *) exit "$status" ;;\n'
        "esac\n"
    ), {
        "GH_TOKEN": "${{ github.token }}",
        "LOST_RACES": "${{ inputs.lost_races || 0 }}",
        "VERSION": "${{ needs.release_build.outputs.version }}",
    }, if_="needs.release_build.result == 'success' && needs.release_build.outputs.skip != 'true'"),
)


class ApprovedStep(NamedTuple):
    """A step that may run before one of VERSION_STEPS, by a name messages use: the action it runs
    and that action's with:, or its run: text, and its env:. None where the step has no such key."""

    name: str
    uses: str | None = None
    with_: dict | None = None
    run: str | None = None
    env: dict[str, str] | None = None


# release_publish's "Determine latest" and "Advance latest image" steps, which run before its
# confirm step: raw strings, so each holds the shell exactly as the workflow's block does.
DETERMINE_LATEST = r"""git fetch origin main --tags

image_digest() {
  local sha=$1
  local error_file
  error_file=$(mktemp)
  if IMAGE_DIGEST=$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "$IMAGE:$sha" 2>"$error_file"); then
    rm -f "$error_file"
    return 0
  fi
  if grep -Eqi 'manifest unknown|name unknown|not found' "$error_file"; then
    rm -f "$error_file"
    return 1
  fi
  cat "$error_file" >&2
  rm -f "$error_file"
  exit 1
}

if ! git merge-base --is-ancestor "$GITHUB_SHA" origin/main; then
  echo "should_tag_latest=false" >> "$GITHUB_OUTPUT"
  echo "::notice title=Latest promotion skipped::The event SHA is no longer on main."
else
  latest_exists=false
  if image_digest latest; then
    latest_exists=true
    latest_digest="$IMAGE_DIGEST"
    if ! latest_manifest=$(docker buildx imagetools inspect --raw "$IMAGE:latest"); then
      echo "::error title=Cannot establish latest provenance::Unable to read latest's manifest."
      exit 1
    fi
    latest_has_revision=$(printf '%s\n' "$latest_manifest" | jq -r '(.annotations // {}) | has("org.opencontainers.image.revision")')
    latest_sha=$(printf '%s\n' "$latest_manifest" | jq -r '(.annotations // {})["org.opencontainers.image.revision"] // empty')

    # Resolve the one legacy unannotated latest tag through its GHCR package-version
    # metadata. The package is the image without its registry and owner
    # (ghcr.io/<owner>/ -> <name>/envoy), whose remaining slash is percent-encoded
    # because the package name is one segment of the API's path; taken from $IMAGE so
    # that this lookup moves with the repository as the tag above does.
    if [ "$latest_has_revision" = false ]; then
      package=${IMAGE#"ghcr.io/$GITHUB_REPOSITORY_OWNER/"}
      package=${package//\//%2F}
      if ! legacy_source=$(gh api "users/$GITHUB_REPOSITORY_OWNER/packages/container/$package/versions?per_page=100" --paginate --jq '.[] | select(.metadata.container.tags | index("latest")) | [.name, (.metadata.container.tags[] | select(test("^[0-9a-f]{40}$")))] | @tsv'); then
        echo "::error title=Cannot establish latest provenance::Unable to resolve legacy latest through GHCR."
        exit 1
      fi
      case "$legacy_source" in
        *$'\n'*)
          echo "::error title=Cannot establish latest provenance::GHCR returned multiple latest sources."
          exit 1
          ;;
      esac
      IFS=$'\t' read -r legacy_digest latest_sha <<< "$legacy_source"
      if [ -z "$legacy_digest" ] || [ -z "$latest_sha" ] || [ "$legacy_digest" != "$latest_digest" ] ||
        ! git cat-file -e "${latest_sha}^{commit}" 2>/dev/null ||
        ! git merge-base --is-ancestor "$latest_sha" origin/main; then
        echo "::error title=Cannot establish latest provenance::Legacy latest has no matching main SHA image."
        exit 1
      fi
      echo "Resolved legacy latest to $latest_sha"
    elif [ -z "$latest_sha" ] || ! git cat-file -e "${latest_sha}^{commit}" 2>/dev/null ||
      ! git merge-base --is-ancestor "$latest_sha" origin/main; then
      echo "::error title=Cannot establish latest provenance::latest has an unknown or non-main revision annotation."
      exit 1
    fi
  fi

  # Prefer the newest already-published descendant, even if its release_publish job was
  # cancelled.
  promote_sha="$GITHUB_SHA"
  promote_digest="$OWN_DIGEST"
  if ! candidate_shas=$(git rev-list --first-parent "$GITHUB_SHA..origin/main"); then
    echo "::error title=Cannot establish latest provenance::Unable to enumerate newer main commits."
    exit 1
  fi
  if [ -n "$candidate_shas" ]; then
    while IFS= read -r candidate; do
      if image_digest "$candidate"; then
        promote_sha="$candidate"
        promote_digest="$IMAGE_DIGEST"
        break
      fi
    done <<< "$candidate_shas"
  fi
  test -n "$promote_digest"

  if [ "$latest_exists" = false ] ||
    git merge-base --is-ancestor "$latest_sha" "$promote_sha"; then
    if [ "$latest_exists" = true ] && [ "$latest_sha" = "$promote_sha" ]; then
      echo "should_tag_latest=false" >> "$GITHUB_OUTPUT"
      echo "latest already names $promote_sha"
    else
      {
        echo "should_tag_latest=true"
        echo "latest_sha=$promote_sha"
        echo "latest_digest=$promote_digest"
      } >> "$GITHUB_OUTPUT"
      echo "Advancing latest to $promote_sha"
    fi
  else
    echo "should_tag_latest=false" >> "$GITHUB_OUTPUT"
    echo "latest already names descendant $latest_sha"
  fi
fi
"""

ADVANCE_LATEST = r"""test -n "$DIGEST"
docker buildx imagetools create \
  --annotation "index:org.opencontainers.image.revision=$SHA" \
  --tag "$IMAGE:latest" \
  "$IMAGE@$DIGEST"
"""

STEPS_BEFORE_VERSION_STEPS = (
    ApprovedStep("checkout", uses=CHECKOUT, with_={"fetch-depth": 0, "persist-credentials": False}),
    ApprovedStep("checkout of the event's commit", uses=CHECKOUT,
                 with_={"fetch-depth": 0, "persist-credentials": False, "ref": "${{ github.sha }}"}),
    ApprovedStep("Determine latest", run=DETERMINE_LATEST, env={
        "GH_TOKEN": "${{ github.token }}",
        "IMAGE": "${{ needs.targets.outputs.envoy_image }}",
        "OWN_DIGEST": "${{ needs.image_index.outputs.digest }}",
    }),
    ApprovedStep("log in to GHCR", uses=LOGIN, with_={
        "password": "${{ secrets.GITHUB_TOKEN }}", "registry": "ghcr.io", "username": "${{ github.actor }}",
    }),
    ApprovedStep("Advance latest image", run=ADVANCE_LATEST, env={
        "DIGEST": "${{ steps.latest.outputs.latest_digest }}",
        "IMAGE": "${{ needs.targets.outputs.envoy_image }}",
        "SHA": "${{ steps.latest.outputs.latest_sha }}",
    }),
)


class Listing(NamedTuple):
    """One `go list -deps`: the module directory it runs in, its packages, the environment it runs
    under and how a message names it."""

    workdir: str
    packages: tuple[str, ...]
    env: dict[str, str]
    label: str


def legion_listings(targets: list[str]) -> list[Listing]:
    out = []
    for target in targets:
        goos, goarch = target.split("/")
        out.append(Listing(DAEMON, ("./cmd/legion",), {"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"},
                           f"./cmd/legion for {target}"))
    return out


def legion_envoy_listings(arches: list[str], client_targets: list[str]) -> list[Listing]:
    out = []
    for arch in arches:
        for cgo in ("0", "1"):
            out.append(Listing(ENVOY, ("./cmd/listener",), {"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": cgo},
                               f"./cmd/listener for linux/{arch} with CGO_ENABLED={cgo}"))
        out.append(Listing(ENVOY, ("./cmd/agent-secrets", "./cmd/agent-secrets-helper"),
                           {"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"},
                           f"./cmd/agent-secrets and ./cmd/agent-secrets-helper for linux/{arch}"))
    for target in client_targets:
        goos, goarch = target.split("/")
        out.append(Listing(ENVOY, ("./cmd/agent-secrets",), {"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"},
                           f"./cmd/agent-secrets for {target}"))
    return out
