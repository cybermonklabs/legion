#!/bin/sh
# The one command of the visual review layer, run by CI and by a person alike, from
# packages/dispatch:
#
#   bun e2e/run.ts -- e2e/visual-run.sh                      # compare every baseline
#   bun e2e/run.ts -- e2e/visual-run.sh --update-snapshots   # take them again
#
# Every argument it is given is passed on to Playwright, so the update path is the same command
# with one more flag: a baseline a person takes and a baseline CI compares come out of the same
# run of the same binaries in the same image, which is the whole point of the layer.
#
# WHY THE SEED RUNS ON THE HOST AND THE SHOTS IN THE CONTAINER. `e2e/psql.ts` spawns the `psql`
# binary, and both `resetDatabase()` (e2e/seed.ts) and the delivery fixtures
# (e2e/delivery-seed.ts) reach it; the pinned Playwright image carries no `psql`. The harness
# `e2e/run.ts` starts also binds 127.0.0.1, and a page in a bridge-networked container would
# reach it over some other address and so be no secure context - a different configuration of
# this application from the one the rest of the suite tests, with no `navigator.locks` and no
# `crypto.randomUUID`. So the harness and the seed stay on the host, `e2e/visual-seed.ts` writes
# the corpus's ids to a JSON file, and the container runs the Playwright command alone, on the
# host's own network, reading that file.
#
# WHY THE REPOSITORY ROOT IS THE MOUNT. The workspace's packages are reached through bun's
# symlinks, and those point at the REPOSITORY ROOT's `node_modules/.bun/...`
# (packages/dispatch/node_modules/@playwright/test is one). A mount of packages/dispatch alone
# would be a directory of dangling links, so the root is mounted at its own absolute path - the
# paths are then the same inside the container and out, which is also what makes a stack trace
# and a baseline's path readable - and the work directory is that root's packages/dispatch.
#
# WHY `node` AND NOT `bun`. The image has node and no bun. Node runs this suite: it resolves
# @playwright/test through the symlinks above, lists the same 1036 tests and reports the same
# 1.64.1 the host does. The one command below is therefore Playwright's own CLI entry point
# under node, not `bunx playwright`.
#
# The image is the pinned CI copy, named once below as a digest, which
# .github/scripts/check-ci-images.sh reads out of this file (its REACH list names it).
set -eu

# CDPATH is emptied for each `cd` so a value in the caller's environment cannot land this
# somewhere else and print that directory's path.
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
package=$(CDPATH='' cd -- "$here/.." && pwd)
root=$(CDPATH='' cd -- "$package/../.." && pwd)
cd "$package"

# ONE LANE, AND THIS IS WHAT HOLDS THE RUN TO IT. The seed below runs on the host, outside any
# Playwright worker, so `TEST_PARALLEL_INDEX` is unset and `e2e/visual-seed.ts` writes the
# corpus into LANE 0's database alone (e2e/harness-ports.ts). The sweep further down forwards
# every DISPATCH_E2E_* name the host has, DISPATCH_E2E_WORKERS and DISPATCH_E2E_LANES among
# them, and the config inside the container runs one worker per lane
# (e2e/playwright.config.ts). The `visual` and `visual-phone` projects declare no
# `dependencies`, so a run of more than one lane starts them at once on worker indices 0 and 1,
# and worker 1 resolves lane 1, whose database nothing seeded. Nothing would fail: a readiness
# locator such as the Inbox's heading is drawn by an empty page too, so the run would compare -
# or with `--update-snapshots` WRITE - a baseline of an empty corpus. So it is refused here,
# before the seed and before the container.
#
# `--workers=1` on the Playwright command line is NOT the fix: the config refuses a `--workers`
# that disagrees with the lane count, because `e2e/run.ts` has already started one harness per
# lane and a worker resolves the lane of its own index (e2e/playwright.config.ts, "This run has
# N lanes"). The run has to ask for one lane, which is what the message below names.
#
# The lane count is read as `laneCount()` reads it (e2e/harness-ports.ts): DISPATCH_E2E_LANES,
# which `e2e/run.ts` exports for the command it runs, else the DISPATCH_E2E_WORKERS this run
# asks for, else one. An empty value is an unset one there, and is here.
lanes=${DISPATCH_E2E_LANES:-}
[ -n "$lanes" ] || lanes=${DISPATCH_E2E_WORKERS:-}
[ -n "$lanes" ] || lanes=1
if [ "$lanes" != 1 ]; then
  printf '%s\n' \
    "e2e/visual-run.sh: this run has $lanes lanes and the visual review takes one." \
    "e2e/visual-seed.ts runs on the host, outside any Playwright worker, so it seeds lane 0" \
    "alone; the rows of a second lane would open a database nothing seeded and compare - or" \
    "with --update-snapshots write - a baseline of an empty corpus." \
    "Run it with DISPATCH_E2E_WORKERS=1." >&2
  exit 1
