#!/bin/sh
# The one command of the visual review layer, run by CI and by a person alike, from
# packages/dispatch:
#
#   e2e/visual-run.sh                      # compare every baseline
#   e2e/visual-run.sh --update-snapshots   # take them again
#   DISPATCH_E2E_VISUAL_ORDER=reverse e2e/visual-run.sh   # compare them with the rows reversed
#
# Every argument it is given is passed on to Playwright, so the update path is the same command
# with one more flag: a baseline a person takes and a baseline CI compares come out of the same
# run of the same binaries in the same image, which is the whole point of the layer.
#
# A baseline comes from this command alone. A machine with no container, an issue pod among
# them, takes one from the full lane's run in CI instead:
#
#   1. gh run download <failed full-lane run id> --repo <owner>/<repo> \
#        --name dispatch-browser-visual-<attempt>
#   2. A failed row's -actual.png is under the results directory this script names on a failed
#      run, at the same path inside the download.
#   3. Copy it over e2e/__screenshots__/<project>/<route>-<scheme>.png only if every pixel that
#      moved was meant to and a second run gave the same bytes (else the page is not settled),
#      and say why in the commit message.
#
# WHY A SECOND ORDER. One seed serves the whole run, so a page that writes state as it draws
# changes what every row after it compares. The review seed writes each such mark before any row
# runs (`seedReviewRoutes`, e2e/review-seed.ts), and CI's visual job runs the rows a second time
# with `DISPATCH_E2E_VISUAL_ORDER=reverse` (e2e/visual-order.ts): the phone rows first, and each
# project's rows last first. A row that reads another row's write then fails one of the two runs
# by its own name. The variable reaches the container through the `DISPATCH_E2E_` sweep below.
#
# WHY THE SEED RUNS ON THE HOST AND THE SHOTS IN THE CONTAINER. `e2e/psql.ts` spawns the `psql`
# binary, and both `resetDatabase()` (e2e/seed.ts) and the delivery fixtures
# (e2e/delivery-seed.ts) reach it; the pinned Playwright image carries no `psql`. The stack this
# script starts below also binds 127.0.0.1, and a page in a bridge-networked container would
# reach it over some other address and so be no secure context - a different configuration of
# this application from the one the rest of the suite tests, with no `navigator.locks` and no
# `crypto.randomUUID`. So the stack and the seed stay on the host, `e2e/visual-seed.ts` writes
# the corpus's ids to a JSON file, and the container runs the Playwright command alone, on the
# host's own network, reading that file.
#
# WHY THE STACK IS STARTED HERE AND NOT BY PLAYWRIGHT. Every other run of this suite starts its
# stacks from Playwright's own `globalSetup` (e2e/global-setup.ts), which builds the server with
# Go and seeds through `psql`. The image below has node and neither, so the run inside it can
# start nothing: this script starts the one stack on the host through `e2e/harness-up.ts`,
# applies the variables that script prints, and the run inside the container reads
# `DISPATCH_E2E_REUSE_SERVERS=1` among them and starts none of its own.
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
#
# WHY NOTHING HERE PULLS. The two images this run starts - the Playwright copy below and the
# stack's Postgres image, DISPATCH_E2E_PG_IMAGE where the caller names one
# (e2e/harness-postgres.ts) - are each a `docker run`, which pulls a digest the machine does not
# hold before it starts it and pulls nothing where the machine holds it: a person's run after
# their first, and CI's visual job, which pulls both in steps of their own before the step that
# runs this script, so a cold pull is no part of that step's budget for its rows
# (.github/workflows/dispatch-browser.yaml).
set -eu

# CDPATH is emptied for each `cd` so a value in the caller's environment cannot land this
# somewhere else and print that directory's path.
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
package=$(CDPATH='' cd -- "$here/.." && pwd)
root=$(CDPATH='' cd -- "$package/../.." && pwd)
cd "$package"

# WHY ONE STACK IS ENOUGH, AND WHY NO SECOND ONE CAN ARISE. The seed below runs on the host,
# outside any Playwright worker, so `TEST_PARALLEL_INDEX` is unset and `e2e/visual-seed.ts`
# writes the corpus into stack 0's database alone (e2e/harness-ports.ts), and the container's
# Playwright runs one project at a time, each of the two with a single worker of its own
# (e2e/playwright.config.ts). A second stack would be a database nothing seeded, and a row that
# opened it would fail nothing - a readiness locator such as the Inbox's heading is drawn by an
# empty page too - but would compare, or with `--update-snapshots` WRITE, a baseline of an empty
# corpus. That is what a one-stack guard stood here to refuse. It is now impossible rather than
# refused: this script starts exactly one stack, `e2e/harness-up.ts` starts no more than the one
# it is written for, and the run inside the container starts none at all.

