/** The `test` every row of the real-stack browser suite takes: Playwright's own, with the two
 *  things every one of those rows needs put on it once rather than written out in every file.
 *
 *  **The resets run as fixtures, not as a hook each file copies.** A row of this suite seeds
 *  what it reads and must read nothing another row left. It makes its data under names no other
 *  row makes - `rowKey`, `rowPerson`, `rowSessionId` and `rowNumber`, below - or reads a dataset
 *  the worker writes once (`deliverySettings`, `reviewCorpus`), and it takes out again what it
 *  added to a list the stack keeps whole (`ownPeople`, `ownBroadcasts`,
 *  `ownArchitectureSources`). So a row that keys its data needs no database emptied before it:
 *  the worker empties it once, before its first row (`workerDatabase`, through `resetDatabase` in
 *  e2e/seed.ts), for whatever a run before it left there, and again before it seeds the review
 *  corpus (`reviewCorpus`), whose seed reads lists the whole stack holds. The few rows
 *  docs/testing.md names empty it again inside themselves, through `resetDatabase`; among them
 *  are the rows that measure layout over text of a fixed width under a fixed project key
 *  (`fixedKey` in e2e/api.ts), whose project keys and issue numbers must be the same on every
 *  run. A fixed login (`fixedPerson`, below) needs no emptying, since `ownPeople` deletes its
 *  rows when the row ends. What the fake services hold - the fake Envoy's sessions and
 *  subscriptions, the broker's
 *  credential requests, the fake GitHub's repositories, the embedder's counts - is the row's, and
 *  no key separates it, so an automatic fixture resets the fakes before every row (`freshFakes`,
 *  through `resetFakes` in e2e/seed.ts). Both used to
 *  be a `test.beforeEach` at the top of every spec file that emptied the database and the fakes
 *  before each row: one copy of one hook in each file, as many places to read it, and one chance
 *  to forget it in every spec written since. As fixtures they run for every row of every file
 *  that imports this `test`, so a row cannot be written without them. A file whose setup does
 *  more still writes a hook, for that extra work alone.
 *
 *  **The sign-in is the default context's, not the row's.** Most rows of this suite open contexts
 *  of their own - `asUser(browser, "alice")`, `withAlice(browser, ...)` (e2e/users.ts) - and those
 *  are untouched: they take `{ browser }`, never instantiate the `context` fixture, and so never
 *  reach the override below or pay for it. What the override replaces is the other shape, the
 *  rows that take Playwright's own `page` or `context`, whose files each wrote the reset and then
 *  the sign-in, `await signIn(context, "alice")`, into one `test.beforeEach`.
 *
 *  That order is load-bearing, which is why the override names `freshFakes` among its own
 *  fixtures: emptying the database deletes the `people` and `user_sessions` rows every sign-in
 *  writes and re-creates alice's and bob's alone (the head comment of e2e/users.ts), so any other
 *  login signed in before it would carry a cookie the server refuses. Naming `freshFakes`, which
 *  names `workerDatabase`, is what makes Playwright set both up first, so the order is kept here
 *  once instead of in each file that happened to write it.
 *
 *  **How a row opts out.** The `signedIn` option is the login the default context is signed in
 *  as, `"alice"` unless a file says otherwise, and `false` leaves that context signed out;
 *  `own(login)` signs it in as the row's own person for that login (`rowPerson`, below), a
 *  person no other row signs in as and one the row's end deletes again (`ownPeople`):
 *
 *      test.use({ signedIn: false });          // the reader nobody signed in
 *      test.use({ signedIn: "bob" });          // the default context is bob's
 *      test.use({ signedIn: own("alice") });   // the default context is rowPerson("alice")'s
 *
 *  It is read where Playwright reads any option - at file scope, or inside a `test.describe` -
 *  and it governs the default context alone. A row that opens its own context signs that one in
 *  itself, as it always has: `asUser(browser, rowPerson("alice"))` for a person of its own. The
 *  option, `asUser` and `signIn` take the same logins (`SignInLogin`, e2e/users.ts): alice, bob,
 *  a login `rowPerson` or `fixedPerson` answered, or a `fixedLogin` that leaves no person a later
 *  row reads (e.g. `"Bob"`, which the server folds to `"bob"` in e2e/issue.e2e.ts). Any other
 *  literal login fails the typecheck.
 *
 *  **What a worker keeps.** Three things are the worker's, not a row's: its database, emptied
 *  before its first row (`workerDatabase`) and before it seeds the review corpus, the delivery
 *  settings the delivery rows read (`deliverySettings`, handed to a row as its `delivery` place)
 *  and the review corpus the accessibility and foot-sweep rows read (`reviewCorpus`, handed to a
 *  row as its `reviewSeed`). The settings
 *  and the corpus are each written once into each database the worker empties - once for the
 *  worker, and again after a row that empties the database itself - and a row that changes one
 *  puts it back when it ends. A row that empties the database itself does it through
 *  `resetDatabase`, which counts that database too (`databasesEmptied`, e2e/seed.ts).
 *
 *  The stubbed layer takes no part in any of this: it reaches no server, no database and no
 *  sign-in, and declares a `test` of its own (e2e/stub/stub-test.ts). So does the containerised
 *  visual review (e2e/visual.e2e.ts), whose rows seed outside Playwright.
 */

