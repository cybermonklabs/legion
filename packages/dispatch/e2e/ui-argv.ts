/** What each `playwright test` option takes of the tokens after it, read the way the runner's own
 *  parser reads argv.
 *
 *  Two readers of one command line ask this, and they ask it of different targets.
 *  `e2e/configurations.ts`'s `projectsOnCommandLine` asks about a DASH token: is this `--project`
 *  an option of the run, or another option's value? The INFRA-1280 tree asks about a POSITIONAL:
 *  is this path a test-file filter, or another option's value? A flat `argv.some()` answers
 *  neither, because a flat argv cannot tell an option from an option's VALUE: `--grep --ui` hands
 *  Playwright `--ui` as `--grep`'s value and the run is an ordinary one, and `--shuffle
 *  e2e/claim.e2e.ts` names no spec file, because the path is the seed (dispatch://INFRA-1410,
 *  dispatch://INFRA-1280). Both readers take the classification below and `valuesTakenAt`, so
 *  neither carries a rule of its own and a reader of a positional is right by construction.
 *
 *  The readings this is built on were taken against a configuration that refuses nothing and
 *  holds one trivial row, each command `bunx playwright test --config <that config> <arguments>`.
 *  The target is `--ui`, which Playwright reads as a UI session on any of three flags - `--ui`,
 *  `--ui-host <host>` and `--ui-port <port>` (`opts.ui || opts.uiHost || opts.uiPort`,
 *  node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/cli/testActions.js:60,
 *  declared at lib/program.js:223-225) - so each row says in one word whether the token before it
 *  swallowed it.
 *
 *  A DASH-token target, measured in this tree:
 *
 *    --grep --ui --list                      listed 2 tests, exit 0, no UI server
 *    -g --ui --list                          listed 2 tests, exit 0, no UI server
 *    --grep --ui-port --list                 listed 2 tests, exit 0, no UI server
 *    --grep=--ui --list                      listed 2 tests, exit 0, no UI server
 *    --grep --ui-port=0 --list               listed 0 tests, no UI server (it took the whole token)
 *    --debug --ui --list                     UI mode
 *    --update-snapshots --ui --list          UI mode
 *    -u --ui --list                          UI mode
 *    --shuffle --ui --list                   UI mode
 *    --project chromium --ui --list          UI mode
 *    --project chromium firefox --ui --list  UI mode
 *    --ui --list                             UI mode
 *    --project --ui --list                   `Project(s) "--ui" not found`
 *    -- --ui --list                          ran the 2 rows: after `--` a token filters files
 *
 *  This container has no X server, so the UI's own window cannot open and each UI reading above
 *  ends in `runUIMode` with Playwright's "launched a headed browser without having a XServer
 *  running" notice. The same code path with a port prints its listener instead, and those
 *  readings are the `Listening on` lines: `--ui-port=0 --list` gave `Listening on
 *  http://localhost:53621`, `--debug --ui-port=0 --list` gave `http://localhost:30480`,
 *  `--update-snapshots --ui-port=0 --list` gave `http://localhost:34351`, `--shuffle
 *  --ui-port=0 --list` gave `http://localhost:65114`, `--project chromium --ui-port=0 --list`
 *  gave `http://localhost:57029` and `--project chromium --ui-port 0 --list` gave
 *  `http://localhost:35679`.
 *
 *  A NON-DASH target, measured in the INFRA-1280 tree against that tree's own suite, where the
 *  target is a spec path and the reading is the listing it produced:
 *
 *    --list --shuffle e2e/claim.e2e.ts       listed 1,384 tests in 179 files: the whole suite,
 *                                            the path taken as the seed and no file filtered
 *    --list --only-changed e2e/claim.e2e.ts  listed 0 tests in 0 files, `--only-changed <path>`
 *                                            printing `Cannot detect changed files for
 *                                            --only-changed mode: git diff <path> --name-only`
 *
 *  Together the two sets of readings settle every class. A REQUIRED-value option takes the next
 *  token whatever it looks like, a `--ui` included, which is why `--grep --ui` lists rows. An
 *  OPTIONAL-value option leaves a token that begins with a dash alone, which is why `--debug
 *  --ui` opens the UI - and takes one that does not, which is why `--shuffle e2e/claim.e2e.ts`
 *  shuffles by that path and lists every file. The dash-token readings alone would put the
 *  optional four out of a flat set and read a positional after them wrongly; the INFRA-1280
 *  readings are what fix the other half of that rule. A VARIADIC option (`--project
 *  <project-name...>`) behaves as a required one for its first value and stops at the next dash
 *  token, so `--project --ui` reads `--ui` as a project name and `--project chromium --ui`
 *  leaves `--ui` a flag.
 */

/** The class an option's value belongs to, as `bunx playwright test --help` writes it: a required
 *  value `--name <value>`, an optional one `--name [value]`, a variadic one `--name <value...>`.
 *  An option the help writes with no value takes none, and is in no set here. */
export type OptionValueClass = "optional" | "required" | "variadic";

/** Every `playwright test` option that takes a value, by its class, long spelling and short
 *  spelling alike, read from `bunx playwright test --help` at playwright 1.64.1. The help prints
 *  both spellings on one line where an option has a short one (`-g, --grep <grep>`), which is
 *  where the short names below come from.
 *
 *  `e2e/__tests__/ui-argv.test.ts` runs that help and asserts all three sets against it, so a
 *  raise of Playwright that adds, renames, drops or RECLASSIFIES one of these fails a test rather
 *  than silently moving where some command line's values end a year later. */
export const optionValueClasses: Readonly<Record<OptionValueClass, ReadonlySet<string>>> = {
  optional: new Set(["--debug", "--only-changed", "--shuffle", "-u", "--update-snapshots"]),
  required: new Set([
    "--add-reporter",
    "--browser",
    "-c",
    "--config",
    "-g",
    "--grep",
    "-G",
    "--grep-invert",
    "--global-timeout",
    "-j",
    "--workers",
    "--last-failed-file",
    "--max-failures",
    "--output",
    "--repeat-each",
    "--reporter",
    "--retries",
    "--run-agents",
    "--shard",
    "--test-list",
    "--test-list-invert",
    "--timeout",
    "--trace",
    "--tsconfig",
    "--ui-host",
    "--ui-port",
    "--update-source-method",
  ]),
  variadic: new Set(["--project"]),
};

/** How many of the tokens after `argv[index]` that option takes as its value, which is the one
 *  question a reader of a command line has: the option takes the token after it when this is more
 *  than zero, and a reader that adds this to its index lands on the next token the parser reads as
 *  an option or a positional.
 *
 *  A required-value option takes the next token whatever it looks like. An optional-value one
 *  takes it only when it does not begin with a dash. A variadic one takes its first value like a
 *  required option and then every further token up to the next dash token. Anything else - a flag,
 *  a `--name=value` token, which is self-contained, a positional, a bare `--` - takes nothing.
 *
 *  A caller that has only an option and one token asks `valuesTakenAt([option, token], 0) > 0`. */
export function valuesTakenAt(argv: readonly string[], index: number): number {
  const option = argv[index] ?? "";
  const next = argv[index + 1];
  if (next === undefined) return 0;
  if (optionValueClasses.required.has(option)) return 1;
  if (optionValueClasses.optional.has(option)) return next.startsWith("-") ? 0 : 1;
  if (!optionValueClasses.variadic.has(option)) return 0;
  let taken = 1;
  while (!(argv[index + taken + 1] ?? "-").startsWith("-")) taken += 1;
  return taken;
}
