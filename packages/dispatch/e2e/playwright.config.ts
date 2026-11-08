import { fileURLToPath } from "node:url";
import { defineConfig, devices, type Project } from "@playwright/test";
import { baseUrl } from "./api";
import { targetedListeners } from "./harness-listeners";
import {
  harnessPorts,
  harnessVariables,
  laneCount,
  laneCountVariable,
  workerCount,
  workerCountVariable,
} from "./harness-ports";
import { harnessProcessCount } from "./harness-processes";
import { harnessSignIn } from "./harness-signin";
import { measuring } from "./measure";
import { plainHttpHost, plainHttpOrigin } from "./plain-http-origin";

// The plain-HTTP project's origin is a proxy on PLAIN_HTTP_PORT, reached by a host name Chromium
// maps to loopback. The page is a non-loopback plain-HTTP origin, while the proxy forwards to the
// dev sign-in server under its own loopback origin. `plainHttpSpecs` are the files that project
// runs, the origin's own rows and the proxy's; `chromium` and `iphone` ignore them, since they
// open the plain-HTTP name, which only that project maps, and the origin rows' first assertion
// fails on loopback by design.
const plainHttpSpecs = /plain-http-(origin|proxy)\.e2e\.ts/;
// The `iphone`, `webkit` and `firefox` projects run only the rows tagged `@phone`, `@webkit` and
// `@firefox`, written at the end of the test's title. Which tests take `@phone` is in
// `packages/dispatch/AGENTS.md`, "Phone acceptance"; an engine tag names a row for one of two
// reasons, and a comment above the row says which. Either the row exercises one engine behaviour
// the comment above those projects lists - the usual reason - or every measured failure of that
// row was in that engine, so the row is kept where its cause lives: a row green only on an engine
// its defect was never seen on proves nothing about that defect. Two rows carry a tag for the
// second reason today, each named in `packages/dispatch/AGENTS.md`: `keyed-writes.e2e.ts`'s ask
// answer whose answer is lost (two timeouts in 213 CI runs, both in WebKit) and
// `keyboard-agents-picker.e2e.ts`'s row back after the reader left mid-send (six failures on six
// branches, every one in WebKit). A row that only repeats an assertion Chromium already made, and
// whose failures are not an engine's, carries no tag. An untagged
// test drops out with no failure. The pattern is anchored so a tag matches only as a whole
// whitespace-separated token, never inside a longer one such as `@phones`, `x@phone`,
// `@webkit-iphone` or `@firefox-alone`.
function tagged(tag: string): RegExp {
  return new RegExp(`(^|\\s)@${tag}(\\s|$)`);
}
const phoneTagged = tagged("phone");
const webkitTagged = tagged("webkit");
const firefoxTagged = tagged("firefox");
// Specs of two Dispatch processes behind the round-robin proxy (e2e/round-robin-proxy.ts), each
// named `*.multiprocess.e2e.ts`. The default run ignores them, since they need both processes and
// the proxy's own routes; two-process mode (DISPATCH_E2E_PROCESSES=2, `bun run e2e:multiprocess`)
// runs them alone, in Chromium, since every other spec runs against one server already.
const multiprocessSpecs = /\.multiprocess\.e2e\.ts$/;
// The one spec of DISPATCH_E2E_SIGNIN=oidc (e2e/harness-signin.ts), whose servers sign people in
// through the dev OIDC issuer alone: it runs only then, and every other multiprocess spec, which
// signs in through dev sign-in, runs only without it.
const signInSpecs = /\/signin\.multiprocess\.e2e\.ts$/;
if (harnessProcessCount === 2 && process.env.PLAYWRIGHT_BASE_URL) {
  throw new Error(
    "DISPATCH_E2E_PROCESSES=2 runs against two Dispatch servers of this run's own behind the " +
      "round-robin proxy, so it cannot run against PLAYWRIGHT_BASE_URL. Unset one of them."
  );
}

// The rows of e2e/harness-listeners.ts this run targets, as the environment names them when this
// config evaluates.
const targeted = targetedListeners();

// `DISPATCH_E2E_REUSE_SERVERS=1` says the harness ports name servers that are already running:
// the ones e2e/run.ts started, which sets it for the command it runs, or a harness a person
// started and left listening. Any other value is refused rather than quietly read as "no".
function resolveReuseServers(): boolean {
  const requested = process.env.DISPATCH_E2E_REUSE_SERVERS;
  if (requested === undefined || requested === "") return false;
  if (requested === "1") return true;
  throw new Error(
    `DISPATCH_E2E_REUSE_SERVERS must be "1" or unset, not ${JSON.stringify(requested)}. ` +
      "Run the suite through e2e/run.ts, which sets it for the harness it starts, or set it to 1 " +
      "to run against a harness you started yourself."
  );
}

