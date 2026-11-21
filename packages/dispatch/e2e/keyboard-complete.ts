/** What the rows of `keyboard-complete.e2e.ts` (dispatch://INFRA-2160, every page of Dispatch by
 *  the keyboard) read and seed that no other helper does: a page a key opens in a new tab, a
 *  move that loads no new document, the contrast of a choice, the writes a key sends, and the
 *  seeds of their checks.
 *
 *  The rows run on a build of the merge base as their negative control, with that file and this
 *  one checked out onto it, so nothing here imports what this tree adds to or changes in
 *  `web/src`.
 *
 *  Every document, project, person and host here is made up: this repository is public, so
 *  nothing names a real one. */

import { inboxThreadRow } from "@legion/contracts";
import { type BrowserContext, expect, type Locator, type Page } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import {
  createAsk,
  createIssue,
  createPriority,
  dismissInbox,
  editArtifact,
  listMyReplies,
  patchIssue,
  putArchitectureSource,
  requestApproval,
  syncArchitectureSource,
  withOpenedAsk,
} from "./api";
import { composite, contrastRatio, parseComputedColor, toRgbString } from "./colors";
import { expectTimeout } from "./expect-timeout";
import { seedFakeGithub } from "./fake-github-helpers";
import { body, notes } from "./inbox-pane-scroll";
import { agentRowId, replyRowId, seenOf } from "./inbox-threads";
import { type KeyedWrite, watchWrites } from "./keyed-writes";
import { machineSession } from "./machines";
import { sql } from "./psql";
import { sessionIds } from "./session-ids";

declare global {
  interface Window {
    __dispatchDocumentMark?: boolean;
  }
}

/** The session that asks every question these seeds write, and edits every document. */
export const asker = {
  actor: { id: "e2e-keyboard-complete", kind: "session" as const },
  as: "agent" as const,
};

// ---------------------------------------------------------------------------
// A page opened in a new tab, and a move that loads no new document.
// ---------------------------------------------------------------------------

/** The other site the rows' web links point to (`https://example.com/a`). Each context serves
 *  it itself (`newPageAt`), so no row reaches the network. */
export const OTHER_SITE = "https://example.com";

/** The contexts that already serve `OTHER_SITE`, so a second `newPageAt` in one context adds no
 *  second route over the first. */
const servingOtherSite = new WeakSet<BrowserContext>();

/**
 * The page `act` opens in `context`, read from the context's `page` event, which fires for every
 * new page of the context however the page under test opened it: `window.open` with
 * `noopener`, a modifier click, a middle click. The page is answered once its document has
 * loaded, at the address it was opened at. Every `OTHER_SITE` address answers a page of its own
 * from the context's route.
 *
 * Each wait ends at `expectTimeout`, the bound the rows' own assertions wait for a state, with a
 * failure that names `what` - the press the row made - and the wait that ran out, where the
 * context's own deadline would end it later with no word of what was awaited.
 */
export async function newPageAt(
  context: BrowserContext,
  act: () => Promise<unknown>,
  what: string
): Promise<Page> {
  if (!servingOtherSite.has(context)) {
    servingOtherSite.add(context);
    await context.route(`${OTHER_SITE}/**`, (route) =>
      route.fulfill({
        body: "<!doctype html><title>Another site</title><p>Another site.</p>",
        contentType: "text/html",
      })
    );
  }
  // Awaited together, so a press that throws ends the wait with it rather than leaving the wait
  // to fail on its own later.
  const [page] = await Promise.all([
    context.waitForEvent("page", { timeout: expectTimeout }).catch((error: unknown) => {
      throw new Error(`${what}: no new page opened within ${expectTimeout} ms`, { cause: error });
    }),
    act(),
  ]);
  await page
    .waitForLoadState("domcontentloaded", { timeout: expectTimeout })
    .catch((error: unknown) => {
      throw new Error(
        `${what}: the new page at ${page.url()} did not load its document within ${expectTimeout} ms`,
        { cause: error }
      );
    });
  return page;
}

