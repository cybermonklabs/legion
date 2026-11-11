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
 *  waives the harness refusals and the preflight for the `chromium` rows beside it. Playwright
 *  matches a project name without regard to case, so the names come back lower-cased and a
 *  caller compares them against its own lower-case list.
 *
 *  A value holding `*` is a pattern Playwright expands against the config's own projects, which
 *  this cannot see, so it comes back as it was written and a caller reads it as "projects I
 *  cannot name": the conservative answer in both directions is that such a run may hold any
 *  project, so nothing is waived for it.
 *
 *  An empty list is a run that names no project, which is every project of its mode. Every
 *  `--project` of the whole line is read, and a `--` ends one flag's values because it begins
 *  with `-`: `e2e/run.ts` reads the Playwright argv it passes after its own `--`, and the
 *  config reads a Playwright command line that has no `--` in it. */
export function projectsOnCommandLine(argv: readonly string[]): string[] {
  const named: string[] = [];
  for (const [index, word] of argv.entries()) {
    if (word.startsWith("--project=")) {
      named.push(word.slice("--project=".length).toLowerCase());
      continue;
    }
    if (word !== "--project") continue;
    for (const value of argv.slice(index + 1)) {
      if (value.startsWith("-")) break;
      named.push(value.toLowerCase());
    }
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

/** The files the wall-clock projects select, which `e2e/playwright.config.ts`'s `clockSpecs` is:
 *  built from `clockSpecNames` rather than written a second time, so the two cannot drift apart.
 *  The `.e2e.ts` suffix is part of it, since `heavy-bodies.ts` and `heavy-bodies-page.ts` are
 *  modules those specs import and no project may select them as test files. */
export const clockSpecPattern = new RegExp(`(${clockSpecNames.join("|")})\\.e2e\\.ts`);

/** The options of `playwright test` that take the token after them whatever that token is, which
 *  is every option the pinned Playwright declares with a REQUIRED value. The table is Playwright's
 *  own: `node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/program.js`, the
 *  `testOptions` array at line 188, where a required value is written `<file>` and an optional one
 *  `[mode]`. `addTestCommand` at line 37 builds the test command's options from that array and
 *  adds none of its own, so `-c, --config <file>` is the array's entry at line 192; the identical
 *  declaration at line 74 is `clear-cache`'s. Both spellings of an option stand here, the long one
 *  and, where the table gives one, the short, because `-g value` skips its value as `--grep value`
 *  does. `e2e/__tests__/clock-selection.test.ts` reads that table again and fails when either set
 *  has gone stale, so a raise of Playwright is read here rather than guessed at.
 *
 *  "Whatever that token is" is commander's own rule, measured at Playwright 1.64.1 in this
 *  workspace against a scratch config that refuses nothing, over a git repository of two spec
 *  files (`a.spec.ts` changed in the working copy, `heavy-bodies.e2e.ts` committed): `playwright
 *  test --config <scratch>/playwright.config.ts --grep --list` printed "Error: No tests found" and
 *  exited 1, listing no row at all, because `--list` became the grep rather than list mode.
 *
 *  `--project <project-name...>` is variadic and stands here all the same: the project question is
 *  `projectsOnCommandLine`'s, which the reader below asks before it scans, so all the scan needs
 *  of `--project` is that its first value is no positional filter.
 *
 *  The `--name=value` and `-j4` spellings carry their own value and stand nowhere here: they begin
 *  with `-`, which the reader below passes over. */
const optionsTakingTheNextToken: ReadonlySet<string> = new Set([
  "--add-reporter",
  "--browser",
  "-c",
  "--config",
  "--global-timeout",
  "-g",
  "--grep",
  "-G",
  "--grep-invert",
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
  "-j",
  "--workers",
]);

/** The options that take the token after them only when that token does not begin with `-`, which
 *  is every option the pinned table declares with an OPTIONAL value: `--debug [mode]`,
 *  `--only-changed [ref]`, `--shuffle [seed]` and `-u, --update-snapshots [mode]`. The asymmetry
 *  with the set above is commander's, and both halves of it are measured at Playwright 1.64.1
 *  against the scratch repository named above.
 *
 *  A token that is no dash IS taken, and the detached spelling is the attached one:
 *  `--list --only-changed <scratch>/specs/heavy-bodies.e2e.ts` and
 *  `--list --only-changed=<scratch>/specs/heavy-bodies.e2e.ts` printed the same failure, "Cannot
 *  detect changed files for --only-changed mode: git diff
 *  /legion/scratch/tmp/.../specs/heavy-bodies.e2e.ts --name-only", so the path became the option's
 *  own value - a git ref - in the detached form exactly as in the attached one. The same reading
 *  in the real suite: `--list --shuffle e2e/claim.e2e.ts` lists the whole suite, the path having
 *  become the seed, and `--list --debug e2e/claim.e2e.ts` exits 1 with "option '--debug [mode]'
 *  argument 'e2e/claim.e2e.ts' is invalid. Allowed choices are inspector, cli."
 *
 *  A DASH token is not taken: `--only-changed --list` and `--shuffle --list` both reached list mode
 *  and printed rows - 1 test in 1 file, the changed spec, and 2 tests in 2 files - and
 *  `--only-changed -x --list` listed that one row too, so neither `--list` nor `-x` became a value.
 *  That is the whole difference from the required side, where `--grep --list` took `--list`. */
const optionsTakingTheNextTokenUnlessItBeginsWithDash: ReadonlySet<string> = new Set([
  "--debug",
  "--only-changed",
  "--shuffle",
  "-u",
  "--update-snapshots",
]);

/** The wall-clock spec file a Playwright command names as a positional filter while naming no
 *  project, or `undefined` for every other command line. The configuration throws on an answer:
 *  the three wall-clock projects are out of the default selection, so such a command selects no
 *  row at all and Playwright answers "No tests found", which names no cure.
 *
 *  Reading begins after the `test` command word, and a command line with no such word answers
 *  `undefined`. A worker re-evaluates this configuration with Playwright's own argv rather than
 *  the command line (`e2e/stub/selection.ts:43-47`), and `bun test` and the tools that import this
 *  module carry an argv of their own, so none of them can meet the refusal. Nothing after a `--`
 *  is read. An option of `optionsTakingTheNextToken` takes the token after it whatever it is; an
 *  option of `optionsTakingTheNextTokenUnlessItBeginsWithDash` takes it only when it begins with
 *  no `-`, and a dash token behind such an option is read as the option or flag it is. Every other
 *  token beginning with `-` is a flag or a spelling that carries its own value, and is passed
 *  over. What is left is a positional filter, which Playwright matches as a regular expression
 *  against absolute file paths, so the name of a wall-clock file anywhere in one selects that
 *  file.
 *
 *  The project question is `projectsOnCommandLine`'s, so `--project` is read once in this module;
 *  the scan meets it as an option that takes a token, which is all it needs of a flag whose
 *  values, when it has any, have already answered this function above. The rule the reader follows
 *  is Playwright's own parser's: it answers the token that parser would call a positional filter,
 *  and nothing else. A reader that read more than that refuses commands Playwright runs - `--grep
 *  heavy-bodies`, `--reporter list e2e/claim.e2e.ts`, `--shuffle e2e/heavy-bodies.e2e.ts`, whose
 *  path is a seed - which is the one failure mode that matters here. */
export function clockFileOnCommandLine(argv: readonly string[]): string | undefined {
  const command = argv.indexOf("test");
  if (command === -1) return undefined;
  if (projectsOnCommandLine(argv).length > 0) return undefined;
  const separator = argv.indexOf("--", command + 1);
  const scanned = argv.slice(command + 1, separator === -1 ? undefined : separator);
  let pendingValue: "required" | "optional" | undefined;
  for (const word of scanned) {
    const takesThisToken = pendingValue;
    pendingValue = undefined;
    if (takesThisToken === "required") continue;
    if (takesThisToken === "optional" && !word.startsWith("-")) continue;
    if (optionsTakingTheNextToken.has(word)) {
      pendingValue = "required";
      continue;
    }
    if (optionsTakingTheNextTokenUnlessItBeginsWithDash.has(word)) {
      pendingValue = "optional";
      continue;
    }
    if (word.startsWith("-")) continue;
    if (clockSpecNames.some((name) => word.includes(name))) return word;
  }
  return undefined;
}
