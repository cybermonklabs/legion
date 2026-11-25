#!/usr/bin/env bash
# Converts a branch that adds a numbered entry to packages/daemon/internal/api/version.go, by the
# rule the daemon API contract kept before dispatch://INFRA-1569, into an entry file of that
# change (R8). After it no committed file holds a contract number above the last numbered entry:
# main's version.go declares no `const DaemonAPIVersion`, SupportedContracts is the registry's,
# and the manifests, version.json and the state marks carry no number. So a branch made before it
# that adds a numbered entry conflicts with main in version.go and answers.go, and the contract
# check refuses a numbered entry added after the cutover. This script is the one round each such
# branch pays, once.
#
# It runs on the current branch against the branch's merge base with main (or with the base branch
# that --base names), which must be a commit from before the cutover (one whose version.go declares
# `const DaemonAPIVersion`): a branch that has already merged the cutover is refused. It reads the
# branch at HEAD and that base out of git, and does the six steps of the design ("The cutover and
# open pull requests"):
#
#   1. Each numbered entry the branch adds, a `// <n>: ` paragraph of version.go that the base does
#      not hold, becomes a new entry file, packages/daemon/internal/api/contract_<id>_<slug>.go,
#      holding the entry's text verbatim after `// <id> <slug>: `. The ids follow the order of the
#      numbers, one second apart, from the second the script runs.
#   2. The entry's constant, a `const <x>Contract = <n>` of package api that the base does not
#      declare, moves into that file as the entry's declaration, with the id as its value and its
#      own doc comment as a paragraph of the entry's text. The slug is the constant's name less
#      `Contract`, in kebab case. An entry with no constant gets one, named for the issue key its
#      text starts with (`dispatch://INFRA-4508` gives infra4508Contract), else entry<n>Contract.
#   3. Each row of a frozen table that names that constant, and changes no row the base holds,
#      moves verbatim into the entry's registerEntry call: a band of stateNarrowings (`bands`), a
#      row of Answers (`answers`, with the AnswerID the branch declared for it and its place in
#      shapeAnswer's identity case, `identityAnswers`), a row of typedSettings, an EnumeratedField
#      row of stateEnumeratedFields or enumeratedFields() (`enumeratedFields`, with the list of
#      values the branch declared for it where no other code reads that list), a row of
#      AcceptedByName and one of RemovedRoutes. A value added to an existing enumerated field
#      stays: which rows read its list is the author's to say (`enumeratedValues`).
#   4. The branch's edits of each copy the cutover derives go back to the base's: the const line
#      of version.go, the declaration of SupportedContracts, `legion.daemonApiVersion` of the
#      pi-legion and claude-envoy manifests, the `contract` marks of state.json and
#      state-narrowed-repositories.json, and the whole of version.json, contract-shapes.json,
#      request-shapes.json, answers.json and enumerated-fields.json. What the last four held of an
#      entry, the entry fixture records: step 5 writes it from the rows step 3 moved and from what
#      the daemon serves at the entry's number. That gives each answer's record, because the shape
#      gate before the cutover held every answer at every contract to its record; it does not give
#      each route's, because one build's request types read the same fields at every number, so of
#      two entries that each change a request the first would take both changes. So before
#      request-shapes.json goes back, each entry's request part is kept: the routes and field paths
#      by which the branch's record of its number differs from the record of the number before it
#      (a number the file holds no record of changes nothing). It is kept in the git directory, at
#      `git rev-parse --git-path daemon-contract-convert/<branch>/entry-requests.json`, so that it
#      outlasts the run.
#   5. It merges main and runs `go test ./internal/api -run '^TestEntryFixtures$' -update
#      -entry-requests=<that file>`, which writes the fixture of each entry that has none, its
#      request part made over the record of the entries before it. While a row of a frozen table
#      still names an entry, that entry's fixture would leave the row out, so TestEntryFixtures
#      writes none, and the author runs the same `-update` once the rows have moved (step 6).
#   6. It prints `<file>:<line>: <text>` for each line it leaves for the author, at its line after
#      the merge: each row naming an entry's constant that it could not move, every other line of
#      Go code that names an entry's constant (a comparison that must read entryNumber(<x>), a
#      test row), each line the branch added holding an entry's number, and each other line the
#      branch added to a frozen table (a route added to an existing answer, a value strip).
#      TestNoFrozenRowNamesAnEntry fails until each row of a frozen table that names an entry is in
#      that entry's file; the author moves the rows, then writes the fixture with `-update` (or
#      again with `-update-entry=<id>` where step 5 wrote one), with `-entry-requests=<that file>`.
#
# The result is one commit, the merge of main, whose message names each entry, each row moved,
# each copy undone, each fixture written and each line left. Nothing is committed when a step
# before the merge fails. Where the merge conflicts, the conversion alone is committed and the
# conflicting files are named; the author merges main and runs the `-update` of step 5 by hand.
#
#   --base <ref>  the ref the branch merges, its pull request's base branch: main after the cutover,
#                 default refs/remotes/origin/main, which messages call main. A branch stacked on
#                 another branch names that branch, and it takes main's place in each step above:
#                 the script converts only the entries the branch adds over its merge base with
#                 that branch, so no entry the base branch converted is converted again under
#                 another id. A ref whose version.go git cannot read, or that still declares
#                 `const DaemonAPIVersion`, is refused, naming it: the entry files call
#                 registerEntry, which a tree from before the cutover does not define. Where it is
#                 main, fetch main. Where it is the base branch, that branch has not merged the
#                 cutover yet: convert it if it adds a numbered entry, else merge main into it, then
#                 run this with --base naming it. A branch stacked on a base branch that main then
#                 took as a squash, which GitHub retargets to main, still carries that branch's
#                 entries, and main holds them as entry files: an entry whose text opens an entry
#                 file of the ref is refused, naming that file; run this with --base naming the base
#                 branch's last commit before the squash, then merge main. An entry whose constant
#                 an entry file of the ref declares (a text edited after the conversion keeps that
#                 name) is refused too, since two declarations of it do not compile, naming both
#                 causes: either it is such an entry, or another change took that name, and then
#                 the author renames the entry's constant in the branch. An entry whose text changed
#                 after its conversion and that declares no constant, or whose constant was renamed
#                 too, is not caught. A branch with more than one merge
#                 base with a ref that holds the cutover, which merged a commit of the ref's history
#                 apart from the ref, is refused, naming them: which entries are the branch's own
#                 would hang on the one git picks. Merge into the branch the ref as it was before it
#                 merged the cutover. Where that leaves more than one merge base, make the branch
#                 that commit and its own commits, which leaves that commit the one merge base: `git
#                 rebase -i <that commit>` and drop each commit that came into the branch through a
#                 merge, or cherry-pick the branch's own commits onto a branch made at that commit.
#                 A plain `git rebase` replays the commits that came in through a merge too, and
#                 their numbered entries would read as the branch's own. A ref that has not merged
#                 the cutover is refused for that first.
#   --report      steps 1 to 4 read and nothing done: no file written, no merge, no Go toolchain,
#                 and no git read but of commits, trees, the blobs of the files the branch changed
#                 and those of the ref's entry files, so it runs in a blobless clone. It prints, for
#                 <rev> (default HEAD):
#                   base <sha> contract <n>                     the merge base and its number
#                   onmain <n> <number>                         entry <n>, which the ref holds as its
#                                                               numbered entry <number>
#                   onmain <n> <file>                           entry <n>, which the ref holds as the
#                                                               entry file <file>, by its text or its
#                                                               constant
#                   entry <n> <constant> <slug> <file:line|->   each entry it would convert
#                   move <file>:<line> <field> <constant>       each row it would move
#                   undo <file>                                 each derived copy it would undo
#                   <file>:<line>: <text>                       each line it would leave
#                 The entries an onmain line names are refused by the conversion, and the report
#                 leaves them out of the lines after it.
#
# Exits 0 when the branch is converted (or adds no numbered entry), and also where a row of a
# frozen table left for the author keeps step 5 from writing an entry's fixture, which records that
# row: the author runs the `-update` once the row has moved. Non-zero on a failure.
#
# Run from anywhere in the checkout: scripts/daemon-contract-convert.sh [--base <ref>]
#   or: scripts/daemon-contract-convert.sh --report [--base <ref>] [<rev>]
# Its test: scripts/daemon-contract-convert.test.sh.
set -euo pipefail

