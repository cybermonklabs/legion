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
import { scanSources } from "./source-scan";

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
function scan(plants: readonly Plant[]): Scanned {
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
  const { read, refused } = scanSources(tree, scripts);
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

  test("a source in a function is read with the binding at its call, and refused when nothing calls it", () => {
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
    expect(uncalled.refusedAt).toEqual(["scripts/e2e/entry.sh:2"]);
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

// What the scan still does not see, each as it reads it today: these are the limits the pull
// request names, not guarantees. All but the last refuse by name. The last is a source the scan
// does not follow at all. A row here changes when its limit is closed, and so does the line
// naming it.
describe("known limits, each shown as the scan reads it today", () => {
  test("a function only a trap calls runs with nothing known, so its source is refused", () => {
    const found = scan([
      entry(bindRoot, 'load() { . "$root/scripts/e2e/lib/demo.sh"; }', "trap load EXIT"),
      library("scripts/e2e/lib/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:3"]);
    expect(found.read).toEqual(["scripts/e2e/entry.sh"]);
  });

  test("a command named by a variable leaves unknown what any function binds", () => {
    const found = scan([
      entry(bindDir, "set_dir() { dir=/elsewhere; }", "fn=set_dir", '"$fn"', '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refused).toEqual([
      'scripts/e2e/entry.sh:6: . "$dir/demo.sh": $dir may be bound by a function called through a variable',
    ]);
  });

  test("a branch that exits merges as one that goes on, so a binding bash keeps is refused", () => {
    const found = scan([
      entry(bindDir, 'if [ -n "${X:-}" ]; then dir=/elsewhere; exit 1; fi', '. "$dir/demo.sh"'),
      library("scripts/e2e/demo.sh"),
    ]);
    expect(found.refusedAt).toEqual(["scripts/e2e/entry.sh:4"]);
  });

  test("a path read out of an array is refused", () => {
    const found = scan([
      entry(`dirs=("$(cd "$(dirname "\${BASH_SOURCE[0]}")" && pwd -P)")`, '. "${dirs[0]}/demo.sh"'),
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
