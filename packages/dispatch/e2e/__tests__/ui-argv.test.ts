/** `e2e/ui-argv.ts`'s option set, against the help of the Playwright this checkout installs.
 *
 *  `optionsTakingTheNextArgument` decides which `--ui` of a command line is a flag and which is
 *  another option's value, so a raise of Playwright that adds, renames or drops an option that
 *  takes a separate value would silently move that line. It would show up as a refused ordinary
 *  run, or as a UI session that loaded with no stack and said nothing - the very silence
 *  `e2e/__tests__/ui-without-stack.test.ts` exists for. The two rows below read the set out of
 *  `bunx playwright test --help` instead, so the raise fails here.
 */

import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";
import { optionsTakingTheNextArgument } from "../ui-argv";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** One spawn of about two and a half seconds. */
const helpTimeout = 60_000;

/** One option of the help's own option list. `names` holds every spelling the help prints for it,
 *  the short one included; `value` is how the help writes what it takes. */
interface HelpOption {
  readonly names: readonly string[];
  readonly value: "none" | "optional" | "required";
}

/** The option list of `bunx playwright test --help`.
 *
 *  The help prints one option per line, two spaces in, as its spellings and then its value -
 *  `-g, --grep <grep>`, `--debug [mode]`, `--ui` - and at least two spaces before the
 *  description. A description that wraps is indented far enough that no continuation line starts
 *  with a dash in column 3, so the signature of each line is all this reads. */
async function helpOptions(): Promise<readonly HelpOption[]> {
  const child = Bun.spawn(["bunx", "playwright", "test", "--help"], {
    cwd: dispatchPackage,
    stderr: "pipe",
    stdout: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  expect(code, stderr).toBe(0);
  const options: HelpOption[] = [];
  for (const line of stdout.split("\n")) {
    const signature = /^ {2}(-\S.*?)(?: {2,}|\s*$)/.exec(line)?.[1];
    if (signature === undefined) continue;
    const names = signature.split(/[\s,]+/).filter((token) => /^-{1,2}[A-Za-z][\w-]*$/.test(token));
    if (names.length === 0) continue;
    const value = signature.endsWith(">")
      ? "required"
      : signature.endsWith("]")
        ? "optional"
        : "none";
    options.push({ names, value });
  }
  return options;
}

test(
  "every option the help writes `--name <value>` is in the set, in both spellings",
  async () => {
    const options = await helpOptions();
    // A help this read nothing out of would make every assertion below vacuous.
    expect(options.length).toBeGreaterThan(20);
    expect(options.flatMap(({ names }) => names)).toContain("--grep");

    const required = options
      .filter(({ value }) => value === "required")
      .flatMap(({ names }) => names)
      .sort();
    expect(required).toEqual([...optionsTakingTheNextArgument].sort());
  },
  helpTimeout
);

test(
  "an option the help writes with `[value]` or with no value is out of the set",
  async () => {
    // The other half of the rule, and the half that is measured rather than read: an option whose
    // value is optional leaves a token beginning with a dash alone, so `--debug --ui` opens the
    // UI where `--grep --ui` lists rows (the readings are in `e2e/ui-argv.ts`). A raise that made
    // one of these take a value would reach the set through the row above; this row catches the
    // reverse, an option the set names that stopped taking one.
    const options = await helpOptions();
    const optional = options
      .filter(({ value }) => value === "optional")
      .flatMap(({ names }) => names);
    const none = options.filter(({ value }) => value === "none").flatMap(({ names }) => names);
    expect(optional).toContain("--debug");
    expect(none).toContain("--ui");
    for (const name of [...optional, ...none]) {
      expect(optionsTakingTheNextArgument.has(name), `${name} takes no separate value`).toBe(false);
    }
  },
  helpTimeout
);