// A listing (`--list`) starts no server and runs no spec, so it needs no harness: it is read from
// argv before a `--` separator, and yields to a UI token, since `--ui`/`--ui-*` outrank the
// computed list mode (`lib/cli/testActions.js:52`, `:62`) and a UI session runs specs. The test is
// loose (any `--list`, whatever precedes it), and Playwright reads the `--list` of `--grep --list`
// or `--grep-invert --list` as the option's value, which is a real run. So a listing is exempt only
// from the unset-ports refusal: with every port unset nothing is targeted, and such a run fails at
// e2e/preflight.ts; with any port set it meets the reuse refusal below like any run.
function isListMode(argv: readonly string[]): boolean {
  if (argv.some((arg) => arg === "--ui" || arg.startsWith("--ui-"))) return false;
  const separator = argv.indexOf("--");
  const scanned = separator === -1 ? argv : argv.slice(0, separator);
  return scanned.includes("--list");
}

// How many workers the command line asks for, if it does: `--workers <n>`, `--workers=<n>`,
// `-j <n>` or `-j<n>`, read before a `--` separator as the list mode above is. A worker is a
// lane, and e2e/run.ts started one harness per lane before Playwright, so a command line that
// asks for another number is refused rather than quietly leaving a lane unused or putting two
// workers on one lane's servers.
function workersOnCommandLine(argv: readonly string[]): string | undefined {
  const separator = argv.indexOf("--");
  const scanned = separator === -1 ? argv : argv.slice(0, separator);
  for (const [index, argument] of scanned.entries()) {
    if (argument === "--workers" || argument === "-j") return scanned[index + 1];
    if (argument.startsWith("--workers=")) return argument.slice("--workers=".length);
    if (argument.startsWith("-j") && argument.length > 2) return argument.slice(2);
  }
  return undefined;
}

// This config starts no server. e2e/run.ts starts the harness (e2e/harness-start.ts) on ports
// the OS chooses, exports the ports the servers report, and only then runs Playwright, so this
// module, every worker that re-imports it, and every helper that reads a port at import see the
// real ports when they evaluate. Without it, the run targets servers a person started: every
// port set and DISPATCH_E2E_REUSE_SERVERS=1, since e2e/seed.ts truncates the database those
// servers hold. Anything else is refused here, before a single spec runs, but a listing with every
// port unset; `harnessPorts()` also refuses a malformed or duplicated port, a listing's included.
const runThroughWrapper =
  "bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts [args] (`bun run " +
  "e2e` builds the SPA and runs the same)";
const ports = harnessPorts();
const reuseServers = resolveReuseServers();
const unset = targeted.filter((listener) => ports[listener.key].port === 0);
const listing = isListMode(process.argv);
if (!listing && unset.length > 0) {
  throw new Error(
    `The Dispatch e2e harness is not running: ${unset.map((listener) => harnessVariables[listener.key]).join(", ")} ` +
      `${unset.length > 1 ? "are" : "is"} unset. This config starts no server; run it through ` +
      `e2e/run.ts, which starts the harness on ports the OS chooses and exports them: ` +
      `${runThroughWrapper}. To run against servers you started yourself, set ` +
      `${targeted.map((listener) => harnessVariables[listener.key]).join(", ")} and ` +
      "DISPATCH_E2E_REUSE_SERVERS=1."
  );
}
if (!reuseServers && (!listing || unset.length < targeted.length)) {
  const targets = targeted
    .map((listener) => `${ports[listener.key].port} (${harnessVariables[listener.key]})`)
    .join(", ");
  throw new Error(
    `This config starts no server, so it would run against whatever listens on ${targets}. Run ` +
      `it through e2e/run.ts, which starts the harness: ${runThroughWrapper}. Or set ` +
      "DISPATCH_E2E_REUSE_SERVERS=1 to run against servers you started yourself, whose database " +
      "this run truncates."
  );
}

// One worker per lane, and the lanes are e2e/run.ts's: a worker resolves its own harness from
// `TEST_PARALLEL_INDEX` (e2e/harness-ports.ts), so a run with more workers than lanes would put
// two workers on one lane's servers and database, which e2e/seed.ts truncates per row.
const lanes = laneCount();
const asked = workersOnCommandLine(process.argv);
if (asked !== undefined && asked !== String(lanes)) {
  throw new Error(
    `This run has ${lanes} lane${lanes === 1 ? "" : "s"}, each a harness of its own, so ` +
      `--workers ${asked} would not match: a worker reads the lane of its own index. Set ` +
      `${workerCountVariable}=${asked} instead, which starts that many lanes and runs that many ` +
      "workers."
  );
}

