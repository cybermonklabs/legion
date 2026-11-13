"""What each ci.yaml job reads that check-ci-filters.sh cannot find by itself: the data it checks
the changes job's filters against (ci_filter_coverage.py is the engine; check-ci-filters.sh's
header says what is proved).

  GATES     one Gate per filter: the job (or step) it gates, the go commands the job runs, and
            the files its other steps read. Each go command is pinned to the text that starts it:
            `step=` a step of the job whose run: holds `text`, or `source=` a script that holds it.
            A step whose go commands are not in the simple form is an `exact=` entry instead
            (pinned_step): its whole run: script and directory, and the Runs that stand for what
            it compiles. The check fails when a run: step of a ci.yaml job - or of a job of a
            local workflow that job calls, whose steps it reads the same way - starts a go
            command no Run or exact entry claims, and when a pin's text is gone, so the table
            cannot drift from ci.yaml silently. A claimed command the check does not reduce (check-ci-filters.sh's
            header says which it does), and every Run of an exact entry whose step no longer holds
            its script, counts as reading every Go file of the repository and every file under a
            Go module's directory; a script's pin is its whole line.
  RULES     a source file -> what it does when a test run compiles it: a go command it starts (a
            Run, whose closure is added) or a file it reads outside its package (a Read). A
            _test.go file counts when go list lists it in a tested package under the job's tags;
            any other file when it is compiled into a test run's closure. These are found by
            reading the tests, not by the check: a new read through `..` or a new go command a
            test starts is added here by the change that makes it.
  EXCLUDED  filters the check leaves to another: worker_image gates worker-image.yaml's image
            build, whose every input .github/scripts/check-image-trigger-paths.sh proves its
            filter covers. (envoy_smoke's image inputs are that check's too, but its job's go
            commands and other step reads are a gate here.)
  PUSH_WORKFLOWS  filter -> a workflow that runs that filter's job's work on a push to main in a
            workflow of its own, since ci.yaml runs on no push, selected by its `on.push.paths`,
            which must equal the filter: the filter decides the pull request's and merge group's
            run, the paths main's.
  TAG_RUNS  one entry per step that runs a package under a build tag and passes -run: the job
            whose untagged run of that package that -run leans on, whose filter must therefore
            cover every file the tag step reads (the read set of the gate on its filter).

A Read's glob is expanded over the repository's files; a why cites the step (at()) or the
source line that reads it - the line that performs the read, not the one naming its path, and
never a line of the file the why itself lives in, which moves with every line added above it:
inside its own file a comment names the filter, the anchor, the glob or the path instead, which
a grep resolves. at()'s third argument, and a gate's `workflow`, is the workflow file
that holds the job's steps, and it is the job's own called file (job_file): ci.yaml's job of
each gated id keeps the gate and calls .github/workflows/ci-job-<id>.yaml, which holds the
steps. The one gate whose steps are elsewhere names its file itself: ci.yaml's docs job calls
docs-site.yaml, whose job carries an id of its own, so that gate stays on ci.yaml, the file its
own job id is in, and cites docs-site.yaml's build job (at_docs). The Read lists of the jobs
without Go steps (the Bun packages) are as hand-kept as RULES: the check proves the filter
covers them, not that they are complete.
"""

import json
import os
import re
from pathlib import Path
from typing import NamedTuple

from ci_filter_coverage import CACHE_WORKFLOW, Read, TagRun, exact, go_skips_dir, run
from ci_filter_coverage import at as at_in, gate as gate_in

ENVOY = "packages/envoy"
DAEMON = "packages/daemon"
INFRA = "packages/envoy/infra"
FIX = "packages/contracts/fixtures"
GO_TARGETS = ".github/go-release-targets.json"
CLIENT_TARGETS = ".github/agent-secrets-client-targets.json"
INFRA_TARGETS = ".github/envoy-infra-targets.json"
NOTICES = "scripts/go-third-party-notices.sh"
NOTICES_LOOPS = ".github/scripts/go-release-notices.sh"
CI = ".github/workflows/ci.yaml"
DOCS_SITE = ".github/workflows/docs-site.yaml"
BROWSER_SITE = ".github/workflows/dispatch-browser.yaml"
BROWSER_NIGHTLY = ".github/workflows/dispatch-browser-nightly.yaml"


def job_file(job: str) -> str:
    """The workflow file that holds a job's steps. ci.yaml's job of that id keeps the gate - its
    `needs: changes` and its whole `if:` - and calls .github/workflows/ci-job-<id>.yaml, which
    holds the steps, so that is where a gate's step, an Exact entry's, a TAG_RUNS row's and a
    why's citation are looked for."""
    return f".github/workflows/ci-job-{job}.yaml"


def at(job, step, workflow=""):
    """A citation of a step, in the file that holds it: the job's own called file, unless the
    caller names another (at_docs, for the build job ci.yaml's docs job calls)."""
    return at_in(job, step, workflow or job_file(job))


def gate(filter, job, runs=(), reads=(), step="", exact=(), workflow=""):
    """A gate, whose job holds its steps in its own called file unless another file is named.
    The job is ci.yaml's, which keeps the `if:` the filter gates whichever file holds the
    steps.

    Every such gate reads two files: ci.yaml, where every job's `if:` is, and the job's own
    ci-job-<id>.yaml, where the steps it runs are. That pair is appended here rather than
    written out on each gate, so a gate's own `reads` say only what is its own. The two gates
    that name a file - docs and dispatch_browser, whose called workflows carry job ids of their
    own - declare the pair they read themselves."""
    if not workflow:
        reads = list(reads) + CI_SELF + job_self(job)
    return gate_in(filter, job, runs, reads, step, exact, workflow or job_file(job))


def targets(path: str, key: str) -> list:
    return json.loads(Path(path).read_text())[key]


def step_run(job, step, text, workdir, pkgs, **options):
    """A go command a ci.yaml step starts, pinned to the step and its text."""
    return run(workdir, pkgs, f"{at(job, step)}: {text}", step=step, text=text, **options)


def script_run(source, text, started_by, workdir, pkgs, **options):
    """A go command a script starts, pinned to the script and its text."""
    return run(workdir, pkgs, f"{source} ({started_by}): {text}", source=source, text=text, **options)


def pinned_step(job, step, workdir, script, runs, workflow=""):
    """A step pinned to its whole run: script and the directory it runs in (an Exact entry). Each
    of `runs`, (workdir, packages, what the script compiles there, run options), stands for a go
    command of the script; together they must name all it compiles. `job` and `workflow` are the
    job and the file the citation names, the job's own called file unless another is given: the
    step of a gate whose ci.yaml job calls a workflow holding jobs of its own
    (dispatch-browser.yaml) is in that workflow under one of those, while the entry stays on the
    gate."""
    return exact(step, workdir, script, [run(where, pkgs, f"{at(job, step, workflow)} (its pinned script): {what}", **options)
                                         for where, pkgs, what, options in runs])


def reads(*entries):
    return [Read(glob, why) for glob, why in entries]


def envoy_go_files_but_infra():
    """The .go files internal/testnats/nats_test.go's dockerDriverFiles opens: every one under
    packages/envoy but infra's, node_modules' and testdata's, test files included, as one
    `<dir>/**/*.go` per top-level directory that holds one and packages/envoy/*.go if the root
    does. A glob for a directory holding no .go file would name no file, which rule 9 refuses, and
    out - where packages/dispatch/e2e/run-broker.sh builds the broker binaries the browser suite
    starts - holds binaries alone."""
    def holds_go(directory: Path) -> bool:
        for current, subdirs, names in os.walk(directory):
            subdirs[:] = [s for s in subdirs if s not in {"node_modules", "testdata"}]
            if any(n.endswith(".go") for n in names):
                return True
        return False

    globs = [f"{ENVOY}/*.go"] if any(Path(ENVOY).glob("*.go")) else []
    for entry in sorted(Path(ENVOY).iterdir()):
        if entry.is_dir() and entry.name not in {"infra", "node_modules", "testdata"} and holds_go(entry):
            globs.append(f"{ENVOY}/{entry.name}/**/*.go")
    return globs


# ---------------------------------------------------------------- shared step reads
def root_workspaces():
    """Every workspace the root manifest's `workspaces` lists, relative to the repository root."""
    return [workspace.removesuffix("/") for workspace in targets("package.json", "workspaces")]


# The root manifest's own list: a workspace added to or deleted from package.json's workspaces
# needs no edit here.
WORKSPACES = root_workspaces()


def setup_bun(job, workflow=""):
    """.github/actions/setup-bun alone: the Bun a job runs its own file with, where no step of it
    installs the workspace's dependencies. The one job on this read without bun_job below is
    sessions-script, whose test and the script it spawns import node: builtins and bun alone."""
    return reads(
        (".github/actions/setup-bun/**", at(job, "./.github/actions/setup-bun", workflow)),
        (".bun-version", ".github/actions/setup-bun/action.yml:106 bun-version-file: .bun-version, and :84 and :137 "
                         "read the version it pins to tell a Bun of that version from any other"),
        (".github/ci-tools.yaml", ".github/actions/setup-bun/action.yml:119-133 chooses the bun row of this runner's "
                                  "build variant, :174 fetches that row's second source where no Bun of the pinned "
                                  "version is installed, and :208 holds the bun on PATH to that row's digest"),
        (".github/scripts/ci-tools.sh", ".github/actions/setup-bun/action.yml:115,194 source it: ci_tools_source_name "
                                        "reads the word for the source that served the bytes, ci_tools_fetch_fallback "
                                        "installs the npm copy and ci_tools_verify checks the installed binary"),
    )


def bun_job(job, workflow=""):
    """setup_bun and a frozen `bun install`. No Bun configuration file and no .npmrc anywhere."""
    install = at(job, "bun install --frozen-lockfile", workflow)
    return setup_bun(job, workflow) + reads(
        ("package.json", f"{install} reads the root manifest (workspaces package.json:19-31, patchedDependencies :52-60)"),
        ("bun.lock", f"{install} reads the lockfile to install the versions it records"),
        ("patches/**", f"{install} applies package.json:52-60 patchedDependencies"),
        ("packages/proof-editor/proof-sdk-*.tgz",
         f"{install} installs proof-sdk-upstream from it, the file: dependency of packages/proof-editor/package.json"),
        *[(f"{w}/package.json", f"{install}: package.json:19-31 lists {w}; a frozen install reads every workspace manifest")
          for w in WORKSPACES],
    )


def biome(where):
    return reads(
        ("biome.json", f"{where}: Biome reads the root config"),
        (".gitignore", f"{where}: biome.json:3-6 vcs.useIgnoreFile reads the root .gitignore"),
    )


def install_jj(job):
    return reads(
        (".github/actions/install-jj/**", at(job, "./.github/actions/install-jj")),
        (f"{DAEMON}/docker/worker.Dockerfile", ".github/actions/install-jj/action.yml:60 reads ARG JJ_TOOL, the tool spec mise installs"),
        (".github/ci-tools.yaml", ".github/actions/install-jj/action.yml:42-43 installs the mise row's archive, and "
                                  ":74,83 hold the jj binary to the jj row's digest, whichever source served it"),
        (".github/scripts/ci-tools.sh", ".github/actions/install-jj/action.yml:39,58 source it: ci_tools_fetch installs "
                                        "mise, ci_tools_fetch_fallback reads the jj row's second source and "
                                        "ci_tools_verify checks the binary"),
    )


def install_omp(job):
    return reads(
        (".github/actions/install-omp/**", at(job, "./.github/actions/install-omp")),
        (".omp-pin", ".github/actions/install-omp/action.yml:16 pin=$(<.omp-pin)"),
    )


def setup_go(job, gomod, workflow=""):
    return reads((gomod, f"{at(job, 'actions/setup-go', workflow)} go-version-file"))


CI_SELF = reads((CI, "a job reads its own definition"))


def job_self(job):
    """The file holding a job's steps, which ci.yaml's job of that id calls: the job reads the
    definition of the steps it runs, as it reads ci.yaml for the `if:` that selects it, so each
    moved job's gate declares this beside CI_SELF and its filter names the file. That is what
    makes a change to one ci-job-<id>.yaml select that job and no other."""
    return reads((job_file(job), "the job reads the definition of the steps it runs"))


# ci.yaml's docs job and docs-pages.yaml's build job both call docs-site.yaml, which holds the
# steps. The gate stays on ci.yaml's docs job, whose if: the docs filter gates, and every step
# citation names docs-site.yaml's build job.
DOCS_JOB = "build"
DOCS_SITE_SELF = reads((DOCS_SITE, "the docs build reads the definition of the steps it runs"))


def at_docs(step):
    return at(DOCS_JOB, step, DOCS_SITE)


# ci.yaml's dispatch-browser job and dispatch-browser-nightly.yaml's own job both call
# dispatch-browser.yaml, which holds the steps. The gate stays on ci.yaml's dispatch-browser
# job, whose if: the dispatch_browser filter gates, and every step citation names the job of
# dispatch-browser.yaml that holds the step.
BROWSER_JOB = "dispatch-browser"
BROWSER_BINARIES, BROWSER_SHARD = "binaries", "shard"
BROWSER_ALONE, BROWSER_MP, BROWSER_MERGE = "alone", "multiprocess", "merge"
BROWSER_EMBEDDER = "embedder"
BROWSER_VISUAL = "visual"
BROWSER_SITE_SELF = reads((BROWSER_SITE, "the browser suite reads the definition of the steps it runs"))


def at_browser(step, job=BROWSER_SHARD):
    return at(job, step, BROWSER_SITE)


CONTRACTS_SRC = "packages/contracts/src/**"
ENVOY_CLIENT_SRC = "packages/envoy-client/src/**"
BROKER_CLIENT_SRC = "packages/broker-client/**"
# The shared test fixtures every other Bun package's tests import.
TEST_FIXTURES_SRC = "packages/test-fixtures/src/**"


def envoy_client_dep(why):
    """The reads of a gate whose job compiles @legion/envoy-client: that package's sources, and
    packages/broker-client's with them. envoy-client's src/broker-credential.ts:21-22 imports
    @legion/broker-client/environment and /token-client, and that package's exports map every
    entry to ./src/<module>.ts, so all seven of its sources land in the same program - measured
    with `bunx tsc --listFilesOnly`, which counts seven of them in each of claude-envoy,
    envoy-client, pi-envoy, pi-legion, pi-shared and scripts/dispatch, and in the closures of
    envoy-client's bin/dispatch.ts, src/delivery.ts and src/transport.ts. Rule 14 holds the
    workspace *manifests*, which is another thing: a job can compile a workspace whose sources
    its filter never selects, and then the change that breaks that job starts nothing. This is
    one place rather than one entry per gate, so the next workspace envoy-client depends on
    reaches every one of those filters at once."""
    return reads(
        (ENVOY_CLIENT_SRC, why),
        (BROKER_CLIENT_SRC, f"{why}; and @legion/broker-client comes with it, through "
                            "packages/envoy-client/src/broker-credential.ts:21-22"),
    )


# What packages/claude-envoy/scripts/verify-build.sh and the scripts/build.ts it runs read, which
# is every file the published plugin is built from: the claude-envoy gate, and the test gate, for
# two of that job's steps - verify-build, which runs the same command release.yaml's
# claude_envoy_build does, and the build-input-list step
# (packages/claude-envoy/tests/build-inputs.test.ts, which runs the build's two halves itself).
# Those two steps are the whole of what the test job does in this package, so a file only the
# claude-envoy job's lint, typecheck or other tests read is in CLAUDE_ENVOY_JOB below, not here.
VERIFY_BUILD_STEP = "Build, check and pack the Claude Code plugin as the release does"
BUILD_INPUTS_STEP = "Check the Claude Code plugin's build input list covers what the build reads"
CLAUDE_ENVOY_BUILD = reads(
        ("packages/claude-envoy/**", f"{at('claude-envoy', 'Lint Claude Envoy plugin')}, typecheck, verify-build, test and pack in "
                                     f"packages/claude-envoy; {at('test', VERIFY_BUILD_STEP)} and {at('test', BUILD_INPUTS_STEP)} "
                                     "run scripts/verify-build.sh and tests/build-inputs.test.ts of it"),
        (CONTRACTS_SRC, "packages/claude-envoy/tsconfig.json paths @legion/contracts; bundles import it"),
        ("packages/envoy-client/bin/dispatch.ts", "packages/claude-envoy/scripts/build.ts:310 bundles BUNDLE_ENTRYPOINTS' dispatch (:39), which verify-build runs and tests/dist-fresh.test.ts:304 holds to this path"),
        ("packages/pi-shared/src/**", "packages/claude-envoy/src/envoy-channel-server.ts:83-84, legion-claim.ts:7-9, legion-tool.ts:3-12 import @legion/pi-shared/*"),
        ("scripts/third-party-notices.ts", "packages/claude-envoy/scripts/build.ts:24"),
        ("scripts/pi-plugin-prepack.sh", "packages/claude-envoy/scripts/build.ts:198-202 --stage-skills @sjawhar/pi-legion"),
        ("packages/pi-legion/agents/**", "packages/claude-envoy/scripts/build.ts:231-232"),
        ("skills/**", "packages/claude-envoy/scripts/build.ts:198-202 (staged pi-legion partition); packages/claude-envoy/skills/* symlinks into ../../../skills"),
        (".bun-version", "packages/claude-envoy/scripts/build.ts:32 reads the Bun pin"),
        (".github/scripts/check-private-names.sh", "packages/claude-envoy/scripts/verify-build.sh checks the build with it"),
        (".github/scripts/release-push.sh", "packages/claude-envoy/scripts/verify-build.sh runs its check-built; the claude-envoy job runs its extract-built"),
        (".github/workflows/release.yaml", "packages/claude-envoy/tests/build-inputs.test.ts:154 holds its $CLAUDE_ENVOY_VERSION_ARGS equal to scripts/build-inputs.txt"),
) + envoy_client_dep("packages/claude-envoy/src/envoy-channel-server.ts:10-82, hooks/session-hook.ts:91-99 import @legion/envoy-client/*")

# The build above, and what only the claude-envoy job reads: the Claude Code its live answer test
# runs, and the fixtures and modules of its other tests. The test job runs none of them.
CLAUDE_ENVOY_JOB = CLAUDE_ENVOY_BUILD + reads(
        (".github/actions/install-claude-code/**", at("claude-envoy", "./.github/actions/install-claude-code")),
        (".claude-code-pin", ".github/actions/install-claude-code/action.yml:16 pin=$(<.claude-code-pin), the release "
                             "packages/claude-envoy/tests/answers-claude.test.ts:312,338 runs"),
        ("packages/envoy-client/test/**", "packages/claude-envoy/tests/envoy-channel-stall.test.ts:10-12 imports three of its modules, and tsc puts four files of that directory in the claude-envoy program"),
        (f"{FIX}/daemon-api/**", "packages/claude-envoy/tests/daemon-api-version.test.ts:15, legion-claim.test.ts:41, legion-pane-mcp.test.ts:18, legion-tool.test.ts:13"),
        (".claude-plugin/marketplace.json", "packages/claude-envoy/tests/envoy-channel-server.test.ts:8"),
)


# ---------------------------------------------------------------- RULES
LEGION_BUILD = "./cmd/legion"
PACK_READS = [  # bun pm pack of pi-envoy / pi-legion runs their prepack (scripts/pi-plugin-prepack.sh)
    "packages/pi-envoy/**", "packages/pi-legion/**", "packages/pi-shared/**", "packages/envoy-client/**",
    "packages/contracts/**", "scripts/pi-plugin-prepack.sh", "scripts/third-party-notices.ts", "skills/**",
    "package.json", "bun.lock", "patches/**",
]
PMDOC_GEN = [f"{ENVOY}/internal/dispatch/pmdoc/gen/**", "packages/proof-editor/**", CONTRACTS_SRC, "package.json", "bun.lock", "patches/**"]
BROKER_REFGEN_DIRS = ("internal/broker/api", "internal/broker/helper", "internal/broker/config", "internal/broker/approval", "cmd/broker", "cmd/agent-secrets")
MODULE_SOURCES = [Read(f"{ENVOY}/**", "internal/dispatch/api/module_source_test.go:73 walks every non-test .go file "
                                      "under packages/envoy (infra included), for the tests that ask it for the "
                                      "module's sources: lock_level_test.go:89, event_insert_guard_test.go:44, "
                                      "review_order_guard_test.go:49, review_write_guard_test.go:51, "
                                      "one_expression_guard_test.go:66")]


def legion_build(why):
    return run(DAEMON, [LEGION_BUILD], why, mode="build")


