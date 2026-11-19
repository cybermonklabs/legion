"""The controls of the character-count check: planted files through the same code as the scan,
each with the verdicts it must get. They run before every scan."""
from __future__ import annotations

from character_counts_analysis import OTHER_COUNTERS, scan
from character_counts_tree import BINARY, DATED, LEFT_OUT, LINK, MISSING, SELF, judge_tree

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
    ("a message naming its unit stays quiet", "packages/envoy-client/src/unit.ts",
     "export const line = (n: number) => `One block: ${n} characters (UTF-16 units).`;\n", ["names its unit"]),
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
    }
    for marker, want_verdict in readings.items():
        got = [hit.verdict for hit in scan(path, EXEMPT_SITE.replace("{marker}", marker))]
        if len(got) != 1 or not got[0].startswith(want_verdict):
            return f"{name}: with {marker.strip() or 'no comment'!r} answered {got}, want {want_verdict}"
    return None


def tree_controls() -> list[str]:
    """The files the check reads and leaves out, and UNKNOWN over an empty listing, through the
    same code as the scan."""
    problems = []
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
