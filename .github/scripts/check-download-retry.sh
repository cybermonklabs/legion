#!/usr/bin/env bash
# Every download this repository's CI makes retries, and one rule holds that retry
# (dispatch://INFRA-1545, R2). This check is the enforcement half: nothing else refuses a fetch
# added with no retry, a retry below the floor, an attempt with no time bound, or a second copy of
# the retry loop.
#
# THE FLOOR (R1): at least 5 attempts, the last starting at least 120 seconds after the first, and
# each attempt bounded in time, so an attempt against a host that accepts the connection and then
# sends nothing stops and the next one starts. Three mechanisms reach it, and a site invents no
# fourth:
#
#   1. `retry_fetch <what> <bound> -- <command...>`, the one helper, in
#      .github/scripts/ci-retry.sh: five attempts, sleeps of 10, 20, 40 and 80 seconds, each
#      attempt under `timeout <bound>`. A workflow `run:`, a composite action's `run:` and a
#      script source that file and call it.
#   2. curl's own options, `--retry 7 --retry-all-errors --max-time <bound>` with `--fail` —
#      measured on curl 8.14.1: eight attempts whose last starts 127 s after the first, with
#      `--max-time` reset for each one, so it bounds every attempt and not the whole transfer.
#      `--fail` is load-bearing and usually arrives as the `f` of `-fsSL`: without it curl exits 0
#      on a persistent 504 and writes the error body into the output file, which the checksum or
#      the unpack after it then reads. A curl that reads the status itself (`-w '%{http_code}'`
#      and a test of what it printed) answers for `--fail` and passes.
#   3. inside a Dockerfile, one canonical inline `retry()` definition, the same text in every
#      network `RUN` of the three files. A Dockerfile carries the loop inline because it can hold
#      no new file above a layer whose order its own comments set on purpose, and a COPY of a
#      helper there would throw that layer's cache away on every edit of the helper.
#
# WHAT IT READS: every file under .github/workflows/, every action.yml (or action.yaml) under
# .github/actions/, every *.sh under .github/scripts/ but the `.test.sh` harnesses, and the three
# Dockerfiles a CI job builds (DOCKERFILES below; a missing one fails this check, so a rename is
# seen). Files are discovered by walking the tree, not by asking git, so an unsnapshotted change
# cannot read green locally. Workflows and actions are parsed as YAML, never grepped, and only a
# step's `run:` is read. This check reads its own file like any other script.
#
# Rule 8 reads one step across more than one of those files: a step that runs a script under
# .github/scripts/ carries that script's own retried calls, and those of every script it calls
# in turn. SCRIPT_CALL finds a call by the path it writes, scripts_called reads one body's calls
# and script_arithmetic walks them under a `seen` set, which counts a file two of a step's paths
# reach once and stops a cycle. Three budgets of this tree were a one-line `run:` of a script
# call and nothing else — ci.yaml's `pr-title` and `ci-ok` steps and pr-title.yaml's, 1200 s,
# 600 s and 900 s of retries under a 20-, a 10- and a 15-minute budget — and a rule 8 that
# stopped at the `run:` saw the arithmetic of none of them.
#
# Two limits are left in that reading set, and each is a script this check cannot name. A call
# that does not write the script's path — one that builds the path as it runs, or runs a
# variable holding the whole command — is a call SCRIPT_CALL does not find: the script is still
# read as a file of its own, so rules 1 to 6 hold it, but its arithmetic reaches no step's
# budget. And a script outside .github/scripts/ is not read at all, since that one directory is
# the whole set: a `run:` that calls the repository's own scripts/<name>.sh or a
# packages/<package>/scripts/<name>.sh — ci.yaml and the release workflows each hold such calls
# — is read no further than its own words, so a download written inside one of those files is
# held to no floor here and sits in no budget. Moving it under .github/scripts/ is what brings
# it back under both rules, and a new file there is read without anyone remembering to list it.
# The harnesses left out are named at TEST_HARNESS below, with the two network calls they hold
# today and why a retry at either would destroy what the test measures.
#
# THE VERBS. A fetch is one of these commands, in command position, after the wrappers below are
# stripped: curl, wget, apt-get/apt (update, install, download, upgrade, dist-upgrade, source,
# build-dep), pip install, uv (sync, add, pip, tool, python, lock, export), bun install, bun add,
# npm/pnpm/yarn (install, ci, add), playwright install, go mod download, go install,
# mise (install, use), crane (copy, digest, config, pull, export, manifest, blob), docker pull,
# pulumi plugin install, and gh — every subcommand of it but the local ones named next.
#
# gh is read the other way round from the rest, by what it is not. `gh api` and `gh release
# download` were the whole of its table once, and an unretried `gh run list`, `gh pr view`, `gh
# run view`, `gh cache delete`, `gh workflow run` or `gh search code` was therefore invisible
# here: every one of them is the same HTTPS call to the same host as a `gh api`, carrying the
# same token, and the difference between the two sets was a subcommand name and nothing else. So
# the set is gh's own command list minus what reaches no network, read off `gh --help` and off
# each command's own help: GH_LOCAL below — `alias`, `completion`, `config`, `extension`, `help`
# and `version`, which answer out of the local config, the local extension directory or the
# binary — and the four local actions of `gh auth`, `git-credential`, `setup-git`, `switch` and
# `token`, which read the stored token or write the local git config, where `gh auth login`,
# `logout`, `refresh` and `status` talk to the host. Anything else gh runs is a fetch, so a
# subcommand nobody has listed is held to the floor rather than passing unseen. A verb is named
# `gh <subcommand> <action>` for the subcommands that take an action of their own (GH_GROUPS:
# `gh run list`, `gh release view`), and `gh <subcommand>` for the three that take an argument
# instead (`gh api repos/owner/name`, `gh browse`, `gh status`). A gh call that writes is a site
# like a gh call that reads: `gh api --method POST` has always been one here, and the retry is
# for the transport, which does not know the method.
#
# `mise x`, `mise exec`, `mise up` and
# `mise upgrade` are deliberately not among the verbs; rule 7 below says why, and what it does
# with one instead. `bunx`, `npx`, `pnpx`, `sh -c`, `bash -c` and `eval` are transparent: the
# verb is what they run, so a `bunx playwright install` is the Playwright download and a `bunx
# biome check` is not a fetch at all (the binary it runs came from the workspace install, which
# is itself a site).
#
# The wrappers stripped before the command is read are an assignment, a shell keyword, `sudo`,
# `env`, `exec`, `command`, `nice`, `nohup`, `time`, `timeout <seconds>`, `retry_fetch <what>
# <bound> --` and a Dockerfile's `retry <bound> <what>`. The last three are also what says a
# fetch is retried and bounded. A command that answers about itself (`--version`, `--help`) reads
# nothing from the network and is no fetch, whatever its verb.
#
# Two verbs are deliberately outside the subject, because R1's subject is a tool, a toolchain or a
# dependency: `git clone` and `git fetch`, which move this repository's own refs rather than
# anything a job installs, and which no site in the tree retries. `playwright install-deps` is
# named under the exceptions below.
#
# THE RULES.
#
#   1. In a workflow `run:`, an action.yml `run:` or a *.sh under .github/scripts/, a fetch must
#      be a `retry_fetch` call or, for curl, carry curl's own options at the floor.
#   2. A loop (`while`, `until`, `for`) whose body holds a fetch that is not a `retry_fetch` call
#      is a copy of the helper and is refused as one, whether or not that fetch would pass rule 1
#      on its own: R2's rule is that one place holds the attempts. A loop reaches a fetch two
#      ways, and both count: the fetch is written in the body, or the body is a function the same
#      block calls with a fetch among the call's words — `attempts 2 "apt-get update" sudo timeout
#      90 apt-get update`, the shape the shared helper was made from.
#   3. A Dockerfile `RUN` that fetches opens with the canonical `retry()` text (CANONICAL below),
#      compared with its backslash continuations joined and each run of blank space collapsed to
#      one space, which is the one liberty the tree's own two shapes force: a `RUN --mount=...`
#      indents the definition under the flag. Each fetch of that `RUN` is then wrapped in
#      `retry <bound> <what>`, or is a curl carrying curl's own options. A `RUN` that defines a
#      retry loop of its own text is refused. A Dockerfile is judged against the floor alone:
#      rule 2 does not read one, because the canonical definition is itself a loop and because a
#      `RUN` iterating a list is how a Dockerfile writes what a script would write as a function.
#   4. An attempt with no time bound is refused: a curl with no `--max-time`, or a retried fetch
#      of any other verb that no `timeout` and no helper bounds.
#   5. An `apt-get update` carries `-o APT::Update::Error-Mode=any`. This is apt's `--fail`, and
#      the reason is curl's: a fetch that reports success having fetched nothing. `apt-get update`
#      exits 0 when every index fails to download — it prints `W: Failed to fetch … 504 Gateway
#      Timeout` for each one and then `W: Some index files failed to download. They have been
#      ignored, or old ones used instead.` — so a retry around it succeeds on its first attempt
#      having read no list, and the `apt-get install` after it fails with `E: Package '…' has no
#      installation candidate` against a list that will never name the package. That is a failure
#      no further attempt can cure, which is what cost run 42375256920's `download-retry-images`
#      job. The option turns each of those warnings into an error, so the command exits non-zero
#      and the retry retries. It has existed since apt 2.1.10; the images here run apt 2.6
#      (bookworm) and 2.9 (trixie), and the runner's own on Ubuntu 24.04.
#
#      Every `apt-get update` this check holds to the floor carries it, retried or not. The two
#      sets are the same once the tree is green, since rule 1 and rule 3 retry every one of them;
#      scoping the rule to the retried ones would differ only in handing an author who writes a
#      bare `apt-get update` one refusal now and a second one after they add the retry. The sites
#      an exception below removes are outside this rule as they are outside the rest.
#   6. In Python, inside the heredoc of a *.sh under .github/scripts/, a read through
#      `urllib.request` goes through check-ci-images.sh's `read_retrying`, which carries the floor
#      in that check's own idiom — five attempts, waits of 10, 20, 40 and 80 seconds, each bounded
#      by the read's own `timeout=30`. One definition, no copies, as the shell has.
#   7. A `mise x`, `mise exec`, `mise up` or `mise upgrade` carries no retry, anywhere: not a
#      `retry_fetch` call and not a Dockerfile `retry` call. None of the four fails when it
#      cannot resolve the tool. `mise x` warns (`Failed to resolve tool version list for … HTTP
#      host https://api.github.com:443 is unavailable`) and then runs whatever binary of that
#      name is already on PATH — a different build, at a version nobody asked for — and exits 0;
#      `mise up` prints `All tools are up to date` having read nothing, and exits 0. Measured on
#      mise 2026.8.12, the build packages/daemon/docker/worker.Dockerfile pins, against a proxy
#      faulting every host. So a retry around one of them waits for a failure that never comes,
#      and the cure is structural: retry `mise install <tool>`, which exits 1 on the failed fetch
#      on that same build, and read the path afterwards from what that install put on disk.
#
#      That is also why the four are not fetch verbs above, and why a bare one is on no list. A
#      `mise x` that follows the install of the tool it names resolves off disk: with every host
#      faulting and a decoy binary earlier on PATH it still printed the installed path, silently
#      and exit 0. A `mise x` that follows no install fetches only as a side effect of the
#      resolution that install should have done. Holding either to the floor would be demanding
#      the one shape this rule refuses, so the check reads these four only to refuse a retry.
#
#   8. A step that holds a retried fetch and carries its own `timeout-minutes` gives that budget
#      strictly more time than the attempts it allows: 60 x <minutes> is strictly greater than
#      that step's arithmetic, which is 5 x <bound> + 150 s for each `retry_fetch` call and
#      8 x <bound> + 127 s for each curl making its own attempts, summed over the step's own
#      calls, over those of every script under .github/scripts/ the step runs and every script
#      that one runs in turn, and, for a `uses:` step of a local composite action, over that
#      action's calls and the scripts and actions it reaches the same way. Equal is
#      not enough: the last attempt's own error and the runner's kill then land at the same
#      instant, and the retry can lose that race. Measured in run 42386541065's job
#      128445139635, where a `gh api` read bounded at 30 s sat under `timeout-minutes: 5`, its
#      own arithmetic of 5 x 30 + 150 = 300 s against a budget of 300 s: the helper's `::error::`
#      landed at 17:22:16.7444966Z and the runner's kill at 17:22:16.7483230Z, four milliseconds
#      apart. Lose that race and the step reports GitHub's bare `The action '…' has timed out
#      after 5 minutes.` instead of naming the site and its attempts, which is the loud failure
#      the retry exists to give. GitHub's granularity is the minute, so a whole minute is the
#      smallest margin that can be written.
#
# THE EXCEPTIONS, which are R2's four and no others. Each is a site this repository refuses to
# retry on purpose. Each one written out as an entry of EXEMPTIONS below must match something: an
# entry that matches nothing fails this check, so an exception cannot outlive the site it was
# written for. (b) and (d) are read from the tree rather than listed, for the reasons they give.
#
#   a. A `ghcr.io` pull of a package this repository publishes. A failed pull there is a fault of
#      this repository's own package, and it must be seen rather than retried over. R2 named two
#      sites, .github/actions/pull-test-images and the `envoy-client` job's pull; T7's
#      `download-retry-images` job added a third, the pull of the base images of the three
#      Dockerfiles, which is the same package and the same reasoning.
#   b. A `uses:` reference, and anything inside a third-party action. This needs no entry: only a
#      step's `run:` is read, and no third-party action's source is in this tree. A composite
#      action cannot wrap a `uses:` step in a loop, which is why R4 answers the Bun runtime with a
#      restored cache rather than with a retry.
#   c. The apt phase of .github/scripts/playwright-install.sh, which dispatch://INFRA-1443 owns:
#      its own `attempts()` loop, which switches origin after two attempts where the shared
#      helper cannot, and the four calls that hand it `apt-get`, are exempt from rules 1 and 2
#      together, and from rule 5 with them, an exemption taking the whole verdict. That phase was
#      the Playwright step of ci.yaml's `dispatch` job until cybermonklabs/legion#2891 moved the
#      Dispatch browser suite into .github/workflows/dispatch-browser.yaml and wrote its install
#      as that script, which both of that workflow's install steps call; the exception moved with
#      it. Those calls carry `-o APT::Update::Error-Mode=any` all the same: rule 5's hole is a
#      hole in that loop too, and one option is the whole fix. `playwright install-deps` is that
#      same apt phase written as Playwright writes it, and is outside the verb list above for the
#      same reason; the browser download beside it, `playwright install`, is a site like any
#      other.
#   d. The credentialed publish jobs of release.yaml, release-envoy-listener.yaml and
#      release-envoy-infra.yaml, and every *.sh under .github/scripts/ such a job runs.
#      .github/scripts/check-release-credentials.sh pins the command set such a job may run, and
#      a retry wrapper there is a change to that pinned shape. A job is
#      credentialed by that check's own definition, WRITE_SCOPES: it holds `contents`, `id-token`,
#      `attestations` or `actions` at `write`, or names the deploy key. Their build jobs, which
#      hold no credential, are in scope like any other job.
#
#      The scripts are the second half, and they are read from the tree rather than listed: the
#      roots are the scripts a credentialed job's steps call, and the closure adds the scripts
#      those call in turn, which is the reading rule 8 already makes of a step. One script is in
#      that set today, .github/scripts/release-push.sh, with .github/scripts/release-bump.sh,
#      which it calls and which fetches nothing. release-push.sh's own fetches are the gh calls
#      of its `restart`, `release` and `release-section` commands — `gh run list`, `gh workflow
#      run`, `gh release view`, `gh release create`, `gh release upload` and `gh release edit` —
#      and nothing in the tree but those publish jobs runs one of them.
#
#      Retrying them means sourcing .github/scripts/ci-retry.sh into the publish job, and that is
#      what check-release-credentials.sh refuses: its rule 1(a) lets a credentialed job run no
#      dependency code, and its dependency_command() names as dependency code any command whose
#      path holds a `scripts/` component other than release-push.sh itself (`if
#      DEPENDENCY_PATH.search(path) and path != RELEASE_PUSH`). A `.` of ci-retry.sh is read
#      through that check's INTERPRETERS to exactly that path. Measured: sourcing the helper into
#      release.yaml's `publish` step 'pi-envoy: Create GitHub release' makes that check print
#      `job 'publish' holds a credential, but step '…' runs '.github/scripts/ci-retry.sh';
#      dependency code runs only in a job that holds none` and exit 1. Writing the `.` inside
#      release-push.sh instead would hide the same code from that check rather than keep it out
#      of the job, so these sites are left unretried.
#
# TWO SCOPINGS OF THE SUBJECT, which are not exceptions to it: neither site is a download of a
# tool, a toolchain or a dependency, so R1 never reached them.
#
#   * A fetch of 127.0.0.1, localhost or ::1 is a readiness poll of a process the step itself
#     started, not a download. Its loop is a wait for that process, and rule 2 leaves it alone for
#     the same reason.
#   * A call whose own failure is consumed as the answer rather than as a fault — `x=$(cmd …
#     2>/dev/null || true)`, whose result is then tested — is a probe. The one site in the tree is
#     mirror-ci-images.sh's `crane digest` of the package's own tag: that refusal is the signal to
#     copy, and five attempts there would sleep 150 s for every row the package lacks, which are
#     exactly the rows the copy after it then writes. The copy itself, and the digest read that
#     confirms it, do retry.
#
# One more scoping the tree states twice, at ci.yaml's Playwright step and at e2e.yaml's package
# step: an `apt-get install` with no `-d` that follows an `apt-get install -d` in the same `run:`
# unpacks an archive already on disk and takes no network, so the download before it is the half
# that retries. An `apt-get` carrying a simulate flag (`-s`, `--simulate`, `--dry-run`,
# `--just-print`, `--no-act`, `--recon`) reads the lists on disk and fetches nothing either.
#
# `--list` prints the sites this check holds to the floor, one per line, as four tab-separated
# columns: the file, the line, the verb, and the step's name where the site is a step (`-` where
# it is not), sorted by file and then by line. Nothing else goes to standard output in that mode,
# so `wc -l` is the count and `cut -f1,2` is the file and line. dispatch://INFRA-1545's production
# measurement (R5, T10) reads this list at the merged head: it is the machine-readable subject of
# "a step R1 names". A site an exception or a scoping above removes is not on the list, by the
# same reasoning that leaves it unretried.
#
# Run from anywhere: .github/scripts/check-download-retry.sh
#                    .github/scripts/check-download-retry.sh --list
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