RULES = {
    # --- daemon
    f"{DAEMON}/cmd/legion/config_example_test.go": reads(("deploy/kubernetes/daemon/legion.yaml.example", "cmd/legion/config_example_test.go:164 (runtime.Caller at :160 + ../../../..)")),
    f"{DAEMON}/cmd/legion/gh_test.go": reads((f"{DAEMON}/docker/gh", "cmd/legion/gh_test.go:978 ../../docker/gh")),
    f"{DAEMON}/cmd/legion/state_read_budget_test.go": reads(
        (f"{FIX}/daemon-api/state.json", "cmd/legion/state_read_budget_test.go:30 reads the daemon's state golden as the slow daemon's answer")),
    f"{DAEMON}/internal/admit/admit_test.go": reads((f"{DAEMON}/internal/intake/testdata/dispatch/issue-updated.json", "internal/admit/admit_test.go:1058")),
    # main's #2780 added the guard that holds internal/api's reachable claim states to the ones
    # internal/supervise declares, which it reads by parsing that package's own sources.
    f"{DAEMON}/internal/api/reachstates_test.go": reads(
        (f"{DAEMON}/internal/supervise/*", "internal/api/reachstates_test.go:47 lists ../supervise and :57 parses each file of it")),
    f"{DAEMON}/internal/api/real_github_test.go": reads(("scripts/e2e/lib/github-apps.sh", "internal/api/real_github_test.go:115 runs it in bash, and :208,249 read it")),
    f"{DAEMON}/internal/api/state_golden_test.go": reads((f"{FIX}/daemon-api/*", "internal/api/state_golden_test.go:868 (path from goldenPath at :857, declared :32)")),
    f"{DAEMON}/internal/capabilities/capabilities_test.go": reads(
        (f"{FIX}/capability-redaction.json", "internal/capabilities/capabilities_test.go:236 readFixture"),
        (f"{FIX}/capability-redaction-plugin-report.json", "internal/capabilities/capabilities_test.go:236 readFixture")),
    f"{DAEMON}/internal/claim/claim_test.go": reads((f"{FIX}/project-tokens.json", "internal/claim/claim_test.go:196")),
    f"{DAEMON}/internal/classify/fixtures_test.go": reads((f"{FIX}/classification/**", "internal/classify/fixtures_test.go:119,128 (fixtureDirectory :143, runtime.Caller :148)")),
    f"{DAEMON}/internal/config/controller_test.go": reads(("deploy/kubernetes/daemon/controller.yaml.example", "internal/config/controller_test.go:214 LoadController reads it (config.ControllerConfigExample, joined at :213)")),
    f"{DAEMON}/internal/config/kubernetes_test.go": reads(("deploy/kubernetes/operator-route/pod.yml", "internal/config/kubernetes_test.go:1474")),
    # This row and two of the envoy rows below it - internal/dispatch/outbox/
    # expects_reply_never_false_test.go and internal/dispatch/api/live_events_test.go - declare a
    # read every filter gating a job that compiles them already matches. They are here so that a
    # later narrowing of one of those filters is refused by rule 1 rather than dropping the read
    # with nobody noticing (dispatch://INFRA-1413).
    f"{DAEMON}/internal/config/stalldefaults_test.go": reads(
        (f"{FIX}/daemon-api/*", "internal/config/stalldefaults_test.go:72 (the :67 write is under -update, which CI never passes)")),
    f"{DAEMON}/internal/daemon/capability_reports_test.go": reads((f"{FIX}/capability-redaction.json", "internal/daemon/capability_reports_test.go:318")),
    f"{DAEMON}/internal/daemon/daemon_test.go": reads((f"{DAEMON}/internal/intake/testdata/dispatch/issue-updated.json", "internal/daemon/daemon_test.go:1331")),
    # The harness main's #2780 added under the e2e tag for the two agent proofs it brought
    # (claim_compaction_live_test.go and provider_error_retry_live_test.go): it builds the
    # `legion` their panes run and packs both Oh My Pi plugins into the run's own profile. No job
    # of ci.yaml applies this row today - the daemon's e2e tag is vetted (ci-job-typecheck.yaml's
    # `go vet -tags e2e ./...`) and never tested, and a vet run compiles a test file without
    # applying its rules - so it is what a `go test -tags e2e ./internal/daemon/` reads the day a
    # job runs one.
    f"{DAEMON}/internal/daemon/live_agent_proof_test.go": [
        legion_build("internal/daemon/live_agent_proof_test.go:278-281 buildProofLegion runs go build ./cmd/legion in packages/daemon"),
        Read("scripts/e2e/lib/pack-plugin.sh", "internal/daemon/live_agent_proof_test.go:300 runs it in bash, once per plugin"),
        *[Read(g, "internal/daemon/live_agent_proof_test.go:300 packs packages/pi-envoy and packages/pi-legion through "
                  "scripts/e2e/lib/pack-plugin.sh, whose `bun pm pack` runs their prepack")
          for g in PACK_READS],
    ],
    f"{DAEMON}/internal/daemon/nats_permissions_test.go": reads(
        (f"{DAEMON}/internal/intake/testdata/dispatch/issue-updated.json", "internal/daemon/nats_permissions_test.go:76"),
        (f"{DAEMON}/internal/intake/testdata/github/pr-opened.json", "internal/daemon/nats_permissions_test.go:77")),
    f"{DAEMON}/internal/daemon/outbox_scratch_dispatch_test.go": [
        run(ENVOY, ["./cmd/dispatch"], "internal/daemon/outbox_scratch_dispatch_test.go:354 testbuild.EnvoyBinary, which runs go build ./cmd/dispatch in packages/envoy (internal/testbuild/testbuild.go:112)", mode="build")],
    f"{DAEMON}/internal/daemon/supervision_test.go": reads((f"{DAEMON}/internal/stream/*", "internal/daemon/supervision_test.go:1254 Glob ../stream/*.go")),
    f"{DAEMON}/internal/intake/redelivered_test.go": [
        run(ENVOY, ["./cmd/listener", "./cmd/dispatch"], "internal/intake/redelivered_test.go:298-299 testbuild.EnvoyBinary, which runs go build ./cmd/listener and ./cmd/dispatch in packages/envoy (internal/testbuild/testbuild.go:112)", mode="build")],
    f"{DAEMON}/internal/intake/intake_test.go": reads((f"{FIX}/github-envelopes/*", "internal/intake/intake_test.go:802 globs it and :808 reads each file; :902 reads one")),
    f"{DAEMON}/internal/projection/state_golden_test.go": reads((f"{FIX}/daemon-api/*", "internal/projection/state_golden_test.go:209")),
    f"{DAEMON}/internal/prompts/roles_test.go": reads(
        ("packages/pi-legion/agents/*.md", "internal/prompts/roles_test.go:261 stats agents/<name>.md for each task agent"),
        ("skills/legion-worker/references/tools.md", "internal/prompts/roles_test.go:689")),
    f"{DAEMON}/internal/runtime/sandbox/operatorpod_test.go": reads(
        ("deploy/kubernetes/operator-route/pod.yml", "internal/runtime/sandbox/operatorpod_test.go:28"),
        (f"{FIX}/dispatch-public-url.json", "internal/runtime/sandbox/operatorpod_test.go:236"),
        (f"{DAEMON}/docker/worker.Dockerfile", "internal/runtime/sandbox/operatorpod_test.go:298")),
    f"{DAEMON}/internal/runtime/sandbox/boundary_test.go": [legion_build("internal/runtime/sandbox/boundary_test.go:646 go build .../daemon/cmd/legion")],
    f"{DAEMON}/internal/runtime/tmux/tmux_real_test.go": [
        legion_build("internal/runtime/tmux/tmux_real_test.go:86 go build .../daemon/cmd/legion"),
        Read(f"{DAEMON}/cmd/legion/testdata/gh-help/**", "internal/runtime/tmux/tmux_real_test.go:1828")],
    f"{DAEMON}/internal/stream/integration_test.go": [legion_build("internal/stream/integration_test.go:157 go build .../daemon/cmd/legion")],
    f"{DAEMON}/internal/runtime/workerbin/workerbin_test.go": reads((f"{DAEMON}/docker/gh", "internal/runtime/workerbin/workerbin_test.go:58")),
    f"{DAEMON}/internal/textcheck/design_approved_wake_test.go": reads(
        ("skills/legion-architect/SKILL.md", "internal/textcheck/design_approved_wake_test.go:19-26 reads it under ../../../.."),
        ("skills/legion-architect/references/design-gate.md", "internal/textcheck/design_approved_wake_test.go:19-26 reads it under ../../../.."),
        ("skills/legion-architect/references/wake-routing.md", "internal/textcheck/design_approved_wake_test.go:19-26 reads it under ../../../.."),
        (f"{DAEMON}/internal/prompts/roles/architect-root.md", "internal/textcheck/design_approved_wake_test.go:19-26 reads it under ../../../..")),
    f"{DAEMON}/internal/textcheck/file_copy_test.go": reads(
        (f"{DAEMON}/**/*.go", "internal/textcheck/file_copy_test.go:22 walks every .go file under packages/daemon (../..)")),
    f"{DAEMON}/internal/textcheck/classify_imports_test.go": reads(
        (f"{DAEMON}/internal/classify/*", "internal/textcheck/classify_imports_test.go:14-15 globs ../classify/*.go")),
    f"{DAEMON}/internal/textcheck/reclaim_doc_test.go": reads(
        ("docs/kubernetes.md", "internal/textcheck/reclaim_doc_test.go:44,167 reads it under ../../../.."),
        (f"{DAEMON}/internal/runtime/sandbox/reclaim_doc_extract.txt",
         "internal/textcheck/reclaim_doc_test.go:123 reads the committed copy it holds the guide's paragraph to (reclaimExtractLists)")),
    # internal/daemon/upgrade_phase_entry_way_back_test.go reads no text outside its own package
    # any more and has no row: upgrade_way_back_extract.txt stands in its own directory, and this
    # row is what holds that copy to the guide, in the text-check job (dispatch://INFRA-1413).
    f"{DAEMON}/internal/textcheck/way_back_doc_test.go": reads(
        ("docs/kubernetes.md", "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.. "
                               "for way_back_doc_test.go:68 (shippedText)"),
        (f"{DAEMON}/internal/daemon/upgrade_way_back_extract.txt",
         "internal/textcheck/waiting_line_texts_test.go:380 reads it as ../daemon/upgrade_way_back_extract.txt for "
         "way_back_doc_test.go:45, the committed copy it holds the guide's block to (quotedBody)")),
    f"{DAEMON}/internal/textcheck/waiting_line_texts_test.go": reads(
        # The four copies of the shipped texts, each committed beside the package whose own
        # test is held to it and read here through readExtract.
        (f"{DAEMON}/internal/api/waiting_line_contract_extract.txt",
         "internal/textcheck/waiting_line_texts_test.go:380 reads it as ../api/waiting_line_contract_extract.txt"),
        (f"{DAEMON}/internal/workflow/waiting_line_words_extract.txt",
         "internal/textcheck/waiting_line_texts_test.go:380 reads it as ../workflow/waiting_line_words_extract.txt"),
        (f"{DAEMON}/cmd/legion/awaiting_summary_extract.txt",
         "internal/textcheck/waiting_line_texts_test.go:380 reads it as ../../cmd/legion/awaiting_summary_extract.txt"),
        (f"{DAEMON}/cmd/legion/waiting_line_block_extract.txt",
         "internal/textcheck/waiting_line_texts_test.go:380 reads it as ../../cmd/legion/waiting_line_block_extract.txt"),
        # The texts those copies are held to, each read under ../../../.. by shippedText: the
        # seven the contract windows are recomputed from, the eight the band list names, and
        # those eight again read whole for the ban.
        ("AGENTS.md", "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("docs/site/src/content/docs/legion/concepts.md",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("docs/site/src/content/docs/legion/running-legion.md",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("docs/site/src/content/docs/legion/troubleshooting.md",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("packages/contracts/src/legion-api.ts",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("packages/pi-legion/AGENTS.md",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("packages/contracts/.changes/unreleased/pr2989-Added-20261108-074056.yaml",
         "internal/textcheck/waiting_line_texts_test.go:390 reads that one fragment under ../../../.."),
        ("packages/pi-legion/.changes/unreleased/pr2989-Changed-20261108-074108.yaml",
         "internal/textcheck/waiting_line_texts_test.go:390 reads that one fragment under ../../../.."),
        ("packages/claude-envoy/.changes/unreleased/pr2989-Changed-20261108-074111.yaml",
         "internal/textcheck/waiting_line_texts_test.go:390 reads that one fragment under ../../../.."),
        ("skills/legion-architect/references/acceptance.md",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("skills/legion-controller/references/admission.md",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../.."),
        ("skills/legion-controller/references/wakes.md",
         "internal/textcheck/waiting_line_texts_test.go:390 reads it under ../../../..")),
    # The three checks main's #2989 put under packages/daemon, and the waiting-line block check
    # main's #3718 brought, read no text outside it any more: each is held to a copy committed
    # beside its own package, and the row above is what holds those copies to the texts, in the
    # text-check job, where a documentation edit is the run that should start
    # (dispatch://INFRA-1413). cmd/legion/main_test.go is left with no row at all -
    # awaiting_summary_extract.txt and waiting_line_block_extract.txt stand in its own directory
    # - and what is left of the two rows below is what each reads of packages/daemon outside its
    # own package, which daemon_go_tests covers with the module itself.
    f"{DAEMON}/internal/api/waiting_line_test.go": reads(
        (f"{DAEMON}/cmd/legion/main.go",
         "internal/api/waiting_line_test.go:240 reads every name of its list under the root it joins at :226"),
        (f"{DAEMON}/cmd/legion/main_test.go",
         "internal/api/waiting_line_test.go:240 reads every name of its list under the root it joins at :226"),
        (f"{DAEMON}/internal/projection/project.go",
         "internal/api/waiting_line_test.go:240 reads every name of its list under the root it joins at :226"),
        (f"{DAEMON}/internal/projection/project_test.go",
         "internal/api/waiting_line_test.go:240 reads every name of its list under the root it joins at :226")),
    f"{DAEMON}/internal/workflow/waiting_line_band_test.go": reads(
        (f"{DAEMON}/cmd/legion/main.go",
         "internal/workflow/waiting_line_band_test.go:62 reads every file of its list under ../../../.."),
        (f"{DAEMON}/internal/admit/admit.go",
         "internal/workflow/waiting_line_band_test.go:62 reads every file of its list under ../../../.."),
        (f"{DAEMON}/internal/admit/awaiting_person_test.go",
         "internal/workflow/waiting_line_band_test.go:62 reads every file of its list under ../../../.."),
        (f"{DAEMON}/internal/admit/hold_test.go",
         "internal/workflow/waiting_line_band_test.go:62 reads every file of its list under ../../../.."),
        (f"{DAEMON}/internal/api/state.go",
         "internal/workflow/waiting_line_band_test.go:62 reads every file of its list under ../../../.."),
        (f"{DAEMON}/internal/daemon/awaiting_catchup.go",
         "internal/workflow/waiting_line_band_test.go:62 reads every file of its list under ../../../.."),
        (f"{DAEMON}/internal/phase/phase.go",
         "internal/workflow/waiting_line_band_test.go:62 reads every file of its list under ../../../..")),
    # --- envoy
    f"{ENVOY}/cmd/broker-refgen/auth_test.go": [
        Read(f"{ENVOY}/{d}/*", "cmd/broker-refgen/auth_test.go:14 readAPI(repo root) parses it") for d in BROKER_REFGEN_DIRS],
    f"{ENVOY}/cmd/broker/main_test.go": reads((f"{ENVOY}/docker/rds-global-bundle.pem", "cmd/broker/main_test.go:344 sslrootcert=../../docker/rds-global-bundle.pem")),
    f"{ENVOY}/cmd/broker/outage_test.go": [
        run(ENVOY, ["./cmd/agent-secrets"], "cmd/broker/outage_test.go:323-325 cmdtest.BuildCommand . in ../agent-secrets", mode="build")],
    f"{ENVOY}/cmd/broker/multiprocess_test.go": reads((f"{ENVOY}/internal/broker/store/migrations/*", "cmd/broker/multiprocess_test.go:513 countMigrations globs ../../internal/broker/store/migrations/*.up.sql")),
    f"{ENVOY}/cmd/dispatch/main_test.go": reads((f"{ENVOY}/internal/dispatch/store/migrations/*", "cmd/dispatch/main_test.go:822")),
    # cmd/dispatch/memory_profile_test.go:147-148 reads four guides only when
    # DISPATCH_EDIT_PROFILE_OUT is set (line 125), which CI never sets: not a CI read.
    f"{ENVOY}/cmd/dispatch/pod_grace_period_test.go": reads((f"{INFRA}/gcp/testdata/dispatch-pod.golden", "cmd/dispatch/pod_grace_period_test.go:53")),
    f"{ENVOY}/cmd/listener/api_test.go": reads((f"{FIX}/dispatch-targeted-delivery.json", "cmd/listener/api_test.go:174,419,463")),
    f"{ENVOY}/cmd/listener/grace_margin_doc_test.go": reads(
        (f"{INFRA}/README.md", "cmd/listener/grace_margin_doc_test.go:18"),
        (f"{INFRA}/docs/nats-hub.md", "cmd/listener/grace_margin_doc_test.go:19"),
        (f"{ENVOY}/AGENTS.md", "cmd/listener/grace_margin_doc_test.go:20")),
    f"{ENVOY}/cmd/listener/infra_stop_budget_test.go": reads(
        (f"{INFRA}/gcp/clusteredhub_listener.go", "cmd/listener/infra_stop_budget_test.go:18")),
    f"{ENVOY}/cmd/listener/pod_grace_period_test.go": reads(
        (f"{INFRA}/gcp/testdata/clustered-hub-listener-pod.golden", "cmd/listener/pod_grace_period_test.go:23"),
        (f"{INFRA}/gcp/testdata/dispatch-listener-pod.golden", "cmd/listener/pod_grace_period_test.go:28")),
    f"{ENVOY}/internal/broker/config/config_test.go": reads((f"{ENVOY}/docker/rds-global-bundle.pem",
                                                            "internal/broker/config/config_test.go:1348,1378 Load the URL whose sslrootcert is this bundle (rdsBundle :1308), which pgx reads to build the root pool")),
    f"{ENVOY}/internal/cistore/settlement_fixture_test.go": reads((f"{FIX}/checks-settlement/*", "internal/cistore/settlement_fixture_test.go:56")),
    f"{ENVOY}/internal/contracts/dedupe_test.go": reads((f"{FIX}/dedupe-keys.json", "internal/contracts/dedupe_test.go:14")),
    f"{ENVOY}/internal/contracts/golden_test.go": reads(
        (f"{ENVOY}/scripts/fixtures/github/*", "internal/contracts/golden_test.go:48"),
        (f"{FIX}/github-envelopes/*", "internal/contracts/golden_test.go:87")),
    f"{ENVOY}/internal/contracts/normalize_test.go": reads(
        (f"{FIX}/github-envelopes/*", "internal/contracts/normalize_test.go:688"),
        (f"{ENVOY}/scripts/fixtures/github/*", "internal/contracts/normalize_test.go:2458")),
    f"{ENVOY}/internal/dispatch/api/copy_lock_order_test.go": [
        run(ENVOY, ["./cmd/dispatch"], "internal/dispatch/api/copy_lock_order_test.go:264 go list -deps -export ./cmd/dispatch in packages/envoy", mode="build")],
    f"{ENVOY}/internal/dispatch/api/delivery_live_diff_test.go": reads((f"{FIX}/live/*", "internal/dispatch/api/delivery_live_diff_test.go:1016")),
    f"{ENVOY}/internal/dispatch/api/delivery_measures_test.go": reads(
        (f"{ENVOY}/internal/dispatch/delivery/measures/testdata/dora-fixture.json", "internal/dispatch/api/delivery_measures_test.go:82")),
    f"{ENVOY}/internal/dispatch/api/delivery_test.go": reads(
        (f"{ENVOY}/internal/dispatch/delivery/testdata/dev-only-run-jobs.json", "internal/dispatch/api/delivery_test.go:714")),
    f"{ENVOY}/internal/dispatch/api/live_events_test.go": reads(
        (f"{FIX}/live/event.json", "internal/dispatch/api/live_events_test.go:879")),
    f"{ENVOY}/internal/dispatch/api/openapi_test.go": reads(
        ("packages/contracts/schemas/dispatch-openapi.json",
         "internal/dispatch/api/openapi_test.go:189-190 (the path built runtime-relative ../../../../.., then "
         "read: the committed document TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites compares the "
         "emitter's bytes with)")),
    f"{ENVOY}/internal/dispatch/api/lock_level_test.go": MODULE_SOURCES,
    f"{ENVOY}/internal/dispatch/api/event_insert_guard_test.go": MODULE_SOURCES,
    f"{ENVOY}/internal/dispatch/api/review_write_guard_test.go": MODULE_SOURCES,
    # main's #3429 put this check in who_waits_test.go, a behaviour file of this package, where
    # the race job was the only one that ran it and the module walk under it was declared by
    # nobody. It is a file of its own under `//go:build !race` now, so the text-check job runs it
    # and this row is the declaration that holds that job's filter to what it reads.
    f"{ENVOY}/internal/dispatch/api/one_expression_guard_test.go": MODULE_SOURCES,
    f"{ENVOY}/internal/dispatch/docs/approval_carry_test.go": reads((f"{FIX}/decision-blocks/*", "internal/dispatch/docs/approval_carry_test.go:26")),
    f"{ENVOY}/internal/dispatch/docs/history_fold_test.go": reads(
        (f"{ENVOY}/internal/dispatch/pmdoc/testdata/fixtures.json", "internal/dispatch/docs/history_fold_test.go:518")),
    f"{ENVOY}/internal/dispatch/outbox/expects_reply_never_false_test.go": reads(
        ("packages/contracts/src/dispatch-api*.ts",
         "internal/dispatch/outbox/expects_reply_never_false_test.go:137-138 globs ../../../../contracts/src/dispatch-api*.ts "
         "and :149 reads each module the glob finds (the .test.ts files :146 skips apart)")),
    f"{ENVOY}/internal/dispatch/pmdoc/update_test.go": [
        Read(g, "internal/dispatch/pmdoc/update_test.go:236-238 bun run gen/<script> (imports @legion/proof-editor)") for g in PMDOC_GEN],
    f"{ENVOY}/internal/dispatch/pmdoc/render_readback_test.go": [
        Read(g, "internal/dispatch/pmdoc/render_readback_test.go:34 bun run gen/differential.ts") for g in PMDOC_GEN],
    # The server.started envelope packages/daemon's intake decodes, which this package's
    # TestTheBuiltEnvelopeIsTheSharedFixture holds its own build to: the one read here of a file
    # of another module, and the one the envoy_go filter had to grow a pattern for, since it
    # matched no file under packages/daemon and a change to that fixture alone therefore ran the
    # daemon's jobs and not this one (dispatch://INFRA-1413).
    f"{ENVOY}/internal/dispatch/startup/started_test.go": reads(
        (f"{DAEMON}/internal/intake/testdata/dispatch/server-started.json", "internal/dispatch/startup/started_test.go:379")),
    # The deployer's own copy of the migration advisory key, in the shell script beside the Go
    # constant: the test reads both scripts out of packages/envoy/deploy/scripts.
    f"{ENVOY}/internal/dispatch/store/deployer_key_test.go": reads(
        (f"{ENVOY}/deploy/scripts/autodeploy.sh",
         "internal/dispatch/store/deployer_key_test.go:44 reads it, the path built under ../../../deploy/scripts "
         "(:30), and holds every pg_try_advisory_lock literal of it to migrationLockKey"),
        (f"{ENVOY}/deploy/scripts/autodeploy_test.sh",
         "internal/dispatch/store/deployer_key_test.go:44 reads it the same way (:31)")),
    f"{ENVOY}/internal/dispatch/store/search_text_test.go": reads(
        (f"{ENVOY}/**/*.md", "internal/dispatch/store/search_text_test.go:104-120 walks packages/envoy (infra included, "
                             "node_modules and out passed over) and :114 opens each .md file, and no other")),
    f"{ENVOY}/internal/dispatch/text/whitespace_fixture_test.go": reads(
        (f"{FIX}/go-unicode-isspace.json", "internal/dispatch/text/whitespace_fixture_test.go:34 os.ReadFile (goSpaceFixturePath :17, ../../../../contracts/fixtures)")),
    f"{ENVOY}/internal/smoke/rdsbundle_test.go": [
        run(ENVOY, ["./internal/smoke/systemroots"], "internal/smoke/rdsbundle_test.go:36-41 cmdtest.BuildCommand ./systemroots CGO_ENABLED=0 GOOS=linux",
            mode="build", env={"CGO_ENABLED": "0", "GOOS": "linux"}),
        Read(f"{ENVOY}/docker/rds-global-bundle.pem", "internal/smoke/rdsbundle_test.go:62")],
    f"{ENVOY}/internal/testnats/nats_test.go": [
        run(ENVOY, ["./..."], "internal/testnats/nats_test.go:708-709 go list -f ... ./... in packages/envoy", mode="build"),
        *[Read(g, "internal/testnats/nats_test.go:741-754 walks packages/envoy but infra, node_modules and testdata, "
                  "and :754 reads every .go file it finds, test files included (dockerDriverFiles, called from "
                  "TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime at :669)")
          for g in envoy_go_files_but_infra()]],
    f"{ENVOY}/internal/textcheck/claims_doc_test.go": reads(
        ("docs/kubernetes.md", "internal/textcheck/claims_doc_test.go:36,43 reads it under ../../../.."),
        (f"{ENVOY}/internal/broker/oauth/claims_doc_extract.txt",
         "internal/textcheck/claims_doc_test.go:70 reads the committed copy it holds the guide's list to (extractLines, called with it at :53)")),
    f"{ENVOY}/internal/textcheck/environment_reads_test.go": reads(
        (f"{ENVOY}/cmd/dispatch/**", "internal/textcheck/environment_reads_test.go's environmentReads walks .go files"),
        (f"{ENVOY}/internal/dispatch/**", "internal/textcheck/environment_reads_test.go's environmentReads walks .go files")),
    f"{ENVOY}/internal/textcheck/file_copy_test.go": reads(
        (f"{ENVOY}/**", "internal/textcheck/file_copy_test.go:22 walks every .go file under packages/envoy (../.., infra included)")),
    f"{ENVOY}/internal/textcheck/readme_commands_test.go": reads(
        (f"{ENVOY}/loadtest/README.md", "internal/textcheck/readme_commands_test.go:23 reads packages/envoy/loadtest/README.md under ../../../..")),
    # --- envoy-infra
    f"{INFRA}/core/permissions_live_test.go": [
        run(ENVOY, ["./cmd/dispatch", "./cmd/listener"], "infra/core/permissions_live_test.go:328 buildEnvoyCommand builds . in ../../cmd/<dispatch|listener> (root go.work)", mode="build")],
    f"{INFRA}/core/allowed_hosts_agreement_test.go": [run(f"{INFRA}/core", ["."], "infra/core/allowed_hosts_agreement_test.go:9 imports the listener's internal/config, so this package's own closure (go list -deps .) is what compiling the test reads", mode="build")],
    # infra/gcp/imports_test.go moved to infra/internal/textcheck/gcp_imports_test.go in this
    # change, and main's entry for the old path came with the forward merge; the guard's own
    # entry is under its new path below.
    f"{INFRA}/host/heldportwrapper/main_test.go": [
        run(ENVOY, ["./cmd/listener"], "infra/host/heldportwrapper/main_test.go:112 buildListener builds . in ../../../cmd/listener (root go.work)", mode="build")],
    f"{INFRA}/host/live_test.go": [run(INFRA, ["./cmd/pulumi-resource-envoy-infra"], "infra/host/live_test.go pulumitest.Plugin -> internal/pulumitest/pulumitest.go:168 go build", mode="build")],
    f"{INFRA}/internal/textcheck/core_imports_test.go": [
        run(INFRA, ["./core"], "infra/internal/textcheck/core_imports_test.go:20 moduleDeps, whose go list -deps ./core runs in ../.. (:31-32)", mode="build")],
    f"{INFRA}/internal/textcheck/gcp_imports_test.go": [
        run(INFRA, ["./gcp"], "infra/internal/textcheck/gcp_imports_test.go:16 moduleDeps, whose go list -deps ./gcp runs in ../.. (core_imports_test.go:31-32)", mode="build")],
}


# ---------------------------------------------------------------- GATES
NOTICES_STEP = "Check every Go release archive's third-party notices can be written and the cross-built commands compile"


def go_notices_runs():
    """Every go command .github/scripts/go-release-notices.sh starts, one per release target of
    each list it reads. The lines are the script's, not the step's: lint's notices step runs the
    script, and ci-caches.yaml's daemon job runs the same one to fill the cache lint restores."""
    started_by = at("lint", NOTICES_STEP)
    out = []
    for target in targets(GO_TARGETS, "legion"):
        goos, goarch = target.split("/")
        out.append(script_run(NOTICES_LOOPS, '../../scripts/go-third-party-notices.sh "$notices/legion-${target%/*}-${target#*/}" ./cmd/legion',
                              started_by, DAEMON, ["./cmd/legion"], mode="build",
                              env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    for arch in targets(GO_TARGETS, "envoy"):
        out.append(script_run(NOTICES_LOOPS, '../../scripts/go-third-party-notices.sh "$notices/legion-envoy-$arch" ./cmd/listener',
                              started_by, ENVOY, ["./cmd/listener"], mode="build", env={"GOOS": "linux", "GOARCH": arch}))
        out.append(script_run(NOTICES_LOOPS, '../../scripts/go-third-party-notices.sh "$notices/agent-secrets-$arch" ./cmd/agent-secrets ./cmd/agent-secrets-helper',
                              started_by, ENVOY, ["./cmd/agent-secrets", "./cmd/agent-secrets-helper"], mode="build",
                              env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch}))
    for target in targets(CLIENT_TARGETS, "agent-secrets-client"):
        goos, goarch = target.split("/")
        out.append(script_run(NOTICES_LOOPS, f"go -C {ENVOY} build -buildvcs=false -o /dev/null ./cmd/agent-secrets",
                              started_by, ENVOY, ["./cmd/agent-secrets"], mode="build",
                              env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    for target in targets(INFRA_TARGETS, "envoy-infra"):
        goos, goarch = target.split("/")
        out.append(script_run(NOTICES_LOOPS, f"go -C {INFRA} build -buildvcs=false -o /dev/null ./cmd/...",
                              started_by, INFRA, ["./cmd/..."], mode="build",
                              env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    return out


# The steps whose go commands are not in the simple form, pinned whole: envoy-go-checks'
# pack step, envoy-smoke's schema step, and the tag steps below.
PACK_STEP = "Pack envoy binaries"
PACK_SCRIPT = """\
for arch in $(jq -r '.envoy[]' ../../.github/go-release-targets.json); do
  out="../../out/pr-artifacts/envoy-$arch"
  mkdir -p "$out"
  GOOS=linux GOARCH=$arch go build -buildvcs=false -o "$out/envoy-listener" ./cmd/listener
  GOOS=linux GOARCH=$arch ../../scripts/go-third-party-notices.sh "$out/THIRD_PARTY_NOTICES" ./cmd/listener
  tar czf "../../out/pr-artifacts/legion-envoy-$arch.tar.gz" -C "$out" envoy-listener THIRD_PARTY_NOTICES
  tar tzvf "../../out/pr-artifacts/legion-envoy-$arch.tar.gz"
done"""
SCHEMA_STEP = "The envoy-infra plugin's schema carries the version and the download URL it was built with"
SCHEMA_SCRIPT = (
    'url=$(GITHUB_REPOSITORY=cybermonklabs/legion ../../../.github/scripts/repo-paths.sh plugin_download_url)\n'
    """go build -ldflags "-X main.version=1.2.3 -X 'github.com/sjawhar/envoy/infra/provider.downloadURL=$url'" -o "$RUNNER_TEMP/pr" ./cmd/pulumi-resource-envoy-infra\n"""
    "# shellcheck disable=SC2016 # ${VERSION} is Pulumi's placeholder, not a shell expansion\n"
    """pulumi package get-schema "$RUNNER_TEMP/pr" | jq -e '.version == "1.2.3" and .pluginDownloadURL == "https://github.com/cybermonklabs/legion/releases/download/envoy-infra-v${VERSION}"'"""
)
# The six steps that run only what their build tag adds. A tag adds files to a package and
# subtracts none, so `go test -tags T ./pkg` would run the untagged tests another job already
# ran: each step takes its -run from .github/scripts/go-tagged-tests.sh, which reads the names
# off the source at run time. The assignment is a line of its own, so that under bash -e an empty
# or refused list fails the step instead of running -run "", which matches every test. That
# substitution and quote are outside the simple form, so each step is pinned whole here. A -run
# moves no closure: the whole package compiles under the tag, whichever tests the run selects, so
# each entry's Run is the plain tagged go test the step ran before.
TAGGED_TESTS = ".github/scripts/go-tagged-tests.sh"
NATS_MATRIX_STEP = "Test the container-backed NATS matrix (natslive)"
NATS_MATRIX_SCRIPT = f"""\
rig=$(../../{TAGGED_TESTS} natslive ./internal/testnats)
go test -count=1 -tags natslive -run "$rig" ./internal/testnats/
cap=$(../../{TAGGED_TESTS} natslive ./internal/bus)
go test -count=1 -timeout 5m -tags natslive -run "$cap" -v ./internal/bus/"""
# One call per package, never one union -run over the three: a name an untagged test of one
# package shares with a natslive test of another would then run a second time unremarked.
INFRA_NATSLIVE_STEP = "Live tests of envoy-infra against nats-server (natslive)"
INFRA_NATSLIVE_PACKAGES = ["./core", "./seed", "./gcp"]
INFRA_NATSLIVE_SCRIPT = f"""\
for pkg in {" ".join(INFRA_NATSLIVE_PACKAGES)}; do
  pattern=$(../../../{TAGGED_TESTS} natslive "$pkg")
  go test -count=1 -timeout 30m -tags natslive -run "$pattern" "$pkg"
done"""
MEMORY_STEP = "Memory tests of the Dispatch server"
MEMORY_SCRIPT = f"""\
pattern=$(../../{TAGGED_TESTS} memory ./cmd/dispatch)
go test -tags memory -count=1 -timeout 60m -v -run "$pattern" ./cmd/dispatch"""
DISPATCH_MP_STEP = "Two-process tests of the Dispatch server"
DISPATCH_MP_SCRIPT = f"""\
pattern=$(../../{TAGGED_TESTS} multiprocess ./cmd/dispatch)
go test -tags multiprocess -count=1 -timeout 20m -v -run "$pattern" ./cmd/dispatch"""
BROKER_MP_STEP = "Two-process tests of the secrets broker"
BROKER_MP_SCRIPT = f"""\
pattern=$(../../{TAGGED_TESTS} multiprocess ./cmd/broker)
go test -tags multiprocess -count=1 -timeout 20m -v -run "$pattern" ./cmd/broker"""
SOAK_STEP = "Server-change soak of the listener"
SOAK_SCRIPT = f"""\
pattern=$(../../{TAGGED_TESTS} serverchange ./cmd/listener)
go test -tags serverchange -count=1 -timeout 15m -v -run "$pattern" ./cmd/listener"""
# The airlock-lean job's two runs of the model comparison. `-run`, `-fuzz` and `-fuzztime` are no
# flags the simple form reduces, so each step is pinned whole; they move no closure, since both
# runs compile the airlock package's tests, whichever cases they then drive.
AIRLOCK_JOB = "airlock-lean"
AIRLOCK_LEAN = f"{ENVOY}/internal/broker/airlock/lean"
AIRLOCK_PKGS = ["./internal/broker/airlock/"]
AIRLOCK_SEED_STEP = "Compare the Go airlock with the model over the seed corpus"
AIRLOCK_SEED_SCRIPT = "go test -run FuzzAirlock ./internal/broker/airlock/"
AIRLOCK_FUZZ_STEP = "Fuzz the Go airlock against the model"
AIRLOCK_FUZZ_SCRIPT = "go test -fuzz FuzzAirlock -fuzztime=120s -run FuzzAirlock ./internal/broker/airlock/"
AIRLOCK_BUILD_STEP = "Build the Lean model"
AIRLOCK_AXIOMS_STEP = "Print the theorems' axioms"
AIRLOCK_AXIOMS_SCRIPT = f"{ENVOY}/scripts/airlock-lean-axioms.sh"
# The document service's race step runs under .github/scripts/race_memory.py, which fails it with
# a log when the race binary's memory passes RACE_MEM_LIMIT_KB (ci.yaml's comment on the step says
# why), so its line is a shell assignment and a script, not a go command the simple form reduces.
RACE_DOCS_STEP = "Race-test the document service"
RACE_DOCS_SCRIPT = ("RACE_MEM_LIMIT_KB=8388608 ../../.github/scripts/race_memory.py "
                    "go test -json -race -count=1 -timeout 60m ./internal/dispatch/docs/")
# The same package under Go's cleanup detector. GODEBUG is a GO* name, which is outside
# ENV_ALLOWED wherever it is set, so this step too is pinned whole rather than reduced.
FINALIZERS_STEP = "Test the document service under the cleanup detector"
FINALIZERS_SCRIPT = "GODEBUG=checkfinalizers=1 go test -v -count=1 -timeout 34m ./internal/dispatch/docs/"


# ---------------------------------------------------------------- reading a Go file as text
# Two lanes below read Go source as text rather than with a toolchain, because this script runs
# where no Go is guaranteed: the daemon lane holds PLUGIN_PACK_TESTS to the tests its package
# declares (hold_plugin_pack_tests), and the envoy lane finds the tests only a plain run covers
# (race_impossible_tests_of). Both look for an identifier the compiler reads, so both blank what
# it does not read first.
def go_code_only(text: str) -> str:
    """`text` with every comment, string and rune literal blanked out and its newlines kept, so a
    name found in what is left is an identifier the compiler reads, at its own offset."""
    out, i, n = [], 0, len(text)
    while i < n:
        if text.startswith("//", i):
            end = text.find("\n", i)
        elif text.startswith("/*", i):
            end = text.find("*/", i + 2)
            end = -1 if end < 0 else end + 2
        elif text[i] in "\"'`":
            quote, end = text[i], i + 1
            while end < n and text[end] != quote:
                end += 2 if quote != "`" and text[end] == "\\" else 1
            end = min(end + 1, n)
        else:
            out.append(text[i])
            i += 1
            continue
        end = n if end < 0 else end
        out.append("".join(c if c == "\n" else " " for c in text[i:end]))
        i = end
    return "".join(out)


# A test of a Go file: `go help test`'s Test, Fuzz and Example functions, whose name goes on with
# no lower-case letter. TestMain is the package's entry point, not a test. The signature is no
# part of this: a Fuzz takes a *testing.F, an Example takes nothing, a test may take its
# *testing.T under an aliased import of the package, and gofmt keeps a signature its author wrote
# over more than one line - so a test is its name and the `(` that opens its parameters, in
# column 0, which a method (`func (s *suite) TestX(`) and a `func TestX` gofmt indents inside
# another function are not. Both lanes below read this one pattern, so a shape one of them learns
# the other has too.
GO_TEST_FUNC = re.compile(r"^func ((?:Test|Fuzz|Example)(?:[^\Wa-z]\w*)?)\(", re.M)
# A file's build constraint. A step that passes no -tags does not compile a file that carries
# one, so a test declared only there is a test that step can neither run nor name.
GO_BUILD_LINE = re.compile(r"^//go:build +(.+?) *$", re.M)


# ---------------------------------------------------------------- the text checks
# The package each of the three modules keeps the tests whose subject is text in: a guide's
# wording, a file copied from one tree to another, the imports a package may have. The
# text-check job runs those three packages, and the three product lanes leave them out of their
# own runs, so each of those tests runs once and in a job with no database. The name is written
# here once and every one of those six steps is built from it, so a package that moves cannot
# stay compiled into a lane that no longer means to run it.
TEXT_CHECK_PACKAGE = "internal/textcheck"


def has_go_files(directory: Path) -> bool:
    """Whether a `./<directory>/...` pattern names any package at all: a .go file under it, in a
    directory the go command does not pass over (go_skips_dir). `go list` answers a pattern that
    names none with a warning and no package, so packages_kept() leaves such a directory out."""
    for current, subdirs, names in os.walk(directory):
        here = Path(current)
        subdirs[:] = sorted(s for s in subdirs if not go_skips_dir(here, s))
        if any(n.endswith(".go") for n in names):
            return True
    return False


def packages_kept(root: str, pattern: str) -> list:
    """The `go list` patterns that together name the packages a step's `go list ./... | grep -Ev
    '<pattern>'` hands go test in the module at `root`. The pattern is tried as that grep tries
    it: against each import path `go list ./...` prints, which is the module path the root's
    go.mod declares, then the package's directory under the root, for every directory `./...`
    walks (a .go file in it, nothing go_skips_dir passes over on the way). A directory under
    which the pattern takes out no package is one `/...` pattern; one that holds a package it
    takes out is its own package, where it has one the pattern keeps, and then its children in
    turn. So the Run standing for such a step names what the step tests, read from the step's
    own pattern, rather than the superset `./...` or a list kept by hand beside it: the closure of
    `./...` carries a dropped package's test files and RULES - the files only its tests read -
    onto this job's filter, and a change to one of them would go on starting a job that does not
    run the test that reads it (dispatch://INFRA-1413)."""
    base = Path(root)
    module = next(line.split()[1] for line in (base / "go.mod").read_text().splitlines()
                  if line.startswith("module "))
    dropped = []
    for current, subdirs, names in os.walk(base):
        here = Path(current)
        subdirs[:] = sorted(s for s in subdirs if not go_skips_dir(here, s))
        under = here.relative_to(base).as_posix()
        if any(n.endswith(".go") for n in names) and re.search(pattern, module if under == "." else f"{module}/{under}"):
            dropped.append(here)
    if not dropped:
        raise ValueError(f"`grep -Ev '{pattern}'` takes no package of {root} out of `go list ./...`, so no lane "
                         "is leaving one out; follow the package, or drop the grep")
    found = []

    def walk(here: Path):
        if here not in dropped and any(entry.name.endswith(".go") for entry in here.iterdir() if entry.is_file()):
            found.append("./" if here == base else f"./{here.relative_to(base).as_posix()}/")
        for name in sorted(d.name for d in here.iterdir() if d.is_dir() and not go_skips_dir(here, d.name)):
            child = here / name
            if any(d.is_relative_to(child) for d in dropped):
                walk(child)
            elif has_go_files(child):
                found.append(f"./{child.relative_to(base).as_posix()}/...")

    walk(base)
    return found


# envoy-infra's plain run, which leaves the text checks out the way the other two lanes leave
# theirs out: `go test ./...` cannot name "every package but one", so the step lists the module's
# packages and drops that one before go test sees them. `set -o pipefail` is what makes a failed
# go list fail the step rather than pass it with nothing tested.
INFRA_PLAIN_STEP = "Unit test envoy-infra"
INFRA_PLAIN_PATTERN = f"/{TEXT_CHECK_PACKAGE}$"
INFRA_PLAIN_SCRIPT = f"""\
set -o pipefail
go list ./... | grep -Ev '{INFRA_PLAIN_PATTERN}' | xargs go test -count=1"""


# The three packages the test job's race step runs, and which its plain step therefore leaves out.
# One list: both step texts below are built from it, and check-ci-filters.sh fails when either step
# in ci.yaml differs from the text built here, so the two steps cannot name different sets.
# internal/record is not among them: its product code starts no goroutine and uses no sync, so the
# detector has nothing of the package to find there. Its own tests do start goroutines, and the
# cover the detector gave those is what this trade gives up. The plain step leaves out one more
# package than the race step runs, the text checks, which are the text-check job's.
DAEMON_RACE_PACKAGES = ["internal/daemon", "internal/sessionproxy", "internal/launcher"]
DAEMON_RACE_STEP = "Test the concurrent packages under the race detector"
DAEMON_RACE_PKGS = [f"./{p}/" for p in DAEMON_RACE_PACKAGES]
DAEMON_PLAIN_STEP = "Test the Go daemon"
DAEMON_PLAIN_PATTERN = "/(" + "|".join(DAEMON_RACE_PACKAGES + [TEXT_CHECK_PACKAGE]) + ")$"

# The three tests of this module the plugin-pack job runs: the plugin pack test and the two tests
# of agentmodels_omp_test.go (a fourth that loads a plugin on the real Oh My Pi,
# promptreferences_omp_test.go's prompt reference probe, reads no file of the repository and stays
# in the race step). They run once per run of the router, in the plugin-pack job, and the race
# step that runs their package skips them by name:
#   * promptreferences_omp_test.go:132 packs packages/pi-envoy and packages/pi-legion with `bun pm
#     pack` (packedPlugin, :207-228), and each pack runs that package's prepack, which reads
#     bun.lock and builds the bundle from the installed workspace;
#   * agentmodels_omp_test.go:113 probes the prompts this checkout ships, laid out as the release
#     lays them out: packages/pi-legion/agents copied and each plugin's partition of skills/
#     staged with scripts/pi-plugin-prepack.sh itself (shippedPromptPlugin, :165-167);
#   * agentmodels_omp_test.go:186 probes a plugin it writes itself and reads no file of the
#     repository. It is here because it is the other test of agentmodels_omp_test.go, and RULES is
#     keyed on the source file rather than on the test: with both of that file's tests in this job,
#     no test of that file runs outside this job, so what the plugin-pack gate declares below is
#     the whole truth about the file. hold_plugin_pack_tests(), below, is what holds that, rather
#     than a reader noticing: it refuses a test of that file this list does not name, and a name
#     this list holds that the package no longer declares.
# Their reads - the lockfile, the two plugin packages, what their prepack reads, and skills/ - are
# inputs of this one job rather than of the daemon's whole Go suite, so a lockfile-only change, or
# a change to one skill, runs these tests and no other daemon Go test (dispatch://INFRA-1413, A4).
# Both step texts are built from this one list, and check-ci-filters.sh pins each step's whole
# script, so the -skip and the -run cannot come to name different tests.
PLUGIN_PACK_TESTS = ["TestTheRealPluginsOnTheRealOhMyPi", "TestTheAgentModelCheckOnTheRealOhMyPi",
                     "TestTheImageProbeResolvesTheAgentsTheDaemonsPromptsName"]
PLUGIN_PACK_PACKAGE = "internal/daemon"
PLUGIN_PACK_JOB = "plugin-pack"
PLUGIN_PACK_STEP = "Pack the plugin releases and run the prompt probes on the real Oh My Pi"
PLUGIN_PACK_PATTERN = f"'^({'|'.join(sorted(PLUGIN_PACK_TESTS))})$'"
PLUGIN_PACK_SCRIPT = f"go test -count=1 -timeout 15m -run {PLUGIN_PACK_PATTERN} ./{PLUGIN_PACK_PACKAGE}/"
# The skip belongs to the step that would otherwise run these tests, so the two lists are held
# together here rather than by a reader noticing: a package moved out of the race step while the
# pack tests stayed in it would run them twice and read the lockfile in both jobs.
if PLUGIN_PACK_PACKAGE not in DAEMON_RACE_PACKAGES:
    raise ValueError(f"the plugin-pack tests are in {PLUGIN_PACK_PACKAGE}, which the race step does not run, so "
                     f"nothing skips them there; name that package's step here, or move the -skip to the step that "
                     f"runs them")
DAEMON_RACE_SCRIPT = f"go test -count=1 -race -timeout 30m -skip {PLUGIN_PACK_PATTERN} " + " ".join(DAEMON_RACE_PKGS)


PLUGIN_PACK_SOURCE = f"{DAEMON}/{PLUGIN_PACK_PACKAGE}"
# The one file of that package this job took whole, so the list above speaks for the file and not
# only for the tests it happens to name.
PLUGIN_PACK_WHOLE_FILE = f"{PLUGIN_PACK_SOURCE}/agentmodels_omp_test.go"


def hold_plugin_pack_tests():
    """Refuse a PLUGIN_PACK_TESTS that is no longer the truth about internal/daemon's tests: a
    name this list holds that no test of the package declares, and a test of
    agentmodels_omp_test.go this list does not hold. Held to nothing, both pass every other check
    green while a test leaves this job: the -run matches the names that are left, the -skip stops
    naming the test, so the daemon-go race step runs it - and daemon_go_tests' filter names
    neither skills/** nor scripts/pi-plugin-prepack.sh, whose reads are declared on the
    plugin-pack gate below and keyed on these test names, so a skill edit runs that test in no job
    at all. The second rule is agentmodels_omp_test.go's alone, because that whole file moved into
    this job and the gate's declaration therefore speaks for the file however it grows.
    promptreferences_omp_test.go is not such a file - only its pack test moved, and its other
    test, TestThePromptReferenceProbeOnTheRealOhMyPi, runs in the daemon-go race step and reads no
    file of the repository - so widening the rule to it would refuse a test that belongs where it
    is. A test here is what `go test` runs as one and what the two steps' patterns therefore have
    to name: every top-level Test, Fuzz or Example of GO_TEST_FUNC, whatever its signature and
    however many lines it is written over, less TestMain, which is the package's entry point. A
    Fuzz is one of them because a plain run runs its seed corpus, and every Example is counted,
    since a plain run checks one that has an output comment and counting the rest only refuses
    on the safe side. A declaration counts only where the file
    carries no //go:build line (GO_BUILD_LINE): neither step passes -tags, so a name behind a
    constraint is one the -run cannot reach and the -skip need not skip, and a listed name left
    only in such a file is refused by the first rule below.
    What this guard does not see is a new test elsewhere in the package that packs a plugin
    (packedPlugin): it would run in the race step with none of this gate's reads on that job's
    filter, and a reader of such a test moves it here. Nor does it see a subtest - t.Run names no
    declaration, and both patterns match a top-level test whole - or a tagged test some other
    step's own -tags run compiles, which is that step's to declare. The files are read as text
    (GO_TEST_FUNC over go_code_only, so a `func Test…` in a comment or a string counts for
    nothing), since this script runs where no Go toolchain is guaranteed. A file of the package
    that writes none of these names and is not the whole-file one can declare none of them, so it
    is not scanned: the package's tests are 3.8 MB and go_code_only walks a file character by
    character."""
    declared, whole_file = set(), None
    for path in sorted(Path(PLUGIN_PACK_SOURCE).glob("*_test.go")):
        text = path.read_text()
        here = path.as_posix()
        whole = here == PLUGIN_PACK_WHOLE_FILE
        if not whole and not any(name in text for name in PLUGIN_PACK_TESTS):
            continue
        # A file neither step compiles declares no name either step can use, so it is read as
        # declaring none - the whole-file one included, whose listed tests the first rule then
        # reports as gone.
        names = [] if GO_BUILD_LINE.search(text) else [m.group(1) for m in GO_TEST_FUNC.finditer(go_code_only(text))
                                                       if m.group(1) != "TestMain"]
        declared.update(names)
        if whole:
            whole_file = names
    gone = ", ".join(name for name in sorted(PLUGIN_PACK_TESTS) if name not in declared)
    if gone:
        raise ValueError(f"PLUGIN_PACK_TESTS names {gone}, which no untagged file of {PLUGIN_PACK_SOURCE} declares "
                         f"as a test `go test` runs (a top-level `func Test…`, `func Fuzz…` or `func Example…`, "
                         f"TestMain apart): the -run matches nothing of that name, and the -skip skips nothing, so "
                         f"a test renamed out of this list runs in the daemon-go race step with none of this "
                         f"gate's reads on that job's filter. Neither step passes -tags, so a declaration behind a "
                         f"//go:build line is no declaration here. Follow the rename, take the constraint off the "
                         f"file the test is in, or take the name out and give the test's reads back to a RULES row "
                         f"keyed on the file it is in")
    if whole_file is None:
        raise ValueError(f"PLUGIN_PACK_TESTS is the whole truth about {PLUGIN_PACK_WHOLE_FILE}, which is not there "
                         f"to read; follow the file here, or, where its tests have moved out of this job, take "
                         f"them out of this list and give their reads back to RULES rows keyed on the files they "
                         f"are in now")
    unnamed = ", ".join(name for name in sorted(whole_file) if name not in PLUGIN_PACK_TESTS)
    if unnamed:
        raise ValueError(f"{PLUGIN_PACK_WHOLE_FILE} declares {unnamed}, which PLUGIN_PACK_TESTS does not name: "
                         f"this job took that file whole, so the plugin-pack gate's reads are all of the file's "
                         f"reads only while this list names every test of it - every top-level Test, Fuzz and "
                         f"Example a plain `go test` runs, a Fuzz for its seed corpus and an Example for its "
                         f"output comment. A test of it this list leaves out runs in the daemon-go race step "
                         f"instead, whose filter names neither skills/** nor scripts/pi-plugin-prepack.sh. Add "
                         f"the name to this list, or move the test to a file of its own and give its reads a "
                         f"RULES row keyed on that file")


hold_plugin_pack_tests()


# ---------------------------------------------------------------- the supervise table's pure pins
# The two tests of internal/supervise/table_test.go whose subject is the table itself rather than a
# claim in motion: TestTheTableHasARowOrANamedIgnoreForEveryEventInEveryState, which holds every
# event of the sealed set to a row or a named ignore in every state, and TestNoTwoEventTypesShareAKind.
# Each reads source and nothing else - sealed() (table_test.go:27-58) parses every non-test .go file
# of the directory, and the type-check of those files reads the source of every package they import
# too, since table_test.go:99 builds the checker with importer.ForCompiler(..., "source", ...), so
# the whole import closure is read from source: of this repository internal/claim, phase, runtime,
# wait, treelifecycle and backoff and what those import, which packages/daemon/**/*.go carries, and
# outside it the standard library and the module cache - and neither starts a database, a container,
# tmux or jj, so they are text-check work: they run in the text-checks job, where they wait behind
# no fixture, and the daemon-go plain step, which runs their package, skips them by name, so each
# runs once per run of the router (dispatch://INFRA-1413, R3).
# The other three tests of that file drive a machine and stay in the plain step. Nothing of the file
# moves package - all five read unexported identifiers of internal/supervise - which is why these
# two are run by name rather than moved into a package of text checks.
# Both step texts are built from this one list, and check-ci-filters.sh pins each step's whole
# script, so the -skip and the -run cannot come to name different tests.
SUPERVISE_TABLE_TESTS = ["TestTheTableHasARowOrANamedIgnoreForEveryEventInEveryState",
                         "TestNoTwoEventTypesShareAKind"]
SUPERVISE_TABLE_PACKAGE = "internal/supervise"
SUPERVISE_TABLE_SOURCE = f"{DAEMON}/{SUPERVISE_TABLE_PACKAGE}"
SUPERVISE_TABLE_STEP = "Test the supervise table's two pure pins (no database)"
SUPERVISE_TABLE_PATTERN = f"'^({'|'.join(sorted(SUPERVISE_TABLE_TESTS))})$'"
SUPERVISE_TABLE_SCRIPT = f"go test -count=1 -run {SUPERVISE_TABLE_PATTERN} ./{SUPERVISE_TABLE_PACKAGE}/"
# The skip belongs to the step that would otherwise run these tests, so the two lists are held
# together here rather than by a reader noticing: the plain step runs every package of the module but
# the race step's three and the text checks, and this package moved into one of those while these
# tests stayed here would run them in that step too.
if SUPERVISE_TABLE_PACKAGE in DAEMON_RACE_PACKAGES + [TEXT_CHECK_PACKAGE]:
    raise ValueError(f"the table pins are in {SUPERVISE_TABLE_PACKAGE}, which the plain step leaves out, so "
                     f"nothing skips them there; name that package's step here, or move the -skip to the step "
                     f"that runs them")
DAEMON_PLAIN_SCRIPT = f"""\
set -o pipefail
go list ./... | grep -Ev '{DAEMON_PLAIN_PATTERN}' | xargs go test -count=1 -timeout 15m -skip {SUPERVISE_TABLE_PATTERN}"""


def hold_supervise_table_tests():
    """Refuse a SUPERVISE_TABLE_TESTS that is no longer the truth about internal/supervise's tests:
    a name this list holds that no untagged file of the package declares as a test `go test` runs.
    Held to nothing, a rename passes every other check green - a -run that matches nothing is a
    pass, and the -skip then skips nothing - so the text-checks step would go on saying it runs
    these pins while it ran no test at all, and the gate's pinned texts would agree with each other
    about a name neither step could reach. A test is what hold_plugin_pack_tests() reads one as:
    every top-level Test, Fuzz or Example of GO_TEST_FUNC, TestMain apart, and only in a file
    carrying no //go:build line (GO_BUILD_LINE), since neither step passes -tags and a name behind a
    constraint is one the -run cannot reach and the -skip need not skip.
    hold_plugin_pack_tests()'s second rule - that its list name every test of the one file that job
    took whole - is not ported here, because this list is part of a file's tests rather than all of
    them. That rule holds a gate whose reads are keyed on test names: the plugin-pack gate declares
    the lockfile, skills/ and the prepack because those three tests read them, so a test of that file
    left out of the list would run in the daemon-go race step, whose filter names none of them. These
    two pins read source alone - the package's own, and the source of everything it imports, which
    their type-checker reads from source too (table_test.go:99) and which of this repository is
    packages/daemon's own packages - and both jobs' filters already carry it: text_checks
    through packages/daemon/**/*.go and daemon_go_tests through the module's own closure - so no read
    moves with a name here, and a test of table_test.go this list does not name runs in the plain
    step, which is where every test of this module runs unless something takes it out. Demanding the
    whole file would refuse exactly the arrangement this list exists to make.
    What this guard does not see is a test of one of these names declared by another package of the
    module: the plain step's -skip is the module's, while this job's -run names ./internal/supervise/
    alone, so such a test would be skipped in the one step and not run in the other.
    go-test-step-expansion.py's unrun verdict is what finds that, as it finds every hole a -skip
    leaves. The package's own files are what is scanned, and of those only the ones that write one of
    these names, because the module's test files are 14 MB and go_code_only walks a file character by
    character."""
    declared = set()
    for path in sorted(Path(SUPERVISE_TABLE_SOURCE).glob("*_test.go")):
        text = path.read_text()
        if not any(name in text for name in SUPERVISE_TABLE_TESTS) or GO_BUILD_LINE.search(text):
            continue
        declared.update(m.group(1) for m in GO_TEST_FUNC.finditer(go_code_only(text)) if m.group(1) != "TestMain")
    gone = ", ".join(name for name in sorted(SUPERVISE_TABLE_TESTS) if name not in declared)
    if gone:
        raise ValueError(f"SUPERVISE_TABLE_TESTS names {gone}, which no untagged file of {SUPERVISE_TABLE_SOURCE} "
                         f"declares as a test `go test` runs (a top-level `func Test…`, `func Fuzz…` or "
                         f"`func Example…`, TestMain apart): the text-checks step's -run matches nothing of that "
                         f"name and passes green, and the daemon-go plain step's -skip skips nothing, so the two "
                         f"steps say between them that a test runs in a job where it does not. Neither step passes "
                         f"-tags, so a declaration behind a //go:build line is no declaration here. Follow the "
                         f"rename, take the constraint off the file the test is in, or take the name out and leave "
                         f"the test to the plain step")


hold_supervise_table_tests()


# ---------------------------------------------------------------- the envoy race lane
# The three jobs that run packages/envoy's concurrent packages under the race detector, declared
# once: each job's step name, its -timeout, and the packages the step names, in the order it names
# them. Everything the lane decides is built from this list - the three steps' own texts, the
# envoy-go unit step's `grep -Ev`, which leaves out exactly what the lane runs, and the -run lists
# of the plain step that runs what a race build cannot (RACE_IMPOSSIBLE_SCRIPT below) - and
# check-ci-filters.sh fails when ci.yaml's text for any of them differs from the text built here,
# so a package added to the lane reaches all of them or the check goes red. internal/dispatch/pmdoc
# is not in the lane: it runs plain only.
class RacePackage(NamedTuple):
    """A package of the lane, named under packages/envoy. A `tree` package is the one a step names
    as `./<name>/...`, that directory and every package under it; any other is `./<name>/`, the
    package alone."""

    name: str
    tree: bool = False


class RaceJob(NamedTuple):
    """One job of the lane: the ci.yaml job, its race step's name and -timeout, and its packages."""

    job: str
    step: str
    timeout: str
    packages: tuple


ENVOY_RACE_LANE = (
    RaceJob("envoy-go-race",
            "Race-test delivery, the store, the bus, the listener, the registry store, the session cache and kvwatch",
            "30m", (RacePackage("internal/dispatch/delivery", tree=True), RacePackage("internal/dispatch/store", tree=True),
                    RacePackage("internal/bus"), RacePackage("cmd/listener"), RacePackage("internal/store"),
                    RacePackage("internal/session"), RacePackage("internal/kvwatch", tree=True))),
    RaceJob("envoy-go-race-api", "Race-test the Dispatch API", "30m", (RacePackage("internal/dispatch/api"),)),
    RaceJob("envoy-go-race-docs", "Race-test the document service", "60m", (RacePackage("internal/dispatch/docs"),)),
)
RACE_JOBS = {job.job: job for job in ENVOY_RACE_LANE}


def race_pkgs(job):
    """The package arguments of a race job's step, in the order it names them."""
    return [f"./{p.name}/..." if p.tree else f"./{p.name}/" for p in job.packages]


def race_step(job):
    """The one go command of a race job's step, pinned to the text this declaration builds."""
    pkgs = race_pkgs(job)
    text = f"go test -count=1 -race -timeout {job.timeout} " + " ".join(pkgs)
    return step_run(job.job, job.step, text, ENVOY, pkgs, race=True)


# The unit step runs the module less three things: the lane's packages, which the three race jobs
# run under the detector, internal/integration, which envoy-go's integration step runs, and the
# text checks, which the text-check job runs. Its `grep -Ev` is `/integration`, then the text
# checks, then, in the lane's own order, each package in the form go list's import paths need - a
# tree matching the package and everything under it, any other the package alone.
UNIT_TEST_STEP = "Unit test envoy Go package"
UNIT_TEST_PATTERN = "|".join(
    ["/integration", f"/{TEXT_CHECK_PACKAGE}$"]
    + [f"/{p.name}($|/)" if p.tree else f"/{p.name}$" for job in ENVOY_RACE_LANE for p in job.packages])


# ---------------------------------------------------------------- the web app's display fixture
# The tests of internal/dispatch/pmdoc that touch the committed display-tree fixture the web
# app's own unit tests draw. TestWebDisplayFixture (web_fixtures_test.go:191-200) is the one
# whose subject it is. Its `--check` is a flag of its own (web_fixtures_test.go:13-21): with it
# the test compares the committed fixture with what this build makes and fails where they
# differ, and without it it writes that fixture and asserts nothing. So the fixture step below is
# the run with a requirement behind it, and the envoy-go unit step - which runs every package of
# the module the race lane, internal/integration and the text checks leave it, pmdoc among them -
# skips the test by name: a second run there would rewrite the committed file and pass whatever
# that file held, so the pair would run twice per run of the router with nothing asked of the
# second (dispatch://INFRA-1413). TestTheWebFixtureFindsItsFilesFromAnyDirectory
# (web_fixtures_test.go:248-255) stats that the same file exists, and an existence check is a
# read by this table's own rule (the contracts gate declares skills/dispatch-writing-specs/SKILL.md
# for an existsSync alone), so it runs in the fixture step too and the unit step skips it, which
# leaves the fixture a read of the fixture step alone.
# Both step texts are built from this one list, and check-ci-filters.sh pins each step's whole
# script, so the -skip and the -run cannot come to name different tests.
WEB_FIXTURE_TESTS = ["TestWebDisplayFixture", "TestTheWebFixtureFindsItsFilesFromAnyDirectory"]
WEB_FIXTURE_PACKAGE = "internal/dispatch/pmdoc"
WEB_FIXTURE_SOURCE = f"{ENVOY}/{WEB_FIXTURE_PACKAGE}"
FIXTURE_STEP = "Check the web app's display-tree fixture is up to date"
WEB_FIXTURE_PATTERN = f"'^({'|'.join(sorted(WEB_FIXTURE_TESTS))})$'"
# The fixture check's `-run` and its `-args --check` are not flags the simple form reduces, and
# they move no closure: the whole pmdoc package compiles, whichever test the run selects.
FIXTURE_SCRIPT = f"go test -count=1 -run {WEB_FIXTURE_PATTERN} ./{WEB_FIXTURE_PACKAGE}/ -args --check"
# The skip belongs to the step that would otherwise run these tests, so the two are held together
# here rather than by a reader noticing: the unit step runs every package of the module its
# `grep -Ev` does not take out, and pmdoc taken out of it - moved into the race lane, say - while
# the skip stayed here would skip the test in a step that no longer runs it, and the step that
# did run it would write the committed fixture unchecked. Each alternative of UNIT_TEST_PATTERN
# is written against the tail of what `go list` prints, so the package's own path is what they
# are tried against here.
if re.search(UNIT_TEST_PATTERN, f"/{WEB_FIXTURE_PACKAGE}"):
    raise ValueError(f"the display-tree fixture tests are in {WEB_FIXTURE_PACKAGE}, which the unit step's "
                     f"`grep -Ev` takes out, so that step does not run them and the -skip below skips nothing; "
                     f"name the step that runs that package now and move the -skip to it")
UNIT_TEST_SCRIPT = (f"go list ./... | grep -Ev '{UNIT_TEST_PATTERN}' | "
                    f"xargs go test -count=1 -timeout 20m -skip {WEB_FIXTURE_PATTERN}")


def hold_web_fixture_tests():
    """Refuse a WEB_FIXTURE_TESTS that is no longer the truth about internal/dispatch/pmdoc's
    tests: a name this list holds that no untagged file of the package declares as a test
    `go test` runs. Held to nothing, a rename passes every other check green - a -run that
    matches nothing is a pass, and the -skip then skips nothing - so the fixture step would go on
    saying it checks the committed display tree while it ran no test at all, and the renamed test
    would run in the unit step instead, where no `-args --check` reaches it: there it writes the
    committed fixture and asserts nothing, so the one check standing between a changed display
    reading and a stale fixture the web app's view tests draw would be gone with every step
    green. A test is what hold_plugin_pack_tests() reads one as: every top-level Test, Fuzz or
    Example of GO_TEST_FUNC, TestMain apart, and only in a file carrying no //go:build line
    (GO_BUILD_LINE), since neither step passes -tags and a name behind a constraint is one the
    -run cannot reach and the -skip need not skip.
    Of that precedent's two rules this ports the first and not the second - that its list name
    every test of the one file the job took whole - as hold_supervise_table_tests() does not,
    for that guard's reason and one of this list's own. Its reason: the whole-file rule holds a
    gate whose reads are keyed on test names, the plugin-pack gate declaring the lockfile,
    skills/ and the prepack because those three tests read them, while this gate's reads are
    keyed on the package - the fixture step's Run is ./internal/dispatch/pmdoc/ and
    envoy-go-checks' filter carries packages/envoy/** - so no read moves with a name here. Its
    own: what this list takes out of the unit step is one test's second run rather than a file's
    tests, and every other test of web_fixtures_test.go runs in the unit step, which is where
    every test of this module runs unless something takes it out, so demanding the whole file
    would refuse the very arrangement this list makes.
    What this guard does not see is a test of this name declared by another package of the
    module: the unit step's -skip is the module's, while the fixture step's -run names
    ./internal/dispatch/pmdoc/ alone, so such a test would be skipped in the one step and run in
    neither. Nor does it see a second test of this package that comes to write the same fixture,
    which runs unchecked in the unit step until a reader names it here, where the -run then runs
    it under --check and the -skip takes it out of that step. go-test-step-expansion.py's two
    verdicts are what find both, as they find every hole a -skip leaves. The package's own files
    are what is scanned, and of those only the ones that write one of these names, because this
    package's tests are 1 MB over 79 files and go_code_only walks a file character by
    character."""
    declared = set()
    for path in sorted(Path(WEB_FIXTURE_SOURCE).glob("*_test.go")):
        text = path.read_text()
        if not any(name in text for name in WEB_FIXTURE_TESTS) or GO_BUILD_LINE.search(text):
            continue
        declared.update(m.group(1) for m in GO_TEST_FUNC.finditer(go_code_only(text)) if m.group(1) != "TestMain")
    gone = ", ".join(name for name in sorted(WEB_FIXTURE_TESTS) if name not in declared)
    if gone:
        raise ValueError(f"WEB_FIXTURE_TESTS names {gone}, which no untagged file of {WEB_FIXTURE_SOURCE} "
                         f"declares as a test `go test` runs (a top-level `func Test…`, `func Fuzz…` or "
                         f"`func Example…`, TestMain apart): the fixture step's -run matches nothing of that "
                         f"name and passes green, and the unit step's -skip skips nothing, so the committed "
                         f"fixture is written by a run carrying no --check and checked by no run at all. "
                         f"Neither step passes -tags, so a declaration behind a //go:build line is no "
                         f"declaration here. Follow the rename, take the constraint off the file the test is "
                         f"in, or, where the fixture check has gone, take the name out and leave the test to "
                         f"the unit step")


hold_web_fixture_tests()


# Every top-level function declaration, test or not: a gofmt'd file puts the `func` of one in
# column 0. Where its body ends is not a line pattern - a one-line body writes no closing brace
# in column 0 - so go_top_level_spans() counts braces from the declaration's own `{`.
GO_TOP_FUNC = re.compile(r"^func\b", re.M)
GO_BRACE = re.compile(r"[{}]")
RACE_ENABLED = re.compile(r"\braceEnabled\b")
RACE_ENABLED_DECLARATION = re.compile(r"^const +raceEnabled\b|^\t*raceEnabled +=", re.M)


def go_top_level_spans(code: str, path) -> list:
    """Each top-level function declaration of `code` as (start, end): the offset of its `func`,
    and the offset of the `}` closing its own body, counted forward from the first `{` of the
    declaration. `code` has been through go_code_only, so no brace of a comment, a string or a
    rune literal is counted, and a one-line body ends on its own line rather than running to the
    next closing brace in column 0. A declaration with no body - no `{` before the next
    declaration, as an assembly stub has - has no span, so nothing is attributed to it. A
    declaration whose braces never balance raises: the rest of the file is not its body, and a
    file this cannot parse must make ci-filters red rather than be guessed at."""
    starts = [m.start() for m in GO_TOP_FUNC.finditer(code)]
    spans = []
    for i, start in enumerate(starts):
        limit = starts[i + 1] if i + 1 < len(starts) else len(code)
        opened = code.find("{", start, limit)
        if opened < 0:
            continue
        depth = 0
        for brace in GO_BRACE.finditer(code, opened):
            depth += 1 if brace.group() == "{" else -1
            if depth == 0:
                spans.append((start, brace.start()))
                break
        else:
            raise ValueError(f"{path}:{code.count(chr(10), 0, start) + 1}: the braces of this top-level "
                             "declaration never balance, so ci_job_reads.py cannot find where its body ends; "
                             "the file is not one race_impossible_tests_of() can read")
    return spans


# The eight static checks of internal/dispatch/api, keyed by their path in this repository: the
# seven source guards and one_expression_guard_test.go, which reads the module's text for the same
# reason. Each reads the module's own source and carries `//go:build !race` for it, so the walk
# below would put all fifteen of their tests in the plain step; they are left out of it instead,
# because the text-check job runs them by name and a test run twice is a test whose second run
# tells nobody anything. The exemption is held to the files it is about in three ways, so a
# sixteenth such test cannot quietly rejoin the plain step: a file named here that does not
# carry the tag is refused, a file whose tests are not exactly the names here is refused
# (race_impossible_tests_of), and a name here the lane's walk never reads is refused
# (race_impossible_tests). internal/bus's and internal/dispatch/docs' race-skipped tests are in
# none of this: they stay in the plain step.
TEXT_CHECK_TESTS = {
    f"{ENVOY}/internal/dispatch/api/copy_lock_order_test.go": {
        "TestCopyLockComesBeforeEveryPendingRowAndEvent",
        "TestCopyLockOrderGuardFollowsTheLockTakers"},
    f"{ENVOY}/internal/dispatch/api/decision_block_route_guard_test.go": {
        "TestEveryRouteThatTakesABodyRefusesADecisionBlock",
        "TestTheDecisionBlockGuardSeesWhatItGuards",
        "TestTheGuardReadsABodyStringOffARequestType"},
    f"{ENVOY}/internal/dispatch/api/event_insert_guard_test.go": {
        "TestOnlyTheBrokerInsertsEvents"},
    f"{ENVOY}/internal/dispatch/api/keyed_write_guard_test.go": {
        "TestEveryKeyedWriteCommitsInsideATransactionTheMarkHooks",
        "TestTheKeyedWriteGuardSeesWhatItGuards"},
    f"{ENVOY}/internal/dispatch/api/lock_level_test.go": {
        "TestNoForUpdateOnOwnerTables"},
    f"{ENVOY}/internal/dispatch/api/one_expression_guard_test.go": {
        "TestOneExpressionDecidesWhoWaitsInOneSpelling"},
    f"{ENVOY}/internal/dispatch/api/review_order_guard_test.go": {
        "TestEveryReviewReadOrdersByReviewOrder",
        "TestTheReviewOrderGuardReadsBothOrders"},
    f"{ENVOY}/internal/dispatch/api/review_write_guard_test.go": {
        "TestOnlyWriteReviewWritesReviews",
        "TestTheReviewWriterPatternsReadEveryNameOfTheTable",
        "TestTheReviewWriterScanFindsAnIdentifierNamingTheTable"},
}


def race_impossible_tests_of(path: Path) -> set:
    """The tests of one _test.go file that only a plain run covers: every test of a file the race
    build does not compile (`//go:build !race`), and every test whose body reads raceEnabled, the
    constant behind which a test runs smaller under the detector, so the plain run is its full-size
    one. The constant's own declaration, and any mention of it in a comment or a string, count for
    nothing. Raises on a //go:build that names race in a form this does not read, on a file whose
    braces go_top_level_spans() cannot balance, and on a raceEnabled no top-level test encloses:
    the read is attributed to the top-level function whose body - its own, up to the brace that
    closes it - holds it, and only when that function is itself a test, so a read in a helper,
    at package level or in a function literal outside any declaration - which a later test may be
    the one caller of - makes ci-filters red rather than giving a test that does not run smaller
    the plain run and dropping the one that does. A file TEXT_CHECK_TESTS names is the exception:
    it names no test here, since the text-check job runs its tests by name, and it is refused
    unless it carries the tag and its tests are exactly the names that list holds for it."""
    text = path.read_text()
    found = GO_BUILD_LINE.search(text)
    constraint = found.group(1) if found else ""
    if constraint not in ("", "race", "!race") and re.search(r"\brace\b", constraint):
        raise ValueError(f"{path}: `//go:build {constraint}` names race in a form ci_job_reads.py does not read; "
                         "teach race_impossible_tests_of() the form, or the plain step loses the file's tests")
    exempt = TEXT_CHECK_TESTS.get(path.as_posix())
    if exempt is not None and constraint != "!race":
        raise ValueError(f"{path}: TEXT_CHECK_TESTS leaves this file's tests out of the plain step because the "
                         "text-check job runs them by name, but the file does not carry `//go:build !race`, so "
                         "the race build still compiles and runs them; restore the tag, or drop the row")
    # A file the race build compiles and that nowhere writes raceEnabled, in code or in a comment,
    # names no test here and refuses nothing, so it never reaches the blanking pass below. That is
    # almost every file of the lane: 610 of them, 10.9 MB, which this guard takes from 3.0 s to
    # 0.56 s on each import of this module, one per check-ci-filters.sh run and five more per run
    # of its test.
    if constraint != "!race" and not RACE_ENABLED.search(text):
        return set()
    code = go_code_only(text)
    # A file the race build alone compiles has no test a plain run can name, so every raceEnabled
    # of it but the declaration is unattributable, as the empty list below makes it.
    tests = [] if constraint == "race" else [(m.start(), m.group(1)) for m in GO_TEST_FUNC.finditer(code)
                                             if m.group(1) != "TestMain"]
    names = {name for _, name in tests} if constraint == "!race" else set()
    enclosing = dict(tests)  # the start of a top-level test -> its name
    spans = go_top_level_spans(code, path)
    # Each declaration's own line, read from the declaration rather than from the use, so a file
    # that declares the constant and also reads it in a test has that read attributed, not passed
    # over.
    declarations = [(m.start(), code.find("\n", m.start())) for m in RACE_ENABLED_DECLARATION.finditer(code)]
    for use in RACE_ENABLED.finditer(code):
        if any(start <= use.start() < (len(code) if end < 0 else end) for start, end in declarations):
            continue
        holder = next((start for start, end in spans if start <= use.start() <= end), None)
        if holder not in enclosing:
            raise ValueError(f"{path}:{code.count(chr(10), 0, use.start()) + 1}: raceEnabled is read where no "
                             "top-level test encloses it, so ci_job_reads.py cannot say which test runs smaller "
                             "under the detector; move the read into the test that runs smaller for it, or teach "
                             "race_impossible_tests_of() the idiom")
        names.add(enclosing[holder])
    if exempt is not None:
        unnamed = ", ".join(sorted(names - exempt))
        stale = ", ".join(sorted(exempt - names))
        if unnamed or stale:
            raise ValueError(f"{path}: TEXT_CHECK_TESTS does not name this file's tests exactly - "
                             f"in the file and not in the list: {unnamed or 'none'}; in the list and not in the "
                             f"file: {stale or 'none'}. A guard test the text-check job does not run by name "
                             "belongs in the plain step, so name it in both places or in neither")
        return set()
    return names


def race_lane_packages(root) -> list:
    """Every package directory the lane's steps run, in the order its steps name them: a package
    alone for an exact entry, and for a tree its directory and every directory with .go files
    under it, passing over what go passes over (go_skips_dir, the rule pattern_dirs walks a
    `/...` pattern by). Each is (the path under `root`, the directory)."""
    root, seen, found = Path(root), set(), []
    for job in ENVOY_RACE_LANE:
        for package in job.packages:
            base = root / package.name
            if not base.is_dir():
                raise ValueError(f"the race lane names {package.name}, which is no directory under {root}")
            directories = [base]
            if package.tree:
                for current, subdirs, names in os.walk(base):
                    here = Path(current)
                    subdirs[:] = sorted(s for s in subdirs if not go_skips_dir(here, s))
                    if here != base and any(n.endswith(".go") for n in names):
                        directories.append(here)
            for directory in directories:
                under = directory.relative_to(root).as_posix()
                if under not in seen:
                    seen.add(under)
                    found.append((under, directory))
    return found


def race_impossible_tests(root) -> dict:
    """Per package of the lane under `root`, in the lane's own order, the tests only a plain run
    covers (race_impossible_tests_of). A package that has none is left out. A file TEXT_CHECK_TESTS
    names under `root` that this walk never reads is refused: the row would otherwise go on
    exempting a file that has moved out of the lane, or been deleted, and say nothing."""
    found, read = {}, set()
    for under, directory in race_lane_packages(root):
        names = set()
        for path in sorted(directory.glob("*_test.go")):
            read.add(path.as_posix())
            names |= race_impossible_tests_of(path)
        if names:
            found[under] = names
    gone = sorted(f for f in TEXT_CHECK_TESTS if f.startswith(f"{root}/") and f not in read)
    if gone:
        raise ValueError(f"TEXT_CHECK_TESTS names {', '.join(gone)}, which the lane's walk of {root} does not "
                         "read: the file moved out of the lane or is gone, and its row now exempts nothing; "
                         "follow the file, or drop the row")
    return found


def race_impossible_script(packages) -> str:
    """envoy-go's plain run of the tests the detector makes impossible: one `go test` per package
    of `packages` (race_impossible_tests), each naming that package's own tests, anchored and
    sorted, in its -run."""
    lines = []
    for under, names in packages.items():
        lines.append(f"go test -count=1 -timeout 10m -run '^({'|'.join(sorted(names))})$' ./{under}")
    return "\n".join(lines)


# The step as the lane and packages/envoy's own source make it, so a test added under
# `//go:build !race` or behind raceEnabled changes this text and ci.yaml stops matching it.
RACE_IMPOSSIBLE_STEP = "Test what the race build cannot run at all or at full size"
RACE_IMPOSSIBLE_PACKAGES = race_impossible_tests(ENVOY)
RACE_IMPOSSIBLE_SCRIPT = race_impossible_script(RACE_IMPOSSIBLE_PACKAGES)


# The text-check job: the three modules' text checks, and the fifteen tests of the Dispatch API's
# eight static checks - its seven source guards and one_expression_guard_test.go. They read the
# module's source and no database, so they belong here rather than in the Postgres-backed
# suites, and they are the one thing here the job cannot name by package: their package holds
# the API's own Postgres-backed tests too, so the step selects them by name. The -run list is
# built from TEXT_CHECK_TESTS, the same declaration that leaves exactly those fifteen out of
# envoy-go's plain step, so neither step can lose one of them or run one twice. Each call passes
# -count=1: these packages read files outside their own modules, which Go's test cache keys on
# neither, so a cached pass would say nothing about the text as it now stands.
TEXT_CHECK_JOB = "text-checks"
TEXT_CHECK_PKG = f"./{TEXT_CHECK_PACKAGE}/"
TEXT_CHECK_TEXT = f"go test -count=1 {TEXT_CHECK_PKG}"
# The three modules whose text checks the job runs, in the order its steps run them: each step's
# name carries the module path, so the path is written once and the name is built from it.
TEXT_CHECK_MODULES = (ENVOY, INFRA, DAEMON)
TEXT_CHECK_STEPS = {module: f"Check the text {module} pins" for module in TEXT_CHECK_MODULES}
GUARD_PACKAGE = "./internal/dispatch/api"
GUARD_STEP = "Test the Dispatch API's source guards (no database)"
GUARD_SCRIPT = ("go test -count=1 -run "
                f"'^({'|'.join(sorted(name for names in TEXT_CHECK_TESTS.values() for name in names))})$' "
                f"{GUARD_PACKAGE}")


RUN_SERVER = "packages/dispatch/e2e/run-server.sh"
BUILD_BINARIES = "docs/site/scripts/build-binaries.sh"
DISPATCH_SERVER_BUILD = "flock ./.dispatch-e2e.lock go build -o ./dispatch-e2e ./cmd/dispatch"
# The three commands of the real broker's harness rows, and the Dispatch server with them: the
# binaries job of dispatch-browser.yaml builds all four, since the shard and alone jobs run
# them in a container and so run no go command of their own (that file says why).
BROKER_PKGS = ["./cmd/broker", "./cmd/dev-oidc-issuer", "./internal/broker/brokertest/requestseed"]
# The steps of .github/workflows/dispatch-browser.yaml this table cites by name.
BROWSER_BINARIES_STEP = "Build the binaries the browser suite runs"
BROWSER_BINARIES_SCRIPT = """\
mkdir -p "$RUNNER_TEMP/dispatch-e2e-bin"
CGO_ENABLED=0 go build -o "$RUNNER_TEMP/dispatch-e2e-bin/" ./cmd/dispatch ./cmd/broker ./cmd/dev-oidc-issuer ./internal/broker/brokertest/requestseed"""
BROWSER_SHARD_SPECS = "Browser-test dispatch dashboard (shard ${{ matrix.shard }} of 4)"
BROWSER_ALONE_SPECS = "Browser-test dispatch dashboard (${{ matrix.project }})"
BROWSER_MP_SPECS = "Browser-test dispatch dashboard against two server processes"
BROWSER_MP_SIGNIN = "Browser-test a sign-in across two server processes"
BROWSER_EMBEDDER_SPECS = "Browser-test the bar's two tiers against a counting embedder"
BROWSER_MP_BUILD = "Build the Dispatch server"
BROWSER_EMBEDDER_BUILD = "Build the Dispatch server for the embedder lane"
BROWSER_MERGE_STEP = "Merge the legs' blob reports into one report"
BROWSER_SELECT_STEP = "Take the newest attempt of each leg's blob report"
BROWSER_READ_ONE_STEP = "Reading one, the rows"
BROWSER_BLOBS_REFUSED_STEP = "Report a refused blob download as a refused read"
BROWSER_PG_STEP = "Read the lanes' PostgreSQL and time its provider"
# The visual job's own step, which runs e2e/visual-run.sh: that script seeds on the host, where
# e2e/run-server.sh's `go build` of the Dispatch server has already run, and then runs Playwright
# in the pinned container. That build compiles ./cmd/dispatch, which the multiprocess job's
# pinned build step in the gate below names already, so this job adds no command of its own.
BROWSER_VISUAL_SPECS = "Browser-test dispatch dashboard against its baselines"
# The six steps that run the browser suite, named together wherever a Run or a read cites the
# whole suite: one list, so a step renamed above reaches every citation of it. It is declared
# here, below the six names, rather than beside the first of them.
BROWSER_SUITE_STEPS = (
    f"{at_browser(BROWSER_SHARD_SPECS)}, {at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)}, "
    f"{at_browser(BROWSER_MP_SPECS, BROWSER_MP)}, {at_browser(BROWSER_MP_SIGNIN, BROWSER_MP)}, "
    f"{at_browser(BROWSER_EMBEDDER_SPECS, BROWSER_EMBEDDER)} and "
    f"{at_browser(BROWSER_VISUAL_SPECS, BROWSER_VISUAL)}"
)
STALL = "envoy-session-stall"
STALL_CLIENT = "Stall test of envoy-client's NATS connection options"
LOADTEST_RUN_STEP = "One-run-at-a-time test for loadtest/run-step.sh and run-series.sh (fakes go and uv; no stack)"
LOADTEST_LOCK = "Lock test for loadtest/common.sh (inherited lock, refusals, holders in another PID namespace; no stack)"
SMOKE_IMAGE_STEP = "Build the listener image the smoke test runs against"
BASE_IMAGES_STEP = "Read the smoke image's base images"
FOLD_CHECK_STEP = "Check every changelog fragment renders"
CHECK_STEP = "Check every CI filter matches every file its job reads"
GUIDES_STEP = "Check the guides' semantic line breaks"
COPY_STEP = "Copy every image the package does not hold"
PACKAGE_STEP = "Check the package holds every row of .github/ci-images.yaml"
TOOL_COPY_STEP = "Fill the copy of every tool asset the package does not hold"
# The steps of the tool-fallback job the reads below cite. The job is one long drive of
# .github/scripts/ci-tools-proof.sh, so each file it reads is read at a step with a name.
TOOL_BLOB_STEP = "Check the package holds the blob of every mirrored row"
TOOL_ROWS_STEP = "Print every row beside the two call sites that read its digest"
TOOL_STAGE_STEP = "Stage the correct bytes of every row a case serves, before the fault"
TOOL_FAULT_STEP = "Start the fault in front of GitHub's release assets"
TOOL_JJ_SEED_STEP = "Seed mise's install of jj with the staged binary, one byte changed"
TOOL_BUN_BYTE_STEP = "Change the last byte of the installed Bun"
TOOL_WATCH_STEP = "Start the machine watch over every faulted install"
# Both jobs name their report step alike, so one constant cites it in both gates.
TOOL_REPORT_STEP = "Report what this job ran on"
# The step of the tool-fallback-arm job the reads below cite, beside the fault step that job
# shares with the one above: the reading of the Bun this runner holds, which the rig makes
# against the bun-linux-aarch64 row.
TOOL_ARM_READ_STEP = "Read the Bun the release asset served"
TOOL_ARM_WATCH_STEP = "Start the machine watch over both halves"
# The session message-wait suite's own job: the test job's one step that needs a database.
SESSIONS_JOB = "sessions-script"
SESSIONS_STEP = "Test the session message-wait script"

# ------------------------------------------------- the test job's script harnesses, one gate each
# Each `*.test.sh` step of the test job tests one script of this repository, and runs behind a
# changes output of its own naming that harness and that script. Before this table one pattern,
# `.github/scripts/*`, stood for all of them on the job's own filter, so a change to any one
# script ran every harness - and, while the daemon's Go suite was in the same job, 1,083 s
# (median) of Go tests with them (dispatch://INFRA-1413). The job's filter now names what these
# gates name, through the anchors ci.yaml holds each of these lists under, so a change to a
# harness or its subject selects the job and exactly that step's gate is true in it.
#
# Every harness under .github/scripts sources test-lib.sh for its `check`, `is` and `summary`,
# so each of those gates reads it and a change to it runs them all; scripts/sessions-import-pods.test.sh,
# which is not one of them, carries its own helpers.
SCRIPTS = ".github/scripts"
TEST_LIB = f"{SCRIPTS}/test-lib.sh"


def harness(filter, step, harness_script, subjects, lib=True):
    """A gate for one harness step of the test job: the harness itself, test-lib.sh where the
    harness sources it, and `subjects`, the (path, why) pairs of what else that step reads - the
    script it tests first among them. The gate is the step's, so its `if:` must test this
    filter and no other (rule 2)."""
    entries = [(harness_script, at("test", step))]
    if lib:
        entries.append((TEST_LIB, f"{harness_script} sources it for check/is/summary"))
    return gate(filter, "test", step=step,
                reads=reads(*entries, *subjects))


# The private-name port harness, a job of its own since dispatch://INFRA-1413: it is the one
# harness of this repository that builds a binary of this head, so the filter that selects it
# has to name that build's whole closure (LEGION_BUILD, ci.yaml's legion_command). While the
# step was the test job's, that closure was on the `test` filter and a change to any package of
# the daemon ran thirty-odd shell harnesses and the Bun suites of scripts/ beside it; a gate on
# the step inside that job was no cure either, since the gate's own filter would be the closure
# a second time and the changes job's filters string has no room for one (rule 18).
PRIVATE_NAMES_PORT_JOB = "private-names-port"
PRIVATE_NAMES_PORT_STEP = "Test the private-name rules' port against the frozen script"
PRIVATE_NAMES_PORT_BUILD = script_run(
    f"{SCRIPTS}/check-private-names-port.test.sh",
    '(cd "$repo_root/packages/daemon" && go build -o "$engine" ./cmd/legion) || {',
    at(PRIVATE_NAMES_PORT_JOB, PRIVATE_NAMES_PORT_STEP), DAEMON, [LEGION_BUILD], mode="build")


SCRIPT_TEST_GATES = [
    harness("release_push_test", "Test release scripts", f"{SCRIPTS}/release-push.test.sh", [
        (f"{SCRIPTS}/release-push.sh", "the script under test"),
        (f"{SCRIPTS}/release-bump.sh", "release-push.sh:778 runs it for `version`, and the version cases read its refusals"),
        (".github/workflows/release.yaml", "release-push.test.sh:1005 reads it for the checkout-before-push case"),
        ("packages/pi-legion/.changie.yaml", "release-push.test.sh:1928 copies the shipped configuration into its fold-notes fixture"),
    ]),
    harness("release_bump_test", "Test the release version bump", f"{SCRIPTS}/release-bump.test.sh", [
        (f"{SCRIPTS}/release-bump.sh", "the script under test"),
    ]),
    harness("check_bun_version_test", "Test the Bun pin check", f"{SCRIPTS}/check-bun-version.test.sh", [
        (f"{SCRIPTS}/check-bun-version.sh", "the script under test"),
        (f"{SCRIPTS}/ci_images.py", "check-bun-version.test.sh:49 symlinks it in: the gate reads an image reference with it"),
    ]),
    harness("check_playwright_version_test", "Test the Playwright version check",
            f"{SCRIPTS}/check-playwright-version.test.sh", [
        (f"{SCRIPTS}/check-playwright-version.sh", "the script under test"),
        (f"{SCRIPTS}/ci_images.py", "check-playwright-version.test.sh:49 symlinks it in: the script reads a row's upstream with it"),
        ("bun.lock", "its \"the repository's own tree\" case runs the real check, which resolves this repository's own Playwright out of it"),
        (".github/docker/browser.Dockerfile", "that same case: the check reads the browser image's base FROM out of it"),
        (".github/ci-images.yaml", "that same case: the check reads the row the base FROM's digest names"),
    ]),
    harness("check_bun_workspace_manifests_test", "Test the frozen Bun workspace manifest check",
            f"{SCRIPTS}/check-bun-workspace-manifests.test.sh", [
        (f"{SCRIPTS}/check-bun-workspace-manifests.sh", "the script under test"),
    ]),
    harness("check_zod_version_test", "Test the zod version check", f"{SCRIPTS}/check-zod-version.test.sh", [
        (f"{SCRIPTS}/check-zod-version.sh", "the script under test"),
    ]),
    harness("check_gh_version_test", "Test the gh version check", f"{SCRIPTS}/check-gh-version.test.sh", [
        (f"{SCRIPTS}/check-gh-version.sh", "the script under test"),
    ]),
    harness("check_gitleaks_version_test", "Test the gitleaks version check",
            f"{SCRIPTS}/check-gitleaks-version.test.sh", [
        (f"{SCRIPTS}/check-gitleaks-version.sh",
         "the script under test, symlinked into every fixture (check-gitleaks-version.test.sh:30)"),
    ]),
    harness("workflow_paths_test", "Test the workflow path model the path checks import",
            f"{SCRIPTS}/workflow_paths.test.sh", [
        (f"{SCRIPTS}/workflow_paths.py", "the module under test, symlinked in at workflow_paths.test.sh:29"),
    ]),
    harness("check_image_trigger_paths_test", "Test the image trigger path check",
            f"{SCRIPTS}/check-image-trigger-paths.test.sh", [
        (f"{SCRIPTS}/check-image-trigger-paths.sh", "the script under test"),
        (f"{SCRIPTS}/workflow_paths.py", "check-image-trigger-paths.test.sh:34 symlinks in the path model the script imports"),
    ]),
    harness("check_ci_images_test", "Test the CI image check", f"{SCRIPTS}/check-ci-images.test.sh", [
        (f"{SCRIPTS}/check-ci-images.sh", "the script under test"),
        (f"{SCRIPTS}/workflow_paths.py", "check-ci-images.test.sh:92 symlinks in the path model its sweep walks the tree with"),
        (f"{SCRIPTS}/ci_images.py", "check-ci-images.test.sh:93 symlinks in the reference grammar it reads a reference with"),
        (".github/ci-images.yaml", "its last case runs the package mode over this repository's own rows"),
    ]),
    harness("mirror_ci_images_test", "Test the CI image mirror", f"{SCRIPTS}/mirror-ci-images.test.sh", [
        (f"{SCRIPTS}/mirror-ci-images.sh", "the script under test"),
        (f"{SCRIPTS}/check-ci-images.sh", "mirror-ci-images.test.sh:355 runs the gate over the registry a copy left"),
        (f"{SCRIPTS}/ci_images.py", "mirror-ci-images.test.sh:134 symlinks in the grammar both scripts read a row with"),
        (".github/ci-images.yaml", "mirror-ci-images.test.sh:322-351 builds its ordering fixture from this repository's own rows"),
    ]),
    # The tool scripts, in the order the job runs them: the fetch helper every install goes
    # through, the filler that puts the bytes in the copy that helper reads, the gate that holds
    # every row to its pin, and the rig that drives that helper against a fault in the
    # tool-fallback job (main's #3576 gave that rig a harness of its own).
    harness("ci_tools_test", "Test the tool fetch helper", f"{SCRIPTS}/ci-tools.test.sh", [
        (f"{SCRIPTS}/ci-tools.sh", "the script under test"),
        (".github/ci-tools.yaml", "ci-tools.test.sh:525-532 reads every row of this repository's own manifest"),
    ]),
    harness("mirror_ci_tools_test", "Test the tool copy filler", f"{SCRIPTS}/mirror-ci-tools.test.sh", [
        (f"{SCRIPTS}/mirror-ci-tools.sh", "the script under test"),
        (f"{SCRIPTS}/ci-tools.sh", "mirror-ci-tools.test.sh:357 symlinks in the fetch helper the filler sources"),
    ]),
    # main's #3179 gave the gate rule 3's own-repository test: it runs .github/scripts/repo-paths.sh
    # for this repository's addresses, so that script is a second subject of this step.
    harness("check_ci_tools_test", "Test the tool manifest check", f"{SCRIPTS}/check-ci-tools.test.sh", [
        (f"{SCRIPTS}/check-ci-tools.sh", "the script under test"),
        (f"{SCRIPTS}/repo-paths.sh", "check-ci-tools.sh:885-887 runs it in bash for plugin_download_url, and "
                                     "check-ci-tools.test.sh:103 links it into every fixture"),
    ]),
    harness("ci_tools_proof_test", "Test the tool fallback rig", f"{SCRIPTS}/ci-tools-proof.test.sh", [
        (f"{SCRIPTS}/ci-tools-proof.sh", "the script under test"),
        (f"{SCRIPTS}/ci-tools.sh", "ci-tools-proof.sh:379 sources the fetch helper, so every case of the harness reads it"),
    ]),
    harness("check_action_pins_test", "Test the action pin check", f"{SCRIPTS}/check-action-pins.test.sh", [
        (f"{SCRIPTS}/check-action-pins.sh", "the script under test"),
    ]),
    harness("check_private_names_test", "Test the private-name check", f"{SCRIPTS}/check-private-names.test.sh", [
        (f"{SCRIPTS}/check-private-names.sh", "the script under test"),
        (f"{SCRIPTS}/check-pr-text.sh", "check-private-names.test.sh:525 runs it too: the pull request's text is the guard's other surface"),
    ]),
    harness("check_plugin_names_test", "Test the plugin-name check", f"{SCRIPTS}/check-plugin-names.test.sh", [
        (f"{SCRIPTS}/check-plugin-names.sh", "the script under test"),
    ]),
    harness("check_fireevent_reasons_test", "Test the fireEvent reason check",
            f"{SCRIPTS}/check-fireevent-reasons.test.sh", [
        (f"{SCRIPTS}/check-fireevent-reasons.sh", "the script under test"),
    ]),
    harness("check_daemon_api_contract_test", "Test the daemon API contract number check",
            f"{SCRIPTS}/check-daemon-api-contract.test.sh", [
        (f"{SCRIPTS}/check-daemon-api-contract.sh", "the script under test"),
    ]),
    harness("check_release_credentials_test", "Test the release credential check",
            f"{SCRIPTS}/check-release-credentials.test.sh", [
        (f"{SCRIPTS}/check-release-credentials.sh", "the script under test"),
    ]),
    harness("repo_paths_test", "Test the repository path derivation", f"{SCRIPTS}/repo-paths.test.sh", [
        (f"{SCRIPTS}/repo-paths.sh", "the script under test"),
    ]),
    harness("select_e2e_proofs_test", "Test the e2e proof selector", f"{SCRIPTS}/select-e2e-proofs.test.sh", [
        (f"{SCRIPTS}/select-e2e-proofs.sh", "the script under test"),
        (".github/e2e-scripts.json", "its last case runs the selector over this repository's own list"),
        ("scripts/e2e/*.sh", "that same case: the selector names one row per scripts/e2e/*.sh (select-e2e-proofs.sh:175 globs them)"),
    ]),
    harness("go_tagged_tests_test", "Test the build-tag test lister", f"{SCRIPTS}/go-tagged-tests.test.sh", [
        (f"{SCRIPTS}/go-tagged-tests.sh", "the script under test"),
        ("packages/envoy/cmd/listener/*_test.go",
         "go-tagged-tests.test.sh:204 runs the lister over this repository's own packages/envoy/cmd/listener "
         "and holds its answer to `^(TestServerChange)$`, and the lister reads every *_test.go of the "
         "directory it is given (go-tagged-tests.sh:97), so a second serverchange test, a rename of that "
         "function or a compound //go:build line there changes what this step prints"),
    ]),
    harness("ci_ok_test", "Test ci-ok's verdict", f"{SCRIPTS}/ci-ok.test.sh", [
        (f"{SCRIPTS}/ci-ok.sh", "the script under test"),
        (f"{SCRIPTS}/ci-ok.jq", "ci-ok.sh:144-197 runs its program with `jq -L \"$script_dir\"` and `include \"ci-ok\"`"),
        (f"{SCRIPTS}/runner-shutdown.sh",
         "ci-ok.sh:98 sources it, and the harness drives it with a stand-in gh: it is what reads "
         "a failure the platform caused off this run's check runs"),
        (f"{SCRIPTS}/gh-read.sh", "runner-shutdown.sh:103 sources it for its bounded gh reads"),
    ]),
    harness("ci_ok_report_test", "Test ci-ok's report and the check-run post (a stand-in gh)",
            f"{SCRIPTS}/ci-ok-report.test.sh", [
        (f"{SCRIPTS}/ci-ok-report.sh", "the script under test"),
        (f"{SCRIPTS}/post-check-run.sh", "ci-ok-report.sh:234 posts with it, and ci-ok-report.test.sh:292,308 runs it on its own"),
        (f"{SCRIPTS}/gh-read.sh", "ci-ok-report.sh:94 sources it for its bounded gh reads"),
        (f"{SCRIPTS}/runner-shutdown.sh",
         "ci-ok-report.sh:96 sources it, and no early ci-ok is posted for a failure it reads as "
         "a runner shutdown"),
        (f"{SCRIPTS}/ci-ok.sh", "ci-ok-report.sh:210 reaches its verdict with it"),
        (f"{SCRIPTS}/ci-ok.jq", "ci-ok.sh's program includes it"),
    ]),
    harness("ci_ok_reconcile_test", "Test the reconciliation of this head's other ci-ok check runs (a stand-in gh)",
            f"{SCRIPTS}/ci-ok-reconcile.test.sh", [
        (f"{SCRIPTS}/ci-ok-reconcile.sh", "the script under test"),
        (f"{SCRIPTS}/gh-read.sh", "ci-ok-reconcile.sh:80 sources it for its bounded gh reads"),
    ]),
    harness("find_pr_payload_test", "Test the pull request payload both pr-title checks read (a stand-in gh)",
            f"{SCRIPTS}/find-pr-payload.test.sh", [
        (f"{SCRIPTS}/find-pr-payload.sh", "the script under test"),
    ]),
    harness("merge_group_cancels_test", "Test the merge-group cancel decision", f"{SCRIPTS}/merge-group-cancels.test.sh", [
        (f"{SCRIPTS}/merge-group-cancels.sh", "the script under test"),
    ]),
    harness("main_bypass_check_test", "Test the main bypass check's decision", f"{SCRIPTS}/main-bypass-check.test.sh", [
        (f"{SCRIPTS}/main-bypass-check.sh", "the script under test"),
        (".github/workflows/main-bypass-check.yaml",
         "main-bypass-check.test.sh:198-210 holds every run: step of the workflow to a shell with pipefail, and "
         ":216-227 holds the decision's HEAD_SHA and HEAD_MESSAGE to the push payload's head"),
    ]),
    harness("dispatch_browser_latest_blobs_test", "Test the browser lane's blob attempt selection",
            f"{SCRIPTS}/dispatch-browser-latest-blobs.test.sh", [
        (f"{SCRIPTS}/dispatch-browser-latest-blobs.sh", "the script under test"),
    ]),
    harness("dispatch_browser_run_reads_test", "Test the browser lane's run reads",
            f"{SCRIPTS}/dispatch-browser-run-reads.test.sh", [
        (f"{SCRIPTS}/dispatch-browser-run-reads.sh", "the script under test"),
        (f"{SCRIPTS}/gh-read.sh",
         "dispatch-browser-run-reads.sh:79 sources it, and the harness drives its retry ladder with a "
         "stand-in gh that refuses a case's first calls"),
    ]),
    harness("dispatch_browser_samples_test", "Test the browser lane's memory and CPU sampler",
            f"{SCRIPTS}/dispatch-browser-samples.test.sh", [
        (f"{SCRIPTS}/dispatch-browser-samples.sh", "the script under test"),
    ]),
    harness("race_memory_test", "Test the race memory sampler", f"{SCRIPTS}/race_memory.test.sh", [
        (f"{SCRIPTS}/race_memory.py", "the module under test"),
    ]),
    harness("disk_watch_test", "Test the machine watch", f"{SCRIPTS}/disk_watch.test.sh", [
        (f"{SCRIPTS}/disk_watch.py", "the module under test"),
    ]),
    # The one harness of the job that is not under .github/scripts and sources no test-lib.sh.
    harness("sessions_import_pods_test", "Test the session import pods script (a stand-in kubectl)",
            "scripts/sessions-import-pods.test.sh", [
        ("scripts/sessions-import-pods.sh", "the script under test (scripts/sessions-import-pods.test.sh:113 runs it)"),
    ], lib=False),
    harness("check_ci_filters_test", "Test the CI filter check", f"{SCRIPTS}/check-ci-filters.test.sh", [
        (f"{SCRIPTS}/check-ci-filters.sh", "the script under test, symlinked into every fixture (check-ci-filters.test.sh:74)"),
        (f"{SCRIPTS}/ci_filter_coverage.py", "the engine the fixtures link in beside it"),
        (f"{SCRIPTS}/ci_workflow_rules.py", "the rules the fixtures link in beside it"),
        (f"{SCRIPTS}/workflow_paths.py", "the path model the fixtures link in beside it"),
        (f"{SCRIPTS}/ci_job_reads.py", "check-ci-filters.test.sh:1962 imports this table to derive the plain step's -run lists"),
        ("AGENTS.md", "check-ci-filters.test.sh:2315 holds the guide's slow-suite list to the jobs the full-ci label gates"),
    ]),
    harness("check_ts_filters_test", "Test the TypeScript filter check", f"{SCRIPTS}/check-ts-filters.test.sh", [
        (f"{SCRIPTS}/check-ts-filters.sh", "the script under test, symlinked into every fixture (check-ts-filters.test.sh:43)"),
        (f"{SCRIPTS}/ci_filter_coverage.py", "the engine it borrows the changes job's filters from, linked in beside it"),
        (f"{SCRIPTS}/workflow_paths.py", "the path model both import, linked in beside it"),
    ]),
    harness("go_test_step_expansion_test", "Test the go test step expansion",
            f"{SCRIPTS}/go-test-step-expansion.test.sh", [
        (f"{SCRIPTS}/go-test-step-expansion.py", "the module under test, loaded by path (go-test-step-expansion.test.sh:38)"),
        (f"{SCRIPTS}/ci_filter_coverage.py", "the module imports it for step_workdir and called_workflows"),
        (f"{SCRIPTS}/workflow_paths.py", "the path model that engine imports"),
        (".github/workflows/ci-job-envoy-go-race-docs.yaml", "its last case reads this file's own go test line, the wrapper one"),
        (".github/workflows/ci-job-envoy-go-docs-finalizers.yaml", "its last case reads this file's own go test line, the GODEBUG one"),
    ]),
]

# What every harness step above reads, for the test job's own gate: the job runs each of them, so
# its filter must select all of them too, or a change to a harness would select no job and the
# step's gate would be true in a job that never runs.
SCRIPT_TEST_READS = [read for gate_ in SCRIPT_TEST_GATES for read in gate_.reads]

# The guide-layout step's own reads, declared once: the step gate below is them, and the test
# job's gate carries them too, for the same reason the harness reads above are carried. A job's
# `if:` may test one changes output only (ci_filter_coverage refuses a second), so the step's
# gate decides nothing until the job's own filter selects the job for the same change.
GUIDE_LAYOUT_READS = reads(
    ("scripts/semantic-line-breaks.guides.test.ts", at("test", GUIDES_STEP)),
    ("scripts/semantic-line-breaks.ts", "scripts/semantic-line-breaks.guides.test.ts:9,16 spawns it"),
    ("AGENTS.md", "scripts/semantic-line-breaks.guides.test.ts:35 runs the checker over GUIDES (:12)"),
    ("scripts/e2e/README.md", "scripts/semantic-line-breaks.guides.test.ts:35 runs the checker over GUIDES (:12)"),
    ("docs/kubernetes.md", "scripts/semantic-line-breaks.guides.test.ts:35 runs the checker over GUIDES (:12)"),
    ("packages/envoy/AGENTS.md", "scripts/semantic-line-breaks.guides.test.ts:24-27 runs the checker over it"),
    ("packages/envoy/cmd/dispatch/README.md", "scripts/semantic-line-breaks.guides.test.ts:24-27 runs the checker over it"),
)

# docs-pages.yaml builds and publishes main's site in a workflow of its own (its header says why),
# so its on.push.paths stands in for the docs filter on main.
PUSH_WORKFLOWS = {"docs": ".github/workflows/docs-pages.yaml"}

GATES = [
    gate("typecheck", "typecheck", runs=[
        step_run("typecheck", "Vet the Go daemon, its e2e-tagged live proofs included", "go vet -tags e2e ./...", DAEMON, ["./..."],
                 tags="e2e", mode="vet"),
    ], reads=setup_go("typecheck", f"{DAEMON}/go.mod")),

    # The Go daemon's own suite, in a job of its own (.github/workflows/ci-job-daemon-go.yaml):
    # the plain step over every package of the module but the three concurrent ones and the text
    # checks, the race step over those three, and the Stage 1 proof, which builds ./cmd/legion
    # and boots it. The filter is daemon_go_tests, the list that already stood for exactly this
    # closure while it was aliased into the test filter and gated nothing of its own. The tests
    # the race step does not run are the plugin-pack job's (PLUGIN_PACK_TESTS), whose gate is
    # below: with them goes the Bun install, which no other test of this module needs, and with
    # that the lockfile and the workspace manifests, and with them skills/ and the prepack script
    # that stages each plugin's partition of it, which no other test of this module reads.
    # The plain step leaves two tests out as well, internal/supervise's table pins
    # (SUPERVISE_TABLE_TESTS), which the text-check job runs by name: they read that package's
    # source and the source of everything it imports, which their type-checker reads from source
    # too (table_test.go:99) and which of this repository is packages/daemon's own packages, so no
    # read leaves this job with them.
    gate("daemon_go_tests", "daemon-go", runs=[
        script_run("scripts/e2e/stage1-skeleton.sh", 'go build -o "$work/legion" ./cmd/legion',
                   at("daemon-go", "Stage 1 live proof"), DAEMON, ["./cmd/legion"], mode="build"),
    ], exact=[
        # check-ci-filters.sh reduces only a one-line go test, so the step's whole script is
        # pinned, as the envoy-go unit step's is - and it carries a -skip besides, which is no
        # form that check reduces either. What it compiles is every test file of every package
        # it lists, the two skipped table pins' included: -skip leaves a test out of the run,
        # never out of the build, so the closure here is the step's whole closure. The Run names
        # the packages the step's own `grep -Ev` keeps (packages_kept over DAEMON_PLAIN_PATTERN),
        # rather than the superset ./...: the race step's Run below claims the three packages
        # this step leaves out, which are this job's own, while the text checks are the
        # text-check job's, and a Run saying ./... would keep what only their tests read -
        # docs/kubernetes.md - on this job's filter. Only they read it: the way back's check in
        # internal/daemon, which the race step runs, reads upgrade_way_back_extract.txt beside it
        # instead, and internal/textcheck's way_back_doc_test.go holds that copy to the guide.
        pinned_step("daemon-go", DAEMON_PLAIN_STEP, DAEMON, DAEMON_PLAIN_SCRIPT, [
            (DAEMON, packages_kept(DAEMON, DAEMON_PLAIN_PATTERN),
             "go list ./... piped into go test, counted as go test over the packages its grep -Ev keeps, the "
             "supervise table's two pure pins skipped by name", {}),
        ]),
        # The race step carries a -skip, which is no form check-ci-filters.sh reduces either, so
        # its script is pinned too. What it compiles is every test file of the three packages,
        # the skipped tests' included: -skip leaves a test out of the run, never out of the
        # build, so the closure here is the step's whole closure and only those tests' own reads
        # (the plugin-pack gate below) leave this job.
        pinned_step("daemon-go", DAEMON_RACE_STEP, DAEMON, DAEMON_RACE_SCRIPT, [
            (DAEMON, DAEMON_RACE_PKGS, "go test -race over the three concurrent packages, the plugin-pack tests "
                                       "skipped by name", {"race": True}),
        ]),
    ], reads=reads(
        ("scripts/e2e/stage1-skeleton.sh", at("daemon-go", "Stage 1 live proof")),
        ("scripts/e2e/lib/rig.sh", "scripts/e2e/stage1-skeleton.sh:112 sources it"),
        ("scripts/e2e/lib/loopback-dsn.sh", "scripts/e2e/lib/rig.sh:93 sources it"),
        ("scripts/e2e/lib/port-file.sh", "scripts/e2e/lib/rig.sh:102 sources it"),
        ("scripts/e2e/lib/held-port.py", "scripts/e2e/lib/rig.sh:143,157 runs it to hold a port for the daemon"),
        ("scripts/e2e/lib/built-from.sh", "scripts/e2e/stage1-skeleton.sh:145 runs it over the checkout"),
        ("packages/pi-legion/package.json", "scripts/e2e/stage1-skeleton.sh:202 copies its name, version and legion "
                                            "block with jq into the pi-legion plugin the proof's home holds"),
        ("packages/pi-envoy/package.json", "scripts/e2e/stage1-skeleton.sh:203 copies its name and version with jq "
                                           "into the pi-envoy plugin the proof's home holds"),
        (".github/actions/install-uv/**", at("daemon-go", "./.github/actions/install-uv")),
        (".github/ci-tools.yaml", ".github/actions/install-uv/action.yml:25 installs the uv row's binary"),
        (".github/scripts/ci-tools.sh", ".github/actions/install-uv/action.yml:20 sources it, and ci_tools_fetch "
                                        "reads the uv row's two sources"),
        (".github/actions/pull-test-images/**", at("daemon-go", "Pull the NATS test container images")),
        (f"{DAEMON}/internal/testnats/nats.go", f"{at('daemon-go', 'Pull the NATS test container images')} const Image"),
        (f"{ENVOY}/internal/testnats/nats.go", f"{at('daemon-go', 'Pull the NATS test container images')} const HubImage"),
    ) + install_jj("daemon-go") + install_omp("daemon-go") + setup_go("daemon-go", f"{DAEMON}/go.mod")),

    # The plugin-pack job: the three tests of internal/daemon in PLUGIN_PACK_TESTS, which the
    # daemon-go race step skips by name - the one that packs the two plugin releases and loads
    # them, and the two prompt probes of agentmodels_omp_test.go, one of which stages the prompts
    # this checkout ships as the release stages them. The step runs those tests alone,
    # and compiles the package they are in to do it, so this filter is that package's whole test
    # closure as well as what the three read: the two plugin packages, what their prepack builds
    # each bundle from, skills/, and the lockfile the frozen install reads. The job exists for
    # those reads: with the tests here, a change to the lockfile or to one skill runs them
    # instead of the daemon's whole Go suite (dispatch://INFRA-1413).
    gate("plugin_pack", PLUGIN_PACK_JOB, exact=[
        pinned_step(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP, DAEMON, PLUGIN_PACK_SCRIPT, [
            (DAEMON, [f"./{PLUGIN_PACK_PACKAGE}/"], "go test -run over the package the three tests are in", {}),
        ]),
    ], reads=[
        Read(g, f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: internal/daemon/promptreferences_omp_test.go:132,207-228 "
                "packs packages/pi-envoy and packages/pi-legion with `bun pm pack`, which runs their prepack")
        for g in PACK_READS
    ] + reads(
        ("packages/envoy-client/scripts/build-dispatch-cli.sh", "scripts/pi-plugin-prepack.sh:96, run by the pack "
                                                                "in promptreferences_omp_test.go:228"),
        # The prompts this checkout ships, laid out for the agent-model probe as the release lays
        # them out: these were a RULES row of agentmodels_omp_test.go while the daemon-go race
        # step ran that test, which put skills/ on that job's filter and ran 1,083 s of Go tests
        # that could not see a skill (dispatch://INFRA-1413).
        ("packages/pi-legion/agents/**", f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: internal/daemon/"
                                         "agentmodels_omp_test.go:165 copyDir"),
        ("scripts/pi-plugin-prepack.sh", f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: internal/daemon/"
                                         "agentmodels_omp_test.go:175 --stage-skills"),
        ("skills/**", f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: internal/daemon/agentmodels_omp_test.go:175 "
                      "prepack stages skills/<name>"),
    ) + install_jj(PLUGIN_PACK_JOB) + install_omp(PLUGIN_PACK_JOB)
      + setup_go(PLUGIN_PACK_JOB, f"{DAEMON}/go.mod") + bun_job(PLUGIN_PACK_JOB)),

    # The private-name port harness, in a job of its own: one step, which builds the `legion`
    # binary of this head and holds the rules that binary carries to the frozen shell script's
    # verdicts, case for case. The Run is that build, so this filter is its whole closure; the
    # reads are the harness, the helpers it sources and the frozen script it compares against.
    # The comment on PRIVATE_NAMES_PORT_BUILD above says why the step is a job.
    gate("private_names_port", PRIVATE_NAMES_PORT_JOB, runs=[PRIVATE_NAMES_PORT_BUILD], reads=reads(
        (f"{SCRIPTS}/check-private-names-port.test.sh", at(PRIVATE_NAMES_PORT_JOB, PRIVATE_NAMES_PORT_STEP)),
        (TEST_LIB, "check-private-names-port.test.sh:63 sources it for check/is/summary"),
        (f"{SCRIPTS}/check-private-names.sh",
         "the frozen script every case of check-private-names-port.test.sh runs beside the "
         "engine, symlinked into each fixture (:203,411,457,546)"),
    ) + setup_go(PRIVATE_NAMES_PORT_JOB, f"{DAEMON}/go.mod")),

    # What is left of the test job once the daemon's Go suite and the private-name port harness
    # have jobs of their own: the Claude Code plugin build, the changelog fold, about thirty
    # script harnesses - each behind a gate of its own below (SCRIPT_TEST_GATES), whose reads
    # this gate carries too, since the job must be selected for the step to run in it - and the
    # Bun suites of scripts/. One go command is left, the listener build a Bun test starts.
    gate("test", "test", runs=[
        script_run("scripts/e2e/lib/held-port.test.ts", 'const build = Bun.spawnSync(["go", "build", "-o", listener, "./cmd/listener"], {',
                   at("test", "bun test scripts/e2e/lib"), ENVOY, ["./cmd/listener"], mode="build"),
    ], reads=CLAUDE_ENVOY_BUILD + SCRIPT_TEST_READS + GUIDE_LAYOUT_READS + reads(
        ("scripts/third-party-notices.test.ts", at("test", "Test third-party notices")),
        ("scripts/third-party-notices.ts", "scripts/third-party-notices.test.ts:5,122"),
        ("scripts/semantic-line-breaks.test.ts", at("test", "Test the semantic line-break checker")),
        ("scripts/semantic-line-breaks.ts", "scripts/semantic-line-breaks.test.ts:6 spawns it"),
        ("scripts/e2e/**", f"{at('test', 'bun test scripts/e2e/lib')} (lib/script-functions.test.ts:105-108 scans scripts/e2e/*.sh and lib/*); "
                           f"{at('test', 'Test the e2e proof selector')}: the selector reads scripts/e2e/*.sh"),
        (CONTRACTS_SRC, "scripts/e2e/lib/envoy-bridge.ts:9-10, session-settings-services.ts:6; scripts/dispatch/ask-census.ts:9 (tsc -p scripts/dispatch)"),
        (TEST_FIXTURES_SRC, f"{at('test', 'bun test scripts/e2e/lib')} and the other bun test steps import @legion/test-fixtures"),
        # The envoy-client sources, and broker-client's with them, are the shared read below:
        # scripts/e2e/lib/nats-stream.ts:16-18, operator-dispatch.ts:16-21, envoy-bridge.ts:11
        # and scripts/dispatch/ask-census.ts:10-15 import @legion/envoy-client/*, and the
        # scripts/dispatch typecheck holds seven broker-client sources in its own program.
        ("packages/envoy-client/test/fake-dispatch.ts", "scripts/e2e/lib/agent-replay.test.ts:4-7 imports it by path"),
        ("scripts/dispatch/**", f"{at('test', 'Lint the Dispatch census script')}, typecheck and test"),
        ("scripts/github-apps/**", f"{at('test', 'Lint the GitHub App registration script')} and typecheck"),
        ("scripts/sessions/**", f"{at('test', 'Lint the session message-wait script')} and typecheck; the suite "
                                f"itself is {SESSIONS_JOB}'s, the one step of this job that needed a database"),
        ("scripts/approval-eval/**", f"{at('test', 'Lint the approval-eval statistics')}, typecheck and test"),
        ("skills/approval-review/references/never-decide.md",
         f"{at('test', 'Test the approval-eval statistics')}: scripts/approval-eval/never-decide.ts:75-77 reads "
         "it, through refusals.ts:720, which __tests__/refusals.test.ts:181-199 runs"),
        (f"{FIX}/daemon-api/**", "scripts/e2e/lib/stage4b-boot-capabilities.test.ts:17"),
        (f"{DAEMON}/internal/daemon/controller.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:344-345 reads it as text"),
        (f"{DAEMON}/internal/controller/liveness.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:347"),
        (f"{DAEMON}/internal/supervise/machine.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:350"),
        (f"{DAEMON}/internal/intake/consume.go", "scripts/e2e/lib/stage4b-consumers.test.ts:23-26"),
        (f"{DAEMON}/internal/capabilities/testdata/web-search-open-planner.json", "scripts/e2e/lib/stage4b-capabilities.test.ts:13-22"),
        (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root.json", "scripts/e2e/lib/stage4b-pods.test.ts:22-35"),
        (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root-operator-route.json", "scripts/e2e/lib/stage4b-pods.test.ts:179-187"),
        ("packages/pi-shared/src/**", "packages/claude-envoy's build bundles it; scripts/sessions and scripts/dispatch typecheck against it"),
        (".github/actions/install-changie/**", at("test", "./.github/actions/install-changie")),
        (".github/ci-tools.yaml", ".github/actions/install-changie/action.yml:19-20 installs the changie row's tarball"),
        (".github/scripts/ci-tools.sh", ".github/actions/install-changie/action.yml:18 sources it, and ci_tools_fetch "
                                        "reads the changie row's two sources"),
        (".github/changelog-packages", f"{at('test', FOLD_CHECK_STEP)}: the step reads it as the package list"),
        ("packages/*/.changie.yaml", f"{at('test', FOLD_CHECK_STEP)}: fold-check renders each listed package's fragments with that package's own changie configuration"),
        ("packages/*/.changes/unreleased/**", f"{at('test', FOLD_CHECK_STEP)}: fold-check renders every fragment a listed package holds"),
    ) + envoy_client_dep("scripts/e2e/lib/nats-stream.ts:16-18, operator-dispatch.ts:16-21, envoy-bridge.ts:11; "
                         "scripts/dispatch/ask-census.ts:10-15 (tsc -p scripts/dispatch)")
      + setup_go("test", f"{DAEMON}/go.mod")
      + biome(at("test", "Lint the Dispatch census script")) + bun_job("test")),

    # The session message-wait suite, in a job of its own: it is the one test of the whole test
    # job that needs a database (scripts/sessions/__tests__/message-wait.test.ts:20-28 refuses
    # without LEGION_TEST_PG_DSN, since a store it cannot measure is a measurement of nothing),
    # and a Postgres service on the test job would start one for every change that selects any
    # of its thirty-two harness steps. The test reads the script it spawns and nothing else: both
    # are under scripts/sessions, and both import node: builtins and bun alone, so the job
    # installs no dependencies and this filter names no lockfile.
    gate("sessions_script", SESSIONS_JOB, reads=reads(
        ("scripts/sessions/**", f"{at(SESSIONS_JOB, SESSIONS_STEP)}: the suite, and message-wait.ts, which "
                                "scripts/sessions/__tests__/message-wait.test.ts:219 spawns (SCRIPT :18)"),
    ) + setup_bun(SESSIONS_JOB)),

    # The test job's guide-layout step, gated on a filter of its own: the test spawns the
    # checker over five guides, and no other step of the job reads one of them. The job's own
    # filter names them too (GUIDE_LAYOUT_READS above, in the test gate's reads): a job's `if:`
    # may test one changes output only, so without them a guide-only change selected no job and
    # this step - the one check that change is about - ran nowhere.
    gate("guide_layout", "test", step=GUIDES_STEP, reads=GUIDE_LAYOUT_READS),


    # The job ci.yaml gates is docs, and the steps it runs are docs-site.yaml's (at_docs), under
    # a job id of its own, `build`. So this gate keeps `workflow=CI`, the file that holds a job
    # named docs, where every other gate takes the called file job_file() names.
    gate("docs", "docs", runs=[
        script_run(BUILD_BINARIES, 'go -C "$root/$legion_module" build -o "$bin/legion" ./cmd/legion',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", DAEMON, ["./cmd/legion"], mode="build"),
        script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/envoy-dispatch" ./cmd/dispatch',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY, ["./cmd/dispatch"], mode="build"),
        script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/envoy-broker" ./cmd/broker',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY, ["./cmd/broker"], mode="build"),
        script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/" ./cmd/agent-secrets ./cmd/agent-secrets-helper ./cmd/broker-refgen',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY,
                   ["./cmd/agent-secrets", "./cmd/agent-secrets-helper", "./cmd/broker-refgen"], mode="build"),
        script_run(RUN_SERVER, DISPATCH_SERVER_BUILD, f"started by packages/dispatch/e2e/harness-listeners.ts's dispatchServer (harness-start.ts, which "
                   f"docs/site/media/harness.ts:21,98 imports), run by {at_docs('Take the screenshots')}",
                   ENVOY, ["./cmd/dispatch"], mode="build"),
    ], reads=reads(
        ("docs/site/**", f"{at_docs('Typecheck the docs site')}, screenshots (docs/site/media/shots.ts) and build (generate.ts + astro build) in docs/site"),
        ("packages/dispatch/**", f"{at_docs('Build the Dispatch dashboard the screenshots show')}; docs/site/media/harness.ts:21,98 boots packages/dispatch/e2e/harness-start.ts"),
        ("packages/proof-editor/src/**", "packages/dispatch/web/vite.config.ts:127-147 aliases (build:web)"),
        (CONTRACTS_SRC, "packages/dispatch/web/vite.config.ts:117-126; docs/site/generators/dispatch-tools.ts:7"),
        # This gate is the one reader of envoy-client's sources that does not compile
        # broker-client with them: every generator entry resolves to src/dispatch-command.ts,
        # whose program holds no broker-client source (`bunx tsc --listFilesOnly`, 0 of 7), so
        # the shared envoy_client_dep read would declare a read this job does not make.
        (ENVOY_CLIENT_SRC, "docs/site/generators/dispatch-tools.ts:12 imports ../../../packages/envoy-client/src/dispatch-command.ts"),
        ("skills/**", "docs/site/generators/legion-skills.ts:15-19 reads skills/*/SKILL.md"),
        ("deploy/kubernetes/**", "docs/site/generators/legion-config.sh:28-32 renders every tracked deploy/kubernetes *.example/*.yml/*.yaml"),
        ("scripts/third-party-notices.ts", "docs/site/astro.config.mjs:15; packages/dispatch/web/vite.config.ts:12"),
        (".github/scripts/check-private-names.sh", at_docs("Check the built site has no private name, internal host, or tailnet host")),
        (".github/scripts/repo-paths.sh", "docs/site/repo-paths.ts:21,25 runs it for the site's own URL, base and edit links "
                                          f"(astro.config.mjs), read by {at_docs('Build the docs site')}"),
        (f"{ENVOY}/internal/dispatch/api/routes_table.go", "docs/site/generators/dispatch-api.ts:112 sources, checked by generators/lib/markdown.ts:46-49"),
        (f"{ENVOY}/cmd/dispatch/settings.go", "docs/site/generators/dispatch-config.ts:65 sources, checked by generators/lib/markdown.ts:46-49"),
        (BUILD_BINARIES, f"docs/site/scripts/generate.ts:116 runs it ({at_docs('Build the docs site')})"),
        (RUN_SERVER, f"packages/dispatch/e2e/harness-listeners.ts's dispatchServer runs it ({at_docs('Take the screenshots')})"),
        ("scripts/e2e/lib/held-port.py", "packages/dispatch/e2e/run-server.sh:80,89 holds the server's port through it"),
        # broker-refgen (docs/site/generators/broker-reference.sh:14) parses these at run time.
        (f"{ENVOY}/internal/broker/api/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/routes.go:226)"),
        (f"{ENVOY}/internal/broker/config/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/config.go:11,36)"),
        (f"{ENVOY}/cmd/broker/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/config.go:12,40)"),
        (f"{ENVOY}/internal/broker/helper/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/pages.go:16, codes.go:15)"),
        (f"{ENVOY}/cmd/agent-secrets/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/pages.go:17, codes.go:68)"),
        (f"{ENVOY}/internal/broker/approval/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/routes.go:668 g.in of the pageAnswerers directory named at :34, which parses it at bodies.go:26)"),
    ) + setup_go(DOCS_JOB, f"{DAEMON}/go.mod", DOCS_SITE)
      + bun_job(DOCS_JOB, DOCS_SITE) + CI_SELF + DOCS_SITE_SELF, workflow=CI),

    gate("contracts", "contracts", reads=reads(
        ("packages/contracts/**", f"{at('contracts', 'Typecheck contracts')}, test (src/, scripts/gen-go.test.ts) and build in packages/contracts"),
        ("skills/dispatch/SKILL.md", "packages/contracts/src/dispatch-tools.test.ts's dispatchSkillHeadings reads skills/dispatch/SKILL.md headings"),
        ("skills/dispatch-writing-specs/SKILL.md", "packages/contracts/src/dispatch-tools.test.ts:1283 existsSync(skills/dispatch-writing-specs/SKILL.md)"),
        (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-href-references.json", "packages/contracts/src/dispatch-href.test.ts:51-55"),
        (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-text-references.json", "packages/contracts/src/dispatch-text-references.test.ts:10-14"),
        (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-block-id-encodings.json", "packages/contracts/src/dispatch-block-ref.test.ts:98-102 reads the Go side's own table by relative path and asserts set equality against it, so the two halves are one corpus"),
        (f"{DAEMON}/internal/intake/testdata/dispatch/server-started.json", "packages/contracts/src/dispatch-api.test.ts:363 (the new URL at :359-362)"),
        (f"{ENVOY}/internal/dispatch/pmdoc/testdata/block-pointer-values.json", "packages/contracts/src/dispatch-block-ref.test.ts:129-133 reads pmdoc's own JSON copy of the block pointer table by relative path"),
        (f"{ENVOY}/internal/dispatch/pmdoc/testdata/typed-block-id-spellings.json", "packages/contracts/src/dispatch-block-ref.test.ts:146-150 reads pmdoc's own JSON copy of the typed block id spellings by relative path"),
        (f"{ENVOY}/internal/contracts/generated.go", "packages/contracts/scripts/gen-go.test.ts:62 reads every file of generated (:15, this one :7) and gen-go.ts's outputs list (--check compares the committed file)"),
        (f"{ENVOY}/internal/dispatch/pmdoc/schema_generated.go", "packages/contracts/scripts/gen-go.test.ts:62 reads every file of generated (:15, this one :8-10) and gen-go.ts's outputs list (the block schema's shape, generated from schemas/block-schema.schema.json)"),
        (TEST_FIXTURES_SRC, "packages/contracts/src/*.test.ts import @legion/test-fixtures"),
    ) + bun_job("contracts")),

    gate("test_fixtures", "test-fixtures", reads=reads(
        ("packages/test-fixtures/**", f"{at('test-fixtures', 'Typecheck the shared test fixtures')} and test in "
                                      "packages/test-fixtures (its own biome.json among them); its modules import only "
                                      "one another, node: and bun:"),
    ) + bun_job("test-fixtures")),

    gate("conflicts_script", "conflicts-script", reads=reads(
        ("scripts/conflicts/**", f"{at('conflicts-script', 'Lint the conflict measurement script')}, typecheck (tsc -p scripts/conflicts) "
                                 "and test (__tests__/ and __fixtures__/); its modules import only one another, node: and bun:"),
    ) + biome(at("conflicts-script", "Lint the conflict measurement script")) + bun_job("conflicts-script")),

    gate("broker_client", "broker-client", reads=reads(
        ("packages/broker-client/**", f"{at('broker-client', 'Typecheck the broker client')} and test in "
                                      "packages/broker-client; its modules import one another, node:, bun:, the "
                                      "third-party packages its manifest names and @legion/contracts"),
        (CONTRACTS_SRC, "packages/broker-client/src/token-client.ts imports @legion/contracts/trailing-slash"),
        (TEST_FIXTURES_SRC, "packages/broker-client/src/__tests__ import @legion/test-fixtures"),
    ) + bun_job("broker-client")),

    gate("envoy_client", "envoy-client", reads=reads(
        ("packages/envoy-client/**", f"{at('envoy-client', 'Typecheck Envoy client')}, test and build in packages/envoy-client"),
        ("packages/broker-client/**", f"{at('envoy-client', 'Typecheck broker client')} and {at('envoy-client', 'Test broker client')}; packages/envoy-client/src/broker-credential.ts:21-22 imports @legion/broker-client/environment and @legion/broker-client/token-client"),
        (CONTRACTS_SRC, f"{at('envoy-client', 'Build contracts for Envoy client')}; tsconfig paths @legion/contracts -> ../contracts/src/index.ts"),
        (f"{FIX}/dispatch-targeted-delivery.json", "packages/envoy-client/src/__tests__/delivery.test.ts:17-18, dispatch-text.test.ts:128-129"),
        (f"{FIX}/dispatch-public-url.json", "packages/envoy-client/src/__tests__/dispatch-config.test.ts:5"),
        (f"{FIX}/go-unicode-isspace.json", "packages/envoy-client/src/__tests__/dispatch-execute.test.ts:27"),
        (f"{ENVOY}/internal/dispatch/api/testdata/**", "packages/envoy-client/src/__tests__/askgate-new-asks.test.ts:14-21 reads the Dispatch server's table of decision-block inserts, which packages/envoy/internal/dispatch/api's TestDocumentEditAdviceCountsTheAskBlocksTheEditAdded reads too"),
        ("skills/**", "packages/envoy-client/src/__tests__/dispatch-advice.test.ts:1288 reads skills/<skill>/SKILL.md for every skill a notice cites"),
        ("packages/*/AGENTS.md", "packages/envoy-client/src/__tests__/dispatch-asked-of.test.ts:374-382 reads every package's AGENTS.md, and its ALLOWED names lines of four of them, so rewording one of those lines fails that test"),
        (TEST_FIXTURES_SRC, "packages/envoy-client/src/__tests__ import @legion/test-fixtures"),
        (f"{ENVOY}/internal/brokerclient/testdata/wire.json", "packages/broker-client/src/__tests__/wire.test.ts:82 reads the Go package's own corpus by relative path (CORPUS_PATH :27-29) - that file itself, never a copy, so the two clients are held to one wire"),
        (f"{ENVOY}/internal/bus/testdata/private-stream-subjects.json", "packages/envoy-client/src/__tests__/stream-topics.test.ts:131 reads the listener's own cases by relative path (SHARED_CASES :118-121), which packages/envoy/internal/bus's TestReachesPrivateStreamSubjectBoundsInterests reads too"),
    ) + bun_job("envoy-client")),

    gate("pi_shared", "pi-shared", reads=reads(
        ("packages/pi-shared/**", f"{at('pi-shared', 'Typecheck pi-shared')} and test in packages/pi-shared"),
        # envoy-client's sources, and broker-client's with them, are the shared read below.
        ("packages/envoy-client/test/fake-dispatch.ts", "packages/pi-shared/src/handoff-record.test.ts:7 imports @legion/envoy-client/test/fake-dispatch"),
        (CONTRACTS_SRC, "pi-shared and envoy-client import @legion/contracts (contracts exports.bun -> src)"),
        (TEST_FIXTURES_SRC, "packages/pi-shared's src and test suites import @legion/test-fixtures"),
        (f"{FIX}/daemon-api/**", "packages/pi-shared/src/daemon-client.test.ts:18, legion-claim-core.test.ts:11, legion-phase-ops.test.ts:21"),
        ("skills/**", "packages/pi-shared/test/skills-partition.test.ts:22,35-36,53-54 reads every skills/ directory and stages both partitions"),
        ("scripts/pi-plugin-prepack.sh", "packages/pi-shared/test/skills-guard.ts:17,34 runs scripts/pi-plugin-prepack.sh --stage-skills"),
        (f"{DAEMON}/internal/prompts/**", "packages/pi-shared/test/skills-partition.test.ts:70-76 reads every .md under packages/daemon/internal/prompts"),
        (".github/workflows/release.yaml", "packages/pi-shared/test/skills-partition.test.ts:95"),
    ) + envoy_client_dep("packages/pi-shared/tsconfig.json paths @legion/envoy-client/errors; src imports "
                         "@legion/envoy-client/* (exports.bun -> src)")
      + bun_job("pi-shared")),

    gate("pi_envoy", "pi-envoy", reads=reads(
        ("packages/pi-envoy/**", f"{at('pi-envoy', 'Typecheck pi-envoy')}, test and pack in packages/pi-envoy"),
        ("packages/pi-shared/src/**", "packages/pi-envoy/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts; the bundle inlines it"),
        ("packages/pi-shared/test/**", "packages/pi-envoy/extensions/dispatch-first-omp.test.ts:13, src/skills-guard.test.ts:23, scripts/prepack.test.ts:6"),
        (TEST_FIXTURES_SRC, "packages/pi-envoy's extensions and src tests import @legion/test-fixtures"),
        # envoy-client's sources, and broker-client's with them, are the shared read below.
        ("packages/envoy-client/test/envoy-stack.ts", "packages/pi-envoy/extensions/envoy.test.ts:45-50 imports @legion/envoy-client/test/envoy-stack"),
        ("packages/envoy-client/test/fake-dispatch.ts", "packages/pi-envoy/extensions/askgate/decision-record.test.ts:9 imports @legion/envoy-client/test/fake-dispatch"),
        ("packages/envoy-client/bin/dispatch.ts", "packages/pi-envoy/package.json:26 build and scripts/pi-plugin-prepack.sh:96 run build-dispatch-cli.sh, entry bin/dispatch.ts"),
        ("packages/envoy-client/scripts/build-dispatch-cli.sh", "packages/pi-envoy/package.json:26; scripts/pi-plugin-prepack.sh:96"),
        (CONTRACTS_SRC, f"{at('pi-envoy', 'Build contracts for pi-envoy')}; tsconfig paths @legion/contracts"),
        (f"{FIX}/dispatch-targeted-delivery.json", "packages/pi-envoy/extensions/envoy.test.ts:159"),
        ("skills/**", "packages/pi-envoy/src/skills-guard.test.ts:43 stages them (the roots at :29-32); scripts/pi-plugin-prepack.sh:103 stages skills; extensions/envoy.ts's resolveSkillsDirectory"),
        ("scripts/pi-plugin-prepack.sh", f"packages/pi-envoy/package.json:27 prepack ({at('pi-envoy', 'Pack pi-envoy')}); packages/pi-envoy/scripts/prepack.test.ts:84"),
        ("scripts/third-party-notices.ts", "scripts/pi-plugin-prepack.sh:97,100 write dist/THIRD_PARTY_NOTICES with it"),
        ("scripts/e2e/lib/pack-plugin.sh", "packages/pi-envoy/scripts/prepack.test.ts:119-122"),
        (f"{DAEMON}/internal/prompts/**", "packages/pi-envoy/src/skills-guard.test.ts:123 reads every .md of skillAndPromptRoots (:30-33)"),
    ) + envoy_client_dep("packages/pi-envoy/tsconfig.json paths @legion/envoy-client/*; extensions/envoy.ts:19-101 imports")
      + install_jj("pi-envoy") + install_omp("pi-envoy") + bun_job("pi-envoy")),

    gate("pi_legion", "pi-legion", runs=[
        script_run("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts",
                   'const result = Bun.spawnSync(["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args], {',
                   f"daemon-pane.test.ts, run by {at('pi-legion', 'Test pi-legion')}", DAEMON, ["./cmd/daemon-pane"], mode="build",
                   overlay={f"{DAEMON}/cmd/daemon-pane/main.go": "packages/pi-legion/scripts/skill-scenarios/daemon-pane.go"}),
    ], reads=reads(
        ("packages/pi-legion/**", f"{at('pi-legion', 'Typecheck pi-legion')}, test and pack in packages/pi-legion"),
        ("packages/pi-envoy/**", "packages/pi-legion/extensions/legion.test.ts:68,306 and legion-role-claim.test.ts:13,98 import ../../pi-envoy/extensions/envoy; packages/pi-shared/test/omp-harness.ts:811"),
        ("packages/claude-envoy/**", "packages/pi-legion/src/skills-guard.test.ts:276 spawns packages/claude-envoy/scripts/build.ts (PLUGIN_BUILDER :24) and asserts an exact file list over the tree it writes, so that script and everything it reads - its BUNDLE_ENTRYPOINTS at scripts/build.ts:39-44 among them - are read by this job through a spawn, which carries no import and which no derivation sees"),
        ("packages/envoy-client/bin/dispatch.ts", "the fourth entry of that builder's BUNDLE_ENTRYPOINTS (packages/claude-envoy/scripts/build.ts:43), outside the package it builds"),
        ("packages/pi-shared/src/**", "packages/pi-legion/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts"),
        ("packages/pi-shared/test/**", "packages/pi-legion/extensions/legion-phase-stall-omp.test.ts:13, legion-role-tools-omp.test.ts:12, src/skills-guard.test.ts:15, scripts/prepack.test.ts:4"),
        ("packages/envoy-client/test/subscribe-answer.ts", "packages/pi-legion/extensions/legion-role-claim.test.ts:46 "
                                                           "imports @legion/envoy-client/test/subscribe-answer, and "
                                                           "packages/pi-shared/test/omp-harness.ts:17, which this "
                                                           "package's omp tests drive, imports it too"),
        # envoy-client's sources, and broker-client's with them, are the shared read below.
        (CONTRACTS_SRC, f"{at('pi-legion', 'Build contracts for pi-legion')}; tsconfig paths @legion/contracts"),
        (TEST_FIXTURES_SRC, "packages/pi-legion's extensions and src tests import @legion/test-fixtures"),
        (f"{FIX}/daemon-api/**", "packages/pi-legion/extensions/legion.test.ts:1380-1381, legion-role-claim.test.ts:20, src/daemon-api-version.test.ts:14"),
        (f"{FIX}/capability-redaction.json", "packages/pi-legion/src/capability-report.test.ts:848"),
        (f"{FIX}/capability-redaction-plugin-report.json", "packages/pi-legion/src/capability-report.test.ts:968 (PLUGIN_REPORT :854)"),
        ("packages/dispatch/e2e/**", "packages/pi-legion/scripts/skill-scenarios/seed.ts:111 imports packages/dispatch/e2e/api.ts (typecheck includes scripts/skill-scenarios/*.ts), and that program holds six of that directory's modules: api.ts, harness-ports.ts, harness-broker.ts, harness-processes.ts, harness-real-broker.ts and harness-signin.ts"),
        ("packages/dispatch/web/src/api/types.ts", "packages/dispatch/e2e/api.ts:15"),
        ("packages/dispatch/web/src/api/paths.ts", "packages/dispatch/e2e/api.ts:14 imports PathsOf, Op, PathParams, QueryParams, RequestBody, Query, Body and Answer from it, since that client is keyed on the generated document's path patterns"),
        ("skills/**", "packages/pi-legion/src/skills-guard.test.ts:39 stages them and :166 reads the roots at :152-159; scripts/skill-scenarios/rig.test.ts:954,1170; prepack stages skills"),
        ("scripts/pi-plugin-prepack.sh", f"packages/pi-legion/package.json prepack ({at('pi-legion', 'Pack pi-legion')}); packages/pi-shared/test/skills-guard.ts:17,34"),
        ("scripts/third-party-notices.ts", "scripts/pi-plugin-prepack.sh:97,100"),
        (f"{DAEMON}/internal/**", "packages/pi-legion/src/skills-guard.test.ts:166 sweeps every .md/.go/.ts/.yaml/.yml/.example/.sh under packages/daemon/internal, a root of the list at :155"),
        ("docs/kubernetes.md", "packages/pi-legion/src/skills-guard.test.ts:166, a root of the list at :154"),
        ("docs/site/src/content/docs/**", "packages/pi-legion/src/skills-guard.test.ts:166, a root of the list at :156"),
        ("deploy/kubernetes/daemon/controller.yaml.example", "packages/pi-legion/src/skills-guard.test.ts:166, a root of the list at :157"),
        ("AGENTS.md", "packages/pi-legion/src/skills-guard.test.ts:166, a root of the list at :158"),
        ("README.md", "packages/pi-legion/src/skills-guard.test.ts:166, a root of the list at :159"),
        ("scripts/e2e/lib/**", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:6 imports scripts/e2e/lib/script-functions; rig.sh:275 sources scripts/e2e/lib/rig.sh"),
        (".omp-pin", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:1168-1170 copies .omp-pin"),
        ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts", "writes the overlay and runs go run (:29-30)"),
    ) + envoy_client_dep("packages/pi-legion/tsconfig.json paths @legion/envoy-client/*; scripts/skill-scenarios/*.ts "
                         "import @legion/envoy-client/*")
      + install_jj("pi-legion") + install_omp("pi-legion") + setup_go("pi-legion", f"{DAEMON}/go.mod")
      + bun_job("pi-legion")),

    # The dashboard's and the proof editor's fast checks (ci.yaml's comment on dispatch-ui says
    # what they are). No go command runs here.
    gate("dispatch_ui", "dispatch-ui", reads=reads(
        ("packages/dispatch/**", f"{at('dispatch-ui', 'Typecheck dispatch dashboard')}, test, build, check:classes and check:asset-sizes in packages/dispatch"),
        ("packages/proof-editor/**", f"{at('dispatch-ui', 'Typecheck proof editor')} and test; packages/dispatch/web/vite.config.ts:127-147"),
        # packages/contracts/src is no read of this table's: which of its modules this job compiles
        # is what its two tsc programs list, so check-ts-filters.sh writes them into the filter's
        # listed block and holds the block to them (ci_ts_programs.py, LISTED), and rule 9's second
        # direction passes over that block's patterns.
        (f"{FIX}/live/**", "packages/dispatch/web/src/features/delivery/lib/applyDelta.test.ts:2-4, e2e/__tests__/delivery-live-probe.test.ts:12-16"),
        (f"{FIX}/decision-blocks/**", "packages/dispatch/web/src/features/doc/decision-blocks.test.ts:9-10, features/inbox/AskCard.test.tsx:455-456"),
        (f"{ENVOY}/internal/dispatch/pmdoc/schema/**", "packages/dispatch/web/src/__tests__/setup.ts:10 and features/refs/markdown-engine.test.ts:3 read blocks.json; packages/proof-editor/tests/ask-concurrent-edit.test.ts:272-275 reads block-node-types.json of the same directory"),
        (f"{ENVOY}/internal/dispatch/pmdoc/testdata/fixtures.json", "read at packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts:48, whose path is the new URL at :44, by the three suites this job runs over the one table: features/refs/fixture-table-readings.test.ts, markdown-engine.test.ts and markdown-surfaces.test.tsx"),
        (f"{ENVOY}/internal/dispatch/pmdoc/heavybodies/testdata/shared-bodies.json", "packages/dispatch/e2e/__tests__/heavy-bodies-digests.test.ts:44-50, which this job runs: the digest table holding e2e/heavy-bodies.ts's bodies to the Go shapes that spell the same bytes"),
        (f"{ENVOY}/scripts/fixtures/shell-quote.json", "packages/dispatch/e2e/__tests__/local-instance-support.test.ts:162-163, the new URL at :163"),
        ("packages/contracts/schemas/dispatch-openapi.json",
         "packages/dispatch/e2e/__tests__/stub-recordings.test.ts:37-40, the new URL at :38: the generated API "
         "document every committed recording's body is held to, which this job's test step runs"),
        (f"{ENVOY}/going/testdata/**", "packages/proof-editor/tests/going-answer-no-link.test.ts:128-129"),
        ("scripts/third-party-notices.ts", "packages/dispatch/web/vite.config.ts:12 (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
        ("packages/envoy-client/test/fake-dispatch.ts", f"packages/dispatch/e2e/__tests__/delivery-live-probe.test.ts:6-9 and local-instance-support.test.ts:3-6 import @legion/envoy-client/test/fake-dispatch, and {at('dispatch-ui', 'Test dispatch dashboard')} runs both"),
        (ENVOY_CLIENT_SRC, "packages/envoy-client/test/fake-dispatch.ts:30 imports ../src/dispatch-http, so the two suites "
                           "above put envoy-client's sources in this job's program too (check-ts-filters.sh measures it); "
                           "@legion/broker-client does not come with them, since no source of this program reaches "
                           "packages/envoy-client/src/broker-credential.ts"),
        (TEST_FIXTURES_SRC, f"packages/dispatch's web and e2e/__tests__ suites import @legion/test-fixtures, and {at('dispatch-ui', 'Test dispatch dashboard')} runs them; packages/proof-editor/tests/build-upstream-tarball.test.ts imports its temporary-directories fixture, and {at('dispatch-ui', 'Test proof editor')} runs it"),
    ) + bun_job("dispatch-ui")),

    # The browser suite, every row of it: the binaries job, the shard legs, the three `-alone`
    # projects, the two-process job, the visual job and the merge job, all in
    # dispatch-browser.yaml, which ci.yaml's dispatch-browser job and the nightly both call
    # (at_browser cites their steps).
    #
    # ci_filter_coverage.py's check_table reads a gate whose ci.yaml job calls a local workflow
    # over the jobs of that workflow, so dispatch-browser.yaml's go commands, `env:` blocks and
    # `shell:` keys are held here exactly as a job's own steps are: the binaries job's build
    # step and the multiprocess job's are the exact entries below, and a name outside
    # ENV_ALLOWED, or a `shell:`, in either job or in the file around it drops that step out of
    # the check's reduction, which then counts it as reading every Go file of the repository.
    # The gate keeps `workflow=CI`, as the docs gate does and for the same reason:
    # dispatch-browser.yaml carries job ids of its own (binaries, shard, alone, multiprocess,
    # merge) rather than one named for the caller, so ci.yaml is the file that holds a job named
    # dispatch-browser, and each citation names the browser job it is in.
    #
    # This gate lists no script Run, and cannot: a Run pinned to a script's text is reduced only
    # where nothing around the ci.yaml job can move what the script compiles, and around_job
    # reads that over every job of the called workflow. The shard and alone jobs run in a
    # container (dispatch://INFRA-1443), which shell_reason counts as exactly such a thing, so a
    # script Run here would fall to reading every Go file of the repository. It costs this
    # filter nothing. Those two jobs run no go command at all - run-server.sh and run-broker.sh
    # execute the binaries the first entry below builds - and the scripts the multiprocess job
    # and the visual job still start compile closures this filter already carries:
    # run-server.sh, which both of them start, builds ./cmd/dispatch, which the multiprocess
    # job's own pinned build step below names, and run-signin-issuer.sh builds
    # ./cmd/dev-oidc-issuer, which that first entry names.
    gate("dispatch_browser", BROWSER_JOB, exact=[
        # The four commands the shard and alone jobs' scripts built before those jobs moved
        # into a container, built once in the binaries job and downloaded by both: this is the
        # closure that build compiles and no other. The step names an output (-o) and pins
        # CGO_ENABLED on the line, neither of which the simple form reduces, so the step is
        # pinned whole; the Run carries that CGO_ENABLED, since the closure go lists without
        # cgo is the one this build compiles.
        pinned_step(BROWSER_BINARIES, BROWSER_BINARIES_STEP, ENVOY, BROWSER_BINARIES_SCRIPT, [
            (ENVOY, ["./cmd/dispatch"] + BROKER_PKGS,
             "CGO_ENABLED=0 go build of the Dispatch server, the real broker, its dev OIDC issuer and the seeding step",
             {"mode": "build", "env": {"CGO_ENABLED": "0"}}),
        ], workflow=BROWSER_SITE),
        # The multiprocess job builds the server once, so its two processes find it current and
        # start in seconds rather than waiting out run-server.sh's own build in each boot wait.
        # The line has an `-o`, which is no form this check reduces, so the entry pins the whole
        # script and names the one package it compiles - which is the closure run-server.sh
        # compiles in that same job.
        pinned_step(BROWSER_MP, BROWSER_MP_BUILD, ENVOY, "go build -o ./dispatch-e2e ./cmd/dispatch", [
            (ENVOY, ["./cmd/dispatch"], "go build -o ./dispatch-e2e ./cmd/dispatch", {"mode": "build"}),
        ], workflow=BROWSER_SITE),
        # The embedder job builds it once for the same reason, and names its step for its own
        # lane because this check looks a step up by name across the workflow.
        pinned_step(BROWSER_EMBEDDER, BROWSER_EMBEDDER_BUILD, ENVOY, "go build -o ./dispatch-e2e ./cmd/dispatch", [
            (ENVOY, ["./cmd/dispatch"], "go build -o ./dispatch-e2e ./cmd/dispatch", {"mode": "build"}),
        ], workflow=BROWSER_SITE),
    ], reads=reads(
        ("packages/dispatch/**", f"{BROWSER_SUITE_STEPS} run the suite, "
         f"{at_browser('Build dispatch dashboard')} builds the SPA the rows read, and "
         f"{at_browser(BROWSER_MERGE_STEP, BROWSER_MERGE)} merges their reports, in packages/dispatch"),
        ("packages/proof-editor/**", "packages/dispatch/web/vite.config.ts:127-147 (build:web)"),
        # packages/contracts/src is no read of this table's either: the filter's listed block holds
        # the modules dispatch_ui's tsc programs compile, which check-ts-filters.sh writes, and
        # ci_ts_programs.py's LISTED says why this suite loads that set and no other, and each
        # job's `Build contracts for dispatch e2e` step runs `build:dispatch`, whose four entries
        # compile nothing outside it.
        ("packages/envoy-client/bin/dispatch.ts", "packages/dispatch/e2e/inbox-cli.e2e.ts:326 runs it through e2e/dispatch-cli.ts:8"),
        (f"{ENVOY}/internal/dispatch/pmdoc/schema/blocks.json", "packages/dispatch/e2e/engine-reading.ts:13"),
        (f"{ENVOY}/internal/dispatch/pmdoc/testdata/fixtures.json", "read at packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts:48, whose path is the new URL at :44, in the module the browser rows of the seven markdown parity families it holds import; the web unit tests that read the same table run in dispatch-ui, which declares it too"),
        ("scripts/e2e/lib/held-port.py", "packages/dispatch/e2e/run-server.sh:80,89 holds each stack's server port through it"),
        (".github/scripts/dispatch-browser-samples.sh",
         f"{at_browser(BROWSER_SHARD_SPECS)} and {at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)} "
         "start it and read its two summaries"),
        (".github/scripts/dispatch-browser-postgres.sh",
         f"{at_browser(BROWSER_PG_STEP)} and {at_browser(BROWSER_PG_STEP, BROWSER_ALONE)} "
         "run it for the reading of the image's PostgreSQL their stacks start"),
        (".github/scripts/dispatch-browser-latest-blobs.sh", at_browser(BROWSER_SELECT_STEP, BROWSER_MERGE)),
        (".github/scripts/dispatch-browser-run-reads.sh",
         f"{at_browser(BROWSER_READ_ONE_STEP, BROWSER_MERGE)} runs its jobs mode over this run's "
         f"own attempt's jobs, and {at_browser(BROWSER_BLOBS_REFUSED_STEP, BROWSER_MERGE)} its "
         "blobs-refused mode, which prints the line for a blob download actions/download-artifact "
         "could not complete"),
        (".github/scripts/gh-read.sh",
         "dispatch-browser-run-reads.sh:79 sources it, so the merge job's read of its own attempt's "
         "jobs takes its retry ladder, and the blobs-refused mode makes no request"),
        # The container the shard and alone jobs run in, and what decides which bytes it is.
        # No step reads these three: the `container:` key does, before the first step runs, so
        # the citation is that key's own line rather than a step of the job.
        (".github/ci-images.yaml",
         "dispatch-browser.yaml:196 (shard) and :630 (alone) run in the image at the digest the "
         "one `built:` row of this file carries, so a row that moves moves what those two jobs "
         "run inside"),
        (".github/docker/browser.Dockerfile",
         "the Dockerfile that image is built from: its bytes are the browsers, their host "
         "packages and the PostgreSQL the lanes of dispatch-browser.yaml:196 and :630 start"),
        (".github/workflows/browser-image.yaml",
         "the build that publishes that image: its labels, platforms and action pins decide the "
         "bytes the row at dispatch-browser.yaml:196 and :630 names"),
        ("scripts/third-party-notices.ts", "packages/dispatch/web/vite.config.ts:12 (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
    ) + envoy_client_dep("imported by packages/envoy-client/bin/dispatch.ts, which "
                         "packages/dispatch/e2e/inbox-cli.e2e.ts:331 runs")
      + setup_go(BROWSER_BINARIES, f"{ENVOY}/go.mod", BROWSER_SITE)
      + setup_go(BROWSER_MP, f"{ENVOY}/go.mod", BROWSER_SITE) + bun_job(BROWSER_SHARD, BROWSER_SITE)
      + CI_SELF + BROWSER_SITE_SELF, workflow=CI),

    gate("claude_envoy", "claude-envoy", reads=CLAUDE_ENVOY_JOB + reads(
        (TEST_FIXTURES_SRC, "packages/claude-envoy/tests import @legion/test-fixtures"),
    ) + biome(at("claude-envoy", "Lint Claude Envoy plugin")) + bun_job("claude-envoy")),

    gate("envoy_go", "envoy-go", runs=[
        step_run("envoy-go", "Integration test envoy (a real NATS server, in the test process)",
                 "go test ./internal/integration/ -timeout 300s -count=1", ENVOY, ["./internal/integration/"]),
        step_run("envoy-go", "Vet agentloop's live proof (no model call)", "go vet -tags modellive ./agentloop/", ENVOY,
                 ["./agentloop/"], tags="modellive", mode="vet"),
    ], exact=[
        # One go test per package of the plain run the race lane cannot make (RACE_IMPOSSIBLE_*),
        # each selecting that package's own tests with -run.
        pinned_step("envoy-go", RACE_IMPOSSIBLE_STEP, ENVOY, RACE_IMPOSSIBLE_SCRIPT, [
            (ENVOY, [f"./{under}"], f"go test -run of what the race build cannot run in ./{under}", {})
            for under in RACE_IMPOSSIBLE_PACKAGES
        ]),
        # The module but the race lane's packages, internal/integration, which the integration
        # step tests, and the text checks, which the text-check job runs. The Run names the
        # packages the step's own `grep -Ev` keeps (packages_kept over UNIT_TEST_PATTERN), so it
        # is read from the pattern the step runs rather than claimed beside it. A Run saying
        # more would credit this job with tests other jobs run: the race lane's are
        # envoy-go-race's, envoy-go-race-api's and envoy-go-race-docs', the text checks are the
        # text-check job's, and the closure of ./... would put their test files and the files
        # only those tests read on this job's filter, so a change to one of them would start a
        # job that does not run the test it touches.
        # Its -skip takes WEB_FIXTURE_TESTS out of this step: without `--check`
        # TestWebDisplayFixture writes the committed web display fixture rather than checking it,
        # so a second run here asks nothing and can only pass, and
        # TestTheWebFixtureFindsItsFilesFromAnyDirectory stats that same file, a read this job
        # need not carry; the fixture step of envoy-go-checks runs both, with the check that
        # makes the first a requirement. The tests stay compiled into this step's binary, which
        # is what a skipped test is.
        pinned_step("envoy-go", UNIT_TEST_STEP, ENVOY, UNIT_TEST_SCRIPT, [
            (ENVOY, packages_kept(ENVOY, UNIT_TEST_PATTERN),
             "go list ./... piped into go test, counted as go test over the packages its grep -Ev keeps", {}),
        ]),
        # Only what the natslive tag adds, one package at a time: the rig's own matrix, and the
        # one test of internal/bus that needs a real server image. The rest of internal/testnats
        # runs in the unit step; the rest of internal/bus runs in envoy-go-race, since
        # UNIT_TEST_PATTERN leaves the race lane's packages out of the unit step, and the
        # envoy_go entry of TAG_RUNS, one entry per step, checks the testnats half alone.
        pinned_step("envoy-go", NATS_MATRIX_STEP, ENVOY, NATS_MATRIX_SCRIPT, [
            (ENVOY, ["./internal/testnats/"], "go test -tags natslive ./internal/testnats/", {"tags": "natslive"}),
            (ENVOY, ["./internal/bus/"], "go test -tags natslive ./internal/bus/", {"tags": "natslive"}),
        ]),
    ], reads=reads(
        (".github/actions/pull-test-images/**", at("envoy-go", "Pull test container images")),
        (TAGGED_TESTS, f"{at('envoy-go', NATS_MATRIX_STEP)} takes its -run from it"),
    ) + setup_go("envoy-go", f"{ENVOY}/go.mod") + bun_job("envoy-go")),

    # envoy-go's fast half: its format and generated-code checks, the seven shell-script tests
    # and the build and pack of the envoy commands (ci.yaml's comment on envoy-go-checks says why
    # they are split out). Its gofmt step reads the whole module, so no closure stands for it.
    gate("envoy_go_checks", "envoy-go-checks", runs=[
        step_run("envoy-go-checks", "Build envoy services", "go build ./cmd/dispatch ./cmd/listener", ENVOY,
                 ["./cmd/dispatch", "./cmd/listener"], mode="build"),
    ], exact=[
        # `go run` is not one of the three forms the reducer reads, so the step is pinned whole
        # and this Run stands for the command it compiles.
        pinned_step("envoy-go-checks", "Check the OpenAPI document is generated from the route table", ENVOY,
                    "go run ./cmd/dispatch openapi --check", [
                        (ENVOY, ["./cmd/dispatch"],
                         "go run ./cmd/dispatch openapi --check, which emits the document from the "
                         "route table and compares it with the committed one", {"mode": "build"}),
                    ]),
        # One linux build of the listener per architecture the loop reads from the target list.
        pinned_step("envoy-go-checks", PACK_STEP, ENVOY, PACK_SCRIPT, [
            (ENVOY, ["./cmd/listener"], f"GOOS=linux GOARCH={arch} go build ./cmd/listener",
             {"mode": "build", "env": {"GOOS": "linux", "GOARCH": arch}})
            for arch in targets(GO_TARGETS, "envoy")
        ]),
        # The display-tree fixture check: one test of pmdoc, which compiles that package.
        pinned_step("envoy-go-checks", FIXTURE_STEP, ENVOY, FIXTURE_SCRIPT, [
            (ENVOY, ["./internal/dispatch/pmdoc/"], "go test ./internal/dispatch/pmdoc/", {}),
        ]),
    ], reads=reads(
        (f"{ENVOY}/**", f"{at('envoy-go-checks', 'Check Go formatting')}: gofmt -l . in packages/envoy reads every .go file under it, infra included; the script tests below live here too"),
        ("packages/contracts/scripts/gen-go.ts", f"{at('envoy-go-checks', 'Check Go contracts are generated from the shared schema')} (packages/contracts/package.json check:go)"),
        ("packages/contracts/schemas/**", "packages/contracts/scripts/gen-go.ts's envelopeFile and blockSchemaFile read schemas/"),
        # The pinned step above compares the emitter's bytes with this file, so an edit of it alone
        # must start this job: the Run stands for what the command compiles, never for what it reads.
        ("packages/contracts/schemas/dispatch-openapi.json",
         f"{at('envoy-go-checks', 'Check the OpenAPI document is generated from the route table')}: "
         "cmd/dispatch/subcommands.go:125 reads the committed document to compare it (:171 only builds a "
         "candidate path for it)"),
        (CONTRACTS_SRC, "packages/contracts/scripts/gen-go.ts's imports of ../src/*"),
        (f"{ENVOY}/internal/dispatch/pmdoc/gen/**", f"{at('envoy-go-checks', 'Check pmdoc fixtures are up to date')} (gen.ts --check)"),
        ("packages/proof-editor/src/**", "packages/envoy/internal/dispatch/pmdoc/gen/gen.ts:4-6 imports @legion/proof-editor (exports -> src/lib.ts, src/lib-headless.ts, src/document-reading.ts)"),
        # The web app's display-tree fixture, whose content the fixture step's --check compares the
        # trees it builds with, and whose existence the other test of WEB_FIXTURE_TESTS,
        # TestTheWebFixtureFindsItsFilesFromAnyDirectory (web_fixtures_test.go:248-255), stats in
        # that same step. Declared on this step and not as a RULES row keyed on
        # web_fixtures_test.go: a row keyed on the file would put the fixture into every job that
        # compiles pmdoc's tests, envoy-go's unit step among them, which skips both tests and so
        # touches the file in no way. The fixture is this job's read alone.
        ("packages/dispatch/web/src/features/refs/__fixtures__/display-trees.json",
         f"{at('envoy-go-checks', FIXTURE_STEP)}: internal/dispatch/pmdoc/web_fixtures_test.go:40-45 "
         "webFixturePath joins it under pmdocSourceDir, and --check reads its content to compare"),
        (f"{ENVOY}/loadtest/run-step.test.sh", at("envoy-go-checks", LOADTEST_RUN_STEP)),
        (f"{ENVOY}/loadtest/run-step.sh", "packages/envoy/loadtest/run-step.test.sh runs it"),
        (f"{ENVOY}/loadtest/run-series.sh", "packages/envoy/loadtest/run-step.test.sh runs it"),
        (f"{ENVOY}/loadtest/common.test.sh", at("envoy-go-checks", LOADTEST_LOCK)),
        (f"{ENVOY}/loadtest/common.sh", "packages/envoy/loadtest/common.test.sh sources it; run-step.sh and run-series.sh source it"),
        (NOTICES, f"{at('envoy-go-checks', 'Pack envoy binaries')} runs it"),
        (GO_TARGETS, f"{at('envoy-go-checks', 'Pack envoy binaries')}: jq -r '.envoy[]'"),
    ) + setup_go("envoy-go-checks", f"{ENVOY}/go.mod") + bun_job("envoy-go-checks")),


    gate("envoy_infra_go", "envoy-infra-go", runs=[
        step_run("envoy-infra-go", "Vet envoy-infra, its live-test tags included", "go vet -tags pulumilive,natslive ./...", INFRA, ["./..."],
                 tags="pulumilive,natslive", mode="vet"),
    ], exact=[
        # The module but the text checks, which the text-check job runs: `go test ./...` cannot
        # say "but one package", so the step lists the packages and drops that one, and is
        # pinned whole as the other two lanes' plain steps are. The vet step above still
        # compiles the text checks, and must: vetting them keeps them compiling, and a vet run
        # reads none of the files a test of theirs reads.
        pinned_step("envoy-infra-go", INFRA_PLAIN_STEP, INFRA, INFRA_PLAIN_SCRIPT, [
            (INFRA, packages_kept(INFRA, INFRA_PLAIN_PATTERN),
             "go list ./... piped into go test, counted as go test over the packages its grep -Ev keeps", {}),
        ]),
        # One go test per package, each running only what the natslive tag adds to it.
        pinned_step("envoy-infra-go", INFRA_NATSLIVE_STEP, INFRA, INFRA_NATSLIVE_SCRIPT, [
            (INFRA, [pkg], f"go test -tags natslive {pkg}", {"tags": "natslive"})
            for pkg in INFRA_NATSLIVE_PACKAGES
        ]),
    ], reads=reads(
        (f"{INFRA}/**", f"{at('envoy-infra-go', 'Check Go formatting')}: gofmt -l . in packages/envoy/infra reads every .go file under it"),
        (".github/actions/pull-test-images/**", at("envoy-infra-go", "Pull the NATS and Postgres test container images")),
        (f"{ENVOY}/internal/testnats/nats.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: consts Image, HubImage"),
        (f"{ENVOY}/internal/testpg/testpg.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: const Image"),
        (TAGGED_TESTS, f"{at('envoy-infra-go', INFRA_NATSLIVE_STEP)} takes each call's -run from it"),
    ) + setup_go("envoy-infra-go", f"{INFRA}/go.mod")),

    # The text-check job: one `go test` of each module's text checks, in that module's own
    # directory, the Dispatch API's source guards by name, and the supervise table's two pure
    # pins by name. Two workspaces meet here - the root go.work binds packages/daemon and
    # packages/envoy, packages/envoy/infra's own binds itself and packages/envoy - so each Run's
    # workspace names the module files it reads. No Bun and no database: nothing here starts
    # either.
    gate("text_checks", TEXT_CHECK_JOB, runs=[
        *[step_run(TEXT_CHECK_JOB, TEXT_CHECK_STEPS[module], TEXT_CHECK_TEXT, module, [TEXT_CHECK_PKG])
          for module in TEXT_CHECK_MODULES],
    ], exact=[
        # The fifteen static tests by name: a -run is outside the simple form, so the step is
        # pinned whole. A -run moves no closure - the whole package compiles whichever tests the
        # run selects - so the Run is the plain go test of the package, which is why this gate
        # reads everything the API's tests read.
        pinned_step(TEXT_CHECK_JOB, GUARD_STEP, ENVOY, GUARD_SCRIPT, [
            (ENVOY, [GUARD_PACKAGE], f"go test -run of the API's fifteen source-check tests in {GUARD_PACKAGE}", {}),
        ]),
        # The supervise table's two pure pins by name (SUPERVISE_TABLE_TESTS), which the
        # daemon-go plain step skips: pinned whole for the same reason, and its Run is the plain
        # go test of the package, since the step compiles every test file of internal/supervise
        # whichever two the -run then selects.
        pinned_step(TEXT_CHECK_JOB, SUPERVISE_TABLE_STEP, DAEMON, SUPERVISE_TABLE_SCRIPT, [
            (DAEMON, [f"./{SUPERVISE_TABLE_PACKAGE}/"],
             f"go test -run of the table's two pure pins in {SUPERVISE_TABLE_PACKAGE}", {}),
        ]),
    ], reads=setup_go(TEXT_CHECK_JOB, f"{ENVOY}/go.mod")),

    # The lane's three jobs: each step's text and packages come from ENVOY_RACE_LANE, which the
    # envoy-go unit step and its plain companion are built from too. envoy-go-race-docs is the one
    # whose step is not race_step's text: it runs the package under .github/scripts/race_memory.py
    # (dispatch://INFRA-1085), so its gate pins that whole line while the lane table still names
    # the package, which is what keeps it out of the unit step.
    gate("envoy_go_race", "envoy-go-race", runs=[race_step(RACE_JOBS["envoy-go-race"])],
         reads=setup_go("envoy-go-race", f"{ENVOY}/go.mod")),

    gate("envoy_go_race_api", "envoy-go-race-api", runs=[race_step(RACE_JOBS["envoy-go-race-api"])],
         reads=setup_go("envoy-go-race-api", f"{ENVOY}/go.mod")),

    gate("envoy_go_race_docs", "envoy-go-race-docs", exact=[
        pinned_step("envoy-go-race-docs", RACE_DOCS_STEP, ENVOY, RACE_DOCS_SCRIPT, [
            (ENVOY, ["./internal/dispatch/docs/"], "go test -race -count=1 -timeout 60m ./internal/dispatch/docs/", {"race": True}),
        ]),
    ], reads=reads(
        (".github/scripts/race_memory.py", f"{at('envoy-go-race-docs', RACE_DOCS_STEP)} runs the package under it"),
    ) + setup_go("envoy-go-race-docs", f"{ENVOY}/go.mod")),

    # A second gate on envoy_go_race_docs: this job runs the same package, so it reads that
    # closure and no other, and check-ci-filters.sh keys a claim by (job, step), so the two gates
    # claim different steps and do not collide.
    gate("envoy_go_race_docs", "envoy-go-docs-finalizers", exact=[
        pinned_step("envoy-go-docs-finalizers", FINALIZERS_STEP, ENVOY, FINALIZERS_SCRIPT, [
            (ENVOY, ["./internal/dispatch/docs/"], "go test -v -count=1 -timeout 34m ./internal/dispatch/docs/", {}),
        ]),
    ], reads=setup_go("envoy-go-docs-finalizers", f"{ENVOY}/go.mod")),

    gate("dispatch_memory", "dispatch-memory", exact=[
        pinned_step("dispatch-memory", MEMORY_STEP, ENVOY, MEMORY_SCRIPT, [
            (ENVOY, ["./cmd/dispatch"], "go test -tags memory ./cmd/dispatch", {"tags": "memory"}),
        ]),
    ], reads=reads(
        (TAGGED_TESTS, f"{at('dispatch-memory', MEMORY_STEP)} takes its -run from it"),
    ) + setup_go("dispatch-memory", f"{ENVOY}/go.mod")),

    gate("dispatch_multiprocess", "dispatch-multiprocess", exact=[
        pinned_step("dispatch-multiprocess", DISPATCH_MP_STEP, ENVOY, DISPATCH_MP_SCRIPT, [
            (ENVOY, ["./cmd/dispatch"], "go test -tags multiprocess ./cmd/dispatch", {"tags": "multiprocess"}),
        ]),
    ], reads=reads(
        (TAGGED_TESTS, f"{at('dispatch-multiprocess', DISPATCH_MP_STEP)} takes its -run from it"),
    ) + setup_go("dispatch-multiprocess", f"{ENVOY}/go.mod")),

    gate("listener_server_change", "listener-server-change", exact=[
        pinned_step("listener-server-change", SOAK_STEP, ENVOY, SOAK_SCRIPT, [
            (ENVOY, ["./cmd/listener"], "go test -tags serverchange ./cmd/listener", {"tags": "serverchange"}),
        ]),
    ], reads=reads(
        (TAGGED_TESTS, f"{at('listener-server-change', SOAK_STEP)} takes its -run from it"),
    ) + setup_go("listener-server-change", f"{ENVOY}/go.mod")),

    gate("broker_multiprocess", "broker-multiprocess", exact=[
        pinned_step("broker-multiprocess", BROKER_MP_STEP, ENVOY, BROKER_MP_SCRIPT, [
            (ENVOY, ["./cmd/broker"], "go test -tags multiprocess ./cmd/broker", {"tags": "multiprocess"}),
        ]),
    ], reads=reads(
        (TAGGED_TESTS, f"{at('broker-multiprocess', BROKER_MP_STEP)} takes its -run from it"),
    ) + setup_go("broker-multiprocess", f"{ENVOY}/go.mod")),

    gate("airlock_lean", AIRLOCK_JOB, exact=[
        pinned_step(AIRLOCK_JOB, AIRLOCK_SEED_STEP, ENVOY, AIRLOCK_SEED_SCRIPT, [
            (ENVOY, AIRLOCK_PKGS, "the airlock package's tests over the committed seed corpus", {}),
        ]),
        pinned_step(AIRLOCK_JOB, AIRLOCK_FUZZ_STEP, ENVOY, AIRLOCK_FUZZ_SCRIPT, [
            (ENVOY, AIRLOCK_PKGS, "the same tests, fuzzed against the model for 120 seconds", {}),
        ]),
    ], reads=reads(
        (".github/actions/install-elan/**", at(AIRLOCK_JOB, "./.github/actions/install-elan")),
        (".github/ci-tools.yaml", ".github/actions/install-elan/action.yml:29-30 installs the elan row's tarball"),
        (".github/scripts/ci-tools.sh", ".github/actions/install-elan/action.yml:22 sources it, and ci_tools_fetch "
                                        "reads the elan row's two sources"),
        (f"{AIRLOCK_LEAN}/**", f"{at(AIRLOCK_JOB, AIRLOCK_BUILD_STEP)}: lake build reads the model's whole project, and "
                               f"{at(AIRLOCK_JOB, AIRLOCK_AXIOMS_STEP)} runs Axioms.lean over the library it builds"),
        (f"{AIRLOCK_LEAN}/lean-toolchain", f"{at(AIRLOCK_JOB, 'elan-cache')}: the elan cache's key is hashFiles of it"),
        (AIRLOCK_AXIOMS_SCRIPT, f"{at(AIRLOCK_JOB, AIRLOCK_AXIOMS_STEP)} is one line running it, and check k15 of "
                                f"{ENVOY}/scripts/airlock-check.sh runs the same script, so the two cannot drift"),
    ) + setup_go(AIRLOCK_JOB, f"{ENVOY}/go.mod")),


    # The listener image this job builds and smokes is built from the repository root, so every
    # file packages/envoy/docker/Dockerfile copies into that context is a file this job reads:
    # each one below names the COPY line that reads it. Only those the job's go commands do not
    # already reach are named, which is what keeps this list short - the module files, the whole
    # of packages/envoy/internal and the RDS bundle are reads of the smoke and live tests
    # themselves. check-image-trigger-paths.sh holds the filter to the image's inputs from the
    # other side, proving the list complete; this gate proves no pattern of the filter is idle.
    gate("envoy_smoke", "envoy-smoke", runs=[
        step_run("envoy-smoke", "Container smoke test (testcontainers)", "go test -tags smoke -v -timeout 5m ./internal/smoke/",
                 ENVOY, ["./internal/smoke/"], tags="smoke"),
        step_run("envoy-smoke", "Live tests of envoy-infra's host:Listener", "go test -tags pulumilive -v -timeout 15m ./host/",
                 INFRA, ["./host/"], tags="pulumilive"),
    ], exact=[
        pinned_step("envoy-smoke", SCHEMA_STEP, INFRA, SCHEMA_SCRIPT, [
            (INFRA, ["./cmd/pulumi-resource-envoy-infra"], "go build ./cmd/pulumi-resource-envoy-infra", {"mode": "build"}),
        ]),
    ], reads=reads(
        (".github/actions/pull-test-images/**", at("envoy-smoke", "Pull test container images")),
        (f"{ENVOY}/internal/testnats/nats.go", f"{at('envoy-smoke', 'Pull test container images')}: const Image"),
        (".github/actions/install-pulumi/**", at("envoy-smoke", "./.github/actions/install-pulumi")),
        (".github/actions/install-docker-plugin/**",
         at("envoy-smoke", "Download the docker provider plugin the live tests install")),
        (".github/ci-tools.yaml", ".github/actions/install-pulumi/action.yml:20-21 installs the pulumi-cli row's "
                                  "tarball, and .github/actions/install-docker-plugin/action.yml:33-35 downloads the "
                                  "pulumi-docker row's"),
        (".github/scripts/ci-tools.sh", ".github/actions/install-pulumi/action.yml:19 and "
                                        ".github/actions/install-docker-plugin/action.yml:30 source it, and "
                                        "ci_tools_fetch reads each row's two sources"),
        (".github/scripts/repo-paths.sh",
         f"{at('envoy-smoke', SCHEMA_STEP)}: it derives the download URL the build stamps in"),
        (".github/scripts/disk_watch.py",
         f"{at('envoy-smoke', 'Start the machine watch over the live tests')} and "
         f"{at('envoy-smoke', 'Report what the live tests ran on')} run it"),
        (f"{ENVOY}/docker/Dockerfile", f"{at('envoy-smoke', SMOKE_IMAGE_STEP)} builds it (its `file:`), and "
                                       f"{at('envoy-smoke', BASE_IMAGES_STEP)} reads its FROM lines"),
        (".dockerignore", f"{at('envoy-smoke', SMOKE_IMAGE_STEP)} builds with `context: .`, so the root .dockerignore "
                          "decides which of the files below reach any COPY of that Dockerfile"),
        (".bun-version", "packages/envoy/docker/Dockerfile:20 COPY .bun-version, which :21-27 holds the web stage's own Bun to"),
        ("package.json", "packages/envoy/docker/Dockerfile:29 COPY package.json bun.lock ./"),
        ("bun.lock", "packages/envoy/docker/Dockerfile:29 COPY package.json bun.lock ./"),
        ("patches/**", "packages/envoy/docker/Dockerfile:30 COPY patches patches"),
        ("packages/broker-client/package.json", "packages/envoy/docker/Dockerfile:32"),
        ("packages/envoy-client/package.json", "packages/envoy/docker/Dockerfile:33"),
        ("packages/pi-envoy/package.json", "packages/envoy/docker/Dockerfile:34"),
        ("packages/pi-legion/package.json", "packages/envoy/docker/Dockerfile:35"),
        ("packages/pi-shared/package.json", "packages/envoy/docker/Dockerfile:36"),
        ("packages/claude-envoy/package.json", "packages/envoy/docker/Dockerfile:37"),
        ("packages/test-fixtures/package.json", "packages/envoy/docker/Dockerfile:43"),
        ("docs/site/package.json", "packages/envoy/docker/Dockerfile:45; :62 RUN bun install --frozen-lockfile reads every manifest above"),
        ("packages/contracts/**", "packages/envoy/docker/Dockerfile:31 and :68 COPY packages/contracts packages/contracts, which :76 runs gen:go in"),
        ("packages/dispatch/**", "packages/envoy/docker/Dockerfile:38 and :69 COPY packages/dispatch packages/dispatch, which :78 runs build:web in"),
        ("packages/proof-editor/**", "packages/envoy/docker/Dockerfile:39, :42 and :70 COPY packages/proof-editor packages/proof-editor, which the SPA build aliases"),
        ("scripts/third-party-notices.ts", "packages/envoy/docker/Dockerfile:72 COPY scripts/third-party-notices.ts, which the SPA build writes its notices with"),
        ("scripts/go-third-party-notices.sh", "packages/envoy/docker/Dockerfile:95 COPY, run by :96 and :128"),
        ("scripts/assemble-third-party-notices.sh", "packages/envoy/docker/Dockerfile:95 COPY, run by :130"),
        (f"{ENVOY}/cmd/**", "packages/envoy/docker/Dockerfile:100 COPY packages/envoy/cmd ./cmd, which :145-151 builds the four binaries from"),
        # agentloop: the model loop cmd/broker's judge compiles in, a package outside internal/,
        # so the image build copies it beside cmd and internal (main's #2984). No go command of
        # this gate reaches it, so rule 9 of check-ci-filters.sh reads the filter's pattern as
        # dead without this row, while check-image-trigger-paths.sh refuses the filter without
        # the pattern: the read is the image build's, and this is where the two checks meet.
        (f"{ENVOY}/agentloop/**", "packages/envoy/docker/Dockerfile:105 COPY packages/envoy/agentloop ./agentloop"),
        (f"{ENVOY}/going/**", "packages/envoy/docker/Dockerfile:112 COPY packages/envoy/going ./going"),
        (f"{ENVOY}/listenfd/**", "packages/envoy/docker/Dockerfile:108 COPY packages/envoy/listenfd ./listenfd"),
        (f"{ENVOY}/migrationid/**", "packages/envoy/docker/Dockerfile:115 COPY packages/envoy/migrationid ./migrationid"),
        (f"{ENVOY}/roleholder/**", "packages/envoy/docker/Dockerfile:117 COPY packages/envoy/roleholder ./roleholder"),
        # The second image this job builds. Its context is that directory alone, test file and
        # all, so the filter owes every file of it: check-image-trigger-paths.sh refuses a
        # trigger that leaves one out, and rule 17 of check-ci-filters.sh would otherwise write
        # the exclusion that leaves main_test.go out, no go command of this gate compiling it.
        (f"{INFRA}/host/heldportwrapper/**", "packages/envoy/infra/host/heldportwrapper/Dockerfile:31 COPY "
                                             "packages/envoy/infra/host/heldportwrapper ./host/heldportwrapper, "
                                             "which :32 builds the held-port wrapper from"),
    ) + setup_go("envoy-smoke", f"{ENVOY}/go.mod")),

    gate("envoy_e2e_local", "envoy-e2e-local", runs=[
        script_run(f"{ENVOY}/scripts/e2e-local.sh", 'go build -o "$listener_binary" ./cmd/listener',
                   f"run by {at('envoy-e2e-local', 'Local end-to-end acceptance')}", ENVOY, ["./cmd/listener"], mode="build"),
        script_run(f"{ENVOY}/scripts/e2e-local.sh", 'go build -o "$nats_binary" ./internal/testnats/cmd/natsd',
                   f"run by {at('envoy-e2e-local', 'Local end-to-end acceptance')}", ENVOY, ["./internal/testnats/cmd/natsd"], mode="build"),
    ], reads=reads(
        (f"{ENVOY}/scripts/e2e-local.test.sh", at("envoy-e2e-local", "Refusal test for e2e-local.sh")),
        (f"{ENVOY}/scripts/e2e-local.sh", f"{at('envoy-e2e-local', 'Local end-to-end acceptance')}; e2e-local.test.sh:6,27"),
        (f"{ENVOY}/scripts/fixtures/github/**", "packages/envoy/scripts/e2e-local.sh:877-908 posts each fixture"),
        # envoy-client's sources, and broker-client's with them, are the shared read below.
        (CONTRACTS_SRC, "packages/envoy-client/src/delivery.ts imports @legion/contracts"),
    ) + envoy_client_dep("packages/envoy/scripts/e2e-local.sh:953-957 and :1185-1193 run bun in packages/envoy-client "
                         "importing @legion/envoy-client/delivery and @legion/envoy-client/transport")
      + setup_go("envoy-e2e-local", f"{ENVOY}/go.mod") + bun_job("envoy-e2e-local")),

    gate("envoy_session_stall", STALL, runs=[
        script_run("packages/envoy-client/test/envoy-stack.ts",
                   'const spawned = Bun.spawn(["go", "build", "-o", `${build.bin}${path.sep}`, ...build.packages], {',
                   f"buildGoBinaries, which both rigs of that directory call; this one's packages are the Envoy "
                   f"stack's, built for {at(STALL, STALL_CLIENT)}, {at(STALL, 'Stall test of pi-envoy')} and "
                   f"{at(STALL, 'Stall test of claude-envoy')}", ENVOY, ["./internal/testnats/cmd/natsd", "./cmd/listener"], mode="build"),
        script_run("packages/envoy-client/test/dispatch-stack.ts",
                   'packages: ["./cmd/dispatch"],',
                   f"the packages that rig hands buildGoBinaries, run by "
                   f"{at(STALL, 'Design-gate advice of the real CLI against a real Dispatch')}",
                   ENVOY, ["./cmd/dispatch"], mode="build"),
    ], reads=reads(
        ("packages/envoy-client/**", f"{at(STALL, STALL_CLIENT)}: src/__tests__/nats-stall.test.ts and test/*.ts, which the other two import"),
        ("packages/pi-envoy/**", at(STALL, "Stall test of pi-envoy")),
        (TEST_FIXTURES_SRC, "packages/pi-envoy/extensions/envoy.test.ts imports @legion/test-fixtures/environment"),
        ("packages/claude-envoy/**", f"{at(STALL, 'Stall test of claude-envoy')} (tests/envoy-channel-stall.test.ts runs bin/envoy-channel.ts)"),
        ("packages/pi-shared/src/**", "packages/pi-envoy and packages/claude-envoy import @legion/pi-shared/*"),
        ("packages/contracts/**", at(STALL, "Build contracts for the stall suites")),
        (BROKER_CLIENT_SRC, f"{at(STALL, 'Stall test of claude-envoy')} runs packages/claude-envoy/bin/envoy-channel.ts, whose bun build loads six of @legion/broker-client's seven modules through packages/envoy-client/src/broker-credential.ts:21-22"),
    ) + setup_go(STALL, f"{ENVOY}/go.mod") + bun_job(STALL)),

    gate("go_notices", "lint", step=NOTICES_STEP, runs=go_notices_runs(), reads=reads(
        (NOTICES_LOOPS, f"{at('lint', NOTICES_STEP)} runs it; it holds the loops"),
        (NOTICES, f"{NOTICES_LOOPS} runs it once per release target"),
        (GO_TARGETS, f"{NOTICES_LOOPS}: jq reads it"),
        (CLIENT_TARGETS, f"{NOTICES_LOOPS}: jq reads it"),
        (INFRA_TARGETS, f"{NOTICES_LOOPS}: jq reads it"),
    )),

    # This check itself: it lists every module's packages (vet mode: each package directory, its
    # dependencies' and the test files, without the files tests read), reads the target lists
    # the runs above expand, reads every workflow of .github/workflows, and is its own script,
    # engine, data, rules and path model.
    gate("ci_filters", "ci-filters", runs=[
        run(DAEMON, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the daemon module", mode="vet", step=CHECK_STEP,
            text=".github/scripts/check-ci-filters.sh"),
        run(ENVOY, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the envoy module", mode="vet", step=CHECK_STEP,
            text=".github/scripts/check-ci-filters.sh"),
        run(INFRA, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the envoy-infra module", mode="vet", step=CHECK_STEP,
            text=".github/scripts/check-ci-filters.sh"),
    ], reads=reads(
        ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.go", "the pi_legion gate's go run -overlay compiles it"),
        (".github/scripts/check-ci-filters.sh", at("ci-filters", CHECK_STEP)),
        (".github/scripts/ci_filter_coverage.py", "check-ci-filters.sh's engine"),
        (".github/scripts/ci_job_reads.py", "check-ci-filters.sh's data"),
        (".github/scripts/ci_workflow_rules.py", "check-ci-filters.sh's rules about ci.yaml's conditions and "
                                                 "ci-caches.yaml, which ci_filter_coverage.main() runs"),
        (".github/scripts/workflow_paths.py", "ci_filter_coverage.py reads filters through it"),
        ("package.json", "ci_job_reads.py reads the workspace list from the root manifest"),
        (GO_TARGETS, "ci_job_reads.py expands the notices and pack runs over it"),
        (CLIENT_TARGETS, "ci_job_reads.py expands the notices runs over it"),
        (INFRA_TARGETS, "ci_job_reads.py expands the notices runs over it"),
        (DOCS_SITE, "ci_filter_coverage.py looks the docs gate's step citations up in it"),
        (BROWSER_SITE, "ci_filter_coverage.py looks the dispatch_browser gate's step citations up in it"),
        (BROWSER_NIGHTLY, "dispatch-browser.yaml's other caller: a change to either must re-run this check, which "
                          "holds the dispatch_browser filter to that gate"),
        (CACHE_WORKFLOW, "ci_workflow_rules.py holds its cache saves to ci.yaml's restores"),
        (".github/workflows/**", "ci_workflow_rules.check_one_merge_group_workflow reads every workflow of the "
                                 "directory, and a rule that refuses a new one has to run on the change that adds it"),
        # Rule 17 computes each gate filter's generated block from two sets: the repository's Go
        # test files and the ones each gate reads. A test file added or deleted anywhere changes
        # what that rule writes, so the change that adds one has to start this job - otherwise
        # ci.yaml goes stale on a pull request this check never ran on, and the next pull
        # request that does start it goes red for a commit of its own that changed nothing here.
        ("**/*_test.go", "rule 17 of check-ci-filters.sh writes each filter's exclusions out of the repository's Go "
                         "test files, so one added or deleted changes this check's own answer"),
    ) + reads(*((path, f"check-ci-filters.sh holds its on.push.paths equal to the {name} filter (PUSH_WORKFLOWS)")
                for name, path in PUSH_WORKFLOWS.items()))
      + setup_go("ci-filters", f"{DAEMON}/go.mod")),

    # The proof-sdk-offline job: a frozen install behind a proxy that answers 504 for every host
    # the proof editor's old `github:sjawhar/proof-sdk#<sha>` route used, which proves the
    # install reaches none of them. It reads what any frozen install reads (bun_job: setup-bun,
    # the manifests, the lockfile, the patches and the tarball the proof editor installs
    # proof-sdk-upstream from), the proxy program its first step starts, and this file.
    gate("proof_sdk_offline", "proof-sdk-offline", reads=reads(
        (".github/scripts/ci-fault-proxy.py",
         f"{at('proof-sdk-offline', 'Start the fault proxy over the old proof-sdk route')} runs it; "
         f"the hosts it faults (the job's FAULT_HOSTS) and the log its refusals are counted from "
         f"are that step's arguments"),
    ) + bun_job("proof-sdk-offline")),

    # The ci-images job: the image copy, the package mode of the image gate, which reads
    # ghcr.io, and the tool copy, which fills the second source of every tool install. These are
    # the files that decide what those three steps do. The image gate's tree mode is a step of
    # lint, which runs on every change, and no filter gates it.
    gate("ci_images", "ci-images", reads=reads(
        (".github/ci-images.yaml", f"{at('ci-images', COPY_STEP)} copies the rows it holds, and "
                                   f"{at('ci-images', PACKAGE_STEP)} reads each row's digest"),
        (".github/scripts/mirror-ci-images.sh", f"{at('ci-images', COPY_STEP)} runs it"),
        (".github/scripts/check-ci-images.sh", f"{at('ci-images', PACKAGE_STEP)} runs it"),
        (".github/scripts/ci_images.py", "every reader of a row goes through it, the recipe hash "
                                         "rule 10 compares among them"),
        (".github/docker/**", f"{at('ci-images', PACKAGE_STEP)} hashes every file of the build "
                              f"context, which is this directory, and refuses the built row whose "
                              f"published image carries another recipe's sha256 in its "
                              f"legion.browser-image.source-sha256 label"),
        (".github/workflows/browser-image.yaml", f"the other half of that recipe: "
                                                 f"{at('ci-images', PACKAGE_STEP)} hashes this file "
                                                 f"too, since its labels, outputs, platforms and "
                                                 f"action pins decide the bytes the build pushes"),
        (".github/ci-tools.yaml", f"{at('ci-images', TOOL_COPY_STEP)} fills the copy of every ghcr-mirror row it holds"),
        (".github/scripts/mirror-ci-tools.sh", f"{at('ci-images', TOOL_COPY_STEP)} runs it"),
        (".github/scripts/ci-tools.sh", f"{at('ci-images', TOOL_COPY_STEP)}: the filler reads a row's fields and "
                                        "its seams the way that helper does, and the copy it fills is what the "
                                        "helper then reads"),
        (".github/actions/install-crane/**", at("ci-images", "./.github/actions/install-crane")),
    )),

    # The tool-fallback job: the live proof that every tool install reads the second source
    # .github/ci-tools.yaml names when the first one does not serve the bytes. It runs no go
    # command, so the gate is its reads alone: the manifest, the fetch helper, the filler and
    # the gate beside it, the rig and the fault proxy it drives, the eight actions it installs
    # through, and the two files a row's version is pinned in that a step of it reads.
    gate("tool_fallback", "tool-fallback", reads=reads(
        (".github/ci-tools.yaml", f"{at('tool-fallback', TOOL_BLOB_STEP)} reads the digest of every ghcr-mirror row, "
                                  f"and {at('tool-fallback', TOOL_ROWS_STEP)} reads every row"),
        (".github/scripts/ci-tools.sh", f"{at('tool-fallback', TOOL_STAGE_STEP)}: the rig fetches each row's correct "
                                        "bytes through it, and the two steps above source it for a row's fields"),
        (".github/scripts/ci-tools-proof.sh", f"{at('tool-fallback', TOOL_STAGE_STEP)} runs it, as every other step "
                                              "of this job that drives the proof does; tool-fallback-arm drives it "
                                              "too, and the test job runs its ladder-report verb over logs of its "
                                              "own"),
        (".github/scripts/ci-fault-proxy.py", f"{at('tool-fallback', TOOL_FAULT_STEP)}: the rig starts it in front "
                                              "of GitHub's release assets"),
        (".github/scripts/mirror-ci-tools.sh", f"{at('tool-fallback', TOOL_BLOB_STEP)} refuses a row that script has "
                                               "not filled, and states the rule that is the opposite of its own"),
        (".github/scripts/check-ci-tools.sh", f"{at('tool-fallback', TOOL_ROWS_STEP)} prints the one literal both "
                                              "paths are held to, which that gate holds every row and every call "
                                              "site of the tree to"),
        (".github/actions/install-crane/**", at("tool-fallback", "./.github/actions/install-crane")),
        (".github/actions/install-changie/**", at("tool-fallback", "./.github/actions/install-changie")),
        (".github/actions/install-jj/**", at("tool-fallback", "./.github/actions/install-jj")),
        (".github/actions/setup-bun/**", at("tool-fallback", "./.github/actions/setup-bun")),
        (".github/actions/install-elan/**", at("tool-fallback", "./.github/actions/install-elan")),
        (".github/actions/install-uv/**", at("tool-fallback", "./.github/actions/install-uv")),
        (".github/actions/install-pulumi/**", at("tool-fallback", "./.github/actions/install-pulumi")),
        (".github/actions/install-docker-plugin/**",
         at("tool-fallback", "./.github/actions/install-docker-plugin")),
        (f"{DAEMON}/docker/worker.Dockerfile", f"{at('tool-fallback', TOOL_JJ_SEED_STEP)}: the rig reads ARG JJ_TOOL "
                                               "to name mise's own install directory for that tool, and "
                                               ".github/actions/install-jj reads that same ARG, while the mise row "
                                               "that action installs mise from is pinned to this file's "
                                               "ARG MISE_VERSION and ARG MISE_SHA256 by .github/scripts/check-ci-tools.sh"),
        (".bun-version", f"{at('tool-fallback', TOOL_BUN_BYTE_STEP)} reads the pinned version out of it, and "
                         ".github/actions/setup-bun installs the Bun it pins"),
        (".github/scripts/disk_watch.py", f"{at('tool-fallback', TOOL_WATCH_STEP)} runs it, and "
                                          f"{at('tool-fallback', TOOL_REPORT_STEP)} runs it again"),
    )),

    # A second gate on tool_fallback: the tool-fallback-arm job proves the same two halves of
    # the Bun install on an arm64 runner, the one architecture no other job of this repository
    # installs a tool on. It drives the same action over the same manifest through the same
    # helper, so one filter selects both jobs, and check-ci-filters.sh keys a claim by
    # (job, step), so the two gates claim different steps and do not collide.
    # It runs no go command either, and reads less than the job above: the action, the version
    # it pins, the manifest row the action and the rig both read, the helper that reads it, and
    # the rig and the fault proxy that put the release hosts out of reach.
    gate("tool_fallback", "tool-fallback-arm", reads=reads(
        (".github/actions/setup-bun/**", at("tool-fallback-arm", "./.github/actions/setup-bun")),
        (".bun-version", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)}: the rig holds what bun answers to the "
                         "version that file pins, and .github/actions/setup-bun installs the Bun it pins"),
        (".github/ci-tools.yaml", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)}: the rig reads the "
                                  "bun-linux-aarch64 row's version and digest, as the action reads that row to "
                                  "verify what it installed"),
        (".github/scripts/ci-tools.sh", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)}: the rig reads that row "
                                        "through it, which is the PyYAML read this job proves the arm64 runner can "
                                        "make"),
        (".github/scripts/ci-tools-proof.sh", f"{at('tool-fallback-arm', TOOL_ARM_READ_STEP)} runs it, as the fault "
                                              "and probe steps of this job do"),
        (".github/scripts/ci-fault-proxy.py", f"{at('tool-fallback-arm', TOOL_FAULT_STEP)}: the rig starts it in "
                                              "front of GitHub's release assets"),
        (".github/scripts/disk_watch.py", f"{at('tool-fallback-arm', TOOL_ARM_WATCH_STEP)} runs it, and "
                                          f"{at('tool-fallback-arm', TOOL_REPORT_STEP)} runs it again"),
    )),
] + SCRIPT_TEST_GATES

# One entry per step of ci.yaml that runs a package under a build tag and passes -run: the job
# whose untagged run of that package the -run leans on, which check-ci-filters.sh holds to running
# in every run the tag step runs (check_tag_runs). The note says which step runs the untagged
# tests, the judgement the check cannot derive. The last two entries pair a job with itself: the
# untagged run is a step of the same job, so the filters are the same one and the subset holds
# trivially; they are here all the same, so the rule covers every -run step and a later move of
# one to another job is caught. What the check holds an untagged job's filter to is what the tag
# step's gate reads, never what the tag filter selects, so these two hold their filter to nothing
# its own gate does not read already, and rules 9 and 17 judge it as they judge any other.
TAG_RUNS = [
    TagRun("dispatch_memory", "dispatch-memory", MEMORY_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/dispatch's untagged tests",
           job_file("dispatch-memory")),
    TagRun("dispatch_multiprocess", "dispatch-multiprocess", DISPATCH_MP_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/dispatch's untagged tests",
           job_file("dispatch-multiprocess")),
    TagRun("broker_multiprocess", "broker-multiprocess", BROKER_MP_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/broker's untagged tests",
           job_file("broker-multiprocess")),
    # envoy-go's unit step leaves /cmd/listener$ out, since the lane runs it under the detector.
    TagRun("listener_server_change", "listener-server-change", SOAK_STEP, "envoy_go_race", "envoy-go-race",
           f"envoy-go-race's \"{RACE_JOBS['envoy-go-race'].step}\" runs ./cmd/listener's untagged tests",
           job_file("listener-server-change")),
    TagRun("envoy_go", "envoy-go", NATS_MATRIX_STEP, "envoy_go", "envoy-go",
           f"envoy-go's own \"{UNIT_TEST_STEP}\" runs ./internal/testnats's untagged tests",
           job_file("envoy-go")),
    TagRun("envoy_infra_go", "envoy-infra-go", INFRA_NATSLIVE_STEP, "envoy_infra_go", "envoy-infra-go",
           f"envoy-infra-go's own \"Unit test envoy-infra\" runs {', '.join(INFRA_NATSLIVE_PACKAGES)} untagged",
           job_file("envoy-infra-go")),
]

EXCLUDED = {
    "worker_image": "gates worker-image.yaml's image build; .github/scripts/check-image-trigger-paths.sh proves it covers "
                    "every file the build reads",
    "browser_image": "gates browser-image.yaml's image build; .github/scripts/check-image-trigger-paths.sh proves it covers "
                     "every file the build reads",
}
