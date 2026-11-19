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
# its answer instead. It checks two things.
#
# 1. Every go command a file starts runs with GOPROXY=off, or says why it need not. A site is a
#    place that starts `go` with a subcommand that can load a module (build, run, list, install,
#    get, mod, test, vet, env, generate, work, version: env and version too, since a go.mod's
#    toolchain line can make either fetch a toolchain), or that resolves the go binary a later
#    line starts:
#      Go          exec.Command("go", ...), exec.CommandContext(ctx, "go", ...),
#                  exec.LookPath("go"), filepath.Join(..., "bin", "go")
#      TypeScript  Bun.spawn, Bun.spawnSync, spawn, spawnSync, execFile, execFileSync, execa and
#                  their Async forms with "go" as the command, an argv array that starts "go" and
#                  a subcommand (or "-C", a directory and a subcommand), Bun's $`go ...` and
#                  exec("go ...")
#      Python      an argv list or tuple that starts "go", and subprocess's string form
#      shell       `go <subcommand>` or `go -C <directory> <subcommand>`, the directory quoted
#                  or not, or a path ending /go, as a command: outside a quoted string (a $(...)
#                  inside double quotes is a command) and outside a heredoc's body
#    Comments and the text of string literals are not read for sites, so a fixture or a message
#    that spells a go command starts nothing here. A site passes when it carries one of:
#      the offline rule in its own text: "GOPROXY=off" (Go, Python, shell), GOPROXY: "off"
#        (TypeScript) or Python's "GOPROXY": "off", on its line or within the lines after it that
#        set its environment (4 for Go and Python, 6 for TypeScript); in shell on the command
#        itself, backslash continuations included, or an `export GOPROXY=off` line earlier in
#        the file;
#      a reason: a comment holding `go-offline: <why>` on its line or within the 5 lines above
#        it, saying why the command reads no module, why it is no test, or which job alone runs
#        it, for a reader to judge;
#      a file reason: a comment holding `go-offline-file: <why>` within the file's first 40
#        lines, which this refuses in a test file and in any file a required workflow names
#        (ci.yaml and every local workflow it calls, whose jobs ci-ok judges), so no file a
#        required job runs can wave its sites through at once.
#    Files under a directory named testdata, node_modules, dist, .git or .jj are not read: they
#    are a test's inputs, and the test that runs one is itself a site. Which file is source, in
#    which language, and how each language is read without its comments and string text, is
#    .github/scripts/source_scan.py's, which check-omp-offline.sh reads files with too.
#
# 2. Every job that runs a test built through testbuild fetches, in a step before, what that build
#    needs. packages/daemon's testbuild.EnvoyBinary builds envoy commands while a test runs, with
#    GOPROXY=off, and the daemon's own build never loads those commands, so a job that runs such
#    a test and fetches nothing first fails it on a cold or stale module cache. For every job of
#    every workflow (required or not: e2e.yaml's nightly proofs compile and run the same tests),
#    a step that runs go test of a package whose tests call testbuild.EnvoyBinary - in its run:
#    text, or in a script that step runs, or in a file that script sources or runs - must come
#    after a step that runs `.github/scripts/go-fetch.sh deps` of packages/envoy naming every
#    command testbuild.EnvoyBinary is called with. The package's module is the directory `go -C
#    DIR test` or `go test -C DIR` names, or a `cd DIR` earlier on the line moved to, else the
#    step's. A `go test -c -o /dev/null` compiles and runs nothing, and a `go test -list` runs no
#    test: neither is such a step.
#
# It is a presence check: whether a reason is true, and whether a fetch step fetches into the
# cache the test reads, is the reviewer's to judge, as check-fireevent-reasons.sh says of its own.
# A scan that read no file of a language, found no testbuild caller where testbuild exists, or saw
# no step run those callers' tests, reports UNKNOWN and exits 2: a scan that cannot see must not
# read as a clean one.
#
# Run from anywhere: .github/scripts/check-go-offline.sh
# CI runs it in ci.yaml's lint job.
set -euo pipefail

# The shared reading of source files (source_scan.py) sits beside this script's own file, which a
# test may link into a tree of its own: PYTHONPATH names that directory, and the scan reads the
# tree this script stands in. PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__.
scripts=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
cd "$(dirname "$0")/../.."

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$scripts" python3 <<'PY'
import os
import re
import sys
from pathlib import Path

import yaml

import source_scan as scan

SUBCOMMANDS = r"(?:build|run|list|install|get|mod|test|vet|env|generate|work|version)"
REASON = re.compile(r"\bgo-offline:\s*\S")
FILE_REASON = re.compile(r"\bgo-offline-file:\s*\S")
FILE_REASON_LINES = 40
# The string literals that stay code: the words a go command is spelled with, and a string that is
# a go command itself (`"go build ..."`). Every other string's text is blanked.
GO_WORDS = {"go", "-C", "bin"} | set(SUBCOMMANDS[3:-1].split("|"))
GO_COMMAND_TEXT = re.compile(r"go\s+" + SUBCOMMANDS + r"\b")