# The test whose `-update` writes the fixture of each entry that has none, and whose
# `-update-entry=<id>` writes that entry's again (entry_fixtures_test.go).
UPDATE_RUN='^TestEntryFixtures$'

die() {
  echo "daemon-contract-convert: $*" >&2
  exit 1
}

report=0
# The ref the branch merges, and the name messages give it: main, or the ref --base names.
onto=refs/remotes/origin/main
onto_name=main
rev=
while [ $# -gt 0 ]; do
  case $1 in
  --report) report=1 ;;
  --base) onto=${2:?--base needs a ref}; onto_name=$onto; shift ;;
  --base=*) onto=${1#--base=}; onto_name=$onto ;;
  -h | --help) sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
  -*) die "unknown option $1" ;;
  *) [ -z "$rev" ] || die "one revision at most"; rev=$1 ;;
  esac
  shift
done
if [ "$report" = 0 ] && [ -n "$rev" ]; then
  die "a revision is for --report alone: the conversion runs on the current branch"
fi

root=$(git rev-parse --show-toplevel) || die "not in a git checkout"
cd "$root"
onto_sha=$(git rev-parse --verify -q "$onto^{commit}") || die "cannot read $onto"
head_sha=$(git rev-parse --verify -q "${rev:-HEAD}^{commit}") || die "cannot read ${rev:-HEAD}"

