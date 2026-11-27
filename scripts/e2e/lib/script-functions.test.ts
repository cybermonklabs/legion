import { beforeAll, describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  controlPlantMark,
  expectNoMissingHelpers,
  nameMissingHelpers,
  scriptFunctions,
  sourcedLibrariesIn,
  unreadableSources,
} from "./script-functions";

const dir = temporaryDirectory("script-functions-test");
const path = join(dir, "fixture.sh");
/** The fixture's functions, read once the hook below has written it. */
let fn: (name: string) => string;
beforeAll(() => {
  writeFileSync(
    path,
    `#!/usr/bin/env bash
one_line() { echo "one line"; }
# A multi-line function follows a one-line one: the one-line definition ends at its own line.
several() {
  local x=1
  echo "$x"
}
`
  );
  fn = scriptFunctions(path);
});

describe("scriptFunctions", () => {
  test("a one-line function is that line alone, not the functions after it", () => {
    expect(fn("one_line")).toBe('one_line() { echo "one line"; }');
  });

  test("a multi-line function runs to the first closing brace alone on its line", () => {
    expect(fn("several")).toBe('several() {\n  local x=1\n  echo "$x"\n}');
  });

  test("a function the script does not define throws, naming the script", () => {
    expect(() => fn("absent")).toThrow(/fixture\.sh defines no absent\(\)/);
  });
});

// A harness that extracts a function must extract or stub every helper of the script that it
// calls; one it leaves out is a bare exit 127 the caller has to trace. The guard names it.
describe("nameMissingHelpers", () => {
  // Runs the fixture's several() with the guard above it, as a harness composes a run.
  function guarded(stubs: string) {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(path)}\n${stubs}\n${fn("several")}\nseveral\none_line`,
    ]);
    return { out: result.stdout.toString(), err: result.stderr.toString() };
  }

  test("names a helper of the script the harness left undefined", () => {
    const run = guarded("");
    expect(run.err).toContain("harness: no definition for one_line");
    expect(() => expectNoMissingHelpers(run.out, run.err)).toThrow(
      /neither extracts nor stubs one_line/
    );
  });

  test("says nothing once the harness stubs it", () => {
    const run = guarded("one_line() { :; }");
    expect(run.err).toBe("");
    expect(() => expectNoMissingHelpers(run.out, run.err)).not.toThrow();
  });

  test("names a helper the extracted function calls with its stdout and stderr silenced", () => {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(path)}\nquiet() { one_line >/dev/null 2>&1 || echo "code $?"; }\nquiet`,
    ]);
    expect(result.stdout.toString()).toBe("code 127\n");
    expect(result.stderr.toString()).toBe("harness: no definition for one_line\n");
  });

  test("names a helper a library the script sources defines", () => {
    const sourcing = join(dir, "sourcing.sh");
    writeFileSync(sourcing, '#!/usr/bin/env bash\n. "$root/scripts/e2e/lib/rig.sh"\n');
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(sourcing)}\nuntil_true 1 x true 2>/dev/null; echo "code $?"`,
    ]);
    expect(result.stdout.toString()).toBe("code 127\n");
    expect(result.stderr.toString()).toBe("harness: no definition for until_true\n");
  });

  test("leaves a missing command the script does not define as it was: exit 127, unnamed", () => {
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `${nameMissingHelpers(path)}\nno-such-command-${process.pid}\necho "code $?"`,
    ]);
    expect(result.stdout.toString()).toBe("code 127\n");
    expect(result.stderr.toString()).not.toContain("harness: no definition");
    expectNoMissingHelpers(result.stdout.toString(), result.stderr.toString());
  });
});

// The mirror of that rule: a stage script may define no helper nothing calls. A cutover that
// leaves one behind leaves a reader to work out which of two ways is the live one.
describe("every helper a stage script defines is called", () => {
  const e2e = join(import.meta.dir, "..");
  const readers = [
    ...new Bun.Glob("*.sh").scanSync({ cwd: e2e, absolute: true }),
    ...new Bun.Glob("lib/*.{sh,ts,py,jq}").scanSync({ cwd: e2e, absolute: true }),
  ].map((file) => readFileSync(file, "utf8"));

  for (const name of ["stage4b-sandbox-tree.sh", "stage4a-sandbox-runtime.sh"]) {
    test(name, () => {
      const text = readFileSync(join(e2e, name), "utf8");
      const defined = [...text.matchAll(/^([A-Za-z_][A-Za-z0-9_]*)\(\) \{/gm)].map((m) => m[1]);
      const dead = defined.filter((helper) => {
        const word = new RegExp(`\\b${helper}\\b`, "g");
        return readers.reduce((seen, file) => seen + (file.match(word)?.length ?? 0), 0) < 2;
      });
      expect(dead).toEqual([]);
    });
  }
});

// A library a script sources is found by resolving the path the script writes, against the
// checkout for `$root` and against the script's own directory for any other variable. Reading
// such a path by its last segments names a different file where the path holds `..`, and a
// library read under the wrong name hides what the real one holds.
//
// The rows read their scripts in a tree of their own, laid out as the checkout is and holding the
// libraries they source as stand-ins. A control written in the checkout sits in scripts/e2e/lib
// for as long as its row runs, and every scan of that directory on another worker reads it as a
// script of the tree: start-settings.test.ts reports its deliberately unreadable source as a real
// one, and a scan that listed it before it was removed fails reading it. A name of this run alone
// does not keep a directory scan from reading it.
const tree = temporaryDirectory("script-functions-sources");
const libraries = [
  "scripts/e2e/lib/port-file.sh",
  "packages/envoy/scripts/lib/pg-url.sh",
  "packages/envoy/scripts/lib/exec-env.sh",
];
beforeAll(() => {
  for (const library of libraries) {
    mkdirSync(dirname(join(tree, library)), { recursive: true });
    writeFileSync(join(tree, library), `#!/usr/bin/env bash\n${controlPlantMark}\n`);
  }
  // A link to a library is the library: the stricter file check must still read it.
  symlinkSync("port-file.sh", join(tree, "scripts", "e2e", "lib", "port-file-link.sh"));
});

