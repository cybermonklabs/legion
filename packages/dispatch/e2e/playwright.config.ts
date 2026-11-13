import { fileURLToPath } from "node:url";
import { defineConfig, devices, type Project, type ReporterDescription } from "@playwright/test";
import { baseUrl } from "./api";
import {
  clockFileOnCommandLine,
  clockProjectNames,
  clockSpecPattern,
  clockSpecsNamed,
  phoneAloneSpecPattern,
  phoneAloneSpecsNamed,
  stubProjectNames,
  tagged,
} from "./configurations";
import { harnessProcessCount } from "./harness-processes";
import { harnessSignIn } from "./harness-signin";
import { measuring } from "./measure";
import { plainHttpHost, plainHttpOrigin } from "./plain-http-origin";
import { stubOrigin } from "./stub/origin";
import { stubSelection } from "./stub/selection";

// The plain-HTTP project's origin is a proxy on PLAIN_HTTP_PORT, reached by a host name Chromium
// maps to loopback. The page is a non-loopback plain-HTTP origin, while the proxy forwards to the
// dev sign-in server under its own loopback origin. `plainHttpSpecs` are the files that project
// runs, the origin's own rows and the proxy's; `chromium` and `iphone` ignore them, since they
// open the plain-HTTP name, which only that project maps, and the origin rows' first assertion
// fails on loopback by design.
const plainHttpSpecs = /plain-http-(origin|proxy)\.e2e\.ts/;
// The `iphone`, `webkit` and `firefox` projects run only the rows tagged `@phone` (or the
// `@phone-only` below it), `@webkit` and `@firefox`, written at the end of the test's title.
// Which tests take `@phone` is in `packages/dispatch/AGENTS.md`, "Phone acceptance"; an engine
// tag names a row for one of two reasons, and a comment above the row says which. Either the
// row exercises one engine behaviour the comment above those projects lists - the usual reason
// - or every measured failure of that row was in that engine, so the row is kept where its
// cause lives: a row green only on an engine its defect was never seen on proves nothing about
// that defect. Two rows carry a tag for the second reason today, each named in
// `packages/dispatch/AGENTS.md`: `keyed-writes.e2e.ts`'s ask answer whose answer is lost (two
// timeouts in 213 CI runs, both in WebKit) and `keyboard-agents-picker.e2e.ts`'s row back after
// the reader left mid-send (six failures on six branches, every one in WebKit). A row that only
// repeats an assertion Chromium already made, and whose failures are not an engine's, carries
// no tag. An untagged test drops out with no failure. The pattern is anchored so a tag matches
// only as a whole whitespace-separated token, never inside a longer one such as `@phones`,
// `x@phone`, `@webkit-iphone` or `@firefox-alone`.
const phoneTagged = tagged("phone");
// A row of the phone projects alone, which `@phone` does not say: `@phone` means "runs on the
// phone AS WELL", and `chromium`, `stub` and `visual` run those rows at their own width. A row
// whose claim exists only below the phone breakpoint carries ` @phone-only` BESIDE its `@phone`,
// and the desktop half of each pair inverts that second tag alone. `@phone` is never inverted:
// a review of the stubbed layer read the three rows that cost when `stub` inverted it, and the
// comment above `stub-phone` at the end of this file holds that reading.
//
// The pair is a selector, not a skip. Each of these rows used to be scheduled in the desktop
// project and call `test.skip` against `testInfo.project.name` in its own body, so the report
// named a row that was never going to run; the tag lets the project that owns the row select it
// (dispatch://INFRA-1410). Playwright reads an array of patterns as OR, so a phone project
// names both tags and takes a row carrying either.
const phoneOnlyTagged = tagged("phone-only");
const phoneRowTagged = [phoneTagged, phoneOnlyTagged];
// `delivery-live.e2e.ts`'s one phone row, which `webkit-iphone` owns by itself: it carries no
// `@phone`, since the Chromium phone project never ran it, and no `@phone-only` would say that
// either - that tag would hand it to `iphone`. `chromium` is the only other project the file
// reaches. Both projects name this constant: `webkit-iphone`'s `grep` selects the row by it,
// and `chromium`'s `grepInvert` drops the row by it. The fragment is written here once, so a
// title edit changes both projects together. A second copy of it could miss such an edit and
// leave the row in `chromium`, where its phone assertions fail at the desktop width rather
// than passing unseen.
const webkitPhoneDeliveryRow = /on a phone the delivery page/;
const webkitTagged = tagged("webkit");
const firefoxTagged = tagged("firefox");
// Specs of two Dispatch processes behind the round-robin proxy (e2e/round-robin-proxy.ts), each
// named `*.multiprocess.e2e.ts`. The default run ignores them, since they need both processes and
// the proxy's own routes; two-process mode (DISPATCH_E2E_PROCESSES=2, `bun run e2e:multiprocess`)
// runs them alone, in Chromium, since every other spec runs against one server already.
const multiprocessSpecs = /\.multiprocess\.e2e\.ts$/;
// Specs of the stubbed layer, each named `*.stub.e2e.ts`: a real browser against a recording of
// the API, with no Go server, no Postgres, no fakes, no sign-in and no reset (e2e/stub/). The
// `stub` project at the end of this file runs them and nothing else runs them: this config's own
// `testIgnore` holds the pattern, and so does every project that names a `testIgnore` of its own,
// since a project's overrides this one's. A moved row lives in such a file rather than carrying an
// annotation, because a real-stack spec takes its `test` from e2e/test.ts, which resets the
// database before every row and signs the default context in: a stubbed row left among them would
// pay that fixture for nothing (packages/dispatch/AGENTS.md, "The stubbed layer").
const stubSpecs = /\.stub\.e2e\.ts$/;
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