// `laneCount` answers the worker count where `DISPATCH_E2E_LANES` is unset, so that a run against
// servers a person started reads its own. That fallback is right for one worker and wrong for
// more: nothing exported a lane, so every worker would resolve lane 0 and share one harness's
// servers and database, which e2e/seed.ts truncates per row - rows would delete each other's
// fixtures and fail in ways that read as product faults. Only e2e/run.ts exports that variable.
if (process.env[laneCountVariable] === undefined && workerCount() > 1) {
  throw new Error(
    `${workerCountVariable}=${workerCount()} asks for more than one worker, but nothing exported ` +
      `${laneCountVariable}, so there is one harness and every worker would share it. Run this ` +
      `config through e2e/run.ts, which starts a lane per worker and exports that count: ` +
      `${runThroughWrapper}.`
  );
}

// The default wait for asynchronous server and rendering readiness. A spec only sets its own
// timeout when that deadline is its observable contract (the keyboard chord-expiry test).
const expectTimeout = 15_000;

// The per-test budget, measured rather than inherited. Playwright's own default is 30 s, chosen
// when this suite ran one row at a time; it now runs four lanes on a four-core runner. Read from
// the report.json artifact of 201 ci.yaml runs between 2026-11-05T08:56:19Z and
// 2026-11-05T20:32:13Z (78 merge_group, 119 pull_request, 4 push), at base b7866f96d7fe.
//
// The population is the one this number governs, and the filter is the point of it: the results
// that passed, in that window, on rows of the spec files this tree has that ran under the
// inherited budget rather than one of their own. The filter is per row, not per file, because
// 29 of the tree's 32 `test.setTimeout`/`test.slow()` calls sit inside a single row: a file is
// not out of the population, a row is, and the other rows of `keyed-writes`, `live-socket`,
// `phone`, `refetch-budget`, `claim-refilling`, `composer-body-cap`, `machine-login` and
// `threads-order` are governed by this budget whatever their one slow neighbour asks for. Only
// `prefix-links`, `reference-cards` and `reference-titles` set a timeout for every row they hold.
//
// Derive it from the measurement rather than from the sources: a report carries, per row, the
// timeout that row ran under, so a result taken at its project's own timeout is one this budget
// governed, and the filter cannot age. A source-level filter cannot be written as one line - a
// call inside a row's body is the row's and a call at module scope is the file's, which is an AST
// question, and `--list` reads module scope only, so it misses all 29.
//
//   jq -r '(.config.projects|map({key:.name,value:.timeout})|from_entries) as $c
//     | [..|objects|select(has("specs"))|.specs[]][] | . as $s | $s.tests[]
//     | select(.timeout == $c[.projectName]) | .results[] | select(.status=="passed")
//     | [($s.file|sub(".*/";"")), .projectName, .duration] | @tsv' <report.json…>
//
// then keep the rows of files `ls e2e/*.e2e.ts` lists, which drops the window's results of files
// this tree does not have - 1,560 of them in 12 files, other branches' rows. Compare `passed`
// results alone: a skipped row reports the static timeout, never the one its body would have set.
//
// Over that population: 320,101 results, median 2,543 ms, 95th percentile 6,773 ms, 99th
// 9,886 ms, 99.9th about 15,097 ms (15,056 by floor, 15,101 by nearest rank), maximum 29,856 ms,
// in deep-links.e2e.ts's "emitted document item hrefs select and scroll their anchored thread" on
// WebKit. Over the whole of what this tree has, rows with their own timeout included, the
// maximum is 126,560 ms, and the 21 results above this budget are all in three rows of
// agent-list-refilling.e2e.ts, whose rows ask for 150 s to 300 s themselves.
//
// What the window bounds, and what it does not. The population is passing results, and every
// governed row of the window ran under Playwright's 30 s default, so a row that wanted longer was
// killed rather than measured: 27 governed results timed out at that cap and are not in the
// population at all - 19 in pictures.e2e.ts on chromium, 5 in deep-links.e2e.ts on firefox and
// one on WebKit, which is the very row that set the figure below, and 2 in keyed-writes.e2e.ts on
// WebKit, each ending between 30,000 and 30,006 ms. So 29,856 ms is the slowest result that
// survived the cap, not the distribution's maximum, and the tail above 30 s is unknown.
//
// The budget does not rest on that figure being a maximum. It is sized to let a healthy row
// finish, and the distribution of rows that did finish says what healthy costs: 60 s is 2.01
// times the slowest survivor, 4.0 times the 99.9th percentile and 24 times the median, no
// governed result of the window reaches it, and nothing in the window shows a row finishing its
// work and needing more. A censored tail changes what the maximum means, not what the budget has
// to cover; a row that hangs still fails inside a minute.
// It lengthens no row's work; a row that needs it has already lost whatever it was waiting for.
// Raise it only from a measurement of the same shape, over the same population.
const testTimeout = 60_000;

