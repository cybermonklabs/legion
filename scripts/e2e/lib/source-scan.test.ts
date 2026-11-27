/** The source scan names, for every `.` or `source` command an e2e script runs, the file the
 *  shell reads there, or refuses the command by name. Each row plants scripts in a tree of its own,
 *  laid out as the checkout is, and reads what the scan read and refused.
 *
 *  A plant's text starts on its file's second line, under its `#!` line, so a refusal of the
 *  plant's first line is at `:2`. A plant marked `run` is executable, which the scan reads as a
 *  script that runs; any other is a library, whose caller is whichever script sources it. The scan
 *  is given only the plants marked `run` or `scanned`, so a library is read only where a source
 *  reaches it: given every plant, a row would count a library as read that no source reached.
 */

import { describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { libRowBoundMs, rowWaits } from "./row-waits";
import { controlPlantMark } from "./script-functions";
import { type ScanAssumptions, scanSources } from "./source-scan";

rowWaits(libRowBoundMs);

const trees = temporaryDirectories("source-scan");

interface Plant {
  readonly path: string;
  readonly text: string;
  readonly run?: boolean;
  readonly scanned?: boolean;
}

interface Scanned {
  /** Every file the scan read, under the tree. */
  readonly read: string[];
  /** Every refusal, as the scan writes it. */
  readonly refused: string[];
  /** Where each refusal is: `<path>:<line>`, or the path alone. */
  readonly refusedAt: string[];
}

/** Plants `plants` in a fresh tree and scans the ones marked `run` or `scanned`. */
function scan(plants: readonly Plant[], assumptions: ScanAssumptions = {}): Scanned {
  const tree = trees();
  mkdirSync(join(tree, "scripts", "e2e", "lib"), { recursive: true });
  for (const { path, text, run } of plants) {
    const file = join(tree, path);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, `#!/usr/bin/env bash\n${text}\n${controlPlantMark}\n`, {
      mode: run === true ? 0o755 : 0o644,
    });
  }
  const scripts = plants
    .filter(({ run, scanned }) => run === true || scanned === true)
    .map(({ path }) => join(tree, path));
  const { read, refused } = scanSources(tree, scripts, assumptions);
  return {
    read: read.map((path) => relative(tree, path)).sort(),
    refused,
    refusedAt: refused.map((line) => /^[^:]+(?::\d+)?/.exec(line)?.[0] ?? line),
  };
}

/** A script that runs, at scripts/e2e/entry.sh, holding `lines`. */
const entry = (...lines: string[]): Plant => ({
  path: "scripts/e2e/entry.sh",
  text: lines.join("\n"),
  run: true,
});
/** A library at `path` that sources nothing. */
const library = (path: string): Plant => ({ path, text: ": a library" });
/** The checkout, as a script in scripts/e2e binds it. */
const bindRoot = 'root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)';
/** The script's own directory, bound to `dir`. */
const bindDir = 'dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)';

describe("a source in any command position is read or named", () => {
  // The reader once took `.` or `source` only at a line's start, so a source later in a command
  // list reached neither the walk nor the refusal.
  const positions = [
    '. "$root/scripts/e2e/lib/a.sh"; . "$root/scripts/e2e/lib/b.sh"',
    'true && . "$root/scripts/e2e/lib/b.sh"',
    'false || . "$root/scripts/e2e/lib/b.sh"',
    'if true; then . "$root/scripts/e2e/lib/b.sh"; fi',
    'for _ in 1; do . "$root/scripts/e2e/lib/b.sh"; done',
    '{ . "$root/scripts/e2e/lib/b.sh"; }',
    'load() { . "$root/scripts/e2e/lib/b.sh"; }; load',
  ];

  test("a source later in a command list is read", () => {
    for (const position of positions) {
      const found = scan([
        entry(bindRoot, position),
        library("scripts/e2e/lib/a.sh"),
        library("scripts/e2e/lib/b.sh"),
      ]);
      expect({ position, read: found.read.includes("scripts/e2e/lib/b.sh") }).toEqual({
        position,
        read: true,
      });
      expect({ position, refused: found.refused }).toEqual({ position, refused: [] });
    }
  });

  test("a source later in a command list that names no file it can know is refused at its line", () => {
    for (const position of positions) {
      const unknown = position.replaceAll("$root/scripts/e2e/lib/b.sh", "$elsewhere/b.sh");
      const found = scan([entry(bindRoot, unknown), library("scripts/e2e/lib/a.sh")]);
      expect({ unknown, refusedAt: found.refusedAt }).toEqual({
        unknown,
        refusedAt: ["scripts/e2e/entry.sh:3"],
      });
    }
  });
});

