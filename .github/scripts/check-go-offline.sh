#!/usr/bin/env bash
# No test fetches a Go module while it runs, in any language its job runs it in.
#
# A test that reaches the module proxy or the checksum database fails whenever that service
# fails, on a change that touched nothing it tests: in run 44317642380, envoy-infra-go failed on a
# `go install` of helm inside a test that could not read sum.golang.org. The rule that ends that
# is the same everywhere: a go command a test starts reads the module cache alone (GOPROXY=off),
# and the job fetches what it needs in a step before the tests, tried again on failure
# (.github/scripts/go-fetch.sh). A list of the tests that follow it would go stale the day a new
# one is written, in Go, TypeScript, Python or shell, so this program asks every such place for
# its answer instead. It checks six things.
#
# 1. Every go command a file starts runs with GOPROXY=off, or says why it need not. Each file is
#    read whole, a statement at a time, as its language parses it (a call's arguments across
#    lines, Python through its ast module, shell as simple commands with continued lines joined),
#    so an argv Biome writes one element a line is one argv. The go binary is "go", a path ending
#    /go ("/usr/local/go/bin/go", "$GOROOT/bin/go", a + of strings that ends /go), a last "go"
#    of filepath.Join, path.join, path.resolve or os.path.join, exec.LookPath("go"),
#    Bun.which("go"), shutil.which("go"), or a name bound to one of those earlier in the same
#    function or in the package (Go), file (TypeScript), function or module (Python) or script
#    (shell: a variable set to it, by local or declare too, or to $(command -v go); ${NAME:-go},
#    whose default is it; $(command -v go) itself; an array that starts it with a subcommand, run
#    as "${NAME[@]}"). An argv's program is the word after the launchers a shell reads past: env
#    with its settings and options, command, exec, nice, nohup, time, timeout and flock with their
#    operands (`timeout 600 go test`). A site is where it starts:
#      Go          exec.Command and exec.CommandContext, os/exec under whatever name the file
#                  imports it as, of the go binary, with any arguments; of a shell's -c (`bash -c
#                  "go build"`, `bash -ec ...`), whose script is read as shell; and an
#                  exec.LookPath("go") or filepath.Join(..., "bin", "go") that no exec.Command of
#                  the same function starts
#      TypeScript  Bun.spawn, Bun.spawnSync (an argv, or `cmd:`), spawn, spawnSync, execFile,
#                  execFileSync, execFileAsync, execa and execaSync of the go binary or of a
#                  shell's -c, and one whose program is a command line (spawnSync("go build", {
#                  shell: true })), read as shell; exec, execSync and execAsync of a shell string;
#                  Bun's $`...` (a substitution that names the go binary reads as go, any other as
#                  a word), read as shell; each under any name child_process, node:child_process,
#                  execa or bun is imported as (import { spawnSync as run }, import * as cp,
#                  const { exec: sh } = require(...), import { $ as sh }); and any argv array that
#                  starts the go binary and a subcommand (or -C, a directory and a subcommand),
#                  one a helper of the test's own is given too
#      Python      an argv list or tuple, or a call's own arguments, that start the go binary or a
#                  shell's -c; a string os.system, os.popen or any subprocess function is given,
#                  read as shell (an f-string's {...} as a word), and one shlex.split is given,
#                  read as shell in the environment of the call it goes to; and a
#                  shutil.which("go") no argv of the same function starts
#      shell       a word that is the go binary followed by a subcommand that can load a module
#                  (build, run, list, install, get, mod, test, vet, env, generate, work, version:
#                  env and version too, since a go.mod's toolchain line can make either fetch a
#                  toolchain), after an optional -C DIR, anywhere in a simple command (after
#                  `env`, `xargs` or a function of its own), in a $(...) or `...`, in the script
#                  of a shell's -c (one inside another's too), in what eval runs, in a heredoc a
#                  shell with no script operand reads, and in what such a shell reads from a `cat`
#                  of a heredoc, an `echo` or a `printf` before it in a pipeline; one word that
#                  quotes a whole command (echo "go build"), a comment, and any other heredoc's
#                  body start nothing. A shell file is one named .sh or .bash, or one whose first
#                  line runs a shell (#!/usr/bin/env bash), whatever its name
#    Comments and the text of string literals are not read for sites, so a fixture or a message
#    that spells a go command starts nothing here. A site passes when it carries one of:
#      the offline rule in its own environment, where the last setting of GOPROXY, after the
#        environment it inherits, is off: in Go the Env of the same *exec.Cmd variable (the last
#        `X.Env = ...` after the call; append(os.Environ(), "GOPROXY=off") passes,
#        append([]string{"GOPROXY=off"}, os.Environ()...) does not); in TypeScript the call's own
#        options' env object, GOPROXY: "off" after every spread, with what a later statement
#        sets in that object (env.GOPROXY = ..., env["GOPROXY"] = ..., delete env.GOPROXY,
#        Object.assign(env, ...)), and no spread of other options after it but one of objects
#        that hold no env (Bun's $ reads a .env({...}) chained to it); in Python the call's env=
#        (a dict display after every ** spread, dict() or a name bound to one); in shell the
#        command's own NAME=value prefix or the settings of an `env` before the go, or an
#        `export GOPROXY=off` before it at the top level of the script or in the function or
#        subshell that holds it (one in another function, in a pipeline or in the background
#        counts for nothing), with no later `unset`, `export -n`, `env -u GOPROXY` or other
#        GOPROXY. A command of a pipeline has its own environment: in `GOPROXY=off go list |
#        xargs go build` the build is online. A command inside a shell's -c, an eval or a
#        heredoc a shell reads starts with the environment of the command that runs it. A
#        "GOPROXY=off" in a comment, a neighbour's environment or a string no command is given
#        counts for nothing;
#      a reason: a comment holding `go-offline: <why>` on its line or within the 5 lines above
#        it, saying why the command reads no module, why it is no test, or which job alone runs
#        it, for a reader to judge; or, for a site whose file a comment may not change (a file a
#        cache key hashes), an entry of .github/go-offline-reasons.yaml naming that file, the
#        site's line as the file has it, and the reason, counted as a line reason. An entry that
#        names no site needing it (its command changed, or its file is gone) is refused, so the
#        list cannot go stale silently;
#      a file reason: a comment holding `go-offline-file: <why>` within the file's first 40
#        lines, which this refuses in a test file, in any file a required workflow names
#        (ci.yaml and every local workflow it calls, whose jobs ci-ok judges), and in any file
#        rule 2's walk reaches from a step of a required workflow by a path it reads (a script
#        the step runs or sources, one that script runs in turn, a program it hands to python3,
#        node or bun or runs by its path, a TypeScript file under a test run's directory), so no
#        file a required job runs can wave its sites through at once. A script the walk only
#        guesses from a variable's name, or one a TypeScript file names in a string (which a test
#        may read as text), is not held to this.
#    Its limits, each a form this reads as no site: a go binary a function is handed as a
#    parameter or returns, or one held in a struct field or a map (this follows a name to its
#    assignments, not a value through a call or a structure); an argv built by spreading another
#    array or joined at run time ([...base, "build"], "go " + verb), but for a string a shell
#    runs, and a string a shell runs that a call builds (fmt.Sprintf("go build %s", dir)) (this
#    reads literal text, and a + of it, alone); an *exec.Cmd made as a struct literal; a call
#    inside a template literal's ${...}; an env object a function it is handed to changes. And one
#    form it reads as offline though it may not be: a setting made on one branch of an if or a
#    case (a Go Env set in an if, an export in an if the test does not take), read as made, since
#    this follows no branch. A shell function an `export GOPROXY=off` in another function feeds
#    reads as online, and is refused. A Python file its ast module cannot parse is refused, since
#    none of its go commands is seen.
#    Files under a directory named testdata, node_modules, dist, .git or .jj are not read: they
#    are a test's inputs, and the test that runs one is itself a site. Which file is source, in
#    which language, and how each language is read, statement by statement, is
#    .github/scripts/source_scan.py's and the source_*.py modules' beside it, which
#    check-omp-offline.sh reads files with too; this rule's own reading of each language's sites is
#    go_offline_sites.py's.
#
# 2. Every job fetches, in a step before, what each offline build its steps reach needs. For every
#    job of every workflow (required or not: e2e.yaml's nightly proofs and the nightly re-record
#    build offline too), each step's run text is walked as bash runs it: the directory each `cd`
#    moves to ($(dirname "$0"), $(cd X && pwd) and a variable set to either are read), each script
#    a command runs or sources, each function it calls with that call's arguments, the environment
#    an export or a prefix gives it, and each shell script a TypeScript file under a Playwright
#    run's directory (or under the paths a bun test run names) names in a string. What it reaches:
#      a go test of a package whose tests call packages/daemon's testbuild.EnvoyBinary, which
#        builds envoy commands with GOPROXY=off while the test runs: a step before must run
#        `.github/scripts/go-fetch.sh deps packages/envoy` naming every command it is called with.
#        A `go test -c -o /dev/null` compiles and runs nothing, and a `go test -list` runs no test;
#      an offline go build, install, run, list or generate (GOPROXY=off by the rules of 1), in a
#        module of the repository: a step before must fetch that module's named packages (a
#        go-fetch.sh deps of that module, its -test and -tags T passed over), or a tool pin with
#        go-fetch.sh tools; and a goCommand of a TypeScript file such a run reaches, its
#        `packages:` fetched in any module. A `go-fetch.sh file FILE` fetches what each deps and
#        tools line of FILE would, its paths read from the repository's root.
#    A step that needs none of it says why in a `# go-offline: <why>` line of its run text or of
#    the 5 lines above its name (one whose job has no Go and runs binaries another job built).
#    Its limits: a build whose directory this walk cannot place (a scratch copy) or whose packages
#    are not literal, a goCommand whose packages are not, and a Go test's own offline build other
#    than testbuild's are no need it holds a step to; a bun run of a package script is followed
#    no further.
#
# 3. In every job of a required workflow, each step that runs go-fetch.sh (deps, tools or file)
#    comes after a step of the same job that restores the module cache: actions/cache/restore or
#    actions/cache with ~/go/pkg/mod in its path. A fetch with no restore before it downloads every
#    module the job's cache would have held, on every run. The check lists each such fetch step
#    with the restore before it.
#
# 4. Every Go job of a required workflow runs with GOPROXY=off and GOFLAGS=-mod=readonly in its env,
#    and a step that sends either online says why (.github/scripts/go_offline_jobs.py, whose header
#    says what it reads and what it does not see). A refusal names the job's line, or the step's.
#
# 5. No step of a required workflow, or of a local action one uses, installs an Ubuntu package off
#    the apt cache the job restores: a bare apt-get, apt or dpkg -i, a Playwright --with-deps, and
#    an apt-cache.sh install with no restore or no bound before it are refused at their line
#    (.github/scripts/package_installs.py, whose header says how it reads a step).
#
# 6. Each go command a required job's image build runs reads the job's module cache alone: a RUN of
#    a stage the build builds runs it with GOPROXY=off, GOFLAGS=-mod=readonly and GOTOOLCHAIN=local
#    and mounts a named context the build hands (the job's module cache) at the module cache, or
#    says why it need not (.github/scripts/go_offline_images.py, whose header says which builds and
#    stages it reads). A refusal names the Dockerfile, its line, and the job and step that build it.
#
# 7. Each offline go command a step of a required Go job runs in its own run text, or in a shell
#    that text hands more shell to (bash -c, a pipe, a heredoc), loads only packages a fetch
#    before it in the job names: a deps line of a `go-fetch.sh file` (or a `go-fetch.sh deps`)
#    whose patterns hold its packages, with -test where it is go test or go vet, the same tags
#    (its -tags, else its GOFLAGS') and the same GOOS, GOARCH, CGO_ENABLED and GOWORK, from its
#    environment as rule 1 reads it. A command whose packages, tags or settings are not literal
#    is refused as well. Either says why in a
#    `# go-offline: <why>` line above it, or above its step's name. The job's fill runs the same fetch file
#    (check-ci-filters.sh's rule 22), so a package no line covers is read from whatever an older
#    entry holds, and fails on the run that restores the entry its own fill saved
#    (.github/scripts/go_offline_fetched.py, whose header says what it holds and what it does not
#    read).
#
# Rules 4, 5, 6 and 7 are modules of their own, each runnable alone; this script calls each one's
# check(), adds its problems to its own and its summary to the line a clean run ends with, and an
# UNKNOWN of any of them is this check's UNKNOWN.
#
# What catches a go command this check misses (a form its limits name): every job of a required
# workflow runs with GOPROXY=off and GOFLAGS=-mod=readonly, so such a command in a required job's
# test fails at once, naming the module its cache lacks, instead of downloading it. This check is
# the first line, which names the place before CI runs it; that setting is the second.
#
# It is a presence check: whether a reason is true, and whether a fetch step fetches into the
# cache the test reads, is the reviewer's to judge, as check-fireevent-reasons.sh says of its own.
# A scan that read no file of a language, found no testbuild caller where testbuild exists, or saw
# no step run those callers' tests, reports UNKNOWN and exits 2: a scan that cannot see must not
# read as a clean one. A file whose text holds nothing a site of its language is spelled with (an
# exec.Command, a spawn, a go a string ends in, a go followed by a subcommand: MAYBE below) is not
# read a statement at a time, which keeps the scan to the files that can start go.
#
# Run from anywhere: .github/scripts/check-go-offline.sh
# CI runs it in ci.yaml's lint job.
set -euo pipefail

