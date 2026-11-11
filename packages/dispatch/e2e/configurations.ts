/** The configuration each Playwright project runs its rows in, and the rows a `--list` run names,
 *  so a row that runs twice in one configuration can be found.
 *
 * A configuration is what a repeat is counted by: the engine, the viewport, the origin with its
 * secure context, and how many Dispatch server processes the run has. Two projects may share a
 * configuration without repeating anything - `chromium` and `chromium-alone` do (the projects of
 * those names in e2e/playwright.config.ts), and so do `firefox` and `firefox-download-navigation`
 * - because they run disjoint files. A repeat is the same `(configuration, file, title)` twice,
 * which `duplicateRows` below reports.
 *
 * Nothing here imports Playwright: the values come from a config object the caller passes and
 * from a listing's JSON, so every function is callable from a plain Bun test.
 */

import { valuesTakenAt } from "./playwright-argv";

/** A row of `playwright test --list --reporter=json`: one test of one project. */
export type ListedRow = {
  /** What Playwright expects of the row, as the listing states it before anything runs:
   *  `skipped` for a row skipped where it is declared - `test.skip(title, fn)`, `test.fixme`,
   *  a `describe.skip`, or a describe-level `test.skip(true)` or `test.fixme()` - and `failed`
   *  for a row marked `test.fail()`. A row skipped inside its own body, or by a describe-level
   *  modifier whose condition is a function of a fixture, is `passed` here, because only the
   *  run can know. */
  expectedStatus: string;
  /** The spec file, as the listing names it: relative to the config's root directory. */
  file: string;
  /** The project that runs the row, by `name` in the config. */
  project: string;
  /** The row's title, its `describe` titles first, joined by the character Playwright joins a
   *  title path with. */
  title: string;
};

/** A `(configuration, file, title)` more than one project runs. */
export type Duplicate = {
  /** The configuration key the projects share. */
  configuration: string;
  file: string;
  /** Every project that runs the row, in listing order. */
  projects: string[];
  title: string;
};

/** The `use` values a configuration is read from.
 *
 * `defaultBrowserType` is the engine a `devices[...]` entry carries
 * (playwright-core/types/types.d.ts:18944). Playwright reads `browserName` from it when a project
 * sets no `browserName`, and defaults it to `chromium`
 * (playwright/lib/index.js:187-188), so this module resolves the engine the same way. Playwright's
 * public `TestOptions` does not name `defaultBrowserType`, which is why this type does rather than
 * reusing it. */
export type ConfigurationUse = {
  baseURL?: string;
  browserName?: string;
  defaultBrowserType?: string;
  viewport?: { height: number; width: number } | null;
};

/** As much of a Playwright config as a configuration is read from: the top-level `use` every
 *  project inherits, and the projects that override it key by key. */
export type ProjectsConfig = {
  projects?: readonly { name?: string; use?: ConfigurationUse }[];
  use?: ConfigurationUse;
};

/** The shape of `playwright test --list --reporter=json`'s output that this module reads. */
export type Listing = {
  config?: { projects?: readonly { name?: string }[] };
  suites?: readonly ListingSuite[];
};

type ListingSuite = {
  specs?: readonly ListingSpec[];
  suites?: readonly ListingSuite[];
  title?: string;
};

type ListingSpec = {
  file: string;
  tests?: readonly { expectedStatus?: string; projectName?: string }[];
  title: string;
};

/** Playwright's own viewport when neither the project nor the config sets one: "Defaults to an
 *  1280x720 viewport" (playwright-core/types/types.d.ts:26679). `viewport: null` is a different
 *  thing - no fixed viewport at all - and leaves a project with no configuration here. */
const defaultViewport = { height: 720, width: 1280 };

/** What joins a row's `describe` titles to its own, so two rows of one file whose own titles
 *  match but whose blocks differ stay two rows. Playwright's own reports read this way. */
const titleSeparator = " › ";

