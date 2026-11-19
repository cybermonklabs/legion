"""The controls of the character-count check: planted files through the same code as the scan,
each with the verdicts it must get. They run before every scan."""
from __future__ import annotations

from character_counts_analysis import OTHER_COUNTERS, scan
from character_counts_tree import BINARY, DATED, FIXTURE, LEFT_OUT, LINK, MISSING, SELF, classify, judge_tree

EXEMPT_SITE = (
    "package planted\n\n"
    "func r(n int) error {\n"
    "{marker}"
    '\treturn fmt.Errorf("cut %d characters", n)\n'
    "}\n"
)
CONTROLS = [
    ("a planted UTF-16 count in packages/daemon fires", "packages/daemon/internal/planted/planted.go",
     'package planted\n\nfunc f(name string) string {\n'
     '\treturn fmt.Sprintf("name is %d characters", itemblocks.Units16(name))\n}\n', ["instance"]),
    ("a planted .length count in packages/envoy-client fires", "packages/envoy-client/src/planted.ts",
     "export const cut = (text: string) => `cut at ${text.length} characters`;\n", ["instance"]),
    ("an ASCII-only grammar in the count's sentence stays quiet", "packages/envoy/internal/planted/ascii.go",
     'package planted\n\nvar e = errors.New("commit is the 40-character lowercase hex commit the check ran at")\n',
     ["ASCII grammar"]),
    ("an ASCII-only grammar in another sentence passes nothing", "packages/envoy/internal/planted/hex.go",
     'package planted\n\nfunc h(title string) error {\n'
     '\treturn fmt.Errorf("title is at most %d characters; the commit beside it is 40 hex", itemblocks.Units16(title))\n}\n',
     ["instance"]),
    ("a message naming the unit its counter counts stays quiet", "packages/envoy-client/src/unit.ts",
     "export const line = (text: string) => `One block: ${text.length} characters (UTF-16 units).`;\n", ["names its unit"]),
    ("a unit a message names fires where its counter counts another", "packages/envoy/internal/planted/unit.go",
     'package planted\n\nfunc u(s string) error {\n\treturn fmt.Errorf("title is %d characters (UTF-16 code units)", len(s))\n}\n',
     ["instance | it names UTF-16 code units, and len() counts bytes"]),
    ("a unit a message names over a value the check cannot follow fires", "packages/envoy-client/src/unnamed.ts",
     "export const line = (n: number) => `One block: ${n} characters (UTF-16 units).`;\n", ["instance | cannot judge"]),
    ("a bound its if counts in code points stays quiet", "packages/envoy/internal/planted/runes.go",
     'package planted\n\nfunc g(id string) string {\n\tif n := utf8.RuneCountInString(id); n > 200 {\n'
     '\t\treturn fmt.Sprintf("an id is at most 200 characters, and this one is %d", n)\n\t}\n\treturn ""\n}\n',
     ["code points"]),
    ("a bound its if counts in bytes fires, though the call counts code points", "packages/envoy/internal/planted/header.go",
     'package planted\n\nfunc t(title string) error {\n\tif len(title) > 200 {\n'
     '\t\treturn fmt.Errorf("a title is at most 200 characters, and this one is %d", utf8.RuneCountInString(title))\n'
     '\t}\n\treturn nil\n}\n',
     ["instance"]),
    ("each count is judged by its own value, and one behind a helper the check cannot follow fires",
     "packages/envoy/internal/planted/helper.go",
     'package planted\n\nfunc refusal(title, body string) string {\n\ttitleSize := measured(title)\n'
     '\tbodySize := utf8.RuneCountInString(body)\n'
     '\treturn fmt.Sprintf("title is %d characters; body is %d characters", titleSize, bodySize)\n}\n'
     'func measured(value string) int {\n\treturn len(value)\n}\n',
     ["instance", "code points"]),
    ("a count the call itself takes in bytes fires", "packages/envoy/internal/planted/direct.go",
     'package planted\n\nfunc d(title, body string) string {\n'
     '\treturn fmt.Sprintf("title is %d characters; body is %d characters", len(title), utf8.RuneCountInString(body))\n}\n',
     ["instance", "code points"]),
    ("a %q before the count takes its own argument, so the count is judged by the next",
     "packages/envoy/internal/planted/quoted.go",
     'package planted\n\nfunc q(name string) string {\n'
     '\treturn fmt.Sprintf("%q holds %d characters", utf8.RuneCountInString(name), len(name))\n}\n',
     ["instance"]),
    ("a double-quoted shell word holding \"$(cmd \"x\")\" ends at its own quote",
     "packages/envoy/scripts/planted-substitution.sh",
     "#!/usr/bin/env bash\ncheck \"names it\" \"$(contains \"$out\" \"the gate's refusal\")\"\n"
     "echo \"name: $(printf '%s' \"$1\" | wc -c) characters\"\n# keep a 40-character note\n",
     ["instance | wc -c counts bytes"]),
    ("a count beside a word chosen in a template's expression fires", "packages/envoy-client/src/plural.ts",
     'export const left = (n: number) => `${n} ${n === 1 ? "character" : "characters"} left`;\n', ["instance"]),
    ("a single-quoted TypeScript string is read", "packages/envoy-client/src/single.ts",
     "export const said = (text: string) => 'cut at ' + text.length + ' characters';\n",
     ["instance | .length counts"]),
    ("in a, b := x, y each name takes its own value", "packages/envoy/internal/planted/pair.go",
     'package planted\n\nfunc p(x, y string) string {\n\tsent, held := utf8.RuneCountInString(x), len(y)\n'
     '\treturn fmt.Sprintf("sent %d characters, held %d characters", sent, held)\n}\n',
     ["code points", "instance"]),
    ("a %s count from a format verb is followed to its len()", "packages/envoy/internal/planted/verb.go",
     'package planted\n\nfunc v(name string) string {\n'
     '\treturn fmt.Sprintf("%s characters", strconv.Itoa(len(name)))\n}\n', ["instance"]),
    ("a name a Go for header assigns takes the header's value, not its block's",
     "packages/envoy/internal/planted/loop.go",
     'package planted\n\nfunc l(t *testing.T, s string) {\n'
     '\tfor n := range utf8.RuneCountInString(s) + 1 {\n'
     '\t\tt.Logf("the first %d characters of %d bytes", n, len(s))\n\t}\n'
     '\tconst places = 20\n\tfor place := range places {\n'
     '\t\tt.Errorf("%q at character %d", s[:len(s)/2], place)\n\t}\n}\n',
     ["code points", "instance | cannot judge"]),
    ("a comment is no hit", "packages/envoy-client/src/comment.ts",
     "// a 40-character name, in a comment\nconst re = /[`]/g;\n", []),
    ("a Python comment is no hit", "scripts/planted/comment.py",
     '# docs example: amount + " characters"\nprint("done")\n', []),
    ("a message holding a URL is read whole", "packages/envoy-client/src/url.ts",
     'export const said = (text: string) => "https://help/name: " + text.length + " characters";\n', ["instance"]),
    ("a count beside a word chosen in an expression, or joined to it with +, fires",
     "packages/dispatch/web/src/planted/Counter.tsx",
     "const left = MAX - note.length;\n"
     'const view = <p>\n  {left} {left === 1 ? "character" : "characters"} left\n</p>;\n'
     'const said = "cut at " + note.length + " characters";\n',
     ["instance", "instance"]),
    ("a count and the word in literals a + joins across lines fire, in TypeScript", "packages/contracts/src/planted-split.ts",
     "const LIMIT = 32000;\nexport const RULE =\n  `Each block holds at most ${LIMIT} ` +\n"
     "  `characters; a longer one is refused.`;\n", ["instance"]),
    ("a count and the word in literals a + joins across lines fire, in Go", "packages/envoy/internal/planted/split.go",
     'package planted\n\nfunc s(body string) error {\n\treturn fmt.Errorf("each block holds at most %d "+\n'
     '\t\t"characters", len(body))\n}\n', ["instance"]),
    ("a shell here-document's text is read, and an apostrophe in it closes nothing", "packages/envoy/scripts/planted-usage.sh",
     "#!/usr/bin/env bash\nusage() {\n  cat >&2 <<'USAGE'\nusage: planted-usage.sh <title>   the gate's title, at most 200 characters\n"
     "  a longer title is cut at character 200\nUSAGE\n}\nn=$(printf '%s' \"${1:-}\" | wc -c)\necho \"the title is $n characters\"\n",
     ["instance | cannot judge: a bound", "instance | cannot judge: a bound", "instance | wc -c counts bytes"]),
    ("a shell count in a UTF-8 locale the script sets itself is code points", "packages/envoy/scripts/planted-locale.sh",
     "#!/usr/bin/env bash\nexport LC_ALL=C.UTF-8\nname=$1\nprintf 'name: %d characters\\n' \"${#name}\"\n",
     ["code points"]),
    ("a shell count in the caller's locale fires", "packages/envoy/scripts/planted-nolocale.sh",
     "#!/usr/bin/env bash\nname=$1\nprintf 'name: %d characters\\n' \"${#name}\"\n", ["instance"]),
    ("a count in prose fires, and one naming its unit stays quiet", "packages/pi-envoy/extensions/planted/rules.md",
     "A rewritten question is at most\n2,000 characters, Dispatch's limit.\n\n"
     "A block is at most 32,000 characters (UTF-16 code units).\n", ["instance", "names its unit"]),
    ("a constant's name as the count fires in a literal; an acronym before the word is no count",
     "packages/dispatch/web/src/planted/Named.test.tsx",
     'test("the filter writes at most SEARCH_QUERY_MAX characters into the URL", () => {});\n'
     'test("HTML characters and UTF8 characters are no count", () => {});\n', ["instance | cannot judge: a bound"]),
    ("a constant's name in backticks as the count fires in prose", "packages/dispatch/planted/NAMED.md",
     "The answer is cut to `ANSWER_PREVIEW_CHARS` characters at a word.\n", ["instance"]),
    ("a count in a workflow's YAML fires", ".github/workflows/planted.yaml",
     "jobs:\n  a:\n    steps:\n      - run: echo \"title is $(printf '%s' \"$T\" | wc -c) characters\"\n", ["instance"]),
    ("chars is the word", "packages/pi-legion/scripts/planted/score.ts",
     "export const note = (q: string) => `chars=${q.length}`;\n", ["instance"]),
    ("the word in capitals is the word", "packages/envoy/internal/planted/upper.go",
     'package planted\n\nfunc u(t string) string {\n\treturn fmt.Sprintf("TITLE: %d CHARACTERS", len(t))\n}\n',
     ["instance | len() counts"]),
    ("the word written with an escape is the word in Go", "packages/envoy/internal/planted/escape.go",
     'package planted\n\nfunc e(s string) error {\n\treturn fmt.Errorf("title is %d \\u0063haracters", len(s))\n}\n',
     ["instance | len() counts"]),
    ("the word written with an escape is the word in JSON", "packages/contracts/planted/escape.json",
     '{"rule": "a title is at most 2000 \\u0063haracters"}\n', ["instance"]),
    ("a word between the count and the word names the count's unit", "packages/envoy/internal/planted/qualifier.go",
     'package planted\n\nfunc q(s string) error {\n\treturn fmt.Errorf("at most %d Unicode characters", len(s))\n}\n',
     ["instance | it names code points, and len() counts bytes"]),
    ("a count in words or after a hyphen fires, and at least one is any unit", "docs/planted/words.md",
     "A query needs at least two characters.\n\nIt refuses over-40-character labels.\n\n"
     "A word of five or more characters.\n\nA name holds at least one character.\n",
     ["instance", "instance", "instance", "not empty"]),
    ("a range, a count in code or in bold and a k suffix fire in prose", "docs/planted/forms.md",
     "A label is 1-40 characters.\n\nAt most `2000` characters.\n\nAt most **2,000** characters.\n\nAt most 32k characters.\n",
     ["instance", "instance", "instance", "instance"]),
    ("a count after the word fires: a limit of it, a count used, a length in the word",
     "packages/envoy/internal/planted/after.go",
     'package planted\n\nfunc a(s string) error {\n\treturn fmt.Errorf("over the character limit (%d)", len(s))\n}\n'
     'func b(s string) string {\n\treturn fmt.Sprintf("characters used: %d of 200", len(s))\n}\n'
     'func c(s string) error {\n\treturn fmt.Errorf("its length in characters is %d", len(s))\n}\n',
     ["instance | len() counts", "instance | len() counts", "instance | len() counts"]),
    ("a Go print call's separate arguments are one message", "packages/envoy/internal/planted/println.go",
     'package planted\n\nfunc p(s string) string {\n\tfmt.Println("title is", len(s), "characters")\n'
     '\treturn fmt.Sprint(len(s), " characters")\n}\n', ["instance | len() counts", "instance | len() counts"]),
    ("a TypeScript print call's arguments and an array's join are one message", "packages/envoy-client/src/joined.ts",
     'export const f = (s: string) => console.log("title is", s.length, "characters");\n'
     'export const g = (s: string) => [s.length, "characters"].join(" ");\n',
     ["instance | .length counts", "instance | .length counts"]),
    ("a Python print call's separate arguments are one message", "scripts/planted/print.py",
     'import sys\ns = sys.argv[1].encode()\nprint("title is", len(s), "characters")\n', ["instance | cannot judge: Python's len()"]),
    ("a %s whose argument is the word, or a name holding it, reads as the word", "packages/envoy/internal/planted/wordarg.go",
     'package planted\n\nfunc w(t string) string {\n\treturn fmt.Sprintf("%d %s", len(t), "characters")\n}\n'
     'func v(t string) string {\n\tword := "characters"\n\treturn fmt.Sprintf("the title is %d %s", len(t), word)\n}\n',
     ["instance | len() counts", "instance | len() counts"]),
    ("Python's format fields and % mapping keys are counts", "scripts/planted/format.py",
     'import sys\nb = sys.stdin.buffer.read()\nprint("{} characters".format(len(b)))\n'
     'print("{n} characters".format(n=len(b)))\nprint("%(n)d characters" % {"n": len(b)})\n',
     ["instance | cannot judge: Python's len()"] * 3),
    ("a Python f-string count is its value, and Python's len() is no code-point count", "scripts/planted/fstring.py",
     'import sys\nb = sys.stdin.buffer.read()\nprint(f"{len(b)} characters")\n', ["instance | cannot judge: Python's len()"]),
    ("an LC_ALL that is not UTF-8 sets no UTF-8 locale", "packages/envoy/scripts/planted-c.sh",
     "#!/usr/bin/env bash\nexport LC_ALL=C\nname=$1\nprintf 'name: %d characters\\n' \"${#name}\"\n", ["instance"]),
    ("a UTF-8 locale set after the count, or for one command, is not the count's",
     "packages/envoy/scripts/planted-after.sh",
     "#!/usr/bin/env bash\nLC_ALL=C.UTF-8 sort -o \"$2\" \"$2\"\nname=$1\nprintf 'name: %d characters\\n' \"${#name}\"\n"
     "export LC_ALL=C.UTF-8\n", ["instance"]),
    ("a grammar word in another clause of the sentence passes nothing", "packages/envoy/internal/planted/clause.go",
     'package planted\n\nfunc c(id, s string) error {\n\tif s == "" {\n\t\treturn fmt.Errorf("issue %s, a uuid, has a body of %d characters", id, len(s))\n\t}\n'
     '\treturn fmt.Errorf("ask %s (a hex id) holds %d characters", id, len(s))\n}\n',
     ["instance | len() counts", "instance | len() counts"]),
    ("unquoted shell words are read by the one grammar", "packages/envoy/scripts/planted-words.sh",
     '#!/usr/bin/env bash\nn=$(printf %s "$1" | wc -c)\necho chars=$n\necho "$n" characters\necho "the title is $n" "characters"\n',
     ["instance | wc -c counts", "instance | wc -c counts", "instance | wc -c counts"]),
    ("JSX text is read by the one grammar", "packages/dispatch/web/src/planted/Text.tsx",
     "export const C = ({ s }: { s: string }) => <p>chars={s.length}</p>;\n"
     "export const A = ({ s }: { s: string }) => <p>at character {s.length}</p>;\n",
     ["instance | .length counts", "instance | .length counts"]),
    ("a cap named in the word is judged by its counter", "packages/envoy-client/src/cap.ts",
     "const MAX = 8000;\nexport const f = (t: string) => {\n  if (t.length > MAX) {\n    return 'over the character cap';\n  }\n  return '';\n};\n",
     ["instance | .length counts"]),
    ("a cap named in the word in prose fires", "docs/planted/cap.md",
     "A margin is bounded in bytes and not by that character limit.\n\nIt names the characters left for it.\n",
     ["instance", "instance"]),
    ("an exemption covers the message under it and no other", "packages/envoy/internal/planted/window.go",
     'package planted\n\nfunc e(id, name string) error {\n'
     '\t// characters: code points, validSessionID counts the id with utf8.RuneCountInString.\n'
     '\tif !validSessionID(id) { return fmt.Errorf("an id is at most 64 characters") }\n'
     '\treturn fmt.Errorf("a name is at most 40 characters, and this one is %d", name)\n}\n',
     ["exempt, code points: validSessionID counts the id with utf8.RuneCountInString.", "instance | cannot judge"]),
    ("an ASCII-alone exemption passes a counter the check followed, a code-points one does not",
     "packages/envoy/internal/planted/ascii_exempt.go",
     'package planted\n\nfunc a(id, name string) (error, error) {\n\t// characters: ASCII alone, an id is lowercase hex.\n'
     '\tx := fmt.Errorf("id is %d characters", len(id))\n\t// characters: code points, the name is counted in runes.\n'
     '\ty := fmt.Errorf("name is %d characters", len(name))\n\treturn x, y\n}\n',
     ["exempt, ASCII alone: an id is lowercase hex.", "instance | len() counts"]),
    ("a count a branch assigns again is judged by each value", "packages/envoy/internal/planted/branch.go",
     'package planted\n\nfunc b(s string, raw bool) error {\n\tn := utf8.RuneCountInString(s)\n\tif raw {\n\t\tn = len(s)\n\t}\n'
     '\treturn fmt.Errorf("the text is %d characters", n)\n}\n', ["instance | len() counts"]),
    ("a Go template's value fires", "packages/envoy/internal/planted/tmpl.go",
     'package planted\n\nvar T = template.Must(template.New("t").Parse("the title holds {{.N}} characters"))\n',
     ["instance | cannot judge: a template"]),
    ("a plural helper's count and a word held in a name are read", "packages/envoy-client/src/plurals.ts",
     'const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;\n'
     'export const f = (s: string) => `${plural(s.length, "character")} left`;\n'
     'export const g = (t: string) => {\n  const word = t.length === 1 ? "character" : "characters";\n  return `${t.length} ${word} left`;\n};\n',
     ["instance | .length counts", "instance | .length counts"]),
    ("an exempt site stays quiet, and fires once its comment or its reason is gone", None, None, None),
]


