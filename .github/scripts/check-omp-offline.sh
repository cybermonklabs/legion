#!/usr/bin/env bash
# Every test that starts the real Oh My Pi runs it under its language's offline helper, or says
# why it need not.
#
# Under a fresh HOME the pinned Oh My Pi fetches model catalogs over HTTPS, on `omp models` and at
# every session start (catalog.stencil.so, and the public model lists of the providers that need
# no key): one `omp models list` made 52 connections off the machine and 7 DNS lookups. No setting
# of the pin stops all of it. A test that runs it so reaches the network while it runs, and those
# hosts' faults become its own. The rule that ends that is one helper per language, which starts a
# listener of the test's own on loopback that refuses every request and names it as HTTPS_PROXY,
# the proxy Bun's fetch reads: testbin.OMPOffline in Go (packages/daemon/internal/testbin, which
# testbin.OMPEnviron includes), and ompOffline in TypeScript
# (packages/pi-shared/test/omp-offline.ts, which the harness's ompEnv includes). A list of the
# tests that follow it would go stale the day a new one is written, so this program asks every
# place a test starts Oh My Pi for its answer instead.
#
# It reads test code: Go `_test.go` files and the files of a package directory named testbin, and
# TypeScript and JavaScript `.test.` and `.spec.` files and the files under a directory named test
# or tests. Files under testdata, node_modules, dist, .git or .jj are not read. A start is:
#   Go          a call of testbin.OMP, the door the pinned binary comes in by; a read of
#               LEGION_TEST_OMP through os.Getenv or os.LookupEnv, by its name or by a constant of
#               the package that names it; and an exec.Command or exec.CommandContext whose program
#               is "omp" or named omp (omp, r.omp, ompPath). Each passes when the top-level function
#               that holds it, its closures included, calls testbin.OMPOffline or
#               testbin.OMPEnviron (inside testbin, OMPOffline or OMPEnviron).
#   TypeScript  a Bun.spawn, Bun.spawnSync, spawn, spawnSync, execFile, execFileSync, execa or
#               execaSync whose command (its first argument, or the first element of its argv or
#               of its `cmd`) is "omp", the identifier omp or binary, the names the harness and
#               every test give the real binary, or reads LEGION_TEST_OMP; and Bun's $`${omp} ...`.
#               Each passes when its call, from its first line through the 6 lines of code after
#               its command, calls ompOffline or ompEnv, and it is no Sync form: the helper's
#               listener answers only while the test's event loop runs, which a Sync call holds.
# A start of either language also passes with a reason: a comment holding `omp-offline: <why>` on
# its line or within the 5 lines above it, saying why the run reaches no network or why the
# program is not Oh My Pi, for a reader to judge. Comments and the text of string literals are not
# read as code, so a fixture or a message that spells a start starts nothing here. Which file is
# source, in which language, and how each is read without its comments and string text, is
# .github/scripts/source_scan.py's, which check-go-offline.sh reads files with too.
#
# The helpers are held to the rule too: testbin's OMPOffline and the TypeScript ompOffline each
# name HTTPS_PROXY, and OMPEnviron and ompEnv, where they exist, each call theirs. A helper that
# stopped naming the proxy fails here instead of passing every start that calls it.
#
# It is a presence check: whether every command of a function that calls the helper runs under its
# entry, and whether a reason is true, is the reviewer's to judge, as check-go-offline.sh says of
# its own. The proxy catches what Oh My Pi asks for over HTTPS through Bun's fetch, which is every
# request off the machine the pin was seen to make; a plain-HTTP request to another machine, or one
# made outside Bun's fetch, would pass it, and only a trace of the run's connects shows one. A scan
# that read no Go or no TypeScript test file, or found no start where testbin or the harness
# exists, reports UNKNOWN and exits 2: a scan that cannot see must not read as a clean one.
#
# Run from anywhere: .github/scripts/check-omp-offline.sh
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

import source_scan as scan

DOOR_NAME = "LEGION_TEST_OMP"
# String literals that stay code: the variable a door reads, and the binary's own name.
KEPT_LITERALS = {DOOR_NAME, "omp"}
REASON = re.compile(r"\bomp-offline:\s*\S")
TS_AFTER = 6

GO_DOOR = re.compile(r"\btestbin\.OMP\(")
GO_GETENV = re.compile(r"\bos\.(?:Getenv|LookupEnv)\(\s*(\"" + DOOR_NAME + r"\"|\w+)\s*\)")
GO_CONST = re.compile(r"\b(\w+)\s*(?:string\s*)?=\s*\"" + DOOR_NAME + r"\"")
GO_EXEC = re.compile(r"\bexec\.Command(?:Context)?\(\s*(?:[\w.]+(?:\(\))?\s*,\s*)?"
                     r"((?:[A-Za-z_]\w*\.)*[oO][mM][pP]\w*|\"omp\")\s*[,)]")