fi

# The host's copy of the seed, under this run's temporary directory, and the path the container
# sees it at: /tmp is the container's own and is writable there whatever the host's layout is.
seed_json="${TMPDIR:-/tmp}/dispatch-visual-seed.$$.json"
seed_in_container=/tmp/dispatch-visual-seed.json

bun e2e/visual-seed.ts "$seed_json"

# Every harness variable the container needs. `-e NAME` with no value forwards the host's, and
# forwards nothing when the host has none, so a listener added to e2e/harness-ports.ts later is
# carried by the prefix sweep without an edit here. DISPATCH_E2E_VISUAL_SEED is the exception:
# it names a path inside the container, so a host value for it is dropped and the container's is
# set below.
forward=""
for name in $(env | sed -n 's/^\(DISPATCH_E2E_[A-Za-z0-9_]*\)=.*/\1/p'); do
  if [ "$name" != DISPATCH_E2E_VISUAL_SEED ]; then
    forward="$forward -e $name"
  fi
done
# The listeners whose names carry no `DISPATCH_E2E_` prefix (e2e/harness-ports.ts), the base URL
# a deployed run targets, and CI, which the config reads for its retry count.
for name in DEV_OIDC_ISSUER_PORT FAKE_BROKER_PORT FAKE_ENVOY_PORT FAKE_GITHUB_PORT \
  PLAIN_HTTP_PORT REAL_BROKER_PORT PLAYWRIGHT_BASE_URL CI; do
  forward="$forward -e $name"
done

# `$forward` is a list of flags and must split into words, which is what the disable is for.
# The limits are docs/testing.md's rule for a container a test starts ("In a script, `docker
# run` takes `--cpus` and `--memory` before the image"), sized to this work: one worker per
# project and one project at a time, so one Chromium drawing one page, with the test process
# beside it. `--ipc=host` is Playwright's own recommendation for Chromium, whose shared memory
# would otherwise be the container's 64 MB /dev/shm. `--user` keeps every file the run writes -
# a new baseline, a diff, a trace - owned by the person who ran it, and `HOME=/tmp` gives that
# uid, which the image has no home for, a writable one.
status=0
# shellcheck disable=SC2086
docker run --rm --network host --ipc=host --user "$(id -u):$(id -g)" \
  --cpus 2 --memory 4g \
  -v "$root:$root" \
  -v "$seed_json:$seed_in_container:ro" \
  -w "$root/packages/dispatch" \
  -e HOME=/tmp \
  $forward \
  -e "DISPATCH_E2E_VISUAL_SEED=$seed_in_container" \
  ghcr.io/cybermonklabs/legion/ci@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f \
  node node_modules/@playwright/test/cli.js test --config e2e/playwright.config.ts \
  --project=visual --project=visual-phone "$@" || status=$?

rm -f "$seed_json"
exit "$status"