describe("quoted text and heredoc content are not commands", () => {
  // A jq program or a Python heredoc holds lines that start with `.` or `source` and are no
  // commands. The line reader refused them, so the next script to embed one failed the guard.
  test("a jq program and a heredoc are read as text, and the source after them is read", () => {
    const found = scan([
      entry(
        bindRoot,
        "jq -n '",
        "  . as $runs",
        "  | . + {seen: true}",
        "'",
        "python3 - <<'PY'",
        'source = "not a library"',
        "PY",
        '. "$root/scripts/e2e/lib/a.sh"'
      ),
      library("scripts/e2e/lib/a.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toEqual(["scripts/e2e/entry.sh", "scripts/e2e/lib/a.sh"]);
  });
});

describe("a variable holds at a source what the shell gives it there", () => {
  // The reviewer's case: a library sources a path on a variable its caller bound, and binds the
  // same variable again below the source. Bash reads the file in the caller's directory.
  test("a library reads the binding its caller gave, not an assignment below the source", () => {
    const found = scan([
      entry(bindDir, '. "$dir/lib/user.sh"'),
      { path: "scripts/e2e/lib/user.sh", text: `. "$dir/demo.sh"\n${bindDir}` },
      library("scripts/e2e/demo.sh"),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.read).toContain("scripts/e2e/demo.sh");
    expect(found.read).not.toContain("scripts/e2e/lib/demo.sh");
  });

  test("a variable bound only below the source is refused", () => {
    const found = scan([entry('. "$dir/demo.sh"', bindDir), library("scripts/e2e/demo.sh")]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:2"]);
  });

  test("a variable bound in one branch is refused, and one bound alike in both is read", () => {
    const one = scan([
      entry(`if [ -n "\${X:-}" ]; then ${bindDir}; fi`, '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(one.refusedAt).toEqual(["scripts/e2e/entry.sh:3"]);
    const both = scan([
      entry(`if [ -n "\${X:-}" ]; then ${bindDir}; else ${bindDir}; fi`, '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(both.refused).toEqual([]);
    expect(both.read).toContain("scripts/e2e/demo.sh");
  });

  test("a variable bound in a function is bound once the function runs, and not before", () => {
    const uncalled = scan([
      entry(`set_dir() { ${bindDir}; }`, '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(uncalled.refusedAt).toEqual(["scripts/e2e/entry.sh:3"]);
    const called = scan([
      entry(`set_dir() { ${bindDir}; }`, "set_dir", '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(called.refused).toEqual([]);
    expect(called.read).toContain("scripts/e2e/demo.sh");
  });

  // A function runs only where something calls it, so a source in one nothing calls is never run:
  // bash reads no file there, and the scan neither reads nor refuses one.
  test("a source in a function is read with the binding at its call, and not run where nothing calls it", () => {
    const called = scan([
      entry('load() { . "$dir/demo.sh"; }', bindDir, "load"),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(called.refused).toEqual([]);
    expect(called.read).toContain("scripts/e2e/demo.sh");
    const uncalled = scan([
      entry('load() { . "$dir/demo.sh"; }', bindDir),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(uncalled.refused).toEqual([]);
    expect(uncalled.read).toEqual(["scripts/e2e/entry.sh"]);
  });

  test("the last binding before the source is the one read", () => {
    const last = scan([
      entry("dir=/no/such/directory", bindDir, '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(last.refused).toEqual([]);
    expect(last.read).toContain("scripts/e2e/demo.sh");
    const reversed = scan([
      entry(bindDir, "dir=/no/such/directory", '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(reversed.refusedAt).toEqual(["scripts/e2e/entry.sh:4"]);
  });

  test("a binding in a subshell or a loop does not reach the source after it", () => {
    for (const binding of [`( ${bindDir} )`, `for _ in 1; do ${bindDir}; done`]) {
      const found = scan([entry(binding, '. "$dir/demo.sh"'), library("scripts/e2e/demo.sh")]);
      expect({ binding, refusedAt: found.refusedAt }).toEqual({
        binding,
        refusedAt: ["scripts/e2e/entry.sh:3"],
      });
    }
  });

  // stage3-4b13b-acceptance.sh binds root to ACCEPT_ROOT where the environment sets it. Read as
  // its default, the scan would read this tree's libraries for a run that reads another's.
  test("a value the environment may give is not known, and a default given where it is empty is", () => {
    const environment = scan([
      entry(
        `dir=\${ELSEWHERE_DIR:-$(cd "$(dirname "\${BASH_SOURCE[0]}")" && pwd -P)}`,
        '. "$dir/demo.sh"'
      ),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(environment.refused).toEqual([
      'scripts/e2e/entry.sh:3: . "$dir/demo.sh": $ELSEWHERE_DIR comes from the environment where it is set, so ${ELSEWHERE_DIR:-…} is not known here',
    ]);
    const empty = scan([
      entry(
        "given=",
        `dir=\${given:-$(cd "$(dirname "\${BASH_SOURCE[0]}")" && pwd -P)}`,
        '. "$dir/demo.sh"'
      ),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(empty.refused).toEqual([]);
    expect(empty.read).toContain("scripts/e2e/demo.sh");
  });

  // `read`, `for`, `printf -v` and `getopts` give a variable a value no text of the script holds.
  // The same word in text a command prints gives nothing.
  test("a value given by read, for, printf -v or getopts is not known, and the word read in text gives none", () => {
    for (const second of [
      "read -r dir </dev/null",
      "if ! read -r dir </dev/null; then exit 1; fi",
      "for dir in /a /b; do :; done",
      'printf -v dir "%s" /elsewhere',
      "getopts a: dir",
    ]) {
      const found = scan([
        entry(bindDir, second, '. "$dir/demo.sh"'),
        library("scripts/e2e/demo.sh"),
      ]);
      expect({ second, refusedAt: found.refusedAt }).toEqual({
        second,
        refusedAt: ["scripts/e2e/entry.sh:4"],
      });
    }
    const text = scan([
      entry(bindDir, 'echo "could not read the file dir names"', '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(text.refused).toEqual([]);
    expect(text.read).toContain("scripts/e2e/demo.sh");
  });

  // An `eval` runs text the scan does not parse, which may source a library or bind any variable,
  // so the eval itself is refused by name and nothing is known after it.
  test("an eval is refused by name, and no binding is known after it", () => {
    const found = scan([
      entry(bindDir, 'eval "dir=/elsewhere"', '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refused).toEqual([
      'scripts/e2e/entry.sh:3: eval "dir=/elsewhere": eval runs text this scan does not parse, which may source a library',
      'scripts/e2e/entry.sh:4: . "$dir/demo.sh": $dir is not known here: eval ran before here (scripts/e2e/entry.sh:3)',
    ]);
  });

  // A script sees its aliases only once it turns expand_aliases on, and then a word may stand for
  // `.` or `source`: the turning on is refused by name.
  test("a script that turns on aliases is refused by name", () => {
    const found = scan([
      entry(bindDir, "shopt -s expand_aliases", "alias load=source", 'load "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refused).toEqual([
      "scripts/e2e/entry.sh:3: shopt -s expand_aliases: an alias may name a source, and this scan does not follow aliases",
    ]);
  });
});

describe("$0 and the checkout are proved at each source", () => {
  // `$0` is the script that runs. In a library another script sources, it is that script, not
  // the library, so a library's `$(dirname "$0")` names its caller's directory.
  test("$0 in a sourced library is its caller", () => {
    const found = scan([
      entry(bindRoot, '. "$root/scripts/e2e/lib/user.sh"'),
      { path: "scripts/e2e/lib/user.sh", text: '. "$(dirname "$0")/demo.sh"' },
      library("scripts/e2e/demo.sh"),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.read).toContain("scripts/e2e/demo.sh");
    expect(found.read).not.toContain("scripts/e2e/lib/demo.sh");
  });

  test("$0 in a script that runs is the script itself", () => {
    const found = scan([
      entry('. "$(dirname "$0")/lib/demo.sh"'),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toContain("scripts/e2e/lib/demo.sh");
  });

  test("$0 in a library no script of the tree sources is refused", () => {
    const found = scan([
      { path: "scripts/e2e/lib/alone.sh", text: '. "$(dirname "$0")/demo.sh"', scanned: true },
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/lib/alone.sh:2"]);
  });

  test("$root is read through the binding its caller gave, and refused where nothing bound it", () => {
    const user = { path: "scripts/e2e/lib/user.sh", text: '. "$root/scripts/e2e/lib/demo.sh"' };
    const bound = scan([
      entry(bindRoot, '. "$root/scripts/e2e/lib/user.sh"'),
      user,
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(bound.refused).toEqual([]);
    expect(bound.read).toContain("scripts/e2e/lib/demo.sh");
    const unbound = scan([
      entry('. "$(dirname "${BASH_SOURCE[0]}")/lib/user.sh"'),
      user,
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(unbound.refusedAt).toEqual(["scripts/e2e/lib/user.sh:2"]);
    const elsewhere = scan([
      entry("root=/no/such/tree", '. "$(dirname "${BASH_SOURCE[0]}")/lib/user.sh"'),
      user,
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(elsewhere.refusedAt).toEqual(["scripts/e2e/lib/user.sh:2"]);
  });
});

describe("a path relative to the working directory is proved or refused", () => {
  test("a path built on the script's own path is refused after the script changes directory", () => {
    const found = scan([
      entry("cd /", '. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"'),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:3"]);
  });

  test("a directory bound before the change, and a change in a subshell, still read", () => {
    for (const lines of [
      [bindRoot, "cd /", '. "$root/scripts/e2e/lib/demo.sh"'],
      ["( cd / )", '. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"'],
    ]) {
      const found = scan([entry(...lines), library("scripts/e2e/lib/demo.sh")]);
      expect({ lines, refused: found.refused }).toEqual({ lines, refused: [] });
      expect(found.read).toContain("scripts/e2e/lib/demo.sh");
    }
  });

  test("a relative path, which the shell reads from the working directory, is refused", () => {
    const found = scan([
      entry(". ./lib/demo.sh", ". lib/demo.sh"),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:2", "scripts/e2e/entry.sh:3"]);
  });
});

describe("the word the shell reads is the word read", () => {
  test("a quoted part with more of the word after it names the whole word", () => {
    const found = scan([
      entry(
        bindRoot,
        '. "$root/scripts/e2e/lib/demo.sh"x',
        `. "$root/scripts/e2e/lib/demo.sh"'.bak'`
      ),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:3", "scripts/e2e/entry.sh:4"]);
    expect(found.read).toEqual(["scripts/e2e/entry.sh"]);
  });

  test("a path that reaches a directory is refused", () => {
    const found = scan([
      entry(bindRoot, '. "$root/scripts/e2e/lib"'),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:3"]);
    expect(found.read).toEqual(["scripts/e2e/entry.sh"]);
  });

  test("a script the parser cannot read is refused by name", () => {
    const found = scan([entry('. "$root/scripts/e2e/lib/demo.sh')]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh"]);
  });
});

describe("a refusal says why", () => {
  // One scan per case: after a source it refuses, the scan knows no binding, so a later case in
  // the same script would be refused for that reason rather than its own.
  test("each refusal names the command and the reason", () => {
    const cases: [string, string][] = [
      ['. "$elsewhere/a.sh"', '. "$elsewhere/a.sh": $elsewhere holds nothing known here'],
      [
        '. "$root/scripts/e2e/lib/no-such.sh"',
        '. "$root/scripts/e2e/lib/no-such.sh": names no regular file',
      ],
      [". ./lib/demo.sh", ". ./lib/demo.sh: a relative path reads the working directory"],
      [
        'cd /; . "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"',
        '. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh": its path is relative to a working directory the script changed',
      ],
    ];
    for (const [source, refusal] of cases) {
      const found = scan([entry(bindRoot, source), library("scripts/e2e/lib/demo.sh")]);
      expect(found.refused).toEqual([`scripts/e2e/entry.sh:3: ${refusal}`]);
    }
    // After a refused source nothing is known, and the refusal of what follows says so.
    const after = scan([
      entry(bindRoot, '. "$elsewhere/a.sh"', '. "$root/scripts/e2e/lib/demo.sh"'),
    ]);
    expect(after.refused).toEqual([
      'scripts/e2e/entry.sh:3: . "$elsewhere/a.sh": $elsewhere holds nothing known here',
      'scripts/e2e/entry.sh:4: . "$root/scripts/e2e/lib/demo.sh": $root is not known here: a source before here could not be read (scripts/e2e/entry.sh:3)',
    ]);
  });
});

// Review 6090323589 planted these shapes, each one bash runs as a source, and the scan neither read
// nor refused them. Each row holds one: read where the scan can know the file, refused by name
// where it cannot.
describe("every shape review 6090323589 planted is read or refused", () => {
  /** The one refusal a plant must give, by where it is, or that it is read without one. */
  const refusedAtLine = (lines: string[], at: number) => {
    const found = scan([entry(...lines), library("scripts/e2e/lib/demo.sh")]);
    return { lines, refusedAt: found.refusedAt, at: `scripts/e2e/entry.sh:${at}` };
  };

  test("a source in a substitution of a declaration, a loop's words, a redirection or a test is refused", () => {
    const hidden = '. "$elsewhere/demo.sh"';
    for (const lines of [
      [`export v=$(${hidden}; echo)`],
      [`f() { local v=$(${hidden}; echo); }`, "f"],
      [`for v in $(${hidden}; echo); do :; done`],
      [`while read -r v; do :; done < <(${hidden}; echo)`],
      [`[[ -n $(${hidden}) ]]`],
      [`case $(${hidden}) in *) : ;; esac`],
      [`cat <<EOF\n$(${hidden})\nEOF`],
    ]) {
      const found = scan([entry(...lines), library("scripts/e2e/lib/demo.sh")]);
      expect({ lines, refusedAt: found.refusedAt }).toEqual({
        lines,
        refusedAt: [`scripts/e2e/entry.sh:${lines[0]?.startsWith("cat") ? 3 : 2}`],
      });
    }
  });

  test('a source behind a command named by a known value, a "$@" wrapper or command -p is read', () => {
    const lib = '"$root/scripts/e2e/lib/demo.sh"';
    for (const lines of [
      [bindRoot, "src=source", `"$src" ${lib}`],
      [bindRoot, 'quietly() { "$@"; }', `quietly . ${lib}`],
      [bindRoot, 'run_after() { shift; "$@"; }', `run_after ignored source ${lib}`],
      [bindRoot, `command -p . ${lib}`],
      [bindRoot, `builtin source ${lib}`],
    ]) {
      const found = scan([entry(...lines), library("scripts/e2e/lib/demo.sh")]);
      expect({
        lines,
        refused: found.refused,
        read: found.read.includes("scripts/e2e/lib/demo.sh"),
      }).toEqual({
        lines,
        refused: [],
        read: true,
      });
    }
  });

  test("a command named by a value the scan does not know is refused where it may be a source of a file", () => {
    const found = scan([
      entry('"$unknown_command" "$unknown_file"'),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refused).toEqual([
      'scripts/e2e/entry.sh:2: "$unknown_command" "$unknown_file": its name may be `source`: $unknown_command holds nothing known here',
    ]);
    // An option after it is one `source` refuses as invalid, and a path in its own name, as in
    // "$work/bin/tool", is one `.` and `source` never are.
    const notSource = scan([
      entry(
        '"$unknown_command" --version',
        '"$work/bin/tool" "$unknown_file"',
        "tool=$(command -v tool)",
        '"$tool" "$unknown_file"'
      ),
    ]);
    expect(notSource.refused).toEqual([]);
  });

  test("a function defined in both arms of an if has both bodies walked at its call", () => {
    const found = scan([
      entry(
        bindRoot,
        `if [ -n "\${X:-}" ]; then load() { . "$root/scripts/e2e/lib/a.sh"; }; else load() { . "$root/scripts/e2e/lib/b.sh"; }; fi`,
        "load"
      ),
      library("scripts/e2e/lib/a.sh"),
      library("scripts/e2e/lib/b.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toEqual([
      "scripts/e2e/entry.sh",
      "scripts/e2e/lib/a.sh",
      "scripts/e2e/lib/b.sh",
    ]);
  });

  test("a loop body that rebinds through a function or a sourced file refuses the source it changes", () => {
    for (const rebind of ["set_dir() { dir=/elsewhere; }", ""]) {
      const lines = [
        bindRoot,
        rebind || ": no function",
        'dir="$root/scripts/e2e/lib"',
        `for _ in 1 2; do . "$dir/demo.sh"; ${rebind ? "set_dir" : '. "$root/scripts/e2e/lib/rebind.sh"'}; done`,
      ];
      const found = scan([
        entry(...lines),
        library("scripts/e2e/lib/demo.sh"),
        { path: "scripts/e2e/lib/rebind.sh", text: "dir=/elsewhere" },
      ]);
      expect({ rebind, refusedAt: found.refusedAt }).toEqual({
        rebind,
        refusedAt: ["scripts/e2e/entry.sh:5"],
      });
      expect(found.read).toContain("scripts/e2e/lib/demo.sh");
    }
  });

  test("shopt -s lastpipe is refused by name", () => {
    expect(
      refusedAtLine([bindRoot, "shopt -s lastpipe", "echo /elsewhere | read -r dir"], 3)
    ).toEqual(expect.objectContaining({ refusedAt: ["scripts/e2e/entry.sh:3"] }));
  });

  test("a function a substitution in a declaration calls is called, with the bindings there", () => {
    const found = scan([
      entry(
        bindRoot,
        'load() { . "$root/scripts/e2e/lib/demo.sh"; }',
        "g() { local out=$(load); }",
        "g"
      ),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toContain("scripts/e2e/lib/demo.sh");
  });

  test("builtin and command in front of eval, cd, read and printf -v are read through", () => {
    for (const [second, at] of [
      [`builtin eval '. "$dir/demo.sh"'`, 4],
      [`command eval '. "$dir/demo.sh"'`, 4],
      ["command cd /", 5],
      ["command read -r dir </dev/null", 5],
      ["builtin printf -v dir %s /elsewhere", 5],
    ] as const) {
      const lines = [bindRoot, 'dir="$root/scripts/e2e/lib"', second, '. "$dir/demo.sh"'];
      if (second === "command cd /") lines[3] = '. "$(dirname "${BASH_SOURCE[0]}")/lib/demo.sh"';
      const found = scan([entry(...lines), library("scripts/e2e/lib/demo.sh")]);
      expect({ second, refusedAt: found.refusedAt.slice(0, 1) }).toEqual({
        second,
        refusedAt: [`scripts/e2e/entry.sh:${at}`],
      });
    }
  });

  // A trap runs when its signal comes, which is after it is set and at the latest at the script's
  // end, so its text is walked with what holds both where it was set and at the end.
  test("a trap's text is walked with what holds where it was set and at the end", () => {
    const found = scan([
      entry(bindRoot, `trap '. "$root/scripts/e2e/lib/demo.sh"' EXIT`),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toContain("scripts/e2e/lib/demo.sh");
    const rebound = scan([
      entry(
        bindRoot,
        'dir="$root/scripts/e2e/lib"',
        `trap '. "$dir/demo.sh"' EXIT`,
        'dir="$root/scripts/e2e"'
      ),
      library("scripts/e2e/demo.sh"),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(rebound.refused).toEqual([]);
    expect(rebound.read).toEqual([
      "scripts/e2e/demo.sh",
      "scripts/e2e/entry.sh",
      "scripts/e2e/lib/demo.sh",
    ]);
    const unknownText = scan([entry('trap "$cleanup_text" EXIT')]);
    expect(unknownText.refused).toEqual([
      'scripts/e2e/entry.sh:2: trap "$cleanup_text" EXIT: a trap\'s text this scan does not know may source a library; $cleanup_text holds nothing known here',
    ]);
  });

  // A double-quoted trap text is expanded where the trap is set. An expansion that is one shell
  // word, as `printf %q` and `mktemp` print, cannot end a command in it, so the text is read with
  // that word in its place; any other expansion may, so the trap is refused.
  test("a trap's text holding a printf %q or mktemp word is read, and one holding any other expansion is refused", () => {
    const opaque = scan([
      entry(
        bindRoot,
        'q=$(printf %q "$tmp")',
        "t=$(mktemp)",
        'trap "rm -f $q $t; . \\"$root/scripts/e2e/lib/demo.sh\\"" EXIT'
      ),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(opaque.refused).toEqual([]);
    expect(opaque.read).toContain("scripts/e2e/lib/demo.sh");
    const other = scan([entry('trap "rm -f $unknown_path" EXIT')]);
    expect(other.refusedAt).toEqual(["scripts/e2e/entry.sh:2"]);
  });

  // A `case` branch runs only where its patterns match the word, so a variable the case tests
  // holds there a value they match. A loop that unsets every caller variable of some prefixes
  // leaves the script's own variables as they were; one that unsets any variable may empty them.
  test("a case branch narrows the variable it tests, so an unset it guards leaves other variables", () => {
    const sweep = (guarded: boolean) => [
      bindRoot,
      "for variable in $(compgen -e); do",
      guarded
        ? '  case $variable in DISPATCH_* | ENVOY_*) unset "$variable" ;; esac'
        : '  unset "$variable"',
      "done",
      '. "$root/scripts/e2e/lib/demo.sh"',
    ];
    const guarded = scan([entry(...sweep(true)), library("scripts/e2e/lib/demo.sh")]);
    expect(guarded.refused).toEqual([]);
    expect(guarded.read).toContain("scripts/e2e/lib/demo.sh");
    const open = scan([entry(...sweep(false)), library("scripts/e2e/lib/demo.sh")]);
    expect(open.refused).toEqual([
      'scripts/e2e/entry.sh:6: . "$root/scripts/e2e/lib/demo.sh": names no regular file',
    ]);
  });

  test("an arithmetic word is one argument, and `mise which` names a program", () => {
    const found = scan([
      entry(
        bindRoot,
        'wait_for() { shift 2; "$@"; }',
        'wait_for $((1 + 1)) label . "$root/scripts/e2e/lib/demo.sh"',
        "gh_bin=$(mise which gh)",
        '"$gh_bin" api graphql'
      ),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toContain("scripts/e2e/lib/demo.sh");
  });

  // A variable of the environment the scan is told names a program is that program as a command,
  // and never `source`; without that, a command it names may be `source` of a file.
  test("a command named by a program the environment names is that program", () => {
    const lines = ["kind_bin=${LEGION_E2E_KIND:-kind}", '"$kind_bin" create cluster'];
    expect(scan([entry(...lines)], { programs: ["LEGION_E2E_KIND"] }).refused).toEqual([]);
    expect(scan([entry(...lines)]).refusedAt).toEqual(["scripts/e2e/entry.sh:3"]);
  });

  test("a nameref, and a binding target only its call names, bind the variable they name", () => {
    for (const [lines, at] of [
      [
        [
          bindRoot,
          'dir="$root/scripts/e2e/lib"',
          "declare -n ref=dir",
          "ref=/elsewhere",
          '. "$dir/demo.sh"',
        ],
        6,
      ],
      [
        [
          bindRoot,
          'dir="$root/scripts/e2e/lib"',
          'set_var() { printf -v "$1" %s /elsewhere; }',
          "set_var dir",
          '. "$dir/demo.sh"',
        ],
        6,
      ],
      [
        [
          bindRoot,
          'dir="$root/scripts/e2e/lib"',
          'set_var() { declare -g "$1=$2"; }',
          "set_var dir /elsewhere",
          '. "$dir/demo.sh"',
        ],
        6,
      ],
      [
        [
          bindRoot,
          'dir="$root/scripts/e2e/lib"',
          'printf -v "$unknown_name" %s /elsewhere',
          '. "$dir/demo.sh"',
        ],
        5,
      ],
    ] as const) {
      const found = scan([entry(...lines), library("scripts/e2e/lib/demo.sh")]);
      expect({ lines, refusedAt: found.refusedAt }).toEqual({
        lines,
        refusedAt: [`scripts/e2e/entry.sh:${at}`],
      });
    }
  });

  // Each of these was a known limit at f5279681, and the walk now reads it.
  test("a command named by a known value runs the function it names, and the binding that leaves is read", () => {
    const found = scan([
      entry(bindDir, "set_dir() { dir=/elsewhere; }", "fn=set_dir", '"$fn"', '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refused).toEqual([
      'scripts/e2e/entry.sh:6: . "$dir/demo.sh": names no regular file',
    ]);
  });

  test("a path read out of an array's first word is read", () => {
    const found = scan([
      entry(
        `dirs=("$(cd "$(dirname "\${BASH_SOURCE[0]}")" && pwd -P)" /elsewhere)`,
        '. "${dirs[0]}/demo.sh"'
      ),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toContain("scripts/e2e/demo.sh");
  });

  // Where the branches before a source leave a variable any of a few known values, the scan reads
  // the file each names, and refuses one that names no file.
  test("a source on a variable two branches bound differently reads both files", () => {
    const found = scan([
      entry(
        bindRoot,
        `if [ -n "\${X:-}" ]; then dir="$root/scripts/e2e"; else dir="$root/scripts/e2e/lib"; fi`,
        '. "$dir/demo.sh"'
      ),
      library("scripts/e2e/demo.sh"),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toEqual([
      "scripts/e2e/demo.sh",
      "scripts/e2e/entry.sh",
      "scripts/e2e/lib/demo.sh",
    ]);
  });
});

// What the scan still does not see, each as it reads it today: these are the limits the pull
// request names, not guarantees. Two are not refusals: a command whose name is not known is taken
// for a program, and a source inside text another shell runs is not followed. A row here changes
// when its limit is closed, and so does the line naming it.
describe("known limits, each shown as the scan reads it today", () => {
  // A trap's text is walked with what holds both where it was set and at the script's end. A
  // binding the script changes and changes back in between is read as it was at both: bash here
  // reads /elsewhere/demo.sh where the signal comes while dir is /elsewhere; the scan reads only
  // scripts/e2e/lib/demo.sh.
  test("a trap's text does not see a binding changed and changed back between its setting and the end", () => {
    const found = scan([
      entry(
        bindRoot,
        'dir="$root/scripts/e2e/lib"',
        `trap '. "$dir/demo.sh"' EXIT`,
        "dir=/elsewhere",
        'dir="$root/scripts/e2e/lib"'
      ),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toEqual(["scripts/e2e/entry.sh", "scripts/e2e/lib/demo.sh"]);
  });

  // A command whose name the scan does not know is refused where it may be `source` of a file. Where
  // it is not, it is taken for a program: a function of the script it may name is not run through
  // it, so what that function binds is not applied. Bash here runs set_dir where $unknown_command
  // holds "set_dir", and then reads /elsewhere/demo.sh; the scan reads scripts/e2e/demo.sh.
  test("a command whose name is not known is taken for a program, not for a function it may name", () => {
    const found = scan([
      entry(bindDir, "set_dir() { dir=/elsewhere; }", '"$unknown_command"', '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toContain("scripts/e2e/demo.sh");
  });

  test("a branch that exits merges as one that goes on, so a binding bash keeps is refused", () => {
    const found = scan([
      entry(bindDir, 'if [ -n "${X:-}" ]; then dir=/elsewhere; exit 1; fi', '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:4"]);
  });

  test("a path read out of an array at an index other than 0 is refused", () => {
    const found = scan([
      entry(
        `dirs=(/elsewhere "$(cd "$(dirname "\${BASH_SOURCE[0]}")" && pwd -P)")`,
        '. "${dirs[1]}/demo.sh"'
      ),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:3"]);
  });

  test("an associative array's key holding a : and no quotes makes the whole script refused", () => {
    const found = scan([entry("declare -A seen", "seen[label:$key]=1", ". /dev/null")]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh"]);
  });

  test("a source inside text another shell runs is not followed: neither read nor refused", () => {
    const found = scan([
      entry(bindDir, `bash -c '. "$1/demo.sh"' bash "$dir"`),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refused).toEqual([]);
    expect(found.read).toEqual(["scripts/e2e/entry.sh"]);
  });
});

describe("without the parser the scan refuses", () => {
  // The scan reads shell through shfmt's syntax tree. Where shfmt cannot be built it must fail
  // naming the build, not pass over every script as one it read.
  test("a go that cannot build shfmt fails the scan, naming the build", () => {
    const run = spawnSync(
      "bun",
      [
        "-e",
        `import { scanSources } from ${JSON.stringify(join(import.meta.dir, "source-scan.ts"))}; scanSources("/", []); console.log("scanned");`,
      ],
      { env: { ...process.env, SOURCE_SCAN_GO: "/no/such/go" }, encoding: "utf8" }
    );
    expect(run.status).not.toBe(0);
    expect(run.stdout).not.toContain("scanned");
    expect(run.stderr).toContain("go install mvdan.cc/sh/v3/cmd/shfmt@v3.14.1");
  });
});

// A link to a library is read as that library; the file check takes a link to a regular file.
test("control: a sourced path that is a link to a library is read", () => {
  const tree = trees();
  mkdirSync(join(tree, "scripts", "e2e", "lib"), { recursive: true });
  writeFileSync(join(tree, "scripts/e2e/lib/demo.sh"), "#!/usr/bin/env bash\n: a library\n");
  symlinkSync("demo.sh", join(tree, "scripts/e2e/lib/link.sh"));
  const script = join(tree, "scripts/e2e/entry.sh");
  writeFileSync(script, `#!/usr/bin/env bash\n${bindRoot}\n. "$root/scripts/e2e/lib/link.sh"\n`, {
    mode: 0o755,
  });
  const { read, refused } = scanSources(tree, [script]);
  expect(refused).toEqual([]);
  expect(read.map((path) => relative(tree, path))).toContain("scripts/e2e/lib/link.sh");
});
