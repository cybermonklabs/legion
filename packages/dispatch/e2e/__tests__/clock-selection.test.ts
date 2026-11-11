/** The wall-clock file a Playwright command names, read as `e2e/playwright.config.ts` reads it
 *  before it refuses such a command (`e2e/configurations.ts`'s `clockFileOnCommandLine`).
 *
 *  Both directions cost a reader something, and only one of them is loud. A command the reader
 *  misses selects no row and Playwright answers "No tests found", which names no cure, and that
 *  answer is the whole reason the refusal exists (INFRA-1280). A command the reader answers for
 *  where Playwright would have run it is refused for nothing, and the rows below are the shapes
 *  that invite that reading: an option whose value is a file name or a grep, a `--` the filter
 *  sits behind, a user-interface run, which lists the non-default projects and lets a file filter
 *  select their rows, a filter holding part of a wall-clock name rather than the whole of it, and
 *  an argv that is not a Playwright command line at all.
 *
 *  Every row is a command a person or CI can type, written as the whole `process.argv` the
 *  Playwright process reads: the executable, the script, the `test` command word and the rest.
 */
import { describe, expect, test } from "bun:test";

import { clockFileOnCommandLine, clockSpecPattern } from "../configurations";
import { optionValueClasses } from "../playwright-argv";

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
    // The dash token is not the seed: an optional-value option leaves a token that begins with
    // a dash alone, measured at Playwright 1.64.1 (e2e/playwright-argv.ts). `--headed` is a
    // flag of its own, so the path behind it is a file this command does name.
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
    // mode: git diff <path> --name-only", measured at Playwright 1.64.1 (e2e/playwright-argv.ts).
    title: "a wall-clock path behind `--only-changed`, which Playwright takes as a git ref",
    argv: playwrightArgv("--only-changed", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    title: "the command the configuration's own refusal prints, which carries its project",
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
  {
    // The user interface lists the non-default projects so that they can be selected by name
    // (`playwright/lib/runner/index.js:6971` and its comment, `:6937`), so this command does
    // select rows and the refusal's premise does not hold for it; the cure it prints would drop
    // the `--ui` the reader asked for (e2e/configurations.ts).
    title: "a wall-clock file in a user-interface run, where that file's rows are selectable",
    argv: playwrightArgv("--ui", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    // `--ui-port` takes the token after it, so the port is read as its value either way: what
    // this row holds is the carve-out, which reads `--ui` and every `--ui-*` spelling over the
    // whole argv (`clockFileOnCommandLine`, e2e/configurations.ts).
    title: "that file under `--ui-port`, a user-interface run in another spelling",
    argv: playwrightArgv("--ui-port", "0", "e2e/heavy-bodies.e2e.ts"),
  },
  {
    // Measured at this head: `--list nested` is not refused and Playwright answers "No tests
    // found. Make sure that arguments are regular expressions matching test files." above
    // "Total: 0 tests in 0 files". A filter is read as naming a wall-clock file only where its
    // text holds that file's whole name, and this one holds a part of `nested-directive`.
    title: "a partial wall-clock name, which this reader does not answer for",
    argv: playwrightArgv("--list", "nested"),
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

/** The pinned Playwright's option table, read where this tree reads it: `optionValueClasses`
 *  (`e2e/playwright-argv.ts`), whose own test re-runs `bunx playwright test --help` and fails
 *  when that table adds, renames, drops or reclassifies an option
 *  (`e2e/__tests__/playwright-argv.test.ts`). Reading it a second time here would be a second
 *  copy of the same fact, one of which could age while the other did not. */
describe("the reader against the option classes it steps over", () => {
  test("a value of any class swallows the path, and a dash token behind one does not", () => {
    const rows = [
      ...[...optionValueClasses.required].map((spelling) => ({ spelling, taking: "required" })),
      ...[...optionValueClasses.optional].map((spelling) => ({ spelling, taking: "optional" })),
      ...[...optionValueClasses.variadic].map((spelling) => ({ spelling, taking: "variadic" })),
      // Two tokens no class holds: a flag, which takes nothing and leaves the path a filter,
      // and the user interface's own, which answers before any of this.
      { spelling: "--headed", taking: "nothing" },
      { spelling: "--ui", taking: "nothing" },
    ];
    // A parse that found nothing would assert nothing below.
    expect(rows.length).toBeGreaterThan(30);
    const answers = rows.map(({ spelling }) => {
      const alone = clockFileOnCommandLine(playwrightArgv(spelling, clockFile));
      const behindDash = clockFileOnCommandLine(playwrightArgv(spelling, "--headed", clockFile));
      return [
        `${spelling} <path> → ${alone ?? "no file named"}`,
        `${spelling} --headed <path> → ${behindDash ?? "no file named"}`,
      ].join(", ");
    });
    // An option that takes a value, of whatever class, takes the path and the command names no
    // file; a flag leaves the path a positional filter. Behind the dash token `--headed`, which
    // is a flag, the path is readable where the option's value stopped at it: a required value
    // takes `--headed` itself, an optional one takes nothing. The variadic `--project` is the
    // exception in both forms, because the project question is asked before the scan: it names
    // `--headed` and the path as its projects, so the command names a project and this reader
    // answers for no file. The user-interface spellings - `--ui`, `--ui-host` and `--ui-port` -
    // name no file either way: the user interface lists the non-default projects and a file
    // filter does select their rows, so the refusal's premise does not hold for such a command
    // (e2e/configurations.ts).
    const expected = rows.map(({ spelling, taking }) => {
      const userInterface = spelling === "--ui" || spelling.startsWith("--ui-");
      const namesAFilter = taking === "nothing" && !userInterface;
      const behindDash = !userInterface && taking !== "variadic";
      return [
        `${spelling} <path> → ${namesAFilter ? clockFile : "no file named"}`,
        `${spelling} --headed <path> → ${behindDash ? clockFile : "no file named"}`,
      ].join(", ");
    });
    expect(answers).toEqual(expected);
  });
});

describe("the pattern the wall-clock projects select their files by", () => {
  test("it holds no module those specs import, which is what the `.e2e.ts` suffix is for", () => {
    expect(clockSpecPattern.test("e2e/heavy-bodies.ts")).toBe(false);
    expect(clockSpecPattern.test("e2e/heavy-bodies-page.ts")).toBe(false);
    expect(clockSpecPattern.test("e2e/heavy-streamed-turn.e2e.ts")).toBe(false);
  });
});