# convert_py runs the conversion's reading and writing, one phase per call:
#   analyze report|convert HEAD MAIN NAME STATE REQUESTS REQUESTS_AT
#                                                  steps 1 to 4 over the merge base of HEAD and
#                                                  MAIN, the ref the branch merges, which messages
#                                                  call NAME (convert writes them, STATE, and the
#                                                  entries' request parts at REQUESTS, which is
#                                                  `git rev-parse --git-path REQUESTS_AT`)
#   finish STATE                                   step 6 and the commit message, after the merge
convert_py() {
  python3 - "$@" <<'PY'
import bisect
import datetime
import json
import os
import re
import subprocess
import sys

API = "packages/daemon/internal/api"
VERSION_GO = f"{API}/version.go"
ANSWERS_GO = f"{API}/answers.go"
SHAPING_GO = f"{API}/shaping.go"
REQUESTS_GO = f"{API}/requests.go"
FIXTURES = "packages/contracts/fixtures/daemon-api"
MANIFESTS = ("packages/pi-legion/package.json", "packages/claude-envoy/package.json")
STATE_GOLDENS = (f"{FIXTURES}/state.json", f"{FIXTURES}/state-narrowed-repositories.json")
REQUEST_SHAPES = f"{FIXTURES}/request-shapes.json"
WHOLE_COPIES = tuple(f"{FIXTURES}/{name}" for name in (
    "version.json", "contract-shapes.json", "request-shapes.json", "answers.json", "enumerated-fields.json"))
# An entry's slug: words of lowercase letters and digits joined by hyphens.
SLUG_WORDS = r"[a-z0-9]+(?:-[a-z0-9]+)*"
SLUG = re.compile(rf"^{SLUG_WORDS}$")
ENTRY_FILE = re.compile(rf"(?:^|/)contract_\d{{14}}_{SLUG_WORDS}(?:_test)?\.go$")
HEADER = re.compile(r"^// (\d+): (.*)$")
# The first line of an entry file's text, as entry_file and scripts/daemon-contract-entry.sh write it.
ENTRY_HEADER = re.compile(rf"^// \d{{14}} {SLUG_WORDS}:(?: (.*))?$")
# An entry file's declaration of its entry's constant, whose value is the entry's id.
ENTRY_CONST = re.compile(r"^const (\w+) = \d{14}$", re.M)
CONST_LINE = re.compile(r"^const DaemonAPIVersion = (\d+)\s*$", re.M)

# Each frozen table of the base, by the variable it is (or, for the rows enumeratedFields()
# appends, the function), and the field of contractEntry its rows move to. valueStrips' rows name
# no contract, so none moves; a row the branch added there is a line left for the author.
TABLES = {
    "stateNarrowings": "bands",
    "valueStrips": None,
    "Answers": "answers",
    "typedSettings": "typedSettings",
    "stateEnumeratedFields": "enumeratedFields",
    "enumeratedFields()": "enumeratedFields",
    "AcceptedByName": "acceptedByName",
    "RemovedRoutes": "removedRoutes",
}
TABLE_FILES = (ANSWERS_GO, SHAPING_GO, REQUESTS_GO)
# contractEntry's fields in the order the entry file writes them, with each one's Go type.
FIELDS = (
    ("bands", "[]stateNarrowing"),
    ("answers", "[]Answer"),
    ("identityAnswers", "[]AnswerID"),
    ("enumeratedFields", "[]EnumeratedField"),
    ("typedSettings", "[]TypedSetting"),
    ("removedRoutes", "map[string]int"),
    ("acceptedByName", "[]AcceptedField"),
)


def die(message):
    sys.stderr.write(f"daemon-contract-convert: {message}\n")
    sys.exit(1)


def git(*args, data=None):
    result = subprocess.run(["git", *args], input=data, capture_output=True)
    if result.returncode:
        die(f"git {' '.join(args)}: {result.stderr.decode(errors='replace').strip()}")
    return result.stdout


def read_blobs(specs):
    """The text of each `<rev>:<path>`, or None where the tree holds no such file. One cat-file
    reads them all, so a blobless clone fetches each missing blob in the one process."""
    specs = list(dict.fromkeys(specs))
    if not specs:
        return {}
    out = git("cat-file", "--batch", data=("\n".join(specs) + "\n").encode())
    texts, at = {}, 0
    for spec in specs:
        newline = out.index(b"\n", at)
        header = out[at:newline].decode()
        at = newline + 1
        if header.endswith(" missing"):
            texts[spec] = None
            continue
        size = int(header.split()[2])
        texts[spec] = out[at:at + size].decode()
        at += size + 1
    return texts


# ---- Go source, read as far as the frozen tables need: tokens, brackets and declarations. ----

TOKEN = re.compile(r"""
    (?P<space>\s+)
  | (?P<comment>//[^\n]*|/\*.*?\*/)
  | (?P<string>`[^`]*`|"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*')
  | (?P<ident>[^\W\d]\w*)
  | (?P<number>\.?\d[\w.]*)
  | (?P<op>.)
""", re.S | re.X)


class Source:
    def __init__(self, path, text):
        self.path, self.text = path, text
        self.tokens = [(m.lastgroup, m.group(), m.start(), m.end())
                       for m in TOKEN.finditer(text) if m.lastgroup != "space"]
        self.starts = [0] + [m.end() for m in re.finditer(r"\n", text)]
        self.match, stack = {}, []
        for i, (kind, word, _, _) in enumerate(self.tokens):
            if kind != "op":
                continue
            if word in "([{":
                stack.append(i)
            elif word in ")]}" and stack:
                j = stack.pop()
                self.match[i], self.match[j] = j, i
        self._decls = None

    def line(self, offset):
        return bisect.bisect_right(self.starts, offset)

    def line_start(self, offset):
        return self.starts[self.line(offset) - 1]

    def line_end(self, offset):
        newline = self.text.find("\n", offset)
        return len(self.text) if newline < 0 else newline + 1

    def line_text(self, number):
        start = self.starts[number - 1]
        return self.text[start:self.line_end(start)].rstrip("\n")

    def leads_line(self, i):
        start = self.tokens[i][2]
        return self.text[self.line_start(start):start].strip() == ""

    def next_code(self, i):
        i += 1
        while i < len(self.tokens) and self.tokens[i][0] == "comment":
            i += 1
        return i

    def lead_comments(self, first, floor):
        """The first token of the comment lines directly above token `first`, none before
        `floor`: a doc comment, or the comment a row of a table carries above it."""
        line, j = self.line(self.tokens[first][2]), first - 1
        while j > floor and self.tokens[j][0] == "comment" and self.tokens[j][1].startswith("//") \
                and self.line(self.tokens[j][2]) == line - 1 and self.leads_line(j):
            first, line, j = j, line - 1, j - 1
        return first

    def decls(self):
        if self._decls is None:
            keywords, depth = [], 0
            for i, (kind, word, start, _) in enumerate(self.tokens):
                if kind == "op" and word in "([{":
                    depth += 1
                elif kind == "op" and word in ")]}":
                    depth -= 1
                elif kind == "ident" and depth == 0 and word in ("var", "const", "func", "type", "import") \
                        and (start == 0 or self.text[start - 1] == "\n"):
                    # A declaration of the file starts its line, as gofmt writes it; a `func` at
                    # depth 0 elsewhere is the type of a variable (`var hook func(string)`).
                    keywords.append(i)
            self._decls = []
            for n, kw in enumerate(keywords):
                last = (keywords[n + 1] if n + 1 < len(keywords) else len(self.tokens)) - 1
                while last > kw and self.tokens[last][0] == "comment":
                    last -= 1
                self._decls.append(Decl(self, kw, last, keywords[n - 1] if n else -1))
        return self._decls

    def code_between(self, first, last):
        return [t for t in self.tokens[first:last + 1] if t[0] != "comment"]

    def idents_at(self, names):
        """Each code token that is one of names, as (offset, name)."""
        return [(t[2], t[1]) for t in self.tokens if t[0] == "ident" and t[1] in names]


class Decl:
    def __init__(self, src, kw, last, floor):
        self.src, self.kw, self.last = src, kw, last
        self.keyword = src.tokens[kw][1]
        name = src.next_code(kw)
        if self.keyword == "func" and src.tokens[name][1] == "(":
            name = src.next_code(src.match[name])
        self.name = src.tokens[name][1] if src.tokens[name][0] == "ident" else None
        self.block = src.tokens[name][1] == "(" and self.keyword != "func"
        self.doc = src.lead_comments(kw, floor)
        self.start = src.line_start(src.tokens[self.doc][2])
        self.end = src.line_end(src.tokens[last][3])

    def text(self):
        return self.src.text[self.start:self.end]

    def doc_lines(self):
        return self.src.text[self.start:self.src.line_start(self.src.tokens[self.kw][2])].splitlines()

    def open_brace(self):
        """The `{` of the composite literal a `var x = T{...}` declares."""
        seen_assign = False
        for i in range(self.kw, self.last + 1):
            kind, word = self.src.tokens[i][:2]
            if kind == "op" and word == "=":
                seen_assign = True
            elif seen_assign and kind == "op" and word == "{":
                return i
            elif seen_assign and kind == "op" and word == "(":
                return None
        return None


class Element:
    """One element of a composite literal or argument list, with the comment lines above it and
    the comment that trails its comma."""

    def __init__(self, src, first, last, comma, floor):
        self.src, self.first, self.last, self.comma = src, first, last, comma
        self.lead = src.lead_comments(first, floor)
        self.start, self.end = src.tokens[first][2], src.tokens[last][3]
        code = src.code_between(first, last)
        self.norm = " ".join(t[1] for t in code)
        self.idents = {t[1] for t in code if t[0] == "ident"}
        tail = src.tokens[comma][3] if comma is not None else self.end
        after = src.next_code(comma) if comma is not None else None
        trailing = comma + 1 if comma is not None else None
        if trailing is not None and trailing < len(src.tokens) and src.tokens[trailing][0] == "comment" \
                and src.line(src.tokens[trailing][2]) == src.line(tail):
            tail = src.tokens[trailing][3]
        self.block_start = src.line_start(src.tokens[self.lead][2])
        self.block_end = src.line_end(tail)
        rest = src.text[tail:self.block_end].strip()
        # A row moves as whole lines: it leads its first line and its comma ends its last.
        self.whole_lines = comma is not None and src.leads_line(self.lead) and rest == "" and (
            after is None or after >= len(src.tokens) or src.line(src.tokens[after][2]) > src.line(tail))
        self.line = src.line(self.start)

    def lines(self):
        """The row as the entry file writes it: its own lines, with a comma where it had none, and
        without the type name an argument of append() spells out (`EnumeratedField{`)."""
        if self.whole_lines:
            text = self.src.text[self.block_start:self.block_end]
        else:
            text = self.src.text[self.block_start:self.end] + ","
        lines = text.rstrip("\n").split("\n")
        code = next(i for i, line in enumerate(lines) if not line.lstrip().startswith("//"))
        lines[code] = re.sub(r"^(\s*)EnumeratedField\{", r"\1{", lines[code])
        return lines


def elements(src, open_i):
    close, out, first, last = src.match[open_i], [], None, None
    floor, i = open_i, open_i + 1
    while i < close:
        kind, word = src.tokens[i][:2]
        if kind == "comment":
            i += 1
            continue
        if kind == "op" and word == ",":
            if first is not None:
                out.append(Element(src, first, last, i, floor))
            first, floor = None, i
            i += 1
            continue
        if first is None:
            first = i
        if kind == "op" and word in "([{":
            i = src.match[i]
        last = i
        i += 1
    if first is not None:
        out.append(Element(src, first, last, None, floor))
    return out


class Table:
    def __init__(self, name, src, open_i, rows):
        self.name, self.src, self.open, self.rows = name, src, open_i, rows
        self.start, self.end = src.tokens[open_i][2], src.tokens[src.match[open_i]][3]

    def lines(self):
        return self.src.text[self.start:self.end].splitlines()


def tables(src):
    found = {}
    for decl in src.decls():
        if decl.keyword == "var" and decl.name in TABLES:
            open_i = decl.open_brace()
            if open_i is not None:
                found[decl.name] = Table(decl.name, src, open_i, elements(src, open_i))
        elif decl.keyword == "func" and decl.name == "enumeratedFields":
            calls = [i for i in range(decl.kw, decl.last) if src.tokens[i][1] == "append"
                     and src.tokens[src.next_code(i)][1] == "("]
            if calls:
                paren = src.next_code(calls[-1])
                found["enumeratedFields()"] = Table("enumeratedFields()", src, paren, elements(src, paren)[1:])
    return found


def value_lists(src):
    """Each package-level list of enumerated values: `[]EnumeratedValue{...}` or valuesFrom()."""
    return {d.name: d for d in src.decls() if d.keyword == "var" and d.name and not d.block
            and re.search(r"=\s*(?:append\()?(?:\[\]EnumeratedValue\{|valuesFrom\()", d.text())}


def identity_case(src):
    """The case of shapeAnswer that answers each listed id with the body as written, as
    (case token, colon token, [id tokens])."""
    for decl in src.decls():
        if decl.keyword != "func" or decl.name != "shapeAnswer":
            continue
        best = None
        for i in range(decl.kw, decl.last):
            if src.tokens[i][:2] != ("ident", "case"):
                continue
            ids, j = [], src.next_code(i)
            while j < decl.last and src.tokens[j][1] != ":":
                if src.tokens[j][0] == "ident":
                    ids.append(j)
                elif src.tokens[j][1] != ",":
                    ids = None
                    break
                j = src.next_code(j)
            if ids and all(src.tokens[k][1].startswith("Answer") for k in ids) \
                    and (best is None or len(ids) > len(best[2])):
                best = (i, j, ids)
        return best
    return None


def string_field(norm, field):
    match = re.search(r"\b" + field + r' : ("(?:[^"\\]|\\.)*"|\w+)', norm)
    return match.group(1) if match else None


def identity(table, row):
    """What a row is a row of: two rows with one identity are one row, changed."""
    if table == "Answers":
        key = string_field(row.norm, "ID")
    elif table == "typedSettings":
        key = string_field(row.norm, "Key")
    elif table in ("stateEnumeratedFields", "enumeratedFields()"):
        key = (string_field(row.norm, "Answer"), string_field(row.norm, "KeyPath"))
        key = key if all(key) else None
    elif table == "AcceptedByName":
        key = (string_field(row.norm, "Route"), string_field(row.norm, "Field"))
        key = key if all(key) else None
    elif table == "RemovedRoutes":
        key = row.norm.split(" : ")[0]
    else:
        # A band names its own contract, so a band that names an entry is a new row.
        return row.norm
    return key or row.norm


def imports(src):
    """Each import of the file: the name it is read by, and its spec line."""
    out = {}
    for decl in src.decls():
        if decl.keyword != "import":
            continue
        first, last = decl.kw + 1, decl.last
        specs, current = [], []
        for t in src.code_between(first, last):
            if t[1] in "()":
                continue
            current.append(t)
            if t[0] == "string":
                specs.append(current)
                current = []
        for spec in specs:
            path = spec[-1][1].strip('"`')
            name = spec[0][1] if len(spec) == 2 else path.rsplit("/", 1)[-1]
            out[name] = " ".join(t[1] for t in spec)
    return out


def selectors(src, start, end):
    """Each package name read in src between two offsets, as in `claim.RoleImplementer`."""
    names = set()
    toks = src.tokens
    for i, t in enumerate(toks):
        if t[2] < start or t[3] > end or t[0] != "ident":
            continue
        if i + 1 < len(toks) and toks[i + 1][1] == "." and (i == 0 or toks[i - 1][1] != "."):
            names.add(t[1])
    return names


# ---- The entries the branch adds to version.go. ----

def numbered(text):
    lines = text.split("\n")
    const = next((i for i, line in enumerate(lines) if CONST_LINE.match(line)), None)
    heads = [i for i, line in enumerate(lines[:const]) if HEADER.match(line)]
    entries = []
    for n, h in enumerate(heads):
        stop = heads[n + 1] if n + 1 < len(heads) else const
        if n + 1 < len(heads) and lines[stop - 1] == "//":
            stop -= 1
        body = lines[h:stop]
        while body and body[-1] == "//":
            body.pop()
        entries.append({"number": int(HEADER.match(lines[h]).group(1)), "first": h, "stop": stop, "lines": body})
    return lines, const, entries


def joined(parts):
    return re.sub(r"\s+", " ", " ".join(parts)).strip()


def summary(entry):
    """An entry's first three lines without its number: what a renumber leaves as it was."""
    return joined([HEADER.match(entry["lines"][0]).group(2)] + [line[2:] for line in entry["lines"][1:3]])


def held_in_entry_files(ref, entries):
    """The entry file of ref that holds each entry, by the entry's number, as (path, None) where its
    text opens with the entry's summary (an entry file writes the entry's text verbatim after `// <id>
    <slug>: `), else as (path, constant) where it declares the constant the entry declares, whose name
    an edit of the text after the conversion leaves as it was."""
    if not entries:
        return {}
    paths = [p for p in git("ls-tree", "--name-only", ref, f"{API}/").decode().split("\n")
             if ENTRY_FILE.search(p) and not p.endswith("_test.go")]
    texts = read_blobs([f"{ref}:{p}" for p in paths])
    files = []
    for path in paths:
        text = texts[f"{ref}:{path}"]
        if text is None:
            die(f"cannot read {path} at {ref[:10]}")
        lines = text.split("\n")
        first = next((i for i, line in enumerate(lines) if ENTRY_HEADER.match(line)), None)
        opening = None
        if first is not None:
            rest = []
            for line in lines[first + 1:]:
                if not line.startswith("//"):
                    break
                rest.append(line[2:])
            opening = joined([ENTRY_HEADER.match(lines[first]).group(1) or ""] + rest)
        files.append((path, opening, set(ENTRY_CONST.findall(text))))
    held = {}
    for e in entries:
        want = summary(e)
        path = next((p for p, opening, _ in files
                     if opening is not None and (opening == want or opening.startswith(want + " "))), None)
        if path:
            held[e["number"]] = (path, None)
            continue
        path = next((p for p, _, consts in files if e["const_path"] and e["const"] in consts), None)
        if path:
            held[e["number"]] = (path, e["const"])
    return held


def kebab(name):
    words = re.findall(r"[A-Z]+(?=[A-Z][a-z]|\d|$)|[A-Z]?[a-z]+|[A-Z]+|\d+", name)
    return "-".join(w.lower() for w in words)


def lower_camel(slug):
    words = slug.split("-")
    return words[0] + "".join(w[:1].upper() + w[1:] for w in words[1:])


def json_contract_block(text):
    """The lines of a state golden's top-level `contract` object, as (first, stop) line indexes."""
    lines = text.split("\n")
    for i, line in enumerate(lines):
        if re.match(r'^  "contract": \{\s*$', line):
            for j in range(i + 1, len(lines)):
                if re.match(r"^  \}", lines[j]):
                    return i, j + 1
    return None


def manifest_number(text):
    match = re.search(r'^\s*"daemonApiVersion":\s*(\d+)', text or "", re.M)
    return (int(match.group(1)), match) if match else (None, None)


def request_parts(text, base_number, numbers):
    """What each of numbers changes of the requests, by the records of request-shapes.json as the
    branch holds it (text), in the form of an entry fixture's `routes`: the routes and field paths
    by which the record of the number differs from the record before it, from the base's own
    number up. The rule before the cutover wrote a number's records while it was the daemon's own
    and kept them after, so they tell one entry's change from the next one's, which one build's
    request types, reading the same fields at every number, cannot. A number the file holds no
    record of changes nothing: the record before it still stands."""
    records = {}
    for record in json.loads(text) if text else []:
        records.setdefault(record["contract"], {})[record["route"]] = set(record["fieldPaths"] or ())
    below = [c for c in records if c <= base_number]
    before = records[max(below)] if below else {}
    parts = {}
    for number in sorted(numbers):
        parts[number] = part = []
        at = records.get(number)
        if at is None:
            continue
        for route in sorted(set(before) | set(at)):
            if route not in at:
                part.append({"route": route, "change": "remove"})
                continue
            was = before.get(route, set())
            change = {"route": route} if route in before else {"route": route, "change": "add"}
            if at[route] - was:
                change["addFieldPaths"] = sorted(at[route] - was)
            if was - at[route]:
                change["removeFieldPaths"] = sorted(was - at[route])
            if len(change) > 1:
                part.append(change)
        before = at
    return parts


def changed_paths(base, head, *paths):
    return [p for p in git("diff", "--name-only", "--no-renames", base, head, "--", *paths).decode().split("\n") if p]


def added_lines(base, head, paths):
    """Each line the branch added to each path, as {path: {line number: text}}. Reading the diff
    fetches, in a blobless clone, the blobs of both sides in one batch."""
    out = {}
    if not paths:
        return out
    diff = git("diff", "--no-color", "--no-ext-diff", "--no-renames", "-U0", base, *( [head] if head else [] ),
               "--", *paths).decode(errors="replace")
    path, number = None, 0
    for line in diff.split("\n"):
        if line.startswith("+++ "):
            path = line[6:] if line.startswith("+++ b/") else None
            out.setdefault(path, {})
        elif line.startswith("@@"):
            number = int(re.match(r"@@ -\S+ \+(\d+)", line).group(1))
        elif line.startswith("+") and path:
            out[path][number] = line[1:]
            number += 1
    return out


class Edits:
    """The edits of one file's text, applied at the end from the last one back."""

    def __init__(self, text):
        self.text, self.spans = text, []

    def replace(self, start, end, new=""):
        for s, e, _ in self.spans:
            if start < e and s < end:
                return False
        self.spans.append((start, end, new))
        return True

    def covers(self, offset):
        return any(s <= offset < e for s, e, _ in self.spans)

    def apply(self):
        text = self.text
        for start, end, new in sorted(self.spans, reverse=True):
            text = text[:start] + new + text[end:]
            # A declaration cut from between two blank lines leaves one of them.
            if not new and text[max(0, start - 2):start] == "\n\n" and text[start:start + 1] == "\n":
                text = text[:start] + text[start + 1:]
        return text


def merge_bases(head, ref):
    """Each merge base of head and ref, as `git merge-base --all` prints them: none where the two share
    no commit, where git exits 1 and prints nothing."""
    result = subprocess.run(["git", "merge-base", "--all", head, ref], capture_output=True)
    if result.returncode and (result.returncode != 1 or result.stdout.strip() or result.stderr.strip()):
        die(f"git merge-base --all {head} {ref}: {result.stderr.decode(errors='replace').strip()}")
    return result.stdout.decode().split()


def before_cutover(name):
    """The commit a refusal tells the author to merge or rebase onto: name as it was before it merged
    the cutover."""
    return (f"{name} as it was before it merged the cutover (the first parent of the commit that took "
            f"`const DaemonAPIVersion` out of its {VERSION_GO})")


def analyze(mode, head, main, main_name, state_path, requests_path="", requests_at=""):
    bases = merge_bases(head, main)
    version_specs = read_blobs([f"{rev}:{VERSION_GO}" for rev in (head, main, *bases[:1])])
    head_version, main_version = (version_specs[f"{rev}:{VERSION_GO}"] for rev in (head, main))
    # The entry files call registerEntry, which a tree from before the cutover does not define, so the
    # ref the branch merges must hold the cutover; a version.go that git cannot read shows no cutover,
    # and is refused too.
    if main_version is None:
        die(f"{main_name} ({main[:10]}) holds no {VERSION_GO} that git can read")
    if CONST_LINE.search(main_version):
        die(f"{main_name} ({main[:10]}) declares `const DaemonAPIVersion` in {VERSION_GO}: it has not merged the "
            "cutover, and the entry files would call registerEntry, which it does not define. Where it is main, "
            "fetch main. Where it is the base branch of a stacked branch, that branch has not merged the cutover "
            "yet: convert it if it adds a numbered entry, else merge main into it, then run this with --base naming it")
    # Which entries are the branch's own is read over its merge base with the ref. Where they have more
    # than one, git picks which it prints, so the branch is refused. That comes after the check of the
    # cutover above: before_cutover names a commit only a ref that has merged the cutover holds.
    if not bases:
        die(f"{head[:10]} and {main_name} share no commit")
    if len(bases) > 1:
        die(f"{head[:10]} and {main_name} have {len(bases)} merge bases, {', '.join(b[:10] for b in bases)}: the "
            f"branch merged a commit of the history of {main_name} apart from {main_name} itself, and which entries "
            f"are its own would hang on the one git picks. Merge {before_cutover(main_name)} into the branch, then "
            "run this again. Where that leaves more than one merge base, make the branch that commit and its own "
            "commits, which leaves that commit the one merge base: `git rebase -i <that commit>` and drop each commit "
            "that came into the branch through a merge, or cherry-pick the branch's own commits onto a branch made "
            "at that commit. A plain `git rebase` replays the commits that came in through a merge too, and their "
            "numbered entries would read as the branch's own")
    base = bases[0]
    base_version = version_specs[f"{base}:{VERSION_GO}"]
    if base_version is None or not CONST_LINE.search(base_version):
        die(f"the merge base {base[:10]} of {head[:10]} and {main_name} declares no `const DaemonAPIVersion` in "
            f"{VERSION_GO}: the branch has merged {main_name} after the cutover, and this script converts a branch "
            "made before it")
    base_number = int(CONST_LINE.search(base_version).group(1))
    print(f"base {base} contract {base_number}")
    if head_version is None or not CONST_LINE.search(head_version):
        die(f"{head[:10]} declares no `const DaemonAPIVersion` in {VERSION_GO}")
    base_heads = {e["number"] for e in numbered(base_version)[2]}
    head_lines, head_const, head_entries = numbered(head_version)
    entries = [e for e in head_entries if e["number"] not in base_heads and e["number"] > base_number]
    entries.sort(key=lambda e: e["number"])

    go_paths = [p for p in changed_paths(base, head, "packages/daemon") if p.endswith(".go")]
    copies = changed_paths(base, head, *MANIFESTS, *STATE_GOLDENS, *WHOLE_COPIES)
    added = added_lines(base, head, go_paths + [p for p in copies if p in MANIFESTS + STATE_GOLDENS])
    api_paths = [p for p in go_paths if p.startswith(API + "/") and "/" not in p[len(API) + 1:]]
    reads = set(go_paths) | {p for p in TABLE_FILES}
    texts = read_blobs([f"{head}:{p}" for p in sorted(reads)] + [f"{base}:{p}" for p in TABLE_FILES + (ANSWERS_GO,)]
                       + [f"{r}:{p}" for r in (base, head) for p in copies if p in MANIFESTS + STATE_GOLDENS])
    head_src = {p: Source(p, texts[f"{head}:{p}"]) for p in sorted(reads) if texts.get(f"{head}:{p}") is not None}
    base_src = {p: Source(p, texts[f"{base}:{p}"]) for p in TABLE_FILES if texts.get(f"{base}:{p}") is not None}
    edits = {p: Edits(s.text) for p, s in head_src.items()}

    # Step 2's constants: each `const <x>Contract = <n>` of package api the base does not declare.
    for e in entries:
        e.update(const=None, const_path=None, const_line=None, doc=[], rows={f: [] for f, _ in FIELDS},
                 decls=[], imports=set())
    for path in api_paths:
        if path.endswith("_test.go") or path not in head_src:
            continue
        src = head_src[path]
        for decl in src.decls():
            if decl.keyword != "const":
                continue
            specs = []
            if decl.block:
                open_i = src.next_code(decl.kw)
                i = open_i + 1
                while i < src.match[open_i]:
                    if src.tokens[i][0] == "ident" and src.leads_line(i):
                        line = src.line(src.tokens[i][2])
                        j = i
                        while j + 1 < src.match[open_i] and src.line(src.tokens[j + 1][2]) == line:
                            j += 1
                        specs.append((i, j))
                        i = j + 1
                    else:
                        i += 1
            else:
                specs.append((src.next_code(decl.kw), decl.last))
            for first, last in specs:
                code = src.code_between(first, last)
                if len(code) == 3 and code[0][0] == "ident" and code[1][1] == "=" and code[2][0] == "number" \
                        and code[2][1].isdigit() and re.search(r"[Cc]ontract$", code[0][1]):
                    entry = next((e for e in entries if e["number"] == int(code[2][1])), None)
                    if entry is None or entry["const"] is not None:
                        continue
                    if decl.block:
                        lead = src.lead_comments(first, decl.kw)
                        start = src.line_start(src.tokens[lead][2])
                        doc = src.text[start:src.line_start(src.tokens[first][2])].splitlines()
                        end = src.line_end(src.tokens[last][3])
                    else:
                        start, end, doc = decl.start, decl.end, decl.doc_lines()
                    entry.update(const=code[0][1], const_path=path, const_line=src.line(src.tokens[first][2]),
                                 const_span=(start, end), doc=[re.sub(r"^\s*", "", line) for line in doc])
    taken = set()
    for e in entries:
        if e["const"]:
            e["slug"] = kebab(re.sub(r"[Cc]ontract$", "", e["const"]))
        else:
            key = re.match(r"(?:dispatch://)?([A-Z][A-Z0-9]*-\d+)\b", HEADER.match(e["lines"][0]).group(2))
            e["slug"] = key.group(1).lower() if key else f"entry-{e['number']}"
        if not SLUG.match(e["slug"]):
            e["slug"] = f"entry-{e['number']}"
        slug, n = e["slug"], 2
        while e["slug"] in taken:
            e["slug"], n = f"{slug}-{n}", n + 1
        taken.add(e["slug"])
        if not e["const"]:
            e["const"] = lower_camel(e["slug"]) + "Contract"

    # Which entries are the branch's own: neither check below writes anything, and a convert refuses
    # what either finds. An entry whose text main holds under a number of its own is main's change,
    # which this branch carries from another branch it was stacked on and main took as a squash before
    # the cutover: the branch has to merge main as it was before the cutover first. An entry an entry
    # file of main holds is such a change main took after the cutover, after which GitHub retargets
    # the branch to main: over its merge base with main the branch would convert it again under
    # another id, as a second entry file of one change.
    on_main = {summary(e): e["number"] for e in numbered(main_version)[2]}
    stale = {e["number"]: on_main[summary(e)] for e in entries if summary(e) in on_main}
    for number, theirs in sorted(stale.items()):
        print(f"onmain {number} {theirs}")
    if stale and mode == "convert":
        die("; ".join(f"entry {n} of this branch is entry {m} of {main_name}" for n, m in sorted(stale.items())) +
            f": the branch carries a change {main_name} already holds. Merge {before_cutover(main_name)} into the "
            "branch first, then convert it")
    held = held_in_entry_files(main, [e for e in entries if e["number"] not in stale])
    for number, (path, _) in sorted(held.items()):
        print(f"onmain {number} {path}")
    if held and mode == "convert":
        squash = f"with --base naming the base branch's last commit before the squash, then merge {main_name}"
        by_text = [f"entry {n} of this branch is {path} of {main_name}"
                   for n, (path, const) in sorted(held.items()) if not const]
        # A constant the ref declares is either a base branch's entry taken as a squash, or another
        # change's that took the same name; two declarations of it do not compile either way.
        by_const = [f"entry {n} of this branch declares {const}, which {path} of {main_name} declares too"
                    for n, (path, const) in sorted(held.items()) if const]
        refusals = []
        if by_text:
            refusals.append("; ".join(by_text) + f": the branch carries the entries of a base branch that "
                            f"{main_name} took as a squash after the cutover, and would convert them again under "
                            f"other ids. Run this {squash}")
        if by_const:
            refusals.append("; ".join(by_const) + ": two declarations of one constant in package api do not "
                            f"compile. Either {main_name} took that entry from a base branch as a squash after the "
                            f"cutover: run this {squash}. Or another change took that name: rename the entry's "
                            "constant in the branch, its declaration and each line that names it, then run this again")
        die(". ".join(refusals))
    entries = [e for e in entries if e["number"] not in stale and e["number"] not in held]
    if not entries:
        print(f"{head[:10]} adds no numbered entry of its own to {VERSION_GO}: nothing to convert")
        return None
    numbers = {e["number"]: e for e in entries}
    for e in entries:
        if e["const_path"]:
            edits[e["const_path"]].replace(*e["const_span"])
    owner = {e["const"]: e for e in entries if e["const_path"]}

    # Step 3: the rows of the frozen tables that name an entry's constant.
    moved_answers = {}
    pending_lists = {}
    lists_at = {}
    for path in TABLE_FILES:
        if path not in head_src:
            continue
        lists_at.update({name: (path, decl) for name, decl in value_lists(head_src[path]).items()})
    base_lists = set()
    for path in TABLE_FILES:
        if path in base_src:
            base_lists |= set(value_lists(base_src[path]))
    new_lists = {}
    for name, (path, decl) in lists_at.items():
        named = {t[1] for t in head_src[path].code_between(decl.kw, decl.last) if t[0] == "ident"} & set(owner)
        if name not in base_lists and len(named) == 1:
            new_lists[name] = owner[named.pop()]
    moves = []
    for path in TABLE_FILES:
        if path not in head_src or path not in go_paths:
            continue
        src, ours = head_src[path], tables(head_src[path])
        theirs = tables(base_src[path]) if path in base_src else {}
        for name, table in ours.items():
            field = TABLES[name]
            base_rows = theirs[name].rows if name in theirs else []
            base_norms = {r.norm for r in base_rows}
            base_ids = {identity(name, r) for r in base_rows}
            for row in table.rows:
                if row.norm in base_norms or field is None:
                    continue
                named = row.idents & set(owner)
                values = string_field(row.norm, "Values") if field == "enumeratedFields" else None
                via = values if values in new_lists and not named else None
                if via:
                    named = {new_lists[via]["const"]}
                if len(named) != 1 or identity(name, row) in base_ids:
                    continue
                if not row.whole_lines:
                    # The last argument of append(), which its `)` closes: it moves with the comma
                    # of the row before it, where it starts a line of its own.
                    prior = [r for r in table.rows if r.comma is not None and r.end <= row.start]
                    if row.comma is not None or not prior or not src.leads_line(row.lead):
                        continue
                    start, end = src.tokens[prior[-1].comma][2], row.end
                else:
                    start, end = row.block_start, row.block_end
                if not edits[path].replace(start, end):
                    continue
                entry = owner[named.pop()]
                entry["rows"][field].append(row)
                moves.append((path, row.line, field, entry["const"]))
                entry["imports"] |= {(path, n) for n in selectors(src, row.block_start, row.end)}
                if via:
                    pending_lists.setdefault(via, entry)
                if name == "Answers":
                    moved_answers[identity(name, row)] = entry

    # The list of values the branch declared for a moved EnumeratedField row: every value of it
    # arrived at the entry, so it is the entry's, and code elsewhere in the package still reads it
    # by name.
    for name, entry in pending_lists.items():
        path, decl = lists_at[name]
        if edits[path].replace(decl.start, decl.end):
            entry["decls"].append(decl.text().rstrip("\n").split("\n"))
            entry["imports"] |= {(path, n) for n in selectors(head_src[path], decl.start, decl.end)}
            moves.append((path, head_src[path].line(head_src[path].tokens[decl.kw][2]), "declaration", entry["const"]))

    # A moved answer's AnswerID, where the branch declared it, and its place in the identity case.
    if moved_answers and ANSWERS_GO in head_src:
        src = head_src[ANSWERS_GO]
        base_text = base_src[ANSWERS_GO].text if ANSWERS_GO in base_src else ""
        for decl in src.decls():
            if decl.keyword != "const" or not decl.block:
                continue
            open_i = src.next_code(decl.kw)
            for i in range(open_i + 1, src.match[open_i]):
                word = src.tokens[i][1]
                if word in moved_answers and src.leads_line(i) and src.tokens[src.next_code(i)][1] == "AnswerID" \
                        and not re.search(r"^\s*" + word + r"\s+AnswerID\b", base_text, re.M):
                    lead = src.lead_comments(i, open_i)
                    start = src.line_start(src.tokens[lead][2])
                    end = src.line_end(src.tokens[i][2])
                    spec = src.text[start:end].rstrip("\n").split("\n")
                    if edits[ANSWERS_GO].replace(start, end):
                        moved_answers[word]["decls"].append(
                            [line.strip() for line in spec[:-1]] + ["const " + spec[-1].strip()])
                        moves.append((ANSWERS_GO, src.line(src.tokens[i][2]), "declaration",
                                      moved_answers[word]["const"]))
    if moved_answers and SHAPING_GO in head_src and SHAPING_GO in base_src:
        ours, theirs = identity_case(head_src[SHAPING_GO]), identity_case(base_src[SHAPING_GO])
        if ours and theirs:
            src = head_src[SHAPING_GO]
            ids = [src.tokens[k][1] for k in ours[2]]
            base_ids = [base_src[SHAPING_GO].tokens[k][1] for k in theirs[2]]
            leaving = [w for w in ids if w in moved_answers and w not in base_ids]
            if leaving and [w for w in ids if w not in leaving] == base_ids:
                b = base_src[SHAPING_GO]
                head_case = (src.tokens[ours[0]][2], src.tokens[ours[1]][3])
                base_case = b.text[b.tokens[theirs[0]][2]:b.tokens[theirs[1]][3]]
                if edits[SHAPING_GO].replace(*head_case, base_case):
                    for w in leaving:
                        moved_answers[w]["rows"]["identityAnswers"].append(w)
                        moves.append((SHAPING_GO, src.line(src.tokens[ours[0]][2]), "identityAnswers",
                                      moved_answers[w]["const"]))

    # Step 4: the derived copies.
    undo = []
    version = edits.setdefault(VERSION_GO, Edits(head_version))
    offsets = [0]
    for line in head_lines:
        offsets.append(offsets[-1] + len(line) + 1)
    for e in entries:
        first = e["first"] - 1 if e["first"] > 0 and head_lines[e["first"] - 1] == "//" else e["first"]
        version.replace(offsets[first], offsets[e["stop"]])
    base_const = CONST_LINE.search(base_version)
    version.replace(offsets[head_const], offsets[head_const] + len(head_lines[head_const]), base_const.group(0).rstrip())
    undo.append((VERSION_GO, f"const DaemonAPIVersion = {CONST_LINE.search(head_version).group(1)}, back to {base_number}"))
    if ANSWERS_GO in head_src and ANSWERS_GO in base_src:
        ours = [d for d in head_src[ANSWERS_GO].decls() if d.name == "SupportedContracts"]
        theirs = [d for d in base_src[ANSWERS_GO].decls() if d.name == "SupportedContracts"]
        if ours and theirs and ours[0].text() != theirs[0].text():
            edits[ANSWERS_GO].replace(ours[0].start, ours[0].end, theirs[0].text())
            undo.append((ANSWERS_GO, "the declaration of SupportedContracts, back to the base's"))
    written = {}
    for path in copies:
        if path in MANIFESTS:
            mine, match = manifest_number(texts.get(f"{head}:{path}"))
            theirs, _ = manifest_number(texts.get(f"{base}:{path}"))
            if match and theirs is not None and mine != theirs:
                text = texts[f"{head}:{path}"]
                written[path] = text[:match.start(1)] + str(theirs) + text[match.end(1):]
                undo.append((path, f"legion.daemonApiVersion {mine}, back to {theirs}"))
        elif path in STATE_GOLDENS:
            mine, theirs = texts.get(f"{head}:{path}"), texts.get(f"{base}:{path}")
            a, b = json_contract_block(mine or ""), json_contract_block(theirs or "")
            if a and b:
                ours, base_block = mine.split("\n"), theirs.split("\n")
                if ours[a[0]:a[1]] != base_block[b[0]:b[1]]:
                    written[path] = "\n".join(ours[:a[0]] + base_block[b[0]:b[1]] + ours[a[1]:])
                    undo.append((path, "the contract marks, back to the base's"))
        else:
            written[path] = None
            undo.append((path, "back to the base's"))

    # Step 6's lines, read at HEAD: what names an entry, or holds its number, outside what moves.
    # A line inside a frozen table (rows) is a row TestNoFrozenRowNamesAnEntry reads, or one the
    # entry's fixture records: no fixture of that entry can be written until it moves.
    spans = {}
    for path in TABLE_FILES:
        if path in head_src:
            src = head_src[path]
            spans[path] = [(t.start, t.end) for t in tables(src).values()]
            spans[path] += [(d.start, d.end) for d in value_lists(src).values()]
            case = identity_case(src) if path == SHAPING_GO else None
            if case:
                spans[path].append((src.tokens[case[0]][2], src.tokens[case[1]][3]))
    left, rows = {}, set()

    def leave(path, line, text, offset):
        left[(path, line)] = text
        if any(s <= offset < e for s, e in spans.get(path, [])):
            rows.add((path, line))

    names = {e["const"] for e in entries}
    for path, src in head_src.items():
        if path not in go_paths or ENTRY_FILE.search(path) or path == VERSION_GO:
            continue
        for offset, _ in src.idents_at(names):
            if not edits[path].covers(offset):
                line = src.line(offset)
                leave(path, line, src.line_text(line).strip(), offset)
    wanted = {str(n) for n in numbers}
    for path, lines in added.items():
        if path not in head_src or path == VERSION_GO or ENTRY_FILE.search(path):
            continue
        src = head_src[path]
        for number, text in lines.items():
            offset = src.starts[number - 1]
            line_tokens = [t for t in Source(path, text).tokens if t[0] == "number"]
            if any(t[1] in wanted for t in line_tokens) and not edits[path].covers(offset):
                leave(path, number, text.strip(), offset)
    for path in spans:
        if path not in go_paths:
            continue
        src = head_src[path]
        for number in added.get(path, {}):
            offset = src.starts[number - 1]
            text = src.line_text(number).strip()
            if text and not text.startswith("//") and any(s <= offset < e for s, e in spans[path]) \
                    and not edits[path].covers(offset):
                leave(path, number, text, offset)

    if mode == "report":
        for e in entries:
            where = f"{e['const_path']}:{e['const_line']}" if e["const_path"] else "-"
            print(f"entry {e['number']} {e['const']} {e['slug']} {where}")
        for path, line, field, const in sorted(moves):
            print(f"move {path}:{line} {field} {const}")
        for path, _ in undo:
            print(f"undo {path}")
        for (path, line), text in sorted(left.items()):
            print(f"{path}:{line}: {text}")
        return None

    # Convert: write steps 1 to 4 into the working tree.
    now = datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)
    existing = set(git("ls-tree", "--name-only", main, f"{API}/").decode().split("\n")) | \
        set(git("ls-tree", "--name-only", head, f"{API}/").decode().split("\n"))
    used_ids = {re.search(r"contract_(\d{14})_", p).group(1) for p in existing if ENTRY_FILE.search(p)}
    touched = []
    for e in entries:
        while now.strftime("%Y%m%d%H%M%S") in used_ids:
            now += datetime.timedelta(seconds=1)
        e["id"] = now.strftime("%Y%m%d%H%M%S")
        used_ids.add(e["id"])
        now += datetime.timedelta(seconds=1)
        e["file"] = f"{API}/contract_{e['id']}_{e['slug']}.go"
        with open(e["file"], "w") as out:
            out.write(entry_file(e, head_src))
        touched.append(e["file"])
        print(f"entry {e['number']} -> {e['file']} ({e['const']} = {e['id']})")
    for path, line, field, const in sorted(moves):
        print(f"moved {path}:{line} -> {field} of {const}")
    for path, edit in edits.items():
        if edit.spans:
            with open(path, "w") as out:
                out.write(edit.apply())
            touched.append(path)
    # Each entry's request part, read from the branch's request-shapes.json before step 4 puts the
    # file back, for step 5's `-update -entry-requests` (request_parts).
    parts = request_parts(read_blobs([f"{head}:{REQUEST_SHAPES}"])[f"{head}:{REQUEST_SHAPES}"], base_number, numbers)
    os.makedirs(os.path.dirname(requests_path), exist_ok=True)
    with open(requests_path, "w") as out:
        json.dump({e["id"]: parts[e["number"]] for e in entries}, out, indent=2)
        out.write("\n")
    for e in entries:
        print(f"request part of entry {e['number']}: {json.dumps(parts[e['number']]) if parts[e['number']] else 'none'}")
    print(f"kept the request part of each entry, read from {REQUEST_SHAPES} at its number, in {requests_path}")
    for path, text in written.items():
        if text is None:
            text = read_blobs([f"{base}:{path}"])[f"{base}:{path}"]
        with open(path, "w") as out:
            out.write(text)
        touched.append(path)
    for path, what in undo:
        print(f"undid {path}: {what}")
    state = {
        "base": base, "head": head, "main": main, "mainName": main_name, "baseNumber": base_number,
        "entries": [{k: e[k] for k in ("number", "id", "slug", "const", "file")} for e in entries],
        "moves": moves, "undo": undo, "touched": sorted(set(touched)), "requestsAt": requests_at,
        "left": [[path, line, text, (path, line) in rows] for (path, line), text in sorted(left.items())],
    }
    with open(state_path, "w") as out:
        json.dump(state, out)
    return state