import { randomBytes } from "node:crypto";

import { PROJECT_KEY_PATTERN, PROJECT_KEY_RULE } from "@legion/contracts";
import { test as base, type TestInfo } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { deleteArchitectureSource, type RowKey } from "./api";
import {
  type DeliveryPlace,
  deleteDeliveryRowsBut,
  deliveryPlace,
  deliveryRowsHeld,
  MEASURES_DAYS_LATER,
  putDeliverySettings,
  ROW_DAYS_EARLIER,
  readDeliverySettings,
  writeDeliverySettings,
} from "./delivery-seed";
import { expectTimeout } from "./expect-timeout";
import { sql } from "./psql";
import type { ReviewSeed } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import {
  checkLogicalLogin,
  type RowIdentity,
  rowKeyOf,
  rowNumberOf,
  rowPersonOf,
  rowSessionIdOf,
} from "./row-names";
import {
  broadcastIds,
  databasesEmptied,
  deleteBroadcastsBut,
  resetDatabase,
  resetFakes,
  sqlLiteral,
} from "./seed";
import { type RowLogin, type SignInLogin, signIn } from "./users";
import { workspaceSessions } from "./workspace";

/** Bytes this process drew once, when it loaded this module, and folds into every `rowKey`: a
 *  second run of the suite against the same database, or a retry in a new worker, draws others,
 *  so it never meets a key the first one made. */
const processNonce = randomBytes(16).toString("hex");

/** The phase each row that has moved on from its first is in (`nextRowPhase`); a row that never
 *  moved on is in phase 0. */
const rowPhases = new WeakMap<TestInfo, number>();

/** Moves the running row on to its next phase: from here on every `rowKey`, `rowPerson`,
 *  `rowSessionId` and `rowNumber` it asks answers a name of the new phase's own. A phase then
 *  seeds beside what the phases before it wrote, under names none of them wrote, as a row seeds
 *  beside what other rows wrote, and needs no emptied database (`phase`, e2e/issue-page.ts).
 *  Outside a running row it throws, as `test.info()` does. */
export function nextRowPhase(): void {
  const row = base.info();
  rowPhases.set(row, (rowPhases.get(row) ?? 0) + 1);
}

/** The running row as every name of it derives from it (`RowIdentity`, e2e/row-names.ts): its
 *  `testId`, `repeatEachIndex` and `retry`, the phase it is in (`nextRowPhase`), and this
 *  process's nonce. `rowKey`, `rowPerson`, `rowSessionId` and `rowNumber` hand it to the
 *  derivations there. Outside a running row it throws, as `test.info()` does. */
function runningRow(): RowIdentity {
  const row = base.info();
  return {
    nonce: processNonce,
    phase: rowPhases.get(row) ?? 0,
    repeatEachIndex: row.repeatEachIndex,
    retry: row.retry,
    testId: row.testId,
  };
}