// `DISPATCH_E2E_REUSE_SERVERS=1` says the harness ports name servers that are already running:
// a harness a person started and left listening, or the one `e2e/harness-up.ts` printed for
// another process to read. Any other value is refused rather than quietly read as "no".
//
// The refusal is made here, rather than in the hook, because this evaluates for every run, a
// listing included, where `globalSetup` does not. What the value MEANS - that this run starts
// no stack - is the hook's, and it reads the variable itself, by then knowing that it holds
// "1" or nothing.
function refuseUnknownReuseServers(): void {
  const requested = process.env.DISPATCH_E2E_REUSE_SERVERS;
  if (requested === undefined || requested === "" || requested === "1") return;
  throw new Error(
    `DISPATCH_E2E_REUSE_SERVERS must be "1" or unset, not ${JSON.stringify(requested)}. ` +
      "Leave it unset to have this run start its own stacks (e2e/global-setup.ts), or set it to " +
      "1 to run against a harness you started yourself."
  );
}

refuseUnknownReuseServers();

// THE PORTS ARE UNSET WHEN THIS EVALUATES, AND THAT IS THE DESIGN. Playwright loads this config
// before it runs `globalSetup`, which is what starts the stacks (e2e/global-setup.ts), so in this
// process every harness port is unset and `harnessPorts()` answers 0 for each of them - no
// refusal, no listener addressed, nothing read from them here. Every worker process re-evaluates
// this file AFTER `globalSetup`, with the manifest that hook wrote in its environment, and it is
// that evaluation which decides the worker's `use.baseURL` and every import-time origin beside it
// (e2e/api.ts, e2e/plain-http-origin.ts). The four refusals this file used to make - the unset
// ports, the missing `DISPATCH_E2E_REUSE_SERVERS`, a `--workers` that disagreed with the number
// of harnesses the wrapper had started and a worker count it had started none for - were all
// about a harness that had to be up before Playwright started, which is no longer how a run
// works.
//
// What this run wants of the stubbed layer, read once (e2e/stub/selection.ts): whether it is
// recording, whether it holds a stubbed row at all - which is what decides the static file
// server below - and whether it is the stubbed layer alone, which is what tells `globalSetup` to
// start nothing. That module says why each question is asked and why the third travels to the
// workers in an environment variable.
const stub = stubSelection({
  argv: process.argv,
  env: process.env,
  processes: harnessProcessCount,
  projects: stubProjectNames,
});

// A command that names a wall-clock spec file as a filter while naming no project
// (`clockFileOnCommandLine`, e2e/configurations.ts). The six `-alone` projects
// (`clockProjectNames`, e2e/configurations.ts) are out of the default selection, so no run that
// names no project holds a row of the file such a command named, whatever Playwright then does
// with the rest of the line - for example, answer "No tests found", or run the whole default
// selection, which is what a filter dropped after a `--` leaves (e2e/configurations.ts). No such
// answer names a cure; this says the command that does run the file. Throwing as this module
// evaluates is what puts the message on standard error and the run's status above zero, and
// Playwright prints a config error with a stack after it, so the first line is the command and
// nothing else.
//
// That line names its own worker count. The measurement a wall-clock row makes is a reading
// only in a run one worker wide, and `refuseASharedRun` (e2e/main-thread-hold.ts) fails a
// measuring row in any wider run, so the command printed here says `--workers=1` rather than
// resting on this config's default (`workers: 1` below), which a `--workers` of the reader's
// own would override without changing a word of this line.
//
// It stands under no guard at all, where the reuse-servers refusal above is the only other one
// this file makes: a command line that would list nothing is as wrong in a run of the stubbed
// layer alone as in any other.
const clockFile = clockFileOnCommandLine(process.argv);
if (clockFile !== undefined) {
  throw new Error(
    `bun run e2e -- --project=chromium-alone ${clockFile} --workers=1\n` +
      `\nThat is how ${clockFile} is run, and this command named no project. The wall-clock ` +
      `projects - ${clockProjectNames.join(", ")} - are out of the default selection, because ` +
      "their rows are read from the wall clock and need the machine to themselves, so no run " +
      "that names no project holds a row of that file, whatever Playwright then does with the " +
      'rest of this command line - for example, answer "No tests found", or run the whole ' +
      "default selection, which is what a filter dropped after a `--` leaves. Write the " +
      "project as --project=<name> and never --project <name>: the option is variadic, so the " +
      "file after it would be read as a second project name. To run every wall-clock row, " +
      "run `bun run e2e:clock`."
  );
}