/** Whether a page at `origin` is a secure context, which decides what the browser gives it:
 *  `navigator.locks` and `crypto.randomUUID` on a secure one, neither on a plain-HTTP name that is
 *  not loopback, which is what the `chromium-plain-http*` projects are for
 *  (e2e/plain-http-origin.ts:3-9). */
function secureContext(origin: URL): boolean {
  if (origin.protocol === "https:") return true;
  const host = origin.hostname;
  return (
    host === "localhost" ||
    host.endsWith(".localhost") ||
    host === "[::1]" ||
    /^127\.\d+\.\d+\.\d+$/.test(host)
  );
}

/** The configuration `use` puts a project's rows in, or `undefined` when it names no configuration
 *  this can read: no origin to open, an origin that is no URL, or `viewport: null`, which leaves
 *  the window whatever size the browser opens. A project the key cannot be read for is left out of
 *  `projectConfigurations` below, so the test that lists the projects names it. */
function configurationKey(use: ConfigurationUse, processes: number): string | undefined {
  const engine = use.browserName ?? use.defaultBrowserType ?? "chromium";
  const viewport = use.viewport === undefined ? defaultViewport : use.viewport;
  if (viewport === null || use.baseURL === undefined) return undefined;
  let origin: URL;
  try {
    origin = new URL(use.baseURL);
  } catch {
    return undefined;
  }
  const context = secureContext(origin) ? "secure" : "insecure";
  return `${engine} ${viewport.width}x${viewport.height} ${origin.origin} ${context} ${processes}-process`;
}

/** Every project of `config` by name, with the configuration its rows run in. `processes` is how
 *  many Dispatch server processes the run has (e2e/harness-processes.ts), which is also what
 *  chooses the project list (the config's `projects:`, e2e/playwright.config.ts). One listing is
 *  one run, so every key of it carries the same process count and that segment can never tell two
 *  of its rows apart. It is in the key because it is the only place the one-against-two-process
 *  distinction is written down, and a failure prints the key, so the reader sees which run's
 *  projects were compared.
 *
 * A project whose configuration cannot be read, and one with no `name` at all, is left out rather
 * than given a guessed key: the caller sees a project the map has none for. */
export function projectConfigurations(
  config: ProjectsConfig,
  processes: number
): Map<string, string> {
  const configurations = new Map<string, string>();
  for (const project of config.projects ?? []) {
    if (project.name === undefined) continue;
    // A project's `use` overrides the config's key by key, so the configuration is read from the
    // merge, not from either alone: `iphone` takes its viewport from the device it spreads and its
    // origin from the config's top-level `use` above it (e2e/playwright.config.ts).
    const key = configurationKey({ ...config.use, ...project.use }, processes);
    if (key !== undefined) configurations.set(project.name, key);
  }
  return configurations;
}

/** Every project the listing names, whether or not any row fell to it: Playwright reports its
 *  resolved config, so a project whose files are all filtered out is in here too. */
export function listedProjects(listing: Listing): string[] {
  const names = new Set<string>();
  for (const project of listing.config?.projects ?? []) names.add(project.name ?? "");
  return [...names].sort();
}

function collectRows(suite: ListingSuite, titles: readonly string[], rows: ListedRow[]): void {
  for (const spec of suite.specs ?? []) {
    const title = [...titles, spec.title].join(titleSeparator);
    for (const test of spec.tests ?? []) {
      if (test.projectName === undefined) {
        throw new Error(`a listed row of ${spec.file} names no project: ${title}`);
      }
      rows.push({
        expectedStatus: test.expectedStatus ?? "passed",
        file: spec.file,
        project: test.projectName,
        title,
      });
    }
  }
  for (const child of suite.suites ?? []) {
    collectRows(child, [...titles, child.title ?? ""], rows);
  }
}

/** Every row the listing names, one per project a spec runs in. A top-level suite is a file and
 *  its title is that file's path, so a row's title starts at the `describe` blocks below it. */
export function rowsOfListing(listing: Listing): ListedRow[] {
  const rows: ListedRow[] = [];
  for (const file of listing.suites ?? []) collectRows(file, [], rows);
  return rows;
}

