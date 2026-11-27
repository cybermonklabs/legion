"""What each ci.yaml job reads that check-ci-filters.sh cannot find by itself, from which the
changes job computes its filters (ci_filter_coverage.py is the engine; check-ci-filters.sh's header
says what is proved). This module keeps the helpers and constants every gate shares and loads the
hand-kept facts from beside their owners (ci_filter_sources.py says how):

  GATES     one Gate per file of .github/ci-gates, `GATE = gate(...)`: the job (or step) a filter
            gates, the go commands the job runs, and the files its other steps read. Each go
            command is pinned to the text that starts it:
            `step=` a step of the job whose run: holds `text`, or `source=` a script that holds it.
            A step whose go commands are not in the simple form is an `exact=` entry instead
            (pinned_step): its whole run: script and directory, and the Runs that stand for what
            it compiles. The check fails when a run: step of a ci.yaml job - or of a job of a
            local workflow that job calls, whose steps it reads the same way - starts a go
            command no Run or exact entry claims, when a pin's text is gone, and when a step an
            exact entry pins runs another script or directory, or under an env, shell, container
            or job that could move what it compiles, naming the step and what differs, so the
            table cannot drift from ci.yaml silently. A claimed command the check does not reduce
            (check-ci-filters.sh's header says which it does) counts as reading every Go file of
            the repository and every file under a Go module's directory; a script's pin is its
            whole line.
  RULES     a source file -> what it does when a test run compiles it: a go command it starts (a
            Run, whose closure is added) or a file it reads outside its package (a Read), from the
            //ci: lines of that file, which sit in one block right after its package clause. A
            _test.go file counts when go list lists it in a tested package under the job's tags;
            any other file when it is compiled into a test run's closure. These are found by
            reading the tests, not by the check: a new read through `..` or a new go command a
            test starts is declared by the change that makes it, in that block.
  EXCLUDED  filters the check leaves to another, each a file of .github/ci-gates holding its
            patterns: worker_image gates worker-image.yaml's image build, whose every input
            .github/scripts/check-image-trigger-paths.sh proves its filter covers. (envoy_smoke's
            image inputs are that check's too, but its job's go commands and other step reads are
            a gate here.)
  PUSH_WORKFLOWS  filter -> a workflow that runs that filter's job's work on a push to main in a
            workflow of its own, since ci.yaml runs on no push, selected by its `on.push.paths`,
            which must equal the filter: the filter decides the pull request's and merge group's
            run, the paths main's.
  TAG_RUNS  one entry per step that runs a package under a build tag and passes -run: the job
            whose untagged run of that package that -run leans on, whose filter must therefore
            cover every file the tag step reads (the read set of the gate on its filter, and that
            filter's listed block).
  READ_SETS the sets a //ci:reads-set line names, each computed when the table is read.

A Read's glob is expanded over the repository's files; a why cites the step (at()) or the
source line that reads it - the line that performs the read, not the one naming its path, and
never a line of the file the why itself lives in, which moves with every line added above it:
inside its own file a why names the function or test that reads, the filter, the anchor, the
glob or the path instead, which a grep resolves. at()'s third argument, and a gate's `workflow`,
is the workflow file
that holds the job's steps, and it is the job's own called file (job_file): ci.yaml's job of
each gated id keeps the gate and calls .github/workflows/ci-job-<id>.yaml, which holds the
steps, or, for a job SHARED_FILE names, the file of the job it maps to. The one
gate whose steps are elsewhere names its file itself: ci.yaml's docs job calls
docs-site.yaml, whose job carries an id of its own, so that gate stays on ci.yaml, the file its
own job id is in, and cites docs-site.yaml's build job (at_docs). The Read lists of the jobs
without Go steps (the Bun packages) are as hand-kept as RULES: the check proves the filter
covers them, not that they are complete.

Until the change that moved them, GATES, RULES and EXCLUDED were literal lists in this module and
the filters a committed file, .github/ci-filters.yaml. A branch made before it keeps them in its
copy of this module when it merges main; .github/scripts/ci-reads-convert moves what the branch
added into the gate files and //ci: lines, and this module refuses to load while it still binds
one of those names.
"""

import dataclasses
import functools
import json
import os
import re
import subprocess
from pathlib import Path
from typing import NamedTuple

from ci_filter_coverage import CACHE_WORKFLOW, TEST_SUFFIX, Read, TagRun, exact, repo_files, run
from ci_filter_coverage import at as at_in, gate as gate_in
from ci_filter_sources import excluded, load_directives, load_gate_files
from go_build_tags import compiled, go_skips_dir, tag_sets
import go_legs

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
BUILD_TAGS = ".github/scripts/go_build_tags.py"


# Each job whose steps are in another job's called file, mapped to that other job: the halves of
# a job that was split so a pull request runs one and the merge queue the other, and envoy-refill,
# a job of its own that shares envoy-e2e-local's file. Two jobs share a file because ci.yaml
# already calls as many reusable workflows as .github/scripts/check-workflow-files.sh allows it
# (.github/ci-notes/ci.md#two-jobs-that-call-one-file), and each job
# of the file runs under its own caller alone (ci_filter_coverage.check_called_jobs).
SHARED_FILE = {
    "tooling-checks": "test",
    "pi-legion-build": "pi-legion",
    "envoy-infra-build": "envoy-infra-go",
    "envoy-refill": "envoy-e2e-local",
}


def job_file(job: str) -> str:
    """The workflow file that holds a job's steps. ci.yaml's job of that id keeps the gate - its
    `needs: changes` and its whole `if:` - and calls .github/workflows/ci-job-<id>.yaml, which
    holds the steps, or the file of the job SHARED_FILE maps it to; so that is where a gate's
    step, an Exact entry's, a TAG_RUNS row's and a why's citation are looked for."""
    return f".github/workflows/ci-job-{SHARED_FILE.get(job, job)}.yaml"


def at(job, step, workflow=""):
    """A citation of a step, in the file that holds it: the job's own called file, unless the
    caller names another (at_docs, for the build job ci.yaml's docs job calls)."""
    return at_in(job, step, workflow or job_file(job))


def gate(filter, job, runs=(), reads=(), step="", exact=(), workflow=""):
    """A gate, whose job holds its steps in its own called file unless another file is named.
    The job is ci.yaml's, which keeps the `if:` the filter gates whichever file holds the
    steps.

    Every such gate reads three files: ci.yaml, where every job's `if:` is (CI_SELF), the file
    job_file names, where the steps it runs are, and its own file of .github/ci-gates, which its
    filter is computed from (ci_filter_sources.load_gate_files adds that one). The first two are
    appended here rather than written out on each gate, so a gate's own `reads` say only what is
    its own. The gates that name a file - docs, dispatch_browser and dispatch_browser_pr, whose
    called workflows carry job ids of their own - declare what they read themselves, CI_SELF among
    it."""
    if not workflow:
        reads = list(reads) + CI_SELF + job_self(job)
    return gate_in(filter, job, runs, reads, step, exact, workflow or job_file(job))


def targets(path: str, key: str) -> list:
    return json.loads(Path(path).read_text())[key]


def step_run(job, step, text, workdir, pkgs, workflow="", **options):
    """A go command a ci.yaml step starts, pinned to the step and its text, in `workflow` when the
    job's steps are not in its own called file (envoy-go-natslive's, in ci-job-envoy-go.yaml)."""
    return run(workdir, pkgs, f"{at(job, step, workflow)}: {text}", step=step, text=text, **options)


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


def darwin_vet_script(workdir):
    """The run of a darwin vet step in `workdir`: the tag sets BUILD_TAGS prints for the module,
    each as a whole `-tags=` flag so no line is empty, taken into a variable first so a failure
    of the script fails the step before go runs, then one vet of every package per set."""
    up = "/".join([".."] * len(Path(workdir).parts))
    return (f"flags=$(python3 {up}/{BUILD_TAGS} . darwin arm64)\n"
            "while IFS= read -r flag; do\n"
            '  echo "Vetting for darwin with $flag"\n'
            '  GOOS=darwin GOARCH=arm64 go vet "$flag" ./...\n'
            'done <<< "$flags"')


def darwin_vet(job, step, workdir):
    """A step that vets every package of a module, its tests and every tagged file included, as
    darwin/arm64 compiles them: the vet each Go module's job runs so that a file compiling on
    linux alone fails the pull request, not a Mac. The step loops over the tag sets BUILD_TAGS
    reads off the module's own build constraints, so a command substitution, a loop and GOOS and
    GOARCH in front of the go command put it outside the simple form: it is pinned whole, with
    one Run per set the script prints for the module now, each carrying the platform and that
    set, which makes the closure the one darwin compiles. check-ci-filters.sh's rule 20 then asks
    go list whether those vets between them pass over any file darwin builds under some tags.

    The gate reads BUILD_TAGS and every .go file of the module, whose constraints it scans;
    darwin_vet_reads names both for the gate's `reads`."""
    return pinned_step(job, step, workdir, darwin_vet_script(workdir), [
        (workdir, ["./..."], f"GOOS=darwin GOARCH=arm64 go vet -tags {','.join(tags) or '(none)'} ./...",
         {"tags": ",".join(tags), "mode": "vet", "env": {"GOOS": "darwin", "GOARCH": "arm64"}})
        for tags in tag_sets(Path(workdir), "darwin", "arm64")
    ])


def darwin_vet_reads(job, step, workdir):
    """What darwin_vet's step reads beside its go command's closure: the script that prints its
    tag sets, and the .go files whose build constraints that script reads."""
    return reads(
        (BUILD_TAGS, f"{at(job, step)} runs it for the vet's tag sets"),
        (f"{workdir}/**/*.go", f"{at(job, step)}: {BUILD_TAGS} reads the build constraint of each"),
    )


# The three darwin vet steps, one per Go module that ships for darwin.
DAEMON_DARWIN_VET = "Vet the Go daemon for darwin, its e2e-tagged live proofs included"
ENVOY_DARWIN_VET = "Vet envoy for darwin, its tagged tests included"
INFRA_DARWIN_VET = "Vet envoy-infra for darwin, its live-test tags included"


def reads(*entries):
    return [Read(glob, why) for glob, why in entries]


def credential_check_reads():
    """The files packages/daemon/internal/textcheck/credential_argument_test.go reads: every one
    git tracks but those under docs/plans and docs/superpowers. A glob cannot leave a tree out, so
    the read is one glob per top-level entry git tracks - `<dir>/**` for a directory, the name for
    a file - with docs/ split the same way one level down, and each glob names a tracked file, as
    rule 9 holds every read to."""
    listed = subprocess.run(["git", "ls-files", "-z"], capture_output=True, check=True).stdout
    globs = set()
    for path in os.fsdecode(listed).split("\0"):
        parts = path.split("/")
        if not path:
            continue
        if parts[0] == "docs" and len(parts) > 1:
            if parts[1] in {"plans", "superpowers"}:
                continue
            globs.add(f"docs/{parts[1]}/**" if len(parts) > 2 else path)
        else:
            globs.add(f"{parts[0]}/**" if len(parts) > 1 else path)
    return sorted(globs)


def files_but_tests(directory, suffix=".go", tree=False, skip=()):
    """The files of `directory` a reader that passes over Go test files reads, one path each: the
    ones whose name ends in `suffix` ("" for every name) but each `_test.go` one, and with `tree`
    those of every directory under it too, but a directory named in `skip` and node_modules, which
    holds no file of the repository. A `<dir>/*` or `<dir>/**` read would count every test file
    of the directory as read, so rule 17 of check-ci-filters.sh would write no `!` pattern for one
    and a change to a test file alone would start the job; no glob leaves the test files out, so
    the files are listed from the repository as it is on each run, and one added later is declared
    with no edit here. The repository is repo_files(): a build output a local run leaves in the
    tree, such as the e2e harness's packages/envoy/out/dispatch-e2e-broker, is in no checkout CI
    makes, and rule 9 would refuse a read of it as matching no file. They come in the order a
    top-down walk visits them: a directory's own files, then each directory under it.
    `directory` is refused unless it is relative and in normal form (os.path.normpath), since
    repo_files() spells every path that way: `./d`, `d/` or an absolute path would match no file
    of it, and the reads would go missing with nothing failing."""
    if os.path.isabs(directory) or directory == "." or os.path.normpath(directory) != directory:
        raise ValueError(f"files_but_tests is given {directory!r}, which is not a path relative to the "
                         f"repository root in normal form, so no file of repo_files() would match it; "
                         f"write it as {os.path.normpath(directory)!r}")
    prefix, pruned = f"{directory}/", {"node_modules", *skip}
    out = []
    for path in repository_files():
        if not path.startswith(prefix):
            continue
        *dirs, name = path[len(prefix):].split("/")
        if dirs and (not tree or pruned.intersection(dirs)):
            continue
        if name.endswith(suffix) and not name.endswith(TEST_SUFFIX):
            out.append(path)
    return sorted(out, key=lambda path: [(1, part) for part in path.split("/")[:-1]] + [(0, path.rsplit("/", 1)[-1])])


@functools.cache
def repository_files():
    """repo_files(), read once: the repository does not change while this module builds its tables,
    and files_but_tests asks for it a dozen times."""
    return tuple(repo_files())


ENVOY_MODULE = "github.com/sjawhar/envoy"
GO_IMPORT = re.compile(r'^\s*(?:import\s+)?(?:[A-Za-z_.][A-Za-z0-9_]*\s+)?"(' + re.escape(ENVOY_MODULE) + r'/[^"]+)"', re.M)
GO_IMPORT_BLOCK = re.compile(r'^import\s*\((.*?)^\)', re.M | re.S)
GO_IMPORT_LINE = re.compile(r'^import\s+(?:[A-Za-z_.][A-Za-z0-9_]*\s+)?"[^"]*"', re.M)


def envoy_import_closure(*directories):
    """The directories of `directories` and of every package of this module their non-test .go
    files import, then those packages' imports in turn, sorted: each a package directory under
    packages/envoy. The imports are read from each file's import declarations as text, whatever
    build constraint the file carries, so the set holds every package a build of any tag reaches."""
    seen, todo = set(), list(directories)
    while todo:
        directory = todo.pop()
        if directory in seen:
            continue
        seen.add(directory)
        for path in files_but_tests(directory):
            text = Path(path).read_text(encoding="utf-8")
            decls = "\n".join([m.group(1) for m in GO_IMPORT_BLOCK.finditer(text)]
                              + [m.group(0) for m in GO_IMPORT_LINE.finditer(text)])
            for imported in GO_IMPORT.findall(decls):
                todo.append(f"{ENVOY}/{imported.removeprefix(ENVOY_MODULE + '/')}")
    return tuple(sorted(seen))


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
        (".bun-version", "`.github/actions/setup-bun/action.yml#bun-version-file`, `.github/actions/setup-bun/action.yml#bun-already-here` and `.github/actions/setup-bun/action.yml#bun-second-source`: "
                         "oven-sh/setup-bun installs the version bun-version-file names, and the steps bun-already-here "
                         "and bun-second-source read the version it pins to tell a Bun of that version from any other"),
        (".github/ci-tools.yaml", "`.github/actions/setup-bun/action.yml#bun-second-source`, `.github/actions/setup-bun/action.yml#ci_tools_fetch_fallback` and `.github/actions/setup-bun/action.yml#ci_tools_verify`: "
                                  "the bun-second-source step chooses the bun row of this runner's build variant, "
                                  "ci_tools_fetch_fallback fetches that row's second source where no Bun of the pinned "
                                  "version is installed, and ci_tools_verify holds the bun on PATH to that row's digest"),
        (".github/scripts/ci-tools.sh", "`.github/actions/setup-bun/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` "
                                        "source it: ci_tools_source_name reads the word for the source that served the bytes, "
                                        "ci_tools_fetch_fallback installs the npm copy and ci_tools_verify checks the installed binary"),
    )


def bun_job(job, workflow=""):
    """setup_bun and a frozen `bun install`. No .npmrc anywhere, and one Bun configuration file,
    packages/dispatch/bunfig.toml, which holds `[test]` alone and so changes no install: an
    install runs at the repository root, and Bun reads bunfig.toml from the working directory."""
    install = at(job, "bun install --frozen-lockfile", workflow)
    return setup_bun(job, workflow) + reads(
        ("package.json", f"{install} reads the root manifest, `package.json#workspaces` and `package.json#patchedDependencies`"),
        ("bun.lock", f"{install} reads the lockfile to install the versions it records"),
        ("patches/**", f"{install} applies the patches `package.json#patchedDependencies` names"),
        ("packages/proof-editor/proof-sdk-*.tgz",
         f"{install} installs proof-sdk-upstream from it, the file: dependency of packages/proof-editor/package.json"),
        *[(f"{w}/package.json", f"{install}: `package.json#workspaces` lists {w}; a frozen install reads every workspace manifest")
          for w in WORKSPACES],
    )


def biome(where):
    return reads(
        ("biome.json", f"{where}: Biome reads the root config"),
        (".gitignore", f"{where}: `biome.json#useIgnoreFile`, in its vcs block, reads the root .gitignore"),
    )


def install_jj(job, workflow=""):
    return reads(
        (".github/actions/install-jj/**", at(job, "./.github/actions/install-jj", workflow)),
        (f"{DAEMON}/docker/worker.Dockerfile", "`.github/actions/install-jj/action.yml#jj_tool` reads ARG JJ_TOOL, the tool spec mise installs"),
        (".github/ci-tools.yaml", "`.github/actions/install-jj/action.yml#ci_tools_fetch` and `.github/actions/install-jj/action.yml#ci_tools_verify`: ci_tools_fetch "
                                  "installs the archive of the mise row, or of mise-aarch64 on an arm64 runner, and "
                                  "ci_tools_verify holds the jj binary to the digest of the jj row or of jj-aarch64, "
                                  "whichever source served it"),
        (".github/scripts/ci-tools.sh", "`.github/actions/install-jj/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` "
                                        "source it: ci_tools_fetch installs mise, ci_tools_fetch_fallback reads the jj row's "
                                        "second source and ci_tools_verify checks the binary"),
    )


def install_omp(job, workflow=""):
    return reads(
        (".github/actions/install-omp/**", at(job, "./.github/actions/install-omp", workflow)),
        (".omp-pin", "`.github/actions/install-omp/action.yml#pin=$(<.omp-pin)`"),
        (f"{DAEMON}/docker/worker.Dockerfile", "`.github/actions/install-omp/action.yml#s/^ARG OMP_SHA256=//p` reads ARG OMP_SHA256, the "
                                               "sha256 the release archive is held to"),
    )


def setup_go(job, gomod, workflow=""):
    return reads((gomod, f"{at(job, 'actions/setup-go', workflow)} go-version-file"))


# Every job reads ci.yaml, where its `if:` is: a change to it reaches every job. The docs and
# dispatch_browser gates add this by hand, since they name the file their steps are in. Each gate
# reads its own file of .github/ci-gates too, which the loader adds (ci_filter_sources.py): the
# job's filter is computed from it, as it was read from the filters file this replaced.
CI_SELF = reads(
    (CI, "a job reads its own definition"),
)


def job_self(job):
    """The file holding a job's steps (job_file), which ci.yaml's job of that id calls: the job
    reads the definition of the steps it runs, as it reads ci.yaml for the `if:` that selects it,
    so each moved job's gate declares this beside CI_SELF and its filter names the file. That is
    what makes a change to one ci-job-<id>.yaml select the jobs whose steps it holds and no
    other."""
    return reads((job_file(job), "the job reads the definition of the steps it runs"))


# ci.yaml's docs job and docs-pages.yaml's build job both call docs-site.yaml, which holds the
# steps. The gate stays on ci.yaml's docs job, whose if: the docs filter gates, and every step
# citation names docs-site.yaml's build job.
DOCS_JOB = "build"
DOCS_SITE_SELF = reads((DOCS_SITE, "the docs build reads the definition of the steps it runs"))


def at_docs(step):
    return at(DOCS_JOB, step, DOCS_SITE)


# ci.yaml's dispatch-browser, dispatch-browser-pr and dispatch-references jobs and
# dispatch-browser-nightly.yaml's own job all call dispatch-browser.yaml, which holds the steps.
# ci.yaml's dispatch-browser and dispatch-browser-pr each keep a gate, whose filter its if: tests
# (dispatch-references' filter is EXCLUDED, so its steps are read under those two), and every step
# citation names the job of dispatch-browser.yaml that holds the step.
BROWSER_JOB = "dispatch-browser"
BROWSER_PR_JOB = "dispatch-browser-pr"
BROWSER_BINARIES, BROWSER_SHARD = "binaries", "shard"
BROWSER_ALONE, BROWSER_MP, BROWSER_MERGE = "alone", "multiprocess", "merge"
BROWSER_REAL, BROWSER_STUB = "real", "stub"
BROWSER_VISUAL = "visual"
BROWSER_SITE_SELF = reads((BROWSER_SITE, "the browser suite reads the definition of the steps it runs"))


def at_browser(step, job=BROWSER_SHARD):
    return at(job, step, BROWSER_SITE)


CONTRACTS_SRC = "packages/contracts/src/**"
ENVOY_CLIENT_SRC = "packages/envoy-client/src/**"
BROKER_CLIENT_SRC = "packages/cipher-client/**"
# The shared test fixtures every other Bun package's tests import.
TEST_FIXTURES_SRC = "packages/test-fixtures/src/**"