def go_literal(value):
    return value in GO_WORDS or bool(GO_COMMAND_TEXT.match(value))


SITE = {
    "go": re.compile(
        r'\bexec\.Command(?:Context)?\(\s*(?:[\w.]+(?:\(\))?\s*,\s*)?"go"\s*[,)]'
        r'|\bexec\.LookPath\(\s*"go"\s*\)'
        r'|\bfilepath\.Join\([^)]*"bin"\s*,\s*"go"\s*\)'),
    "ts": re.compile(
        r"\b(?:Bun\.spawn(?:Sync)?|spawn(?:Sync)?|execFile(?:Sync|Async)?|execa(?:Sync)?)\s*\(\s*\[?\s*([\"'`])go\1"
        r"|\[\s*([\"'`])go\2\s*,\s*(?:([\"'`])-C\3\s*,\s*[^,\]]+,\s*)?([\"'`])" + SUBCOMMANDS + r"\4"
        r"|\$`\s*go\s+" + SUBCOMMANDS + r"\b"
        r"|\bexec(?:Sync)?\(\s*([\"'`])go\s+" + SUBCOMMANDS + r"\b"),
    "py": re.compile(
        r"[\[(]\s*([\"'])go\1\s*[,\])]"
        r"|\bsubprocess\.\w+\(\s*([\"'])go\s+" + SUBCOMMANDS + r"\b"),
    # A quoted -C directory is blanked to spaces by shell_code, so the directory's word may be absent.
    "sh": re.compile(r"(?<![\w./$-])(?:go|[\w./${}-]*/go)(?=\s+(?:-C(?:=\S*\s+|\s+(?:\S+\s+)?))?"
                     + SUBCOMMANDS + r"\b)"),
}
OFFLINE = {
    "go": (re.compile(r'"GOPROXY=off"'), 4),
    "ts": (re.compile(r"GOPROXY\s*:\s*[\"'`]off[\"'`]|GOPROXY=off"), 6),
    "py": (re.compile(r"[\"']GOPROXY[\"']\s*:\s*[\"']off[\"']|GOPROXY=off"), 4),
}


def code_lines(text, lang):
    if lang == "sh":
        return scan.shell_code(text)
    return scan.code(text, lang, go_literal)


def sites_of(raw, code, lang):
    """(index, offline) for each site of a file."""
    found, exported = [], False
    for index, line in enumerate(code):
        if lang == "sh" and re.match(r"^\s*export\s+GOPROXY=off\b", line):
            exported = True
        if not SITE[lang].search(line):
            continue
        if lang == "sh":
            found.append((index, exported or "GOPROXY=off" in scan.continued(raw, index)))
            continue
        offline, after = OFFLINE[lang]
        # The site's line and the next `after` lines of code after it, comments passed over.
        window = [raw[k] for k in range(index, len(raw)) if code[k].strip()][:after + 1]
        found.append((index, any(offline.search(l) for l in window)))
    return found


def reasoned(raw, index, lang):
    return scan.reasoned(raw, lang, REASON, [index])


def required_workflows():
    """ci.yaml and every local workflow it calls, followed through each called file, as their
    text with comment lines taken out."""
    names, todo = set(), [".github/workflows/ci.yaml"]
    while todo:
        path = todo.pop()
        if path in names or not os.path.isfile(path):
            continue
        names.add(path)
        todo += re.findall(r"uses:\s*\./(\.github/workflows/[\w.-]+\.ya?ml)", Path(path).read_text())
    return {p: "\n".join(l for l in Path(p).read_text().splitlines() if not l.lstrip().startswith("#")) for p in names}


def named_by(path, workflows):
    word = re.compile(r"(?<![\w.-])" + re.escape(os.path.basename(path)) + r"(?![\w-])")
    return sorted(w for w, text in workflows.items() if path in text or word.search(text))


errors = []
counts = dict.fromkeys(("go", "ts", "py", "sh"), 0)
read = dict.fromkeys(("go", "ts", "py", "sh"), 0)
offline_count = reason_count = file_reason_count = 0
workflows = required_workflows()
if not workflows:
    scan.unknown("no .github/workflows/ci.yaml here, so no workflow is known to be required")