/** Each `(configuration, file, title)` that more than one of `rows` holds: the rows that run twice
 *  in one configuration. `keyOf` answers the configuration a row runs in, which keeps this
 *  function free of any knowledge of projects and of Playwright. */
export function duplicateRows<Row extends ListedRow>(
  rows: Iterable<Row>,
  keyOf: (row: Row) => string
): Duplicate[] {
  const seen = new Map<string, Duplicate>();
  for (const row of rows) {
    const configuration = keyOf(row);
    // JSON is the separator-free way to key three strings: no title, file or configuration can
    // run into the next one.
    const id = JSON.stringify([configuration, row.file, row.title]);
    const found = seen.get(id);
    if (found === undefined) {
      seen.set(id, { configuration, file: row.file, projects: [row.project], title: row.title });
      continue;
    }
    found.projects.push(row.project);
  }
  return [...seen.values()].filter((duplicate) => duplicate.projects.length > 1);
}

/** The projects of the stubbed layer, which run a real browser against a recording of the API
 *  (`e2e/stub/`). They are named here rather than in the config so that the config, a bun test
 *  and a tool all read one list: a project added to the layer and missed in the budget test
 *  would leave that test measuring a lane the shard job does not run. */
export const stubProjectNames: readonly string[] = ["stub", "stub-phone"];

/** The projects the pull-request lane runs, which is what
 *  `.github/workflows/dispatch-browser.yaml`'s shard job names for `lane: pull-request`. The
 *  lane is these four and the `@pr` tag; every other project, and every row without the tag,
 *  runs on the full lane at the merge queue and on the nightly. */
export const pullRequestLaneProjects: readonly string[] = [
  "chromium",
  "iphone",
  ...stubProjectNames,
];

/** A title tag as this suite selects one: `@phone`, `@webkit`, `@firefox`, `@pr`. The pattern is
 *  anchored so a tag matches only as a whole whitespace-separated token, never inside a longer
 *  one such as `@phones`, `x@phone` or `@pr-only`. */
export function tagged(tag: string): RegExp {
  return new RegExp(`(^|\\s)@${tag}(\\s|$)`);
}

/** The projects `--project` names on `argv`, read as Playwright reads them, lower-cased.
 *
 *  Playwright's `--project <project-name...>` is variadic, so one flag takes every value up to
 *  the next option: `--project stub chromium` names two. A reader that took one value per flag
 *  would see `stub` alone there and answer that the run is the stubbed layer by itself, which
 *  leaves the `chromium` rows beside it with no stack at all: `globalSetup` starts none for a
 *  run of the stubbed layer alone (`e2e/global-setup.ts`). Playwright matches a project name
 *  without regard to case, so the names come back lower-cased and a caller compares them
 *  against its own lower-case list.
 *
 *  A value holding `*` is a pattern Playwright expands against the config's own projects, which
 *  this cannot see, so it comes back as it was written and a caller reads it as "projects I
 *  cannot name": the conservative answer in both directions is that such a run may hold any
 *  project, so it is given the server and its stacks alike.
 *
 *  An empty list is a run that names no project, which is every DEFAULT project of its mode:
 *  the three wall-clock projects below carry `default: false` and are outside it, measured at
 *  this head as 1,299 rows in 179 files against the whole suite's 1,349 in 181. Every
 *  `--project` of the whole line is read, and the values of every OTHER option are skipped over
 *  (`e2e/playwright-argv.ts`), because a token that reads like a flag of this run may be some
 *  option's value: `--output --project=stub` hands Playwright `--project=stub` as the output
 *  directory and names no project at all, and a reader that took it for one answered "the
 *  stubbed layer alone" for a run holding real-stack rows, which then met no stack
 *  (dispatch://INFRA-1410). A bare `--` ends the options, so no token after it is a project,
 *  whatever it is spelled like: `e2e/playwright-argv.ts`'s own measurements show a command
 *  line that holds one (`-- --ui --list` ran its rows, the `--ui` naming no mode). */