def entry_file(e, head_src):
    """The entry file: the entry's text and declaration as scripts/daemon-contract-entry.sh writes
    them, and the registration with each row step 3 moved."""
    first = HEADER.match(e["lines"][0])
    text = [f"// {e['id']} {e['slug']}: {first.group(2)}".rstrip()] + e["lines"][1:]
    if e["doc"]:
        text += ["//"] + e["doc"]
    specs = set()
    for path, name in e["imports"]:
        spec = imports(head_src[path]).get(name)
        if spec:
            specs.add(spec)
    out = ["package api", ""]
    if specs:
        # The standard library's first, then the module's, as goimports groups them.
        path_of = lambda s: s.split()[-1].strip('"`')
        std = sorted((s for s in specs if "." not in path_of(s).split("/")[0]), key=path_of)
        rest = sorted((s for s in specs if "." in path_of(s).split("/")[0]), key=path_of)
        out += ["import ("] + [f"\t{s}" for s in std] + ([""] if std and rest else []) + \
            [f"\t{s}" for s in rest] + [")", ""]
    out += text + [f"const {e['const']} = {e['id']}", "", "func init() {"]
    fields = [(f, t) for f, t in FIELDS if e["rows"][f]]
    if not fields:
        out.append(f'\tregisterEntry(contractEntry{{id: {e["const"]}, slug: "{e["slug"]}"}})')
    else:
        out += ["\tregisterEntry(contractEntry{", f'\t\tid: {e["const"]}, slug: "{e["slug"]}",']
        for field, kind in fields:
            if field == "identityAnswers":
                out.append(f"\t\tidentityAnswers: []AnswerID{{{', '.join(e['rows'][field])}}},")
                continue
            out.append(f"\t\t{field}: {kind}{{")
            for row in e["rows"][field]:
                lines = row.lines()
                indent = re.match(r"\t*", lines[0]).group(0)
                multiline_raw = any(t[0] == "string" and t[1].startswith("`") and "\n" in t[1]
                                    for t in row.src.tokens[row.first:row.last + 1])
                for line in lines:
                    if not multiline_raw and line.startswith(indent):
                        line = "\t\t\t" + line[len(indent):]
                    out.append(line)
            out.append("\t\t},")
        out.append("\t})")
    out.append("}")
    for decl in e["decls"]:
        out += [""] + decl
    return "\n".join(out) + "\n"