for path, lang in scan.sources():
    read[lang] += 1
    text = scan.read(path)
    if "go" not in text:
        continue
    raw = text.split("\n")
    found = sites_of(raw, code_lines(text, lang), lang)
    if not found:
        continue
    file_reason = any(scan.COMMENT[lang].match(l) and FILE_REASON.search(l) for l in raw[:FILE_REASON_LINES])
    if file_reason:
        refusal = ""
        if scan.is_test_file(path):
            refusal = "it is a test file"
        elif named_by(path, workflows):
            refusal = "a required workflow names it (" + ", ".join(named_by(path, workflows)) + ")"
        if refusal:
            errors.append((path, 1, f"a go-offline-file reason, which this file may not carry: {refusal}; "
                                    "give each of its go commands its own"))
            file_reason = False
    for index, offline in found:
        counts[lang] += 1
        if offline:
            offline_count += 1
        elif reasoned(raw, index, lang):
            reason_count += 1
        elif file_reason:
            file_reason_count += 1
        else:
            errors.append((path, index + 1, "a go command with no GOPROXY=off and no go-offline reason: "
                           + raw[index].strip()))

blind = [lang for lang, n in read.items() if n == 0]
if blind:
    scan.unknown(f"read no {', '.join(blind)} file under {os.getcwd()}, so their go commands are unseen")

# --- 2. The jobs that run a test built through testbuild fetch its commands first.
TESTBUILD = "packages/daemon/internal/testbuild/testbuild.go"
ENVOY_BINARY = re.compile(r'\btestbuild\.EnvoyBinary\(\s*\w+\s*,\s*"([^"]+)"\s*\)')
SEPARATORS = scan.SHELL_SEPARATORS
COMMAND_PREFIXES = {"if", "then", "do", "else", "elif", "while", "until", "!", "{", "time", "exec", "command", "env"}
built, users = set(), set()
for directory, dirs, files in os.walk("packages/daemon"):
    dirs[:] = [d for d in dirs if d not in scan.SKIP_DIRS]
    for name in files:
        if name.endswith("_test.go"):
            calls = ENVOY_BINARY.findall(scan.read(os.path.join(directory, name)))
            if calls:
                built.update(calls)
                users.add(os.path.normpath(directory))


def repo_path(raw_path, base="."):
    raw_path = raw_path.strip("\"'")
    for prefix in ("$root/", "${root}/", "$repo_root/", "${repo_root}/", "$GITHUB_WORKSPACE/", "${GITHUB_WORKSPACE}/"):
        if raw_path.startswith(prefix):
            return os.path.normpath(raw_path[len(prefix):])
    return os.path.normpath(os.path.join(base, raw_path))


def go_tests(raw_line, workdir):
    """The (module directory, package arguments, listed) of each go test of a line that runs
    tests: a -c build that is not to /dev/null runs them later, a -list runs none. The module is
    the directory a `-C DIR` names, before `test` or right after it, else the one a `cd DIR` at a
    command position earlier on the line moved to (until the end of the ( ) group it is in), else
    the step's."""
    tokens, found = scan.shell_words(raw_line), []
    here, groups, command = workdir, [], True
    for k, token in enumerate(tokens):
        if token in ("(", "$("):
            groups.append(here)
        elif token == ")" and groups:
            here = groups.pop()
        if token in SEPARATORS:
            command = True
            continue
        if command and token == "cd" and k + 1 < len(tokens) and tokens[k + 1] not in SEPARATORS:
            here = repo_path(tokens[k + 1], here)
        command = False
        if token != "go" and not token.endswith("/go"):
            continue
        rest, module = tokens[k + 1:], here
        if rest[:1] == ["-C"] and len(rest) > 1:
            module, rest = repo_path(rest[1], here), rest[2:]
        if rest[:1] != ["test"]:
            continue
        args = []
        for t in rest[1:]:
            if t in SEPARATORS:
                break
            args.append(t)
        if args[:1] == ["-C"] and len(args) > 1:
            module, args = repo_path(args[1], here), args[2:]
        if "-list" in args or ("-c" in args and any(a == "/dev/null" for a in args)):
            continue
        packages = [a for a in args if a.startswith("./") or a == "."]
        found.append((module, packages, not packages or "xargs" in tokens[:k]))
    return found


def scripts_of(raw_line, workdir):
    """The files a line sources or runs, each as the command of the line or of a part of it
    after ;, &&, ||, |, ( or $(: `. file`, `source file`, `bash file`, `sh file`, or a path
    ending .sh. A word that is only an argument names a file the line reads, not one it runs.
    A $VAR in a path matches any name there."""
    tokens, named, command = scan.shell_words(raw_line), [], True
    for k, token in enumerate(tokens):
        if token in SEPARATORS:
            command = True
            continue
        if command and (re.match(r"^\w+=", token) or token in COMMAND_PREFIXES):
            continue
        if command and token in (".", "source", "bash", "sh") and k + 1 < len(tokens):
            named.append(tokens[k + 1])
        elif command and token.endswith(".sh"):
            named.append(token)
        command = False
    paths = []
    for name in named:
        for base in (workdir, "."):
            candidate = repo_path(name, base)
            if "$" in candidate:
                pattern = re.sub(r"\$\{?\w+\}?", "*", candidate)
                paths += sorted(str(p) for p in Path(".").glob(pattern) if p.is_file() and p.suffix == ".sh")
            elif os.path.isfile(candidate):
                paths.append(candidate)
    return paths