# One planted value per entry of OTHER_COUNTERS: a code-point counter and that entry together.
COUNTER_CASES = [
    ("go", "len()", "len(name)"),
    ("go", "Units16", "itemblocks.Units16(name)"),
    ("go", "utf16.", "utf16.RuneLen(rune(name[0]))"),
    ("ts", ".length", "name.length"),
    ("ts", "byteLength", "Buffer.byteLength(name)"),
    ("sh", "wc -c", "$(printf '%s' \"$name\" | wc -c)"),
    ("ts", "TextEncoder", "new TextEncoder().encode(name).length"),
]


def counter_controls() -> list[tuple[str, str | None]]:
    """Each case of COUNTER_CASES must fire naming its own counter, and each entry of
    OTHER_COUNTERS must have a case: the reason a hit fails names the counter, so an entry taken
    off the list fails its case even where the check would fail the hit as one it cannot judge."""
    out = []
    for lang, label, sample in COUNTER_CASES:
        if lang == "go":
            path = "packages/envoy/internal/planted/mixed.go"
            content = ('package planted\n\nfunc m(name string) string {\n'
                       f'\treturn fmt.Sprintf("%d characters", utf8.RuneCountInString(name)+{sample})\n}}\n')
        elif lang == "ts":
            path = "packages/envoy-client/src/mixed.ts"
            content = f"export const m = (name: string) => `${{[...name].length + {sample}}} characters`;\n"
        else:
            path = "packages/envoy/scripts/planted-mixed.sh"
            content = ("#!/usr/bin/env bash\nexport LC_ALL=C.UTF-8\nname=$1\n"
                       f"n=$(( ${{#name}} + {sample} ))\necho \"$n characters\"\n")
        name = f"a value holding a code-point counter and {label} fires, naming {label}"
        got = [(hit.verdict, hit.why) for hit in scan(path, content)]
        ok = len(got) == 1 and got[0][0] == "instance" and got[0][1].startswith(f"{label} counts")
        out.append((name, None if ok else f"{name}: answered {got}"))
    cased = {(lang, label) for lang, label, _ in COUNTER_CASES}
    for lang, entries in OTHER_COUNTERS.items():
        for label, _, _ in entries:
            if (lang, label) not in cased:
                out.append((f"{lang} counter {label} has a case", f"{lang} counter {label} has no case in COUNTER_CASES"))
    return out