/** Writes a control script holding `lines` in the tree's scripts/e2e/lib, so a path written
 *  against the script's own directory lands where it would in the checkout, and answers its path. */
function controlScript(lines: readonly string[]): string {
  const script = join(tree, "scripts", "e2e", "lib", `source-control-${randomUUID()}.sh`);
  writeFileSync(script, `#!/usr/bin/env bash\n${lines.join("\n")}\n${controlPlantMark}\n`);
  return script;
}

/** The libraries a script holding `lines` sources, as paths under the tree. */
function sourcedBy(...lines: string[]): string[] {
  const script = controlScript(lines);
  try {
    return sourcedLibrariesIn(tree, [script])
      .filter((path) => path !== script)
      .map((path) => relative(tree, path));
  } finally {
    rmSync(script, { force: true });
  }
}

/** The source lines of a script holding `lines` that the reader refuses, each as it is written. */
function refusedBy(...lines: string[]): string[] {
  const script = controlScript(lines);
  try {
    return unreadableSources(script, tree).map((found) => found.replace(/^[^:]*:\d+: /, ""));
  } finally {
    rmSync(script, { force: true });
  }
}

/** The line a script of the tree writes to keep its own directory in `lib`, which a source line
 *  built on `$lib` is read through. */
const ownLib = 'lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)';

// #4296's loopback-dsn.sh writes this line. Read by its last segments it names
// scripts/e2e/lib/pg-url.sh, a file that does not exist; resolved, it names the one it sources.
test("a sourced path that climbs out of its own directory names the file it reaches", () => {
  expect(
    sourcedBy(
      'psql_env_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)',
      '. "$psql_env_dir/../../../packages/envoy/scripts/lib/pg-url.sh"'
    )
  ).toEqual(["packages/envoy/scripts/lib/pg-url.sh"]);
});

// The shape INFRA-3862's coordinator reports: a script sourcing a library of another package by a
// path holding /lib/<name>.sh. Matched by its tail it reads as a library of scripts/e2e/lib,
// which is where the suite's ENOENT came from.
test("a sourced library of another package is read from that package, not from scripts/e2e/lib", () => {
  expect(
    sourcedBy(
      'co=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)',
      '. "$co/packages/envoy/scripts/lib/pg-url.sh"'
    )
  ).toEqual(["packages/envoy/scripts/lib/pg-url.sh"]);
});
test("a sourced path against the checkout names the file it reaches", () => {
  expect(sourcedBy('. "$root/packages/envoy/scripts/lib/exec-env.sh"')).toEqual([
    "packages/envoy/scripts/lib/exec-env.sh",
  ]);
});

test("a sourced path against the script's own directory names the file beside it", () => {
  expect(sourcedBy(ownLib, '. "$lib/port-file.sh"')).toEqual(["scripts/e2e/lib/port-file.sh"]);
});