export function projectsOnCommandLine(argv: readonly string[]): string[] {
  const named: string[] = [];
  for (let index = 0; index < argv.length; index += 1) {
    const word = argv[index] ?? "";
    if (word === "--") break;
    if (word.startsWith("--project=")) {
      named.push(word.slice("--project=".length).toLowerCase());
      continue;
    }
    const values = valuesTakenAt(argv, index);
    if (word === "--project") {
      for (let value = 1; value <= values; value += 1) {
        named.push((argv[index + value] ?? "").toLowerCase());
      }
    }
    index += values;
  }
  return named;
}

/** The projects whose rows are read from the wall clock: that a card's draw holds the main thread
 *  for less than a second, measured through `e2e/main-thread-hold.ts`. They run one row at a time
 *  so that a reading is the page's own work rather than what three other lanes took. They are
 *  named here rather than in the config for the reason `stubProjectNames` above gives: the config
 *  selects its own projects with this list, a bun test holds each of them to the fields that keep
 *  them out of the default selection and at one row at a time, and the three modules that account
 *  for the whole suite list these projects beside the default ones, since a listing that names no
 *  project holds no row of theirs once they are out of the default selection. A fourth wall-clock
 *  project added to the config and missed here is a project no accounting sees. The order is the
 *  config's own. */
export const clockProjectNames: readonly string[] = [
  "chromium-alone",
  "firefox-alone",
  "webkit-alone",
];

/** The two specs those projects run, each by the part of its file name that is its own:
 *  `heavy-bodies.e2e.ts` and `nested-directive.e2e.ts`. A wall-clock file is spelled once here
 *  and read twice - by the pattern below, which is how the projects select their files, and by
 *  `clockFileOnCommandLine`, which is how a command that names one of those files with no project
 *  is refused - so a third such file is one edit of this list. */
export const clockSpecNames: readonly string[] = ["heavy-bodies", "nested-directive"];

/** The files the wall-clock projects select, which is `chromium-alone`'s `testMatch` in
 *  `e2e/playwright.config.ts` and the `testIgnore` of the four parallel projects that would
 *  otherwise see them - `chromium`, `iphone`, `webkit` and `firefox`: built from
 *  `clockSpecNames` rather than written a second time, so the two cannot drift apart. The
 *  `.e2e.ts` suffix is part of it, since `heavy-bodies.ts` and `heavy-bodies-page.ts` are
 *  modules those specs import and no project may select them as test files. */
export const clockSpecPattern = new RegExp(`(${clockSpecNames.join("|")})\\.e2e\\.ts`);

