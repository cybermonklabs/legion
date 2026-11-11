/** The wall-clock file a Playwright command names, read as `e2e/playwright.config.ts` reads it
 *  before it refuses such a command (`e2e/configurations.ts`'s `clockFileOnCommandLine`).
 *
 *  Both directions cost a reader something, and only one of them is loud. A command the reader
 *  misses selects no row and Playwright answers "No tests found", which names no cure, and that
 *  answer is the whole reason the refusal exists (INFRA-1280). A command the reader answers for
 *  where Playwright would have run it is refused for nothing, and the rows below are the shapes
 *  that invite that reading: an option whose value is a file name or a grep, a `--` the filter
 *  sits behind, and an argv that is not a Playwright command line at all.
 *
 *  Every row is a command a person or CI can type, written as the whole `process.argv` the
 *  Playwright process reads: the executable, the script, the `test` command word and the rest.
 */
import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { clockFileOnCommandLine, clockSpecNames, clockSpecPattern } from "../configurations";

const clockFile = "e2e/heavy-bodies.e2e.ts";

/** `bunx playwright test --config e2e/playwright.config.ts <rest>` as the process that evaluates
 *  the configuration reads it. */
function playwrightArgv(...rest: readonly string[]): readonly string[] {
  return [
    "/usr/local/bin/bun",
    "node_modules/.bun/playwright@1.64.1/node_modules/playwright/cli.js",
    "test",
    "--config",
    "e2e/playwright.config.ts",
    ...rest,
  ];
}

const refused: readonly { title: string; argv: readonly string[]; answer: string }[] = [
  {
    title: "the path of a wall-clock spec, which is what a person copies out of the suite",
    argv: playwrightArgv("e2e/heavy-bodies.e2e.ts"),
    answer: "e2e/heavy-bodies.e2e.ts",
  },
  {
    title: "that spec's bare name, which Playwright matches against the whole path",
    argv: playwrightArgv("heavy-bodies"),
    answer: "heavy-bodies",
  },
  {
    title: "one row of a wall-clock spec, named by its file and line",
    argv: playwrightArgv("e2e/heavy-bodies.e2e.ts:245"),
    answer: "e2e/heavy-bodies.e2e.ts:245",
  },
  {
    title: "the other wall-clock spec, which `chromium-alone` alone runs",
    argv: playwrightArgv("e2e/nested-directive.e2e.ts"),
    answer: "e2e/nested-directive.e2e.ts",
  },
  {
    title: "an option's own value standing between the command word and the filter",
    argv: playwrightArgv("--repeat-each", "2", "heavy-bodies"),
    answer: "heavy-bodies",
  },
  {
    // The dash token is not the seed: `--shuffle --list` and `--only-changed --list` both reach
    // list mode and print rows, measured at Playwright 1.64.1 (e2e/configurations.ts). `--headed`
    // is a flag of its own, so the path behind it is a file this command does name.
    title: "a wall-clock file behind `--shuffle --headed`, whose dash token is no seed",
    argv: playwrightArgv("--shuffle", "--headed", "e2e/heavy-bodies.e2e.ts"),
    answer: "e2e/heavy-bodies.e2e.ts",
  },
];

