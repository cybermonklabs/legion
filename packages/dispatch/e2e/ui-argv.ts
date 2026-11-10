/** Whether a `playwright test` command line names a UI session, read the way the runner's own
 *  parser reads argv.
 *
 *  Playwright enters UI mode on any of three flags - `--ui`, `--ui-host <host>` and
 *  `--ui-port <port>` (`opts.ui || opts.uiHost || opts.uiPort`,
 *  node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/cli/testActions.js:60,
 *  declared at lib/program.js:223-225) - and `e2e/playwright.config.ts` refuses such a session
 *  when no stack is up. A flat `process.argv.some()` over those spellings refused runs that were
 *  no UI sessions at all, because a flat argv cannot tell an option from an option's VALUE:
 *  `--grep --ui` hands Playwright `--ui` as `--grep`'s value, and the run is an ordinary one
 *  (dispatch://INFRA-1410).
 */

/** Every `playwright test` option that takes the NEXT argument as its value, long spelling and
 *  short spelling alike, read from `bunx playwright test --help` at playwright 1.64.1. The help
 *  writes such an option `--name <value>`, and both spellings on one line where it has a short
 *  one (`-g, --grep <grep>`), which is where the short names below come from. An option the help
 *  writes `--name [value]` takes the next argument only when that argument is no option, and one
 *  the help writes with neither takes none.
 *
 *  `e2e/__tests__/ui-argv.test.ts` runs that help and asserts this set against it, both halves of
 *  it, so a raise of Playwright that adds, renames or drops one of these fails a test rather than
 *  refusing an ordinary run a year later. */
export const optionsTakingTheNextArgument: ReadonlySet<string> = new Set([
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
  "--project",
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
]);

/** Whether `argv` names a UI session.
 *
 *  The scan runs left to right, as the runner's parser does. An option of the set above swallows
 *  the token after it, so that token is a value and not a flag. A bare `--` ends the options, so
 *  every token after it is a test-file filter. Any other token that is exactly `--ui` or begins
 *  `--ui-` is a UI flag, the `--ui-port=0` spelling included: that argument is self-contained and
 *  needs no lookahead.
 *
 *  Measured at playwright 1.64.1 against a configuration that refuses nothing and holds one
 *  trivial row, each command `bunx playwright test --config <that config> <arguments>`:
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
 *  Those readings settle the two questions the set above cannot answer by itself. A
 *  REQUIRED-value option takes the next token whatever it looks like, a `--ui` included, which is
 *  why `--grep --ui` lists rows. An OPTIONAL-value option (`--debug [mode]`,
 *  `-u, --update-snapshots [mode]`, `--shuffle [seed]`, `--only-changed [ref]`) leaves a token
 *  that begins with a dash alone, which is why `--debug --ui` opens the UI, and those four are
 *  out of the set for that reason. A VARIADIC option (`--project <project-name...>`) behaves as a
 *  required one for its first value and stops at the next dash token, so `--project --ui` reads
 *  `--ui` as a project name and `--project chromium --ui` leaves `--ui` a flag. */
export function namesUiSession(argv: readonly string[]): boolean {
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index] ?? "";
    if (argument === "--") return false;
    if (argument === "--ui" || argument.startsWith("--ui-")) return true;
    if (optionsTakingTheNextArgument.has(argument)) index += 1;
  }
  return false;
}