case "$#:${1:-}" in
  0: | 1:--list) ;;
  *)
    echo "::error file=.github/scripts/check-download-retry.sh::this check takes no argument, or \`--list\` to print the sites it holds to the floor. Run \`.github/scripts/check-download-retry.sh\` or \`.github/scripts/check-download-retry.sh --list\`." >&2
    exit 2
    ;;
esac
export DOWNLOAD_RETRY_MODE="${1:-}"

# The walk is workflow_paths.py's, beside this script in the repository it checks, which
# PYTHONPATH names: the files this check reads are the files the other gates read.
# PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import os
import re
import signal
import sys
from pathlib import Path

import yaml

from workflow_paths import walk

LIST_MODE = os.environ.get("DOWNLOAD_RETRY_MODE") == "--list"


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where each key was written."""

    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

# The three Dockerfiles a CI job builds. Named rather than discovered: a Dockerfile elsewhere in
# the tree is not built by CI, and a rename of one of these must fail here rather than pass by
# matching nothing.
DOCKERFILES = (
    "packages/daemon/docker/worker.Dockerfile",
    "packages/envoy/docker/Dockerfile",
    "packages/envoy/infra/host/heldportwrapper/Dockerfile",
)
WORKFLOWS = Path(".github/workflows")
ACTIONS = Path(".github/actions")
SCRIPTS = Path(".github/scripts")
HELPER = ".github/scripts/ci-retry.sh"
# This file, named by the problems it raises about its own lists.
SELF = ".github/scripts/check-download-retry.sh"
# The `.test.sh` harnesses beside the checks are not read. A harness is not a job's download: the
# two network calls in the tree today are ci-fault-proxy.test.sh's `run_curl`, which drives one
# curl at the fault proxy precisely to watch a single unretried attempt, and
# release-push.test.sh's `gh api meta`, which compares the pinned host keys with what GitHub
# publishes and prints SKIP when the read fails. A retry at either would destroy what the test
# measures, and neither brings a tool, a toolchain or a dependency into a job.
#
# The widened gh table above does not add a third. The other gh words a harness writes reach no
# host: release-push.test.sh and check-private-names.test.sh each write a stub `gh` into the
# test's own bin directory and put that directory first on PATH, so the words are the stub's own
# `case` arms and the calls the script under test makes land on it; check-ci-filters.test.sh's
# `gh run cancel` is a line of a workflow fixture it writes for the checker to read, which
# nothing runs. A retry around any of them would wrap a shell function or a string.
TEST_HARNESS = ".test.sh"
# What names a script in a call: a word ending in `/<name>.sh`, wherever on the line it sits —
# `.github/scripts/x.sh`, `"$script_dir/x.sh"`, `"$GITHUB_ACTION_PATH/../../scripts/x.sh"`,
# `tag=$(.github/scripts/x.sh)`, `. .github/scripts/ci-retry.sh`. Only the name is read, and only
# a name SCRIPTS holds is followed: every script a CI job runs lives in that one directory, and
# the path a call writes in front of the name is one of half a dozen ways of reaching the same
# file. A `.test.sh` harness matches nothing here, since the name this reads holds no dot.
SCRIPT_CALL = re.compile(r"/([a-z0-9-]+\.sh)")
# The canonical inline retry of a Dockerfile RUN, already in the shape the comparison uses: its
# continuations joined and each run of blank space one space.
CANONICAL = (
    'retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; '
    '[ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; '
    'sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }'
)
RETRY_FLOOR = 7
ATTEMPT_FLOOR = 5
# Rule 8's arithmetic, the worst case of one retried call. The helper sleeps 10, 20, 40 and 80
# seconds between its five attempts; curl, making `RETRY_FLOOR + 1` attempts itself, waits
# 1, 2, 4, 8, 16, 32 and 64 seconds between its own.
HELPER_WAIT = 150
CURL_WAIT = 127
# What a `timeout` argument's suffix multiplies its number by, as timeout(1) reads it.
SUFFIX = {"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}
# The one Python function a `urllib.request` read of a script under .github/scripts/ goes through,
# and the one file that defines it.
PYTHON_READER = "read_retrying"
PYTHON_READER_FILE = ".github/scripts/check-ci-images.sh"
PYTHON_READ = re.compile(r"urllib\.request\.urlopen\s*\(|(?<![\w.])urlopen\s*\(")
PYTHON_DEF = re.compile(r"^(\s*)def\s+(\w+)\s*\(")

LOOPBACK = ("127.0.0.1", "localhost", "::1")
# What a word sheds to become the name of the program it runs.
PROGRAM = re.compile(r"^(\$\{?GITHUB_WORKSPACE\}?/)?(\.\.?/)*")
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\+?=")
EXPRESSION = re.compile(r"\$\{\{.*?\}\}")
FUNCTION_WORD = re.compile(r"^[A-Za-z_][\w:.-]*\(\)$")
# Transparent in command position, taking no argument of their own.
PASSTHROUGH = {"sudo", "exec", "command", "builtin", "nice", "ionice", "nohup", "time", "env",
               "stdbuf", "setsid", "doas", "xvfb-run"}
# Transparent, but the command they run is inside the script string behind `-c`.
INTERPRETERS = {"bash", "sh", "dash", "zsh"}
# Transparent, and the verb is what they run: a package runner resolves a binary the workspace's
# own install already provides.
RUNNERS = {"bunx", "npx", "pnpx", "pnpm-dlx"}
KEYWORDS = {"if", "then", "else", "elif", "fi", "do", "done", "while", "until", "for", "in",
            "case", "esac", "select", "function", "!", "[[", "]]", "{", "}"}
LOOP_WORDS = {"while", "until", "for", "select"}
APT_SUBCOMMANDS = {"update", "install", "download", "upgrade", "dist-upgrade", "full-upgrade",
                   "source", "build-dep", "reinstall"}
APT_SIMULATE = {"-s", "--simulate", "--just-print", "--dry-run", "--no-act", "--recon"}
APT_DOWNLOAD_ONLY = {"-d", "--download-only"}
# What an `apt-get update` option has to say for rule 5 to be satisfied, the value of an `-o`.
APT_ERROR_MODE = re.compile(r"apt::update::error-mode\s*=\s*any", re.IGNORECASE)
# A command that answers about itself reads nothing from the network, whatever its verb.
INFORMATIONAL = {"--version", "--help", "--usage"}
# The mise subcommands that fetch and say so: both exit 1 when the fetch fails.
MISE_SUBCOMMANDS = {"install", "use"}
# Rule 7's four: each exits 0 when it cannot resolve the tool, so none of them is a fetch verb
# and a retry around one protects nothing. The check reads them only to refuse that retry.
MISE_SILENT = {"x", "exec", "up", "upgrade"}
CRANE_SUBCOMMANDS = {"copy", "cp", "digest", "config", "pull", "export", "manifest", "blob"}
UV_SUBCOMMANDS = {"sync", "add", "pip", "tool", "python", "lock", "export"}
NPM_SUBCOMMANDS = {"install", "ci", "i", "install-test", "add"}
BUN_SUBCOMMANDS = {"install", "add", "i"}
# gh. Every subcommand of it but these reaches api.github.com over HTTPS, so the table is written
# the other way round from the rest: what is named here is what fetches nothing, and anything
# else gh runs is a fetch. GH_LOCAL answers out of the local config, the local extension
# directory or the binary itself: `gh config get`, `gh alias list`, `gh completion`, `gh
# extension list`, `gh help` and `gh version`. `gh auth` is half of each — `git-credential`,
# `setup-git`, `switch` and `token` read the stored token or write the local git config, while
# `login`, `logout`, `refresh` and `status` talk to the host — so its local actions are named
# apart. GH_GROUPS is the set of subcommands that take an action word of their own, which is
# gh's own grouping (`gh run list`, `gh release view`, `gh cache delete`): a verb of one of them
# is named with that action, and `gh api`, `gh browse` and `gh status`, which take an argument
# instead, are named by the subcommand alone.
GH_LOCAL = {"alias", "completion", "config", "extension", "help", "version"}
GH_AUTH_LOCAL = {"git-credential", "setup-git", "switch", "token"}
GH_GROUPS = {"accessibility", "attestation", "auth", "cache", "codespace", "gist", "gpg-key",
             "issue", "label", "org", "preview", "project", "release", "repo", "ruleset", "run",
             "search", "secret", "ssh-key", "variable", "workflow"}
# The write scopes check-release-credentials.sh counts as a credential (its WRITE_SCOPES), and the
# deploy key it reads beside them.
WRITE_SCOPES = ("contents", "id-token", "attestations", "actions")
DEPLOY_KEY = re.compile(r"secrets\s*(\.\s*DEPLOY_KEY\b|\[\s*['\"]DEPLOY_KEY['\"]\s*\])",
                        re.IGNORECASE)
RELEASE_WORKFLOWS = (
    ".github/workflows/release.yaml",
    ".github/workflows/release-envoy-listener.yaml",
    ".github/workflows/release-envoy-infra.yaml",
)

problems: list[str] = []


def problem(path: str, line: int, message: str) -> None:
    problems.append(f"::error file={path},line={line}::{message}")


# ---------------------------------------------------------------- reading the shell


def without_heredocs(text: str) -> tuple[str, list[tuple[int, str]]]:
    """The text with every heredoc body blanked, and those bodies as (line, text) pairs. A body is
    data to the shell, not commands, so the shell rules never read one; the Python a `python3
    <<'PY'` runs is read by rule 5 instead, off the bodies this returns."""
    out: list[str] = []
    bodies: list[tuple[int, str]] = []
    lines = text.split("\n")
    index = 0
    while index < len(lines):
        line = lines[index]
        out.append(line)
        index += 1
        for match in re.finditer(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1", line):
            terminator = match.group(2)
            while index < len(lines) and lines[index].strip() != terminator:
                bodies.append((index + 1, lines[index]))
                out.append("")
                index += 1
            if index < len(lines):
                out.append(lines[index])
                index += 1
    return "\n".join(out), bodies


def probe_spans(text: str) -> list[tuple[int, int]]:
    """The character spans of each `x=$(… )` whose failure is consumed as the answer: its own
    error silenced and `|| true` inside the substitution or right after it."""
    spans = []
    for match in re.finditer(r"[A-Za-z_][A-Za-z0-9_]*=\$\(", text):
        depth = 1
        index = match.end()
        while index < len(text) and depth:
            if text[index] == "(":
                depth += 1
            elif text[index] == ")":
                depth -= 1
            index += 1
        inner = text[match.end():index - 1]
        consumed = re.search(r"\|\|\s*true\s*$", inner.strip()) or re.match(
            r"\s*\|\|\s*true\b", text[index:index + 40]
        )
        if consumed and ("2>/dev/null" in inner or "2>&1" in inner):
            spans.append((match.start(), index))
    return spans


class Command:
    """One simple command of a shell body: its words, the line it starts on, the character offset
    it starts at, and the innermost loop it sits in."""

    def __init__(self, words: list[str], line: int, offset: int) -> None:
        self.words = words
        self.line = line
        self.offset = offset
        self.loop: int | None = None


class Scanner:
    """One quote-aware pass over a shell body, collecting each simple command with its line. The
    shape is .github/scripts/check-release-credentials.sh's Scanner, which reads a `run:` the same
    way; this one keeps the keywords, so a loop and a function definition are visible, and carries
    each command's line and offset, so a problem names where to edit."""

    def __init__(self, first_line: int) -> None:
        self.commands: list[Command] = []
        self.words: list[str] = []
        self.word = ""
        self.started = False
        self.at_word_start = True
        self.redirect_target = False
        self.line = first_line
        self.word_line = first_line
        self.word_offset = 0
        self.command_line = first_line
        self.command_offset = 0
        self.open_command = False

    def add(self, char: str, offset: int) -> None:
        if not self.started:
            self.word_line = self.line
            self.word_offset = offset
        if not self.open_command:
            self.command_line = self.line
            self.command_offset = offset
            self.open_command = True
        self.word += char
        self.started = True
        self.at_word_start = False

    def end_word(self) -> None:
        brace = False
        if self.started and not self.redirect_target:
            self.words.append(self.word)
            # `{` and `}` are reserved words, so `fetch() { curl …; }` holds a command of its own
            # after the brace: without this the curl would read as an argument of `fetch()`.
            brace = self.word in ("{", "}")
        if self.started:
            self.redirect_target = False
        self.word = ""
        self.started = False
        self.at_word_start = True
        if brace:
            self.end_command()

    def end_command(self) -> None:
        self.end_word()
        if self.words:
            self.commands.append(Command(self.words, self.command_line, self.command_offset))
        self.words = []
        self.redirect_target = False
        self.open_command = False

    def scan(self, text: str) -> list[Command]:
        quote = None
        open_subs: list[tuple[str, str | None]] = []
        i = 0
        while i < len(text):
            char = text[i]
            after = text[i + 1:i + 2]
            if char == "\n":
                self.line += 1
            if quote == "'":
                if char == "'":
                    quote = None
                else:
                    self.add(char, i)
            elif char == "`":
                self.end_command()
                if open_subs and open_subs[-1][0] == "tick":
                    quote = open_subs.pop()[1]
                else:
                    open_subs.append(("tick", quote))
                    quote = None
            elif char == "$" and after == "(":
                self.add(char, i)
                self.end_command()
                open_subs.append(("paren", quote))
                quote = None
                i += 1
            elif quote == '"':
                if char == '"':
                    quote = None
                elif char == "\\" and after in ("$", "`", '"', "\\"):
                    self.add(after, i)
                    i += 1
                elif char == "\\" and after == "\n":
                    self.line += 1
                    i += 1
                else:
                    self.add(char, i)
            elif char == "#" and self.at_word_start:
                end = text.find("\n", i)
                i = len(text) if end < 0 else end
                continue
            elif char == "\\" and after == "\n":
                self.line += 1
                i += 1
            elif char == "\\" and after:
                self.add(after, i)
                i += 1
            elif char in "'\"":
                quote = char
                self.started = True
                self.at_word_start = False
                if not self.open_command:
                    self.command_line = self.line
                    self.command_offset = i
                    self.open_command = True
            elif char == "(" and after == ")" and self.started:
                # `name()` opens a function; the parentheses belong to the word, not to a subshell.
                self.add("(", i)
                self.add(")", i)
                i += 1
            elif char == "(" or (char in "<>" and after == "("):
                if char != "(":
                    i += 1
                self.end_command()
                open_subs.append(("paren", None))
            elif char == ")":
                self.end_command()
                if open_subs and open_subs[-1][0] == "paren":
                    quote = open_subs.pop()[1]
            elif char in "<>":
                self.end_word()
                self.redirect_target = True
                while text[i + 1:i + 2] in ("<", ">", "&", "|"):
                    i += 1
            elif char in ";&|\n":
                self.end_command()
            elif char in " \t":
                self.end_word()
            else:
                self.add(char, i)
            i += 1
        self.end_command()
        return self.commands