# The host's copy of the seed, under this run's temporary directory, and the path the container
# sees it at: /tmp is the container's own and is writable there whatever the host's layout is.
seed_json="${TMPDIR:-/tmp}/dispatch-visual-seed.$$.json"
seed_in_container=/tmp/dispatch-visual-seed.json
# How the stack's variables arrive. `e2e/harness-up.ts` prints them as `export NAME=value` lines
# and then holds the stack up until it is signalled, so its stdout is a pipe this shell reads
# rather than a file it polls: the read below ends on the last line the stack prints, and it
# ends on the harness itself where that line never comes.
exports_fifo="${TMPDIR:-/tmp}/dispatch-visual-exports.$$"

# The stack goes down however this script ends, and this waits for it: its Postgres and its Go
# servers are that process's own children (e2e/harness-up.ts), so they are gone only once it is,
# and a stack left behind would hold its ports and its data directory against the next run.
harness=""
cleanup() {
  if [ -n "$harness" ]; then
    kill "$harness" 2>/dev/null || true
    wait "$harness" 2>/dev/null || true
  fi
  rm -f "$exports_fifo" "$seed_json"
}
trap cleanup EXIT
# A signal exits, and the exit runs the trap above. A cleanup bound to INT or TERM directly
# would stop the stack and let the script carry on into the run it was interrupted during.
trap 'exit 130' INT
trap 'exit 143' TERM

mkfifo "$exports_fifo"
bun e2e/harness-up.ts > "$exports_fifo" &
harness=$!
# Opened for the rest of the script, not for the loop alone: the harness writes nothing on
# stdout after the last line below, and a reader that closed the pipe would kill it with SIGPIPE
# if it ever did. The open blocks until the harness opens its end, as its own open blocks until
# this one does.
exec 3< "$exports_fifo"
ready="export DISPATCH_E2E_REUSE_SERVERS=1"
started=""
while IFS= read -r line <&3; do
  # Applied to this shell, so the seed below reaches the stack's database and the sweep further
  # down forwards its ports to the container. Every line that script prints is an export of a
  # shell-quoted value; anything else is something it said, and is passed through to this run's
  # own output rather than run.
  case $line in
    export\ *) eval "$line" ;;
    *) printf '%s\n' "$line" ;;
  esac
  if [ "$line" = "$ready" ]; then
    started=1
    break
  fi
done
# The read ended with the harness: it failed to bring the stack up, and what it said about that
# is already on this run's stderr, where it wrote it.
[ -n "$started" ] || {
  echo "e2e/visual-run.sh: the stack stopped before it had printed its variables." >&2
  exit 1
}

bun e2e/visual-seed.ts "$seed_json"

# Every harness variable the container needs. `-e NAME` with no value forwards the host's, and
# forwards nothing when the host has none, so a listener added to e2e/harness-ports.ts later
# under a `DISPATCH_E2E_` name is carried by the prefix sweep without an edit here; one added
# under any other name is not, and goes in the list below. The sweep carries the stack's own
# names too - DISPATCH_E2E_REUSE_SERVERS, which is what tells the run inside to start nothing,
# and DISPATCH_E2E_STACK_COUNT, which only a server's own sizing reads (e2e/run-server.sh) and
# which no process in the container is. DISPATCH_E2E_VISUAL_SEED is the exception to the sweep:
# it names a path inside the container, so a host value for it is dropped and the container's
# is set below.
forward=""
for name in $(env | sed -n 's/^\(DISPATCH_E2E_[A-Za-z0-9_]*\)=.*/\1/p'); do
  if [ "$name" != DISPATCH_E2E_VISUAL_SEED ]; then
    forward="$forward -e $name"
  fi
done
# The listeners whose names carry no `DISPATCH_E2E_` prefix (e2e/harness-ports.ts), the base URL
# a deployed run targets, and CI, which the config reads for its retry count.
for name in DEV_OIDC_ISSUER_PORT FAKE_BROKER_PORT FAKE_EMBEDDER_PORT FAKE_ENVOY_PORT \
  FAKE_GITHUB_PORT PLAIN_HTTP_PORT REAL_BROKER_PORT PLAYWRIGHT_BASE_URL CI; do
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