// The default wait for asynchronous server and rendering readiness. A spec only sets its own
// timeout when that deadline is its observable contract (the keyboard chord-expiry test).
const expectTimeout = 15_000;

// The per-test budget, measured rather than inherited. Playwright's own default is 30 s, chosen
// when this suite ran one row at a time; it now runs four workers on a four-core runner. Read from
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

// The per-action deadline, and the one timeout Playwright leaves at 0 by default - no deadline at
// all, so an action whose target has gone runs until the row's budget ends and is reported at
// whatever line the row's `finally` holds rather than at the call that hung. A 60 ms product race
// cost 120 s twice that way, at `await context.close()` (dispatch://INFRA-2452). It bounds one
// action - a click, a fill, a press - and not a row: an `expect` carries `expectTimeout`, a whole
// row `testTimeout` above, and a navigation its own `navigationTimeout`, which is set apart from
// this one and stays 0 (node_modules/.bun/playwright@1.64.1/node_modules/playwright-core/lib/
// coreBundle.js:64256-64257). It is the context's default timeout as well (`actionTimeout || 0`,
// node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/index.js:360), so a
// `waitForResponse` or `waitForFunction` that names no timeout of its own takes it too. The
// longest such wait this change read is `replayed()` (e2e/keyed-writes.ts), which answers the
// 18 s server hold of keyed-send.e2e.ts:411, well inside this.
//
// Derived from the population `testTimeout` above measures, not asserted: over those 320,101
// passing results of 201 ci.yaml runs the slowest WHOLE row is 29,856 ms, so no single action in
// any of them reached 30 s. An action a governed row can afford at all is under that row's 60 s,
// since the row pays for everything else it does out of the same budget. That leaves the band
// between, and 45 s sits in it: above every passing result the window measured, and below the
// budget that would otherwise swallow the failure.
//
// The same limit applies as to the budget above, rather than being papered over: the population
// is censored at Playwright's old 30 s default, where 27 governed results were killed (19 in
// pictures.e2e.ts, 6 in deep-links.e2e.ts, 2 in keyed-writes.e2e.ts), so the tail above 30 s is
// unknown and a governed row's single action between 30 s and 45 s has been measured by nobody.
// Move it only on a measurement of the same shape.
const actionTimeout = 45_000;

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

// The three rows of `heavy-bodies.e2e.ts` that read where a reader's place ends up once a long
// body fills under them. The compensation they measure is layout Chromium and WebKit anchor
// differently, and those are the two engines it was ever measured on, so `firefox-alone` below
// inverts them. Each fragment names one row of that file and no other.
const readersPlaceRows =
  /a long message arriving above the reader|a reader who scrolls while a long body|Jump to latest keeps a newest long turn/;

// The five rows read from the wall clock over a large document are three files of their own,
// one per project, and the `-alone` projects below select them by file: every parallel project
// ignores all three through `clockSpecPattern` (e2e/configurations.ts), as it ignores the three
// specs those projects run whole. They were rows of `large-documents-measured.e2e.ts` picked
// out by patterns over their titles until sjawhar/legion#2697, and a title is no selector: a
// row renamed, or one added under a title the pattern missed, ran four workers wide in a
// parallel project with every suite green. The argument above `aloneProjects` is theirs - a
// reading of the clock is the page's own work only on a machine the run is not sharing - and
// CI has said it of each of them.
//
// Four are read on the phone the spec names - the two measurements of a first editable block
// under 4x CPU throttling, the row that times a page timer and a socket message handed over
// inside the editor's build, and the row that times a decision answered inside one. At one
// head the two measurements read 4,824 ms and 8,311 ms on
// their first attempt and 3,838 ms and 5,105 ms on the retry, with each attempt's network reads
// the same or slower and every main-thread number halved, which is the other workers easing
// rather than any cache warming; at the same head the timer row's worst wait was 8,279 ms
// behind a build whose own longest task was 732 ms. The decision row holds two readings of the
// page's own clock 500 ms apart under 4x CPU throttling - the press the page made and the
// request its answer left in - which four workers on one machine measure as surely as they
// measure the other three (dispatch://INFRA-341).
//
// Those four are two projects and not one, so they are two files. The two measurements take
// three readings each and hold the median of them to R11's targets, which is the protocol the
// spec's Design paragraph names (`measuredReadings`, e2e/large-documents-page.ts), so each
// costs about three times the wall clock it cost when it took one reading, and on CI's own
// figures the two of them are most of what their leg spends. They are
// `large-documents-measured.e2e.ts`, which `iphone-measure-alone` below runs, a leg of its own
// in .github/workflows/dispatch-browser.yaml with a budget derived from their own measurement;
// the timer row and the decision row, which neither changed nor repeat, are
// `large-documents-build-serves.e2e.ts`, which `iphone-alone` runs, keeping the budget their
// own measurement earned (dispatch://INFRA-341).
//
// The fifth is the guard on the longest main-thread task of a build at a desktop width, whose
// number is as much the runner's as theirs: over three CI runs of one branch, each in a
// `chromium` shard leg four workers wide, it printed `longest 445 ms`, `longest 467 ms` and
// `longest 557 ms`, the last of them a failure against its 500 ms guard on both attempts, while
// the build it reads grew from 23,061 ms to 41,888 ms with nothing of the page changing - the
// other three workers of the shard, and the row's own number moving with them
// (dispatch://INFRA-341). Its claim is about a desktop width, so neither phone project is a
// home for it: it is `large-documents-desktop-build.e2e.ts`, which `chromium-alone` below runs
// beside the three specs it runs whole, Desktop Chrome with the machine to itself.