def structure(commands: list[Command]) -> dict[str, int]:
    """Marks each command with the loop it sits in, and returns the line of the loop in each
    function that holds one — the loops a call of that function reaches."""
    loops: list[int] = []
    function: str | None = None
    function_depth = 0
    function_loop: dict[str, int] = {}
    for command in commands:
        first = command.words[0]
        if first == "done" and loops:
            loops.pop()
        command.loop = loops[-1] if loops else None
        if function is not None:
            if command.loop is not None and function not in function_loop:
                function_loop[function] = command.loop
            function_depth += command.words.count("{") - command.words.count("}")
            if function_depth <= 0:
                function = None
        elif FUNCTION_WORD.fullmatch(first):
            function = first[:-2]
            function_depth = command.words.count("{") - command.words.count("}")
        if first in LOOP_WORDS:
            loops.append(command.line)
    return function_loop


def reduce_words(words: list[str]) -> tuple[list[str], str | None, bool, int | None]:
    """The words of the command that actually runs, the retry wrapping it (`retry_fetch`, the one
    helper, or `retry`, a Dockerfile's inline definition), whether something bounds it, and the
    seconds of that bound where it is written as a number — rule 8's `<bound>`."""
    retry = None
    bounded = False
    bound: int | None = None
    words = list(words)
    while words:
        first = words[0]
        if ASSIGNMENT.match(first) or first in PASSTHROUGH or first in KEYWORDS:
            words.pop(0)
            continue
        if first == "timeout":
            words.pop(0)
            while words and words[0].startswith("-"):
                words.pop(0)
                if words and re.fullmatch(r"\d+", words[0]):
                    break
            if words and (match := re.fullmatch(r"(\d+)([smhd]?)", words[0])):
                words.pop(0)
                bounded = True
                if bound is None:
                    bound = int(match.group(1)) * SUFFIX[match.group(2)]
            continue
        if first == "retry_fetch":
            retry = "retry_fetch"
            words.pop(0)
            # `<what> <bound> --`: the bound is the one of those words that is a count of
            # seconds, and a call written without one bounds no attempt.
            for _ in range(3):
                if not words:
                    break
                word = words.pop(0)
                if re.fullmatch(r"[1-9]\d*", word):
                    bounded = True
                    if bound is None:
                        bound = int(word)
                if word == "--":
                    break
            continue
        if first == "retry" and len(words) > 2 and re.fullmatch(r"\d+", words[1]):
            retry = "retry"
            bounded = True
            if bound is None:
                bound = int(words[1])
            del words[:3]
            continue
        break
    return words, retry, bounded, bound