// Two-process mode's one project: the multiprocess specs, in Chromium, the sign-in spec alone under
// DISPATCH_E2E_SIGNIN=oidc and every other one without it.
//
// `retries: 0`, against the one retry the run takes on CI, and for a reason of its own: the
// `-alone` projects below also set it, but their argument is about the wall clock and does not
// reach here. This project's argument is that nothing publishes its flaky rows. The job that
// runs it (.github/workflows/dispatch-browser.yaml's multiprocess job) writes no blob report,
// and the merge job that reports the run's flake rate reads the shard and alone legs' blobs
// alone, so a row that failed and passed on a retry here would appear in no rate and no list.
// Its test-results upload is `if: failure()`, so a retried row would leave no trace either. With
// no retry the job stays what it has always been: a failed row fails it, the upload runs, and
// the trace of that one attempt is kept (`trace: 'retain-on-first-failure'` below). A blob leg
// for this job would end the argument, and there is none.
const multiprocessProjects = [
  {
    name: "chromium",
    testMatch: harnessSignIn === "oidc" ? signInSpecs : multiprocessSpecs,
    testIgnore: harnessSignIn === "oidc" ? undefined : signInSpecs,
    use: { ...devices["Desktop Chrome"] },
    retries: 0,
  },
];

// The specs whose rows are read from the wall clock: that a card's draw holds the main thread
// for less than one second (`heavy-bodies.e2e.ts` and `nested-directive.e2e.ts`, both through
// `main-thread-hold.ts`). Four lanes on one machine leave such a claim unmeasurable - the wall
// time of a draw holds what the other three lanes took as surely as the work the page did -
// so these files run in the `-alone` projects at the end of this list, after every other project
// and one row at a time, and in no project above them (sjawhar/legion#2612).
const clockSpecs = /(heavy-bodies|nested-directive)\.e2e\.ts/;