# The rules' code (go_offline_sites.py, go_offline_walk.py, go_offline_restore.py, go_offline_jobs.py,
# package_installs.py, go_offline_images.py and go_offline_fetched.py, with go_offline_argv.py, the
# reading of a go command's and a fetch's words that rules 2 and 7 share) and the shared reading of
# source files (source_scan.py and the source_*.py modules) sit beside this script's own file, which a test may link into a tree of
# its own: PYTHONPATH names that directory, and the scan reads the tree this script stands in.
# PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__. Each rule adds its problems to
# errors and a clause to the line a clean run ends with.
scripts=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$scripts" python3 <<'PY'
import os
import re

import go_offline_fetched as fetched
import go_offline_images as images
import go_offline_jobs as jobs
import go_offline_restore as restore
import go_offline_sites as sites
import go_offline_walk as walk
import package_installs as installs
import source_scan as scan

# A file reason: a comment holding `go-offline-file: <why>` within the file's first 40 lines.
FILE_REASON = re.compile(r"\bgo-offline-file:\s*\S")
FILE_REASON_LINES = 40

# --- 1. Every go command of every file, read in its language: offline, reasoned or refused.
walk.read_testbuild()
errors = []
counts = dict.fromkeys(("go", "ts", "py", "sh"), 0)
read = dict.fromkeys(("go", "ts", "py", "sh"), 0)
offline_count = reason_count = file_reason_count = 0
# Each file whose file reason no test name and no required workflow's text refuses: (path, language,
# lines, the go commands it would cover). Rule 2's walk decides it: refused in a file a step of a
# required workflow reaches.
file_reasons = []
# The reasons .github/go-offline-reasons.yaml holds for sites whose file a comment may not change.
outside = sites.OutsideReasons(errors)
workflows = walk.required_workflows()
if not workflows:
    scan.unknown("no .github/workflows/ci.yaml here, so no workflow is known to be required")
