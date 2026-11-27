import { beforeAll, describe, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  expectNoMissingHelpers,
  nameMissingHelpers,
  scriptFunctions,
  sourcedLibraries,
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
const checkout = join(import.meta.dir, "..", "..", "..");

/** The libraries a script holding `lines` sources, as paths under the checkout. The script is
 *  written in the checkout's own scripts/e2e/lib, so a path written against the script's own
 *  directory lands where it would in the checkout, and under a name of this run alone, so two
 *  runs never read each other's. */
function sourcedBy(...lines: string[]): string[] {
  const script = join(checkout, "scripts", "e2e", "lib", `sourced-path-control-${randomUUID()}.sh`);
  try {
    writeFileSync(script, `#!/usr/bin/env bash\n${lines.join("\n")}\n`);
    return sourcedLibraries(script)
      .filter((path) => path !== script)
      .map((path) => relative(checkout, path));
  } finally {
    rmSync(script, { force: true });
  }
}

/** The source lines of a script holding `lines` that the reader refuses, each as it is written. */
function refusedBy(...lines: string[]): string[] {
  const script = join(checkout, "scripts", "e2e", "lib", `refused-path-control-${randomUUID()}.sh`);
  try {
    writeFileSync(script, `#!/usr/bin/env bash\n${lines.join("\n")}\n`);
    return unreadableSources(script).map((found) => found.replace(/^[^:]*:\d+: /, ""));
  } finally {
    rmSync(script, { force: true });
  }
}

// #4296's loopback-dsn.sh writes this line. Read by its last segments it names
// scripts/e2e/lib/pg-url.sh, a file that does not exist; resolved, it names the one it sources.
test("a sourced path that climbs out of its own directory names the file it reaches", () => {
  expect(sourcedBy('. "$psql_env_dir/../../../packages/envoy/scripts/lib/pg-url.sh"')).toEqual([
    "packages/envoy/scripts/lib/pg-url.sh",
  ]);
});

// The shape INFRA-3862's coordinator reports: a script sourcing a library of another package by a
// path holding /lib/<name>.sh. Matched by its tail it reads as a library of scripts/e2e/lib,
// which is where the suite's ENOENT came from.
test("a sourced library of another package is read from that package, not from scripts/e2e/lib", () => {
  expect(sourcedBy('. "$co/packages/envoy/scripts/lib/pg-url.sh"')).toEqual([
    "packages/envoy/scripts/lib/pg-url.sh",
  ]);
});
test("a sourced path against the checkout names the file it reaches", () => {
  expect(sourcedBy('. "$root/packages/envoy/scripts/lib/exec-env.sh"')).toEqual([
    "packages/envoy/scripts/lib/exec-env.sh",
  ]);
});

test("a sourced path against the script's own directory names the file beside it", () => {
  expect(sourcedBy('. "$lib/port-file.sh"')).toEqual(["scripts/e2e/lib/port-file.sh"]);
});

// Two shapes that fell between the walk's pattern and the refusal's, so each was read by one of
// them and not the other, or by neither. One matcher now reads both.
test("a sourced path written without quotes names the file it reaches", () => {
  expect(sourcedBy(". $lib/port-file.sh")).toEqual(["scripts/e2e/lib/port-file.sh"]);
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
  expect(sourcedBy('. "$lib/port-file.sh" # see "notes"')).toEqual([
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