def finish(state_path):
    """Step 6 after the merge: each left line at its line in the merged tree, then the message."""
    with open(state_path) as f:
        state = json.load(f)
    main, main_name = state["main"], state["mainName"]
    changed = [p for p in git("diff", "--name-only", "--no-renames", main, "--", "packages/daemon").decode().split("\n")
               if p.endswith(".go") and os.path.exists(p) and not ENTRY_FILE.search(p)]
    added = added_lines(main, None, changed)
    names = {e["const"] for e in state["entries"]}
    left, used = {}, set()
    for path, line, text, _ in state["left"]:
        lines = added.get(path, {})
        found = next((n for n, t in sorted(lines.items()) if t.strip() == text and (path, n) not in used), None)
        if found is None and os.path.exists(path):
            with open(path) as f:
                found = next((n for n, t in enumerate(f.read().split("\n"), 1)
                              if t.strip() == text and (path, n) not in used), None)
        found = found or line
        used.add((path, found))
        left[(path, found)] = text
    for path in changed:
        with open(path) as f:
            src = Source(path, f.read())
        for offset, _ in src.idents_at(names):
            line = src.line(offset)
            left.setdefault((path, line), src.line_text(line).strip())
    entries = state["entries"]
    subject = "chore(daemon): convert contract " + ", ".join(
        f"entry {e['number']} to entry {e['id']} {e['slug']}" for e in entries) + f", and merge {main_name}"
    body = ["scripts/daemon-contract-convert.sh moved each numbered entry this branch added to",
            f"{VERSION_GO} into an entry file (dispatch://INFRA-1569, R8), and merged {main_name}",
            f"({main}) into the branch.", ""]
    body += [f"- entry {e['number']}: {e['file']} ({e['const']})" for e in entries]
    body += [f"- moved {path}:{line} ({field}) into the entry file of {const}" for path, line, field, const in state["moves"]]
    body += [f"- undid {path}: {what}" for path, what in state["undo"]]
    body += [f"- wrote {path}" for path in state.get("fixtures", [])]
    if not state.get("fixtures") and any(item[3] for item in state["left"]):
        body.append("- wrote no entry fixture: each row of a frozen table below names an entry, whose fixture "
                    "records it; `go test ./internal/api -run '^TestEntryFixtures$' -update -entry-requests=<file>` "
                    "writes it once they move, <file> being the request part of each entry that the conversion "
                    f"kept, `git rev-parse --git-path {state['requestsAt']}`")
    if left:
        body += ["", "Left for the author, each at its line in this commit: TestNoFrozenRowNamesAnEntry",
                 "fails until each row of a frozen table that names an entry is in its entry file.", ""]
        body += [f"{path}:{line}: {text}" for (path, line), text in sorted(left.items())]
    with open(state_path + ".message", "w") as out:
        out.write(subject + "\n\n" + "\n".join(body) + "\n")
    with open(state_path + ".left", "w") as out:
        out.write("".join(f"{path}:{line}: {text}\n" for (path, line), text in sorted(left.items())))
    # How many of the lines are rows of a frozen table, which no entry fixture can be written past.
    with open(state_path + ".rows", "w") as out:
        out.write(f"{sum(1 for item in state['left'] if item[3])}\n")