/** Marks the document `page` shows, on its `window`: a move inside the app keeps the mark, and
 *  a load of a new document, a reload included, replaces `window` and the mark with it. */
export async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    window.__dispatchDocumentMark = true;
  });
}

/** Whether `page` loaded a new document since `markDocument` marked the one it showed. */
export async function reloaded(page: Page): Promise<boolean> {
  return page.evaluate(() => window.__dispatchDocumentMark !== true);
}

// ---------------------------------------------------------------------------
// The contrast of a choice.
// ---------------------------------------------------------------------------

/** One element's colours, as `contrastOf` reads them once they have stopped easing. */
export interface Contrast {
  /** The element's computed `color`, laid over `background`, as `rgb(r, g, b)`. */
  readonly color: string;
  /** What the text is read on: the first background that is not transparent on the element or
   *  on an element above it, laid over the ones under it down to the first opaque one, as
   *  `rgb(r, g, b)`. */
  readonly background: string;
  /** The WCAG 2.1 contrast ratio of `color` on `background`, from 1 to 21. */
  readonly ratio: number;
}

/** What the page reports of one element: its `color`, the `background-color` of it and of each
 *  element above it up to the root, whether a `color` or `background-color` transition still
 *  runs on any of them, and whether the element is a native `<select>` or `<option>`. */
interface ColourReading {
  readonly backgrounds: string[];
  readonly color: string;
  readonly easing: boolean;
  readonly native: boolean;
}

/**
 * The colours of the element `locator` names and their WCAG 2.1 contrast ratio, worked out here
 * from the computed styles (`e2e/colors.ts`) rather than read from the app. A button or a link
 * eases its `color` and `background-color` over 150 ms (`styles.css`), so after
 * `page.emulateMedia({ colorScheme })` the reading is polled until no such transition runs on
 * the element or above it. A translucent layer is laid over what is under it; `opacity` is not
 * read. The root of every Dispatch page draws an opaque background (`:root` in `styles.css`), so
 * an element with no opaque background under it is refused by name. A native `<select>` or
 * `<option>` whose own `background-color` is transparent is refused by name too: the browser or
 * the system can draw a native control's face and its open list on a colour of its own where
 * that background is transparent, and the open list outside the page, so the page's layers under
 * it say nothing of what the reader sees there.
 */
export async function contrastOf(locator: Locator): Promise<Contrast> {
  let reading: ColourReading | undefined;
  await expect
    .poll(
      async () => {
        reading = await locator.evaluate((element): ColourReading => {
          const chain: Element[] = [];
          for (let node: Element | null = element; node !== null; node = node.parentElement) {
            chain.push(node);
          }
          // `getAnimations` brings the style up to date first, which starts any transition a
          // switch of the colour scheme asks for, so a transition not yet begun is seen too.
          const easing = chain.some((node) =>
            node
              .getAnimations()
              .some(
                (animation) =>
                  animation instanceof CSSTransition &&
                  (animation.transitionProperty === "color" ||
                    animation.transitionProperty === "background-color")
              )
          );
          return {
            backgrounds: chain.map((node) => getComputedStyle(node).backgroundColor),
            color: getComputedStyle(element).color,
            easing,
            native: element instanceof HTMLSelectElement || element instanceof HTMLOptionElement,
          };
        });
        return reading.easing;
      },
      { message: "the colours of the element and of those above it are still easing" }
    )
    .toBe(false);
  if (reading === undefined) {
    throw new Error("the element's colours were never read");
  }
  const layers = reading.backgrounds.map(parseComputedColor);
  if (reading.native && layers[0]?.alpha === 0) {
    throw new Error(
      `a native <select> or <option> with a transparent background-color: ${reading.backgrounds[0]}`
    );
  }
  const first = layers.findIndex((layer) => layer.alpha > 0);
  const opaque = layers.findIndex((layer, index) => index >= first && layer.alpha >= 1);
  if (first === -1 || opaque === -1) {
    throw new Error(
      `no opaque background on the element or above it: ${reading.backgrounds.join(", ")}`
    );
  }
  let background = layers[opaque].rgb;
  for (let index = opaque - 1; index >= first; index -= 1) {
    background = composite(layers[index], background);
  }
  const color = composite(parseComputedColor(reading.color), background);
  return {
    background: toRgbString(background),
    color: toRgbString(color),
    ratio: contrastRatio(color, background),
  };
}

