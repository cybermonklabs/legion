/** What each `playwright test` option takes of the tokens after it, read the way the runner's own
 *  parser reads argv.
 *
 *  Two readers in this tree ask it, both in `e2e/configurations.ts`. `projectsOnCommandLine`
 *  asks about a DASH token: whether a `--project` is an option of the run or another option's
 *  value. `clockFileOnCommandLine` asks about both kinds - whether a `--ui` is the mode it
 *  spells or an option's value, and whether a NON-DASH token is a positional filter or an
 *  option's value. A flat `argv.some()` answers either question wrongly, because a flat argv
 *  cannot tell an option from an option's VALUE: `--output --project=stub` hands Playwright
 *  `--project=stub` as the output directory and names no project at all, and `--grep --ui`
 *  hands it `--ui` as `--grep`'s value and the run is an ordinary one (dispatch://INFRA-1410).
 *  Both readers take the classification below and `valuesTakenAt` rather than carrying a rule
 *  of their own.
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
 *  A NON-DASH target, measured in this tree, where the target is a spec path and the reading is
 *  the listing it produced. These two rows are the evidence for the other half of the OPTIONAL
 *  rule - what such an option does with a token that does NOT begin with a dash
 *  (dispatch://INFRA-1280) - which is the half `clockFileOnCommandLine` rests on, and
 *  `e2e/__tests__/clock-selection.test.ts` holds a row of its own for each of them, "a
 *  wall-clock path behind `--shuffle`, which Playwright takes as the seed" and "a wall-clock
 *  path behind `--only-changed`, which Playwright takes as a git ref".
 *
 *    --list --shuffle e2e/claim.e2e.ts       listed 1,493 tests in 204 files, exit 0: the
 *                                            default selection, the path taken as the seed and
 *                                            no file filtered
 *    --list --only-changed e2e/claim.e2e.ts  listed 0 tests in 0 files, exit 1, `--only-changed
 *                                            <path>` printing `Cannot detect changed files for
 *                                            --only-changed mode: git diff <path> --name-only`
 *
 *  Those 1,493 rows are what a listing that names no project holds, and not the whole suite:
 *  the six `-alone` projects carry `default: false` (`clockProjectNames`,
 *  `e2e/configurations.ts`) and stand outside it. Measured at this head against
 *  `e2e/playwright.config.ts`, `--list` alone lists `Total: 1493 tests in 204 files`, `--list`
 *  naming `chromium-alone`, `firefox-alone`, `webkit-alone`, `iphone-alone`,
 *  `iphone-measure-alone` and `iphone-long-build-alone` lists
 *  `Total: 59 tests in 7 files`, and `--list` naming all eighteen projects of the config lists
 *  `Total: 1552 tests in 210 files`, which is the suite.
 *
 *  Together the two sets of readings settle every class. A REQUIRED-value option takes the next
 *  token whatever it looks like, a `--ui` included, which is why `--grep --ui` lists rows. An
 *  OPTIONAL-value option leaves a token that begins with a dash ALONE, which is why `--debug
 *  --ui` opens the UI - and that half is the one that matters here: it is why `--shuffle
 *  --project chromium` names `chromium` to `projectsOnCommandLine`, rather than swallowing the
 *  `--project` and answering that the run names no project
 *  (`e2e/__tests__/projects-on-command-line.test.ts` holds the row). It TAKES a token that does
 *  not begin with a dash, which is why `--shuffle e2e/claim.e2e.ts` shuffles by that path and
 *  filters no file. That is the half `clockFileOnCommandLine` rests on, and the two rows named
 *  above are its evidence. A VARIADIC option (`--project <project-name...>`) behaves as a
 *  required one for its first value and stops at the next dash token, so `--project --ui` reads
 *  `--ui` as a project name and `--project chromium --ui` leaves `--ui` a flag.
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
 *  `e2e/__tests__/playwright-argv.test.ts` runs that help and asserts all three sets against
 *  it, so a raise of Playwright that adds, renames, drops or RECLASSIFIES one of these fails a
 *  test rather than silently moving where some command line's values end a year later. */
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
 *  a `--name=value` token, which is self-contained, a positional, a bare `--` - takes nothing. */
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