def subcommand(words: list[str]) -> str | None:
    """The first word that is not an option."""
    return next((word for word in words if not word.startswith("-")), None)


def gh_verb_of(rest: list[str]) -> str | None:
    """The verb a `gh` call runs, named `gh <subcommand>` or `gh <subcommand> <action>`, or None
    where that subcommand reads and writes nothing over the network. Every gh subcommand but
    GH_LOCAL and the four local `gh auth` actions reaches api.github.com over HTTPS, which is the
    read `gh api` already makes, so the default here is that a gh call is a fetch: a subcommand
    nobody listed is held to the floor rather than passing unseen."""
    words = [word for word in rest if not word.startswith("-")]
    sub = words[0] if words else None
    if sub is None or sub in GH_LOCAL:
        return None
    action = words[1] if len(words) > 1 else None
    if sub == "auth" and action in GH_AUTH_LOCAL:
        return None
    if sub not in GH_GROUPS:
        # `api`, `browse` and `status` take no action word: the word after them is an argument
        # (`gh api repos/owner/name`), and naming it in the verb would name every route.
        return f"gh {sub}"
    return f"gh {sub} {action}" if action else f"gh {sub}"


def verb_of(words: list[str]) -> str | None:
    """The fetch verb these words run, named as the list and the messages name it, or None."""
    if not words:
        return None
    # Its own options, which stop at a `--`: past that separator the words belong to a command
    # this one runs (`mise x "$pin" -- omp --version` resolves the tool and then asks it, and
    # that trailing `--version` says nothing about this command).
    own = words[1:words.index("--")] if "--" in words[1:] else words[1:]
    if any(word in INFORMATIONAL for word in own):
        return None
    name = PROGRAM.sub("", words[0]).rsplit("/", 1)[-1]
    rest = words[1:]
    if name in RUNNERS:
        return verb_of(rest)
    if name in ("curl", "wget") and any(word in ("-V", "-h") for word in rest):
        return None
    if name == "curl":
        return "curl"
    if name == "wget":
        return "wget"
    if name in ("apt-get", "apt", "aptitude"):
        sub = subcommand(rest)
        if sub not in APT_SUBCOMMANDS or any(word in APT_SIMULATE for word in rest):
            return None
        return f"apt-get {sub}"
    if name in ("pip", "pip3") and subcommand(rest) == "install":
        return "pip install"
    if name == "uv":
        sub = subcommand(rest)
        return f"uv {sub}" if sub in UV_SUBCOMMANDS else None
    if name == "bun":
        sub = subcommand(rest)
        return f"bun {sub}" if sub in BUN_SUBCOMMANDS else None
    if name in ("npm", "pnpm", "yarn"):
        sub = subcommand(rest)
        return f"{name} {sub}" if sub in NPM_SUBCOMMANDS else None
    if name == "playwright":
        # `install-deps` is the apt phase, which dispatch://INFRA-1443 owns: it is not this one.
        return "playwright install" if rest and rest[0] == "install" else None
    if name == "go":
        while len(rest) > 1 and rest[0] == "-C":
            del rest[:2]
        if rest[:2] == ["mod", "download"]:
            return "go mod download"
        if rest[:1] == ["install"]:
            return "go install"
        return None
    if name == "mise":
        sub = subcommand(rest)
        return f"mise {sub}" if sub in MISE_SUBCOMMANDS else None
    if name == "crane":
        sub = subcommand(rest)
        return f"crane {sub}" if sub in CRANE_SUBCOMMANDS else None
    if name == "docker":
        return "docker pull" if subcommand(rest) == "pull" else None
    if name == "gh":
        return gh_verb_of(rest)
    if name == "pulumi" and rest[:2] == ["plugin", "install"]:
        return "pulumi plugin install"
    return None