// ---------------------------------------------------------------------------
// The writes a key sends.
// ---------------------------------------------------------------------------

/** The route one dismissal, or one Move to Inbox, posts to. */
const DISMISS_PATH = "/api/v1/me/inbox/dismiss";

/** Every snooze `page` writes for the ask `askId` from here on, in order: `PUT
 *  /api/v1/me/asks/{id}/snooze`, whose body names `snoozed_until`. */
export function snoozeWrites(page: Page, askId: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "PUT", `/api/v1/me/asks/${encodeURIComponent(askId)}/snooze`);
}

/** Every Un-snooze `page` sends for the ask `askId` from here on: `DELETE
 *  /api/v1/me/asks/{id}/snooze`. */
export function unsnoozeWrites(page: Page, askId: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "DELETE", `/api/v1/me/asks/${encodeURIComponent(askId)}/snooze`);
}

/** The moment a snooze write names, its `snoozed_until`, in milliseconds since the epoch. */
export function snoozedUntil(write: KeyedWrite<unknown> | undefined): number {
  const input = write?.input;
  if (
    typeof input !== "object" ||
    input === null ||
    !("snoozed_until" in input) ||
    typeof input.snoozed_until !== "string"
  ) {
    throw new Error(`the snooze write names no moment: ${JSON.stringify(input)}`);
  }
  return Date.parse(input.snoozed_until);
}

/** The two mornings a Snooze menu offers, as the page's own clock and timezone read them at the
 *  call: 9:00 on the next day (Tomorrow), and 9:00 on the next Monday (Next week), a Monday
 *  naming the Monday after. Worked out here rather than read from the app, since the merge base
 *  the rows also run on offers no such menu. */
export async function returnMornings(page: Page): Promise<{ nextWeek: number; tomorrow: number }> {
  return page.evaluate(() => {
    const now = new Date();
    const morning = (days: number) => {
      const at = new Date(now);
      at.setDate(at.getDate() + days);
      at.setHours(9, 0, 0, 0);
      return at.getTime();
    };
    return { nextWeek: morning(7 - ((now.getDay() + 6) % 7)), tomorrow: morning(1) };
  });
}

/** Every review `page` posts of the document `artifactId` at whichever version the server takes
 *  for its latest: `POST /api/v1/artifacts/{id}/reviews`, which the merge base's Approve posts
 *  to. */
export function reviewWrites(page: Page, artifactId: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "POST", `/api/v1/artifacts/${encodeURIComponent(artifactId)}/reviews`);
}

/** Every review `page` posts of version `version` of the document `artifactId`: `POST
 *  /api/v1/artifacts/{id}/versions/{number}/reviews`, which `a`, Shift+R and Approve post to with
 *  the version the page read. */
export function versionReviewWrites(
  page: Page,
  artifactId: string,
  version: number
): KeyedWrite<unknown>[] {
  return watchWrites(
    page,
    "POST",
    `/api/v1/artifacts/${encodeURIComponent(artifactId)}/versions/${version}/reviews`
  );
}

/** Every write `page` sends to the issue `issueKey` from here on - its priority, status,
 *  assignee or repository: `PATCH /api/v1/issues/{key}`. */
export function issueWrites(page: Page, issueKey: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "PATCH", `/api/v1/issues/${encodeURIComponent(issueKey)}`);
}