for path, lang in scan.sources(shebang=True):
    read[lang] += 1
    text = scan.read(path)
    walk.index(path, lang, text)
    if not sites.MAYBE[lang].search(text):
        continue
    raw = text.split("\n")
    found = sites.sites_of(path, text, lang)
    if found is None:
        errors.append((path, 1, "a Python file this check cannot parse, so its go commands are unseen; fix its syntax"))
        continue
    if not found:
        continue
    file_reason = any(scan.COMMENT[lang].match(l) and FILE_REASON.search(l) for l in raw[:FILE_REASON_LINES])
    if file_reason:
        refusal = ""
        if scan.is_test_file(path):
            refusal = "it is a test file"
        elif walk.named_by(path, workflows):
            refusal = "a required workflow names it (" + ", ".join(walk.named_by(path, workflows)) + ")"
        if refusal:
            errors.append((path, 1, f"a go-offline-file reason, which this file may not carry: {refusal}; "
                                    "give each of its go commands its own"))
            file_reason = False
    covered = []
    for index, is_offline in found:
        counts[lang] += 1
        if is_offline:
            offline_count += 1
        elif sites.reasoned(raw, index, lang) or outside.covers(path, raw[index]):
            reason_count += 1
        elif file_reason:
            covered.append(index)
        else:
            errors.append((path, index + 1, "a go command with no GOPROXY=off and no go-offline reason: "
                           + raw[index].strip()))
    if file_reason:
        file_reasons.append((path, lang, raw, covered))