def silent_verb_of(words: list[str]) -> str | None:
    """The rule 7 mise subcommand these words run, or None. These are no fetch verbs: each exits
    0 when it cannot resolve the tool, so what the check does with one is refuse a retry."""
    if not words:
        return None
    own = words[1:words.index("--")] if "--" in words[1:] else words[1:]
    if any(word in INFORMATIONAL for word in own):
        return None
    if PROGRAM.sub("", words[0]).rsplit("/", 1)[-1] != "mise":
        return None
    sub = subcommand(words[1:])
    return f"mise {sub}" if sub in MISE_SILENT else None


def verb_in(words: list[str]) -> tuple[str, list[str]] | None:
    """The first fetch verb among these words, wherever it sits: what a call hands a loop that
    runs `"$@"`."""
    for index in range(len(words)):
        verb = verb_of(words[index:])
        if verb:
            return verb, words[index:]
    return None


def curl_options(words: list[str]) -> tuple[int | None, bool, bool, bool]:
    """What a curl carries: its `--retry` count, whether it passes `--retry-all-errors`, whether
    it bounds each attempt with `--max-time`, and whether a persistent error fails it (`--fail`,
    the `f` of a short cluster, or a read of the status it wrote out)."""
    retries = None
    all_errors = False
    bounded = False
    fails = "%{http_code}" in " ".join(words)
    index = 1
    while index < len(words):
        word = words[index]
        value = None
        if "=" in word and word.startswith("--"):
            word, _, value = word.partition("=")
        if word == "--retry":
            if value is None and index + 1 < len(words):
                value = words[index + 1]
            if value and value.isdigit():
                retries = int(value)
        elif word == "--retry-all-errors":
            all_errors = True
        elif word in ("--max-time", "-m"):
            bounded = True
        elif word in ("--fail", "--fail-with-body", "--fail-early"):
            fails = True
        elif re.fullmatch(r"-[A-Za-z]*f[A-Za-z]*", word):
            fails = True
        index += 1
    return retries, all_errors, bounded, fails


def curl_bound(words: list[str]) -> int | None:
    """The seconds a curl gives each of its own attempts, its `--max-time`, where that option
    carries a number: rule 8's `<bound>` for a curl that makes the attempts itself."""
    for index, word in enumerate(words):
        value = None
        if word.startswith("--max-time="):
            value = word.partition("=")[2]
        elif word in ("--max-time", "-m") and index + 1 < len(words):
            value = words[index + 1]
        if value and re.fullmatch(r"\d+", value):
            return int(value)
    return None


def apt_error_mode(words: list[str]) -> bool:
    """Whether this `apt-get update` fails when an index read fails, rather than warning and
    exiting 0: `-o APT::Update::Error-Mode=any`, in any spelling apt takes for an option. Option
    names are matched without regard to case, as apt reads them."""
    for index, word in enumerate(words):
        lowered = word.lower()
        value = None
        if lowered in ("-o", "--option"):
            value = words[index + 1] if index + 1 < len(words) else None
        elif lowered.startswith("-o"):
            value = word[2:].lstrip("=")
        elif lowered.startswith("--option="):
            value = word.partition("=")[2]
        if value and APT_ERROR_MODE.fullmatch(value.strip()):
            return True
    return False


class Site:
    """One network fetch: where it is, what verb it uses, what retries it, what bounds each of
    its attempts, and what it is in. `silent` marks rule 7's shape instead: a `mise x`, `mise
    exec`, `mise up` or `mise upgrade` with a retry around it, which is no fetch to hold to the
    floor but a retry to refuse."""

    def __init__(self, path: str, line: int, verb: str, step: str, words: list[str],
                 retry: str | None, bounded: bool, loop: int | None, bound: int | None = None,
                 silent: bool = False) -> None:
        self.path = path
        self.line = line
        self.verb = verb
        self.step = step
        self.words = words
        self.retry = retry
        self.bounded = bounded
        self.loop = loop
        self.bound = bound
        self.silent = silent