def control(name, path, content, want) -> str | None:
    """A planted file's hits against want: each item a verdict, or `<verdict> | <start of its why>`."""
    if path is not None:
        got = [(hit.verdict, hit.why) for hit in scan(path, content)]
        wanted = [(item.split(" | ", 1) + [""])[:2] for item in want]
        ok = len(got) == len(wanted) and all(v == w[0] and why.startswith(w[1]) for (v, why), w in zip(got, wanted))
        return None if ok else f"{name}: answered {got}, want {want}"
    path = "packages/envoy/internal/planted/exempt.go"
    readings = {
        "\t// characters: code points, the caller counts n with utf8.RuneCountInString\n": "exempt, code points",
        "": "instance",
        "\t// characters: code points\n": "instance",
        # A reason is at least 10 characters: 9 pass nothing, 10 pass.
        "\t// characters: code points, too short\n": "instance",
        "\t// characters: code points, is counted\n": "exempt, code points",
    }
    for marker, want_verdict in readings.items():
        got = [hit.verdict for hit in scan(path, EXEMPT_SITE.replace("{marker}", marker))]
        if len(got) != 1 or not got[0].startswith(want_verdict):
            return f"{name}: with {marker.strip() or 'no comment'!r} answered {got}, want {want_verdict}"
    return None