GO_HELPER = re.compile(r"\btestbin\.OMP(?:Offline|Environ)\(")
GO_HELPER_IN_TESTBIN = re.compile(r"(?<![\w.])OMP(?:Offline|Environ)\(|\btestbin\.OMP(?:Offline|Environ)\(")
TS_SPAWN = re.compile(r"(?<![\w$])(?:Bun\.)?(spawn|spawnSync|execFile|execFileSync|execa|execaSync)\s*\(\s*"
                      r"(?:\{\s*cmd\s*:\s*)?\[?\s*([^,\])]*)")
TS_SHELL = re.compile(r"\$`\s*\$\{\s*([^}]*)\}")
TS_COMMAND = re.compile(r"(?:omp|binary)!?|[\"'`]omp[\"'`]")
# A call of the helper by its own name, a spread `...ompOffline(cleanup)` included, and no method of
# another object that shares the name.
TS_HELPER = re.compile(r"(?<![\w$])(?<![\w$)\]]\.)(?:ompOffline|ompEnv)\s*\(")


def test_code(path, lang):
    """A test file by its name, or a file of a Go testbin package or of a TypeScript test or tests
    directory."""
    directories = path.split("/")[:-1]
    if lang == "go":
        return scan.is_test_file(path) or "testbin" in directories
    return scan.is_test_file(path) or any(d in ("test", "tests") for d in directories)


def code_of(text, lang):
    """The text's lines with its comments and the text of its string literals blanked, but for a
    literal that is exactly LEGION_TEST_OMP or omp, since a door and a start are made of one."""
    return scan.code(text, lang, KEPT_LITERALS.__contains__)


def function_spans(code, opener):
    """For each line, the index of the first line of the top-level function that holds it, and
    each function's (first, last) line, from its opener to the next line that is `}` at column 0,
    which gofmt and the TypeScript formatter both put there."""
    holder, spans, start = [None] * len(code), {}, None
    for k, line in enumerate(code):
        if start is None and opener.match(line):
            start = k
            if line.count("{") and line.count("{") == line.count("}"):
                holder[k], spans[k], start = k, (k, k), None
                continue
        if start is not None:
            holder[k] = start
            if re.match(r"\}", line):
                spans[start] = (start, k)
                start = None
    return holder, spans


line_of = scan.line_of


def reasoned(raw, lang, *lines):
    return scan.reasoned(raw, lang, REASON, lines)


GO_FUNC = re.compile(r"func\b")
TS_FUNC = re.compile(r"(?:export\s+)?(?:async\s+)?function\b")
# A file none of whose text could make a start or define a helper is not lexed: the lexer reads a
# character at a time, and most test files start no process at all.
GO_MAYBE = re.compile(r"testbin\.OMP\(|" + DOOR_NAME + r"|exec\.Command|os\.(?:Getenv|LookupEnv)\(")
TS_MAYBE = re.compile(r"\b(?:spawn|spawnSync|execFile|execFileSync|execa|execaSync)\s*\(|\$`|function omp")

go_files, ts_files, door_consts = [], [], {}
for path, lang in scan.sources():
    if lang not in ("go", "ts"):
        continue
    if lang == "go":
        text = scan.read(path)
        if DOOR_NAME in text:
            for name in GO_CONST.findall("\n".join(code_of(text, "go"))):
                door_consts.setdefault(os.path.dirname(path), set()).add(name)
    if test_code(path, lang):
        (go_files if lang == "go" else ts_files).append(path)

errors = []
starts = {"go": 0, "ts": 0}
helped = reasons = 0
testbin_dirs, harnesses = set(), set()
go_helper_defs, ts_helper_defs = {}, {}

for path in go_files:
    raw_text = scan.read(path)
    directory = os.path.dirname(path)
    in_testbin = os.path.basename(directory) == "testbin"
    if in_testbin:
        testbin_dirs.add(directory)
    elif not GO_MAYBE.search(raw_text):
        continue
    raw = raw_text.split("\n")
    code = code_of(raw_text, "go")
    holder, spans = function_spans(code, GO_FUNC)
    for first, (_, end) in spans.items():
        name = re.match(r"func\s+(\w+)", code[first])
        if in_testbin and name and name.group(1) in ("OMPOffline", "OMPEnviron"):
            go_helper_defs[name.group(1)] = (path, first, "\n".join(raw[first:end + 1]), "\n".join(code[first:end + 1]))
    helper = GO_HELPER_IN_TESTBIN if in_testbin else GO_HELPER
    joined = "\n".join(code)
    found = []
    if not in_testbin:
        found += [(line_of(joined, m.start()), "takes the real Oh My Pi (testbin.OMP)") for m in GO_DOOR.finditer(joined)]
    for m in GO_GETENV.finditer(joined):
        if m.group(1) == f'"{DOOR_NAME}"' or m.group(1) in door_consts.get(directory, ()):
            found.append((line_of(joined, m.start()), f"reads {DOOR_NAME}"))
    found += [(line_of(joined, m.start()), "starts Oh My Pi") for m in GO_EXEC.finditer(joined)]
    for index, what in sorted(set(found)):
        starts["go"] += 1
        start = holder[index]
        body = "\n".join(code[start:spans[start][1] + 1]) if start is not None and start in spans else ""
        if body and helper.search(body):
            helped += 1
        elif reasoned(raw, "go", index):
            reasons += 1
        else:
            errors.append((path, index + 1, f"a test {what} in a function that calls neither testbin.OMPOffline nor "
                                            f"testbin.OMPEnviron, and gives no omp-offline reason: {raw[index].strip()}"))