/** The project key the running row makes and names for the key `logical` its spec writes, so
 *  that no two rows share a project and a row reads none another row left.
 *
 *  It is the first five characters of `logical` (all of it when it is shorter) and five more,
 *  the consonants other than Y and the digits in base 30, from a SHA-256 of the row (`testId`,
 *  `repeatEachIndex`, `retry`, its phase), of this process's nonce and of `logical` itself
 *  (`rowKeyOf`, e2e/row-names.ts), so two logical keys of one row differ even when their first
 *  five characters agree. A key is at most ten characters, and starts with A-Z, as the server's
 *  ProjectKeyPattern requires (`^[A-Z][A-Z0-9]{1,9}$`,
 *  packages/envoy/internal/contracts/generated.go).
 *
 *  The suffix holds no vowel and no Y, so it spells no word a control is named by beyond what
 *  consonants and digits spell alone (`SUFFIX_SYMBOLS`, e2e/row-names.ts, says why). It holds
 *  digits, so a key holds digits its logical key does not: a count drawn beside a key is read
 *  in its element's whole text, the key in it (`toHaveText`), never as a digit the text holds
 *  (`toContainText("1")`), which a digit of the key would pass, or, said to be absent
 *  (`not.toContainText("1")`), fail.
 *
 *  Within a row every call with the same `logical` answers the same key, so a row, its hooks and
 *  the helpers it calls agree without passing the key around; a row that moves on to a next
 *  phase (`nextRowPhase`) gets keys of that phase's own from then on. Outside a running row it
 *  throws, as `test.info()` does: a key held at module scope belongs to no row.
 *
 *  It answers a `RowKey` (e2e/api.ts), the one type `createProject` takes for a key, so a
 *  real-stack row that writes a literal key there fails the typecheck. A helper that makes a
 *  project under a key its caller passes takes a `RowKey` too.
 *
 *      const core = rowKey("CORE");
 *      await createProject({ key: core, name: "Core" });
 *      await page.goto(`/projects/${core}`); */
export function rowKey(logical: string): RowKey {
  return rowKeyOf(runningRow(), logical) as RowKey;
}

/** `key`, a key of the running row's own (`rowKey`), behind `prefix`: a key no other row makes
 *  either, since none makes `key`, for a row whose claim needs one of its keys written inside
 *  another, as `XCORE` holds `CORE` (`seedReferenceWorld`, e2e/reference-surfaces.ts). It
 *  throws where the result is no key the server takes (`PROJECT_KEY_PATTERN`, @legion/contracts):
 *  a `rowKey` of a logical key of five letters or more is ten characters already. */
export function rowKeyBehind(prefix: string, key: RowKey): RowKey {
  const behind = `${prefix}${key}`;
  if (!PROJECT_KEY_PATTERN.test(behind)) {
    throw new Error(
      `rowKeyBehind: ${JSON.stringify(behind)} is no project key: ${PROJECT_KEY_RULE}`
    );
  }
  return behind as RowKey;
}

/** The Envoy session id the running row seeds and names for the session `logical` its spec
 *  writes (`planner`, `reviewer`), so that no two rows share a session and a row reads no message,
 *  ask, read mark or pin another row left on it: Dispatch keeps what a session said and what was
 *  said to it by the session's id, for every person at once (`listAgentMessages`).
 *
 *  It is a UUID, since Dispatch's agent routes refuse any other id (`agentStreamSessionID`): the
 *  first sixteen bytes of `rowKey`'s SHA-256, taken over `session:` and `logical`, with RFC
 *  9562's version 8 and its variant written into them (`rowSessionIdOf`, e2e/row-names.ts).
 *  Within a row (a phase of it, `nextRowPhase`) every call with the same `logical` answers the
 *  same id; outside a running row it throws, as `rowKey` does.
 *
 *      const planner = { session_id: rowSessionId("planner"), title: "Planner" };
 *      await setLiveSessions([planner]); */