def runs_testbuild_tests(text, workdir, seen):
    code = scan.shell_code(text)
    for raw_line, code_line in zip(text.split("\n"), code):
        if not code_line.strip():
            continue
        if re.search(r"(?<![\w./-])go\b", code_line):
            for module, packages, listed in go_tests(raw_line, workdir):
                for user in users:
                    if listed and (user == module or user.startswith(module.rstrip("/") + "/")) and module != ".":
                        return True
                    for arg in packages:
                        pkg = os.path.normpath(os.path.join(module, arg))
                        if pkg == user or (arg.endswith("/...") and user.startswith(pkg[:-3].rstrip("/") + "/")) \
                                or (arg == "./..." and user.startswith(module.rstrip("/") + "/")):
                            return True
        for script in scripts_of(raw_line, workdir):
            if script in seen:
                continue
            seen.add(script)
            if runs_testbuild_tests(scan.read(script), ".", seen):
                return True
    return False


def fetches(text, workdir):
    for raw_line, code_line in zip(text.split("\n"), scan.shell_code(text)):
        if "go-fetch.sh" not in code_line:
            continue
        tokens = scan.shell_words(raw_line)
        for k, token in enumerate(tokens):
            if token.endswith("go-fetch.sh") and tokens[k + 1:k + 2] == ["deps"] and len(tokens) > k + 2:
                packages = set()
                for t in tokens[k + 3:]:
                    if t in SEPARATORS:
                        break
                    packages.add(t)
                if repo_path(tokens[k + 2], workdir) == "packages/envoy" and built <= packages:
                    return True
    return False


jobs_checked = 0
if os.path.isfile(TESTBUILD):
    if not users:
        scan.unknown(f"{TESTBUILD} exists and no daemon test calls testbuild.EnvoyBinary with a command, so the "
                     "jobs that run one are unseen")
    for workflow in sorted(Path(".github/workflows").glob("*.y*ml")):
        document = yaml.safe_load(workflow.read_text()) or {}
        default_dir = ((document.get("defaults") or {}).get("run") or {}).get("working-directory", ".")
        for job_id, job in (document.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            job_dir = ((job.get("defaults") or {}).get("run") or {}).get("working-directory", default_dir)
            fetched = False
            for step in job.get("steps") or []:
                run = step.get("run")
                if not isinstance(run, str):
                    continue
                workdir = os.path.normpath(step.get("working-directory", job_dir))
                if runs_testbuild_tests(run, workdir, set()):
                    jobs_checked += 1
                    if not fetched:
                        name = step.get("name") or run.strip().splitlines()[0]
                        errors.append((str(workflow), 1,
                                       f"job {job_id}, step \"{name}\", runs the tests of {', '.join(sorted(users))}, "
                                       f"whose testbuild.EnvoyBinary builds {' '.join(sorted(built))} with GOPROXY=off, "
                                       f"and no step before it fetches them: add `.github/scripts/go-fetch.sh deps "
                                       f"packages/envoy {' '.join(sorted(built))}` before it"))
                if fetches(run, workdir):
                    fetched = True
    # A tree where tests call testbuild and no step of any workflow runs them is one whose steps
    # this reading cannot follow (a go test, cd or script it does not see), not a clean one.
    if jobs_checked == 0:
        scan.unknown(f"{', '.join(sorted(users))} call testbuild.EnvoyBinary and no step of any workflow was seen "
                     "to run their tests, so the jobs that run one are unseen")

total = sum(counts.values())
summary = (f"{total} go commands of {sum(read.values())} files read (Go {counts['go']}, TypeScript {counts['ts']}, "
           f"Python {counts['py']}, shell {counts['sh']})")
if errors:
    scan.refuse(errors, summary, "A test fetches no module while it runs. Run the go command with GOPROXY=off and "
                "fetch what it needs in a step before the tests (.github/scripts/go-fetch.sh deps DIR PACKAGE...), "
                "or, where it reads no module or runs in no test of a required job, say so beside it in a "
                "`go-offline: <why>` comment.")
print(f"every one of the {summary} runs with GOPROXY=off ({offline_count}), carries a reason ({reason_count}) or "
      f"is in a file whose reason covers it ({file_reason_count}); each of the {jobs_checked} steps that run "
      f"testbuild's tests comes after its fetch")
PY