/** The wall-clock spec file a Playwright command names as a positional filter while naming no
 *  project, or `undefined` for every other command line. The configuration throws on an answer:
 *  the three wall-clock projects are out of the default selection, so no run that names no
 *  project holds a row of the file such a command named, whatever Playwright then does with the
 *  rest of the line - answer "No tests found", or run the whole default selection, which is what
 *  a filter dropped after a `--` leaves (the post-dash drop below). Neither answer names a cure.
 *
 *  Reading begins after the `test` command word, and a command line with no such word answers
 *  `undefined`. A worker re-evaluates this configuration with Playwright's own argv rather than
 *  the command line (`stubOnlyVariable`'s comment in `e2e/stub/selection.ts`), and `bun test` and
 *  the tools that import this module carry an argv of their own, so none of them can meet the
 *  refusal. The scan ends at the first `--` no option took as its value; a `--` an option DID
 *  take is that option's value, and Playwright reads what follows it as ordinary tokens, so the
 *  scan does too.
 *
 *  How far each option's value reaches is not asked here: `valuesTakenAt`
 *  (`e2e/playwright-argv.ts`) answers how many tokens the option at an index takes, and that
 *  module is this tree's one reader of Playwright's option table and holds the measurements for
 *  every class. The scan steps over what it says an option takes, so a dash token an option did
 *  not take is read as the option or flag it is. Every other token beginning with `-` is a flag
 *  or a spelling that carries its own value, and is passed over. What is left is a positional
 *  filter, which Playwright matches as a regular expression against absolute file paths; this
 *  reader answers such a filter only where its text holds a wall-clock file's whole name, which
 *  is the rule the refusal is written for. A PARTIAL name is left alone: measured at this head,
 *  `bunx playwright test --config e2e/playwright.config.ts --list nested` is not refused here
 *  and exits 1 with Playwright's own "No tests found. Make sure that arguments are regular
 *  expressions matching test files." above "Total: 0 tests in 0 files". That filter does select
 *  a wall-clock file - `nested-directive.e2e.ts` - and this reader says nothing about it.
 *
 *  A user-interface command line answers `undefined`: any `--ui` token and any spelling
 *  beginning `--ui-`, read INSIDE the scan below and before the `valuesTakenAt` step, so that a
 *  `--ui` an option swallowed is no carve-out and a `--ui` behind the `--` is none either. A
 *  wall-clock file the scan has already found is held rather than answered until the scan ends,
 *  so a `--ui` written after the file yields all the same.
 *
 *  Playwright has two listing paths and they read `default: false` differently. A plain command
 *  line leaves the flag unset - `runAllTestsWithConfig` calls
 *  `filterProjects(config.projects, options.projectFilter)`
 *  (`playwright/lib/runner/index.js:7131`), and that function (`:2134`) answers
 *  `projects.filter((project) => includeNonDefault || project.default)` at `:2136` - while the
 *  user interface's own `listFiles` (`:6937`) and `listTests` (`:6971`, under the comment
 *  "Non-default projects are listed so that they can be selected by name.") each set
 *  `includeNonDefaultProjects: true`. So in user-interface mode `chromium-alone` IS listed and
 *  its rows ARE selectable by a file filter: the refusal's premise - that no run naming no
 *  project holds a row of the file - is false there, and the command it prints drops the `--ui`
 *  the reader asked for.
 *
 *  Measured at this head from `packages/dispatch`, each line driven as `timeout 45 bunx
 *  playwright test --config e2e/playwright.config.ts --list <rest>`.
 *
 *  Not refused: `--ui e2e/heavy-bodies.e2e.ts` and `e2e/heavy-bodies.e2e.ts --ui` each reach
 *  the user interface's own path and exit 1 on `ProtocolError: Protocol error
 *  (Browser.getVersion): Internal server error, session closed.`, this container having no X
 *  server, and so do `e2e/claim.e2e.ts --grep -- --ui` and `e2e/heavy-bodies.e2e.ts --grep --
 *  --ui`, where `--grep` takes the `--` as its value and the `--ui` after it is the mode the
 *  held file yields to. `--ui-port=0 e2e/heavy-bodies.e2e.ts` and `--ui-port 0
 *  e2e/heavy-bodies.e2e.ts` each print a listener of their own - `Listening on
 *  http://localhost:45340` and `Listening on http://localhost:29092`, the ports those runs
 *  chose - and hold it open until the 45-second `timeout` killed them, exit 124.
 *
 *  Refused, each exiting 1 with the command this file throws above its stack: `--grep --ui
 *  e2e/heavy-bodies.e2e.ts`, where `--ui` is `--grep`'s value and no mode;
 *  `e2e/heavy-bodies.e2e.ts -- --ui`, where it is behind the `--` and reaches Playwright as
 *  nothing at all; and `--grep -- e2e/heavy-bodies.e2e.ts`, where the `--` is `--grep`'s value,
 *  so the scan steps over it and goes on to the file.
 *
 *  That last line is refused although Playwright would not run the file it names. Playwright
 *  keeps `args.slice(0, args.length - postDashCount)` of its positional filters, where
 *  `postDashCount` counts the tokens after the FIRST `--` of its argv, whether or not an option
 *  took that `--` as its value (`const dashDashIndex = process.argv.indexOf("--")`,
 *  `playwright/lib/program.js:51-53`). With one token after the `--` and one filter, as here,
 *  that keeps no filter. So `--list --output -- e2e/claim.e2e.ts` lists the whole default
 *  selection, 1,299 rows in 179 files, where `--list --output /tmp/o e2e/claim.e2e.ts` lists 1 -
 *  the filter is dropped although `--output` swallowed the `--`, which `--list --output --
 *  --not-a-flag` shows it did, that line exiting 1 with `error: unknown option '--not-a-flag'`
 *  rather than the `error: option '--output <dir>' argument missing` of `--list --output`. What
 *  the run does without its filter depends on the option that took the `--`: `--list --grep --
 *  e2e/heavy-bodies.e2e.ts`, written as a regex this reader does not answer for, reached the
 *  runner with no filter and a grep of `--` and exited 1 with `Error: No tests found` above
 *  `Total: 0 tests in 0 files`, and `--list --output -- 'e2e/heavy.bodies.e2e.ts'` lists the
 *  1,299 rows of the default selection. Neither run reaches the wall-clock file the person
 *  named, so the refusal, and the command it prints, is the useful answer to both.
 *
 *  A reader that took any `--ui` of the whole argv for the mode would answer `undefined` for
 *  the first two of those three, and what the person then gets is the answer this refusal
 *  exists to replace: driven with the same two selections written as a regex this reader does
 *  not answer for, `--list --grep --ui 'e2e/heavy.bodies.e2e.ts'` and `--list
 *  'e2e/heavy.bodies.e2e.ts' -- --ui` each exit 1 with `Error: No tests found.` above `Make
 *  sure that arguments are regular expressions matching test files.`
 *
 *  The carve-out gives up no measurement: a wall-clock row started from the user interface still
 *  meets `refuseASharedRun` (`e2e/main-thread-hold.ts`), which fails a measuring row whenever
 *  the run is wider than one worker, so such a run still reports the page's own work or no
 *  reading at all.
 *
 *  The project question is `projectsOnCommandLine`'s, so `--project` is read once in this
 *  module; the scan meets it as an option whose values `valuesTakenAt` steps over, which is all
 *  it needs of a flag whose values, when it has any, have already answered this function above.
 *  The rule the reader follows is Playwright's own parser's: it answers the token that parser
 *  reads as a positional filter, and nothing else - with one exception, the post-dash drop
 *  above, where such a token reaches the runner as no filter and the reader refuses a command
 *  Playwright would run, because the file it names is not what that run would run. A reader
 *  that read more than that refuses commands that DO run the files they name - `--grep
 *  heavy-bodies`, `--reporter list e2e/claim.e2e.ts`, `--shuffle e2e/heavy-bodies.e2e.ts`,
 *  whose path is a seed - which is the failure mode that matters here. */