// The rows read from the wall clock that live in a file whose other rows are not: the three of
// `large-documents.e2e.ts` that the spec's own targets are read from - the two measurements of a
// first editable block under 4x CPU throttling, and the row that times a page timer and a socket
// message handed over inside the editor's build. The argument above `clockSpecs` is theirs too,
// and CI has now said it twice: at one head the two measurements read 4,824 ms and 8,311 ms on
// their first attempt and 3,838 ms and 5,105 ms on the retry, with each attempt's network reads
// the same or slower and every main-thread number halved, which is the other lanes easing rather
// than any cache warming; at the same head the timer row's worst wait was 8,279 ms behind a build
// whose own longest task was 732 ms. A row that differs from itself by three seconds on one
// runner measures the runner. So these three run in `iphone-alone` below, one at a time, after
// every lane and every other `-alone` project, and in no project above it. Their file's other
// rows stay where they are, which is why this selects by row rather than by file.
const clockRows =
  /(measures .* on a phone under 4x CPU throttling|a page timer and a socket message are served while .*'s editor is built)/;

// The one spec of the containerised visual review (`visual.e2e.ts`), which runs in the `visual`
// and `visual-phone` projects at the end of this list and in no project above them: a project
// whose configuration key equals one of theirs - the same engine, viewport, origin and
// server-process count - would run the same row twice, which
// e2e/__tests__/row-configurations.test.ts refuses. `iphone` is that project for `visual-phone`,
// which spreads the same device, and the three other projects that name a `testIgnore` name
// this one too for the reason given above `webkit`: without a `testMatch` a project sees every
// spec in this directory, so the ignore is what keeps this file to its own two projects.
const visualSpecs = /visual\.e2e\.ts$/;

// Every project that runs four lanes wide. The `-alone` projects below name all of them, so a
// project added here is one more the clock rows wait for.
const parallelProjects = [
  {
    name: "chromium",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs, visualSpecs],
    grepInvert: clockRows,
    use: { ...devices["Desktop Chrome"] },
  },
  {
    name: "iphone",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs, visualSpecs],
    grep: phoneTagged,
    grepInvert: clockRows,
    use: { ...devices["iPhone 13"], browserName: "chromium" as const },
  },
  // WebKit runs the rows a WebKit decision can change, and nothing else: it selects by the
  // `@webkit` tag, so each row it runs carries that tag and a comment naming the clause below it
  // exercises. Every clause here names one engine behaviour and the rows that exercise it.
  // Where a caret lands beside a collaborator's own caret: `collab-cursor`'s three rows.
  // A closed select's `change` dispatched inside the key's own task, which the Agents issue
  // picker's and the live header pickers' keyboard-step rule rests on: the six rows of
  // `keyboard-agents-picker` that step a select from the keyboard or leave one without Enter,
  // and all five of `keyboard-session-pickers`.
  // A send held open across two in-app navigations, with the row's draft, its picker and its
  // Reply held until the server answers: `keyboard-agents-picker`'s row back after the reader
  // left mid-send, which carries `@webkit` alone because each of its six measured failures was
  // in WebKit.
  // The engine's cancellation of a chunk the navigation away from it left downloading, and the
  // margin hold's frame and scroll order: seven `deep-links` rows - the held chunk's, the press
  // raced against the landing's own frames, the two hidden-editor rows that run with the page's
  // animation frames held, the per-frame scroll of a past version's landing, the placement the
  // margin's own scroll must not move, and the composer opened from the document after a comment
  // link, whose selection bar the landing's frames can carry off screen.
  // IndexedDB-backed recovery of an edit the server has not taken: three `offline-edits` rows -
  // the edit restored across a reload, the acknowledged one cleared, and two tabs holding two
  // connections to one store.
  // The SVG parser, the shadow DOM and the ELK layout a diagram draws through: `mermaid`'s one
  // row.
  // A Web Worker's chunk, its deadline and a paint that arrives after it: `heavy-streamed-turn`'s
  // row, where the body grows under its own paint as a turn streams. The body a reader opens is
  // `heavy-bodies`, which runs in `webkit-alone` below, since its rows are read from the clock.
  // The one-socket invariant: `live-socket`'s row that a tab holds one live socket for every live
  // view, since WebKit is the engine iOS Safari opens every subscription in.
  // The keyed write path's request failure, its abort, a tab's storage across a reload and the
  // ask before a tab unloads: nine `keyed-writes` rows and two `keyed-send` rows, one of which
  // asks whether the engine's IndexedDB gives a kept upload its `File` back, which WebKit's does
  // not. One of those nine, the ask answer whose answer is lost, carries `@webkit` alone: both of
  // its measured failures were in WebKit.
  // The reload line a read-only admission names: `doc-reload-line`'s one row, which reads the
  // socket the engine reconnects and the paint after it.
  // Without a `testMatch` the project would see every spec in this directory, so it names the
  // same `testIgnore` as `chromium`: no tagged row may sit in a plain-HTTP, multiprocess or
  // clock-read spec, and `heavy-bodies.e2e.ts` belongs to `webkit-alone` alone. Its
  // `grepInvert` is `chromium`'s for the same reason: a clock row of `large-documents.e2e.ts`
  // given an engine tag later would otherwise be measured four lanes wide here.
  {
    name: "webkit",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs, visualSpecs],
    grep: webkitTagged,
    grepInvert: clockRows,
    use: { ...devices["Desktop Safari"] },
  },
  // The live view's phone layout (its keyboard cap, gutter and scroll locks), its model
  // pill's ellipsis (the two engines lay out a flex child's min-width differently), the
  // header's model and thinking pickers on a phone, what the Conversation's floating pills
  // cover on a phone, and the one-socket invariant also run in WebKit, the engine iOS Safari
  // uses; only those rows, since the rest of each spec is engine-agnostic.
  // This project keeps its file `testMatch` and its title `grep` rather than an engine tag: 13
  // rows is already as narrow as a tag would make it, and Playwright reads an array of regexes
  // as OR (`createTitleMatcher` answers on the first that matches), so a tag beside the titles
  // below would widen the project rather than narrow it.
  {
    name: "webkit-iphone",
    testMatch:
      /(agent-view|agent-view-settings|phone-conversation|delivery-live|live-socket)\.e2e\.ts/,
    grep: /on a phone the live view|a document-scrolling route|covers none of|a capped model name|on a phone the delivery page|the header shows the session's reported model|a tab holds one live socket for every live view/,
    use: { ...devices["iPhone 13"] },
  },
  // Firefox runs the rows a Firefox decision can change, and nothing else: it selects by the
  // `@firefox` tag, so each row it runs carries that tag and a comment naming the clause below it
  // exercises. Every clause here names one engine behaviour and the rows that exercise it.
  // Native editing over what follows a block's last line break, which this engine mishandles:
  // `code-line-replace`'s two rows.
  // The engine's focus rule for a click on a link inside the editor, where Firefox focuses the
  // editing host rather than the link: `picture-link-click`'s one row.
  // A closed select's `change` dispatched inside the key's own task, which the Agents issue
  // picker's and the live header pickers' keyboard-step rule rests on: the six rows of
  // `keyboard-agents-picker` that step a select from the keyboard or leave one without Enter,
  // and all five of `keyboard-session-pickers`.
  // The engine's cancellation of a chunk the navigation away from it left downloading, and the
  // margin hold's frame and scroll order: seven `deep-links` rows - the held chunk's, the press
  // raced against the landing's own frames, the two hidden-editor rows that run with the page's
  // animation frames held, the per-frame scroll of a past version's landing, the placement the
  // margin's own scroll must not move, and the composer opened from the document after a comment
  // link, whose selection bar the landing's frames can carry off screen.
  // IndexedDB-backed recovery of an edit the server has not taken: three `offline-edits` rows -
  // the edit restored across a reload, the acknowledged one cleared, and two tabs holding two
  // connections to one store.
  // The SVG parser, the shadow DOM and the ELK layout a diagram draws through: `mermaid`'s one
  // row.
  // A Web Worker's chunk, its deadline and a paint that arrives after it: `heavy-streamed-turn`'s
  // row, where the body grows under its own paint as a turn streams. The body a reader opens is
  // `heavy-bodies`, which runs in `firefox-alone` below, since its rows are read from the clock.
  // The one-socket invariant: `live-socket`'s row that a tab holds one live socket for every live
  // view, which must hold in every engine people read in.
  // The socket rotation the engine makes a page spend, and the reads the page makes around it:
  // `live-socket`'s rotation row, which this engine failed with an extra read of the issue, and
  // a row is kept in the project that failed it so that failure cannot come back unseen.
  // The keyed write path's request failure, its abort, a tab's storage across a reload and the
  // ask before a tab unloads: eight `keyed-writes` rows and two `keyed-send` rows, whose upload
  // row carries the kept-file question with it.
  // The reload line a read-only admission names: `doc-reload-line`'s one row.
  // The `testIgnore` and `grepInvert` are `chromium`'s, for the reasons given above `webkit`.
  {
    name: "firefox",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecs, visualSpecs],
    grep: firefoxTagged,
    grepInvert: clockRows,
    use: { ...devices["Desktop Firefox"] },
  },
  // The one row of `shell.e2e.ts` that follows a Download link before a chunk fails. It keeps its
  // file `testMatch` and its title `grep` rather than an engine tag, for the reason given above
  // `webkit-iphone`: one row is as narrow as a tag would make it, and a tag added beside the
  // title below would be read as OR and widen the project.
  {
    name: "firefox-download-navigation",
    testMatch: /shell\.e2e\.ts/,
    grep: /a chunk that fails after a Download link was followed reloads the page/,
    use: { ...devices["Desktop Firefox"] },
  },
  {
    name: "chromium-plain-http",
    testMatch: plainHttpSpecs,
    use: {
      ...devices["Desktop Chrome"],
      baseURL: plainHttpOrigin,
      launchOptions: { args: [`--host-resolver-rules=MAP ${plainHttpHost} 127.0.0.1`] },
    },
  },
  // The keyed write path's sign-out rows on that same plain-HTTP origin, which is no secure
  // context, so the browser has no `navigator.locks`: there a tab holds the person's writes for
  // another tab's sign-out on the lease that tab renews (`SIGN_OUT_RENEW_MS`,
  // `SIGN_OUT_LEASE_MS`), which a secure context never reaches.
  {
    name: "chromium-plain-http-sign-out",
    testMatch: /keyed-writes\.e2e\.ts/,
    grep: /a sign-out/,
    use: {
      ...devices["Desktop Chrome"],
      baseURL: plainHttpOrigin,
      launchOptions: { args: [`--host-resolver-rules=MAP ${plainHttpHost} 127.0.0.1`] },
    },
  },
] satisfies Project[];