// Two shapes that fell between the walk's pattern and the refusal's, so each was read by one of
// them and not the other, or by neither. One matcher now reads both.
test("a sourced path written without quotes names the file it reaches", () => {
  expect(sourcedBy(ownLib, ". $lib/port-file.sh")).toEqual(["scripts/e2e/lib/port-file.sh"]);
});

test("a source line that goes on past its path is read, and refused where the path is a variable", () => {
  expect(sourcedBy('. "$root/packages/envoy/scripts/lib/exec-env.sh" || exit 1')).toEqual([
    "packages/envoy/scripts/lib/exec-env.sh",
  ]);
  expect(refusedBy('. "$pg_url_lib" || exit 1')).toEqual(['. "$pg_url_lib" || exit 1']);
});

// A quoted word ends at the quote that closes it, not at the last quote on the line. Read to the
// last one, a source with a quoted word in its trailing comment named a path that does not exist,
// so its library was neither read nor refused. The `$(dirname …)` forms hold quotes of their own
// inside their substitution, and still end where their own closing quote is.
test("a source line with a quote in its trailing comment reads the library it names", () => {
  expect(sourcedBy(ownLib, '. "$lib/port-file.sh" # see "notes"')).toEqual([
    "scripts/e2e/lib/port-file.sh",
  ]);
});

test("a quoted word this reader cannot close is refused, rather than read as a path", () => {
  expect(refusedBy('. "$lib/port-file.sh')).toEqual(['. "$lib/port-file.sh']);
});

// A path built on a variable that reaches no file beside the script or under the checkout names
// a directory only the running shell knows, as `. "$drivers/clients.sh"` does: read as a file the
// script makes as it runs, the library behind it was passed over. The same holds for a path
// against `$root` that names no file. Each is refused, quoted, braced or not.
test("a sourced path that reaches no file of the tree is refused, not passed over", () => {
  expect(
    refusedBy(
      '. "$envoy_lib/pg-url.sh"',
      ". ${envoy_lib}/pg-url.sh",
      '. "$root/scripts/e2e/lib/no-such-library.sh"'
    )
  ).toEqual([
    '. "$envoy_lib/pg-url.sh"',
    ". ${envoy_lib}/pg-url.sh",
    '. "$root/scripts/e2e/lib/no-such-library.sh"',
  ]);
});

// A control's script is written in a tree of its own, laid out as the checkout is. Written in the
// checkout, it sat in scripts/e2e/lib for as long as its row ran, and a scan of that directory on
// another worker read it as a script of the tree: a deliberately unreadable source reported as a
// real one, or a file gone between the scan's listing and its read.
test("a control's script is read in a tree of its own, not in the checkout", () => {
  expect(sourcedBy('. "$root/scripts/e2e/lib/rig.sh"')).toEqual([]);
});

// Bash joins a quoted part and whatever touches it into one word, so `"$lib/port-file.sh"x` is
// the file port-file.shx. Read to its closing quote alone, the word named port-file.sh, a file
// the shell never sources. A quote ends the word only where a space, the line's end or one of
// `;&|<>()` follows it; any other continuation is refused.
test("a quoted part with more of the word after it is refused, not read as the part", () => {
  const lines = [
    '. "$lib/port-file.sh"x',
    `. "$root/packages/envoy/scripts/lib/exec-env.sh"'.bak'`,
  ];
  expect(refusedBy(...lines)).toEqual(lines);
});

// The stricter end must refuse nothing the shell reads as the file: the own-directory forms the
// tree's scripts write, whose substitutions hold quotes of their own, and a quote that a `;`, a
// space or the line's end follows.
test("control: a quoted word the shell ends at its quote still reads the file it names", () => {
  const lines = [
    '. "$(dirname "$0")/port-file.sh"',
    '. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/port-file.sh"',
    '. "$lib/port-file.sh";',
    '. "$lib/port-file.sh" # a comment',
  ];
  expect(lines.map((line) => sourcedBy(ownLib, line))).toEqual(
    lines.map(() => ["scripts/e2e/lib/port-file.sh"])
  );
  expect(refusedBy(ownLib, ...lines)).toEqual([]);
});

// A path that reaches a directory is no library: bash's `.` refuses one, and the walk, taking it
// for a file, threw on reading it rather than naming the line. It is refused like a path that
// reaches nothing, and the walk reads on past it.
test("a sourced path that reaches a directory is refused, and the walk reads on", () => {
  expect(refusedBy('. "$root/packages/envoy/scripts/lib"', ". ../lib")).toEqual([
    '. "$root/packages/envoy/scripts/lib"',
    ". ../lib",
  ]);
  expect(
    sourcedBy(ownLib, '. "$root/packages/envoy/scripts/lib"', '. "$lib/port-file.sh"')
  ).toEqual(["scripts/e2e/lib/port-file.sh"]);
});