# The test job's run of the live proofs' libraries, cited by its whole run: text: the root
# package.json's test:e2e-lib (bun test --timeout 200000 scripts/e2e/lib), which every runner uses.
# It leaves out scripts/e2e/lib/nats-image.test.ts, whose one run is lint's (no filter gates lint),
# so the natsfloor build that file starts is no read of this job.
E2E_LIB_TESTS = "bun run test:e2e-lib --path-ignore-patterns=scripts/e2e/lib/nats-image.test.ts"


def dispatch_api_types(step):
    """What a typecheck that compiles packages/contracts/src/dispatch-api.generated.ts reads in its
    place: git ignores that module, and the job's own `bun install` renders it (the root
    package.json's postinstall runs packages/contracts/scripts/gen-api.ts over the OpenAPI
    document), so a change to either file changes what `step` compiles, and no change carries the
    module itself. ci_ts_listing.py's RENDERED lists the same two files in the module's place, so
    check-ts-filters.sh holds the gate's filter to them."""
    return reads(
        ("packages/contracts/schemas/dispatch-openapi.json",
         f"{step} compiles packages/contracts/src/dispatch-api.generated.ts, which the job's bun install renders "
         "from this document (package.json postinstall)"),
        ("packages/contracts/scripts/gen-api.ts",
         f"the job's bun install runs it (package.json postinstall) to render the module {step} compiles"),
    )