/**
 * The last projects of a run: the rows whose claim is a wall-clock time, with the machine to
 * themselves.
 *
 * Each names in `dependencies` every project before it, so Playwright starts the first of them
 * only once every parallel project has finished, and runs these four one after another rather
 * than three at a time. Ordering alone is not quiet: a project's own rows would still run four
 * at a time, so each caps its own workers at one. Playwright 1.64.1 takes that cap per project -
 * `testProject.workers`, "The maximum number of concurrent worker processes to use for
 * parallelizing tests from this project", which its dispatcher holds a project's running jobs to
 * (`_workerLimitPerProjectId`, `playwright/lib/runner/index.js`) - and the run's own `workers`
 * above stays at the lane count, so the suite ahead of these projects runs as wide as ever. A
 * worker here resolves a lane as any other worker does (`TEST_PARALLEL_INDEX`,
 * e2e/harness-ports.ts), so these rows run against one lane's harness while the other lanes
 * stand idle.
 *
 * A project whose dependency has a failing row is not run at all: in a red run these rows are
 * left unrun rather than measured beside whatever the failing row's retry is doing.
 *
 * A command line that names one of these files runs the whole suite, since a project's
 * dependencies are pulled in whole: a file filter narrows a project it selects, never a project
 * selected to run before it. `--no-deps` is how one of these files is run by itself, and such a
 * run is as quiet as the machine it is on.
 *
 * Each engine keeps the rows it ran. `heavy-bodies.e2e.ts` ran in `chromium`, `firefox` and
 * `webkit`, for the reasons written above those projects, and runs here in all three;
 * `nested-directive.e2e.ts` ran in `chromium` alone and runs in `chromium-alone` alone.
 *
 * The first three select by `testMatch` alone, where `webkit` and `firefox` select by an engine
 * tag: their axis is the file, not a behaviour. Every row of a clock-read file runs here
 * whatever it asserts, because what puts it here is that its claim is a wall-clock time, and
 * `heavy-bodies.e2e.ts` carries no engine tag at all - the two tagged projects ignore it.
 * `iphone-alone` is the exception and says why at its own entry: its three rows share a file
 * with rows that belong in the lanes, so it selects by row.
 *
 * Each also sets `retries: 0`, which Playwright takes per project as it takes `workers`,
 * against the one retry the run takes on CI. The reason is these rows' own, and holds whatever
 * the reporting around them does: a row whose claim is a wall-clock time has nothing to gain
 * from a second attempt, because a threshold that a regression misses half the time would fail
 * the first attempt, pass the retry, be reported `flaky`, and leave the run green where it
 * should be red. The multiprocess project above sets the same value on a different argument.
 */