const allowed: readonly { title: string; argv: readonly string[] }[] = [
  {
    title: "a title grep that holds a wall-clock name",
    argv: playwrightArgv("--grep", "heavy-bodies"),
  },
  {
    title: "that grep in the attached spelling, which carries its own value",
    argv: playwrightArgv("--grep=heavy-bodies"),
  },
  {
    title: "that grep in its short spelling",
    argv: playwrightArgv("-g", "heavy-bodies"),
  },
  {
    title: "a short grep whose value is a path, which is a title pattern all the same",
    argv: playwrightArgv("-g", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    title: "an option's value before a filter that names a spec of the default projects",
    argv: playwrightArgv("--reporter", "list", "e2e/claim.e2e.ts"),
  },
  {
    // `--list --shuffle e2e/claim.e2e.ts` lists the whole suite, the path having become the seed,
    // so the command names no file and Playwright runs it.
    title: "a wall-clock path behind `--shuffle`, which Playwright takes as the seed",
    argv: playwrightArgv("--shuffle", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    // `--list --debug e2e/claim.e2e.ts` exits 1 with "option '--debug [mode]' argument
    // 'e2e/claim.e2e.ts' is invalid. Allowed choices are inspector, cli.": the path is the
    // option's own value, which Playwright's choices refuse.
    title: "a wall-clock path behind `--debug`, a value its own choices refuse",
    argv: playwrightArgv("--debug", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    // The path becomes a git ref: `--list --only-changed <path>` and the attached
    // `--only-changed=<path>` print the same "Cannot detect changed files for --only-changed
    // mode: git diff <path> --name-only", measured at Playwright 1.64.1 (e2e/configurations.ts).
    title: "a wall-clock path behind `--only-changed`, which Playwright takes as a git ref",
    argv: playwrightArgv("--only-changed", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    title: "the guide's command for one wall-clock file in one engine",
    argv: playwrightArgv("--project=chromium-alone", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    title: "that command with the project named in the detached spelling",
    argv: playwrightArgv("--project", "chromium-alone", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    title: "a directory, which names no wall-clock file",
    argv: playwrightArgv("e2e/"),
  },
  {
    title: "a spec of the default projects",
    argv: playwrightArgv("e2e/claim.e2e.ts"),
  },
  {
    title: "a wall-clock name standing behind the `--` separator",
    argv: playwrightArgv("--reporter=list", "--", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    // A worker re-evaluates the configuration with Playwright's own argv, and a bun test or a
    // tool imports it with an argv of its own; none of them may meet the refusal.
    title: "a `bun test` argv that holds a wall-clock name and no `test` command word",
    argv: ["/usr/local/bin/bun", "e2e/__tests__/heavy-bodies-digests.test.ts"],
  },
];

describe("the command lines the configuration refuses", () => {
  for (const row of refused) {
    test(row.title, () => {
      expect(clockFileOnCommandLine(row.argv)).toBe(row.answer);
    });
  }
});

describe("the command lines it leaves alone", () => {
  for (const row of allowed) {
    test(row.title, () => {
      expect(clockFileOnCommandLine(row.argv)).toBeUndefined();
    });
  }
});

/** The pinned Playwright's own option table, read out of the version this workspace installed:
 *  `node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/program.js`, the
 *  `testOptions` array at line 188, from which `addTestCommand` at line 37 builds every option
 *  the `test` command has. The path names the pinned version, so a raise of Playwright fails
 *  this test on the missing file and the table is read here again against the new one. */
const programPath = resolve(
  import.meta.dir,
  "../../../../node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/program.js"
);

type DeclaredOption = { spellings: string[]; arity: "required" | "optional" | "none" };

/** Each option of that table: its spellings, long and short, and whether the table writes its
 *  value required (`<file>`), optional (`[mode]`) or declares none. */
function optionsOfPinnedPlaywright(): DeclaredOption[] {
  const source = readFileSync(programPath, "utf8");
  const start = source.indexOf("const testOptions = [");
  const table = source.slice(start, source.indexOf("\n];", start));
  return [...table.matchAll(/^\s*\["([^"]+)",/gm)].map((entry) => {
    const declaration = entry[1] ?? "";
    const arity = declaration.includes("<")
      ? "required"
      : declaration.includes("[")
        ? "optional"
        : "none";
    const spellings = declaration
      .replace(/\s*[<[].*$/, "")
      .split(",")
      .map((spelling) => spelling.trim());
    return { spellings, arity };
  });
}

describe("the reader's option table against the pinned Playwright's own", () => {
  test("a value of either arity swallows the path, and a dash token behind one does not", () => {
    const declared = optionsOfPinnedPlaywright();
    // A parse that found nothing would assert nothing below.
    expect(declared.length).toBeGreaterThan(30);
    const rows = declared.flatMap(({ arity, spellings }) =>
      spellings.map((spelling) => ({ arity, spelling }))
    );
    const answers = rows.map(({ spelling }) => {
      const alone = clockFileOnCommandLine(playwrightArgv(spelling, clockFile));
      const behindDash = clockFileOnCommandLine(playwrightArgv(spelling, "--headed", clockFile));
      return [
        `${spelling} <path> → ${alone ?? "no file named"}`,
        `${spelling} --headed <path> → ${behindDash ?? "no file named"}`,
      ].join(", ");
    });
    // An option that takes a value, required or optional, takes the path and the command names no
    // file; an option that declares none leaves the path a positional filter. Behind the dash
    // token `--headed`, which is a flag, the path is readable whatever the option's arity: a
    // required value takes `--headed` itself, an optional one takes nothing.
    const expected = rows.map(({ arity, spelling }) =>
      [
        `${spelling} <path> → ${arity === "none" ? clockFile : "no file named"}`,
        `${spelling} --headed <path> → ${clockFile}`,
      ].join(", ")
    );
    expect(answers).toEqual(expected);
  });
});

describe("the pattern the wall-clock projects select their files by", () => {
  test("it holds the two wall-clock specs and no module they import", () => {
    expect(clockSpecNames.every((name) => clockSpecPattern.test(`e2e/${name}.e2e.ts`))).toBe(true);
    expect(clockSpecPattern.test("e2e/heavy-bodies.ts")).toBe(false);
    expect(clockSpecPattern.test("e2e/heavy-bodies-page.ts")).toBe(false);
    expect(clockSpecPattern.test("e2e/heavy-streamed-turn.e2e.ts")).toBe(false);
  });
});