blind = [lang for lang, n in read.items() if n == 0]
if blind:
    scan.unknown(f"read no {', '.join(blind)} file under {os.getcwd()}, so their go commands are unseen")

# --- 2. Each job's steps, walked in order: each offline build a step reaches is fetched before it.
jobs_checked, reach_checked, reach_reasoned, reached = walk.check_jobs(errors, workflows)
walked = (f"each of the {jobs_checked} steps that run testbuild's tests comes after its fetch, and each of the "
          f"{reach_checked} steps that reach an offline build comes after a fetch of what it builds "
          f"({reach_reasoned} say why they need none)")

# A file reason in a file a step of a required workflow reaches (a script it runs or sources, one
# that script runs, a program it runs, a test run's TypeScript) is refused, with the go commands it
# would cover: a required job runs them.
for path, lang, raw, covered in file_reasons:
    where = reached.by(path, lang)
    if where is None:
        file_reason_count += len(covered)
        continue
    errors.append((path, 1, f"a go-offline-file reason, which this file may not carry: a step of a required "
                            f"workflow reaches it ({where}); give each of its go commands its own"))
    errors += [(path, index + 1, "a go command with no GOPROXY=off and no go-offline reason: " + raw[index].strip())
               for index in covered]
outside.stale(errors)
summary = (f"{sum(counts.values())} go commands of {sum(read.values())} files read (Go {counts['go']}, "
           f"TypeScript {counts['ts']}, Python {counts['py']}, shell {counts['sh']})")