# A failed run says where a failed row's shots go, and what a failure in reverse order alone
# means, each as what holds if a row ran and failed: the script cannot tell that a row ran.
# Playwright refuses an argument it cannot read (`-uG`, or an option it does not know) before it
# runs a row, and exits 1 as a failed row does, and a run that clears its results directory may
# still run no row (a `--grep` that matches none), so neither the status nor the directory says
# so; Playwright's own output above the lines does.
# Playwright writes a failed row's `-actual.png`, `-expected.png` and `-diff.png` under the run's
# output directory: `e2e/test-results` (e2e/playwright.config.ts's `outputDir`) unless `--output`
# names another, as CI's reverse step names `e2e/test-results-reverse`. Both are relative to this
# package, where the run works, so the line names them from the repository root, as a CI log's
# reader and the job's upload do (`packages/dispatch/e2e/test-results-reverse`); an absolute
# `--output` is printed as given. A row that fails in the reverse order and passes in the normal
# one most likely reads a mark another row writes as it draws (a run under load can also draw a
# frame mid-change, so run the row again before acting). Its fix is that mark in the seed
# (`markWhatTheShotPagesRead`, e2e/review-seed.ts), not a new baseline: a baseline taken from the
# reverse run's shot holds the state the normal order never draws, and fails that run instead.
# The loop reads the arguments as Playwright's `test` command does: `--output=DIR` and `--output
# DIR`, the last one wins, an empty value names nothing, arguments after `--` are no options, and
# the word after an option that takes a value is that value, whatever it looks like - so
# `--grep-invert --output=x` names no output directory, as Playwright reads it. The two lists are
# the options of `playwright test` at the version the run uses (@playwright/test 1.64.1, the
# package.json pin; the image runs this tree's node_modules), copied from its own help (`bun
# node_modules/@playwright/test/cli.js test --help`): those written with `<value>`, which take the
# next word always, and those written with `[value]`, which take it only when it starts with no
# dash, so never an `--output`, and which end a cluster of short options. A short option that
# takes a value takes the rest of its cluster where there is one (`-Gx`), else the next word.
# e2e/__tests__/visual-run.test.ts reads both lists against that help and fails when they differ.
# The status stays the container's however the lines go: a write to a closed stderr fails and is
# ignored, and a write to a pipe whose reader has gone would raise SIGPIPE and end this shell
# before the EXIT trap stopped the stack, so the signal is ignored from here on - after `docker`
# has returned, so neither the container nor the stack inherits that.
playwright_value_options=" --add-reporter --browser -c --config -g --grep -G --grep-invert --global-timeout -j --workers --last-failed-file --max-failures --output --project --repeat-each --reporter --retries --run-agents --shard --test-list --test-list-invert --timeout --trace --tsconfig --ui-host --ui-port --update-source-method "
playwright_optional_options=" --debug --only-changed --shuffle -u --update-snapshots "
if [ "$status" -ne 0 ]; then
  trap '' PIPE
  output=""
  next=""
  for arg in "$@"; do
    case $next in
      output) output=$arg ;;
    esac
    if [ -n "$next" ]; then
      next=""
      continue
    fi
    case $arg in
      --) break ;;
      --output=*) output=${arg#--output=} ;;
      --output) next=output ;;
      --*=*) ;;
      --*)
        case $playwright_value_options in
          *" $arg "*) next=value ;;
        esac
        ;;
      -?*)
        cluster=${arg#-}
        while [ -n "$cluster" ]; do
          letter=${cluster%"${cluster#?}"}
          cluster=${cluster#?}
          case $playwright_value_options in
            *" -$letter "*)
              [ -n "$cluster" ] || next=value
              break
              ;;
          esac
          case $playwright_optional_options in
            *" -$letter "*) break ;;
          esac
        done
        ;;
    esac
  done
  case $output in
    "") output=packages/dispatch/e2e/test-results ;;
    /*) ;;
    *) output=packages/dispatch/${output#./} ;;
  esac
  printf '%s\n' "e2e/visual-run.sh: the run failed. If a row ran and failed, its shots (-actual.png, -expected.png, -diff.png) are under ${output%/}/. Playwright's own output above says whether any row ran." >&2 || :
  if [ "${DISPATCH_E2E_VISUAL_ORDER:-}" = reverse ]; then
    printf '%s\n' "e2e/visual-run.sh: this run put the rows in reverse order. If a row ran and failed here and passes in the normal order, it most likely reads a mark another row writes: run it again, and if it still fails, write that mark in the seed (markWhatTheShotPagesRead, e2e/review-seed.ts), and take no new baseline from this run." >&2 || :
  fi
fi

exit "$status"
