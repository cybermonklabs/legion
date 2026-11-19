"""The planted shapes of the character-count check's controls: every shape of the fault that the
gates of rounds 1 to 3 of its pull request planted, each as (where it came from, path, content, the
lines that hold its word). Each named line of SHAPES holds a count beside the word, so the net must
find a site there. Each named line of OUTSIDE holds the word with no count on its line (the count
is on another line, or the text names a cap in words), which the net does not see: the check's
header names this class under what it cannot see."""

SHAPES = [('gates: control fire: Go %d characters with len()',
  'packages/envoy/internal/p/a.go',
  'package p\n\nfunc f(s, id string) error {\n\treturn fmt.Errorf("title is %d characters", len(s))\n}\n',
  [4]),
 ('gates: control quiet: Go RuneCount',
  'packages/envoy/internal/p/q.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("title is %d characters", utf8.RuneCountInString(s))\n'
  '}\n',
  [4]),
 ('gates: control fire: prose 2,000 characters', 'docs/a.md', 'A title is at most 2,000 characters.\n', [1]),
 ('gates: B2 prose two characters | plant3: review: prose at least two characters',
  'docs/zz/b2a.md',
  'A query needs at least two characters and can be up to 1,000.\n',
  [1]),
 ('gates: B2 prose over-40-character | plant3: review: prose over-40-character labels',
  'docs/zz/b2b.md',
  'It returns 400 LABELS_INPUT for blank or over-40-character labels.\n',
  [1]),
 ('gates: B2 prose under-two-character | plant3: review: prose under-two-character query',
  'docs/zz/b2c.md',
  'An under-two-character query returns 400 INVALID_QUERY.\n',
  [1]),
 ('gates: B2 prose five or more characters | plant3: review: prose five or more characters',
  'docs/zz/b2d.md',
  'A word of five or more characters can be one edit away.\n',
  [1]),
 ('gates: B2 TS test name one-character',
  'packages/contracts/src/zz.test.ts',
  'test("rejects a one-character query", () => { expect(schema.safeParse({query: "a"}).success).toBe(false); '
  '});\n',
  [1]),
 ('gates: N2 prose 32k characters', 'docs/zz/n2a.md', 'A body holds at most 32k characters.\n', [1]),
 ('gates: N2 Go character limit of 200',
  'packages/envoy/internal/p/b.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("the character limit of 200 is passed: %d", len(s))\n'
  '}\n',
  [4]),
 ('gates: N2 Go characters used: %d of 200',
  'packages/envoy/internal/p/c.go',
  'package p\n\nfunc f(s, id string) error {\n\treturn fmt.Errorf("characters used: %d of 200", len(s))\n}\n',
  [4]),
 ('gates: N2 Go Println separate args',
  'packages/envoy/internal/p/d.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\tfmt.Println("the title holds", len(s), "characters"); return nil\n'
  '}\n',
  [4]),
 ('gates: N2 TS console.error separate args',
  'packages/envoy-client/src/zz-a.ts',
  'export const f = (t: string) => console.error("the title holds", t.length, "characters");\n',
  [1]),
 ('gates: N2 TS word in a variable | plant3: review: TS plural word held in a variable',
  'packages/envoy-client/src/zz-b.ts',
  'export const left = (t: string) => {\n'
  '  const word = t.length === 1 ? "character" : "characters";\n'
  '  return `${t.length} ${word} left`;\n'
  '};\n',
  [2]),
 ('gates: N2 TS plural helper',
  'packages/envoy-client/src/zz-c.ts',
  'const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;\n'
  'export const said = (t: string) => `the title holds ${plural(t.length, "character")}`;\n',
  [2]),
 ('gates: N2 Go CHARACTERS capitals',
  'packages/envoy/internal/p/e.go',
  'package p\n\nfunc f(s, id string) error {\n\treturn fmt.Errorf("TITLE: %d CHARACTERS", len(s))\n}\n',
  [4]),
 ('gates: N2 Go template {{.N}} characters',
  'packages/envoy/internal/p/f.go',
  'package p\n\nvar T = template.Must(template.New("t").Parse("the title holds {{.N}} characters"))\n',
  [3]),
 ('gates: N2 Go false unit (code points) over len()',
  'packages/envoy/internal/p/g.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("title: %d characters (code points)", len(s))\n'
  '}\n',
  [4]),
 ('gates: N2 regression: Python {n} .format | plant3: review: Python {n} .format regression',
  'scripts/zz/fmt.py',
  'def said(t):\n    return "the title holds {n} characters".format(n=len(t.encode()))\n',
  [2]),
 ('gates: N2 regression: LC_ALL=C.UTF-8 cmd prefix | plant3: review: LC_ALL=C.UTF-8 cmd sets no script '
  'locale',
  'packages/envoy/scripts/zz-prefix.sh',
  '#!/usr/bin/env bash\n'
  'LC_ALL=C.UTF-8 sort -o /dev/null /dev/null\n'
  'name=$1\n'
  'printf \'name: %d characters\\n\' "${#name}"\n',
  [4]),
 ('gates: deep Go Sprint value then literal',
  'packages/envoy/internal/p/h.go',
  'package p\n\nfunc f(s, id string) error {\n\treturn errors.New(fmt.Sprint(len(s), " characters"))\n}\n',
  [4]),
 ('gates: deep Go %d %s word arg',
  'packages/envoy/internal/p/i.go',
  'package p\n\nfunc f(s, id string) error {\n\treturn fmt.Errorf("%d %s", len(s), "characters")\n}\n',
  [4]),
 ('gates: deep TS array join | plant3: deep: TS array join',
  'packages/envoy-client/src/zz-d.ts',
  'export const f = (s: string) => [s.length, "characters"].join(" ");\n',
  [1]),
 ('gates: deep Python print args | plant3: deep: Python print arguments',
  'scripts/zz/a.py',
  'import sys\ns = sys.argv[1].encode()\nprint("title is", len(s), "characters")\n',
  [3]),
 ('gates: deep Python {} format | plant3: deep: Python str.format {}',
  'scripts/zz/b.py',
  'import sys\ns = sys.argv[1].encode()\nprint("{} characters".format(len(s)))\n',
  [3]),
 ('gates: deep Python %(n)d mapping | plant3: deep: Python %(n)d mapping',
  'scripts/zz/c.py',
  'import sys\ns = sys.argv[1].encode()\nprint("%(n)d characters" % {"n": len(s)})\n',
  [3]),
 ('gates: deep shell value then bare word | plant3: deep: shell quoted value then a bare word',
  'packages/envoy/scripts/zz-c.sh',
  '#!/usr/bin/env bash\nn=$(printf %s "$1" | wc -c)\necho "$n" characters\n',
  [3]),
 ('gates: deep Go 1-40 characters under len() if',
  'packages/envoy/internal/p/j.go',
  'package p\n'
  '\n'
  'func f(s string) error {\n'
  '\tif len(s) > 40 {\n'
  '\t\treturn errors.New("a label is 1-40 characters")\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('gates: deep prose 1-40', 'docs/zz/d1.md', 'A label is 1-40 characters.\n', [1]),
 ('gates: deep prose `2000`', 'docs/zz/d2.md', 'A title is at most `2000` characters.\n', [1]),
 ('gates: deep prose **2,000**', 'docs/zz/d3.md', 'A title is at most **2,000** characters.\n', [1]),
 ('gates: deep Go character limit (%d)',
  'packages/envoy/internal/p/k.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("over the character limit (%d)", len(s))\n'
  '}\n',
  [4]),
 ('gates: deep Go length in characters is %d',
  'packages/envoy/internal/p/l.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("its length in characters is %d", len(s))\n'
  '}\n',
  [4]),
 ('gates: deep Go false unit UTF-16 over len()',
  'packages/envoy/internal/p/m.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("title is %d characters (UTF-16 code units)", len(s))\n'
  '}\n',
  [4]),
 ('gates: deep Go uuid in another clause',
  'packages/envoy/internal/p/n.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("issue %s, a uuid, has a body of %d characters", id, len(s))\n'
  '}\n',
  [4]),
 ('gates: deep Go hex in another clause',
  'packages/envoy/internal/p/o.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("ask %s (a hex id) holds %d characters", id, len(s))\n'
  '}\n',
  [4]),
 ('gates: quality shell unquoted chars=$n',
  'packages/envoy/scripts/zz-q1.sh',
  '#!/usr/bin/env bash\nn=$(printf %s "$1" | wc -c)\necho chars=$n\n',
  [3]),
 ('gates: quality JSX chars={s.length}',
  'packages/envoy-client/src/zz-q2.tsx',
  'export const C = ({ s }: { s: string }) => <p>chars={s.length}</p>;\n',
  [1]),
 ('gates: quality JSX at character {s.length}',
  'packages/envoy-client/src/zz-q3.tsx',
  'export const C = ({ s }: { s: string }) => <p>at character {s.length}</p>;\n',
  [1]),
 ('gates: goal Go %d Unicode characters over len()',
  'packages/envoy/internal/p/r.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("at most %d Unicode characters", len(s))\n'
  '}\n',
  [4]),
 ('gates: goal Go \\u0063haracters',
  'packages/envoy/internal/p/s.go',
  'package p\n'
  '\n'
  'func f(s, id string) error {\n'
  '\treturn fmt.Errorf("title is %d \\u0063haracters", len(s))\n'
  '}\n',
  [4]),
 ('N1: control fire',
  'packages/envoy/internal/p/a-2.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("title is %d characters", len(s))\n'
  '}\n',
  [4]),
 ('N1: control quiet',
  'packages/envoy/internal/p/q-2.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("title is %d characters", utf8.RuneCountInString(s))\n'
  '}\n',
  [4]),
 ('N1: X1 Go %d typed characters',
  'packages/envoy/internal/p/x1.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("the title holds %d typed characters", len(s))\n'
  '}\n',
  [4]),
 ('N1: X2 Go %d user-visible characters',
  'packages/envoy/internal/p/x2.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("the name holds %d user-visible characters", len(s))\n'
  '}\n',
  [4]),
 ('N1: X3 TS ${n} stored characters',
  'packages/envoy-client/src/zz-x3.ts',
  'export const f = (s: string) => `the body holds ${s.length} stored characters`;\n',
  [1]),
 ('N1: X4 prose 2,000 typed characters',
  'docs/zz/x4.md',
  'A title holds at most 2,000 typed characters.\n',
  [1]),
 ('N1: X5 Go %d of the characters',
  'packages/envoy/internal/p/x5.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("it keeps %d of the characters", len(s))\n'
  '}\n',
  [4]),
 ('N1: X6 Go %d of %d characters, len() first, the cap under RuneCount',
  'packages/envoy/internal/p/x6.go',
  'package p\n'
  '\n'
  'func f(s string, max int) error {\n'
  '\tif utf8.RuneCountInString(s) > max {\n'
  '\t\treturn fmt.Errorf("title: %d of at most %d characters", len(s), max)\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('N1: X7 TS ${n} of ${max} characters, .length first',
  'packages/envoy-client/src/zz-x7.ts',
  'const MAX_TITLE = 200;\n'
  'export const f = (s: string) => [...s].length > MAX_TITLE ? `title: ${s.length} of ${MAX_TITLE} '
  'characters` : "";\n',
  [2]),
 ('N1: X8 Go the %dth character',
  'packages/envoy/internal/p/x8.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("a NUL at the %dth character", strings.IndexByte(s, 0))\n'
  '}\n',
  [4]),
 ('N1: X9 Go %d (UTF-16) characters over len()',
  'packages/envoy/internal/p/x9.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("the title is %d (UTF-16) characters", len(s))\n'
  '}\n',
  [4]),
 ('N1: X10 JSX {n} <em>characters</em>',
  'packages/envoy-client/src/zz-x10.tsx',
  'export const C = ({ s }: { s: string }) => <p>{s.length} <em>characters</em></p>;\n',
  [1]),
 ('N1: X11 JSX {n}{" "}characters',
  'packages/envoy-client/src/zz-x11.tsx',
  'export const C = ({ s }: { s: string }) => <p>{s.length}{" "}characters</p>;\n',
  [1]),
 ('N1: X12 Go strings.Join of a slice',
  'packages/envoy/internal/p/x12.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn errors.New(strings.Join([]string{strconv.Itoa(len(s)), "characters"}, " "))\n'
  '}\n',
  [4]),
 ('N1: X13 Python join of a list',
  'scripts/zz/x13.py',
  'import sys\ns = sys.argv[1].encode()\nprint(" ".join([str(len(s)), "characters"]))\n',
  [3]),
 ('N1: X14 Python %d % len()',
  'scripts/zz/x14.py',
  'import sys\ns = sys.argv[1].encode()\nprint("%d characters" % len(s))\n',
  [3]),
 ('N1: X15 Go %w before %d',
  'packages/envoy/internal/p/x15.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("%w: %d characters", errTooLong, len(s))\n'
  '}\n',
  [4]),
 ('N1: X17 Go %d printable ASCII-only Latin wide characters',
  'packages/envoy/internal/p/x17.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("at most %d more visible printable characters", len(s))\n'
  '}\n',
  [4]),
 ('N1: X18 shell printf inside $( ) in a dq word',
  'packages/envoy/scripts/zz-x18.sh',
  '#!/usr/bin/env bash\nn=$(printf %s "$1" | wc -c)\necho "said: $(printf \'%d characters\' "$n")"\n',
  [3]),
 ('N1: X19 Go a single character, len()',
  'packages/envoy/internal/p/x19.go',
  'package p\n'
  '\n'
  'func f(s string) error {\n'
  '\tif len(s) != 1 {\n'
  '\t\treturn errors.New("the flag takes a single character")\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('N1: X20 Go quoted text beside a different count',
  'packages/envoy/internal/p/x20.go',
  'package p\n'
  '\n'
  'func f(s, id string, max int) error {\n'
  '\treturn fmt.Errorf("a %d-character name is refused, as the one character \\"-\\" is", len(s))\n'
  '}\n',
  [4]),
 ('N1: X22 prose a dozen characters', 'docs/zz/x22.md', 'A key holds a dozen characters.\n', [1]),
 ('N1: X23 TS the cap on characters',
  'packages/envoy-client/src/zz-x23.ts',
  'const MAX = 8000;\n'
  'export const f = (s: string) => s.length > MAX ? "the answer passes the cap on characters" : "";\n',
  [2]),
 ('N1: control fire',
  'packages/envoy/internal/p/a-3.go',
  'package p\n\nfunc f(s string) error {\n\treturn fmt.Errorf("title is %d characters", len(s))\n}\n',
  [4]),
 ('N1: X6b Go %d of at most %d characters: the cap a RuneCount compares, the count len()',
  'packages/envoy/internal/p/x6b.go',
  'package p\n'
  '\n'
  'const maxTitle = 200\n'
  '\n'
  'func f(s string) error {\n'
  '\tif utf8.RuneCountInString(s) > maxTitle {\n'
  '\t\treturn fmt.Errorf("title: %d of at most %d characters", len(s), maxTitle)\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [7]),
 ('N1: X6c TS: the cap a [...s].length compares, the count .length',
  'packages/envoy-client/src/zz-x6c.ts',
  'const MAX_TITLE = 200;\n'
  'export const f = (s: string) => {\n'
  '  if ([...s].length > MAX_TITLE) return `title: ${s.length} of at most ${MAX_TITLE} characters`;\n'
  '  return "";\n'
  '};\n',
  [3]),
 ('D10: control quiet: a bound its if counts in code points',
  'packages/envoy/internal/zzaq/quiet.go',
  'package zzaq\n'
  '\n'
  'func quiet(id string) error {\n'
  '\tif n := utf8.RuneCountInString(id); n > 200 {\n'
  '\t\treturn fmt.Errorf("an id is at most 200 characters, and this one is %d", n)\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('D10: control fire: a .length count | plant3: control fire: a .length count',
  'packages/envoy-client/src/zza-fire.ts',
  'export const cut = (text: string) => `cut at ${text.length} characters`;\n',
  [1]),
 ('D10: A1 Go: bound in runes, the length it reports in bytes',
  'packages/envoy/internal/zza1/trail.go',
  'package zza1\n'
  '\n'
  'func t(s string) error {\n'
  '\tif n := utf8.RuneCountInString(s); n > 200 {\n'
  '\t\treturn fmt.Errorf("a title is at most 200 characters, and this one is %d", len(s))\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('D10: A2 TS: bound in code points, the length it reports in UTF-16',
  'packages/envoy-client/src/zza-trail.ts',
  'export const t = (s: string) => {\n'
  '  if ([...s].length > 200) throw new Error(`a title is at most 200 characters, got ${s.length}`);\n'
  '};\n',
  [2]),
 ('D10: B1 Go: non-ASCII read as an ASCII grammar',
  'packages/envoy/internal/zza2/nonascii.go',
  'package zza2\n'
  '\n'
  'func n(name string) error {\n'
  '\treturn fmt.Errorf("a name in non-ASCII script is at most %d characters", len(name))\n'
  '}\n',
  [4]),
 ('D10: B2 prose: a non-ASCII title',
  'docs/zza/nonascii.md',
  'A non-ASCII title is at most 200 characters.\n',
  [1]),
 ('D10: C1 Go: uuid of the item, body counted in bytes',
  'packages/envoy/internal/zza3/uuid.go',
  'package zza3\n'
  '\n'
  'func u(id, body string) error {\n'
  '\treturn fmt.Errorf("the comment with uuid %s holds %d characters", id, len(body))\n'
  '}\n',
  [4]),
 ('D10: C2 TS: sha of the commit, note counted in UTF-16',
  'packages/envoy-client/src/zza-sha.ts',
  'export const n = (sha: string, note: string) => `the note on sha ${sha} holds ${note.length} '
  'characters`;\n',
  [1]),
 ('D10: D1 Go: %d of its characters',
  'packages/envoy/internal/zza4/ofits.go',
  'package zza4\n'
  '\n'
  'func o(s string) string {\n'
  '\treturn fmt.Sprintf("kept the first %d of its characters", len(s))\n'
  '}\n',
  [4]),
 ('D10: D2 Go: %d text characters',
  'packages/envoy/internal/zza4/text.go',
  'package zza4\n'
  '\n'
  'func t(b string) error {\n'
  '\treturn fmt.Errorf("the body holds %d text characters", len(b))\n'
  '}\n',
  [4]),
 ('D10: D3 prose: the first 2,000 of its characters',
  'docs/zza/ofits.md',
  'The answer keeps the first 2,000 of its characters.\n',
  [1]),
 ('D10: E1 Go: too many characters (%d)',
  'packages/envoy/internal/zza5/many.go',
  'package zza5\n'
  '\n'
  'func m(s string) error {\n'
  '\treturn fmt.Errorf("the title has too many characters (%d)", len(s))\n'
  '}\n',
  [4]),
 ('D10: E2 Go: the number of characters is %d',
  'packages/envoy/internal/zza5/number.go',
  'package zza5\n'
  '\n'
  'func n(s string) error {\n'
  '\treturn fmt.Errorf("the number of characters is %d", len(s))\n'
  '}\n',
  [4]),
 ('D10: E3 TS: the number of characters (${s.length})',
  'packages/envoy-client/src/zza-number.ts',
  'export const n = (s: string) => `the number of characters (${s.length}) is over the cap`;\n',
  [1]),
 ('D10: F1 Go: n += len(suffix) after a rune count',
  'packages/envoy/internal/zza6/plus.go',
  'package zza6\n'
  '\n'
  'func c(title, suffix string) error {\n'
  '\tn := utf8.RuneCountInString(title)\n'
  '\tn += len(suffix)\n'
  '\treturn fmt.Errorf("the title with its suffix is %d characters", n)\n'
  '}\n',
  [6]),
 ('D10: F2 TS: n += s.length after a code-point count',
  'packages/envoy-client/src/zza-plus.ts',
  'export const c = (t: string, s: string) => {\n'
  '  let n = [...t].length;\n'
  '  n += s.length;\n'
  '  return `${n} characters`;\n'
  '};\n',
  [4]),
 ('D10: G1 sh: LC_ALL set in a function the script never calls',
  'packages/envoy/scripts/zza-func.sh',
  '#!/usr/bin/env bash\n'
  'sorted() {\n'
  '  export LC_ALL=C.UTF-8\n'
  '  sort "$1"\n'
  '}\n'
  'name=$1\n'
  'echo "the name is ${#name} characters"\n',
  [7]),
 ('D10: G2 sh: LC_ALL set to UTF-8, then to C before the count',
  'packages/envoy/scripts/zza-reset.sh',
  '#!/usr/bin/env bash\n'
  'export LC_ALL=C.UTF-8\n'
  'name=$1\n'
  'export LC_ALL=C\n'
  'echo "the name is ${#name} characters"\n',
  [5]),
 ("D10: H1 TS: Array.from of an encoder's bytes",
  'packages/envoy-client/src/zza-from.ts',
  'export const b = (t: string) => `${Array.from(new TextEncoder().encode(t)).length} characters`;\n',
  [1]),
 ("D10: H2 TS: a spread of a Buffer's bytes",
  'packages/envoy-client/src/zza-buffer.ts',
  'export const b = (t: string) => `${[...Buffer.from(t)].length} characters`;\n',
  [1]),
 ('D10: I1 Go: a quoted example of the same length',
  'packages/envoy/internal/zza7/quote.go',
  'package zza7\n'
  '\n'
  'func q(t string) error {\n'
  '\tif len(t) > 4 {\n'
  '\t\treturn fmt.Errorf("a ticker is at most 4 characters \\"ABCD\\", and %q is not", t)\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('D10: J1 Go: %*d takes a width argument',
  'packages/envoy/internal/zza8/star.go',
  'package zza8\n\nfunc s(t string) string {\n\treturn fmt.Sprintf("%*d characters", 6, len(t))\n}\n',
  [4]),
 ('D10: J2 Go: %v',
  'packages/envoy/internal/zza8/v.go',
  'package zza8\n\nfunc v(t string) string {\n\treturn fmt.Sprintf("%v characters", len(t))\n}\n',
  [4]),
 ('D10: J3 Go: max %d chars',
  'packages/envoy/internal/zza8/chars.go',
  'package zza8\n\nfunc c(t string) string {\n\treturn fmt.Sprintf("max %d chars", len(t))\n}\n',
  [4]),
 ('D10: J4 Go: raw string literal',
  'packages/envoy/internal/zza8/raw.go',
  'package zza8\n\nfunc r(t string) error {\n\treturn fmt.Errorf(`a title is %d characters`, len(t))\n}\n',
  [4]),
 ('D10: J5 Go: exactly one character under len() != 1',
  'packages/envoy/internal/zza8/one.go',
  'package zza8\n'
  '\n'
  'func o(sep string) error {\n'
  '\tif len(sep) != 1 {\n'
  '\t\treturn errors.New("a separator is exactly one character")\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('D10: J6 Python: f-string with a format spec',
  'scripts/zza/spec.py',
  'import sys\nb = sys.stdin.buffer.read()\nprint(f"{len(b):,} characters")\n',
  [3]),
 ('D10: J7 Python: logging argument',
  'scripts/zza/log.py',
  'import logging, sys\nb = sys.stdin.buffer.read()\nlogging.warning("%d characters", len(b))\n',
  [3]),
 ('D10: J8 JS module: .mjs',
  'packages/envoy-client/scripts/zza.mjs',
  'export const f = (s) => `${s.length} characters`;\n',
  [1]),
 ('D10: J9 prose: a hundred characters',
  'docs/zza/hundred.md',
  'A title holds at most a hundred characters.\n',
  [1]),
 ('D10: K1 YAML: a # inside a quoted value',
  'docs/zza/value.yaml',
  'message: "Issue #42: a title is at most 200 characters"\n',
  [1]),
 ('D10: K2 YAML: a run line with a # before the count',
  'docs/zza/run.yaml',
  'steps:\n  - run: echo "::error::pull request #$PR has a title of ${#TITLE} characters"\n',
  [2]),
 ('D10: K3 YAML: a double-quoted \\u escape',
  'docs/zza/escape.yaml',
  'message: "a title is at most 200 \\u0063haracters"\n',
  [1]),
 ('D10: K4 markdown: a character reference',
  'docs/zza/ref.md',
  'A title is at most 200 &#99;haracters.\n',
  [1]),
 ('D10: pass: a code-point count that names UTF-16 is an instance',
  'packages/envoy-client/src/zza-pts.ts',
  'export const p = (s: string) => `${[...s].length} characters (UTF-16 code units)`;\n',
  [1]),
 ('D10: pass: at least one character',
  'packages/envoy/internal/zza9/one.go',
  'package zza9\n'
  '\n'
  'func o(s string) error {\n'
  '\tif len(s) < 1 {\n'
  '\t\treturn errors.New("a title holds at least one character")\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('D10: pass: a quoted dash',
  'packages/envoy/internal/zza9/dash.go',
  'package zza9\n'
  '\n'
  'func d(s string) error {\n'
  '\tif s != "-" {\n'
  '\t\treturn errors.New("send the one character \\"-\\"")\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('plant3: control quiet: a bound its if counts in code points',
  'packages/envoy/internal/zz3q/quiet.go',
  'package zz3q\n'
  '\n'
  'func quiet(id string) error {\n'
  '\tif n := utf8.RuneCountInString(id); n > 200 {\n'
  '\t\treturn fmt.Errorf("an id is at most 200 characters, and this one is %d", n)\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('plant3: quality 1: unquoted chars=$n over wc -c',
  'packages/envoy/scripts/zz3-chars.sh',
  '#!/usr/bin/env bash\nn=$(printf %s "$1" | wc -c)\necho chars=$n\necho "chars=$n"\n',
  [3, 4]),
 ('plant3: quality 1: JSX chars={s.length}',
  'packages/dispatch/web/src/zz3/Chars.tsx',
  'export const C = ({ s }: { s: string }) => <p>chars={s.length}</p>;\n'
  'export const D = ({ s }: { s: string }) => <p>{s.length} characters</p>;\n',
  [1, 2]),
 ('plant3: quality 1: JSX at character {s.length}',
  'packages/dispatch/web/src/zz3/At.tsx',
  'export const A = ({ s }: { s: string }) => <p>at character {s.length}</p>;\n',
  [1]),
 ('plant3: goal: %d Unicode characters over len()',
  'packages/envoy/internal/zz3g/unicode.go',
  'package zz3g\n'
  '\n'
  'func u(s string) error {\n'
  '\treturn fmt.Errorf("at most %d Unicode characters", len(s))\n'
  '}\n',
  [4]),
 ('plant3: goal: \\u0063haracters in Go',
  'packages/envoy/internal/zz3g/escape.go',
  'package zz3g\n'
  '\n'
  'func e(s string) error {\n'
  '\treturn fmt.Errorf("title is %d \\u0063haracters", len(s))\n'
  '}\n',
  [4]),
 ('plant3: goal: \\u0063haracters in TypeScript',
  'packages/envoy-client/src/zz3-escape.ts',
  'export const e = (s: string) => `cut at ${s.length} \\u0063haracters`;\n',
  [1]),
 ('plant3: goal: \\x63haracters in Python',
  'scripts/zz3/escape.py',
  'import sys\nb = sys.stdin.buffer.read()\nprint(f"read {len(b)} \\x63haracters")\n',
  [3]),
 ("plant3: goal: \\u0063haracters in shell $'...'",
  'packages/envoy/scripts/zz3-escape.sh',
  '#!/usr/bin/env bash\nn=$(printf %s "$1" | wc -c)\nprintf $\'%d \\u0063haracters\\n\' "$n"\n',
  [3]),
 ('plant3: goal: \\u0063haracters in JSON prose',
  'docs/zz3/escape.json',
  '{"rule": "a title is at most 2000 \\u0063haracters"}\n',
  [1]),
 ('plant3: deep: Go Println arguments',
  'packages/envoy/internal/zz3d/println.go',
  'package zz3d\n\nfunc p(s string) {\n\tfmt.Println("title is", len(s), "characters")\n}\n',
  [4]),
 ('plant3: deep: Go Sprint value then literal',
  'packages/envoy/internal/zz3d/sprint.go',
  'package zz3d\n\nfunc s(t string) string {\n\treturn fmt.Sprint(len(t), " characters")\n}\n',
  [4]),
 ('plant3: deep: Go %d %s with the word as the argument',
  'packages/envoy/internal/zz3d/wordarg.go',
  'package zz3d\n\nfunc w(t string) string {\n\treturn fmt.Sprintf("%d %s", len(t), "characters")\n}\n',
  [4]),
 ('plant3: deep: TS console.log arguments',
  'packages/envoy-client/src/zz3-log.ts',
  'export const f = (s: string) => console.log("title is", s.length, "characters");\n',
  [1]),
 ('plant3: deep: TS plural helper takes the count',
  'packages/envoy-client/src/zz3-plural.ts',
  'const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;\n'
  'export const f = (s: string) => `${plural(s.length, "character")} left`;\n',
  [2]),
 ('plant3: deep: Go 1-40 characters under if len(s) > 40',
  'packages/envoy/internal/zz3d/range.go',
  'package zz3d\n'
  '\n'
  'func r(s string) error {\n'
  '\tif len(s) > 40 {\n'
  '\t\treturn errors.New("a label is 1-40 characters")\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('plant3: deep: prose 1-40 and 1 to 40 characters',
  'docs/zz3/range.md',
  'A label is 1-40 characters.\n\nA label is 1 to 40 characters.\n',
  [1, 3]),
 ('plant3: deep: prose `2000`, **2,000** and 32k characters',
  'docs/zz3/forms.md',
  'A title is at most `2000` characters.\n'
  '\n'
  'A title is at most **2,000** characters.\n'
  '\n'
  'A body holds at most 32k characters.\n',
  [1, 3, 5]),
 ('plant3: deep: Go character limit (%d)',
  'packages/envoy/internal/zz3d/limit.go',
  'package zz3d\n'
  '\n'
  'func l(s string) error {\n'
  '\treturn fmt.Errorf("over the character limit (%d)", len(s))\n'
  '}\n',
  [4]),
 ('plant3: deep: Go length in characters is %d',
  'packages/envoy/internal/zz3d/length.go',
  'package zz3d\n'
  '\n'
  'func l(s string) error {\n'
  '\treturn fmt.Errorf("its length in characters is %d", len(s))\n'
  '}\n',
  [4]),
 ('plant3: deep: Go a false unit beside len()',
  'packages/envoy/internal/zz3d/unit.go',
  'package zz3d\n'
  '\n'
  'func u(s string) error {\n'
  '\treturn fmt.Errorf("title is %d characters (UTF-16 code units)", len(s))\n'
  '}\n',
  [4]),
 ('plant3: deep: Go uuid in another clause',
  'packages/envoy/internal/zz3d/uuid.go',
  'package zz3d\n'
  '\n'
  'func u(id, s string) error {\n'
  '\treturn fmt.Errorf("issue %s, a uuid, has a body of %d characters", id, len(s))\n'
  '}\n',
  [4]),
 ('plant3: deep: Go hex in a parenthesis',
  'packages/envoy/internal/zz3d/hex.go',
  'package zz3d\n'
  '\n'
  'func h(id, s string) error {\n'
  '\treturn fmt.Errorf("ask %s (a hex id) holds %d characters", id, len(s))\n'
  '}\n',
  [4]),
 ('plant3: review: TS test name one-character',
  'packages/contracts/src/zz3-one.test.ts',
  'test("rejects a one-character query", () => { expect(schema.safeParse({ query: "a" '
  '}).success).toBe(false); });\n',
  [1]),
 ('plant3: review: Go number word under if len()',
  'packages/envoy/internal/zz3r/words.go',
  'package zz3r\n'
  '\n'
  'func w(t string) error {\n'
  '\tif len(t) > 40 {\n'
  '\t\treturn errors.New("a label is at most forty characters")\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('plant3: review: TS 32k characters',
  'packages/envoy-client/src/zz3-k.ts',
  "export const k = (t: string) => t.length > 32000 ? 'a block is at most 32k characters' : '';\n",
  [1]),
 ('plant3: review: the character limit of 200',
  'packages/envoy/internal/zz3r/limitof.go',
  'package zz3r\n'
  '\n'
  'func l(t string) error {\n'
  '\tif len(t) > 200 {\n'
  '\t\treturn fmt.Errorf("title passes the character limit of 200: it is %d", len(t))\n'
  '\t}\n'
  '\treturn nil\n'
  '}\n',
  [5]),
 ('plant3: review: characters used: %d of 200',
  'packages/envoy/internal/zz3r/used.go',
  'package zz3r\n'
  '\n'
  'func u(t string) string {\n'
  '\treturn fmt.Sprintf("characters used: %d of 200", len(t))\n'
  '}\n',
  [4]),
 ('plant3: review: TS console.error arguments',
  'packages/envoy-client/src/zz3-error.ts',
  'export const p = (t: string) => console.error("the title holds", t.length, "characters");\n',
  [1]),
 ('plant3: review: Go CHARACTERS in capitals',
  'packages/envoy/internal/zz3r/upper.go',
  'package zz3r\n\nfunc u(t string) string {\n\treturn fmt.Sprintf("TITLE: %d CHARACTERS", len(t))\n}\n',
  [4]),
 ('plant3: review: Go template {{.N}} characters',
  'packages/envoy/internal/zz3r/tmpl.go',
  'package zz3r\n\nvar T = template.Must(template.New("t").Parse("the title holds {{.N}} characters"))\n',
  [3]),
 ('plant3: review: Go %d characters (code points) over len()',
  'packages/envoy/internal/zz3r/points.go',
  'package zz3r\n'
  '\n'
  'func n(t string) string {\n'
  '\treturn fmt.Sprintf("title: %d characters (code points)", len(t))\n'
  '}\n',
  [4]),
 ('plant3: review: TS TextEncoder bytes spread into an array',
  'packages/envoy-client/src/zz3-enc.ts',
  'export const b = (t: string) => `${[...new TextEncoder().encode(t)].length} characters`;\n',
  [1]),
 ('plant3: item 5: export LC_ALL=C is no UTF-8 locale',
  'packages/envoy/scripts/zz3-c.sh',
  '#!/usr/bin/env bash\nexport LC_ALL=C\nname=$1\nprintf \'name: %d characters\\n\' "${#name}"\n',
  [4]),
 ('plant3: item 5: a UTF-8 locale set after the count',
  'packages/envoy/scripts/zz3-after.sh',
  '#!/usr/bin/env bash\nname=$1\nprintf \'name: %d characters\\n\' "${#name}"\nexport LC_ALL=C.UTF-8\n',
  [3]),
 ('plant3: item 5: a Python f-string len() over bytes',
  'scripts/zz3/fstring.py',
  'import sys\nb = sys.stdin.buffer.read()\nprint(f"{len(b)} characters")\n',
  [3]),
 ('plant3: accept D7: an exemption covers only the next message',
  'packages/envoy/internal/zz3a/exempt.go',
  'package zz3a\n'
  '\n'
  'func e(id, name string) error {\n'
  '\t// characters: code points, validSessionID counts the id with utf8.RuneCountInString.\n'
  '\tif !validSessionID(id) { return fmt.Errorf("an id is at most 64 characters") }\n'
  '\treturn fmt.Errorf("a name is at most 40 characters, and this one is %d", len(name))\n'
  '}\n',
  [5, 6]),
 ('plant3: accept D7: a branch assigns the count in bytes',
  'packages/envoy/internal/zz3a/branch.go',
  'package zz3a\n'
  '\n'
  'func b(s string, raw bool) error {\n'
  '\tn := utf8.RuneCountInString(s)\n'
  '\tif raw {\n'
  '\t\tn = len(s)\n'
  '\t}\n'
  '\treturn fmt.Errorf("the text is %d characters", n)\n'
  '}\n',
  [8])]

OUTSIDE = [('N1: X16 Go Errorf with the word in a constant',
  'packages/envoy/internal/p/x16.go',
  'package p\n'
  '\n'
  'const unit = "characters"\n'
  '\n'
  'func f(s string) error {\n'
  '\treturn fmt.Errorf("the title is %d %s", len(s), unit)\n'
  '}\n',
  [3]),
 ('N1: X21 prose table header',
  'docs/zz/x21.md',
  '| Field | Most characters |\n| --- | --- |\n| title | 1000 |\n',
  [1]),
 ('N1: X11b JSX wrapped by a formatter: {n}{" "} then the word on the next line',
  'packages/envoy-client/src/zz-x11b.tsx',
  'export const C = ({ s }: { s: string }) => (\n'
  '  <p className="hint">\n'
  '    {s.length}{" "}\n'
  '    characters left of the title\n'
  '  </p>\n'
  ');\n',
  [4]),
 ('N1: X11c JSX one run across lines',
  'packages/envoy-client/src/zz-x11c.tsx',
  'export const C = ({ s }: { s: string }) => (\n'
  '  <p className="hint">\n'
  '    {s.length}\n'
  '    characters left of the title\n'
  '  </p>\n'
  ');\n',
  [4]),
 ('D10: L1 TS: the maximum number of characters under .length',
  'packages/envoy-client/src/zza-maxnum.ts',
  'const MAX = 8000;\n'
  'export const f = (t: string) => {\n'
  '  if (t.length > MAX) {\n'
  "    return 'the title passes the maximum number of characters';\n"
  '  }\n'
  "  return '';\n"
  '};\n',
  [4]),
 ('D10: L2 TS: too many characters under .length',
  'packages/envoy-client/src/zza-toomany.ts',
  'const MAX = 8000;\n'
  'export const f = (t: string) => {\n'
  '  if (t.length > MAX) {\n'
  "    return 'the title has too many characters';\n"
  '  }\n'
  "  return '';\n"
  '};\n',
  [4]),
 ('plant3: review: Go word in a variable as a %s argument',
  'packages/envoy/internal/zz3r/wordvar.go',
  'package zz3r\n'
  '\n'
  'func v(t string) string {\n'
  '\tword := "characters"\n'
  '\treturn fmt.Sprintf("the title is %d %s", len(t), word)\n'
  '}\n',
  [4]),
 ('plant3: item 14: prose that character limit',
  'docs/zz3/cap.md',
  'A margin is bounded in bytes and not by that character limit.\n\nIt names the characters left for it.\n',
  [1, 3]),
 ('plant3: item 14: TS the character cap under .length',
  'packages/envoy-client/src/zz3-cap.ts',
  'const MAX = 8000;\n'
  'export const f = (t: string) => {\n'
  '  if (t.length > MAX) {\n'
  "    return 'over the character cap';\n"
  '  }\n'
  "  return '';\n"
  '};\n',
  [4])]