clauses = [f"every one of the {summary} runs with GOPROXY=off ({offline_count}), carries a reason ({reason_count}) "
           f"or is in a file whose reason covers it ({file_reason_count})", walked]

# --- 3. In every job of a required workflow, each step that runs go-fetch.sh comes after a step of
# the same job that restores ~/go/pkg/mod.
pairs = restore.check(workflows, errors)
clauses.append(f"each of the {len(pairs)} fetch steps of a required job comes after a restore of ~/go/pkg/mod")

# --- 7. Each go command a step of a required Go job runs loads only packages a fetch before it names.
held, said = fetched.check(workflows, errors)
clauses.append(f"each of the {held} offline go commands the steps of required Go jobs run loads only packages a "
               f"fetch before it in its job names ({said} say which fetch covers them, or why they need none)")

# --- 4, 5 and 6. Every Go job of a required workflow reads the module cache alone, no required job
# installs a package off its apt cache, and each go command of a required job's image build reads
# the job's module cache alone. Each rule's summary is a clause; an UNKNOWN of one is the check's.
for rule in (jobs, installs, images):
    found, said = rule.check(".")
    if said.startswith("UNKNOWN: "):
        scan.unknown(said[len("UNKNOWN: "):])
    errors += found
    clauses.append(said)

print("Each fetch step of a required job, after the restore of the module cache before it:")
print("\n".join(pairs) if pairs else "  (none)")
if errors:
    scan.refuse(errors, summary, "A test fetches no module while it runs. Run the go command with GOPROXY=off and "
                "fetch what it needs in a step before the tests (.github/scripts/go-fetch.sh deps DIR PACKAGE...), "
                "after the job restores its module cache, or, where it reads no module or runs in no test of a "
                "required job, say so beside it in a `go-offline: <why>` comment.")
print("; ".join(clauses))
PY