# One planted path per rule of LEFT_OUT, written out apart from it, with the class it is left out
# as, and paths the check must read: a rule added to LEFT_OUT has no case here, and one that
# reaches a path it must read leaves that path out, so either fails.
LEFT_OUT_CASES = [
    (SELF, LEFT_OUT[0][1]),
    ("packages/envoy/CHANGELOG.md", DATED[0]),
    ("docs/plans/planted.md", DATED[0]),
    ("docs/superpowers/planted.md", DATED[0]),
    ("docs/research/planted.md", DATED[0]),
    ("docs/solutions/planted.md", DATED[0]),
    ("packages/daemon/cmd/legion/testdata/gh-help/planted.txt", FIXTURE[0]),
    ("packages/daemon/cmd/legion/testdata/gh-help-walk/planted.txt", FIXTURE[0]),
    ("scripts/planted.cast", FIXTURE[0]),
    ("packages/envoy/internal/dispatch/pmdoc/heavybodies/testdata/planted.md", FIXTURE[0]),
    ("scripts/approval-eval/fixtures/planted.md", FIXTURE[0]),
]
MUST_READ = [
    "docs/site/src/content/docs/dispatch/planted.md", "docs/planted.md", "README.md", "packages/envoy/AGENTS.md",
    "skills/planted/SKILL.md", "packages/daemon/cmd/legion/testdata/planted.txt", "packages/envoy/internal/dispatch/pmdoc/planted.md",
    "scripts/planted.sh", ".github/scripts/character_counts_analysis.py", ".github/scripts/check-character-counts.sh",
]