def arithmetic_of(site: Site) -> int:
    """Rule 8's arithmetic for one call: the wall time it takes to exhaust its attempts. Five at
    the helper's own bound with its sleeps of 10, 20, 40 and 80 s between them, or eight at
    curl's `--max-time` with curl's waits doubling from one second, where curl makes the
    attempts itself. A call whose bound is not written as a number adds nothing: there is no
    arithmetic to do, and rule 4 is what refuses an unbounded attempt."""
    if site.bound is None or site.silent:
        return 0
    if site.retry == "retry_fetch":
        return ATTEMPT_FLOOR * site.bound + HELPER_WAIT
    if site.verb == "curl" and site.retry is None:
        return (RETRY_FLOOR + 1) * site.bound + CURL_WAIT
    return 0


def scripts_called(text: str) -> set[str]:
    """The scripts under .github/scripts/ a shell body runs, whose own retried calls rule 8
    counts as this body's: a step that runs a script waits for every attempt that script makes.

    Read off the text by the path each call writes, with backslash continuations joined and
    comment lines left out, rather than off the commands this file scans elsewhere: a call
    reaches a script in command position, inside a `$(…)`, as the value of an assignment, behind
    a `source` or `.`, or out of a heredoc a `python3` runs, and the path is the one thing all
    of those write. A name SCRIPTS does not hold is nothing this check reads."""
    found = set()
    for line in text.replace("\\\n", " ").split("\n"):
        if line.lstrip().startswith("#"):
            continue
        for name in SCRIPT_CALL.findall(line):
            path = str(SCRIPTS / name)
            if path in script_files:
                found.add(path)
    return found


def commands_of(body: str,
                first_line: int) -> tuple[list[Command], dict[str, int], list[tuple[int, int]]]:
    """The simple commands of a shell body, the loop each local function holds, and the probe
    spans."""
    text, _ = without_heredocs(EXPRESSION.sub("EXPR", body))
    commands = Scanner(first_line).scan(text)
    return commands, structure(commands), probe_spans(text)


def sites_of(path: str, body: str, first_line: int, step: str) -> list[Site]:
    """Every fetch of one shell body."""
    commands, function_loop, probes = commands_of(body, first_line)
    found: list[Site] = []
    for command in commands:
        if any(start <= command.offset < end for start, end in probes):
            continue
        found += sites_of_command(path, command, function_loop, step)
    return unpacks_dropped(found)


def unpacks_dropped(found: list[Site]) -> list[Site]:
    """The sites with the apt unpack phases taken out. An `apt-get install` with no `-d` that
    follows an `apt-get install -d` in the same body installs an archive already on disk and
    takes no network: the download before it is the half that retries, which is what ci.yaml's
    Playwright step and e2e.yaml's package step each say where they do it."""
    downloaded = False
    kept = []
    for site in found:
        if site.verb == "apt-get install":
            only = any(word in APT_DOWNLOAD_ONLY for word in site.words)
            if downloaded and not only:
                continue
            downloaded = downloaded or only
        kept.append(site)
    return kept


def sites_of_command(path: str, command: Command, function_loop: dict[str, int],
                     step: str, retry: str | None = None, bounded: bool = False,
                     depth: int = 0, bound: int | None = None) -> list[Site]:
    words, wrapped, wrapped_bound, wrapped_seconds = reduce_words(command.words)
    retry = retry or wrapped
    bounded = bounded or wrapped_bound
    if bound is None:
        bound = wrapped_seconds
    if not words or depth > 3:
        return []
    name = PROGRAM.sub("", words[0]).rsplit("/", 1)[-1]
    if name in INTERPRETERS or name == "eval":
        script = None
        if name == "eval":
            script = " ".join(words[1:])
        else:
            rest = words[1:]
            while rest and rest[0].startswith(("-", "+")):
                flag = rest.pop(0)
                if not flag.startswith("--") and "c" in flag:
                    script = rest[0] if rest else None
                    break
        if script is None:
            return []
        inner = Scanner(command.line).scan(script)
        found = []
        for sub in inner:
            sub.line = command.line
            sub.offset = command.offset
            found += sites_of_command(path, sub, function_loop, step, retry, bounded, depth + 1,
                                      bound)
        return found
    if name in function_loop:
        # A loop that runs what it is handed: the verb among the call's words is that loop's fetch.
        held = verb_in(words[1:])
        if not held:
            return []
        verb, rest = held
        _, _, inner_bound, inner_seconds = reduce_words(rest)
        return [Site(path, command.line, verb, step, rest, retry, bounded or inner_bound,
                     function_loop[name], bound if bound is not None else inner_seconds)]
    silent = silent_verb_of(words)
    if silent:
        # Rule 7. A bare one is no fetch this check holds to anything; a retried one is the
        # shape the rule refuses, and it is recorded so the verdict can name it.
        if not retry:
            return []
        return [Site(path, command.line, silent, step, words, retry, bounded, command.loop,
                     bound, silent=True)]
    verb = verb_of(words)
    if not verb:
        return []
    if verb in ("curl", "wget") and loopback_only(words):
        return []
    if verb == "curl" and retry is None:
        # curl makes the attempts itself, and `--max-time` is what bounds each one of them.
        bound = curl_bound(words) or bound
    return [Site(path, command.line, verb, step, words, retry, bounded, command.loop, bound)]


def loopback_only(words: list[str]) -> bool:
    """Whether every address this fetch names is the loopback: a readiness poll of a process the
    step itself started, which is no download."""
    urls = [word for word in words if "://" in word]
    if not urls:
        return False
    return all(any(host in url for host in LOOPBACK) for url in urls)


# ---------------------------------------------------------------- the exceptions


class Exemption:
    """One site R2 excepts, by the file, the job and the step it sits in and the verbs it covers.
    `why` is what a reader of a stale entry is told the entry was for."""

    def __init__(self, path: str, job: str | None, step: str | None,
                 verbs: tuple[str, ...] | None, why: str) -> None:
        self.path = path
        self.job = job
        self.step = step
        self.verbs = verbs
        self.why = why
        self.used = 0

    def covers(self, site: Site, job: str | None) -> bool:
        return (
            site.path == self.path
            and (self.job is None or self.job == job)
            and (self.step is None or self.step == site.step)
            and (self.verbs is None or site.verb in self.verbs)
        )


GHCR_WHY = ("a ghcr.io pull of a package this repository publishes: a failed pull is a fault of "
            "this repository's own package and must be seen, not retried over")
EXEMPTIONS = [
    Exemption(".github/actions/pull-test-images/action.yml", None, None,
              ("docker pull",), GHCR_WHY),
    Exemption(".github/workflows/ci.yaml", "envoy-client",
              "Pre-pull the NATS image the credential tests run", ("docker pull",), GHCR_WHY),
    Exemption(".github/workflows/ci.yaml", "download-retry-images",
              "Pull every base image the three builds read", ("docker pull",), GHCR_WHY),
    # A script site is read with no job and no step, so this entry names neither: the call at the
    # `for path in scripts:` loop below passes job=None and step=None.
    Exemption(".github/scripts/playwright-install.sh", None, None,
              ("apt-get update", "apt-get install"),
              "the apt phase of this script is dispatch://INFRA-1443's: its own attempts() loop "
              "switches origin after two attempts, which the shared helper cannot do"),
]


def credentialed_jobs(path: str, document: Mapping) -> set[str]:
    """The jobs of a release workflow that hold a credential, by check-release-credentials.sh's
    own definition: a write scope of WRITE_SCOPES, or the deploy key."""
    if path not in RELEASE_WORKFLOWS:
        return set()
    found = set()
    for name, job in (document.get("jobs") or {}).items():
        if not isinstance(job, Mapping):
            continue
        permissions = job.get("permissions")
        scopes = permissions if isinstance(permissions, Mapping) else {}
        # repr, not a YAML dump: the loader's mappings are a subclass no dumper represents, and
        # what is wanted is only whether the job's text names the key anywhere in it.
        if any(scopes.get(scope) == "write" for scope in WRITE_SCOPES) or DEPLOY_KEY.search(
            repr(job)
        ):
            found.add(name)
    return found


# ---------------------------------------------------------------- the reading set

files = walk()
workflows = [
    path for path in files
    if Path(path).parent == WORKFLOWS and Path(path).suffix in {".yml", ".yaml"}
]
actions = [
    path for path in files
    if ACTIONS in Path(path).parents and Path(path).name in {"action.yml", "action.yaml"}
]
scripts = [
    path for path in files
    if Path(path).parent == SCRIPTS
    and path.endswith(".sh")
    and not path.endswith(TEST_HARNESS)
]
for dockerfile in DOCKERFILES:
    if dockerfile not in files:
        problem(SELF, 1,
                f"{dockerfile} is one of the three Dockerfiles a CI job builds and this check "
                "reads, and the tree does not hold it. Point DOCKERFILES at where it moved, or "
                "drop it if CI no longer builds it.")