// The file check's own control: a link to a library is read as the library it links to.
test("control: a sourced path that is a link to a file is read", () => {
  expect(sourcedBy(ownLib, '. "$lib/port-file-link.sh"')).toEqual([
    "scripts/e2e/lib/port-file-link.sh",
  ]);
  expect(refusedBy(ownLib, '. "$lib/port-file-link.sh"')).toEqual([]);
});

// A path built on a variable other than `$root` is read through that variable's assignment in the
// same script, and only there. Read by trying the directory beside the script and then the
// checkout, a variable that named another directory reached the file of the same name beside the
// script: a real file, read, while the shell sources a different one.
test("a sourced path on a variable this script assigns elsewhere is refused, not read beside it", () => {
  const lines = ["drivers=/no/such/directory", '. "$drivers/port-file.sh"'];
  expect(sourcedBy(...lines)).toEqual([]);
  expect(refusedBy(...lines)).toEqual(['. "$drivers/port-file.sh"']);
});

test("a sourced path on a variable this script never assigns is refused", () => {
  expect(refusedBy('. "$drivers/port-file.sh"')).toEqual(['. "$drivers/port-file.sh"']);
});

test("a sourced path on a variable this script assigns a second time is refused", () => {
  expect(refusedBy(ownLib, "lib=/no/such/directory", '. "$lib/port-file.sh"')).toEqual([
    '. "$lib/port-file.sh"',
  ]);
});

// `read`, `for` and `printf -v` assign a variable too, and a script that gives the directory a
// second value that way is one this reader cannot be sure of. The same words inside text a
// command prints are no assignment, and must not make it unsure.
test("a variable given a second value by read is refused, and the word read in text is not", () => {
  for (const second of [
    "read -r lib </dev/null",
    "if ! read -r lib </dev/null; then exit 1; fi",
    "for lib in /a /b; do :; done",
    'printf -v lib "%s" /elsewhere',
  ]) {
    expect({ second, refused: refusedBy(ownLib, second, '. "$lib/port-file.sh"') }).toEqual({
      second,
      refused: ['. "$lib/port-file.sh"'],
    });
  }
  const lines = [ownLib, 'echo "could not read the file lib names"', '. "$lib/port-file.sh"'];
  expect(refusedBy(...lines)).toEqual([]);
  expect(sourcedBy(...lines)).toEqual(["scripts/e2e/lib/port-file.sh"]);
});

// The controls for the stricter rule: every spelling of a script's own directory the repository
// writes, assigned to a variable or written in the source line itself, and a directory under
// `$root`, read through to the file the shell sources.
test("control: a variable assigned the script's own directory, or a path under root, is read", () => {
  const cases: [string[], string][] = [
    [[ownLib, '. "$lib/port-file.sh"'], "scripts/e2e/lib/port-file.sh"],
    [
      ['script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)', '. "$script_dir/port-file.sh"'],
      "scripts/e2e/lib/port-file.sh",
    ],
    [
      [
        'repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd -P)',
        '. "$repo_root/scripts/e2e/lib/port-file.sh"',
      ],
      "scripts/e2e/lib/port-file.sh",
    ],
    [
      [
        "case ${BASH_SOURCE[0]} in */*) here=${BASH_SOURCE[0]%/*} ;; *) here=. ;; esac",
        '. "$here/port-file.sh"',
      ],
      "scripts/e2e/lib/port-file.sh",
    ],
    [
      ["drivers=$root/packages/envoy/scripts/lib", '. "$drivers/pg-url.sh"'],
      "packages/envoy/scripts/lib/pg-url.sh",
    ],
  ];
  expect(cases.map(([lines]) => sourcedBy(...lines))).toEqual(cases.map(([, file]) => [file]));
  expect(cases.flatMap(([lines]) => refusedBy(...lines))).toEqual([]);
});

test("control: each spelling of the script's own directory written in the source line is read", () => {
  const lines = [
    '. "$(dirname "${BASH_SOURCE[0]}")/port-file.sh"',
    '. "$(dirname -- "${BASH_SOURCE[0]}")/port-file.sh"',
    '. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/port-file.sh"',
    '. "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)/lib/port-file.sh"',
  ];
  expect(lines.map((line) => sourcedBy(line))).toEqual(
    lines.map(() => ["scripts/e2e/lib/port-file.sh"])
  );
  expect(refusedBy(...lines)).toEqual([]);
});