const aloneProjects = [
  {
    name: "chromium-alone",
    dependencies: parallelProjects.map((project) => project.name),
    testMatch: clockSpecs,
    use: { ...devices["Desktop Chrome"] },
    workers: 1,
    retries: 0,
  },
  {
    name: "firefox-alone",
    dependencies: [...parallelProjects.map((project) => project.name), "chromium-alone"],
    testMatch: /heavy-bodies\.e2e\.ts/,
    use: { ...devices["Desktop Firefox"] },
    workers: 1,
    retries: 0,
  },
  {
    name: "webkit-alone",
    dependencies: [
      ...parallelProjects.map((project) => project.name),
      "chromium-alone",
      "firefox-alone",
    ],
    testMatch: /heavy-bodies\.e2e\.ts/,
    use: { ...devices["Desktop Safari"] },
    workers: 1,
    retries: 0,
  },
  // The three clock rows of `large-documents.e2e.ts` (`clockRows` above), on the phone they
  // measure: the iPhone 13 viewport, touch and Chromium the `iphone` project gives them, since
  // every target the spec names is read on that device under 4x CPU throttling. It selects by
  // row rather than by file, because the file's other rows belong in the lanes, and it runs
  // last, after the three `-alone` projects above it, so nothing of the suite is left beside it.
  // Its three rows are the whole of its cost: the measurements took 34.3 s and 1.2 m per attempt
  // on CI's own runner and the timer row 1.3 m, each one row wide.
  {
    name: "iphone-alone",
    dependencies: [
      ...parallelProjects.map((project) => project.name),
      "chromium-alone",
      "firefox-alone",
      "webkit-alone",
    ],
    testMatch: /large-documents\.e2e\.ts/,
    grep: clockRows,
    use: { ...devices["iPhone 13"], browserName: "chromium" as const },
    workers: 1,
    retries: 0,
  },
] satisfies Project[];

/**
 * The containerised visual review: one row per page of `e2e/review-routes.ts`'s shot set, each
 * comparing that page against a committed baseline in light and in dark, and the pages a phone
 * width rearranges once more in that viewport (e2e/visual.e2e.ts).
 *
 * Both projects run that one spec and split its rows by the ` @phone` tag its phone rows carry,
 * the tag the suite already selects phone rows by (`phoneTagged` above): `visual-phone` greps
 * it and `visual` inverts it, so every row runs under exactly one project and reads the
 * baseline directory of that project alone.
 *
 * A baseline a person takes and a baseline CI compares must be the same rendering, so both
 * projects run inside the pinned Playwright image and nowhere else: `e2e/visual-run.sh` seeds on
 * the host, starts that container and runs exactly these two projects in it. A run outside the
 * container reaches the rows, and every one of them skips itself, because the host's browser
 * builds, fonts and GPU stack are not the image's and its shots would be a different rendering
 * of the same page (the skip and its message are in the spec).
 *
 * They are declared unconditionally, never behind the variable that turns the rows on.
 * `e2e/__tests__/row-configurations.test.ts` lists this config in a subprocess whose environment
 * is cut to `HOME`, `PATH` and `DISPATCH_E2E_PROCESSES`, so a project declared only when a
 * variable is set is invisible to that guard: its rows would never be checked for a duplicate
 * configuration while the test still read green.
 *
 * They sit after `aloneProjects` in a group of their own, so `parallelProjects` does not grow -
 * a project added there is one more the clock rows wait for - and the `-alone` projects gain no
 * dependency on a project that skips its rows on every host run.
 *
 * `workers: 1`, which Playwright takes per project: these rows compare images, and two of them
 * drawing at once on one machine is the contention that makes a comparison read a half-drawn
 * frame. `retries: 0`, against the one retry the run takes on CI, for a reason of their own: a
 * screenshot comparison that fails and then passes is a tolerance that is too tight or a page
 * that is not settled, and either one is the thing this layer exists to find, so it is reported
 * as a failure rather than as a flake.
 */