export function clockFileOnCommandLine(argv: readonly string[]): string | undefined {
  const command = argv.indexOf("test");
  if (command === -1) return undefined;
  if (projectsOnCommandLine(argv).length > 0) return undefined;
  let file: string | undefined;
  for (let index = command + 1; index < argv.length; index += 1) {
    const word = argv[index] ?? "";
    // The separator, read INSIDE the scan for the reason `projectsOnCommandLine` reads it there:
    // a `--` the loop reaches is one no option took, because the `valuesTakenAt` step below has
    // already stepped over every option's value. A `--` found flat over the whole argv is not
    // that one: `--grep -- <file>` hands Playwright the `--` as `--grep`'s value and goes on
    // parsing what follows, so a scan that stopped at that `--` would read neither the file
    // after it nor a `--ui` written past it.
    if (word === "--") break;
    // The user-interface carve-out above, read here rather than over the whole argv: a token an
    // option swallowed is that option's value and no mode, and the scan has already stopped at
    // the `--` above. It is read before the `valuesTakenAt` step because `--ui-host` and
    // `--ui-port` take a value of their own and are user-interface spellings all the same.
    if (word === "--ui" || word.startsWith("--ui-")) return undefined;
    const values = valuesTakenAt(argv, index);
    if (values > 0) {
      index += values;
      continue;
    }
    if (word.startsWith("-")) continue;
    // The first such filter is held rather than answered, so that a `--ui` after it still wins.
    if (file === undefined && clockSpecNames.some((name) => word.includes(name))) file = word;
  }
  return file;
}