/** Every hand-on `page` writes for the ask `askId`: `PUT /api/v1/asks/{id}/asked-of`. */
export function handOnWrites(page: Page, askId: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "PUT", `/api/v1/asks/${encodeURIComponent(askId)}/asked-of`);
}

/** Every answer `page` posts to the ask `askId`, an approval drawn as an ask card included:
 *  `POST /api/v1/asks/{id}/answer`. */
export function answerWrites(page: Page, askId: string): KeyedWrite<unknown>[] {
  return watchWrites(page, "POST", `/api/v1/asks/${encodeURIComponent(askId)}/answer`);
}

/** Every dismissal and every Move to Inbox `page` posts: `POST /api/v1/me/inbox/dismiss`. */
export function dismissWrites(page: Page): KeyedWrite<unknown>[] {
  return watchWrites(page, "POST", DISMISS_PATH);
}

/** The writes `holdWrites` caught, and what lets them through. */
export interface HeldWrites {
  /** Each write's body, in the order sent. */
  readonly bodies: readonly string[];
  /** Sends every write held so far on to Dispatch, and lets each later one straight through. */
  readonly release: () => void;
}

/** Every `method` request `page` sends from here on to a path `path` matches, each held until
 *  the row calls `release` and then sent on to Dispatch unchanged, in the order sent: a control's
 *  write stays out for as long as the row takes to press the control again. A hold of a fixed
 *  time is not enough: under load the row's own steps between the two presses outlast it, and
 *  the second press then lands on a control whose write has already come back. */
export async function holdWrites(page: Page, method: string, path: RegExp): Promise<HeldWrites> {
  const bodies: string[] = [];
  const released = Promise.withResolvers<void>();
  await page.route(
    (url) => path.test(url.pathname),
    async (route) => {
      if (route.request().method() !== method) return route.fallback();
      bodies.push(route.request().postData() ?? "");
      // The hold is the row's subject: the write stays out until the row lets it go, then goes
      // on to Dispatch as sent.
      await released.promise;
      return route.continue();
    }
  );
  return { bodies, release: () => released.resolve() };
}

// ---------------------------------------------------------------------------
// A control the rows reach for.
// ---------------------------------------------------------------------------

/** The ask card's hand-on control, wherever that card is drawn: the trigger of the list of
 *  people, inside the badge the card marks with its ask's id, named for the list and then for
 *  the badge's words ("Hand this question to Asked of alice"). Every query for it is scoped to
 *  the card's own ask rather than to a bare role, since a page can draw many cards. */
export function handOnControl(scope: Locator | Page, askId: string): Locator {
  return scope
    .locator(`[data-ask-hand-on="${askId}"]`)
    .getByRole("button", { name: "Hand this question to" });
}

/** Holds the key `control` draws beside its label (`ControlKeyHint`) to `key`, for the message
 *  `message`: a `kbd` hidden from assistive tech whose `data-key` is `key`, which its `::before`
 *  draws. The key is CSS generated content and no text of the page, so none of the words axe
 *  holds to the control's name (WCAG 2.5.3, Label in Name) are the key's; the control names its
 *  keys in its `aria-keyshortcuts`. */
export async function expectKeyHint(control: Locator, key: string, message: string): Promise<void> {
  const hint = control.locator('[aria-hidden="true"] kbd');
  await expect(hint, message).toHaveAttribute("data-key", key);
  await expect(hint, `${message}, drawn by CSS and no text of the page`).toHaveText("");
  await expect
    .poll(() => hint.evaluate((kbd) => getComputedStyle(kbd, "::before").content), {
      message: `${message}, drawn by its ::before`,
    })
    .toBe(JSON.stringify(key));
}

// ---------------------------------------------------------------------------
// The seeds.
// ---------------------------------------------------------------------------

/**
 * Takes the thread row `rowId` (`replyRowId`, `agentRowId`) out of `login`'s Inbox, as its
 * Dismiss does: the dismissal carries in `seen_replies` what the list route answers for that
 * row, which is what the page sends for a row it drew (`e2e/inbox-dismiss.e2e.ts`), so the
 * server dismisses it rather than keeping it for a write the page never drew.
 */