def envoy_client_dep(why):
    """The reads of a gate whose job compiles @legion/envoy-client: that package's sources, and
    packages/cipher-client's with them. envoy-client's src/broker-credential.ts imports
    @legion/cipher-client/environment (brokerTokenClient), /signins (chooseSignIn) and
    /token-client (isFinalRefusal), and that package's exports map every
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
        (BROKER_CLIENT_SRC, f"{why}; and @legion/cipher-client comes with it, through "
                            "packages/envoy-client/src/broker-credential.ts, which imports brokerTokenClient from "
                            "@legion/cipher-client/environment"),
    )


# What packages/claude-envoy/scripts/verify-build.sh and the scripts/build.ts it runs read, which
# is every file the published plugin is built from: the claude-envoy gate, and the tooling-checks
# gate, for two of that job's steps - verify-build, which runs the same command release.yaml's
# claude_envoy_build does, and the build-input-list step
# (packages/claude-envoy/tests/build-inputs.test.ts, which runs the build's two halves itself).
# Those two steps are the whole of what the tooling-checks job does in this package, so a file
# only the claude-envoy job's lint, typecheck or other tests read is in CLAUDE_ENVOY_JOB below,
# not here.
VERIFY_BUILD_STEP = "Build, check and pack the Claude Code plugin as the release does"
BUILD_INPUTS_STEP = "Check the Claude Code plugin's build input list covers what the build reads"
TOOLING_JOB = "tooling-checks"
CLAUDE_ENVOY_BUILD = reads(
        ("packages/claude-envoy/**", f"{at('claude-envoy', 'Lint Claude Envoy plugin')}, typecheck, verify-build, test and pack in "
                                     f"packages/claude-envoy; {at(TOOLING_JOB, VERIFY_BUILD_STEP)} and {at(TOOLING_JOB, BUILD_INPUTS_STEP)} "
                                     "run scripts/verify-build.sh and tests/build-inputs.test.ts of it"),
        (CONTRACTS_SRC, "packages/claude-envoy/tsconfig.json paths @legion/contracts; bundles import it"),
        ("packages/envoy-client/bin/dispatch.ts", "`packages/claude-envoy/scripts/build.ts#bundleFromPackageRoot` and `packages/claude-envoy/scripts/build.ts#BUNDLE_ENTRYPOINTS` "
                                                  "bundles BUNDLE_ENTRYPOINTS' dispatch, which verify-build runs and "
                                                  "`packages/claude-envoy/tests/dist-fresh.test.ts#bin/dispatch is executable and runs a fresh dist/dispatch.js without node_modules` "
                                                  "holds to this path"),
        ("packages/pi-shared/src/**", "`packages/claude-envoy/src/envoy-channel-server.ts#@legion/pi-shared/handoff-record`, "
                                      "`packages/claude-envoy/src/legion-claim.ts#@legion/pi-shared/daemon-client` and `packages/claude-envoy/src/legion-claim.ts#@legion/pi-shared/legion-claim-core` "
                                      "and `packages/claude-envoy/src/legion-tool.ts#@legion/pi-shared/legion-phase-ops` import @legion/pi-shared/*"),
        ("scripts/third-party-notices.ts", "`packages/claude-envoy/scripts/build.ts#thirdPartyNotices` imports it"),
        ("scripts/pi-plugin-prepack.sh", "`packages/claude-envoy/scripts/build.ts#buildLegionPlugin` runs it with --stage-skills @sjawhar/pi-legion"),
        ("packages/pi-legion/agents/**", "`packages/claude-envoy/scripts/build.ts#buildLegionPlugin` reads each agent of it"),
        ("skills/**", "`packages/claude-envoy/scripts/build.ts#buildLegionPlugin` stages pi-legion's partition of it; packages/claude-envoy/skills/* symlinks into ../../../skills"),
        (".bun-version", "`packages/claude-envoy/scripts/build.ts#pinnedBunVersion` reads the Bun pin"),
        (f"{FIX}/daemon-api/**", "`packages/claude-envoy/scripts/build.ts#pluginManifest` writes this tree's contract into the "
                                 "manifest it builds and its bundles inline, `packages/contracts/src/daemon-contract.ts#daemonContract` of the "
                                 "committed manifest, which names none, for which `packages/contracts/src/daemon-contract.ts#lastReleased` "
                                 "reads released-contracts.json and `packages/contracts/src/daemon-contract.ts#entryFixtures` lists "
                                 "contracts/*.json; the glob is the directory's, since contracts/ holds no file until the "
                                 "first entry lands, and a glob of no file is refused"),
        (".github/scripts/release-push.sh", "packages/claude-envoy/scripts/verify-build.sh runs its check-built; the claude-envoy job runs its extract-built"),
        (".github/workflows/release.yaml", "`packages/claude-envoy/tests/build-inputs.test.ts#.github/workflows/release.yaml's $CLAUDE_ENVOY_VERSION_ARGS carries the build inputs` "
                                           "holds its $CLAUDE_ENVOY_VERSION_ARGS equal to scripts/build-inputs.txt"),
) + envoy_client_dep("`packages/claude-envoy/src/envoy-channel-server.ts#@legion/envoy-client/transport` and `packages/claude-envoy/hooks/session-hook.ts#@legion/envoy-client/dispatch-http` import @legion/envoy-client/*")

# The build above, and what only the claude-envoy job reads: the Claude Code its live answer test
# runs, and the fixtures and modules of its other tests. The test job runs none of them.
CLAUDE_ENVOY_JOB = CLAUDE_ENVOY_BUILD + reads(
        (".github/actions/install-claude-code/**", at("claude-envoy", "./.github/actions/install-claude-code")),
        (".claude-code-pin", "`.github/actions/install-claude-code/action.yml#pin=$(<.claude-code-pin)`, the release "
                             "`packages/claude-envoy/tests/answers-claude.test.ts#print` and `packages/claude-envoy/tests/answers-claude.test.ts#streamSession` runs"),
        ("packages/envoy-client/test/**", "`packages/claude-envoy/tests/envoy-channel-stall.test.ts#@legion/envoy-client/test/envoy-stack` and tests/agent-stream-reply-grant.test.ts import modules of that directory, and tsc puts them in the claude-envoy program"),
        ("packages/pi-shared/test/image-manifest.ts", "`packages/claude-envoy/tests/legion-tool.test.ts#withImageManifest` imports it from @legion/pi-shared/test/image-manifest"),
        (f"{FIX}/daemon-api/**", "`packages/claude-envoy/tests/legion-claim.test.ts#daemonFixture`, `packages/claude-envoy/tests/legion-pane-mcp.test.ts#daemonFixture` "
                                 "and `packages/claude-envoy/tests/legion-tool.test.ts#daemonFixture` read its responses; "
                                 "`packages/claude-envoy/tests/daemon-api-version.test.ts#package.json names no daemon API contract, and the plugin speaks this tree's` "
                                 "reads released-contracts.json and lists contracts/*.json; and `packages/contracts/src/daemon-contract.ts#lastReleased` "
                                 "and `packages/contracts/src/daemon-contract.ts#entryFixtures` read and list the same two for "
                                 "`packages/contracts/src/daemon-contract.ts#daemonContract` of the committed manifest, as "
                                 "`packages/claude-envoy/src/legion-claim.ts#PLUGIN_CONTRACT` and `packages/pi-shared/src/legion-phase-ops.ts#pluginBuildOf` "
                                 "for `packages/claude-envoy/src/legion-tool.ts#BUILD` take it when a test imports them, and as the "
                                 "tests' own PLUGIN_CONTRACT takes it"),
        (f"{FIX}/envelope-without-role.json", "`packages/claude-envoy/tests/envelope-role-field.test.ts#envelopeWithoutRole` imports it"),
        ("packages/pi-shared/test/daemon-state.ts", f"{at('claude-envoy', 'Typecheck Claude Envoy plugin')}: `packages/claude-envoy/tests/legion-tool.test.ts#stateAtPhase` imports it from @legion/pi-shared/test/daemon-state, so tsc compiles it in that program"),
        (".agents/plugins/marketplace.json", "`packages/claude-envoy/tests/envoy-channel-server.test.ts#codexMarketplace` imports it"),
        (".claude-plugin/marketplace.json", "`packages/claude-envoy/tests/envoy-channel-server.test.ts#marketplace` imports it"),
        (".github/scripts/release-claude-envoy-check-packed.sh", "packages/claude-envoy/tests/packed-skills.test.ts's test \"the release's "
                                                                 "check of the packed tarball passes a listing longer than a pipe holds\" "
                                                                 "copies it into a root of its own and runs it over a long listing"),
        (".github/scripts/release-codex-envoy-check-packed.sh", "packages/claude-envoy/tests/codex-packed.test.ts's tests \"the release's "
                                                                "check passes the tarball the build packed\" and the five that refuse a "
                                                                "copy of it copy it into a root of their own and run it"),
        (".github/scripts/release-claude-envoy-manifest-repository.sh", "packages/claude-envoy/tests/release-manifest-repository.test.ts's "
                                                                        "tests \"the publishing repository's url is written into both "
                                                                        "manifests\" and \"a manifest jq cannot parse fails the step before "
                                                                        "the next manifest is written\" copy it into a root of their own and "
                                                                        "run it"),
        (".github/scripts/release-claude-envoy-version.sh", "packages/claude-envoy/tests/release-version.test.ts's tests \"the release's "
                                                            "version is written into all five manifests\" and \"a manifest jq cannot "
                                                            "parse fails the step before the next manifest is written\" copy it into a "
                                                            "root of their own and run it"),
        ("scripts/daemon-contract.sh", "packages/claude-envoy/tests/release-version.test.ts's releaseRoot copies it beside "
                                       ".github/scripts/release-claude-envoy-version.sh, which runs it in the test \"the "
                                       "release's version is written into all five manifests\""),
        (".github/scripts/repo-paths.sh", "packages/claude-envoy/tests/packed-skills.test.ts's test \"the release's check of the "
                                          "packed tarball passes a listing longer than a pipe holds\" runs it for the repository "
                                          "url, and copies it beside the release's check, which runs it, and release-manifest-repository.test.ts "
                                          "and codex-packed.test.ts copy it beside the scripts that run it"),
        (".github/actions/private-names-packed-plugin/**",
         at("private-names", "./.github/actions/private-names-packed-plugin", job_file("claude-envoy"))),
        (".github/scripts/check-private-names.sh", ".github/actions/private-names-packed-plugin/action.yml runs it, "
                                                   "called by " + at("private-names", "./.github/actions/private-names-packed-plugin",
                                                                     job_file("claude-envoy"))),
        (f"{FIX}/session-grant.json", "packages/claude-envoy/tests/agent-stream-reply-grant.test.ts runs sessionGrantConfig, and "
                                      "`packages/envoy-client/test/nats-server.ts#sessionGrantConfig` and `packages/envoy-client/test/nats-server.ts#sessionGrantFixture` builds that grant "
                                      "from this file, which it imports as sessionGrantFixture"),
)


# ---------------------------------------------------------------- shared read sets
LEGION_BUILD = "./cmd/legion"
PACK_READS = [  # bun pm pack of pi-envoy / pi-legion runs their prepack (scripts/pi-plugin-prepack.sh)
    "packages/pi-envoy/**", "packages/pi-legion/**", "packages/pi-shared/**", "packages/envoy-client/**",
    "packages/contracts/**", "scripts/pi-plugin-prepack.sh", "scripts/third-party-notices.ts", "skills/**",
    "package.json", "bun.lock", "patches/**",
]
# broker-refgen's tests parse the broker's packages through
# `packages/envoy/cmd/cipher-refgen/source.go#parseDir`, which reads the non-test .go files of one directory and
# none under it. `packages/envoy/cmd/cipher-refgen/pages.go#readAPI` parses the API package
# (`packages/envoy/cmd/cipher-refgen/routes.go#newGraph`), the approval page's through the pageAnswerers call
# its routes make (`packages/envoy/cmd/cipher-refgen/routes.go#resolve`), and each package a route's refusal
# takes a sentinel error's message from (`packages/envoy/cmd/cipher-refgen/routes.go#sentinel`);
# `packages/envoy/cmd/cipher-refgen/pages.go#apiPage` parses the packages declaring the struct types of the
# bodies it documents (`packages/envoy/cmd/cipher-refgen/bodies.go#structOf`). refgen finds a
# sentinel's or a body's package from the import of the file naming it
# (`packages/envoy/cmd/cipher-refgen/source.go#importDir`), an import of this module, so every package it can
# parse beside those two is in their import closure within the module: REFGEN_API is that
# closure, read from the imports of the non-test files at each run (envoy_import_closure), so a
# route that names one more package's error or type is declared with no edit here. The
# `//ci:reads-set refgen_api` lines of cmd/cipher-refgen's tests name it (READ_SETS).
REFGEN_API = envoy_import_closure(f"{ENVOY}/internal/cipher/api", f"{ENVOY}/internal/cipher/approval")


# ---------------------------------------------------------------- GATES
NOTICES_STEP = "Check every Go release archive's third-party notices can be written and the cross-built commands compile"


def go_notices_runs():
    """Every go command .github/scripts/go-release-notices.sh starts, one per release target of
    each list it reads. The lines are the script's, not the step's: lint's notices step runs the
    script, and ci-caches.yaml's notices job runs the same one to fill the cache lint restores."""
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
        out.append(script_run(NOTICES_LOOPS, '../../scripts/go-third-party-notices.sh "$notices/agent-secrets-$arch" ./cmd/cipher ./cmd/cipher-helper',
                              started_by, ENVOY, ["./cmd/cipher", "./cmd/cipher-helper"], mode="build",
                              env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch}))
    for target in targets(CLIENT_TARGETS, "agent-secrets-client"):
        goos, goarch = target.split("/")
        out.append(script_run(NOTICES_LOOPS, f"go -C {ENVOY} build -buildvcs=false -o /dev/null ./cmd/cipher",
                              started_by, ENVOY, ["./cmd/cipher"], mode="build",
                              env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    for target in targets(INFRA_TARGETS, "envoy-infra"):
        goos, goarch = target.split("/")
        out.append(script_run(NOTICES_LOOPS, f"go -C {INFRA} build -buildvcs=false -o /dev/null ./cmd/...",
                              started_by, INFRA, ["./cmd/..."], mode="build",
                              env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    return out


# The steps whose go commands are not in the simple form, pinned whole: envoy-go-build's
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
go test -count=1 -timeout 5m -tags natslive -run "$cap" -v ./internal/bus/
callout=$(../../{TAGGED_TESTS} natslive ./internal/cipher/natscallout)
go test -count=1 -timeout 5m -tags natslive -run "$callout" -v ./internal/cipher/natscallout/
grant=$(../../{TAGGED_TESTS} natslive ./natsgrant)
go test -count=1 -timeout 5m -tags natslive -run "$grant" -v ./natsgrant/
edge=$(../../{TAGGED_TESTS} natslive ./cmd/listener)
go test -count=1 -timeout 10m -tags natslive -run "$edge" -v ./cmd/listener/"""
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
# The two-build job's Go step, which runs only what the twobuild tag adds, as the two above do:
# envoy-go's unit step runs ./cmd/dispatch's untagged tests, so a run of them here would be a
# second one. The step is a gate of its own (dispatch_two_builds_go), so the TAG_RUNS pairing
# below holds envoy_go to this step's reads and not to the browser half's.
TWO_BUILDS_JOB = "dispatch-two-builds"
TWO_BUILDS_STEP = "Two-build tests of the Dispatch server"
TWO_BUILDS_SCRIPT = f"""\
pattern=$(../../{TAGGED_TESTS} twobuild ./cmd/dispatch)
go test -tags twobuild -count=1 -timeout 30m -run "$pattern" ./cmd/dispatch"""
# The same suite in the same file's dispatch-two-builds-same job, with this tree's own build as
# the previous build, which a merge-group run alone runs (that job's comment says why). Its gates
# and its TAG_RUNS entry are the Go step's.
TWO_BUILDS_SAME_JOB = "dispatch-two-builds-same"
TWO_BUILDS_SAME_STEP = "Two-build tests of the Dispatch server against its own build"
BROKER_MP_STEP = "Two-process tests of the secrets broker"
BROKER_MP_SCRIPT = f"""\
pattern=$(../../{TAGGED_TESTS} multiprocess ./cmd/cipherd)
go test -tags multiprocess -count=1 -timeout 20m -v -run "$pattern" ./cmd/cipherd"""
SOAK_STEP = "Server-change soak of the listener"
SOAK_SCRIPT = f"""\
pattern=$(../../{TAGGED_TESTS} serverchange ./cmd/listener)
go test -tags serverchange -count=1 -timeout 15m -v -run "$pattern" ./cmd/listener"""
# The airlock-lean job's two runs of the model comparison. `-run`, `-fuzz` and `-fuzztime` are no
# flags the simple form reduces, so each step is pinned whole; they move no closure, since both
# runs compile the proxyrules package's tests, whichever cases they then drive.
AIRLOCK_JOB = "airlock-lean"
AIRLOCK_LEAN = f"{ENVOY}/internal/cipher/proxyrules/lean"
AIRLOCK_PKGS = ["./internal/cipher/proxyrules/"]
AIRLOCK_SEED_STEP = "Compare the Go airlock with the model over the seed corpus"
AIRLOCK_SEED_SCRIPT = "go test -v -run '^(FuzzAirlock|TestModelAgreesOnHostShapes|TestTheModelRefusesAHostlessEntry)$' ./internal/cipher/proxyrules/"
AIRLOCK_FUZZ_STEP = "Fuzz the Go airlock against the model"
AIRLOCK_FUZZ_SCRIPT = "go test -fuzz FuzzAirlock -fuzztime=120s -run FuzzAirlock ./internal/cipher/proxyrules/"
AIRLOCK_BUILD_STEP = "Build the Lean model"
AIRLOCK_AXIOMS_STEP = "Print the theorems' axioms"
AIRLOCK_AXIOMS_SCRIPT = f"{ENVOY}/scripts/airlock-lean-axioms.sh"
# Go's cleanup detector: at each collection the runtime reports a cleanup or a finalizer whose
# closure or argument reaches the object it is attached to. internal/dispatch/docs is the only
# package of the repository that registers a cleanup (runtime.AddCleanup in roomcursor.go and
# tree.go), so every run of its tests runs under it: envoy-go-race-docs' race run below, and the
# plain run of what the race build cannot run (race_impossible_script), and no job runs the
# package a third time for the detector alone.
CLEANUP_DETECTOR = "GODEBUG=checkfinalizers=1"
CLEANUP_DETECTOR_PACKAGES = ("internal/dispatch/docs",)
# The document service's race step runs under .github/scripts/race_memory.py, which fails it with
# a log when the race binary's memory passes RACE_MEM_LIMIT_KB (the step's comment in
# ci-job-envoy-go-race-docs.yaml says why), and under the cleanup detector, so its line is shell
# assignments and a script, not a go command the simple form reduces. GODEBUG is a GO* name,
# ---------------------------------------------------------------- a job whose steps run in legs
# .github/go-legs.yaml lists the packages each leg of a Go job runs, and, for a package too long
# for one leg, the group of its tests each leg runs; .github/scripts/go_legs.py prints them for
# the leg, holding the file to `go list ./...` and the package's own test listing first.
# why: .github/ci-notes/ci-job-daemon-go.md#why-the-daemons-tests-run-in-legs
GO_LEGS = go_legs.LEGS
GO_LEGS_SCRIPT = ".github/scripts/go_legs.py"
GO_LEGS_GROUPS = go_legs.GROUPS


def legs_script(module, job, command):
    """The whole run: of a legs job's go test step: its leg's packages, piped from go_legs.py into
    `command` (a go test, or a script that runs one). go_legs.py takes the leg's number and how
    many legs the matrix runs, and refuses a file whose legs are not those or a leg with no
    package. It also takes the command's own build flags (-race, -tags), under which it lists the
    tests of a package a leg runs a group of, so the listing is the build the run then reuses and
    names the tests the run can choose from. `set -o pipefail` is what fails the step when
    go_legs.py refuses the file or go list fails, and `xargs -r` runs nothing on the empty output a
    refusal leaves, where a plain xargs would run the command once with no package (for the skip
    counter, a run of its whole default table) before the pipeline's status failed the step."""
    words = command.split()
    # `--race` is how packages/envoy/scripts/database-skips.sh takes the same flag, and a leg
    # piped into that counter builds under the detector as surely as one piped into go test.
    build = "".join(" -race" if word in ("-race", "--race") else f" {word}"
                    for word in words if word in ("-race", "--race") or word.startswith("-tags="))
    build += "".join(f" -tags={words[i + 1]}" for i, word in enumerate(words[:-1]) if word == "-tags")
    up = "/".join([".."] * len(Path(module).parts))
    return ("set -o pipefail\n"
            f"python3 {up}/{GO_LEGS_SCRIPT} {module} {job} ${{{{ matrix.leg }}}} ${{{{ strategy.job-total }}}}{build} "
            f"| xargs -r {command}")


# which is outside ENV_ALLOWED wherever it is set, so the step is pinned whole.
RACE_DOCS_STEP = "Race-test the document service"
# The document service's legs: as the API's, with the memory sampler and the cleanup detector the
# one job carried. xargs runs a program and not a shell, so `env` is what carries the two
# variables to the sampler.
RACE_DOCS_LEGS_JOB = "envoy-go-race-docs-legs"
RACE_DOCS_SCRIPT = legs_script(ENVOY, "envoy-go-race-docs",
                               f"env {CLEANUP_DETECTOR} RACE_MEM_LIMIT_KB=8388608 "
                               "../../.github/scripts/race_memory.py "
                               "go test -json -race -count=1 -timeout 60m")
# Every lane whose tests start a Dispatch process checks its Postgres service's max_connections in
# its CEILING_STEP, which runs .github/scripts/pg-connection-ceiling.sh. The check reads the step's
# run: line, not what the script runs, so each such gate declares the script, and the go run on the
# script's line CEILING_GO_RUN, whose closure is internal/dispatch/store/cmd/connections,
# internal/dispatch/testlimits and the store.
CEILING = ".github/scripts/pg-connection-ceiling.sh"
CEILING_STEP = "Check the service allows this lane's Dispatch processes"
CEILING_GO_RUN = 'server=$(cd -- "$repo_root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections)'


def ceiling_run(job, workflow=""):
    """The go run pg-connection-ceiling.sh starts in `job`'s ceiling step, in `workflow` when the
    job's steps are not in its own called file (daemon-go-race's, in ci-job-daemon-go.yaml)."""
    return script_run(CEILING, CEILING_GO_RUN, f"run by {at(job, CEILING_STEP, workflow)}", ENVOY,
                      ["./internal/dispatch/store/cmd/connections"], mode="build")


def ceiling_reads(job, workflow=""):
    """pg-connection-ceiling.sh itself, which `job`'s ceiling step runs."""
    return reads((CEILING, f"{at(job, CEILING_STEP, workflow)} runs it"))


# Every job with a Postgres service starts it on the command line
# .github/scripts/postgres-service-settings.sh prints and reads the settings back with that script
# in its SERVICE_SETTINGS_STEP, before its tests (.github/scripts/check-workflow-files.sh refuses a
# service or a job that does not). The check reads the step's run: line, not what the script runs,
# so each such gate declares the script.
SERVICE_SETTINGS = ".github/scripts/postgres-service-settings.sh"
SERVICE_SETTINGS_STEP = "Read the service's settings back"


def service_settings_reads(job, workflow="", step=SERVICE_SETTINGS_STEP):
    """postgres-service-settings.sh itself, which `job`'s read-back step runs."""
    return reads((SERVICE_SETTINGS, f"{at(job, step, workflow)} runs it"))


# The forced-order steps run packages/envoy/scripts/forced-order.sh, whose go test builds each
# package's TestForcedOrder tests under -tags odatrace and -race against a copy of nats-server
# that the script hooks with internal/testnats/testdata/hooked/v<version>, and replaces through a
# copy of the workspace file or of the module's go.mod: a replacement outside the repository, of a
# third-party module, so the first-party closure is the plain one under those tags. The check reads the step's run: line,
# not what the script runs, so each such gate declares the script and the hooks it applies, and
# the go test on the script's line FORCED_ORDER_GO_TEST, pinned as a Run per step.
FORCED_ORDER = f"{ENVOY}/scripts/forced-order.sh"
FORCED_ORDER_HOOKS = f"{ENVOY}/internal/testnats/testdata/hooked/**"
FORCED_ORDER_GO_TEST = ('"${go_cmd[@]}" test "${flags[@]}" -count=1 -v -timeout "${GO_TEST_TIMEOUT:-10m}" '
                        "-run '^TestForcedOrder' \"$@\"")
# The envoy steps, one per key, each with the packages it runs: one on the nats-server the
# workspace links ("", no -server, so a go.mod that moves to a version with no hooks fails the
# step), and one on 2.10.29. cmd/listener's forced-order row holds a point v2.10.29 has not
# (internal/testnats/hooks.go's missing: its purge takes the lock its update holds, so the order
# cannot happen there), where it would skip, and a package whose every forced-order test skipped
# fails forced-order.sh, so that package runs on the linked server alone.
ENVOY_FORCED_ORDER_PKGS = {
    "": ["./internal/testnats/hooktest/", "./cmd/listener/"],
    "2.10.29": ["./internal/testnats/hooktest/"],
}
DAEMON_FORCED_ORDER_STEP = "Forced-order tests of the daemon on the hooked nats-server the module links"
DAEMON_FORCED_ORDER_PKGS = ["./internal/testnats/"]
# The script's own test, envoy-go-race's step before the two above: packages/envoy/scripts/
# forced-order.test.sh runs forced-order.sh in five cases, and the one run that gets as far as a
# go test is over its fixture package, hooktest/skiponly, on the linked version; the other four
# are refused before anything is built.
FORCED_ORDER_TEST = f"{ENVOY}/scripts/forced-order.test.sh"
ENVOY_FORCED_ORDER_TEST_STEP = "Test forced-order.sh's refusals"
ENVOY_FORCED_ORDER_TEST_PKGS = ["./internal/testnats/hooktest/skiponly/"]


def envoy_forced_order_step(server):
    """The envoy-go-race step that runs the forced-order tests on nats-server `server`, or, for "",
    on the version the workspace links."""
    return f"Forced-order tests on the hooked nats-server v{server}" if server else \
        "Forced-order tests on the hooked nats-server the module links"


def forced_order_run(job, step, workdir, pkgs, workflow=""):
    """The go test forced-order.sh starts in `job`'s `step`, over `pkgs` of the module at `workdir`,
    in `workflow` when the job's steps are not in its own called file (daemon-go-legs', in
    ci-job-daemon-go.yaml)."""
    return script_run(FORCED_ORDER, FORCED_ORDER_GO_TEST, f"run by {at(job, step, workflow)}", workdir, pkgs,
                      tags="odatrace", race=True)


def forced_order_reads(job, step, workflow=""):
    """forced-order.sh itself and the hooks it applies, which `job`'s `step` reads."""
    return reads((FORCED_ORDER, f"{at(job, step, workflow)} runs it"),
                 (FORCED_ORDER_HOOKS, f"{FORCED_ORDER}, run by {at(job, step, workflow)}, patches nats-server with them"))


def two_builds_go_step():
    """The two-build job's Go step, pinned whole, since its -run is outside the simple form. Both
    of that job's gates carry it: dispatch_two_builds_go, the step's own gate, and
    dispatch_two_builds, since the job must be selected for the step to run in it."""
    return pinned_step(TWO_BUILDS_JOB, TWO_BUILDS_STEP, ENVOY, TWO_BUILDS_SCRIPT, [
        (ENVOY, ["./cmd/dispatch"], "go test -tags twobuild ./cmd/dispatch", {"tags": "twobuild"}),
    ])


def two_builds_same_step():
    """The same-build job's Go step, pinned whole for the Go step's reason, in both of the Go
    step's gates for the same reason."""
    return pinned_step(TWO_BUILDS_SAME_JOB, TWO_BUILDS_SAME_STEP, ENVOY, TWO_BUILDS_SCRIPT, [
        (ENVOY, ["./cmd/dispatch"], "go test -tags twobuild ./cmd/dispatch", {"tags": "twobuild"}),
    ], workflow=job_file(TWO_BUILDS_JOB))


def two_builds_go_reads():
    """What the two-build job's Go step reads beyond its closure and the //ci:reads lines of the
    twobuild files: the script its -run comes from, the script that wrote the previous build it
    runs against, the checks of the Postgres it runs on, and the Go and the Bun install its CLI
    test runs this tree's dispatch command with."""
    return reads(
        (TAGGED_TESTS, f"{at(TWO_BUILDS_JOB, TWO_BUILDS_STEP)} takes its -run from it"),
        (f"{ENVOY}/scripts/build-dispatch-build.sh",
         f"{at(TWO_BUILDS_JOB, 'Build the previous build')} writes the previous build "
         f"{at(TWO_BUILDS_JOB, TWO_BUILDS_STEP)} runs against (DISPATCH_PREVIOUS_BUILD), whose parts "
         "cmd/dispatch/twobuild_harness_test.go checks, and "
         f"{at(TWO_BUILDS_SAME_JOB, 'Build this tree as the previous build', job_file(TWO_BUILDS_JOB))} "
         "writes the one its own Go step runs against"),
    ) + ceiling_reads(TWO_BUILDS_JOB) + service_settings_reads(TWO_BUILDS_JOB) + setup_go(TWO_BUILDS_JOB, f"{ENVOY}/go.mod") + bun_job(TWO_BUILDS_JOB)


# ---------------------------------------------------------------- reading a Go file as text
# The lanes below read Go source as text rather than with a toolchain's listing of each step's
# build: the holds of PLUGIN_PACK_TESTS, SUPERVISE_TABLE_TESTS, WEB_FIXTURE_TESTS and
# UNIT_ALONE_TESTS read the tests a package declares (declared_tests), and the envoy lane finds the
# tests only a plain run covers (race_impossible_tests_of). Each looks for an identifier the
# compiler reads, so each blanks what it does not read first. Which files the four holds read is
# no reading of theirs: `go list -find` says which test files a runner's build compiles
# (declared_tests, through go_build_tags.compiled), so Go's own constraint grammar, `// +build`
# lines, file-name suffixes and cgo decide it.
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
# A file's `//go:build` line, read as text: the envoy lane's derivation of what only a plain run
# covers (race_impossible_tests_of) reads whether it names race, and refuses a form it does not
# read. It is no answer to which files a build compiles - declared_tests asks go list that - so it
# reads neither `// +build` lines nor the file's name, and would read such a line inside a raw
# string too.
GO_BUILD_LINE = re.compile(r"^//go:build +(.+?) *$", re.M)
# The build a CI runner's go test compiles with no -tags and no -race: GOOS linux, cgo on (a native
# build on a GitHub runner, which has a C compiler) and the runner's GOARCH, which a caller names.
RUNNER_GOOS = "linux"
AMD64 = "amd64"  # ubuntu-24.04's GOARCH
ARM64 = "arm64"  # ubuntu-24.04-arm's GOARCH


def declared_tests(module, directories, names, arches, always=()) -> dict:
    """The tests each _test.go file of `directories` (each a package directory of `module`)
    declares that a runner of each GOARCH of `arches` compiles: per file, as a posix path, the
    GO_TEST_FUNC names of its code (go_code_only), TestMain apart. Which files a runner compiles is
    `go list -e -find` under GOOS linux, that GOARCH, CGO_ENABLED=1, no -tags and GOPROXY=off
    (go_build_tags.compiled), one listing per GOARCH over every directory, so a `//go:build linux`
    file and an `x_linux_test.go` count, and a `//go:build natslive` file, a `// +build darwin`
    file and an `x_darwin_test.go` do not; a file compiled on one GOARCH of `arches` and not on
    another is read as compiled on none. A file is read only when its text writes one of `names`,
    or `always` names it, since go_code_only walks a file character by character; a file the
    runners do not compile is read as declaring none. The one reader the holds below use."""
    directories = [Path(directory) for directory in directories]
    built = None
    for arch in arches:
        files = compiled(Path(module), RUNNER_GOOS, arch, [], directories, CGO_ENABLED="1", GOFLAGS="")
        built = files if built is None else built & files
    found = {}
    for directory in directories:
        for path in sorted(directory.glob("*_test.go")):
            here = path.as_posix()
            text = path.read_text(errors="replace")
            if here not in always and not any(name in text for name in names):
                continue
            found[here] = [] if str(path.resolve()) not in built else [
                m.group(1) for m in GO_TEST_FUNC.finditer(go_code_only(text)) if m.group(1) != "TestMain"]
    return found


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


def packages_kept(root: str, pattern: str, under: str = ".") -> list:
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
    # A step that lists a subtree rather than the module (`go list ./<under>/... | grep -Ev`,
    # database-skips.sh's ./internal/cipher/...) passes it as `under`; "." is the module's ./....
    start = base / under
    module = next(line.split()[1] for line in (base / "go.mod").read_text().splitlines()
                  if line.startswith("module "))
    dropped = []
    for current, subdirs, names in os.walk(start):
        here = Path(current)
        subdirs[:] = sorted(s for s in subdirs if not go_skips_dir(here, s))
        rel = here.relative_to(base).as_posix()
        if any(n.endswith(".go") for n in names) and re.search(pattern, module if rel == "." else f"{module}/{rel}"):
            dropped.append(here)
    if not dropped:
        raise ValueError(f"`grep -Ev '{pattern}'` takes no package of {root} out of `go list ./{under}/...`, so no "
                         "lane is leaving one out; follow the package, or drop the grep")
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

    walk(start)
    return found


# envoy-infra's plain run, which leaves the text checks out the way the other two lanes leave
# theirs out: `go test ./...` cannot name "every package but one", so the step's script lists the
# module's packages and drops that one before go test sees them. It runs under the race detector:
# no test file of the module carries a race build constraint, so the race build runs every test
# the plain one would. The step runs INFRA_RACE_UNIT_SCRIPT in its run mode, and
# ci-caches.yaml's envoy-infra fill runs it in its compile mode (ci_workflow_rules.py's
# check_race_fill_scripts), so the two take one package list and one -race from one file.
INFRA_PLAIN_STEP = "Unit test envoy-infra"
INFRA_PLAIN_PATTERN = f"/{TEXT_CHECK_PACKAGE}$"
INFRA_RACE_UNIT_SCRIPT = f"{INFRA}/scripts/race-unit.sh"
INFRA_PLAIN_SCRIPT = "scripts/race-unit.sh run"
# ci-caches.yaml's fills that compile a race step's script, each (fill job, script, the directory
# the fill runs it in, the one line it runs it with): ci_workflow_rules.py's check_race_fill_scripts
# holds each job to that line and to no race build of its own.
RACE_FILL_SCRIPTS = [("envoy-infra", INFRA_RACE_UNIT_SCRIPT, INFRA, "scripts/race-unit.sh compile")]


def hold_infra_race_unit_script():
    """Refuse a race-unit.sh whose EXCLUDE is not INFRA_PLAIN_PATTERN. The step's Run below claims
    the packages that pattern keeps, so a script dropping another package would leave the claim
    naming a package the step no longer runs, with every check green."""
    text = Path(INFRA_RACE_UNIT_SCRIPT).read_text()
    found = re.findall(r"^EXCLUDE='(.*)'$", text, re.M)
    if found != [INFRA_PLAIN_PATTERN]:
        raise ValueError(f"{INFRA_RACE_UNIT_SCRIPT}'s EXCLUDE is {found}, and INFRA_PLAIN_PATTERN here is "
                         f"'{INFRA_PLAIN_PATTERN}': make them one pattern, the packages envoy-infra-build's unit "
                         f"step leaves to the text-check job")


hold_infra_race_unit_script()


# The five packages daemon-go's race step runs, and which its plain step therefore leaves out, each
# for state its product code shares across goroutines. internal/daemon: the outbox's lane registry,
# lease renewal and held clock, which share state with the intake's commit hooks and the API.
# internal/sessionproxy, and internal/launcher, which wires it: the accept loop, copy pairs, pair
# tracking and bounded drain. internal/intake: the consumers' goroutines (Run's, one per durable
# consumer, and Lag's, one per consumer read) and the lock they take (Consumers.mu). Its consumers
# share one handle on the notification stream, and nats.go's Info writes a handle's cached info
# under no lock while Lag's read of the oldest waiting message (GetMsg) reads it, so each read of
# the stream's info, DispatchTarget's and createConsumer's, goes through a handle of its own. That
# race is what this list now guards: under the detector,
# TestEachReadOfTheStreamsInfoBesideALagReadGoesThroughAHandleOfItsOwn fails when either read is
# put back on the shared handle, and a run without the detector passes it.
# internal/shimwire: the shim's two pumps, the goroutines internal/shim reads the daemon's stream
# and the wrapped process's stdout on, share the dedupe's delivery record under Dedupe.mu, and a
# Writer writes each frame whole under a lock of its own (Writer.mu). Under the detector,
# TestTheDedupeIsSafeForTheShimsTwoPumps fails when the dedupe's lock is taken out, and a run
# without the detector passes it.
# One list: every daemon-go step text below is built from it, and check-ci-filters.sh fails when a
# step in ci-job-daemon-go.yaml differs from the text built here, so the steps cannot name different
# sets. internal/record is not among them: its product code starts no goroutine and uses no sync,
# so the detector has nothing of the package to find there. Its own tests do start goroutines, and
# the cover the detector gave those is what this trade gives up. The plain steps leave out one more
# package than the race steps run, the text checks, which are the text-check job's.
DAEMON_RACE_PACKAGES = ["internal/daemon", "internal/sessionproxy", "internal/launcher", "internal/intake",
                        "internal/shimwire"]
# The job of ci-caches.yaml whose race build fills the cache the race step restores, and which
# ci_workflow_rules.py's check_race_fill_packages holds to that list: a race build of any other
# job of that file fills another family.
DAEMON_RACE_FILL_JOB = "daemon"
DAEMON_RACE_STEP = "Test the concurrent packages under the race detector"
DAEMON_PLAIN_STEP = "Test the Go daemon"

# daemon-go's tests run in legs. .github/go-legs.yaml lists the packages each leg runs, and each
# leg's go test step pipes them from .github/scripts/go_legs.py, which first holds the file to
# `go list ./...`: every package of the module in exactly one leg, or in `elsewhere`, the text
# checks. The legs of a kind are one matrix job of ci-job-daemon-go.yaml, daemon-go-legs for the
# plain step and daemon-go-race-legs for the race step, every leg with that step's one text, so
# the mode (-race, -skip, -timeout) is the step's and no leg's own. The jobs daemon-go and
# daemon-go-race keep the check-run names the steps had before, and pass only when every leg of
# their kind passed.
# why: .github/ci-notes/ci-job-daemon-go.md#why-the-daemons-tests-run-in-legs
DAEMON_PLAIN_JOB = "daemon-go"
DAEMON_RACE_JOB = "daemon-go-race"
DAEMON_PLAIN_LEGS_JOB = "daemon-go-legs"
DAEMON_RACE_LEGS_JOB = "daemon-go-race-legs"
DAEMON_LEGS = go_legs.module_entry(DAEMON, Path(GO_LEGS))


def legs_key_problems(entry: dict, plain_job: str, race_job: str, where: str, extra=(), jobs=()) -> list:
    """What keeps a module's go-legs.yaml entry from having exactly the keys its legs run under:
    its plain job, its race job and `jobs` (envoy's single-package race jobs), whose legs `where`
    runs, beside `elsewhere`, `unlisted` and `extra` (envoy's `twice`); and from naming the plain
    job under `unlisted`, since the last leg of that job runs a package no place names and a race
    leg would run it under the detector, which its tests may not build for. A package filed under
    another key passes every leg, which sees only its own job, and runs in no job. The one check
    both holds below make."""
    problems = []
    keys = set(entry)
    beside = sorted({go_legs.ELSEWHERE, go_legs.UNLISTED, *extra})
    ran_by = [plain_job, race_job, *jobs]
    wanted = {*ran_by, *beside}
    if keys != wanted:
        problems.append(f"its keys are {', '.join(sorted(keys))}, and the jobs whose legs run are "
                        f"{', '.join(ran_by[:-1])} and {ran_by[-1]} ({where}), beside {', '.join(beside[:-1])} and "
                        f"{beside[-1]}: a package under {', '.join(sorted(keys - wanted)) or 'a missing key'} runs in "
                        f"no job, so file it under one of those")
    if entry.get(go_legs.UNLISTED) != plain_job:
        problems.append(f"its {go_legs.UNLISTED} is {entry.get(go_legs.UNLISTED)!r}, and the job whose last leg runs a "
                        f"package no place names is {plain_job}, which runs a package plain: a race leg would run it "
                        f"under the detector, which its tests may not build for")
    return problems


def leg_run_packages(entry: dict, job: str, packages: list) -> list:
    """Every package a job's legs run: the packages its legs list (go_legs.job_packages), and, for
    the job the entry's `unlisted` names, each of `packages` - the module's packages as `go list
    ./...` prints them - that no place names, which that job's last leg runs (go_legs.unlisted). A
    gate's Run over the legs names these, so a change to an unlisted package's test files starts
    the job that runs them."""
    found = go_legs.job_packages(entry.get(job))
    return found + (go_legs.unlisted(entry, packages) if entry.get(go_legs.UNLISTED) == job else [])


def hold_daemon_legs(entry: dict) -> None:
    """Refuse a go-legs.yaml entry for packages/daemon that a leg of ci-job-daemon-go.yaml would not
    run as written. go_legs.py, in each leg, holds the whole entry to `go list ./...` and the
    running job's legs to its matrix, but it sees only its own job: a package filed under a job key
    no matrix job runs (a typo, a job since renamed) is in the file once, passes every leg, and
    runs in no job. So the entry's keys and its `unlisted` are what legs_key_problems() asks of
    them; the race legs run the race packages, each once, the set the gates and
    ci-caches.yaml's race fill read from DAEMON_RACE_PACKAGES; and the text checks, the
    text-check job's, are the one package no leg runs. check-ci-filters.sh imports this table in
    the changes and ci-filters jobs, so a refusal here fails the run that brings the entry.

    A package several legs of one job run a group of the tests of (`- internal/daemon: 1`) is one
    package here, as go_legs.job_packages counts it: those legs together run it once."""
    problems = legs_key_problems(entry, DAEMON_PLAIN_JOB, DAEMON_RACE_JOB, job_file(DAEMON_PLAIN_JOB))
    race = sorted(go_legs.job_packages(entry.get(DAEMON_RACE_JOB) or {}))
    if race != sorted(DAEMON_RACE_PACKAGES):
        problems.append(f"its {DAEMON_RACE_JOB} legs run {race}, and DAEMON_RACE_PACKAGES here is "
                        f"{sorted(DAEMON_RACE_PACKAGES)}: make them one set, each package once")
    # job_packages counts a package once however many legs name it, which is what the gate wants
    # of a package split by test; `each package once` is held here on the entries themselves, so a
    # whole package in two legs fails in the changes job rather than in every leg 90 s in.
    whole = [package for leg in (entry.get(DAEMON_RACE_JOB) or {}).values() for package, group in
             (go_legs.item(written) for written in leg or []) if group is None]
    if len(whole) != len(set(whole)):
        twice = sorted({package for package in whole if whole.count(package) > 1})
        problems.append(f"its {DAEMON_RACE_JOB} legs run {', '.join(twice)} whole in more than one leg, so each "
                        f"would run in every one of them: keep a whole package in one leg, or split it by test")
    if entry.get(go_legs.ELSEWHERE) != {TEXT_CHECK_PACKAGE: "text-checks"}:
        problems.append(f"it runs packages elsewhere as {entry.get(go_legs.ELSEWHERE)}, and the one package no leg "
                        f"runs is {TEXT_CHECK_PACKAGE}, the text-checks job's")
    if problems:
        raise ValueError(f"{GO_LEGS}'s entry for {DAEMON}: " + "; ".join(problems))


hold_daemon_legs(DAEMON_LEGS)
# The packages the daemon's plain legs run, a package no place names among them (leg_run_packages),
# read from the module's own `go list ./...` rather than from the file, so a gate's Run over them
# covers a new package's test files before anyone lists it.
DAEMON_PLAIN_PACKAGES = leg_run_packages(DAEMON_LEGS, DAEMON_PLAIN_JOB, go_legs.go_packages(DAEMON))
DAEMON_RACE_LEG_PACKAGES = go_legs.job_packages(DAEMON_LEGS[DAEMON_RACE_JOB])


# The three tests of this module the plugin-pack job runs: the plugin pack test and the two tests
# of agentmodels_omp_test.go (a fourth that loads a plugin on the real Oh My Pi,
# promptreferences_omp_test.go's prompt reference probe, reads no file of the repository and stays
# in the race step). They run once per run of the router, in the plugin-pack job, and the race
# step that runs their package skips them by name:
#   * `packages/daemon/internal/daemon/promptreferences_omp_test.go#TestTheRealPluginsOnTheRealOhMyPi` and `packages/daemon/internal/daemon/promptreferences_omp_test.go#packedPlugin` packs
#     packages/pi-envoy and packages/pi-legion with `bun pm pack`, and each pack runs that
#     package's prepack, which reads bun.lock and builds the bundle from the installed workspace;
#   * `packages/daemon/internal/daemon/agentmodels_omp_test.go#TestTheAgentModelCheckOnTheRealOhMyPi` and `packages/daemon/internal/daemon/agentmodels_omp_test.go#shippedPromptPlugin`
#     probes the prompts this checkout ships, laid out as the release lays them out:
#     packages/pi-legion/agents copied and each plugin's partition of skills/ staged with
#     scripts/pi-plugin-prepack.sh itself;
#   * `packages/daemon/internal/daemon/agentmodels_omp_test.go#TestTheImageProbeResolvesTheAgentsTheDaemonsPromptsName` probes a
#     plugin it writes itself and reads no file of the repository. It is here because it is the
#     other test of agentmodels_omp_test.go, and RULES is keyed on the source file rather than on
#     the test: with both of that file's tests in this job, no test of that file runs outside this
#     job, so what the plugin-pack gate declares below is the whole truth about the file.
#     hold_plugin_pack_tests(), below, is what holds that, rather than a reader noticing: it
#     refuses a test of that file this list does not name, and a name this list holds that the
#     package no longer declares.
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
    raise ValueError(f"the plugin-pack tests are in {PLUGIN_PACK_PACKAGE}, which no race step runs, so "
                     f"nothing skips them there; name that package's step here, or move the -skip to the step that "
                     f"runs them")


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
    on the safe side. A declaration counts only where the plugin-pack job's runner
    (ubuntu-24.04, amd64) compiles its file with no -tags (declared_tests): neither step passes
    -tags, so a name behind a tag is one the -run cannot reach and the -skip need not skip, and a
    listed name left only in such a file is refused by the first rule below. A `//go:build linux`
    file, which an older reading of this rule took for a tagged one, is compiled and counts.
    What this guard does not see is a new test elsewhere in the package that packs a plugin
    (packedPlugin): it would run in the race step with none of this gate's reads on that job's
    filter, and a reader of such a test moves it here. Nor does it see a subtest - t.Run names no
    declaration, and both patterns match a top-level test whole - or a tagged test some other
    step's own -tags run compiles, which is that step's to declare. The files are read as text
    (declared_tests: GO_TEST_FUNC over go_code_only, so a `func Test…` in a comment or a string
    counts for nothing). A file of the package that writes none of these names and is not the
    whole-file one can declare none of them, so it is not scanned: the package's tests are 3.8 MB
    and go_code_only walks a file character by character."""
    # A file neither step compiles declares no name either step can use, so it is read as
    # declaring none - the whole-file one included, whose listed tests the first rule then
    # reports as gone.
    files = declared_tests(DAEMON, [PLUGIN_PACK_SOURCE], PLUGIN_PACK_TESTS, (AMD64,), always=(PLUGIN_PACK_WHOLE_FILE,))
    declared = {name for names in files.values() for name in names}
    whole_file = files.get(PLUGIN_PACK_WHOLE_FILE)
    gone = ", ".join(name for name in sorted(PLUGIN_PACK_TESTS) if name not in declared)
    if gone:
        raise ValueError(f"PLUGIN_PACK_TESTS names {gone}, which no file of {PLUGIN_PACK_SOURCE} that the runner's "
                         f"linux/amd64 build compiles declares as a test `go test` runs (a top-level `func Test…`, "
                         f"`func Fuzz…` or `func Example…`, TestMain apart): the -run matches nothing of that name, "
                         f"and the -skip skips nothing, so a test renamed out of this list runs in the daemon-go race "
                         f"step with none of this gate's reads on that job's filter. Neither step passes -tags, so a "
                         f"declaration in a file that build leaves out, behind a tag or for another platform, is none "
                         f"here. Follow the rename, move the test to a file that build compiles, or take the name out "
                         f"and give the test's reads back to //ci:reads lines of the file it is in")
    if whole_file is None:
        raise ValueError(f"PLUGIN_PACK_TESTS is the whole truth about {PLUGIN_PACK_WHOLE_FILE}, which is not there "
                         f"to read; follow the file here, or, where its tests have moved out of this job, take "
                         f"them out of this list and give their reads back to //ci:reads lines of the files they "
                         f"are in now")
    unnamed = ", ".join(name for name in sorted(whole_file) if name not in PLUGIN_PACK_TESTS)
    if unnamed:
        raise ValueError(f"{PLUGIN_PACK_WHOLE_FILE} declares {unnamed}, which PLUGIN_PACK_TESTS does not name: "
                         f"this job took that file whole, so the plugin-pack gate's reads are all of the file's "
                         f"reads only while this list names every test of it - every top-level Test, Fuzz and "
                         f"Example a plain `go test` runs, a Fuzz for its seed corpus and an Example for its "
                         f"output comment. A test of it this list leaves out runs in the daemon-go race step "
                         f"instead, whose filter names neither skills/** nor scripts/pi-plugin-prepack.sh. Add "
                         f"the name to this list, or move the test to a file of its own and give its reads "
                         f"//ci:reads lines of that file")


hold_plugin_pack_tests()


# ---------------------------------------------------------------- the supervise table's pure pins
# The two tests of internal/supervise/table_test.go whose subject is the table itself rather than a
# claim in motion: TestTheTableHasARowOrANamedIgnoreForEveryEventInEveryState, which holds every
# event of the sealed set to a row or a named ignore in every state, and TestNoTwoEventTypesShareAKind.
# Each reads source and nothing else - sealed() reads the sealed set through
# `packages/daemon/internal/supervise/table_test.go#sealedSource`, which parses every non-test .go file of the
# directory, and the type-check of those files reads the source of every package they import too,
# since `packages/daemon/internal/supervise/table_test.go#readSealedSet` builds the checker with
# importer.ForCompiler(..., "source", ...), so the whole import closure is
# read from source: of this repository internal/claim, phase, runtime, wait, treelifecycle and
# backoff and what those import, which packages/daemon/**/*.go carries, and outside it the
# standard library and the module cache - and neither starts a database, a container, tmux or jj,
# so they are text-check work: they run in the text-checks job, where they wait behind no fixture,
# and the daemon-go plain step, which runs their package, skips them by name, so each runs once
# per run of the router (dispatch://INFRA-1413, R3).
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
# together here rather than by a reader noticing: the plain legs' step skips them, and this package
# moved out of the plain legs, into a race leg or the text checks, while these tests stayed here
# would run them in that step too.
if SUPERVISE_TABLE_PACKAGE not in DAEMON_PLAIN_PACKAGES:
    raise ValueError(f"the table pins are in {SUPERVISE_TABLE_PACKAGE}, which no plain leg of {GO_LEGS} runs, so "
                     f"nothing skips them there; name that package's step here, or move the -skip to the step "
                     f"that runs them")
DAEMON_PLAIN_SCRIPT = legs_script(DAEMON, DAEMON_PLAIN_JOB,
                                  f"go test -count=1 -timeout 15m -skip {SUPERVISE_TABLE_PATTERN}")
DAEMON_RACE_SCRIPT = legs_script(DAEMON, DAEMON_RACE_JOB,
                                 f"go test -count=1 -race -timeout 30m -skip {PLUGIN_PACK_PATTERN}")


def hold_supervise_table_tests():
    """Refuse a SUPERVISE_TABLE_TESTS that is no longer the truth about internal/supervise's tests:
    a name this list holds that no file of the package that the text-checks runner compiles declares
    as a test `go test` runs.
    Held to nothing, a rename passes every other check green - a -run that matches nothing is a
    pass, and the -skip then skips nothing - so the text-checks step would go on saying it runs
    these pins while it ran no test at all, and the gate's pinned texts would agree with each other
    about a name neither step could reach. A test is what hold_plugin_pack_tests() reads one as:
    every top-level Test, Fuzz or Example of GO_TEST_FUNC, TestMain apart (declared_tests), and
    only in a file the -run step's runner (text-checks, ubuntu-24.04-arm, arm64) compiles with no
    -tags (declared_tests), since neither step passes -tags and a name behind a tag is one the -run
    cannot reach and the -skip need not skip.
    hold_plugin_pack_tests()'s second rule - that its list name every test of the one file that job
    took whole - is not ported here, because this list is part of a file's tests rather than all of
    them. That rule holds a gate whose reads are keyed on test names: the plugin-pack gate declares
    the lockfile, skills/ and the prepack because those three tests read them, so a test of that file
    left out of the list would run in the daemon-go race step, whose filter names none of them. These
    two pins read source alone - the package's own, and the source of everything it imports, which
    their type-checker reads from source too (`packages/daemon/internal/supervise/table_test.go#readSealedSet`)
    and which of this repository is packages/daemon's own packages - and both jobs' filters
    already carry it: text_checks through packages/daemon/**/*.go and daemon_go_tests through the
    module's own closure - so no read moves with a name here, and a test of table_test.go this
    list does not name runs in the plain step, which is where every test of this module runs
    unless something takes it out. Demanding the whole file would refuse exactly the arrangement
    this list exists to make.
    What this guard does not see is a test of one of these names declared by another package of the
    module: the plain step's -skip is the module's, while this job's -run names ./internal/supervise/
    alone, so such a test would be skipped in the one step and not run in the other.
    go-test-step-expansion.py's unrun verdict is what finds that, as it finds every hole a -skip
    leaves. The package's own files are what is scanned, and of those only the ones that write one of
    these names, because the module's test files are 14 MB and go_code_only walks a file character by
    character."""
    files = declared_tests(DAEMON, [SUPERVISE_TABLE_SOURCE], SUPERVISE_TABLE_TESTS, (ARM64,))
    declared = {name for names in files.values() for name in names}
    gone = ", ".join(name for name in sorted(SUPERVISE_TABLE_TESTS) if name not in declared)
    if gone:
        raise ValueError(f"SUPERVISE_TABLE_TESTS names {gone}, which no file of {SUPERVISE_TABLE_SOURCE} that the "
                         f"text-checks runner's linux/arm64 build compiles declares as a test `go test` runs (a "
                         f"top-level `func Test…`, `func Fuzz…` or `func Example…`, TestMain apart): the text-checks "
                         f"step's -run matches nothing of that name and passes green, and the daemon-go plain step's "
                         f"-skip skips nothing, so the two steps say between them that a test runs in a job where it "
                         f"does not. Neither step passes -tags, so a declaration in a file that build leaves out, "
                         f"behind a tag or for another platform, is none here. Follow the rename, move the test to a "
                         f"file that build compiles, or take the name out and leave the test to the plain step")


hold_supervise_table_tests()


# ---------------------------------------------------------------- the envoy race lane
# The three jobs that run packages/envoy's concurrent packages under the race detector, declared
# once: each job's step name, its -timeout, and the packages the step names, in the order it names
# them. Everything the lane decides is built from this list or held to it - the three steps' own
# texts, the race legs' packages and the unit legs' (hold_envoy_legs: the race legs run every
# package of the lane, each package under a tree's directory among them, and the unit legs run
# none of those but the packages whose skips they count; a package added under a tree with no
# line in the race legs fails the hold, since `unlisted` would run it plain), and the -run lists of
# the plain step that runs what a race build cannot (RACE_IMPOSSIBLE_SCRIPT below) - and
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
            "Race-test delivery, the store, the bus, the listener, the registry store, the session cache, kvwatch, the "
            "NATS credentials, the NATS auth service and the sign-in code flow",
            "30m", (RacePackage("internal/dispatch/delivery", tree=True), RacePackage("internal/dispatch/store", tree=True),
                    RacePackage("internal/bus"), RacePackage("cmd/listener"), RacePackage("internal/store"),
                    RacePackage("internal/session"), RacePackage("internal/kvwatch", tree=True),
                    RacePackage("natscred"), RacePackage("internal/cipher/natscallout"), RacePackage("internal/oidc"))),
    RaceJob("envoy-go-race-api", "Race-test the Dispatch API", "70m", (RacePackage("internal/dispatch/api"),)),
    RaceJob("envoy-go-race-docs", "Race-test the document service", "60m", (RacePackage("internal/dispatch/docs"),)),
)
RACE_JOBS = {job.job: job for job in ENVOY_RACE_LANE}


def race_pkgs(job):
    """The package arguments of a race job's step, in the order it names them."""
    return [f"./{p.name}/..." if p.tree else f"./{p.name}/" for p in job.packages]


def race_lane_packages(root, jobs=None) -> list:
    """Every package directory the lane's steps run (or those of `jobs`, some of its jobs), in the
    order its steps name them: a package alone for an exact entry, and for a tree its directory
    and every directory with .go files under it, passing over what go passes over (go_skips_dir,
    the rule pattern_dirs walks a `/...` pattern by). Each is (the path under `root`, the
    directory). The plain run's derivation reads the lane's packages so, and so does
    hold_envoy_legs, which holds every one of envoy-go-race's to a race leg. The lane is read when
    the walk runs, not when it was defined, so a reader that swaps ENVOY_RACE_LANE (the harness's
    fixtures) walks the lane it swapped in."""
    root, seen, found = Path(root), set(), []
    for job in ENVOY_RACE_LANE if jobs is None else jobs:
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


# The job of ci-caches.yaml whose race build fills go-envoy-race, the cache every leg of the lane
# restores: it compiles every job's packages of the lane, in the lane's order.
ENVOY_RACE_FILL_JOB = "envoy-race"

# ci-caches.yaml's race builds that name a lane's packages inline, each (fill job, the name of the
# constant naming it, the package words its race build must name, the name of the list they come
# from, the step that restores the cache, the jobs that restore it, and the clause saying which
# steps follow that list): ci_workflow_rules.py's check_race_fill_packages holds each fill job's race build to its
# words, so the fill and the lane cannot name other packages with nothing failing.
RACE_FILLS = [
    (DAEMON_RACE_FILL_JOB, "DAEMON_RACE_FILL_JOB", [f"./{p}/" for p in DAEMON_RACE_PACKAGES], "DAEMON_RACE_PACKAGES",
     f"{CI}'s race step", CI, f"{CI}'s two race steps follow it already"),
    (ENVOY_RACE_FILL_JOB, "ENVOY_RACE_FILL_JOB", [word for job in ENVOY_RACE_LANE for word in race_pkgs(job)],
     "ENVOY_RACE_LANE", "the envoy race lane's steps", "the envoy race lane",
     "envoy-go-race-api's step follows it already (RACE_API_SCRIPT), envoy-go-race's legs are held to it "
     "(hold_envoy_legs refuses race legs whose packages are not the lane's, every package under a tree "
     "among them), and envoy-go-race-docs' line is RACE_DOCS_SCRIPT's, written by hand, so a change to the "
     "lane's docs entry changes that line too"),
]


# The architecture-note and spec-file rules and the commands that run them, which envoy-go-build
# tests in a step of their own: they need no database, so they run there on every push, and that
# step is their one run. Both that step's package list and the unit step's exclusion of them are
# built from this list, so go-test-step-expansion.py finds no pair the two steps both run and
# neither step can come to name a package the other does not leave out.
NOTE_AND_SPEC_PACKAGES = ["internal/dispatch/architecture/notes", "internal/runnerlog", "internal/dispatch/specgraph",
                          "cmd/architecture-check", "cmd/spec-check"]
NOTE_AND_SPEC_STEP = "Test the architecture-note and spec-file rules and their commands"
NOTE_AND_SPEC_PKGS = [f"./{p}/" for p in NOTE_AND_SPEC_PACKAGES]
NOTE_AND_SPEC_TEXT = "go test -count=1 " + " ".join(NOTE_AND_SPEC_PKGS)

# listenerwire's tests and wirecheck's, the tool its clients test runs, which envoy-go-build runs
# in a step of their own, their one run. clients_test.go type-checks the daemon's listener clients
# (its //ci: lines), so run in envoy-go's unit step a change to one of those daemon packages would
# start that whole job, its database and its 20-minute unit step, for one type check. They need no
# database. As with the architecture-note and spec-file packages, that step's package list and the
# unit step's exclusion are built from this one list.
LISTENERWIRE_PACKAGES = ["listenerwire", "internal/wirecheck"]
LISTENERWIRE_STEP = "Test the listener's wire package, its clients and wirecheck"
LISTENERWIRE_PKGS = [f"./{p}/" for p in LISTENERWIRE_PACKAGES]
LISTENERWIRE_TEXT = "go test -count=1 " + " ".join(LISTENERWIRE_PKGS)

# The route adopter, cmd/dispatch-route-adopt, runs its rows under git and under jj, so
# envoy-go-skips installs jj (./.github/actions/install-jj) and tests the package in a step of its
# own with -v, which names each row in the log: a leg's go test -json report names no test that
# passed, so a jj row that skipped would read as green there. The rows that need jj fail rather
# than skip on GitHub Actions without it. As with the packages above, that step's package list
# and the package's `elsewhere` line in .github/go-legs.yaml (hold_envoy_legs) are built from this
# one list. The tool is deleted once the branches open when routes moved into files have adopted,
# and this list and its step with it.
ADOPTER_PACKAGES = ["cmd/dispatch-route-adopt"]
ADOPTER_STEP = "Test the route adopter under git and jj"
ADOPTER_PKGS = [f"./{p}/" for p in ADOPTER_PACKAGES]
ADOPTER_TEXT = "go test -count=1 -v -timeout 4m " + " ".join(ADOPTER_PKGS)

# The packages whose skips envoy-go's unit step counts: each leg runs its packages through
# packages/envoy/scripts/database-skips.sh --count-table, one `go test -json`, and the script
# counts the skips of the packages of its default table (DEFAULT_PACKAGES there) among them: the
# broker's tree, internal/cipher and every package under it, its command, cmd/cipherd, and
# DATABASE_SKIPS_PACKAGES, each the package alone: the writer hold's client, internal/holdrecord,
# whose journal tests run on the broker's own routes over its Postgres. It fails on any skip there
# as on any failed test in the run. They ran in a step of their own after the unit step
# until INFRA-1075: cmd/cipherd alone, 714 s, held that step at 15.3 minutes, in which its four -p
# slots ran 1,817 s of package time, half what they had, while the unit step, 14.8 minutes, ran
# before it (merge-group run 45048924209, job 137290624677); in one go test the two share the
# slots, and now each leg's packages share its own. The script's own DEFAULT_EXCEPT, a `grep -Ev`
# over that table's import paths, leaves out
# DATABASE_SKIPS_RACE_ONLY: race lane packages under the tree that open no database, which the
# lane's race job runs, where counting them would add them to the run, each one's second in the
# same CI run, and would count nothing. It names none today. The auth service's natscallout is in
# the lane, and its hold rows open the broker's Postgres and skip without it, so a unit leg runs it
# too, its second run in CI, for the count of its skips (`.github/go-legs.yaml#twice`).
# DATABASE_SKIPS_PATTERN is built from the lane's declaration of those packages.
# hold_database_skips_packages() below refuses a script whose table is not this list, or whose
# DEFAULT_EXCEPT is not that pattern.
DATABASE_SKIPS_TREE = "internal/cipher"
DATABASE_SKIPS_COMMAND = "cmd/cipherd"
DATABASE_SKIPS_PACKAGES = ["internal/holdrecord"]
DATABASE_SKIPS_PKGS = ([f"./{DATABASE_SKIPS_TREE}/...", f"./{DATABASE_SKIPS_COMMAND}/"]
                       + [f"./{p}/" for p in DATABASE_SKIPS_PACKAGES])
DATABASE_SKIPS_SCRIPT = "packages/envoy/scripts/database-skips.sh"
DATABASE_SKIPS_RACE_ONLY: list = []
_lane_under_tree = {p.name: p for job in ENVOY_RACE_LANE for p in job.packages
                    if p.name == DATABASE_SKIPS_TREE or p.name.startswith(f"{DATABASE_SKIPS_TREE}/")}
if any(name not in _lane_under_tree for name in DATABASE_SKIPS_RACE_ONLY):
    raise ValueError(f"DATABASE_SKIPS_RACE_ONLY names {DATABASE_SKIPS_RACE_ONLY}, and only race lane packages under "
                     f"{DATABASE_SKIPS_TREE} ({sorted(_lane_under_tree)}) can be left to the race job")
DATABASE_SKIPS_PATTERN = "|".join(
    f"/{p.name}($|/)" if p.tree else f"/{p.name}$" for p in _lane_under_tree.values() if p.name in DATABASE_SKIPS_RACE_ONLY)
# The -timeout of envoy-go's unit step, which bounds each package's test binary.
UNIT_TEST_TIMEOUT = "20m"

# The tests envoy-go's step "Count the store-form rows on each backend" runs through
# packages/envoy/scripts/backend-count.sh, by package, counting each one's `aws` and `gcp` passes
# (the verification's check c8). It runs them in envoy-go-skips, with the job's env, no -tags and
# no -race, the build the unit legs run cmd/cipher and internal/cipher/api in, so a second run in
# a leg asked nothing the count does not: the legs skip them by name, and the count is their one
# run (O15 of dispatch://INFRA-1408, which
# go-test-step-expansion.py's duplicate verdict reads; its BACKEND_COUNT_TESTS is this list).
BACKEND_COUNT_AGENT_SECRETS = "cmd/cipher"
BACKEND_COUNT_BROKER_API = f"{DATABASE_SKIPS_TREE}/api"
BACKEND_COUNT_TESTS = {
    BACKEND_COUNT_AGENT_SECRETS: ["TestEverySecretFormNamesTheSourceItUses", "TestNoSecretFormAsksTheBrokerForACredential",
                                  "TestUseCommandOutsideASessionReachesTheMachineSource",
                                  "TestTheChainSkipsAbsenceAndStopsOnABrokenSource"],
    BACKEND_COUNT_BROKER_API: ["TestSettingsNameTheGoogleExchangeAudienceOnEitherStore"],
}


def names_pattern(names):
    """The anchored alternation of whole test names a step's -run or -skip writes, quoted for the
    shell: `'^(Name|Name|…)$'`, the names sorted."""
    return f"'^({'|'.join(sorted(names))})$'"


# The counter's own test, which envoy-go-skips runs beside the unit legs: its controls run the
# counter on these five packages, each narrowed to its planted and expected tests.
DATABASE_SKIPS_TEST_STEP = "Test the database skip counter"
DATABASE_SKIPS_TEST = "packages/envoy/scripts/database-skips.test.sh"
DATABASE_SKIPS_TEST_PKGS = ["./internal/cipher/api/", "./cmd/cipherd/", "./internal/holdrecord/", "./internal/dispatch/api/",
                            "./internal/fold/"]


def hold_database_skips_packages():
    """Refuse a database-skips.sh whose default table is not DATABASE_SKIPS_PKGS, or whose
    DEFAULT_EXCEPT is not DATABASE_SKIPS_PATTERN. The unit step counts that table's skips less
    DATABASE_SKIPS_RACE_ONLY, and runs every package of the table it counts, so a package the
    script added without this list would be counted by nothing here that declares it, one it
    dropped would run uncounted, and a lane package this list leaves to the race job that the
    script kept would be added to the unit step's run, its second in the CI run, with every check
    green."""
    text = Path(DATABASE_SKIPS_SCRIPT).read_text()
    found = re.findall(r"^DEFAULT_PACKAGES=\((.*)\)$", text, re.M)
    if found != [" ".join(DATABASE_SKIPS_PKGS)]:
        raise ValueError(f"{DATABASE_SKIPS_SCRIPT}'s DEFAULT_PACKAGES is {found}, and DATABASE_SKIPS_PKGS here is "
                         f"{DATABASE_SKIPS_PKGS}: make them one list, the packages whose skips the unit step "
                         f"counts")
    excepted = re.findall(r"^DEFAULT_EXCEPT='(.*)'$", text, re.M)
    if excepted != [DATABASE_SKIPS_PATTERN]:
        raise ValueError(f"{DATABASE_SKIPS_SCRIPT}'s DEFAULT_EXCEPT is {excepted}, and DATABASE_SKIPS_PATTERN here, "
                         f"the race lane's packages under {DATABASE_SKIPS_TREE} that DATABASE_SKIPS_RACE_ONLY leaves to "
                         f"the race job, is '{DATABASE_SKIPS_PATTERN}': make them one pattern, so the count adds no "
                         f"package this list leaves to the lane's race job")


hold_database_skips_packages()

# envoy's unit step and its race lane's envoy-go-race step run in legs. .github/go-legs.yaml's
# packages/envoy entry lists the packages each leg runs, and each leg's step pipes them from
# .github/scripts/go_legs.py, which first holds the file to `go list ./...`: every package of the
# module in exactly one place, a leg of envoy-go, a leg of envoy-go-race or `elsewhere`, but those
# `twice` names, which run in a leg of each. The unit step's legs are envoy-go-legs, a matrix job of
# ci-job-envoy-go.yaml, and the job's other steps run beside them, each kind on a runner of its
# own: envoy-go-alone runs the rows whose bound is on time and the xid window rows, which need a
# runner nothing else loads; envoy-go-race-impossible the rest of the plain run the race build
# cannot make; envoy-go-skips the skip counter's test, the store-form count and the two vets; and
# envoy-go-natslive the container-backed matrix, the integration suite and the callout check. The
# race step's legs are envoy-go-race-legs, of ci-job-envoy-go-race.yaml. The jobs envoy-go and
# envoy-go-race keep the check-run names the one jobs had before, and pass only when every job
# they wait for passed.
# why: .github/ci-notes/ci.md#why-envoys-go-tests-run-in-legs
ENVOY_PLAIN_JOB = "envoy-go"
ENVOY_RACE_JOB = "envoy-go-race"
ENVOY_GO_FILE = job_file(ENVOY_PLAIN_JOB)
ENVOY_RACE_FILE = job_file(ENVOY_RACE_JOB)
ENVOY_LEGS_JOB = "envoy-go-legs"
ENVOY_ALONE_JOB = "envoy-go-alone"
ENVOY_RACE_IMPOSSIBLE_JOB = "envoy-go-race-impossible"
ENVOY_SKIPS_JOB = "envoy-go-skips"
ENVOY_NATSLIVE_JOB = "envoy-go-natslive"
ENVOY_RACE_LEGS_JOB = "envoy-go-race-legs"
# The package envoy-go-natslive's integration step tests, which no leg runs.
ENVOY_INTEGRATION_PACKAGE = "internal/integration"
ENVOY_LEGS = go_legs.module_entry(ENVOY, Path(GO_LEGS))
UNIT_TEST_STEP = "Unit test envoy Go package"


def within(package, lane_package):
    """Whether a package (a directory under packages/envoy) is a race lane package: that package,
    or for a tree any package under it."""
    return package == lane_package.name or (lane_package.tree and package.startswith(f"{lane_package.name}/"))


def database_skips_table():
    """The packages whose skips the unit legs count, as directories under packages/envoy: the
    broker's command, DATABASE_SKIPS_PACKAGES and every directory of the broker's tree that holds a
    .go file, less DATABASE_SKIPS_RACE_ONLY's, which database-skips.sh's DEFAULT_EXCEPT takes out."""
    found = [DATABASE_SKIPS_COMMAND] + DATABASE_SKIPS_PACKAGES
    for current, subdirs, names in os.walk(Path(ENVOY) / DATABASE_SKIPS_TREE):
        subdirs[:] = sorted(s for s in subdirs if not go_skips_dir(Path(current), s))
        if any(n.endswith(".go") for n in names):
            found.append(Path(current).relative_to(ENVOY).as_posix())
    race_only = [p for p in _lane_under_tree.values() if p.name in DATABASE_SKIPS_RACE_ONLY]
    return [p for p in found if not any(within(p, lane) for lane in race_only)]


def hold_envoy_legs(entry: dict, root=ENVOY) -> None:
    """Refuse a go-legs.yaml entry for packages/envoy that the legs would not run as the jobs
    around them are built. go_legs.py, in each leg, holds the whole entry to `go list ./...` and
    the running job's legs to its matrix, but it sees only its own job, and nothing of why a
    package is where it is. So: the entry's keys and its `unlisted` are what legs_key_problems()
    asks of them, `twice` among the keys and the race lane's two single-package jobs
    (envoy-go-race-api and envoy-go-race-docs, a group of its one package's tests per leg) among
    the jobs; each single-package job's legs run that job's lane package and nothing else; the
    race legs run the packages of ENVOY_RACE_LANE's envoy-go-race and nothing else, each tree's
    root among them, and every package each tree holds under `root` (race_lane_packages, the walk
    of `./<tree>/...` the plain run's derivation reads the lane by) - a package added under a tree
    with no line would otherwise run plain in envoy-go's last leg, which `unlisted` sends it to,
    and lose the detector the tree's `./<tree>/...` gave it on main; a plain leg runs
    no package the lane runs under the detector but those `twice` names; `twice` names exactly the
    race legs' packages whose skips the unit legs count (database_skips_table), so each of those
    runs plain, where it is counted, and no other runs twice; and `elsewhere` is exactly the
    packages other jobs' steps are built here to run, each with that job: envoy-go-natslive's
    integration suite, the text checks, envoy-go-build's (NOTE_AND_SPEC_PACKAGES,
    LISTENERWIRE_PACKAGES) and envoy-go-skips' route adopter (ADOPTER_PACKAGES). With go_legs.py's
    own check, every package of the module then runs
    once in the job it is built for, and every package of the counter's table in a plain leg,
    which counts its skips. check-ci-filters.sh imports this table in the changes and ci-filters
    jobs, so a refusal here fails the run that brings the entry, the pull request that adds a
    package under a tree among them."""
    single = {job.job: job for job in ENVOY_RACE_LANE if job.job != ENVOY_RACE_JOB}
    problems = legs_key_problems(entry, ENVOY_PLAIN_JOB, ENVOY_RACE_JOB,
                                 f"{ENVOY_GO_FILE}, {ENVOY_RACE_FILE} and the race lane's single-package jobs' files",
                                 extra=(go_legs.TWICE,), jobs=tuple(sorted(single)))
    for name, job in single.items():
        ran = go_legs.job_packages(entry.get(name))
        if ran != [p.name for p in job.packages]:
            problems.append(f"its {name} legs run {', '.join(ran) or 'nothing'}, and ENVOY_RACE_LANE's {name} is "
                            f"{', '.join(p.name for p in job.packages)}: give the job's legs that package alone, a "
                            f"group of its tests each")
    plain = go_legs.job_packages(entry.get(ENVOY_PLAIN_JOB))
    race = go_legs.job_packages(entry.get(ENVOY_RACE_JOB))
    twice = sorted(entry.get(go_legs.TWICE) or {})
    lane = RACE_JOBS[ENVOY_RACE_JOB].packages
    stray = [p for p in race if not any(within(p, l) for l in lane)]
    if stray:
        problems.append(f"its {ENVOY_RACE_JOB} legs run {', '.join(stray)}, which ENVOY_RACE_LANE's {ENVOY_RACE_JOB} "
                        f"does not name: add the package to the lane, or move it to a plain leg")
    missing = [l.name for l in lane if l.name not in race]
    if missing:
        problems.append(f"ENVOY_RACE_LANE's {ENVOY_RACE_JOB} names {', '.join(missing)}, which no {ENVOY_RACE_JOB} leg "
                        f"runs: put each in a race leg, or take it out of the lane")
    named = {l.name for l in lane}
    unraced = [under for under, _ in race_lane_packages(root, (RACE_JOBS[ENVOY_RACE_JOB],))
               if under not in named and under not in race]
    if unraced:
        problems.append(f"the race lane's trees hold {', '.join(unraced)}, which no {ENVOY_RACE_JOB} leg lists: "
                        f"{go_legs.UNLISTED} would run each plain in {ENVOY_PLAIN_JOB}'s last leg, with no race "
                        f"detector, where its tree's packages run under it; put each in the race leg of its tree")
    detected = [p for p in plain if p not in twice and any(within(p, l) for job in ENVOY_RACE_LANE for l in job.packages)]
    if detected:
        problems.append(f"its {ENVOY_PLAIN_JOB} legs run {', '.join(detected)}, which the race lane runs under the "
                        f"detector: take each out of the plain legs, or, where its skips must be counted, name it under "
                        f"{go_legs.TWICE}")
    table = database_skips_table()
    counted = sorted(p for p in race if p in table)
    if twice != counted:
        problems.append(f"its {go_legs.TWICE} names {', '.join(twice) or 'nothing'}, and the race legs' packages whose "
                        f"skips the unit legs count ({DATABASE_SKIPS_SCRIPT}'s table) are {', '.join(counted) or 'none'}: "
                        f"make them one set, so each of those runs plain too, where it is counted, and no other runs "
                        f"twice")
    elsewhere = {ENVOY_INTEGRATION_PACKAGE: ENVOY_NATSLIVE_JOB, TEXT_CHECK_PACKAGE: "text-checks"}
    elsewhere.update({p: "envoy-go-build" for p in NOTE_AND_SPEC_PACKAGES + LISTENERWIRE_PACKAGES})
    elsewhere.update({p: ENVOY_SKIPS_JOB for p in ADOPTER_PACKAGES})
    if (entry.get(go_legs.ELSEWHERE) or {}) != elsewhere:
        problems.append(f"it runs packages elsewhere as {entry.get(go_legs.ELSEWHERE)}, and the packages other jobs' "
                        f"steps are built here to run are {elsewhere}")
    if problems:
        raise ValueError(f"{GO_LEGS}'s entry for {ENVOY}: " + "; ".join(problems))


hold_envoy_legs(ENVOY_LEGS)
# The packages the unit legs run: what they list, a package split by test across legs once
# (`- cmd/listener: 1`, though only race legs split one today), and each package `go list ./...`
# prints that no place names, which envoy-go's last leg runs (leg_run_packages), read from the
# module rather than from the file, so the gate's Run covers a new package's test files before
# anyone lists it.
ENVOY_GO_PACKAGES = go_legs.go_packages(ENVOY)
ENVOY_UNIT_PACKAGES = leg_run_packages(ENVOY_LEGS, ENVOY_PLAIN_JOB, ENVOY_GO_PACKAGES)
ENVOY_RACE_LEG_PACKAGES = go_legs.job_packages(ENVOY_LEGS[ENVOY_RACE_JOB])
ENVOY_RACE_STEP = RACE_JOBS[ENVOY_RACE_JOB].step
ENVOY_RACE_SCRIPT = legs_script(ENVOY, ENVOY_RACE_JOB, f"go test -count=1 -race -timeout {RACE_JOBS[ENVOY_RACE_JOB].timeout}")

# envoy-go-race-api's step runs its package through database-skips.sh, which counts the skips of
# that same run, so the package's contract tests, which skip without BROKER_TEST_DATABASE_URL, and
# the rest, which skip without DISPATCH_TEST_DATABASE_URL, cannot skip unseen in a step that runs
# without -v. Its text is built from the lane's declaration: the race job's packages, and its
# -timeout as the counter's --timeout.
RACE_API = RACE_JOBS["envoy-go-race-api"]
# The API's legs: each runs a group of internal/dispatch/api's tests, so the packages and the
# group's -run come from go_legs.py and the counter takes them through xargs. -run reaches the
# counter's EXPECTED_SKIPS rule too, which then holds only the tests the group runs.
RACE_API_LEGS_JOB = "envoy-go-race-api-legs"
RACE_API_SCRIPT = legs_script(ENVOY, RACE_API.job,
                              "scripts/database-skips.sh --var DISPATCH_TEST_DATABASE_URL "
                              f"--var BROKER_TEST_DATABASE_URL --race --timeout {RACE_API.timeout}")


# ---------------------------------------------------------------- the web app's display fixture
# The tests of internal/dispatch/pmdoc that touch the committed display-tree fixture the web
# app's own unit tests draw. `packages/envoy/internal/dispatch/pmdoc/web_fixtures_test.go#TestWebDisplayFixture` is the one whose
# subject it is. Its `--check` is a flag of its own (`packages/envoy/internal/dispatch/pmdoc/web_fixtures_test.go#checkWebFixture`): with it
# the test compares the committed fixture with what this build makes and fails where they
# differ, and without it it writes that fixture and asserts nothing. So the fixture step below is
# the run with a requirement behind it, and the envoy-go unit step - which runs every package of
# the module the race lane, internal/integration and the text checks leave it, pmdoc among them -
# skips the test by name: a second run there would rewrite the committed file and pass whatever
# that file held, so the pair would run twice per run of the router with nothing asked of the
# second (dispatch://INFRA-1413). The other test,
# `packages/envoy/internal/dispatch/pmdoc/web_fixtures_test.go#TestTheWebFixtureFindsItsFilesFromAnyDirectory`, stats that the same file
# exists, and an existence check is a read by this table's own rule (the contracts gate declares
# skills/dispatch-writing-specs/SKILL.md for an existsSync alone), so it runs in the fixture step
# too and the unit step skips it, which leaves the fixture a read of the fixture step alone.
# Both step texts are built from this one list, and check-ci-filters.sh pins each step's whole
# script, so the -skip and the -run cannot come to name different tests.
WEB_FIXTURE_TESTS = ["TestWebDisplayFixture", "TestTheWebFixtureFindsItsFilesFromAnyDirectory"]
WEB_FIXTURE_PACKAGE = "internal/dispatch/pmdoc"
WEB_FIXTURE_SOURCE = f"{ENVOY}/{WEB_FIXTURE_PACKAGE}"
FIXTURE_STEP = "Check the web app's display-tree fixture is up to date"
WEB_FIXTURE_PATTERN = names_pattern(WEB_FIXTURE_TESTS)
# The fixture check's `-run` and its `-args --check` are not flags the simple form reduces, and
# they move no closure: the whole pmdoc package compiles, whichever test the run selects.
FIXTURE_SCRIPT = f"go test -count=1 -run {WEB_FIXTURE_PATTERN} ./{WEB_FIXTURE_PACKAGE}/ -args --check"
# pmdoc's readers on tables in a build made without inlining (-gcflags=all=-l), envoy-go-build's
# step of its own: goldmark builds pmdoc's readers through an interface, which Go's initialization
# order does not look through, so a reader that copies a variable of pmdoc can be handed nil in
# that build alone. The table tests read through the block reader, and the display fixture test
# through the display reader. Its `-run` and `-gcflags` are not flags the simple form reduces, and
# they move no closure: the whole pmdoc package compiles, whichever test the run selects.
NO_INLINE_TABLE_STEP = "Test pmdoc's tables in a build without inlining"
NO_INLINE_TABLE_SCRIPT = (f"go test -count=1 -gcflags=all=-l "
                          f"-run '^(TestTable.*|TestReadForDisplayMatchesItsFixtures)$' ./{WEB_FIXTURE_PACKAGE}/")
# The skip belongs to the step that would otherwise run these tests, so the two are held together
# here rather than by a reader noticing: the unit legs run the packages go-legs.yaml lists under
# envoy-go, and pmdoc taken out of them - moved into the race lane, say - while the skip stayed
# here would skip the test in a step that no longer runs it, and the step that did run it would
# write the committed fixture unchecked.
if WEB_FIXTURE_PACKAGE not in ENVOY_UNIT_PACKAGES:
    raise ValueError(f"the display-tree fixture tests are in {WEB_FIXTURE_PACKAGE}, which no {ENVOY_PLAIN_JOB} leg of "
                     f"{GO_LEGS} runs, so the unit step does not run them and the -skip below skips nothing; name "
                     f"the step that runs that package now and move the -skip to it")
# The backend count's tests of cmd/cipher and internal/cipher/api (BACKEND_COUNT_TESTS above) are
# skipped here by the same rule, and held to this step the same way.
for backend_package in BACKEND_COUNT_TESTS:
    if backend_package not in ENVOY_UNIT_PACKAGES:
        raise ValueError(f"the backend count's tests of {backend_package} are skipped by the unit step, and no "
                         f"{ENVOY_PLAIN_JOB} leg of {GO_LEGS} runs that package, so the -skip below skips nothing; "
                         f"name the step that runs that package now and move the skip to it")
# The tests whose assertion is a bound on how long something takes, measured from the test's own
# clock, under twice its slowest reading (the rule of dispatch://INFRA-1075, which #2733 applied to
# every package of the unit step over its paired runs): beside a leg's other packages they measure
# the load, so envoy-go-alone runs them on a runner nothing else loads, one package at a time and
# one test at a time (-parallel 1), and the unit legs skip each by its name. Two of #2733's forty
# are not on main (duty's TestHoldTakesOverOnTheFirstTryAfterTheLapse and pmdoc's
# TestParseResolvesUnmatchedReferencesWithoutComparingEveryDefinition). A test moves in by the same
# rule: one name in its package's list. hold_unit_alone_tests() holds each name to a test its
# package declares, and to no other package of the unit legs, since a -skip is read in every
# package a leg runs.
UNIT_ALONE_TESTS = {
    "cmd/cipher": [
        "TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown",
        "TestExecLearnsOfAGrantAfterAFullHold",
        "TestExecLearnsOfTheGrantWithinASecond",
        "TestGCPSecretMetadataSourceIsPresentWhenItYieldsAToken",
        "TestIdentity",
    ],
    "cmd/dispatch": [
        "TestAProcessReportsASilentNATSCutWithinSeconds",
        "TestARollingUpdatesSuccessorHandsEachMentionTheOldProcessLeftOnce",
        "TestClosingThePoolGivesUpOnALateConnectionOnceTheDatabaseStopsAnswering",
        "TestHealthzAnswersWhilePostgresAndTheBucketBothStopAnswering",
        "TestHealthzAnswersWhilePostgresStopsAnswering",
        "TestSIGTERMWhileARequestHoldsHTTPShutdownStillSettlesTheOwedDocument",
        "TestSIGTERMWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument",
        "TestSIGTERMWithTheDatabaseUnansweringExitsWithinTheShutdownBudget",
    ],
    "internal/cistore": [
        "TestADeletedBucketFailsAWriteWithinTheBudget",
    ],
    "internal/dispatch/architecture": [
        "TestSyncDeadlineIsRecordedOnTheRow",
    ],
    "internal/dispatch/duty": [
        "TestHoldExpiryMatchesTheDatabaseClock",
        "TestHoldReportsLossBeforeTheDatabaseExpiryWhenRenewalsFail",
    ],
    "internal/dispatch/embedqueue": [
        "TestAClaimLeftByAnotherProcessIsTakenWhenItLapses",
        "TestAWriteIsEmbeddedOnItsNotification",
        "TestReserveTokensSharesItsBudgetAcrossConnections",
        "TestScanPassReturnsWhenThePendingRowsFallDue",
        "TestTheIdentityStepsRunOnceTheQueueFirstEmbeds",
    ],
    "internal/dispatch/outbox": [
        "TestABatchCutOffInFlightKeepsItsOrder",
        "TestAHolderWhoseRecordFindsTheHoldAnothersStopsItsRenewal",
        "TestAStandbyDoesNotTryTheHoldForEveryEventTheHolderWrites",
        "TestAStoppedOutboxHandsThePublisherToTheOtherAtOnce",
        "TestAnEventNATSNeverAcknowledgesIsBackedOffAndRecorded",
        "TestAnOutboxThatLosesNATSReleasesThePublisher",
        "TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther",
        "TestOnlyOneOutboxPublishesAtATime",
        "TestTheLostLineIsWrittenBeforeTheReleaseAndDatesTheStop",
        "TestTheOutboxDoesNotScanWhileIdle",
    ],
    "internal/dispatch/textversions": [
        "TestAStandbyTakesAFreeHoldWithNoNotification",
        "TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds",
    ],
    "internal/legionagents": [
        "TestAStalledListingFailsWithinTheReadTimeout",
    ],
    "internal/pglisten": [
        "TestTheListenerReplacesAHalfOpenConnection",
    ],
    "internal/testnats": [
        "TestABucketWaitWhoseLookupsAreHeldFailsByNameInTime",
    ],
    "loadtest": [
        "TestSamplerHeapReadDelaysNoSample",
    ],
}
UNIT_ALONE_STEP = "Test the rows whose bound is on time, alone"


def hold_unit_alone_tests(alone=None, packages=None, module=ENVOY):
    """Refuse a UNIT_ALONE_TESTS (`alone`, each package of `module` to its names) that the legs
    would not run as it says, over `packages`, the unit legs' packages: a package no unit leg runs
    (its tests would then run nowhere, the legs' -skip and the alone step's -run both naming them
    in vain), a name no file the runner builds declares as a test in its package (the alone
    step's -run would match nothing and pass, and the test would run in no job), and a name a test
    file of another package of the unit legs declares too (the legs' -skip, read in every package
    a leg runs, would leave that test out there, where no alone step runs it). A test is what
    hold_plugin_pack_tests() reads one as (declared_tests), in a file the alone step's runner
    (ubuntu-24.04-arm, arm64) compiles with no -tags (declared_tests): a `//go:build linux` file
    and an `x_linux_test.go` do, and a `//go:build natslive` file and an `x_darwin_test.go` do
    not. check-ci-filters.test.sh holds the refusals on a fixture module."""
    alone = UNIT_ALONE_TESTS if alone is None else alone
    packages = ENVOY_UNIT_PACKAGES if packages is None else packages
    wanted = {name for names in alone.values() for name in names}
    problems = []
    declared = {}
    for path, names in declared_tests(module, [Path(module) / package for package in packages], wanted,
                                      (ARM64,)).items():
        package = Path(path).parent.relative_to(Path(module)).as_posix()
        for name in set(names) & wanted:
            declared.setdefault(name, set()).add(package)
    for package, names in alone.items():
        if package not in packages:
            problems.append(f"{package}, which no {ENVOY_PLAIN_JOB} leg of {GO_LEGS} runs")
            continue
        for name in names:
            where = declared.get(name, set())
            if package not in where:
                problems.append(f"{name}, which no _test.go file of {module}/{package} that the runner's "
                                f"linux/arm64 build compiles declares as a test `go test` runs (neither the alone "
                                f"step nor the legs pass -tags, so a declaration in a file that build leaves out, "
                                f"behind a tag or for another platform, is none)")
            elif len(where) > 1:
                problems.append(f"{name}, which {', '.join(sorted(where - {package}))} declare(s) too, where the "
                                f"legs' -skip would leave it out and no alone step runs it")
    if problems:
        raise ValueError("UNIT_ALONE_TESTS names " + "; ".join(problems))


hold_unit_alone_tests()
UNIT_ALONE_SCRIPT = "\n".join(
    f"go test -count=1 -parallel 1 -timeout 10m -run {names_pattern(names)} ./{package}/"
    for package, names in sorted(UNIT_ALONE_TESTS.items()))
UNIT_SKIP_TESTS = (WEB_FIXTURE_TESTS + [t for ts in BACKEND_COUNT_TESTS.values() for t in ts]
                   + [t for ts in UNIT_ALONE_TESTS.values() for t in ts])
# Each leg's packages in one go test through the skip counter, which counts the skips of the
# table's packages among them (DATABASE_SKIPS_PKGS above says why one go test, and why a leg).
UNIT_TEST_SCRIPT = legs_script(
    ENVOY, ENVOY_PLAIN_JOB,
    f"{Path(DATABASE_SKIPS_SCRIPT).relative_to(ENVOY).as_posix()} --timeout {UNIT_TEST_TIMEOUT} "
    f"--skip {names_pattern(UNIT_SKIP_TESTS)} --count-table")


def hold_web_fixture_tests():
    """Refuse a WEB_FIXTURE_TESTS that is no longer the truth about internal/dispatch/pmdoc's
    tests: a name this list holds that no file of the package that the fixture step's runner
    compiles declares as a test `go test` runs. Held to nothing, a rename passes every other check
    green - a -run that
    matches nothing is a pass, and the -skip then skips nothing - so the fixture step would go on
    saying it checks the committed display tree while it ran no test at all, and the renamed test
    would run in the unit step instead, where no `-args --check` reaches it: there it writes the
    committed fixture and asserts nothing, so the one check standing between a changed display
    reading and a stale fixture the web app's view tests draw would be gone with every step
    green. A test is what hold_plugin_pack_tests() reads one as: every top-level Test, Fuzz or
    Example of GO_TEST_FUNC, TestMain apart (declared_tests), and only in a file the -run step's
    runner (envoy-go-build, ubuntu-24.04, amd64) compiles with no -tags (declared_tests), since
    neither step passes -tags and a name behind a tag is one the -run cannot reach and the -skip
    need not skip.
    Of that precedent's two rules this ports the first and not the second - that its list name
    every test of the one file the job took whole - as hold_supervise_table_tests() does not,
    for that guard's reason and one of this list's own. Its reason: the whole-file rule holds a
    gate whose reads are keyed on test names, the plugin-pack gate declaring the lockfile,
    skills/ and the prepack because those three tests read them, while this gate's reads are
    keyed on the package - the fixture step's Run is ./internal/dispatch/pmdoc/ and
    envoy-go-build's filter carries packages/envoy/** - so no read moves with a name here. Its
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
    files = declared_tests(ENVOY, [WEB_FIXTURE_SOURCE], WEB_FIXTURE_TESTS, (AMD64,))
    declared = {name for names in files.values() for name in names}
    gone = ", ".join(name for name in sorted(WEB_FIXTURE_TESTS) if name not in declared)
    if gone:
        raise ValueError(f"WEB_FIXTURE_TESTS names {gone}, which no file of {WEB_FIXTURE_SOURCE} that the "
                         f"envoy-go-build runner's linux/amd64 build compiles declares as a test `go test` runs (a "
                         f"top-level `func Test…`, `func Fuzz…` or `func Example…`, TestMain apart): the fixture "
                         f"step's -run matches nothing of that name and passes green, and the unit step's -skip skips "
                         f"nothing, so the committed fixture is written by a run carrying no --check and checked by no "
                         f"run at all. Neither step passes -tags, so a declaration in a file that build leaves out, "
                         f"behind a tag or for another platform, is none here. Follow the rename, move the test to a "
                         f"file that build compiles, or, where the fixture check has gone, take the name out and "
                         f"leave the test to the unit step")


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


# The ten static checks of internal/dispatch/api, keyed by their path in this repository: the
# nine source guards and one_expression_guard_test.go, which reads the module's text for the same
# reason. Each reads the module's own source and carries `//go:build !race` for it, so the walk
# below would put all twenty-two of their tests in the plain step; they are left out of it instead,
# because the text-check job runs them by name and a test run twice is a test whose second run
# tells nobody anything. The exemption is held to the files it is about in three ways, so a
# twenty-third such test cannot quietly rejoin the plain step: a file named here that does not
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
    f"{ENVOY}/internal/dispatch/api/issue_route_writer_guard_test.go": {
        "TestOnlyTheIssuePatchWritesTheRoute",
        "TestOnlyTheJudgedWritersWriteAnIssuesStatusLabelsOrParent",
        "TestTheJudgedWriterGuardSeesAPlantedWriter",
        "TestTheRouteWriterGuardSeesAPlantedWriter"},
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
        "TestOnlyQueueReviewWritesReviews",
        "TestTheReviewWriterPatternsReadEveryNameOfTheTable",
        "TestTheReviewWriterScanFindsAnIdentifierNamingTheTable"},
    f"{ENVOY}/internal/dispatch/api/spec_writer_route_guard_test.go": {
        "TestEveryRouteThatWritesADocumentChecksItsWriter",
        "TestTheSpecWriterExemptionsStillHold",
        "TestTheSpecWriterGuardSeesAPlantedWriter"},
}

# The mention sender's claim model (internal/dispatch/api), keyed by path as TEXT_CHECK_TESTS is:
# the search over every interleaving of the sender's own code, and the test that holds the rules
# it restates to their SQL. Both carry `//go:build !race`, since the detector slows the search
# about sevenfold, so the walk below would put them in envoy-go's plain step; they are left out of
# it because the envoy-go-claim-model job runs them by name, alone on its runner, where the search
# has every core and a deadline measured for it. They are held to the files they are about as
# TEXT_CHECK_TESTS' are, by the same three refusals.
CLAIM_MODEL_TESTS = {
    f"{ENVOY}/internal/dispatch/api/comment_mention_claim_model_search_test.go": {
        "TestTheMentionSenderClaimModel"},
    f"{ENVOY}/internal/dispatch/api/comment_mention_claim_model_sql_test.go": {
        "TestTheClaimModelsRulesAreTheSQLsRules"},
}
# Every file whose tests a job of their own runs by name, and which therefore names no test of the
# plain step: the text-check job's and the claim model job's.
BY_NAME_TESTS = {**TEXT_CHECK_TESTS, **CLAIM_MODEL_TESTS}


def by_name_list(path: Path) -> str:
    """The name of the list of BY_NAME_TESTS that holds `path`'s row."""
    return "CLAIM_MODEL_TESTS" if path.as_posix() in CLAIM_MODEL_TESTS else "TEXT_CHECK_TESTS"


def by_name_job(path: Path) -> str:
    """The job that runs the tests of `path`'s BY_NAME_TESTS row by name."""
    return "the envoy-go-claim-model job" if path.as_posix() in CLAIM_MODEL_TESTS else "the text-check job"


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
    the plain run and dropping the one that does. A file BY_NAME_TESTS names is the exception:
    it names no test here, since a job of its own runs its tests by name (the text-check job, or
    the claim model's), and it is refused unless it carries the tag and its tests are exactly the
    names that list holds for it."""
    text = path.read_text()
    found = GO_BUILD_LINE.search(text)
    constraint = found.group(1) if found else ""
    if constraint not in ("", "race", "!race") and re.search(r"\brace\b", constraint):
        raise ValueError(f"{path}: `//go:build {constraint}` names race in a form ci_job_reads.py does not read; "
                         "teach race_impossible_tests_of() the form, or the plain step loses the file's tests")
    exempt = BY_NAME_TESTS.get(path.as_posix())
    if exempt is not None and constraint != "!race":
        raise ValueError(f"{path}: {by_name_list(path)} leaves this file's tests out of the plain step because "
                         f"{by_name_job(path)} runs them by name, but the file does not carry `//go:build !race`, "
                         "so the race build still compiles and runs them; restore the tag, or drop the row")
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
            raise ValueError(f"{path}: {by_name_list(path)} does not name this file's tests exactly - "
                             f"in the file and not in the list: {unnamed or 'none'}; in the list and not in the "
                             f"file: {stale or 'none'}. A test {by_name_job(path)} does not run by name "
                             "belongs in the plain step, so name it in both places or in neither")
        return set()
    return names


def race_impossible_tests(root, packages=None) -> dict:
    """Per package under `root`, in the order `packages` lists them, the tests only a plain run
    covers (race_impossible_tests_of). `packages` is each (path under `root`, directory) to read,
    and the envoy lane's (race_lane_packages) when it is not given. A package that has none is left
    out. A file BY_NAME_TESTS names under `root` that this walk never reads is refused: the row
    would otherwise go on exempting a file that has moved out of the lane, or been deleted, and say
    nothing."""
    found, read = {}, set()
    for under, directory in race_lane_packages(root) if packages is None else packages:
        names = set()
        for path in sorted(directory.glob("*_test.go")):
            read.add(path.as_posix())
            names |= race_impossible_tests_of(path)
        if names:
            found[under] = names
    gone = sorted(f for f in BY_NAME_TESTS if f.startswith(f"{root}/") and f not in read)
    if gone:
        raise ValueError(f"{', '.join(sorted({by_name_list(Path(f)) for f in gone}))} names {', '.join(gone)}, "
                         f"which the lane's walk of {root} does not read: the file moved out of the lane or is "
                         "gone, and its row now exempts nothing; follow the file, or drop the row")
    return found


def race_impossible_script(packages) -> str:
    """A plain run of the tests the detector makes impossible: one `go test` per package
    of `packages` (race_impossible_tests), each naming that package's own tests, anchored and
    sorted, in its -run, under a 10-minute timeout. A package of CLEANUP_DETECTOR_PACKAGES runs
    under the cleanup detector and with -v: go test prints a passing package's own output only
    with -v, and the document service's is evidence - its TestMain's count of services left
    alive, and the xid window rig's logs of which branch a load test reached
    (xid_window_load_linux_test.go, xid_window_ownps_linux_test.go) - which the job that ran the
    package plain under the detector alone printed for the same reason."""
    return "\n".join(f"{CLEANUP_DETECTOR + ' ' if under in CLEANUP_DETECTOR_PACKAGES else ''}"
                     f"go test{' -v' if under in CLEANUP_DETECTOR_PACKAGES else ''} -count=1 -timeout 10m "
                     f"-run '^({'|'.join(sorted(names))})$' ./{under}"
                     for under, names in packages.items())


# The step as the lane and packages/envoy's own source make it, so a test added under
# `//go:build !race` or behind raceEnabled changes this text and ci.yaml stops matching it. Its
# rows that measure time need a runner nothing else loads, and run in envoy-go-alone
# (RACE_IMPOSSIBLE_ALONE_*): every xid window test, which judges the statement-free gaps of a
# window against a bound; the xid rig's own tests (internal/dispatch/store/storetest), which read
# the thread clocks those gaps come from; the API's 200 ms bound on the issue list
# (TestAPriorityListAnswersWithinItsBoundOnEveryPlan); and the auth service's restart storm, whose
# five hundred connections must all be admitted within the callout's deadline. The rest, the bus's
# allocation count and the documents package's memory bounds and render storms, measure no time
# and run in envoy-go-race-impossible.
RACE_IMPOSSIBLE_STEP = "Test what the race build cannot run at all or at full size"
RACE_IMPOSSIBLE_ALONE_STEP = "Test the xid window rows, the API's issue-list bound and the auth service's storm, alone"
RACE_IMPOSSIBLE_ALONE_PREFIX = "TestXidWindow"
RACE_IMPOSSIBLE_ALONE_PACKAGES = ("internal/dispatch/store/storetest",)
RACE_IMPOSSIBLE_ALONE_TESTS = {
    "internal/dispatch/api": {"TestAPriorityListAnswersWithinItsBoundOnEveryPlan"},
    "internal/cipher/natscallout": {"TestACalloutStormAfterAServerRestartAdmitsEveryConnection"},
}


def race_impossible_alone(under, name):
    """Whether a test of the plain run the race build cannot make runs alone (the step's comment
    above says which and why)."""
    return (name.startswith(RACE_IMPOSSIBLE_ALONE_PREFIX) or under in RACE_IMPOSSIBLE_ALONE_PACKAGES
            or name in RACE_IMPOSSIBLE_ALONE_TESTS.get(under, ()))


RACE_IMPOSSIBLE_ALL = race_impossible_tests(ENVOY)
# A name RACE_IMPOSSIBLE_ALONE_TESTS holds that the run does not hold would move nothing and say
# nothing: refused, as is a package of RACE_IMPOSSIBLE_ALONE_PACKAGES the run has no test of.
_alone_gone = ([f"{under} {name}" for under, names in RACE_IMPOSSIBLE_ALONE_TESTS.items() for name in names
                if name not in RACE_IMPOSSIBLE_ALL.get(under, set())]
               + [under for under in RACE_IMPOSSIBLE_ALONE_PACKAGES if under not in RACE_IMPOSSIBLE_ALL])
if _alone_gone:
    raise ValueError(f"RACE_IMPOSSIBLE_ALONE_TESTS and RACE_IMPOSSIBLE_ALONE_PACKAGES name {', '.join(_alone_gone)}, "
                     f"which the plain run the race build cannot make does not hold: follow the test, or drop the row")
RACE_IMPOSSIBLE_ALONE_RUN = {under: {n for n in names if race_impossible_alone(under, n)}
                             for under, names in RACE_IMPOSSIBLE_ALL.items()
                             if any(race_impossible_alone(under, n) for n in names)}
RACE_IMPOSSIBLE_PACKAGES = {under: {n for n in names if not race_impossible_alone(under, n)}
                            for under, names in RACE_IMPOSSIBLE_ALL.items()
                            if any(not race_impossible_alone(under, n) for n in names)}
RACE_IMPOSSIBLE_SCRIPT = race_impossible_script(RACE_IMPOSSIBLE_PACKAGES)
RACE_IMPOSSIBLE_ALONE_SCRIPT = race_impossible_script(RACE_IMPOSSIBLE_ALONE_RUN)


def daemon_race_packages() -> list:
    """daemon-go's race step's packages (DAEMON_RACE_PACKAGES), in its order, as
    race_impossible_tests() reads them: each (path under DAEMON, directory)."""
    found = []
    for under in DAEMON_RACE_PACKAGES:
        directory = Path(DAEMON) / under
        if not directory.is_dir():
            raise ValueError(f"the daemon-go race step names {under}, which is no directory under {DAEMON}")
        found.append((under, directory))
    return found


# daemon-go's plain run of what its race step's packages hold that the race build cannot run: the
# race step runs those packages and the plain step leaves them out, so without this step such a
# test runs in no step of daemon-go. The first is internal/launcher's call-graph walk, under
# `//go:build !race`, which loads the launcher's whole program from source and builds its call
# graph twice. Built from the race step's own package list and the source, as the envoy step above
# is, so a test added under `//go:build !race` or behind raceEnabled in one of those packages
# changes this text and ci-job-daemon-go.yaml stops matching it.
DAEMON_RACE_IMPOSSIBLE_STEP = "Test what the race build cannot run"
DAEMON_RACE_IMPOSSIBLE_PACKAGES = race_impossible_tests(DAEMON, daemon_race_packages())
DAEMON_RACE_IMPOSSIBLE_SCRIPT = race_impossible_script(DAEMON_RACE_IMPOSSIBLE_PACKAGES)


# daemon-go's second run of internal/api's shape gate, stamped with HEAD's contract entries in
# main's merge order (scripts/daemon-contract-order.sh), in two steps. The first,
# DAEMON_CONTRACT_ORDER_STEP, fetches the history that script reads and writes the stamp as its
# output; the gate, DAEMON_STAMPED_GATE_STEP, is one go test of the gate's two tests linked with
# that output. The gate is one line because go-test-step-expansion.py's step reader refuses a
# step whose $(…) call is not of go-tagged-tests.sh. -ldflags and -run are no form
# check-ci-filters.sh reduces, so the gate step's whole script is pinned.
DAEMON_CONTRACT_ORDER_STEP = "Stamp HEAD's contract entries in main's merge order"
DAEMON_STAMPED_GATE_STEP = "Test the shape gate in main's merge order"
DAEMON_STAMPED_GATE_SCRIPT = (
    "go test -count=1 -timeout 1m ./internal/api/ "
    "\"-ldflags=-X=github.com/sjawhar/legion/daemon/internal/api.contractOrder=${{ steps.contract-order.outputs.stamp }}\" "
    "-run '^(TestEveryAnswerAtEveryContractIsTheShapeItsRecordDescribes|TestTheRecordSetIsOneRecordPerAnswerPerContract)$'")


# The text-check job: the three modules' text checks, and the twenty-two tests of the Dispatch API's
# ten static checks - its nine source guards and one_expression_guard_test.go. They read the
# module's source and no database, so they belong here rather than in the Postgres-backed
# suites, and they are the one thing here the job cannot name by package: their package holds
# the API's own Postgres-backed tests too, so the step selects them by name. The -run list is
# built from TEXT_CHECK_TESTS, the same declaration that leaves exactly those twenty-two out of
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

# The claim model's job: the mention sender's claim model and the test of the rules it restates,
# by name (CLAIM_MODEL_TESTS), the same declaration that leaves exactly those two out of envoy-go's
# plain step. -run and -v are outside the simple form, so the step is pinned whole. A -run moves
# no closure: the step compiles every test file of internal/dispatch/api, the source guards that
# walk the module's source among them, so this gate reads what the API's plain tests read.
CLAIM_MODEL_JOB = "envoy-go-claim-model"
CLAIM_MODEL_PACKAGE = "./internal/dispatch/api"
CLAIM_MODEL_STEP = "Search every order of the mention sender's claims"
# Twice the 19 min 18 s the step took on an ubuntu-24.04 runner at three periods; the job's
# timeout-minutes is this and ten.
CLAIM_MODEL_TIMEOUT = "40m"
CLAIM_MODEL_SCRIPT = (f"go test -count=1 -timeout {CLAIM_MODEL_TIMEOUT} -v -run "
                      f"'^({'|'.join(sorted(name for names in CLAIM_MODEL_TESTS.values() for name in names))})$' "
                      f"{CLAIM_MODEL_PACKAGE}")


RUN_SERVER = "packages/dispatch/e2e/run-server.sh"
BUILD_BINARIES = "docs/site/scripts/build-binaries.sh"
DISPATCH_SERVER_BUILD = "flock ./.dispatch-e2e.lock go build -o ./dispatch-e2e ./cmd/dispatch"
# The three commands of the real broker's harness rows, and the Dispatch server with them: the
# binaries job of dispatch-browser.yaml builds all four, since every other job of that file
# either runs in a container, and so runs no go command of its own (that file says why), or
# names what it built on DISPATCH_E2E_SERVER_BIN, DISPATCH_E2E_BROKER_BIN_DIR and
# DISPATCH_E2E_SIGNIN_ISSUER_BIN and executes it. The last of those is the multiprocess job's
# sign-in step, whose dev OIDC issuer is the ./cmd/dev-oidc-issuer this build makes, so no go
# command is left outside this job.
BROKER_PKGS = ["./cmd/cipherd", "./cmd/dev-oidc-issuer", "./internal/cipher/ciphertest/requestseed"]
# The steps of .github/workflows/dispatch-browser.yaml this table cites by name.
BROWSER_BINARIES_STEP = "Build the binaries and the dashboard the browser suite runs"
# The whole step, pinned: it builds the four Go commands, and starts
# .github/scripts/dispatch-browser-build-dashboards.sh beside them for the dashboard and, on the
# full lane, the second rollout bundle, as two background jobs it waits on by process id. Its two
# go commands, chained in one background subshell, stay in the step's own text, where this entry
# pins them, and not in that script, which starts no go command: a script Run is no form this
# gate can hold (its comment says why). Nothing in the step is in the simple form
# check-ci-filters.sh reduces - a `cd`, a `&`, a `$!`, a `${{ }}` - so the entry below pins the
# text and names the one closure its go commands compile.
BROWSER_BINARIES_SCRIPT = """\
set -euo pipefail
mkdir -p "$RUNNER_TEMP/dispatch-e2e-bin"
(cd packages/envoy && CGO_ENABLED=0 go build -ldflags='-s -w' -o "$RUNNER_TEMP/dispatch-e2e-bin/" ./cmd/dispatch ./cmd/dev-oidc-issuer ./internal/cipher/ciphertest/requestseed && CGO_ENABLED=0 go build -ldflags='-s -w' -o "$RUNNER_TEMP/dispatch-e2e-bin/broker" ./cmd/cipherd) &
commands=$!
.github/scripts/dispatch-browser-build-dashboards.sh "${{ inputs.lane }}" &
dashboards=$!
status=0
wait "$commands" || { echo "::error::the Go commands the browser suite runs did not build"; status=1; }
wait "$dashboards" || status=1
exit "$status\""""
BROWSER_SHARD_SPECS = "Browser-test dispatch dashboard (shard ${{ matrix.shard }} of 2)"
BROWSER_ALONE_SPECS = "Browser-test dispatch dashboard (the three wall-clock projects)"
BROWSER_REAL_SPECS = "Browser-test dispatch dashboard against the real stack (pull request)"
BROWSER_STUB_SPECS = "Browser-test dispatch dashboard against recordings (pull request)"
# The steps that read the stacks' connection limit before a browser step starts its stacks
# (e2e/harness-postgres.ts takes it as DISPATCH_E2E_PG_CONNECTIONS): the binaries job's, whose
# output every job of dispatch-browser.yaml that starts stacks reads, since none of them sets up
# Go - the shard, real and alone jobs' containers have none, and the multiprocess and visual
# jobs install none - and the two-build job's own, which sets up Go for its own build. A name per
# job, since the check finds a step by name across the workflow, and a script per job: the
# binaries job's `go run` carries CGO_ENABLED=0 on the line, as that job's build step's go builds
# do, so it reuses their objects, and the two-build job's runs on the runner's default cgo, as
# that job's own build does. Pinned whole, since `go run` is not a form the simple form reduces.
BROWSER_CONNECTIONS_STEP = "Read the connections each stack's Postgres must allow"
TWO_BUILDS_CONNECTIONS_STEP = "Read the connections the two-build stack's Postgres must allow"
BROWSER_CONNECTIONS_SCRIPT = """\
connections=$(CGO_ENABLED=0 go run ./internal/dispatch/store/cmd/connections)
[[ "$connections" =~ ^[1-9][0-9]*$ ]] || { echo "::error::go run ./internal/dispatch/store/cmd/connections printed '$connections', not a positive whole number"; exit 1; }
echo "connections=$connections" >> "$GITHUB_OUTPUT\""""
TWO_BUILDS_CONNECTIONS_SCRIPT = """\
connections=$(go run ./internal/dispatch/store/cmd/connections)
[[ "$connections" =~ ^[1-9][0-9]*$ ]] || { echo "::error::go run ./internal/dispatch/store/cmd/connections printed '$connections', not a positive whole number"; exit 1; }
echo "connections=$connections" >> "$GITHUB_OUTPUT\""""
BROWSER_MP_SPECS = "Browser-test dispatch dashboard against two server processes"
BROWSER_MP_SIGNIN = "Browser-test a sign-in across two server processes"
BROWSER_EMBEDDER_SPECS = "Browser-test the bar's two tiers against a counting embedder"
BROWSER_MERGE_STEP = "Merge the legs' blob reports into one report"
BROWSER_SELECT_STEP = "Take the newest attempt of each leg's blob report"
BROWSER_READ_ONE_STEP = "Reading one, the rows"
# The merge job's three other readings and its verdict, each a step running a script of its own.
BROWSER_READ_TWO_STEP = "Reading two, the flake rate"
BROWSER_READ_THREE_STEP = "Reading three, the leg exits with no failed row"
BROWSER_READ_FOUR_STEP = "Reading four, the row durations"
BROWSER_VERDICT_STEP = "Fail when a leg failed or the merged report holds a failed row"
BROWSER_BLOBS_REFUSED_STEP = "Report a refused blob download as a refused read"
BROWSER_MACHINE_TIME_STEP = "Reading five, the lane's machine time"
BROWSER_PG_STEP = "Read the lanes' PostgreSQL and time its provider"
BROWSER_WAIT_STORE_STEP = "Wait for the S3-compatible server"
# One name, three steps: the shard job's, the real job's and the alone job's second read of the
# binaries artifact.
BROWSER_FETCH_STEP = "Download the binaries again, after a first attempt that did not complete"
# One name, three steps: the shard job's and the real job's, which run one script, and the alone
# job's, which runs a script of its own, since the shard and real jobs' machines also run the
# file store whose log those jobs collect.
BROWSER_RECORD_STEP = "What the run left behind"
# The visual job's own step, which runs e2e/visual-run.sh: that script seeds on the host and
# then runs Playwright in the pinned container. Neither it nor e2e/run-server.sh builds a
# Dispatch server - that job names the binaries job's own on DISPATCH_E2E_SERVER_BIN - so this
# job starts no go command and adds no Run of its own.
BROWSER_VISUAL_SPECS = "Browser-test dispatch dashboard against its baselines"
# The steps that run the browser suite, named together wherever a Run or a read cites the
# whole suite: one list, so a step renamed above reaches every citation of it.
BROWSER_SUITE_STEPS = (
    f"{at_browser(BROWSER_SHARD_SPECS)}, {at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)}, "
    f"{at_browser(BROWSER_MP_SPECS, BROWSER_MP)}, {at_browser(BROWSER_MP_SIGNIN, BROWSER_MP)}, "
    f"{at_browser(BROWSER_REAL_SPECS, BROWSER_REAL)}, {at_browser(BROWSER_STUB_SPECS, BROWSER_STUB)}, "
    f"{at_browser(BROWSER_EMBEDDER_SPECS, BROWSER_REAL)}, {at_browser(BROWSER_EMBEDDER_SPECS, BROWSER_ALONE)} and "
    f"{at_browser(BROWSER_VISUAL_SPECS, BROWSER_VISUAL)}"
)
STALL = "envoy-session-stall"
STALL_CLIENT = "Stall test of envoy-client's NATS connection options"
LOADTEST_RUN_STEP = "One-run-at-a-time test for loadtest/run-step.sh and run-series.sh (fakes go and uv; no stack)"
LOADTEST_LOCK = "Lock test for loadtest/common.sh (inherited lock, refusals, holders in another PID namespace; no stack)"
LOADTEST_CLUSTER = "Test loadtest/cluster/cluster.sh (fakes kubectl, gh and curl; no cluster) and compare nats.yaml with a fresh render"
# The step of envoy-go-checks that runs the scan tests of approve-check's session, which spawn
# scripts/approve/scan.ts.
SCAN_TESTS_STEP = "Scan tests of approve-check's session (bun; a stand-in Dispatch, no stack)"
# The step that waits for the script harnesses a job queued through .github/scripts/harness-pool.sh
# and is the verdict on them; envoy-go-checks and the test job each end with one.
HARNESS_POOL_WAIT_STEP = "Wait for the script harnesses and judge them"
# The check that no migration a database may have run is edited without its replaces line: a
# script that builds the command with `go build` and runs the binary, so pinned whole, and the Run
# stands for the command it compiles. EDITED_MIGRATIONS_BASE, which the step runs first, picks the
# base and starts no go command.
EDITED_MIGRATIONS_STEP = "Check that no migration a database may have run is edited without its replaces line"
EDITED_MIGRATIONS_BASE = ".github/scripts/edited-migrations-base.sh"
EDITED_MIGRATIONS_SCRIPT = "\n".join([
    f'base=$(../../{EDITED_MIGRATIONS_BASE})',
    'go build -o "$RUNNER_TEMP/edited-migrations" ./internal/pgmigrate/cmd/edited-migrations',
    '"$RUNNER_TEMP/edited-migrations" --base "$base"',
])
SMOKE_IMAGE_STEP = "Build the listener image the smoke test runs against"
BASE_IMAGES_STEP = "Read the smoke image's base images"
FOLD_CHECK_STEP = "Check every changelog fragment renders"
CHECK_STEP = "Check every CI filter matches every file its job reads"
TC_LINKS_STEP = "Check no command or self-starting test binary links testcontainers-go"
ACTIONLINT_STEP = "Check every workflow file with actionlint"
GUIDES_STEP = "Check the guides' semantic line breaks"
COPY_STEP = "Copy every image the package does not hold"
PACKAGE_STEP = "Check the package holds every row of .github/ci-images.yaml"
TOOL_COPY_STEP = "Fill the copy of every tool asset the package does not hold"
# The steps of the tool-fallback job the reads below cite. The job is one long drive of
# .github/scripts/ci-tools-proof.sh, so each file it reads is read at a step with a name.
TOOL_BLOB_STEP = "Check the package holds the blob of every mirrored row"
TOOL_ROWS_STEP = "Print every row beside the two call sites that read its digest"
TOOL_NOWHERE_STEP = "Check all eight installs with nowhere to go failed, and failed in their own time"
TOOL_VERSIONS_STEP = "Check every tool is the version its row pins"
TOOL_FINAL_STEP = "Print what every tool answers now"
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
# The step that reads every other -aarch64 row's healthy install against its row.
TOOL_ARM_ROWS_STEP = "Read what the release assets served of the other arm64 rows"
# The session message-wait suite's own job: the test job's one step that needs a database.
SESSIONS_JOB = "sessions-script"
SESSIONS_STEP = "Test the session message-wait script"

# ------------------------------------------------- the test job's script harnesses, one gate each
# Each `*.test.sh` step of the test job tests one script of this repository, and runs behind a
# filter of its own naming that harness and that script, which the step reads out of the changes
# job's one list of the filters that matched (.github/ci-notes/ci.md#one-list-for-the-test-jobs-step-gates).
# Before this table one pattern, `.github/scripts/*`, stood for all of them on the job's own
# filter, so a change to any one script ran every harness - and, while the daemon's Go suite was
# in the same job, 1,083 s (median) of Go tests with them (dispatch://INFRA-1413). The job's
# filter now takes what these gates read, as it takes every step gate's reads
# (ci_filter_coverage.gate_read_sets), so a change to a harness or its subject selects the job
# and exactly that step's filter is in the list it reads.
#
# Every harness sources test-lib.sh for its `check`, `is` and `summary`, so each of those gates
# reads it and a change to it runs them all.
SCRIPTS = ".github/scripts"
TEST_LIB = f"{SCRIPTS}/test-lib.sh"


def harness(filter, step, harness_script, subjects, lib=True):
    """A gate for one harness step of the test job: the harness itself, test-lib.sh where the
    harness sources it, and `subjects`, the (path, why) pairs of what else that step reads - the
    script it tests first among them. The gate is the step's, so its `if:` must read this filter
    out of the list and no other (rule 2)."""
    entries = [(harness_script, at("test", step))]
    if lib:
        entries.append((TEST_LIB, f"{harness_script} sources it for check/is/summary"))
    return gate(filter, "test", step=step,
                reads=reads(*entries, *subjects))


# The private-name port harness, a job of its own since dispatch://INFRA-1413: it is the one
# harness of this repository that builds a binary of this head, so the filter that selects it
# has to name that build's whole closure (LEGION_BUILD). While the step was the test job's, that
# closure was on the `test` filter and a change to any package of the daemon ran thirty-odd shell
# harnesses and the Bun suites of scripts/ beside it; a gate on the step inside that job was no
# cure either, since the job's own filter takes every step gate's reads, so the closure would
# select the `test` job all the same.
PRIVATE_NAMES_PORT_JOB = "private-names-port"
PRIVATE_NAMES_PORT_STEP = "Test the private-name rules' port against the frozen script"
PRIVATE_NAMES_PORT_BUILD = script_run(
    f"{SCRIPTS}/check-private-names-port.test.sh",
    '(cd "$repo_root/packages/daemon" && go build -o "$engine" ./cmd/legion) || {',
    at(PRIVATE_NAMES_PORT_JOB, PRIVATE_NAMES_PORT_STEP), DAEMON, [LEGION_BUILD], mode="build")


# The guide-layout step's own reads, declared once, for its gate
# (.github/ci-gates/tooling-checks.guide_layout.py). The tooling-checks job's filter takes them
# too, as it takes every step gate's reads: a job's `if:` may test one changes output only
# (ci_filter_coverage refuses a second), so the step's gate decides nothing until the job's own
# filter selects the job for the same change.
GUIDE_LAYOUT_READS = reads(
    ("scripts/semantic-line-breaks.guides.test.ts", at(TOOLING_JOB, GUIDES_STEP)),
    ("scripts/semantic-line-breaks.ts", "`scripts/semantic-line-breaks.guides.test.ts#Bun.spawnSync` spawns it as `scripts/semantic-line-breaks.guides.test.ts#SCRIPT`"),
    ("AGENTS.md", "`scripts/semantic-line-breaks.guides.test.ts#the root guide, the contributing guide, the e2e README and the Kubernetes doc keep the layout` and `scripts/semantic-line-breaks.guides.test.ts#GUIDES` runs the checker over GUIDES"),
    ("CONTRIBUTING.md", "`scripts/semantic-line-breaks.guides.test.ts#the root guide, the contributing guide, the e2e README and the Kubernetes doc keep the layout` and `scripts/semantic-line-breaks.guides.test.ts#GUIDES` runs the checker over GUIDES"),
    ("scripts/e2e/README.md", "`scripts/semantic-line-breaks.guides.test.ts#the root guide, the contributing guide, the e2e README and the Kubernetes doc keep the layout` and `scripts/semantic-line-breaks.guides.test.ts#GUIDES` runs the checker over GUIDES"),
    ("docs/kubernetes.md", "`scripts/semantic-line-breaks.guides.test.ts#the root guide, the contributing guide, the e2e README and the Kubernetes doc keep the layout` and `scripts/semantic-line-breaks.guides.test.ts#GUIDES` runs the checker over GUIDES"),
    ("packages/envoy/AGENTS.md", "`scripts/semantic-line-breaks.guides.test.ts#the Envoy guide and the Dispatch server README keep the layout` runs the checker over it"),
    ("packages/envoy/cmd/dispatch/README.md", "`scripts/semantic-line-breaks.guides.test.ts#the Envoy guide and the Dispatch server README keep the layout` runs the checker over it"),
)

# docs-pages.yaml builds and publishes main's site in a workflow of its own (its header says why),
# so its on.push.paths stands in for the docs filter on main.
PUSH_WORKFLOWS = {"docs": ".github/workflows/docs-pages.yaml"}


def browser_gate(filter, job):
    """The gate of ci.yaml's dispatch-browser or dispatch-browser-pr job, two of ci.yaml's three
    callers of dispatch-browser.yaml (dispatch-references is the third). Every caller runs the
    same file, whose jobs carry ids of their own rather than one named for any caller, so each job
    of it runs under every caller (ci_filter_coverage.called_job_owners) and each gate reads all
    of it, the jobs a lane skips by its `lane` input among them: which jobs run is a literal each
    caller passes, not a gate."""
    # The browser suite, every row of it: the binaries job, the shards, real and stub,
    # the wall-clock projects and counting embedder steps, two-process, visual and merge, all in
    # dispatch-browser.yaml, which ci.yaml's dispatch-browser, dispatch-browser-pr and
    # dispatch-references jobs and the nightly call (at_browser cites their steps).
    #
    # ci_filter_coverage.py's check_table reads a gate whose ci.yaml job calls a local workflow
    # over the jobs of that workflow, so dispatch-browser.yaml's go commands, `env:` blocks and
    # `shell:` keys are held here exactly as a job's own steps are: the binaries job's build
    # step is the exact entry below, and a name outside ENV_ALLOWED, or a `shell:`, in that job
    # or in the file around it drops that step out of the check's reduction, which then counts
    # it as reading every Go file of the repository.
    # The gate keeps `workflow=CI`, as the docs gate does and for the same reason:
    # dispatch-browser.yaml carries job ids of its own rather than one named for the caller, so ci.yaml is the file that
    # holds a job named for the caller, and each citation names the browser job it is in.
    #
    # This gate lists no script Run, and cannot: a Run pinned to a script's text is reduced only
    # where nothing around the ci.yaml job can move what the script compiles, and around_job
    # reads that over every job of the called workflow. The shard, real and alone jobs run in a
    # container (dispatch://INFRA-1443), which shell_reason counts as exactly such a thing, so a
    # script Run here would fall to reading every Go file of the repository. It costs this
    # filter nothing. Those jobs run no go command at all - run-server.sh and run-broker.sh
    # execute the binaries the first entry below builds. The stub job starts no stack, and
    # neither multiprocess nor visual runs Go: they name those binaries on DISPATCH_E2E_SERVER_BIN,
    # DISPATCH_E2E_BROKER_BIN_DIR and, in the multiprocess job's sign-in step,
    # DISPATCH_E2E_SIGNIN_ISSUER_BIN, which e2e/run-signin-issuer.sh executes in place of its
    # own build of ./cmd/dev-oidc-issuer (dispatch://INFRA-1109). No job's
    # e2e/harness-postgres.ts runs a go command in CI either: each job that starts stacks there
    # hands it the connection limit that the binaries job's step the second entry pins reads.
    # So the binaries job's two pinned steps hold every go command of the file.
    gate_ = gate(filter, job, exact=[
        # Every binary these legs execute, and the dashboard beside them, built once in the
        # binaries job and downloaded by the jobs that run them. The stub leg takes only
        # the dashboard. This is the closure those builds compile and no other. The step names an output
        # (-o), pins CGO_ENABLED on the line, and runs its two go builds - the Dispatch server,
        # its dev OIDC issuer and the seeding step, then ./cmd/cipherd as broker, chained in one
        # subshell - and the dashboards' script as two background jobs it waits on, none of which
        # the simple form reduces, so the step is pinned whole; the Run carries that CGO_ENABLED,
        # since the closure go lists without cgo is the one these builds compile. Its directory
        # is the repository root, which is where the step runs - the subshell `cd`s into
        # packages/envoy inside the pinned script before either go command, and the Run below
        # names that directory, which is where the closure is listed.
        pinned_step(BROWSER_BINARIES, BROWSER_BINARIES_STEP, ".", BROWSER_BINARIES_SCRIPT, [
            (ENVOY, ["./cmd/dispatch"] + BROKER_PKGS,
             "CGO_ENABLED=0 go build of the Dispatch server, the real broker, its dev OIDC issuer and the seeding step",
             {"mode": "build", "env": {"CGO_ENABLED": "0"}}),
        ], workflow=BROWSER_SITE),
        # The stacks' connection limit, read on the host once for the run: every job that starts
        # stacks takes it from the binaries job's output, since none of them sets up Go. The
        # `go run` pins CGO_ENABLED=0 on the line, as the build step's go builds do, and the Run
        # carries it for the same reason as the entry above.
        pinned_step(BROWSER_BINARIES, BROWSER_CONNECTIONS_STEP, ENVOY, BROWSER_CONNECTIONS_SCRIPT, [
            (ENVOY, ["./internal/dispatch/store/cmd/connections"],
             "CGO_ENABLED=0 go run ./internal/dispatch/store/cmd/connections, which prints testlimits.ServerConnections",
             {"mode": "build", "env": {"CGO_ENABLED": "0"}}),
        ], workflow=BROWSER_SITE),
    ], reads=reads(
        ("packages/dispatch/**", f"{BROWSER_SUITE_STEPS} run the suite, "
         f"{at_browser(BROWSER_BINARIES_STEP, BROWSER_BINARIES)} builds the SPA the rows read, and "
         f"{at_browser(BROWSER_MERGE_STEP, BROWSER_MERGE)} merges their reports, in packages/dispatch"),
        ("packages/proof-editor/**", "`packages/dispatch/web/vite.config.ts#@legion/proof-editor` and `packages/dispatch/web/vite.config.ts#proof-sdk-upstream/src` aliases (build:web)"),
        # packages/contracts/src is no read of this table's either: the filter's listed block holds
        # the modules dispatch_ui's tsc programs compile and the dispatch command's bundle loads,
        # which check-ts-filters.sh writes, and ci_ts_programs.py's LISTED says why this suite
        # loads those and no other; each job's `Build contracts for dispatch e2e` step runs
        # `build:dispatch`, whose entries compile nothing outside dispatch_ui's.
        ("packages/envoy-client/bin/dispatch.ts", "`packages/dispatch/e2e/inbox-cli.e2e.ts#runDispatch` runs it through `packages/dispatch/e2e/dispatch-cli.ts#dispatchEntry`"),
        (f"{FIX}/priority-ids.json", "`packages/dispatch/e2e/priorities.e2e.ts#idFixture` reads the shared list of the priority "
                                     "id rule's cases by path"),
        (f"{ENVOY}/internal/dispatch/pmdoc/schema/blocks.json", "`packages/dispatch/e2e/engine-reading.ts#blockSchema`"),
        (f"{ENVOY}/internal/dispatch/api/testdata/visual-shot-reads.json",
         "read at module load by packages/dispatch/e2e/visual-guard.ts (SHOT_READS_FILE), which "
         f"e2e/visual.e2e.ts and e2e/visual-guard.e2e.ts import: {at_browser(BROWSER_VISUAL_SPECS, BROWSER_VISUAL)} "
         f"holds each visual row's reads and live views to it, and {at_browser(BROWSER_SHARD_SPECS)} runs the guard's own row"),
        ("packages/contracts/schemas/dispatch-openapi.json",
         "read at module load by packages/dispatch/e2e/visual-guard.ts (OPENAPI_FILE), which "
         f"e2e/visual.e2e.ts and e2e/visual-guard.e2e.ts import: {at_browser(BROWSER_VISUAL_SPECS, BROWSER_VISUAL)} "
         "resolves each visual row's read to the route the table serves it by"),
        (f"{ENVOY}/internal/dispatch/pmdoc/testdata/fixtures.json", "read at `packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts#pmdocFixtures` and `packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts#TABLE_PATH`, from the new URL TABLE_PATH holds, in the module the browser rows of the seven markdown parity families it holds import; the web unit tests that read the same table run in dispatch-ui, which declares it too"),
        (f"{ENVOY}/internal/dispatch/api/testdata/text-refusals.json", "read at `packages/dispatch/e2e/artifact-dry-run.e2e.ts#rows` and `packages/dispatch/e2e/artifact-dry-run.e2e.ts#textRefusalsFile`, from the new URL textRefusalsFile holds, for that spec's rows, which are the list's rows in each combination the spec runs"),
        ("scripts/e2e/lib/held-port.py", "`packages/dispatch/e2e/run-server.sh#held_port` holds each stack's server port through it"),
        (f"{ENVOY}/scripts/lib/pg-url.sh", "`packages/dispatch/e2e/run-broker.sh#$e2e_dir/../../envoy/scripts/lib/pg-url.sh` sources it, and `packages/dispatch/e2e/harness-listeners.ts#realBrokerListeners` "
                                           f"starts that script in a one-process run on the fake broker: {at_browser(BROWSER_SHARD_SPECS)}, "
                                           f"{at_browser(BROWSER_REAL_SPECS, BROWSER_REAL)} and {at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)}"),
        (".github/scripts/dispatch-browser-samples.sh",
         f"{at_browser(BROWSER_SHARD_SPECS)}, {at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)}, "
         f"{at_browser(BROWSER_REAL_SPECS, BROWSER_REAL)} and {at_browser(BROWSER_STUB_SPECS, BROWSER_STUB)} "
         "start it and read its two summaries"),
        (".github/scripts/dispatch-browser-postgres.sh",
         f"{at_browser(BROWSER_PG_STEP)}, {at_browser(BROWSER_PG_STEP, BROWSER_REAL)} "
         f"and {at_browser(BROWSER_PG_STEP, BROWSER_ALONE)} "
         "run it for the reading of the image's PostgreSQL their stacks start"),
        (".github/scripts/dispatch-browser-latest-blobs.sh", at_browser(BROWSER_SELECT_STEP, BROWSER_MERGE)),
        (".github/scripts/dispatch-browser-fetch-binaries.sh",
         f"{at_browser(BROWSER_FETCH_STEP)}, {at_browser(BROWSER_FETCH_STEP, BROWSER_REAL)} "
         f"and {at_browser(BROWSER_FETCH_STEP, BROWSER_ALONE)} run it "
         "when actions/download-artifact's first read of the binaries did not complete"),
        (".github/scripts/dispatch-browser-run-reads.sh",
         f"{at_browser(BROWSER_READ_ONE_STEP, BROWSER_MERGE)} runs its jobs mode over this run's "
         f"own attempt's jobs, {at_browser(BROWSER_BLOBS_REFUSED_STEP, BROWSER_MERGE)} its "
         "blobs-refused mode, which prints the line for a blob download actions/download-artifact "
         f"could not complete, and {at_browser(BROWSER_MACHINE_TIME_STEP, BROWSER_MERGE)} its "
         "machine-time mode, which holds the lane's summed job seconds to the budget that mode "
         "carries"),
        (".github/scripts/dispatch-browser-build-dashboards.sh",
         f"{at_browser(BROWSER_BINARIES_STEP, BROWSER_BINARIES)} starts it beside its go build for "
         "the dashboard bundle and, on the full lane, the second rollout bundle, which it builds "
         "in packages/dispatch and copies for the upload; it starts no go command"),
        (".github/scripts/dispatch-browser-wait-for-store.sh",
         f"{at_browser(BROWSER_WAIT_STORE_STEP)} and {at_browser(BROWSER_WAIT_STORE_STEP, BROWSER_REAL)} "
         "run it to wait for the file store the step "
         "after the checkout started, which it makes a bucket in and writes, reads back and "
         "deletes one object in"),
        (".github/scripts/dispatch-browser-shard-suite.sh",
         f"{at_browser(BROWSER_SHARD_SPECS)} runs it for this leg's half of the lane's row time"),
        (".github/scripts/dispatch-browser-pr-suite.sh",
         f"{at_browser(BROWSER_REAL_SPECS, BROWSER_REAL)} and {at_browser(BROWSER_STUB_SPECS, BROWSER_STUB)} "
         "run it for their disjoint selections of tagged rows"),
        (".github/scripts/dispatch-browser-shard-record.sh",
         f"{at_browser(BROWSER_RECORD_STEP)} and {at_browser(BROWSER_RECORD_STEP, BROWSER_REAL)} "
         "run it to print and collect the file store's log and the stacks' own logs"),
        (".github/scripts/dispatch-browser-alone-suite.sh",
         f"{at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)} runs it for the three unsharded "
         "wall-clock projects, one after another"),
        (".github/scripts/dispatch-browser-embedder.sh",
         f"{at_browser(BROWSER_EMBEDDER_SPECS, BROWSER_REAL)} and {at_browser(BROWSER_EMBEDDER_SPECS, BROWSER_ALONE)} "
         "run it for the command bar's two counting-embedder rows"),
        (".github/scripts/dispatch-browser-alone-record.sh",
         f"{at_browser(BROWSER_RECORD_STEP, BROWSER_ALONE)} runs it to print and collect the "
         "stack's own logs, there being no file store on that machine"),
        (".github/scripts/dispatch-browser-visual-suite.sh",
         f"{at_browser(BROWSER_VISUAL_SPECS, BROWSER_VISUAL)} runs it for the stack and the "
         "e2e/visual-run.sh the comparison against the baselines needs"),
        (".github/scripts/dispatch-browser-merge-reports.sh",
         f"{at_browser(BROWSER_MERGE_STEP, BROWSER_MERGE)} runs it to join the legs' blob "
         "reports into the one HTML and JSON report of the run"),
        (".github/scripts/dispatch-browser-reading-rows.sh",
         f"{at_browser(BROWSER_READ_ONE_STEP, BROWSER_MERGE)} runs it for the rows each leg's "
         "log printed against the rows the merged report holds, for the legs that owed a "
         "blob report and uploaded none, and for the shard legs' row lists held against each other"),
        (".github/scripts/dispatch-browser-reading-flake-rate.sh",
         f"{at_browser(BROWSER_READ_TWO_STEP, BROWSER_MERGE)} runs it for the count and the "
         "percentage of the rows that passed on a retry"),
        (".github/scripts/dispatch-browser-reading-leg-exits.sh",
         f"{at_browser(BROWSER_READ_THREE_STEP, BROWSER_MERGE)} runs it to name every leg that "
         "concluded anything but success beside what its own blob report holds"),
        (".github/scripts/dispatch-browser-reading-row-durations.sh",
         f"{at_browser(BROWSER_READ_FOUR_STEP, BROWSER_MERGE)} runs it for the row count, the "
         "median and the 95th percentile of each layer's passing rows"),
        (".github/scripts/dispatch-browser-verdict.sh",
         f"{at_browser(BROWSER_VERDICT_STEP, BROWSER_MERGE)} runs it to fail the job on an "
         "upstream result that is neither success nor skipped and on a failed row in the "
         "merged report"),
        (".github/scripts/gh-read.sh",
         "`.github/scripts/dispatch-browser-run-reads.sh#$script_dir/gh-read.sh` sources it, so the merge job's read of its own attempt's "
         "jobs takes its retry ladder, and the blobs-refused mode makes no request"),
        # The image the shard, real and alone jobs run in, and what decides its bytes.
        # No step reads these three: the container key does before the first step runs.
        (".github/ci-images.yaml",
         "dispatch-browser.yaml's shard, real and alone jobs run in the image at the digest the "
         "one built row of this file carries"),
        (".github/docker/browser.Dockerfile",
         "the Dockerfile that image is built from: its bytes are the browsers, their host "
         "packages and the PostgreSQL the containerised browser jobs "
         "start"),
        (".github/workflows/browser-image.yaml",
         "the build that publishes that image: its labels, platforms and action pins decide the "
         "bytes of the image dispatch-browser.yaml's shard, real and alone jobs pin"),
        ("scripts/third-party-notices.ts", "`packages/dispatch/web/vite.config.ts#thirdPartyNotices` (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
    ) + envoy_client_dep("imported by packages/envoy-client/bin/dispatch.ts, which "
                         "`packages/dispatch/e2e/inbox-cli.e2e.ts#runDispatch` runs")
      + setup_go(BROWSER_BINARIES, f"{ENVOY}/go.mod", BROWSER_SITE)
      + bun_job(BROWSER_SHARD, BROWSER_SITE)
      + CI_SELF + BROWSER_SITE_SELF, workflow=CI)
    # Three package trees the suite's filter named by hand before the filters were computed,
    # beyond what the gate's reads above select: both callers' filters keep them, each tree's
    # tests aside.
    return with_reads(gate_, [
        Read(f"{ENVOY}/internal/dispatch/delivery/**", f"filter {filter} held it by hand before the filters were computed", but=("**/*_test.go",)),
        Read(f"{ENVOY}/internal/dispatch/pmdoc/**", f"filter {filter} held it by hand before the filters were computed", but=("**/*_test.go",)),
        Read(f"{ENVOY}/internal/cipher/policy/**", f"filter {filter} held it by hand before the filters were computed", but=("**/*_test.go",)),
    ])



# One entry per step of ci.yaml that runs a package under a build tag and passes -run: the job
# whose untagged run of that package the -run leans on, which check-ci-filters.sh holds to running
# in every run the tag step runs (check_tag_runs). The note says which step runs the untagged
# tests, the judgement the check cannot derive. The envoy_go entry pairs a job with itself: the
# untagged run is a step of the same job, so the filters are the same one and the subset holds
# trivially; it is here all the same, so the rule covers every -run step and a later move of
# one to another job is caught. What the check holds an untagged job's filter to is what the tag
# step's gate reads and the tag step filter's listed block, never what the tag filter selects, so
# that entry holds its filter to nothing its own gate does not read already, and rules 9 and 17
# judge it as they judge any other.
TAG_RUNS = [
    TagRun("dispatch_memory", "dispatch-memory", MEMORY_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/dispatch's untagged tests",
           job_file("dispatch-memory")),
    TagRun("dispatch_multiprocess", "dispatch-multiprocess", DISPATCH_MP_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/dispatch's untagged tests",
           job_file("dispatch-multiprocess")),
    # The two-build job's Go step has a gate of its own (dispatch_two_builds_go), so envoy_go is
    # held to that step's reads and not to the browser half's.
    TagRun("dispatch_two_builds_go", TWO_BUILDS_JOB, TWO_BUILDS_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/dispatch's untagged tests",
           job_file(TWO_BUILDS_JOB)),
    TagRun("dispatch_two_builds_go", TWO_BUILDS_SAME_JOB, TWO_BUILDS_SAME_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/dispatch's untagged tests",
           job_file(TWO_BUILDS_JOB)),
    TagRun("broker_multiprocess", "broker-multiprocess", BROKER_MP_STEP, "envoy_go", "envoy-go",
           f"envoy-go's \"{UNIT_TEST_STEP}\" runs ./cmd/cipherd's untagged tests",
           job_file("broker-multiprocess")),
    # envoy-go's unit legs leave cmd/listener out, since the lane runs it under the detector.
    TagRun("listener_server_change", "listener-server-change", SOAK_STEP, "envoy_go_race", "envoy-go-race",
           f"envoy-go-race's \"{RACE_JOBS['envoy-go-race'].step}\" runs ./cmd/listener's untagged tests",
           job_file("listener-server-change")),
    TagRun("envoy_go", ENVOY_NATSLIVE_JOB, NATS_MATRIX_STEP, "envoy_go", "envoy-go",
           f"envoy-go's own \"{UNIT_TEST_STEP}\" runs ./internal/testnats's untagged tests in a unit leg",
           ENVOY_GO_FILE),
    # The natslive step is the merge-queue half of envoy-infra's old job, and the unit step that
    # runs the three packages' untagged tests went with the half a pull request runs.
    TagRun("envoy_infra_go", "envoy-infra-go", INFRA_NATSLIVE_STEP, "envoy_infra_build", "envoy-infra-build",
           f"envoy-infra-build's \"{INFRA_PLAIN_STEP}\" runs {', '.join(INFRA_NATSLIVE_PACKAGES)} untagged",
           job_file("envoy-infra-go")),
]


# ---------------------------------------------------------------- the gates and the rules
def with_reads(gate_, extra):
    """`gate_` reading `extra` too: how a file of .github/ci-gates adds reads after its gate."""
    return dataclasses.replace(gate_, reads=tuple(gate_.reads) + tuple(extra))


def refgen_api_reads(why):
    """The non-test .go files of every package broker-refgen's readAPI and apiPage can parse
    (REFGEN_API), read when the table is: a route that names one more package's error or type is
    read with no edit anywhere."""
    return [Read(path, why) for directory in REFGEN_API for path in files_but_tests(directory)]


# The sets a //ci:reads-set line of a test file names: each computed from the tree, or shared
# with gates that declare the same read, so it is written once.
READ_SETS = {
    "envoy_client_dep": envoy_client_dep,
    "refgen_api": refgen_api_reads,
}

# Each gate is a file of .github/ci-gates and each RULES row the //ci: lines of the test file it
# was keyed on (ci_filter_sources.py). Read last, so a file of either names any helper above; the
# loader refuses a module that still binds the old tables' names, which a branch made before they
# moved keeps when it merges main.
GATES, EXCLUDED = load_gate_files(globals())
RULES, DIRECTIVES = load_directives(list(repository_files()), READ_SETS)