for path in ts_files:
    raw_text = scan.read(path)
    if os.path.basename(path) == "omp-harness.ts":
        harnesses.add(path)
    if not TS_MAYBE.search(raw_text):
        continue
    raw = raw_text.split("\n")
    code = code_of(raw_text, "ts")
    _, spans = function_spans(code, TS_FUNC)
    for first, (_, end) in spans.items():
        name = re.match(r"(?:export\s+)?(?:async\s+)?function\s+(\w+)", code[first])
        if name and name.group(1) in ("ompOffline", "ompEnv"):
            ts_helper_defs[name.group(1)] = (path, first, "\n".join(raw[first:end + 1]), "\n".join(code[first:end + 1]))
    joined = "\n".join(code)
    found = []
    for m in TS_SPAWN.finditer(joined):
        command = m.group(2).strip()
        if TS_COMMAND.fullmatch(command) or DOOR_NAME in command:
            found.append((line_of(joined, m.start()), line_of(joined, m.start(2)), m.group(1).endswith("Sync")))
    for m in TS_SHELL.finditer(joined):
        command = m.group(1).strip()
        if TS_COMMAND.fullmatch(command) or DOOR_NAME in command:
            found.append((line_of(joined, m.start()), line_of(joined, m.start(1)), False))
    for call, command, sync in sorted(set(found)):
        starts["ts"] += 1
        after = [k for k in range(command + 1, len(code)) if code[k].strip()][:TS_AFTER]
        window = range(call, (after[-1] if after else command) + 1)
        offline = any(TS_HELPER.search(code[k]) for k in window)
        if offline and not sync:
            helped += 1
        elif reasoned(raw, "ts", call, command):
            reasons += 1
        elif offline:
            errors.append((path, call + 1, "a Sync start of Oh My Pi under ompOffline's proxy: the proxy's listener "
                                           "answers only while the event loop runs, which the Sync call holds; "
                                           "start it with Bun.spawn: " + raw[call].strip()))
        else:
            errors.append((path, call + 1, "a start of the real Oh My Pi whose environment calls neither ompOffline nor "
                                           "ompEnv, and gives no omp-offline reason: " + raw[call].strip()))

blind = [name for name, files in (("Go", go_files), ("TypeScript", ts_files)) if not files]
if blind:
    scan.unknown(f"read no {' and no '.join(blind)} test file under {os.getcwd()}, so their starts of Oh My Pi are "
                 "unseen")
if testbin_dirs and starts["go"] == 0:
    scan.unknown(f"{', '.join(sorted(testbin_dirs))} exists and no Go test starts Oh My Pi, so the reading that "
                 "finds the starts sees none")
if harnesses and starts["ts"] == 0:
    scan.unknown(f"{', '.join(sorted(harnesses))} exists and no TypeScript test starts Oh My Pi, so the reading "
                 "that finds the starts sees none")

# The helpers themselves: each names the proxy, and each environment helper calls its own.
for kind, present, defs, offline, environ, call in (
        ("Go", testbin_dirs, go_helper_defs, "OMPOffline", "OMPEnviron", r"(?<![\w.])OMPOffline\("),
        ("TypeScript", harnesses, ts_helper_defs, "ompOffline", "ompEnv", r"(?<![\w$])(?<![\w$)\]]\.)ompOffline\(")):
    if not present:
        continue
    if offline not in defs or "HTTPS_PROXY" not in defs[offline][2]:
        where = defs[offline][0] if offline in defs else sorted(present)[0]
        errors.append((where, defs[offline][1] + 1 if offline in defs else 1,
                       f"no {kind} {offline} that names HTTPS_PROXY, the helper every start of Oh My Pi is held to"))
    if environ in defs and not re.search(call, defs[environ][3]):
        errors.append((defs[environ][0], defs[environ][1] + 1, f"{environ} does not call {offline}"))

total = starts["go"] + starts["ts"]
summary = f"{total} starts of Oh My Pi in {len(go_files) + len(ts_files)} test files read (Go {starts['go']}, TypeScript {starts['ts']})"
if errors:
    scan.refuse(errors, summary, "A test's Oh My Pi reaches no network. Run it under testbin.OMPEnviron's "
                "environment or with testbin.OMPOffline's entry (Go), or with ompOffline's from "
                "@legion/pi-shared/test/omp-offline (TypeScript), or, where the run reaches nothing or the program "
                "is not Oh My Pi, say so beside it in an `omp-offline: <why>` comment.")
print(f"every one of the {summary} runs under its language's offline helper ({helped}) or carries a reason ({reasons})")
PY