export function rowSessionId(logical: string): string {
  return rowSessionIdOf(runningRow(), logical);
}

/** A number of the running row's own for the number `logical` its spec writes - a pull
 *  request's, a run's - so that no two rows share one a table holds unique (a pull request's head
 *  in `pull_request_heads`, by repository and number; the address of an external link, which
 *  names one) and a row reads none another row left.
 *
 *  It is drawn as `rowKey`'s suffix is, from a SHA-256 of the row, of this process's nonce and
 *  of `number:` and `logical`, but read as a number below 26 ** 5 rather than spelled, plus
 *  10,000 (`rowNumberOf`, e2e/row-names.ts): a whole number from 10,000 up and below
 *  10,000 + 26 ** 5, so it fits a Postgres `integer`, which a pull request's number is. Within a
 *  row (a phase of it, `nextRowPhase`) every call with the same `logical` answers the same
 *  number; outside a running row it throws, as `rowKey` does.
 *
 *      const pull = rowNumber("pull");
 *      const url = `https://github.com/acme/widgets/pull/${pull}`; */
export function rowNumber(logical: string): number {
  return rowNumberOf(runningRow(), logical);
}

/** Every login `rowPerson` and `fixedPerson` have named in the running row, which `ownPeople`
 *  deletes when the row ends. A worker runs one row at a time, so one set serves the worker. */
const namedPeople = new Set<string>();

/** The person the running row signs in as and names for the login `logical` its spec writes, so
 *  that a row reading one person's list - an Inbox, its counts, read and unread state, the pins
 *  a person keeps - reads a list no other row added to.
 *
 *  It is `logical` whole and five more characters, consonants other than y and digits:
 *  `rowKey`'s suffix, from the same SHA-256 of the row, of this process's nonce and of `logical`,
 *  lower-cased (`rowPersonOf`, e2e/row-names.ts), since the server records a login trimmed and
 *  lower-cased (`authDevSignIn`, packages/envoy/internal/dispatch/routes/devsignin.go): `alice`
 *  -> `alicek7zvm`. Two logical logins of one row differ, and so does one logical login in two
 *  rows. Within a row (a phase of it, `nextRowPhase`) every call with the same `logical` answers
 *  the same login; outside a running row it throws, as `rowKey` does.
 *
 *  The person needs no seeding: a dev sign-in records any login it signs in (`People.Record`
 *  in the same route), which is what makes the login one a project, an issue or an ask can name.
 *  So the first thing a row does as this person is sign it in - the default context under
 *  `own()`, or an api.ts call with `{ login: rowPerson("alice") }` - before a seed names it. It
 *  pays a dev sign-in where alice and bob carry a kept cookie (`reusableSessionCookie`,
 *  e2e/api.ts), since the reset re-creates neither the person nor a session for it.
 *
 *  The person is the row's to the end: when the row ends, pass or fail, `ownPeople` deletes the
 *  `people` and `user_sessions` rows the sign-in made for every login this named in the row.
 *  Everyone who has signed in is one list (`GET /api/v1/users`, the options of every assignee
 *  control), and a row that reads that list whole - Settings' default assignee offering exactly
 *  alice and bob - reads it back at its start only if each row takes its own people out again.
 *  The login is a `RowLogin` (e2e/users.ts), which `asUser`, `signIn` and the `signedIn` option
 *  take beside alice and bob.
 *
 *      const me = rowPerson("alice");
 *      await createProject({ key: rowKey("CORE"), name: "Core" }, { login: me });
 *      const context = await asUser(browser, me); */
export function rowPerson(logical: string): RowLogin {
  const login = rowPersonOf(runningRow(), logical) as RowLogin;
  namedPeople.add(login);
  return login;
}