export async function dismissThread(rowId: string, login = "alice"): Promise<void> {
  const { rows } = await listMyReplies({ kind: ["reply", "agent"], state: "inbox" }, { login });
  const row = (rows ?? [])
    .map(inboxThreadRow)
    .find(
      (listed) =>
        (listed.kind === "agent" ? agentRowId(listed.thread_id) : replyRowId(listed.thread_id)) ===
        rowId
    );
  if (row === undefined) {
    throw new Error(`${rowId} is not in the Inbox of ${login}`);
  }
  const { dismissed } = await dismissInbox(
    { seen_replies: { [rowId]: seenOf(row) }, threads: [rowId] },
    { login }
  );
  if (dismissed !== 1) {
    throw new Error(`POST ${DISMISS_PATH} dismissed ${dismissed} threads where it named ${rowId}`);
  }
}

/** How many paragraphs a tall spec holds: 160 one-line paragraphs run past four heights of the
 *  reading column at 1440 by 900, the most any check asks of a document. */
const TALL_PARAGRAPHS = 160;

/** A spec of `TALL_PARAGRAPHS` numbered paragraphs under the heading `title`. */
function tallSpec(title: string): string {
  return body([`# ${title}`, ...notes(title, TALL_PARAGRAPHS)]);
}

/** An approval request of one issue's spec, which the reading pane draws as that document. */
export interface SeededApproval {
  readonly artifactId: string;
  readonly askId: string;
  readonly issueKey: string;
  /** The version of the spec the request asks the reader to approve. */
  readonly version: number;
}

/** Two approval requests, each of its own issue's tall spec in `project`, which must exist, in
 *  the order the Inbox lists them: the issues stand at P1 and P2. The two specs share most of
 *  their words, so each issue is made past the server's duplicate check (`force`). */
export async function seedTallAsks(project: string): Promise<SeededApproval[]> {
  const seeded: SeededApproval[] = [];
  for (const [index, title] of ["Ledger handover", "Mirror replay"].entries()) {
    const issue = await createIssue({ force: true, project, spec: tallSpec(title), title });
    await patchIssue(issue.key, { priority: index + 1 });
    const requested = withOpenedAsk(
      await requestApproval(issue.primary_artifact_id, { summary: `The ${title} draft.` }, asker)
    );
    seeded.push({
      artifactId: issue.primary_artifact_id,
      askId: requested.ask.id,
      issueKey: issue.key,
      version: requested.version,
    });
  }
  return seeded;
}

/** An issue titled `title` in `project`, which must exist, whose spec an agent edited once, so
 *  it stands at version 2, with an open approval request of that version. */
export async function seedApprovalAtVersionTwo(
  project: string,
  title: string
): Promise<SeededApproval> {
  const issue = await createIssue({ project, spec: `# ${title}\n\nThe plan.\n`, title });
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ find: "The plan.", op: "replace", with: "The revised plan." }] },
    asker
  );
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary: `${title}, revised.` }, asker)
  );
  expect(requested.version).toBe(2);
  return {
    artifactId: issue.primary_artifact_id,
    askId: requested.ask.id,
    issueKey: issue.key,
    version: requested.version,
  };
}

/** A priority and the one issue of `project` that advances it. */
export interface PriorityWithWork {
  readonly issueKey: string;
  readonly priorityId: string;
}

/** A priority titled `title`, which alice owns, with work under it: an issue of `project`, which
 *  must exist, in progress and advancing it. Each call writes the next priority in the one
 *  order, so two calls make two priorities. */
export async function seedPriorityWithWork(
  project: string,
  title: string
): Promise<PriorityWithWork> {
  const priority = await createPriority({ owner: "alice", title });
  const issue = await createIssue({ project, title: `${title}: the first step` });
  await patchIssue(issue.key, { advances: [priority.id], status: "in_progress" });
  return { issueKey: issue.key, priorityId: priority.id };
}