phase = sys.argv[1]
if phase == "analyze":
    analyze(*sys.argv[2:9])
elif phase == "fixtures":
    with open(sys.argv[2]) as f:
        state = json.load(f)
    state["fixtures"] = [p for p in sys.argv[3:] if p]
    with open(sys.argv[2], "w") as f:
        json.dump(state, f)
elif phase == "finish":
    finish(sys.argv[2])
else:
    die(f"no phase {phase}")
PY
}

if [ "$report" = 1 ]; then
  convert_py analyze report "$head_sha" "$onto_sha" "$onto_name" ""
  exit 0
fi

branch=$(git symbolic-ref -q --short HEAD) || die "HEAD is detached: check out the branch to convert"
git diff --quiet && git diff --cached --quiet || die "the working tree has changes: commit or stash them first"
command -v go >/dev/null || die "go is not on PATH: step 5 runs go test ./internal/api -update"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
state=$work/state.json
# Each entry's request part (step 4), kept in the git directory: the `-update` an author runs after
# moving a row or merging main by hand reads it too, after this run's own files are gone.
requests_at=daemon-contract-convert/$branch/entry-requests.json
requests=$(git rev-parse --path-format=absolute --git-path "$requests_at")

echo "== steps 1 to 4: $branch at ${head_sha:0:10}"
convert_py analyze convert "$head_sha" "$onto_sha" "$onto_name" "$state" "$requests" "$requests_at"
[ -f "$state" ] || exit 0
mapfile -t touched < <(python3 -c 'import json, sys; print("\n".join(json.load(open(sys.argv[1]))["touched"]))' "$state")
mapfile -t gofiles < <(printf '%s\n' "${touched[@]}" | grep '\.go$' || true)
if [ "${#gofiles[@]}" -gt 0 ]; then
  gofmt -w "${gofiles[@]}" || die "gofmt refused the converted Go files; the working tree holds them, unstaged"