/** `login`, a person whose login is fixed on purpose, for a row that measures what the page draws
 *  of a login - the header's height over its assignee's name - and so needs the login that
 *  reading was taken on rather than one of the row's own (`rowPerson`), whose five more
 *  characters change the width being measured. `why` names that reason where the login is
 *  written, and a call without one throws, as `fixedKey`'s does (e2e/api.ts). `login` is written
 *  as the server records it, trimmed and lower-cased, and is neither alice nor bob: the rows sign
 *  in as them, and only a reset re-creates them once `ownPeople` has deleted them.
 *
 *  The person is recorded as `rowPerson`'s is, by the row's first sign-in, and is the row's to
 *  the end in the same way: when the row ends, pass or fail, `ownPeople` deletes its `people`
 *  and `user_sessions` rows, so the list of everyone who has signed in reads at the next row's
 *  start as it read at this one's. The login is a `RowLogin` (e2e/users.ts), as `rowPerson`'s
 *  is, so `asUser`, `signIn` and the `signedIn` option take it. */
export function fixedPerson(login: string, why: string): RowLogin {
  if (why.trim() === "") {
    throw new Error(
      `fixedPerson: the fixed login ${JSON.stringify(login)} must name why it is fixed`
    );
  }
  namedPeople.add(login);
  return login as RowLogin;
}

/** A `signedIn` value that signs the default context in as the row's own person for `login`. */
export interface OwnPerson {
  readonly own: string;
}

/** The `signedIn` value for a file or a `test.describe` whose default context is the row's own
 *  person for `login`, `rowPerson(login)`: `test.use({ signedIn: own("alice") })`. The person is
 *  named when the row runs, since `test.use` is read before any row does. */
export function own(login: string): OwnPerson {
  checkLogicalLogin("own", login);
  return { own: login };
}

/** Every architecture source the stack holds, `<project>\t<repo>` each, read from the store's
 *  own table (migrations 0036 and 0117): the pair Dispatch's delete route names one by. */
async function architectureSourcesHeld(): Promise<string[]> {
  const held = await sql(
    "SELECT project_key || E'\\t' || repo FROM architecture_sources ORDER BY project_key, repo"
  );
  return held === "" ? [] : held.split("\n");
}

/** What a spec may set with `test.use`. */
type DispatchOptions = {
  /** The login Playwright's own `context` is signed in as: a `SignInLogin` (e2e/users.ts),
   *  `own(login)` for the row's own person for that login, or `false` for a signed-out context.
   *  A `SignInLogin` is alice, bob, a `rowPerson` or `fixedPerson` login, or a `fixedLogin` that
   *  leaves no person a later row reads. */
  signedIn: SignInLogin | OwnPerson | false;
};

/** What this `test` adds beyond the options. */
type DispatchFixtures = {
  /** The fakes reset before every row (`resetFakes`, e2e/seed.ts), on the database the worker
   *  emptied before its first row (`workerDatabase`). It is automatic, so no row asks for it, and
   *  it carries no value: what a row, and a fixture that names it, take from it is that it ran. */
  freshFakes: undefined;
  /** The row's own people taken out again once it ends (`rowPerson`, `fixedPerson`). It is
   *  automatic and carries no value: its work is its teardown. */
  ownPeople: undefined;
  /** The broadcasts the row sent taken out again once it ends. It is automatic and carries no
   *  value: its work is its teardown. */
  ownBroadcasts: undefined;
  /** The architecture sources the row added taken out again once it ends. It is automatic and
   *  carries no value: its work is its teardown. */
  ownArchitectureSources: undefined;
  /** The row's own place in the delivery tables (`DeliveryPlace`, e2e/delivery-seed.ts), on the
   *  delivery settings the worker wrote: what a delivery row hands its fixtures, and reads its
   *  numbers, its day and its window from. What the row wrote there is taken out again once it
   *  ends, and the settings are put back. */
  delivery: DeliveryPlace;
  /** The review corpus (e2e/review-seed.ts), as the worker seeded it, for a row that reads the
   *  review layers' pages; the delivery settings are put back once the row ends. */
  reviewSeed: ReviewSeed;
};