/** The repository `seedArchitectureLeaf` reads `project`'s architecture from, on the fake
 *  GitHub. */
const ARCHITECTURE_REPO = "legion/arch";

/** The one component of `seedArchitectureLeaf`'s model, which has no children. */
export const ARCHITECTURE_LEAF = { id: "platform", title: "Platform" } as const;

/** `project`'s architecture, which `project` must exist for: one component with no children,
 *  imported from the fake GitHub. */
export async function seedArchitectureLeaf(project: string): Promise<void> {
  const leaf = `---\ntitle: ${ARCHITECTURE_LEAF.title}\n---\nEverything that runs.\n`;
  await seedFakeGithub({
    [ARCHITECTURE_REPO]: {
      contents: "read",
      files: { [`${ARCHITECTURE_LEAF.id}.md`]: leaf },
      installation_id: 101,
    },
  });
  await putArchitectureSource(project, { branch: "main", repo: ARCHITECTURE_REPO });
  expect((await syncArchitectureSource(project, ARCHITECTURE_REPO)).last_error).toBeNull();
}

/** The two machines `seedTwoMachines` puts one live session on each of. */
export const MACHINES = ["ledger-devbox", "mirror-laptop"] as const;

/** One live session on each of `MACHINES`, seen 30 s before `now`, made live with `others` and
 *  no other session; answers the two. Their ids are not those of the thread sessions
 *  (`e2e/inbox-threads.ts`), so a row can name `THREAD_SESSION` among `others`. */
export async function seedTwoMachines(
  now: number,
  others: readonly FakeSession[] = []
): Promise<FakeSession[]> {
  const sessions = [
    machineSession(sessionIds.observer, "Ledger watcher", {
      host: MACHINES[0],
      lastSeen: now - 30_000,
    }),
    machineSession(sessionIds.archivist, "Mirror archivist", {
      host: MACHINES[1],
      lastSeen: now - 30_000,
    }),
  ];
  await setLiveSessions([...sessions, ...others]);
  return sessions;
}

/** One seeded question of `seedAsksOverTwoProjects`: the ask, its issue and where it stands. */
export interface SpreadAsk {
  readonly askId: string;
  readonly issueKey: string;
  readonly priority: 1 | 2;
  readonly project: string;
}

/** Four questions that wait on alice, one on an issue of each of the two `projects`, which must
 *  exist, at each of P1 and P2: a filter on either field keeps two of them, and a filter on both
 *  keeps one. */
export async function seedAsksOverTwoProjects(
  projects: readonly [string, string]
): Promise<SpreadAsk[]> {
  const seeded: SpreadAsk[] = [];
  for (const project of projects) {
    for (const priority of [1, 2] as const) {
      const issue = await createIssue({ project, title: `${project} rollout at P${priority}` });
      await patchIssue(issue.key, { priority });
      const ask = await createAsk(
        issue.key,
        { question: `Which window does ${project} replay at P${priority}?` },
        asker
      );
      seeded.push({ askId: ask.id, issueKey: issue.key, priority, project });
    }
  }
  return seeded;
}

/** A merged pull request of a second repository, `acme/gadgets`, by the author whose pull
 *  requests the Delivery page counts, inside the day `seedDeliveryFixture` writes its two pull
 *  requests of `acme/widgets` in (`TIMELINE_FIXTURE_QUERY`): with it the Delivery list holds three
 *  rows, its Repository field offers both repositories, and a pick of `acme/widgets` leaves two.
 *  Written after `seedDeliveryFixture`, which writes the delivery settings it is read under. */
export async function seedSecondRepositoryPull(): Promise<void> {
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/gadgets', 3, 'feat: a merged gadget', 'https://github.com/acme/gadgets/pull/3', 'octocat',
        '2024-06-01T04:00:00Z', '2024-06-01T05:00:00Z', 6, 2, false, null, ARRAY[]::text[], false)`
  );
}