// The one spec of the containerised visual review (`visual.e2e.ts`), which runs in the `visual`
// and `visual-phone` projects at the end of this list and in no project above them: a project
// whose configuration key equals one of theirs - the same engine, viewport, origin and
// server-process count - would run the same row twice, which
// e2e/__tests__/row-configurations.test.ts refuses. `iphone` is that project for `visual-phone`,
// which spreads the same device, and the three other projects that name a `testIgnore` name
// this one too for the reason given above `webkit`: without a `testMatch` a project sees every
// spec in this directory, so the ignore is what keeps this file to its own two projects.
const visualSpecs = /visual\.e2e\.ts$/;

// Every project that runs as wide as the run's own workers.
const parallelProjects = [
  // The desktop half of the pair: every row of the suite except the three kinds this project is
  // not the one for. A `@phone-only` row has no desktop case to make, the one row of
  // `delivery-live.e2e.ts` that `webkit-iphone` owns has none either, and a row read from the
  // wall clock is in a file of its own that this project's `testIgnore` holds
  // (`clockSpecPattern`, e2e/configurations.ts).
  {
    name: "chromium",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecPattern, visualSpecs, stubSpecs],
    grepInvert: [phoneOnlyTagged, webkitPhoneDeliveryRow],
    use: { ...devices["Desktop Chrome"] },
  },
  // The phone half of every `@phone` row, but for the files whose phone half is a leg of its own
  // (`phoneAloneSpecPattern`, e2e/configurations.ts): `large-documents-long-build.e2e.ts`, whose
  // three rows `iphone-long-build-alone` runs at the end of this list. `chromium` keeps that
  // file's desktop half.
  {
    name: "iphone",
    testIgnore: [
      plainHttpSpecs,
      multiprocessSpecs,
      clockSpecPattern,
      phoneAloneSpecPattern,
      visualSpecs,
      stubSpecs,
    ],
    grep: phoneRowTagged,
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
  // clock-read spec, and `heavy-bodies.e2e.ts` belongs to `webkit-alone` alone. That ignore is
  // what keeps the six clock-read files to the `-alone` projects at the end of this list and
  // out of every project above them (sjawhar/legion#2612), here and in the three other projects
  // that name it - a tag written on a wall-clock row included, since what this project sees is
  // decided by the file and never by the row.
  {
    name: "webkit",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecPattern, visualSpecs, stubSpecs],
    grep: webkitTagged,
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
  // below would widen the project rather than narrow it. That OR is what lets the `grep` below
  // be an array: the titles this project owns, then the one constant below.
  // One of those rows, `delivery-live.e2e.ts`'s phone row, is this project's alone, and
  // `chromium` inverts it. Both name `webkitPhoneDeliveryRow`, the one place that title
  // fragment is written, and the comment above it says why the row is this project's.
  {
    name: "webkit-iphone",
    testMatch:
      /(agent-view|agent-view-settings|phone-conversation|delivery-live|live-socket)\.e2e\.ts/,
    grep: [
      /on a phone the live view|a document-scrolling route|covers none of|a capped model name|the header shows the session's reported model|a tab holds one live socket for every live view/,
      webkitPhoneDeliveryRow,
    ],
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
  // The `testIgnore` is `chromium`'s, for the reason given above `webkit`.
  {
    name: "firefox",
    testIgnore: [plainHttpSpecs, multiprocessSpecs, clockSpecPattern, visualSpecs, stubSpecs],
    grep: firefoxTagged,
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
 * The projects a command names: the rows whose claim is a wall-clock time, with the machine to
 * themselves, and the phone half of the one file too costly for a shard
 * (`iphone-long-build-alone`, last below).
 *
 * Each sets `default: false`, which leaves all six out of a run that names no project and
 * takes nothing from a run that names one. `filterProjects` answers
 * `projects.filter(p => includeNonDefault || p.default)` where the command line named no
 * project, and ignores `default` altogether where it named any (`function filterProjects` at
 * node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/index.js:2134);
 * `default?: boolean` is a field of `TestProject` and is `true` where it is unset
 * (playwright/types/test.d.ts:170). So `--project=chromium-alone` selects this project as it
 * always did, `bun run e2e:clock` selects all six, and a run that names none holds no row of
 * theirs.
 *
 * That field is also what keeps a file filter cheap, and it replaced something that did not.
 * Each of these projects named every project before it in `dependencies` until
 * dispatch://INFRA-1280: that ordered the run, and it also pulled the whole suite in behind any
 * command naming one of their files, because a filter narrows a project it selects and never a
 * project selected to run before it. With no dependency and no place in the default selection,
 * such a command now selects nothing at all, so the configuration refuses a command that names
 * a wall-clock spec file and no project at the top of this file, and prints the command that
 * runs the file.
 *
 * What gives these rows the quiet machine is no longer the order they run in: it is a run one
 * worker wide. For the rows that read a hold through `e2e/main-thread-hold.ts`, the row itself
 * is what holds the run to that: `refuseASharedRun` fails a measuring row in a run of more than
 * one worker rather than letting it report a reading of what the other workers were running, so
 * a reading that survives was taken alone whatever project it was taken in. The rows of
 * `agent-workspace-timing.e2e.ts` and the five `large-documents` rows these
 * projects take read their numbers from a clock of their own and reach that guard nowhere, so
 * nothing in them refuses a wider run: what keeps them alone is the command, and every command
 * that measures them names `--workers=1` - the refusal at the top of this file prints it, `bun
 * run e2e:clock` names it, and so do the alone job's legs
 * (.github/workflows/dispatch-browser.yaml).
 *
 * Each still caps its own workers at one, which holds a project's own rows to one at a time
 * however wide the run is: the `workers: 1` below is this config's default, which a command
 * line replaces, and CI names its own (.github/workflows/dispatch-browser.yaml). Playwright
 * 1.64.1 takes that cap per project - `testProject.workers`, "The maximum number of concurrent
 * worker processes to use for parallelizing tests from this project"
 * (node_modules/.bun/playwright@1.64.1/node_modules/playwright/types/test.d.ts:761), which its
 * dispatcher holds a project's running jobs to (`_workerLimitPerProjectId`,
 * `playwright/lib/runner/index.js`) - and the run's own `workers` stays what it was given, so
 * the rest of the suite runs as wide as ever. A worker here resolves a stack as any other
 * worker does (`TEST_PARALLEL_INDEX`, e2e/harness-ports.ts), so these rows run against one
 * stack's harness while the other stacks stand idle.
 *
 * Each engine keeps the rows it ran. `heavy-bodies.e2e.ts` ran in `chromium`, `firefox` and
 * `webkit`, for the reasons written above those projects, and runs here in all three;
 * `nested-directive.e2e.ts` ran in `chromium` alone and runs in `chromium-alone` alone, as do
 * `large-documents-desktop-build.e2e.ts` and `agent-workspace-timing.e2e.ts`,
 * whose rows time the live view and the Agents page in the one engine the timed halves are
 * claimed for (INFRA-812's D14; the untimed halves of those checks stay in `chromium` and
 * `iphone`, in `agent-view-cost.e2e.ts` and `agents-window.e2e.ts`).
 *
 * Every one of them selects by `testMatch` alone, where `webkit` and `firefox` select by an
 * engine tag: their axis is the file, not a behaviour. Every row of a clock-read file runs
 * here whatever it asserts, because what puts it here is that its claim is a wall-clock time,
 * and `heavy-bodies.e2e.ts` carries no engine tag at all - the two tagged projects ignore it.
 * Each names its files through `clockSpecsNamed` (e2e/configurations.ts), which refuses a name
 * no wall-clock spec has, so a project whose selector matches nothing cannot sit here reading
 * green. `firefox-alone`'s `grepInvert` is the one title this list still holds, and it drops
 * rows rather than taking them: the three rows of `heavy-bodies.e2e.ts` that read a reader's
 * place are measured in the two engines that anchor layout differently, and a title that
 * stopped matching would run them here as well rather than leaving them unmeasured.
 *
 * Each also sets `retries: 0`, which Playwright takes per project as it takes `workers`,
 * against the one retry the run takes on CI. The reason is these rows' own, and holds whatever
 * the reporting around them does: a row whose claim is a wall-clock time has nothing to gain
 * from a second attempt, because a threshold that a regression misses half the time would fail
 * the first attempt, pass the retry, be reported `flaky`, and leave the run green where it
 * should be red. The multiprocess project above sets the same value on a different argument.
 */
const aloneProjects = [
  // The three specs this project runs whole and the desktop guard beside them, each named in
  // `clockSpecNames` (e2e/configurations.ts) and selected by file: every row of those four
  // files is read from the wall clock, so there is nothing here to narrow by title.
  //
  // One project rather than one of its own for the desktop guard: this is already Desktop
  // Chrome out of the default selection, at one worker and no retries, which is the whole of
  // what that row needs, and `workers: 1` is what keeps its number its own - the rows here run
  // one after another, never beside each other. The two phone projects below are two for a
  // reason of their own, which is wall clock rather than configuration: their entries say it.
  {
    name: "chromium-alone",
    default: false,
    testMatch: clockSpecsNamed(
      "agent-workspace-timing",
      "heavy-bodies",
      "nested-directive",
      "large-documents-desktop-build"
    ),
    use: { ...devices["Desktop Chrome"] },
    workers: 1,
    retries: 0,
  },
  {
    name: "firefox-alone",
    default: false,
    testMatch: clockSpecsNamed("heavy-bodies"),
    grepInvert: readersPlaceRows,
    use: { ...devices["Desktop Firefox"] },
    workers: 1,
    retries: 0,
  },
  {
    name: "webkit-alone",
    default: false,
    testMatch: clockSpecsNamed("heavy-bodies"),
    use: { ...devices["Desktop Safari"] },
    workers: 1,
    retries: 0,
  },
  // The two clock rows read on the phone that take one reading apiece: the timer row and the
  // decision row of `large-documents-build-serves.e2e.ts`, which is this project's whole
  // selection. The iPhone 13 viewport, touch and Chromium the `iphone` project
  // gives them, since every target the spec names is read on that device under 4x CPU
  // throttling. Like the three projects above it, it is out of the default
  // selection, so nothing of the suite is beside it in the run that names it. On CI's own
  // runner, at head f79cb07f in run 43098661012's `alone (iphone-alone)` leg, the timer row
  // took 10.3 s and the decision row 19.8 s, each one row wide.
  {
    name: "iphone-alone",
    default: false,
    testMatch: clockSpecsNamed("large-documents-build-serves"),
    use: { ...devices["iPhone 13"], browserName: "chromium" as const },
    workers: 1,
    retries: 0,
  },
  // The two rows that measure a first editable block, which are
  // `large-documents-measured.e2e.ts`, on the same
  // device profile as `iphone-alone` above and selected the same way, by file. A project of
  // their own and not two more rows of that one, because each of them takes
  // three readings and holds the median to its target, and a budget is derived from what the
  // rows under it measured: three readings put these two at about three times the 16.2 s and
  // 20.7 s they took at one reading on CI (run 43098661012, the `alone (iphone-alone)` leg at
  // head f79cb07f), which is more than the whole of `iphone-alone`'s leg. Raising that leg's
  // budget would have moved the bound for the timer row and the decision row, which nothing
  // changed; this way each leg's budget is derived from its own measurement, and
  // .github/workflows/dispatch-browser.yaml prints both derivations
  // (dispatch://INFRA-341).
  {
    name: "iphone-measure-alone",
    default: false,
    testMatch: clockSpecsNamed("large-documents-measured"),
    use: { ...devices["iPhone 13"], browserName: "chromium" as const },
    workers: 1,
    retries: 0,
  },
  // The phone half of `large-documents-long-build.e2e.ts`, which is this project's whole
  // selection, on the same device profile as the two above and selected the same way, by file
  // (`phoneAloneSpecsNamed`, e2e/configurations.ts). Its three rows read no number from the wall
  // clock; what puts them on a leg of their own is their cost. Each builds the 406,000-character
  // document under 4x CPU throttling, and in `iphone` they stood at the tail of the listing
  // Playwright cuts its shards from by row count, so all three landed in the last shard: 1.8 min,
  // 51.8 s and 39.3 s of the 499 s `shard (5)` took of its 540 s in run 43421318396. `iphone`
  // ignores the file now, and `chromium` keeps its desktop half where it was, four workers wide.
  // One worker and no retry, as every project of this list: the build-time row holds two builds'
  // own times to a ratio, which a second attempt would only blur. The leg's budget is derived from
  // these rows' own measurement in .github/workflows/dispatch-browser.yaml (dispatch://INFRA-341).
  {
    name: "iphone-long-build-alone",
    default: false,
    testMatch: phoneAloneSpecsNamed("large-documents-long-build"),
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
 * baseline directory of that project alone. Each also names ` @phone-only` on its own side of
 * that split, so a phone row written here with that tag alone lands in `visual-phone` rather
 * than in both projects at once; no row of this spec carries it today.
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
 * They sit after `aloneProjects` in a group of their own rather than among the parallel
 * projects, because `parallelProjects` above is the projects that run as wide as the run's own
 * workers and each of these caps its own workers at one, for the reason below.
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
    grepInvert: phoneRowTagged,
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
    grep: phoneRowTagged,
    snapshotPathTemplate: "{testDir}/__screenshots__/{projectName}/{arg}{ext}",
    use: { ...devices["iPhone 13"], browserName: "chromium" as const },
    workers: 1,
    retries: 0,
  },
] satisfies Project[];

/**
 * The stubbed layer: a real browser against a recording of the API (e2e/stub/).
 *
 * It is declared unconditionally, like `visualProjects` above and for the same reason: the guard
 * that finds a row running twice in one configuration lists this config in a subprocess whose
 * environment is cut to `HOME`, `PATH` and `DISPATCH_E2E_PROCESSES`
 * (e2e/__tests__/row-configurations.test.ts), so a project declared only when a variable is set
 * would be invisible to it.
 *
 * Its origin is its own, a static file server over `web/dist` rather than a Dispatch server
 * (e2e/stub/origin.ts), which is also what gives it a configuration key of its own: `chromium`
 * runs the same engine at the same viewport, and the origin is what tells the two apart.
 *
 * `testIgnore: []` because this config's own `testIgnore` holds `stubSpecs`, to keep a project
 * that names neither selector from picking these rows up; a project's `testIgnore` replaces this
 * one's, so the empty array is what lets these rows reach the project that exists for them.
 *
 * Recording (`DISPATCH_E2E_STUB_RECORD=1`) is the one run where these rows meet the real stack:
 * the project takes the harness's base URL, the static server is not started, and each row writes
 * what it fetched (e2e/stub/stub-test.ts).
 *
 * `retries: 0` on both, for two reasons of this layer's own. On replay a stubbed row reaches
 * no server, no database and no fake: its answers come from a committed file, so a row that
 * fails once and passes once is not a flake to be retried past but a defect of the recording,
 * the page or the row, and the lane should say so. A recording run does reach the real
 * harness, and it takes no retry either, deliberately: it is the run that writes what every
 * later replay reads, so a row that only passed on a second attempt would commit whatever
 * that attempt happened to fetch. And the dead-recording guard
 * (`deadRecordingGuard`, e2e/stub/stub-test.ts) would otherwise be retried away: measured in
 * this issue's pod, Playwright re-runs a row that called `test.skip` without running its
 * fixtures at all, so the guard's refusal is raised on the first attempt and never on the
 * retry, and the run reports the row flaky and exits 0.
 */
const stubProjects = [
  {
    name: "stub",
    retries: 0,
    testMatch: stubSpecs,
    testIgnore: [],
    grepInvert: phoneOnlyTagged,
    use: {
      ...devices["Desktop Chrome"],
      baseURL: stub.recording ? baseUrl : stubOrigin(),
    },
  },
  // The phone half, which `iphone` is to `chromium`, and exactly that: `stub` runs every stub
  // row as `chromium` runs every row of the suite, and this one greps the `@phone` tag as
  // `iphone` does. `stub` does **not** invert the tag, which it did until a review read the
  // three rows that cost: a `@phone` row that `chromium` ran at the desktop width lost that
  // case in the move, and a moved row has to assert what it asserted before. A row that is a
  // phone row alone carries ` @phone-only` beside its `@phone`, which this project greps and
  // `stub` inverts - the second tag, and not the first, is the one a desktop project may
  // invert. Those rows used to say it in their own bodies instead, with a `test.skip` against
  // `testInfo.project.name`, so `stub` scheduled a row it was never going to run
  // (dispatch://INFRA-1410).
  {
    name: "stub-phone",
    retries: 0,
    testMatch: stubSpecs,
    testIgnore: [],
    grep: phoneRowTagged,
    use: {
      ...devices["iPhone 13"],
      browserName: "chromium" as const,
      baseURL: stub.recording ? baseUrl : stubOrigin(),
    },
  },
] satisfies Project[];

export default defineConfig({
  testDir: ".",
  testMatch: /.*\.e2e\.ts/,
  // The projects below that name no `testIgnore` of their own inherit this one. `stubSpecs` is
  // here so that a project which names neither selector never picks a stubbed row up: the `stub`
  // project names `testIgnore: []`, which is what lets those rows reach it.
  testIgnore: harnessProcessCount === 1 ? [multiprocessSpecs, stubSpecs] : [stubSpecs],
  outputDir: "test-results",
  // Every row is independent: it empties its stack's database and fake listeners itself
  // (e2e/seed.ts), and a worker never runs two rows at once, so rows of one file may run in
  // different stacks. A file whose rows must run in order says so itself, with
  // `test.describe.configure({ mode: "serial" })` and the reason.
  fullyParallel: true,
  // One worker by default, where Playwright's own default is half the machine's cores. A worker
  // costs a whole stack - a Postgres, a Go server and three to six fake listeners
  // (e2e/harness-pool.ts) - so a 16-core workstation would otherwise start eight of them before
  // its first row. CI names its own `--workers`, which `globalSetup` reads as `config.workers`
  // and starts that many stacks for.
  workers: 1,
  // One retry on CI, none locally. A row that fails once and passes on the retry is reported
  // `flaky` rather than failed, which is what the merged report of the dispatch-browser lane
  // publishes its flake rate from. Locally a retry would hide a failure the developer is looking
  // at, and `bun run e2e` is read as pass or fail. This is the value of every project that sets
  // none of its own: the six `-alone` projects, the two `visual` ones, the two stubbed ones
  // and the multiprocess one each set `retries: 0`, each for a reason its own comment gives.
  retries: process.env.CI ? 1 : 0,
  // `failure-reporter.ts` writes nothing and prints nothing, and is the one way `globalTeardown`
  // learns how this run's rows ended, which is what decides whether its stack logs are kept
  // (e2e/run-outcome.ts says why no other way reaches it). It is in every run that takes this
  // list - and a `--reporter` on the command line REPLACES this list rather than adding to it,
  // which is why a run given one keeps its logs whatever it did, and why
  // .github/workflows/dispatch-browser.yaml names this file in both of its own. With
  // E2E_MEASURE_FILE set, the timing fixture's reporter writes one row per test beside the list
  // reporter's own output (e2e/measure-reporter.ts); without it the run is untouched.
  reporter: [
    ["list"],
    [fileURLToPath(new URL("./failure-reporter.ts", import.meta.url))],
    ...(measuring
      ? [[fileURLToPath(new URL("./measure-reporter.ts", import.meta.url))] as const]
      : []),
  ] satisfies ReporterDescription[],
  timeout: testTimeout,
  expect: { timeout: expectTimeout },
  use: {
    ...devices["Desktop Chrome"],
    actionTimeout,
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
  // Starts this run's stacks and checks each of them before any row (e2e/global-setup.ts), and
  // stops them afterwards (e2e/global-teardown.ts). Both hooks are declared for every run,
  // including the two that start nothing - the stubbed layer alone, and a run against servers a
  // person started - because that decision is the hook's own, which is where a reader now finds
  // it. A deployed run starts a stack of the fakes alone, and the hook says why.
  globalSetup: fileURLToPath(new URL("./global-setup.ts", import.meta.url)),
  globalTeardown: fileURLToPath(new URL("./global-teardown.ts", import.meta.url)),
  // The one server this config declares itself, and it serves files: the built SPA, for the
  // stubbed layer's own origin (e2e/stub/static-server.ts). The harness is the pool's, which
  // `globalSetup` starts (e2e/harness-pool.ts).
  //
  // It is declared for a run that holds a stubbed row and for no other (e2e/stub/selection.ts
  // says which runs those are and why the question has to be asked at all).
  //
  // `reuseExistingServer: false`, deliberately, and it is the one thing that makes the fixed
  // port safe. The port has to be fixed because Playwright polls a URL before the command it
  // starts exists; reusing whatever answers it would let a second checkout on one machine
  // replay this one's rows against that one's `web/dist`, silently, since the probe only asks
  // for a document every build serves. Refusing instead makes the collision a message:
  // Playwright says the port is in use and names it, and `DISPATCH_E2E_STUB_PORT` moves this
  // run off it.
  webServer:
    !stub.holdsStubRow || stub.recording
      ? undefined
      : {
          command: "bun e2e/stub/static-server.ts",
          // Playwright runs a `webServer` command in the config's own directory, and every
          // command of this package is written from the package root, as `package.json`'s
          // scripts are.
          cwd: fileURLToPath(new URL("..", import.meta.url)),
          reuseExistingServer: false,
          stdout: "pipe",
          timeout: 60_000,
          url: `${stubOrigin()}/index.html`,
        },
  // A deployed run (`PLAYWRIGHT_BASE_URL`, `bun run e2e:deployed`) gets no stub project at
  // all: the stubbed layer replays a recording against a build this checkout made, so against
  // a deployed server it would prove nothing about either, and `e2e:deployed` names no project
  // and builds no `web/dist` to serve. `stub.declared` is that answer, and `holdsStubRow`
  // above is built on the same one, so a server can never start for a project this list leaves
  // out, nor a project be declared with no server to answer it.
  projects:
    harnessProcessCount === 2
      ? multiprocessProjects
      : [
          ...parallelProjects,
          ...aloneProjects,
          ...visualProjects,
          ...(stub.declared ? stubProjects : []),
        ],
});
