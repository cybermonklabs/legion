/** `e2e/playwright-argv.ts`'s classification, against the help of the Playwright this checkout
 *  installs.
 *
 *  Two readers in this tree take their rule from that classification, both in
 *  `e2e/configurations.ts`. `projectsOnCommandLine` asks about a DASH token - is this
 *  `--project` an option of the run or another option's value? `clockFileOnCommandLine` asks
 *  the same of a `--ui` and of a `--`, and asks of a NON-DASH token whether it is a positional
 *  filter or some option's value. A raise of Playwright that adds, renames, drops or
 *  reclassifies an option that takes a value moves both readings silently, and each answers
 *  wrongly in a direction nobody looks at: a run given the stubbed layer's answer meets no
 *  stack, and a command that names a wall-clock file with no project is refused or let through
 *  on the wrong token.
 *
 *  Which options take a value is a fact about another program's interface, so this checkout
 *  cannot derive it - it can only read it where that program states it and keep the reading
 *  honest. A measured set in the module, behind a row here that re-reads `bunx playwright test
 *  --help` and compares, is that durable form: the module stays a plain constant every caller
 *  can read without spawning anything, and the day the fact changes a test fails instead of a
 *  run.
 */

import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";
import { optionValueClasses, valuesTakenAt } from "../playwright-argv";
import { runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** One spawn of about two and a half seconds. */
const helpTimeout = 60_000;

/** One option of the help's own option list. `names` holds every spelling the help prints for it,
 *  the short one included; `value` is how the help writes what it takes. */
interface HelpOption {
  readonly names: readonly string[];
  readonly value: "none" | "optional" | "required" | "variadic";
}

/** The option list of `bunx playwright test --help`.
 *
 *  The help prints one option per line, two spaces in, as its spellings and then its value -
 *  `-g, --grep <grep>`, `--debug [mode]`, `--project <project-name...>`, `--ui` - and at least
 *  two spaces before the description. A description that wraps is indented far enough that no
 *  continuation line starts with a dash in column 3, so the signature of each line is all this
 *  reads. */
async function helpOptions(): Promise<readonly HelpOption[]> {
  const { code, named, stderr, stdout } = await runBounded({
    argv: ["bunx", "playwright", "test", "--help"],
    bound: "short",
    cwd: dispatchPackage,
    from: import.meta.url,
  });
  expect(code, `${named} exited ${code}:\n${stderr}`).toBe(0);
  const options: HelpOption[] = [];
  for (const line of stdout.split("\n")) {
    const signature = /^ {2}(-\S.*?)(?: {2,}|\s*$)/.exec(line)?.[1];
    if (signature === undefined) continue;
    const names = signature.split(/[\s,]+/).filter((token) => /^-{1,2}[A-Za-z][\w-]*$/.test(token));
    if (names.length === 0) continue;
    const value = signature.endsWith("...>")
      ? "variadic"
      : signature.endsWith(">")
        ? "required"
        : signature.endsWith("]")
          ? "optional"
          : "none";
    options.push({ names, value });
  }
  return options;
}

test(
  "every option the help writes with a value is in its own class, in both spellings",
  async () => {
    const options = await helpOptions();
    // A help this read nothing out of would make every assertion below vacuous.
    expect(options.length).toBeGreaterThan(20);
    expect(options.flatMap(({ names }) => names)).toContain("--grep");

    const spellings = (value: HelpOption["value"]) =>
      options
        .filter((option) => option.value === value)
        .flatMap(({ names }) => names)
        .sort();
    // Each class separately: an option the help moved from one to another fails here, where a
    // single flat set of everything that takes a value would still hold it.
    expect(spellings("required")).toEqual([...optionValueClasses.required].sort());
    expect(spellings("optional")).toEqual([...optionValueClasses.optional].sort());
    expect(spellings("variadic")).toEqual([...optionValueClasses.variadic].sort());
  },
  helpTimeout
);

test("a required-value option takes the next token whatever it looks like", () => {
  expect(valuesTakenAt(["--grep", "--ui", "--list"], 0)).toBe(1);
  expect(valuesTakenAt(["--output", "--project=stub", "e2e/smoke.e2e.ts"], 0)).toBe(1);
  // Nothing to take: the parser would refuse the line, and a reader of it runs off no end.
  expect(valuesTakenAt(["--grep"], 0)).toBe(0);
});

test("an optional-value option takes the next token only when it is no dash token", () => {
  expect(valuesTakenAt(["--shuffle", "--ui", "--list"], 0)).toBe(0);
  expect(valuesTakenAt(["--shuffle", "e2e/claim.e2e.ts"], 0)).toBe(1);
  expect(valuesTakenAt(["--only-changed", "e2e/claim.e2e.ts"], 0)).toBe(1);
});

test("a variadic option takes its first value like a required one and stops at the next dash", () => {
  expect(valuesTakenAt(["--project", "--ui", "--list"], 0)).toBe(1);
  expect(valuesTakenAt(["--project", "chromium", "firefox", "--list"], 0)).toBe(2);
  expect(valuesTakenAt(["--project", "chromium", "firefox"], 0)).toBe(2);
});

test("a flag, a self-contained `--name=value` and a positional take nothing", () => {
  expect(valuesTakenAt(["--list", "--ui"], 0)).toBe(0);
  expect(valuesTakenAt(["--grep=--ui", "--list"], 0)).toBe(0);
  expect(valuesTakenAt(["e2e/smoke.e2e.ts", "--list"], 0)).toBe(0);
  expect(valuesTakenAt(["--", "--ui", "--list"], 0)).toBe(0);
});