sites: list[Site] = []
exempted: list[Site] = []
# The scripts a credentialed publish job's steps call, and then the closure of that set: the
# loops below fill the first and the walk between them turns it into the second.
credentialed_roots: set[str] = set()
credentialed_scripts: set[str] = set()


class Budget:
    """One step that carries its own `timeout-minutes`, which rule 8 holds above the attempts it
    allows: where the budget is written, what the step is called, the minutes it gives, the
    arithmetic of the calls the step's own `run:` holds, the local composite action a `uses:`
    step runs, whose calls are the step's too, and the scripts the step's `run:` invokes, whose
    calls are the step's too for the same reason."""

    def __init__(self, path: str, line: int, step: str, minutes: int, cost: int,
                 uses: str | None, scripts: set[str]) -> None:
        self.path = path
        self.line = line
        self.step = step
        self.minutes = minutes
        self.cost = cost
        self.uses = uses
        self.scripts = scripts


budgets: list[Budget] = []
# The arithmetic of each composite action's own `run:` steps, and the local actions and the
# scripts each one calls in turn: a `uses:` step of one carries the whole of it.
action_cost: dict[str, int] = {}
action_uses: dict[str, list[str]] = {}
action_scripts: dict[str, set[str]] = {}
action_by_directory = {str(Path(path).parent): path for path in actions}
action_files = set(actions)
# The arithmetic of each script's own calls, and the scripts it calls in turn: whoever runs one
# of them waits for the whole of it.
script_cost: dict[str, int] = {}
script_calls: dict[str, set[str]] = {}
script_files = set(scripts)


def local_action(uses: object) -> str | None:
    """The action.yml a `uses:` names, where it is one of this repository's own composite
    actions. A third-party reference and a called workflow are neither of them read: exception
    (b) above, and a called workflow's jobs carry their own budgets."""
    if not isinstance(uses, str) or not uses.startswith("./"):
        return None
    return action_by_directory.get(uses[2:].rstrip("/"))


def steps_of(path: str, document: Mapping) -> list[tuple[str | None, str, Mapping]]:
    """The steps of a workflow's jobs and of a composite action's `runs`, each with the job it
    sits in (None in an action) and the name a problem calls it by."""
    found = []
    holders: list[tuple[str | None, object]] = []
    runs = document.get("runs")
    if isinstance(runs, Mapping):
        holders.append((None, runs.get("steps")))
    for name, job in (document.get("jobs") or {}).items():
        if isinstance(job, Mapping):
            holders.append((name, job.get("steps")))
    for job, steps in holders:
        if not isinstance(steps, list):
            continue
        for step in steps:
            if not isinstance(step, Mapping):
                continue
            name = step.get("name") or step.get("id") or "-"
            found.append((job, str(name), step))
    return found


for path in workflows + actions:
    try:
        document = yaml.load(Path(path).read_text(), Loader=LineLoader)
    except yaml.YAMLError as error:
        problem(path, 1, f"this file is not valid YAML, so its downloads cannot be read: {error}")
        continue
    if not isinstance(document, Mapping):
        continue
    credentialed = credentialed_jobs(path, document)
    for job, name, step in steps_of(path, document):
        cost = 0
        called: set[str] = set()
        if isinstance(step.get("run"), str):
            called = scripts_called(step["run"])
            for site in sites_of(path, step["run"], step.lines.get("run", 0), name):
                cost += arithmetic_of(site)
                if job in credentialed:
                    exempted.append(site)
                    continue
                covering = next((entry for entry in EXEMPTIONS if entry.covers(site, job)), None)
                if covering:
                    covering.used += 1
                    exempted.append(site)
                    continue
                sites.append(site)
        if job in credentialed:
            credentialed_roots.update(called)
        uses = local_action(step.get("uses"))
        if path in action_files:
            action_cost[path] = action_cost.get(path, 0) + cost
            action_scripts.setdefault(path, set()).update(called)
            if uses:
                action_uses.setdefault(path, []).append(uses)
        minutes = step.get("timeout-minutes")
        if isinstance(minutes, int) and not isinstance(minutes, bool):
            budgets.append(Budget(path, step.lines.get("timeout-minutes", 0), name, minutes,
                                  cost, uses, called))

# Exception (d) for a script: a *.sh under .github/scripts/ that a credentialed publish job runs
# can carry no retry either, because the retry is `. .github/scripts/ci-retry.sh` and that source
# runs inside the credentialed job, where check-release-credentials.sh's rule 1(a) allows no
# dependency code — a command whose path holds a `scripts/` component other than release-push.sh
# is dependency code by its own dependency_command(). Read from the tree, like (b): the roots are
# the scripts such a job's steps call, and the closure below adds the scripts those call in turn,
# the same reading rule 8 makes of a step. It follows a call it can see rather than the branch a
# job takes, so it reaches a script a credentialed job could run; the sites it covers are named
# in exception (d) of this file's header.
while credentialed_roots:
    path = credentialed_roots.pop()
    if path in credentialed_scripts:
        continue
    credentialed_scripts.add(path)
    credentialed_roots.update(scripts_called(Path(path).read_text()))

for path in scripts:
    text = Path(path).read_text()
    script_calls[path] = scripts_called(text)
    for site in sites_of(path, text, 1, "-"):
        script_cost[path] = script_cost.get(path, 0) + arithmetic_of(site)
        if path in credentialed_scripts:
            exempted.append(site)
            continue
        covering = next((entry for entry in EXEMPTIONS if entry.covers(site, None)), None)
        if covering:
            covering.used += 1
            exempted.append(site)
            continue
        sites.append(site)
    # Rule 6: a `urllib.request` read inside this script's Python goes through the one reader.
    _, heredocs = without_heredocs(EXPRESSION.sub("EXPR", text))
    indents: list[tuple[int, str]] = []
    for line, body in heredocs:
        if match := PYTHON_DEF.match(body):
            indent = len(match.group(1))
            indents = [entry for entry in indents if entry[0] < indent]
            indents.append((indent, match.group(2)))
        elif body.strip() and not body.startswith((" ", "\t")):
            indents = []
        if not PYTHON_READ.search(body):
            continue
        inside = indents[-1][1] if indents else None
        if inside == PYTHON_READER and path == PYTHON_READER_FILE:
            continue
        problem(path, line,
                f"this read goes straight to urllib.request. Every Python read of a CI script "
                f"goes through {PYTHON_READER_FILE}'s {PYTHON_READER}(), which carries R1's floor "
                f"in that check's idiom: {ATTEMPT_FLOOR} attempts, waits of 10, 20, 40 and 80 "
                f"seconds, each bounded by the read's own timeout. One definition, no copies.")

# ---------------------------------------------------------------- the Dockerfiles


def instructions(text: str) -> list[tuple[int, str, str]]:
    """Each instruction of a Dockerfile as (line, keyword, the text after the keyword) with its
    backslash continuations and the comment lines between them kept, so a scan of that text reads
    the same lines the build runs."""
    found = []
    lines = text.split("\n")
    index = 0
    while index < len(lines):
        line = lines[index]
        start = index + 1
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            index += 1
            continue
        body = [line]
        while body[-1].rstrip().endswith("\\") and index + 1 < len(lines):
            index += 1
            body.append(lines[index])
        index += 1
        whole = "\n".join(body)
        keyword, _, rest = whole.partition(" ")
        found.append((start, keyword.upper(), rest))
    return found


def joined(text: str) -> str:
    """One line, continuations joined and each run of blank space one space: the shape the
    canonical retry is compared in."""
    return " ".join(text.replace("\\\n", " ").split())


for path in DOCKERFILES:
    if path not in files:
        continue
    for line, keyword, rest in instructions(Path(path).read_text()):
        if keyword != "RUN":
            continue
        while match := re.match(r"\s*--(mount|network|security)=\S+", rest):
            rest = rest[match.end():]
        found = sites_of(path, rest, line, "-")
        if not found:
            continue
        sites += found
        flat = joined(rest)
        opens = flat.startswith(CANONICAL)
        if "retry()" in flat and not opens:
            problem(path, line,
                    "this RUN defines a retry loop of its own text. Every network RUN of the three "
                    "Dockerfiles opens with one canonical definition, byte for byte, so a reader "
                    "sees one loop and not a family of them. The text is in "
                    ".github/scripts/check-download-retry.sh's CANONICAL.")
        elif not opens and any(site.retry == "retry" for site in found):
            problem(path, line,
                    "this RUN calls `retry` and does not open with the canonical definition of it. "
                    "The text is in .github/scripts/check-download-retry.sh's CANONICAL.")

# ---------------------------------------------------------------- the verdict