/** What this `test` keeps for a whole worker. */
type DispatchWorkerFixtures = {
  /** The database this worker empties once, before its first row (`resetDatabase`). It carries
   *  no value: what `freshFakes` takes from it is that it ran. */
  workerDatabase: undefined;
  /** Writes the delivery settings the delivery rows read (`writeDeliverySettings`), unless the
   *  worker has written them into the database it holds now (`databasesEmptied`). */
  deliverySettings: () => Promise<void>;
  /** The review corpus (`seedReviewRoutes`), seeded into a database the worker empties for it
   *  unless the worker has seeded it into the database it holds now (`databasesEmptied`), with
   *  what a row before this one changed of it put back. */
  reviewCorpus: () => Promise<ReviewSeed>;
};

export const test = base.extend<DispatchOptions & DispatchFixtures, DispatchWorkerFixtures>({
  signedIn: ["alice", { option: true }],
  // The worker's one emptying of its database, before its first row, for whatever a run before
  // it left there: a run of the suite against the same database, or the worker this stack served
  // before this one. A row that keys its data needs no more: it makes its data under names of
  // its own (`rowKey`, `rowPerson`, `rowSessionId`, `rowNumber`) or reads a dataset the worker
  // keeps, and takes out again what it added to a list the stack keeps whole (`ownPeople`,
  // `ownBroadcasts`, `ownArchitectureSources`), so it starts beside what the rows before it wrote
  // and reads none of it. The few rows docs/testing.md names empty it again themselves, through
  // `resetDatabase` too, which counts every database it empties (`databasesEmptied`).
  workerDatabase: [
    // Playwright refuses a first argument that is not a destructuring pattern.
    // biome-ignore lint/correctness/noEmptyPattern: the emptying takes no other fixture.
    async ({}, use) => {
      await resetDatabase();
      await use(undefined);
    },
    { scope: "worker" },
  ],
  // The reset every row starts with: the fake services alone (`resetFakes`, e2e/seed.ts), whose
  // state is the row's and which no key of the row separates. It names `workerDatabase`, so a
  // fixture that names this one is set up after both. On the worker's first row the emptying has
  // reset the fakes already, and this resets them once more: one more request to each fake, on
  // that row alone.
  freshFakes: [
    async ({ workerDatabase: _emptiedFirst }, use) => {
      await resetFakes();
      await use(undefined);
    },
    { auto: true },
  ],
  // The teardown is the `finally` of every row that named a person of its own: the `people` row
  // a dev sign-in records and the `user_sessions` row it starts, of every login `rowPerson` or
  // `fixedPerson` answered in the row, one statement whatever the row did with them. Rows of one
  // worker run one at a time on that worker's own stack, so the next row starts on the people
  // list this row started on. No foreign key reaches either table, and nothing records the
  // person again: only a sign-in does, and the row's own contexts are closed by now (the default
  // one by the `context` fixture, which names this one so that it is torn down first).
  ownPeople: [
    // biome-ignore lint/correctness/noEmptyPattern: the cleanup takes no other fixture.
    async ({}, use) => {
      await use(undefined);
      if (namedPeople.size === 0) return;
      const logins = [...namedPeople].map(sqlLiteral).join(", ");
      namedPeople.clear();
      await sql(
        `WITH sessions AS (DELETE FROM user_sessions WHERE login IN (${logins})) DELETE FROM people WHERE email IN (${logins})`
      );
    },
    { auto: true },
  ],
  // The teardown is the `finally` of every row that sends a broadcast, through the page or
  // through e2e/api.ts alike: every broadcast the stack holds at the row's end that it did not
  // hold at its start, with the copies, replies, attempts and events each wrote
  // (`deleteBroadcastsBut`). The list of every broadcast (`GET /api/v1/broadcasts`, the Agents
  // page's Broadcasts region) is the stack's, not a person's, and a row that reads it whole -
  // that it holds exactly the broadcast the row sent - reads it back at its start only if each
  // row takes its own out again. Rows of one worker run one at a time on that worker's own
  // stack, so what the end holds beyond the start is this row's. That includes the review
  // corpus's broadcast when this row was the first to read the corpus: the list stays whole for
  // the rows after it, and no page the accessibility rows scan draws it (`reviewCorpus`). It is
  // read after the worker's emptying and the fakes' reset (`freshFakes`) and torn down after the
  // default context is closed, which names it as it names `ownPeople`.
  ownBroadcasts: [
    async ({ freshFakes: _resetFirst }, use) => {
      const kept = await broadcastIds();
      await use(undefined);
      await deleteBroadcastsBut(kept);
    },
    { auto: true },
  ],
  // The teardown is the `finally` of every row that adds an architecture source, through the
  // Settings page or through e2e/api.ts alike: every source the stack holds at the row's end
  // that it did not hold at its start, each removed by Dispatch's own route, which takes the
  // components it projected with it. The list of every project's sources
  // (`GET /api/v1/settings/architecture-sources`, the Settings page's Architecture sources
  // table) is the stack's, not a project's, and a row that reads it whole - "No architecture
  // sources yet." once it has deleted its own - reads it back only if each row takes its own out
  // again. Rows of one worker run one at a time on that worker's own stack, so what the end
  // holds beyond the start is this row's. Both reads are the store's own table, not the route
  // the page reads, so a row on a stack that signs nobody in through the dev route (the OIDC
  // stacks of signin.multiprocess.e2e.ts) makes no request here; only a source the row added is
  // taken out through the route.
  ownArchitectureSources: [
    async ({ freshFakes: _resetFirst }, use) => {
      const kept = new Set(await architectureSourcesHeld());
      await use(undefined);
      for (const held of await architectureSourcesHeld()) {
        if (kept.has(held)) continue;
        const [project = "", repo = ""] = held.split("\t");
        await deleteArchitectureSource(project, repo);
      }
    },
    { auto: true },
  ],
  // The delivery settings are one row for the whole deployment (`delivery_settings.singleton`),
  // which every delivery row reads and none owns, so the worker writes them rather than each row:
  // once into each database it empties - the one it empties before its first row, and again
  // after a row that empties it itself - with an upsert, which writes them whether or not a
  // settings row stands and empties nothing. A row that changes them - the unconfigured page's
  // row, which starts with none, a row that sets a staging job or a reconcile error - gets them
  // back from the `delivery` fixture when it ends, as the fixture left them. No lock orders those
  // rows: rows of one worker run one at a time on that worker's own stack, so the teardown is
  // what keeps the next row's start, and Playwright has no lock that orders rows of one worker;
  // rows of two workers never share a stack, so there is nothing for a lock between workers to
  // guard.
  deliverySettings: [
    // biome-ignore lint/correctness/noEmptyPattern: the settings take no other fixture.
    async ({}, use) => {
      let writtenInto: number | undefined;
      await use(async () => {
        const database = databasesEmptied();
        if (writtenInto === database) return;
        await writeDeliverySettings();
        writtenInto = database;
      });
    },
    { scope: "worker" },
  ],
  // A delivery row's own place: the row's project (`rowKey("ACME")`), numbers of its own for
  // its pull requests and runs, a multiple of 10,000 from `rowNumber` between 10,000 and
  // 1,999,990,000, which keeps every sum inside a Postgres `integer`, which a pull request's
  // number is, the timeline and the Pipeline written `ROW_DAYS_EARLIER` days before the review
  // corpus's, and the measures `MEASURES_DAYS_LATER` days after the corpus's, past the last
  // deploy it writes; the worker keeps the corpus beside them (`reviewCorpus`). The teardown is
  // the `finally` of every delivery row: the timeline, the measures and the Pipeline read every
  // pull request, run and session group of their window whole ("2 PRs in current
  // filter/window", the four measures, "p90 30m · 2 runs"), so each row deletes what it wrote
  // there (`deleteDeliveryRowsBut`) and puts the settings back, and the next row of the worker
  // starts on the tables and the settings this one started on.
  delivery: async ({ deliverySettings, freshFakes: _resetFirst }, use) => {
    await deliverySettings();
    const held = await deliveryRowsHeld();
    const place = deliveryPlace(
      rowKey("ACME"),
      10_000 * (1 + (rowNumber("delivery") % 199_999)),
      ROW_DAYS_EARLIER,
      MEASURES_DAYS_LATER
    );
    await use(place);
    await deleteDeliveryRowsBut(held, place.project);
    await writeDeliverySettings();
  },
  // The review corpus is a dataset every accessibility row reads alike, so the worker seeds it
  // once into each database it empties, as the delivery settings are, and hands each row the
  // seed it returned. Its keys are the corpus's own (`CORE`, `ACME`), which the stubbed layer's
  // recordings were made under and no other row writes, since every other row writes keys of
  // its own. Two things of it a row before can change without touching those keys, and both are
  // put back before a row reads it: the delivery settings, which a delivery row puts back to
  // `writeDeliverySettings`'s and the corpus seeds with its Pipeline job names and its reconcile
  // error, and the fake Envoy's sessions, which a row may set and a reset of the fakes empties.
  // Its broadcast goes at the end of the row that seeded it (`ownBroadcasts`): every broadcast
  // is one list a row reads whole, and no page these rows scan draws it. The seed reads lists
  // the whole stack holds, every open question in the Inbox (`seedWorkspace`) and every check
  // result (`seedReviewRoutes`), so the worker empties its database just before it seeds the
  // corpus, whatever the rows before this one wrote there: they have ended, and every row after
  // it writes under keys of its own. The first row to read it is often not the worker's first:
  // seeded after the other rows of its worker, the seed's Inbox read never held exactly its five
  // rows, and four rows failed so in CI run 45530954674. Emptying deletes every sign-in but
  // alice's and bob's (the head comment above), so a row that reads the corpus opens its own
  // contexts, as `asUser` and `withAlice` do.
  reviewCorpus: [
    // biome-ignore lint/correctness/noEmptyPattern: the corpus takes no other fixture.
    async ({}, use) => {
      let seeded: { into: number; seed: ReviewSeed; settings: string } | undefined;
      await use(async () => {
        if (seeded?.into === databasesEmptied()) {
          await putDeliverySettings(seeded.settings);
          await setLiveSessions(workspaceSessions);
          return seeded.seed;
        }
        await resetDatabase();
        const into = databasesEmptied();
        // The seed's waits give up at the bound a row's own assertions use, inside its clock.
        const seed = await seedReviewRoutes({ boundMs: expectTimeout });
        seeded = { into, seed, settings: await readDeliverySettings() };
        return seed;
      });
    },
    { scope: "worker" },
  ],
  // The teardown puts the delivery settings back to the ones a delivery row starts on: the
  // corpus's Pipeline job names and reconcile error are its own, and no other row reads them.
  reviewSeed: async ({ freshFakes: _resetFirst, reviewCorpus }, use) => {
    await use(await reviewCorpus());
    await writeDeliverySettings();
  },
  // `freshFakes` is in this list for its order rather than for its value, hence the unused
  // binding: naming it is what makes Playwright empty the worker's database, before its first
  // row, and reset the fakes before this fixture signs anyone in, which is the order the hooks
  // this replaces were written in. `ownPeople` and `ownBroadcasts` are here for their order too:
  // set up before this context, each is torn down after it is closed.
  context: async (
    {
      context,
      signedIn,
      freshFakes: _resetFirst,
      ownPeople: _deletedAfter,
      ownBroadcasts: _broadcastsDeletedAfter,
    },
    use
  ) => {
    if (signedIn !== false) {
      await signIn(context, typeof signedIn === "string" ? signedIn : rowPerson(signedIn.own));
    }
    await use(context);
  },
});

/** What a spec takes from Playwright beside this `test`, re-exported so a converted file has one
 *  import line rather than two: `devices` and `expect`, and the types the suite's specs name. A
 *  type Playwright exports and no spec names yet is not here; add it when a spec needs it. */
export type {
  APIResponse,
  Browser,
  BrowserContext,
  Locator,
  Page,
  Request,
  Response,
  Route,
  TestInfo,
  WebSocket,
  WebSocketRoute,
} from "@playwright/test";
export { devices, expect } from "@playwright/test";