const visualProjects = [
  {
    name: "visual",
    testMatch: visualSpecs,
    grepInvert: phoneTagged,
    // A baseline is an input a person reads in a review, not an output of a row, so these files
    // live beside the suite at `e2e/__screenshots__/<project>/<route>-<scheme>.png` and are
    // committed - the one place in this suite a spec's file is not under `testInfo.outputPath()`
    // (packages/dispatch/AGENTS.md). The template carries no `{platform}` segment, which the
    // default template adds: every shot is taken in the one pinned Linux image, so the segment
    // would say the same thing on every file and in the name of each the route and the scheme
    // are what a reader is looking for.
    snapshotPathTemplate: "{testDir}/__screenshots__/{projectName}/{arg}{ext}",
    use: { ...devices["Desktop Chrome"], viewport: { height: 800, width: 1280 } },
    workers: 1,
    retries: 0,
  },
  {
    name: "visual-phone",
    testMatch: visualSpecs,
    grep: phoneTagged,
    snapshotPathTemplate: "{testDir}/__screenshots__/{projectName}/{arg}{ext}",
    use: { ...devices["iPhone 13"], browserName: "chromium" as const },
    workers: 1,
    retries: 0,
  },
] satisfies Project[];

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  // The projects below that name no `testIgnore` of their own inherit this one.
  testIgnore: harnessProcessCount === 1 ? multiprocessSpecs : undefined,
  outputDir: "test-results",
  // Every row is independent: it empties its lane's database and fake listeners itself
  // (e2e/seed.ts), and a worker never runs two rows at once, so rows of one file may run in
  // different lanes. A file whose rows must run in order says so itself, with
  // `test.describe.configure({ mode: "serial" })` and the reason.
  fullyParallel: true,
  workers: lanes,
  // One retry on CI, none locally. A row that fails once and passes on the retry is reported
  // `flaky` rather than failed, which is what the merged report of the dispatch-browser lane
  // publishes its flake rate from. Locally a retry would hide a failure the developer is looking
  // at, and `bun run e2e` is read as pass or fail. This is the sharded projects' value: the
  // three `-alone` projects and the multiprocess one each set `retries: 0`, each for a reason
  // its own comment gives.
  retries: process.env.CI ? 1 : 0,
  // With E2E_MEASURE_FILE set, the timing fixture's reporter writes one row per test beside the
  // list reporter's own output (e2e/measure-reporter.ts); without it the run is untouched.
  reporter: measuring
    ? [["list"], [fileURLToPath(new URL("./measure-reporter.ts", import.meta.url))]]
    : "list",
  timeout: testTimeout,
  expect: { timeout: expectTimeout },
  use: {
    ...devices["Desktop Chrome"],
    baseURL: baseUrl,
    headless: true,
    // One value for both runs. `retain-on-first-failure` records the first attempt and keeps it
    // when that attempt failed (`mode === "retain-on-first-failure" && this._testInfo.retry
    // === 0`, node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/worker/
    // workerProcessEntry.js:554), so a flaky row carries the trace of the attempt that failed,
    // which is the one that explains it. `on-first-retry` records on the retry alone (:550,
    // `retry === 1`), and a row is flaky exactly when its retry passed, so that mode would keep
    // a trace of a green run and leave the failure none. Locally, with `retries: 0`, the first
    // attempt is the only attempt, so this is the `retain-on-failure` the suite had before.
    trace: "retain-on-first-failure",
  },
  // Runs before any row, against the servers the checks above require: it refuses a target that
  // does not read this run's fake Envoy, and in two-process mode a pair whose processes do not
  // both read it or do not accept each other's cookies (e2e/preflight.ts).
  globalSetup: fileURLToPath(new URL("./preflight.ts", import.meta.url)),
  projects:
    harnessProcessCount === 2
      ? multiprocessProjects
      : [...parallelProjects, ...aloneProjects, ...visualProjects],
});