for site in sites:
    where = f"`{site.verb}`" + (f" in step '{site.step}'" if site.step != "-" else "")
    dockerfile = site.path in DOCKERFILES
    if site.silent:
        problem(site.path, site.line,
                f"{where} is retried, and that retry protects nothing: `mise x`, `mise exec`, "
                "`mise up` and `mise upgrade` exit 0 when they cannot resolve the tool. `mise x` "
                "warns (`Failed to resolve tool version list for …`) and then runs whatever "
                "binary of that name is already on PATH — a different build, at a version nobody "
                "asked for — and `mise up` says `All tools are up to date` having read nothing. "
                "So the retry waits for a failure that never comes, and the first attempt "
                "succeeds on a tool it never fetched. The cure is `mise install <tool>` under "
                "the retry, which exits 1 on the failed fetch, with the resolution after it: "
                "once the install has succeeded, a bare `mise x <tool> -- sh -c 'command -v "
                "<binary>'` reads the path off disk and fetches nothing.")
        continue
    if site.verb == "curl":
        retries, all_errors, bounded, fails = curl_options(site.words)
        if site.retry:
            if not site.bounded:
                problem(site.path, site.line,
                        f"{where} is retried and no attempt of it is bounded in time. A host that "
                        "accepts the connection and then sends nothing would hold one attempt for "
                        "the job's whole budget. Pass `--max-time <seconds>`.")
        elif retries is None or retries < RETRY_FLOOR or not all_errors or not bounded or not fails:
            missing = []
            if retries is None:
                missing.append(f"`--retry {RETRY_FLOOR}` (it passes none)")
            elif retries < RETRY_FLOOR:
                missing.append(f"`--retry {RETRY_FLOOR}` (it passes `--retry {retries}`, whose "
                               "last attempt starts well inside 120 s: curl's waits double from "
                               f"one second, so only {RETRY_FLOOR} reaches 127 s)")
            if not all_errors:
                missing.append("`--retry-all-errors` (without it a connection reset is not a retry)")
            if not bounded:
                missing.append("`--max-time <seconds>`, which curl resets for each attempt")
            if not fails:
                missing.append("`--fail`, usually the `f` of `-fsSL` (without it curl exits 0 on a "
                               "persistent 504 and writes the error body to the output)")
            problem(site.path, site.line,
                    f"{where} fetches without the floor: it lacks " + ", ".join(missing) +
                    ". Pass curl's own options, or wrap the call in `retry_fetch` from "
                    f"{HELPER}.")
        continue
    if site.verb == "apt-get update" and not apt_error_mode(site.words):
        problem(site.path, site.line,
                f"{where} passes no `-o APT::Update::Error-Mode=any`, so a failed index read "
                "leaves it reporting success. apt treats every index it could not fetch as a "
                "warning (`W: Failed to fetch … 504 Gateway Timeout`, then `W: Some index files "
                "failed to download. They have been ignored, or old ones used instead.`) and "
                "exits 0, so the retry around it stops after one attempt having read no list, "
                "and the `apt-get install` after it fails with `has no installation candidate` "
                "against a package list that will never name the package — which no further "
                "attempt can cure. The option makes each of those warnings an error, so the "
                "command exits non-zero and the retry has something to retry. It is apt's "
                "`--fail`.")
    if dockerfile:
        if site.retry != "retry":
            problem(site.path, site.line,
                    f"{where} fetches inside a Dockerfile RUN with no retry around it. Wrap it in "
                    "the canonical inline loop this file's network RUN lines open with: "
                    "`retry <bound in seconds> \"<what>\" <command...>`.")
        continue
    if site.retry != "retry_fetch":
        problem(site.path, site.line,
                f"{where} fetches with no retry. Source {HELPER} and call "
                "`retry_fetch \"<what>\" <bound in seconds> -- <command...>`: five attempts, the "
                "last starting 150 s after the first, each under `timeout <bound>`.")
        continue
    if not site.bounded:
        problem(site.path, site.line,
                f"{where} is retried and no attempt of it is bounded in time. Give `retry_fetch` "
                "a per-attempt bound in seconds.")

# Rule 2: a loop that holds a fetch is a copy of the one helper. Not in a Dockerfile, whose
# canonical definition is itself a loop and which carries no helper file to copy.
seen_loops: set[tuple[str, int]] = set()
for site in sites:
    if site.loop is None or site.retry == "retry_fetch" or site.path in DOCKERFILES:
        continue
    if (site.path, site.loop) in seen_loops:
        continue
    seen_loops.add((site.path, site.loop))
    problem(site.path, site.loop,
            f"this loop retries `{site.verb}` itself, which makes it a second copy of the retry "
            f"{HELPER} holds. One rule holds the attempts (dispatch://INFRA-1545, R2): delete the "
            "loop and call `retry_fetch \"<what>\" <bound in seconds> -- <command...>`.")

# Rule 8: a retried step's own budget strictly exceeds the attempts it allows, so the last
# attempt's own error reaches the log before the runner's kill does.


def script_arithmetic(path: str, seen: set[str]) -> int:
    """The arithmetic a call of this script carries: its own calls and those of every script it
    calls in turn. `seen` is what the step has counted already, so a script two of its calls
    reach is counted once, and a cycle — which the shell would run until something failed, but
    which must not hang this check — stops."""
    if path in seen:
        return 0
    seen.add(path)
    total = script_cost.get(path, 0)
    for target in sorted(script_calls.get(path, ())):
        total += script_arithmetic(target, seen)
    return total


def action_arithmetic(path: str, seen: set[str]) -> int:
    """The arithmetic a `uses:` of this composite action carries: its own calls, the scripts its
    steps run, and every local action it calls in turn, which carries its own the same way.
    `seen` stops a cycle, which GitHub would refuse to run but which must not hang this check."""
    if path in seen:
        return 0
    seen.add(path)
    total = action_cost.get(path, 0)
    for target in sorted(action_scripts.get(path, ())):
        total += script_arithmetic(target, seen)
    for target in action_uses.get(path, ()):
        total += action_arithmetic(target, seen)
    return total


for budget in budgets:
    # One `seen` for the whole step: the script its `run:` calls, the action it uses and that
    # action's own scripts are one reading set, and a file two of those paths reach is one file
    # the step runs once.
    seen: set[str] = set()
    cost = budget.cost
    for target in sorted(budget.scripts):
        cost += script_arithmetic(target, seen)
    if budget.uses:
        cost += action_arithmetic(budget.uses, seen)
    if not cost or 60 * budget.minutes > cost:
        continue
    followed = sorted(target for target in seen if target in script_files)
    through = (f" That arithmetic is the step's own and that of {', '.join(followed)}, which it "
               "runs: a step waits for every attempt the scripts it calls make." if followed
               else "")
    problem(budget.path, budget.line,
            f"step '{budget.step}' carries `timeout-minutes: {budget.minutes}`, "
            f"{60 * budget.minutes} s, and its own retries take {cost} s to exhaust, which that "
            f"budget does not strictly exceed.{through} A budget its own retries reach is a race "
            "between the last attempt's error and the runner's kill, and the retry can lose it: "
            "in run 42386541065's job 128445139635 a `gh api` read bounded at 30 s under "
            "`timeout-minutes: 5` — 5 x 30 + 150 = 300 s against 300 s — printed "
            "`attempt 5 of 5 reached its 30s bound` at 17:22:16.7444966Z and was killed at "
            "17:22:16.7483230Z, four milliseconds later. Lose that race and the step reports "
            "GitHub's bare `The action '…' has timed out after N minutes.` rather than naming "
            "the site and its attempts, which is the loud failure the retry exists to give. "
            f"GitHub's granularity is the minute, so write `timeout-minutes: {cost // 60 + 1}` "
            "or more.")

for entry in EXEMPTIONS:
    if entry.used:
        continue
    problem(SELF, 1,
            f"the exception for {entry.path}"
            + (f" job '{entry.job}'" if entry.job else "")
            + (f" step '{entry.step}'" if entry.step else "")
            + " matches no fetch in the tree. An exception outliving its site is an exception "
            "nobody reads again: drop it, or point it at where that site moved. It was written "
            f"for {entry.why}.")

if LIST_MODE and not problems:
    # A reader of this list pipes it (`| head`, `| cut`), so a closed pipe ends this the way any
    # other tool ends: on the signal, with no traceback.
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    for site in sorted(sites, key=lambda site: (site.path, site.line)):
        print(f"{site.path}\t{site.line}\t{site.verb}\t{site.step}")
    print(f"{len(sites)} site(s)", file=sys.stderr)
    raise SystemExit(0)

if problems:
    for line in problems:
        print(line)
    print(f"{len(problems)} download(s) outside the one retry rule", file=sys.stderr)
    raise SystemExit(1)

print(
    f"every network fetch in CI retries at the floor: {len(sites)} site(s) over "
    f"{len(workflows)} workflow(s), {len(actions)} composite action(s), {len(scripts)} script(s) "
    f"and {len(DOCKERFILES)} Dockerfile(s), with {len(exempted)} site(s) the exceptions name"
)
PY