def left_out_controls() -> list[str]:
    """Each rule of LEFT_OUT has a case, each case is left out as its class, and each path of
    MUST_READ is read."""
    problems = []
    for rule, kind, _ in LEFT_OUT:
        if not any(rule.search(path) for path, _ in LEFT_OUT_CASES):
            problems.append(f"the left-out rule {rule.pattern} has no case in LEFT_OUT_CASES")
    for path, kind in LEFT_OUT_CASES:
        found = classify(path, b"text")
        if found is None or found[0] != kind:
            problems.append(f"{path} is left out as {kind!r}: answered {found}")
    for path in MUST_READ:
        found = classify(path, b"text")
        if found is not None:
            problems.append(f"{path} is read: answered left out, {found}")
    return problems


def tree_controls() -> list[str]:
    """The files the check reads and leaves out, and UNKNOWN over an empty listing, through the
    same code as the scan."""
    problems = left_out_controls()
    planted = {
        ".github/workflows/planted.yaml": b"- run: echo \"$(printf '%s' \"$T\" | wc -c) characters\"\n",
        "packages/envoy/internal/planted/planted_test.go": b'package planted\n\nvar _ = fmt.Sprintf("%d characters", len(s))\n',
        "packages/envoy/internal/planted/planted_generated.go": b'package planted\n\nvar Rule = "at most 32000 characters"\n',
        SELF: b'echo "%d characters"\n',
        "packages/envoy/CHANGELOG.md": b"- A title is at most 1,000 characters.\n",
        "docs/solutions/planted.md": b"a 32,000-character body\n",
        "docs/planted.png": b"\x89PNG\r\n\x1a\n\0\0 2000 characters",
        "packages/claude-envoy/skills/planted": "link",
        "gone.md": None,
    }
    code, report, hits = judge_tree(list(planted), planted.get)
    fired = {hit.path for hit in hits if hit.verdict == "instance"}
    want = {".github/workflows/planted.yaml", "packages/envoy/internal/planted/planted_test.go",
            "packages/envoy/internal/planted/planted_generated.go"}
    if fired != want or code != 1:
        problems.append(f"a count in a workflow, a test and a generated file fires: fired in {sorted(fired)}, exit {code}")
    for kind, count in ((LEFT_OUT[0][1], 1), (DATED[0], 2), (BINARY, 1), (LINK, 1), (MISSING, 1)):
        if not any(line.startswith(f"  left out, {kind}: {count}") for line in report):
            problems.append(f"the report counts {count} file(s) left out as {kind!r}: {report}")
    code, report, _ = judge_tree([], lambda path: None)
    if code != 1 or not any(line.startswith("UNKNOWN:") for line in report):
        problems.append(f"an empty listing prints UNKNOWN and fails: exit {code}, {report}")
    return problems