fi
git add -A -- "${touched[@]}"
converted=$(git commit-tree "$(git write-tree)" -p "$head_sha" \
  -m "chore(daemon): convert the numbered contract entries of $branch to entry files")

echo "== step 5: merge $onto_name (${onto_sha:0:10})"
if ! merged=$(git merge-tree --write-tree --no-messages --name-only "$converted" "$onto_sha"); then
  git update-ref -m "daemon-contract-convert: conversion" "refs/heads/$branch" "$converted" "$head_sha"
  git reset -q
  echo "the merge of $onto_name conflicts in:" >&2
  sed 1d <<<"$merged" | sort -u | sed 's/^/  /' >&2
  die "the conversion is committed as ${converted:0:10}; merge $onto (${onto_sha:0:10}) by hand, then run go test ./internal/api -run '$UPDATE_RUN' -update -entry-requests=$requests in packages/daemon"
fi
tree=$(head -1 <<<"$merged")
git read-tree -m -u "$converted" "$tree"
update_ok=1
if ! (cd packages/daemon && go test -count=1 ./internal/api/ -run "$UPDATE_RUN" -update -entry-requests="$requests" >"$work/update.txt" 2>&1); then
  update_ok=0
fi
mapfile -t fixtures < <(git ls-files --others --exclude-standard -- packages/contracts/fixtures/daemon-api/contracts)
git add -u
[ "${#fixtures[@]}" -eq 0 ] || git add -- "${fixtures[@]}"
convert_py fixtures "$state" "${fixtures[@]}"
convert_py finish "$state"
final=$(git commit-tree "$(git write-tree)" -p "$head_sha" -p "$onto_sha" -F "$state.message")
git update-ref -m "daemon-contract-convert: conversion and merge of $onto_name" "refs/heads/$branch" "$final" "$head_sha"
git reset -q
for f in "${fixtures[@]}"; do echo "wrote $f"; done
echo "committed ${final:0:10}: $(head -1 "$state.message")"
if [ -s "$state.left" ]; then
  echo "== step 6: lines left for the author; TestNoFrozenRowNamesAnEntry fails until each row of a frozen table here is in its entry file"
  cat "$state.left"
fi
if [ "$update_ok" = 0 ]; then
  grep -E '^(---|FAIL|ok|panic)|_test.go:[0-9]+:' "$work/update.txt" | head -40 >&2 || true
  rows=$(cat "$state.rows")
  if [ "$rows" -gt 0 ]; then
    # A row of a frozen table that names an entry is the entry's, so the fixture written now would
    # be wrong: TestEntryFixtures refuses to write it, and the author writes it once the rows move.
    echo "== no entry fixture yet: $rows row(s) of a frozen table above name an entry, and the entry's fixture records them;"
    echo "   move each into its entry file, then run go test ./internal/api -run '$UPDATE_RUN' -update -entry-requests=$requests in packages/daemon"
    exit 0
  fi
  die "go test ./internal/api -run '$UPDATE_RUN' -update -entry-requests=$requests failed after the merge, so an entry may have no fixture yet; the merge is committed"
fi
